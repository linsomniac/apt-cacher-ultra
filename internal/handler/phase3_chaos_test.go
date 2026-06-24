package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// chaos3DebSpec describes one .deb in a Phase 3 chaos fixture: archive-
// relative path plus the (Package, Architecture) tuple the SPEC3 §7.5.3
// hot-set Stage 1 query keys on. Specs are sorted by packageName so the
// hot-set's deterministic ORDER BY (package_name, architecture) places
// them in the slice's slice-index order during the prefetch loop —
// that's the load-bearing property the §12.3 hung-FIRST / hung-LAST
// variants rely on.
type chaos3DebSpec struct {
	rel          string
	packageName  string
	architecture string
}

const (
	chaos3Suite          = "/ubuntu/dists/noble"
	chaos3PackagesGzPath = "main/binary-amd64/Packages.gz"
)

// chaos3Snapshot is the Phase 3 equivalent of chaos2Snapshot. The
// Packages text is rebuilt to include Package: and Architecture:
// stanza fields so adoption can populate package_hash with the
// (Package, Arch) tuple that the v3 hot-set Stage 1 query keys on.
// Without those fields, Stage 1's `ph.package_name <> ”` predicate
// excludes the row and the hot set is empty.
type chaos3Snapshot struct {
	label      string
	inRelease  []byte
	packagesGz []byte
	debBodies  map[string][]byte // suite-rel path → body
	debSpecs   []chaos3DebSpec
}

// makeChaos3Snapshot builds an A-or-B fixture using the v3 Packages
// schema. Each .deb's body embeds the snapshot label, so byte equality
// in client-burst assertions discriminates which snapshot served a
// given response. specs are sorted by packageName ascending — the
// hot-set ORDER BY visits them in slice order.
func makeChaos3Snapshot(label string, specs []chaos3DebSpec) chaos3Snapshot {
	sorted := append([]chaos3DebSpec(nil), specs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].packageName != sorted[j].packageName {
			return sorted[i].packageName < sorted[j].packageName
		}
		return sorted[i].architecture < sorted[j].architecture
	})

	bodies := make(map[string][]byte, len(sorted))
	var sb strings.Builder
	for _, d := range sorted {
		body := []byte(fmt.Sprintf("snapshot-%s|deb|%s", label, d.rel))
		bodies[d.rel] = body
		h := chaos2Sha256Hex(body)
		fmt.Fprintf(&sb, "Package: %s\nArchitecture: %s\nVersion: 1.0\nFilename: %s\nSize: %d\nSHA256: %s\n\n",
			d.packageName, d.architecture, d.rel, len(body), h)
	}
	pkgsTxt := []byte(sb.String())
	pkgsGz := chaos2Gzip(pkgsTxt)
	rel := chaos2BuildRelease(map[string][]byte{
		chaos3PackagesGzPath: pkgsGz,
	})
	return chaos3Snapshot{
		label:      label,
		inRelease:  rel,
		packagesGz: pkgsGz,
		debBodies:  bodies,
		debSpecs:   sorted,
	}
}

// chaos3BodyAndETagFor maps a request path to (body, ETag) for the
// snapshot. Suffix order is load-bearing: /InRelease and /Packages.gz
// are checked before deb suffix matching so a path like
// "...x/InRelease" never accidentally matches a deb body lookup.
func chaos3BodyAndETagFor(s *chaos3Snapshot, urlPath string) ([]byte, string) {
	etag := `"` + s.label + `"`
	switch {
	case strings.HasSuffix(urlPath, "/InRelease"):
		return s.inRelease, etag
	case strings.HasSuffix(urlPath, "/Packages.gz"):
		return s.packagesGz, etag
	}
	for rel, b := range s.debBodies {
		if strings.HasSuffix(urlPath, "/"+rel) {
			return b, etag
		}
	}
	return nil, ""
}

func chaos3WantBody(s *chaos3Snapshot, urlPath string) []byte {
	b, _ := chaos3BodyAndETagFor(s, urlPath)
	return b
}

// chaos3StallSet is a per-path stall registry shared between the
// upstream httptest handler and the test driver. While a path is in
// the set, the upstream handler blocks until either the path is
// removed from the set or the inbound request's ctx is cancelled.
// This simulates the §12.3 "upstream URL hangs forever" precondition
// per .deb path without taking down the whole server.
type chaos3StallSet struct {
	mu      sync.Mutex
	stalled map[string]chan struct{}
}

func newChaos3StallSet() *chaos3StallSet {
	return &chaos3StallSet{stalled: make(map[string]chan struct{})}
}

// Stall registers urlPath as hung. Subsequent inbound requests to
// that path block in WaitIfStalled until UnstallAll runs (test
// cleanup) or the request ctx fires.
func (s *chaos3StallSet) Stall(urlPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stalled[urlPath]; ok {
		return
	}
	s.stalled[urlPath] = make(chan struct{})
}

// UnstallAll closes every registered stall channel; in-flight blocked
// requests resume. Idempotent.
func (s *chaos3StallSet) UnstallAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, ch := range s.stalled {
		close(ch)
		delete(s.stalled, p)
	}
}

// WaitIfStalled blocks until the path's stall channel closes, ctx
// fires, or the path is no longer in the stall set.
func (s *chaos3StallSet) WaitIfStalled(ctx context.Context, urlPath string) {
	s.mu.Lock()
	ch, ok := s.stalled[urlPath]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// chaos3Server starts an httptest server bound to a swappable
// snapshot pointer plus a per-path stall set. The handler stalls
// before serving so the cache's fetch-side ctx (prefetchCtx or
// fetch.Client total_timeout) is what bounds the wait — never the
// httptest server itself.
func chaos3Server(t *testing.T, current *atomic.Pointer[chaos3Snapshot], stalls *chaos3StallSet) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalls.WaitIfStalled(r.Context(), r.URL.Path)
		if r.Context().Err() != nil {
			return
		}
		snap := current.Load()
		body, etag := chaos3BodyAndETagFor(snap, r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(stalls.UnstallAll)
	return srv
}

// chaos3LogBuf is a mutex-protected sink for the slog JSON handler.
// Tests parse the captured records to assert on §10.2 events
// (adoption_hot_prefetch_*, hot_prefetch_*).
type chaos3LogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *chaos3LogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *chaos3LogBuf) records() []map[string]any {
	b.mu.Lock()
	s := b.buf.String()
	b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

// findAll returns every captured record whose msg field equals msg.
func (b *chaos3LogBuf) findAll(msg string) []map[string]any {
	var out []map[string]any
	for _, r := range b.records() {
		if v, ok := r["msg"].(string); ok && v == msg {
			out = append(out, r)
		}
	}
	return out
}

// findForSnapshot returns the first record with msg whose snapshot_id
// equals want, plus the total count of msg records matching that
// snapshot. The chaos tests target a specific adoption cycle (B);
// the prime-phase A adoption also fires lifecycle events, so
// per-cycle assertions filter on snapshot_id.
func (b *chaos3LogBuf) findForSnapshot(msg string, want int64) (map[string]any, int) {
	var matches []map[string]any
	for _, r := range b.findAll(msg) {
		if extractInt64(r, "snapshot_id") == want {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return nil, 0
	}
	return matches[0], len(matches)
}

// phase3ChaosStack is phase2ChaosStack + log capture + hot-prefetch
// configuration.
type phase3ChaosStack struct {
	handler *Handler
	checker *freshness.Checker
	adopter *freshness.Adopter
	logBuf  *chaos3LogBuf
}

// newPhase3ChaosStack wires Phase 2 + hot-prefetch config. The
// adopter receives a JSON-handler logger writing to logBuf so tests
// assert on adoption_hot_prefetch_* events. fetchTotalTimeout
// controls how long a hung-deb fetch waits before the fetch.Client's
// internal timeout fires — variant 2 (budget=0) sets it short so the
// hung deb's per-deb retry exhaustion is the bound; variants 1 and 3
// set it large so the prefetch budget fires first.
func newPhase3ChaosStack(t *testing.T, upstream *url.URL,
	hotWindow, hotBudget, fetchTotalTimeout time.Duration) *phase3ChaosStack {
	t.Helper()

	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	fc, err := fetch.New(fetch.Options{
		ConnectTimeout:   2 * time.Second,
		TotalTimeout:     fetchTotalTimeout,
		MaxRetries:       0,
		AllowedHostRegex: []string{`^127\.0\.0\.1$`, `^::1$`},
		DenyTargetRanges: nil,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	limiter := hostsem.New(8)
	logBuf := &chaos3LogBuf{}
	captureLogger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
		Cache:             c,
		Fetcher:           &chaos2RewritingFetcher{upstream: upstream, inner: fc},
		Verifier:          chaos2PassVerifier{},
		HostLimiter:       limiter,
		Logger:            captureLogger,
		HotPackagesWindow: hotWindow,
		HotPrefetchBudget: hotBudget,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	checker, err := freshness.New(freshness.Config{
		Cache:       c,
		Fetcher:     fc,
		HostLimiter: limiter,
		Cooldown:    0,
		Refresh:     10 * time.Minute,
		Logger:      silentLogger(),
		Adopter:     adopter,
		LifetimeCtx: context.Background(),
	})
	if err != nil {
		t.Fatalf("freshness.New: %v", err)
	}

	h, err := New(Config{
		Parser:      parser,
		Cache:       c,
		Fetch:       fc,
		HostLimiter: limiter,
		Logger:      silentLogger(),
		Freshness:   checker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &phase3ChaosStack{handler: h, checker: checker, adopter: adopter, logBuf: logBuf}
}

// debCanonicalPath returns the canonical (cache-key) path the §10.2
// events log for a given deb spec. Repo prefix is "/ubuntu/" matching
// chaos3Suite's repo root. The hot-set entries use the same
// canonicalization.
func debCanonicalPath(d chaos3DebSpec) string {
	return "/ubuntu/" + d.rel
}

// debURLs builds the suite-relative URL list (InRelease + Packages.gz
// + every .deb).
func chaos3AllPaths(specs []chaos3DebSpec) []string {
	out := make([]string, 0, 2+len(specs))
	out = append(out, chaos3Suite+"/InRelease", chaos3Suite+"/"+chaos3PackagesGzPath)
	for _, d := range specs {
		out = append(out, debCanonicalPath(d))
	}
	return out
}

// chaos3PrimeAndAdoptA primes the cache by fetching every path under
// snapshot A (so url_path rows are populated with last_requested_at)
// then directly adopts A so suite_freshness.current_snapshot_id
// points at A. After this, the prior current snapshot has the v3
// (Package, Arch) package_hash rows the hot-set Stage 1 needs.
func chaos3PrimeAndAdoptA(t *testing.T, stack *phase3ChaosStack, srv *httptest.Server, snap *chaos3Snapshot, suite freshness.SuiteRef) int64 {
	t.Helper()
	for _, p := range chaos3AllPaths(snap.debSpecs) {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("prime %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), chaos3WantBody(snap, p)) {
			t.Fatalf("prime %s: body=%q, want A's bytes", p, rec.Body.Bytes())
		}
	}
	stack.checker.WaitForAdoptions()

	if err := stack.adopter.Run(context.Background(), suite, snap.inRelease, "\""+snap.label+"\"", ""); err != nil {
		t.Fatalf("adopt %s directly: %v", snap.label, err)
	}
	fresh, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
		suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil || fresh == nil || fresh.CurrentSnapshotID == nil {
		t.Fatalf("post-A adoption: suite_freshness=%+v err=%v", fresh, err)
	}
	return *fresh.CurrentSnapshotID
}

// waitForChaos3Flip polls suite_freshness.current_snapshot_id until it
// changes from priorID, nudging with InRelease GETs to trigger fresh
// adoption attempts. Mirrors waitForFlip from phase2_chaos_test.go but
// in the chaos3 path naming.
func waitForChaos3Flip(t *testing.T, stack *phase3ChaosStack, suite freshness.SuiteRef, srvURL string, priorID int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		fresh, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
			suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
		if err == nil && fresh != nil && fresh.CurrentSnapshotID != nil && *fresh.CurrentSnapshotID != priorID {
			return *fresh.CurrentSnapshotID
		}
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srvURL, suite.SuitePath+"/InRelease"))
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("current_snapshot_id still = %d after %v", priorID, timeout)
	return 0
}

// extractMissing pulls the `missing` slot from an
// adoption_hot_prefetch_partial record and returns it as a sorted
// []string. JSON unmarshal yields []any; the slot may be absent
// (returns nil).
func extractMissing(rec map[string]any) []string {
	raw, ok := rec["missing"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// extractInt64 reads a numeric field from a slog JSON record. JSON
// numbers parse to float64 in untyped maps.
func extractInt64(rec map[string]any, key string) int64 {
	v, ok := rec[key]
	if !ok {
		return 0
	}
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// chaos3DebSpecs builds three (Package, Arch)-distinct .debs whose
// alphabetical order is (aaa < bbb < ccc). Tests pick which one
// hangs to position the hung deb FIRST or LAST in iteration order.
func chaos3DebSpecs() []chaos3DebSpec {
	return []chaos3DebSpec{
		{rel: "pool/main/a/aaa-pkg/aaa-pkg_1.0_amd64.deb", packageName: "aaa-pkg", architecture: "amd64"},
		{rel: "pool/main/b/bbb-pkg/bbb-pkg_1.0_amd64.deb", packageName: "bbb-pkg", architecture: "amd64"},
		{rel: "pool/main/c/ccc-pkg/ccc-pkg_1.0_amd64.deb", packageName: "ccc-pkg", architecture: "amd64"},
	}
}

// chaos3SuiteRef is the SuiteRef the §12.3 chaos tests adopt against.
// scheme/host/path mirror chaos2 fixtures so port-rewriting
// chaos2RewritingFetcher continues to work without per-test setup.
func chaos3SuiteRef(upstream *url.URL) freshness.SuiteRef {
	return freshness.SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   upstream.Hostname(),
		SuitePath:       chaos3Suite,
	}
}

// TestPhase3Chaos_HotPrefetchBudget_HungFirst is SPEC3 §12.3 variant 1:
// the hung deb is FIRST in iteration order. With budget=200ms and the
// hung deb's fetch hanging forever, the hot-prefetch loop attempts the
// hung one, prefetchCtx fires DeadlineExceeded mid-flight,
// fetchHotDeb logs hot_prefetch_deb_failed (the in-flight-cancel-as-
// failed contract per the §12.3 ordering pin), and the next loop
// iteration's top check emits adoption_hot_prefetch_partial with
// `missing` set to the N-1 remaining (unattempted) paths — NOT the
// hung one. Adoption flips despite the cancelled prefetch (CommitAdoption
// runs under adoptionCtx, the §7.5 step-10 context split). On first
// post-flip request, the hung deb's path returns 502 — its url_path
// row was never inserted because no prefetch succeeded for it AND
// the snapshot-B miss-path fetch is still hung.
func TestPhase3Chaos_HotPrefetchBudget_HungFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 3 chaos test skipped in -short mode")
	}

	specs := chaos3DebSpecs()
	snapA := makeChaos3Snapshot("A", specs)
	snapB := makeChaos3Snapshot("B", specs)
	hung := specs[0] // aaa-pkg — alphabetical first; hot-set ORDER BY visits this first
	hungCanonical := debCanonicalPath(hung)
	// Stall key matches the URL path the upstream httptest handler
	// observes — the cache fetches at canonical /ubuntu/pool/...
	// (debCanonicalPath), NOT the suite-relative /ubuntu/dists/noble/...
	// that the test driver uses for prime GETs. Stall on the canonical
	// path so the upstream's WaitIfStalled key matches.
	hungUpstreamPath := hungCanonical

	var current atomic.Pointer[chaos3Snapshot]
	current.Store(&snapA)

	stalls := newChaos3StallSet()
	srv := chaos3Server(t, &current, stalls)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Budget 200ms, fetch.Client total_timeout 10s — budget fires
	// first while the hung deb is in flight, the §12.3 variant 1
	// preconditions.
	stack := newPhase3ChaosStack(t, upstreamURL, 24*time.Hour, 200*time.Millisecond, 10*time.Second)
	suite := chaos3SuiteRef(upstreamURL)
	priorID := chaos3PrimeAndAdoptA(t, stack, srv, &snapA, suite)

	// Swap upstream to B and stall the hung deb's path before
	// triggering adoption of B. The stall registers against the
	// suite-relative URL the upstream handler sees (chaos3Suite +
	// "/" + rel); the canonical cache-side path is "/ubuntu/" + rel.
	current.Store(&snapB)
	stalls.Stall(hungUpstreamPath)

	// Trigger adoption: a single InRelease GET drives the freshness
	// conditional path to observe the new ETag and spawn the
	// adoption goroutine. We don't need to gate metadata fetches —
	// the .deb hot prefetch is the slow phase, and the budget caps
	// it at 200ms.
	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos3Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// The flip must happen even though the hot prefetch was
	// cancelled mid-loop — that is the §7.5 step-10 contract.
	newID := waitForChaos3Flip(t, stack, suite, srv.URL, priorID, 15*time.Second)
	stack.checker.WaitForAdoptions()
	if newID == priorID {
		t.Fatalf("flip did not happen: newID=%d == priorID=%d", newID, priorID)
	}

	// All §10.2 assertions filter on snapshot_id == newID so the
	// prime-A adoption's lifecycle events (hot_count=0) don't
	// pollute B's per-cycle counts.
	if _, n := stack.logBuf.findForSnapshot("adoption_hot_prefetch_started", newID); n != 1 {
		t.Errorf("adoption_hot_prefetch_started for newID=%d: count=%d, want 1", newID, n)
	}

	// hot_prefetch_deb_failed must fire for the hung deb's canonical
	// path. SPEC3 §12.3 variant 1: in-flight cancellation by budget
	// elapse maps to deb_failed (NOT cancelled-and-silent).
	foundHungInFailed := false
	for _, r := range stack.logBuf.findAll("hot_prefetch_deb_failed") {
		if extractInt64(r, "snapshot_id") != newID {
			continue
		}
		if p, _ := r["path"].(string); p == hungCanonical {
			foundHungInFailed = true
			break
		}
	}
	if !foundHungInFailed {
		t.Errorf("hot_prefetch_deb_failed: no record for hung path %s on snapshot %d", hungCanonical, newID)
	}

	// adoption_hot_prefetch_partial fires once with `missing` = the
	// N-1 paths that were never attempted. The hung deb is in
	// deb_failed instead — must NOT appear in missing.
	partialRec, partialN := stack.logBuf.findForSnapshot("adoption_hot_prefetch_partial", newID)
	if partialN != 1 {
		t.Fatalf("adoption_hot_prefetch_partial for newID=%d: count=%d, want 1", newID, partialN)
	}
	missing := extractMissing(partialRec)
	wantMissing := []string{
		debCanonicalPath(specs[1]),
		debCanonicalPath(specs[2]),
	}
	sort.Strings(wantMissing)
	if !equalStringSlice(missing, wantMissing) {
		t.Errorf("partial.missing = %v, want %v", missing, wantMissing)
	}
	for _, p := range missing {
		if p == hungCanonical {
			t.Errorf("partial.missing contains hung deb path %s — should be in deb_failed instead", p)
		}
	}

	// adoption_hot_prefetch_complete: hot_count=N, fetched=0,
	// failed=1, mismatched=0, unattempted=N-1.
	completeRec, completeN := stack.logBuf.findForSnapshot("adoption_hot_prefetch_complete", newID)
	if completeN != 1 {
		t.Fatalf("adoption_hot_prefetch_complete for newID=%d: count=%d, want 1", newID, completeN)
	}
	if got := extractInt64(completeRec, "hot_count"); got != int64(len(specs)) {
		t.Errorf("complete.hot_count=%d, want %d", got, len(specs))
	}
	if got := extractInt64(completeRec, "fetched"); got != 0 {
		t.Errorf("complete.fetched=%d, want 0", got)
	}
	if got := extractInt64(completeRec, "failed"); got != 1 {
		t.Errorf("complete.failed=%d, want 1", got)
	}
	if got := extractInt64(completeRec, "mismatched"); got != 0 {
		t.Errorf("complete.mismatched=%d, want 0", got)
	}
	if got := extractInt64(completeRec, "unattempted"); got != int64(len(specs)-1) {
		t.Errorf("complete.unattempted=%d, want %d", got, len(specs)-1)
	}

	// First post-flip request for the hung deb's path: cache miss
	// (no prefetched url_path was inserted; package_hash for B has
	// the new declared hash so any stale row gets evicted) → fetch
	// upstream → still stalled → ctx fires (handler-side fetch.Client
	// total_timeout 10s; the test caps wall-clock at the budget +
	// some slack). To keep this test fast, we use a request with a
	// short ctx so the miss-path fetch fails quickly.
	hungReq := proxyReq("GET", srv.URL, hungCanonical)
	hungCtx, hungCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer hungCancel()
	hungReq = hungReq.WithContext(hungCtx)
	hungRec := httptest.NewRecorder()
	stack.handler.ServeHTTP(hungRec, hungReq)
	if hungRec.Code != http.StatusBadGateway {
		t.Errorf("post-flip hung deb: status=%d, want 502 (body=%q)", hungRec.Code, hungRec.Body.String())
	}
}

// TestPhase3Chaos_HotPrefetchBudgetZero_HardCancel is SPEC3 §12.3
// variant 2: with hot_prefetch_budget = 0s, prefetchCtx == adoptionCtx
// (no wall-clock budget), so a hung deb's per-deb retry exhaustion is
// what bounds the loop — `upstream.total_timeout × upstream.max_retries`.
// adoption_hot_prefetch_partial does NOT fire (no DeadlineExceeded
// path). The flip still happens; the test guards against
// "misconfigured budget = 0 indefinitely stalls adoption" by capping
// the test wall-clock at ~3× total_timeout.
func TestPhase3Chaos_HotPrefetchBudgetZero_HardCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 3 chaos test skipped in -short mode")
	}

	specs := chaos3DebSpecs()
	snapA := makeChaos3Snapshot("A", specs)
	snapB := makeChaos3Snapshot("B", specs)
	hung := specs[0]
	hungUpstreamPath := debCanonicalPath(hung)

	var current atomic.Pointer[chaos3Snapshot]
	current.Store(&snapA)
	stalls := newChaos3StallSet()
	srv := chaos3Server(t, &current, stalls)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Budget 0 (no wall-clock cap), fetch.Client total_timeout 1s
	// (max_retries=0 inside fetch.New — so the per-deb bound is
	// total_timeout × 1 = 1s). The hung deb fails after ~1s; loop
	// continues; adoption flips.
	stack := newPhase3ChaosStack(t, upstreamURL, 24*time.Hour, 0, 1*time.Second)
	suite := chaos3SuiteRef(upstreamURL)
	priorID := chaos3PrimeAndAdoptA(t, stack, srv, &snapA, suite)

	current.Store(&snapB)
	stalls.Stall(hungUpstreamPath)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos3Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Flip wait must exceed total_timeout but not be unbounded.
	newID := waitForChaos3Flip(t, stack, suite, srv.URL, priorID, 10*time.Second)
	stack.checker.WaitForAdoptions()
	if newID == priorID {
		t.Fatalf("flip did not happen: newID=%d == priorID=%d", newID, priorID)
	}

	// adoption_hot_prefetch_partial MUST NOT fire for B's adoption
	// (no DeadlineExceeded on prefetchCtx — there is no prefetch
	// timeout when budget=0).
	if _, n := stack.logBuf.findForSnapshot("adoption_hot_prefetch_partial", newID); n != 0 {
		t.Errorf("adoption_hot_prefetch_partial for newID=%d: count=%d, want 0", newID, n)
	}

	// hot_prefetch_deb_failed must still fire for the hung deb. The
	// fetch.Client's internal total_timeout fires while the outer
	// prefetchCtx is alive — that lands in fetchHotDeb's "genuine
	// upstream failure" branch (ctx.Err() == nil) → deb_failed log,
	// failed bucket.
	foundHung := false
	for _, r := range stack.logBuf.findAll("hot_prefetch_deb_failed") {
		if extractInt64(r, "snapshot_id") != newID {
			continue
		}
		if p, _ := r["path"].(string); p == debCanonicalPath(hung) {
			foundHung = true
			break
		}
	}
	if !foundHung {
		t.Errorf("hot_prefetch_deb_failed: no record for hung path on snapshot %d", newID)
	}

	// adoption_hot_prefetch_complete: fetched=N-1 (the two non-hung
	// debs warmed), failed=1 (the hung), unattempted=0.
	completeRec, completeN := stack.logBuf.findForSnapshot("adoption_hot_prefetch_complete", newID)
	if completeN != 1 {
		t.Fatalf("adoption_hot_prefetch_complete for newID=%d: count=%d, want 1", newID, completeN)
	}
	if got := extractInt64(completeRec, "fetched"); got != int64(len(specs)-1) {
		t.Errorf("complete.fetched=%d, want %d", got, len(specs)-1)
	}
	if got := extractInt64(completeRec, "failed"); got != 1 {
		t.Errorf("complete.failed=%d, want 1", got)
	}
	if got := extractInt64(completeRec, "unattempted"); got != 0 {
		t.Errorf("complete.unattempted=%d, want 0", got)
	}
}

// TestPhase3Chaos_HotPrefetchBudget_HungLast is SPEC3 §12.3 variant 3:
// hung deb is LAST in iteration order. The first N-1 debs fetch
// successfully; the budget elapses while the last deb is in flight;
// hot_prefetch_deb_failed fires for the hung one;
// adoption_hot_prefetch_partial does NOT fire (the loop exits naturally
// after the failed bucket — no further iterations to trigger the
// top-of-iteration partial check); complete reports fetched=N-1,
// failed=1, unattempted=0. This pins the partial-event contract from
// the inverse direction: partial fires only when there's genuine
// unattempted-queue residue.
func TestPhase3Chaos_HotPrefetchBudget_HungLast(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 3 chaos test skipped in -short mode")
	}

	specs := chaos3DebSpecs()
	snapA := makeChaos3Snapshot("A", specs)
	snapB := makeChaos3Snapshot("B", specs)
	hung := specs[len(specs)-1] // ccc-pkg — alphabetical last
	hungUpstreamPath := debCanonicalPath(hung)

	var current atomic.Pointer[chaos3Snapshot]
	current.Store(&snapA)
	stalls := newChaos3StallSet()
	srv := chaos3Server(t, &current, stalls)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Budget 500ms — the first two debs fetch in well under that
	// (loopback bytes), then the budget catches the third in flight.
	// fetch.Client total_timeout large so the test's bound is the
	// budget, not the fetch timeout.
	stack := newPhase3ChaosStack(t, upstreamURL, 24*time.Hour, 500*time.Millisecond, 10*time.Second)
	suite := chaos3SuiteRef(upstreamURL)
	priorID := chaos3PrimeAndAdoptA(t, stack, srv, &snapA, suite)

	current.Store(&snapB)
	stalls.Stall(hungUpstreamPath)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos3Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}

	newID := waitForChaos3Flip(t, stack, suite, srv.URL, priorID, 15*time.Second)
	stack.checker.WaitForAdoptions()
	if newID == priorID {
		t.Fatalf("flip did not happen: newID=%d == priorID=%d", newID, priorID)
	}

	// adoption_hot_prefetch_partial MUST NOT fire for B's adoption —
	// the hung deb at the end of the queue triggers in-flight
	// cancellation, but no subsequent iteration runs to fire partial
	// (queue empty at end-of-loop).
	if _, n := stack.logBuf.findForSnapshot("adoption_hot_prefetch_partial", newID); n != 0 {
		t.Errorf("adoption_hot_prefetch_partial for newID=%d: count=%d, want 0", newID, n)
	}

	// hot_prefetch_deb_failed must fire for the hung deb (in-flight
	// cancellation by budget — fetchHotDeb's DeadlineExceeded branch).
	foundHung := false
	for _, r := range stack.logBuf.findAll("hot_prefetch_deb_failed") {
		if extractInt64(r, "snapshot_id") != newID {
			continue
		}
		if p, _ := r["path"].(string); p == debCanonicalPath(hung) {
			foundHung = true
			break
		}
	}
	if !foundHung {
		t.Errorf("hot_prefetch_deb_failed: no record for hung path on snapshot %d", newID)
	}

	// adoption_hot_prefetch_complete: fetched=N-1, failed=1, unattempted=0.
	completeRec, completeN := stack.logBuf.findForSnapshot("adoption_hot_prefetch_complete", newID)
	if completeN != 1 {
		t.Fatalf("adoption_hot_prefetch_complete for newID=%d: count=%d, want 1", newID, completeN)
	}
	if got := extractInt64(completeRec, "fetched"); got != int64(len(specs)-1) {
		t.Errorf("complete.fetched=%d, want %d", got, len(specs)-1)
	}
	if got := extractInt64(completeRec, "failed"); got != 1 {
		t.Errorf("complete.failed=%d, want 1", got)
	}
	if got := extractInt64(completeRec, "unattempted"); got != 0 {
		t.Errorf("complete.unattempted=%d, want 0", got)
	}
}

// equalStringSlice returns true if a and b are equal element-wise.
// Both slices must be sorted by the caller; this is purely a deep
// equality check.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
