package freshness

// SPEC6_7 coverage: recording integrity-class member skips for repair,
// in-adoption retry, and the freshness-tick repair pass. Born from the
// 2026-06-09 incident where a round-robin mirror served the previous
// publication generation for Contents-amd64.gz during adoption; the
// member was skipped and the suite then served authoritative 404s for
// it until upstream re-published InRelease ~17h later.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// currentSnapshotID resolves the suite's current snapshot id, failing
// the test when the suite has not adopted.
func currentSnapshotID(t *testing.T, env *adoptionTestEnv) int64 {
	t.Helper()
	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("suite has no current snapshot")
	}
	return *sf.CurrentSnapshotID
}

// TestAdopter_IntegritySkipRecordedForRepair: an optional member
// skipped over an integrity failure (the stale-mirror signature) must
// land a snapshot_skipped_member row carrying its declaration — the
// trust anchor the repair pass re-fetches against. A 4xx skip on a
// publication artifact must NOT be repairable.
func TestAdopter_IntegritySkipRecordedForRepair(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newTolerantAdopter(t, env)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	declaredContents := []byte(strings.Repeat("X", 1000))
	releaseText, declared := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          declaredContents,
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short")) // served 5 vs declared 1000
	env.fetcher.fail404(base + "Contents-amd64")               // permanent decoy

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption should tolerate the optional failures, got %v", err)
	}
	snapID := currentSnapshotID(t, env)

	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatalf("ListRepairableSkippedMembers: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("repairable rows = %d, want 1 (only the integrity skip): %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Path != "Contents-amd64.gz" {
		t.Errorf("path = %q, want Contents-amd64.gz", got.Path)
	}
	if got.DeclaredSHA256 != declared["Contents-amd64.gz"] {
		t.Errorf("declared_sha256 = %s, want %s", got.DeclaredSHA256, declared["Contents-amd64.gz"])
	}
	if got.Size != 1000 {
		t.Errorf("size = %d, want 1000", got.Size)
	}
	if got.Reason != cache.SkipReasonOptionalMemberIntegrity {
		t.Errorf("reason = %q, want %q", got.Reason, cache.SkipReasonOptionalMemberIntegrity)
	}
	if got.Detail != "served 5 vs declared 1000" {
		t.Errorf("detail = %q, want \"served 5 vs declared 1000\"", got.Detail)
	}
}

// TestAdopter_SuccessLogCarriesIntegritySkipCount: the adoption_success
// line must break the integrity-class skips out of skipped_count so an
// operator (or alert) can see "this snapshot went live degraded"
// without re-deriving it from per-member WARN lines.
func TestAdopter_SuccessLogCarriesIntegritySkipCount(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	var logBuf bytes.Buffer
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		Logger:                         slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("X", 1000)),
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short"))
	env.fetcher.fail404(base + "Contents-amd64")

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logBuf.String()
	var successLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "msg=adoption_success") {
			successLine = line
		}
	}
	if successLine == "" {
		t.Fatalf("no adoption_success line in log output:\n%s", out)
	}
	if !strings.Contains(successLine, "skipped_integrity_count=1") {
		t.Errorf("adoption_success missing skipped_integrity_count=1: %s", successLine)
	}
	// The aggregate count still includes both skip classes.
	if !strings.Contains(successLine, "skipped_count=2") {
		t.Errorf("adoption_success missing skipped_count=2: %s", successLine)
	}
}

// seqFetcher serves a per-URL SEQUENCE of canned outcomes (body or
// error), consuming one per Fetch call and repeating the final entry
// once the sequence is exhausted. Models a mirror whose answer changes
// between attempts — the mid-sync round-robin pool from the 2026-06-09
// incident.
type seqFetcher struct {
	mu   sync.Mutex
	seq  map[string][]seqStep
	seen []string
}

type seqStep struct {
	body []byte
	err  error
}

func newSeqFetcher() *seqFetcher {
	return &seqFetcher{seq: make(map[string][]seqStep)}
}

func (f *seqFetcher) push(url string, body []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq[url] = append(f.seq[url], seqStep{body: body, err: err})
}

// countFor reports how many Fetch calls targeted url.
func (f *seqFetcher) countFor(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.seen {
		if s == url {
			n++
		}
	}
	return n
}

func (f *seqFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	f.mu.Lock()
	f.seen = append(f.seen, target.URL)
	steps, ok := f.seq[target.URL]
	if !ok || len(steps) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("seqFetcher: no canned response for %s", target.URL)
	}
	step := steps[0]
	if len(steps) > 1 {
		f.seq[target.URL] = steps[1:]
	}
	f.mu.Unlock()
	if step.err != nil {
		return nil, step.err
	}
	if _, err := dst.Write(step.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(step.body))}, nil
}

// makeReleaseAcquireByHash mirrors makeRelease but advertises
// Acquire-By-Hash: yes, so adoptMember prefers the content-addressed
// by-hash URL per member.
func makeReleaseAcquireByHash(members map[string][]byte) ([]byte, map[string]string) {
	hashes := make(map[string]string, len(members))
	var sb strings.Builder
	sb.WriteString("Origin: Test\nSuite: noble\nAcquire-By-Hash: yes\nSHA256:\n")
	for p, body := range members {
		h := shaOf(body)
		hashes[p] = h
		fmt.Fprintf(&sb, " %s %d %s\n", h, len(body), p)
	}
	return []byte(sb.String()), hashes
}

// newRetryAdopter builds a tolerant Adopter with member retry enabled
// and an instant, recorded sleep seam.
func newRetryAdopter(t *testing.T, c *cache.Cache, f AdoptionFetcher, retries int, delay time.Duration, sleeps *[]time.Duration) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          c,
		Fetcher:                        f,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		MemberRetryCount:               retries,
		MemberRetryDelay:               delay,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	ad.retrySleep = func(ctx context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	return ad
}

// TestAdopter_MemberRetryHealsStaleMirrorViaByHash is the 2026-06-09
// incident replay: a round-robin mirror mid-sync 404s the by-hash URL
// for a root-level Contents-amd64.gz while its canonical path serves
// the PREVIOUS publication generation (Content-Length mismatch). One
// retry later the by-hash blob has synced; the member adopts intact —
// no skip, no degraded snapshot, no repair debt.
func TestAdopter_MemberRetryHealsStaleMirrorViaByHash(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	goodContents := []byte(strings.Repeat("C", 100))
	staleContents := []byte(strings.Repeat("c", 90)) // previous generation: wrong size
	releaseText, declared := makeReleaseAcquireByHash(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          goodContents,
	})

	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	sf := newSeqFetcher()
	// Packages by-hash: good from the first attempt.
	sf.push(base+"main/binary-amd64/by-hash/SHA256/"+declared["main/binary-amd64/Packages"], pkgs, nil)
	// Contents by-hash: not yet synced (404), then synced.
	contentsByHash := base + "by-hash/SHA256/" + declared["Contents-amd64.gz"]
	sf.push(contentsByHash, nil, &fetch.StatusError{Code: 404})
	sf.push(contentsByHash, goodContents, nil)
	// Contents canonical: serves the stale generation throughout.
	sf.push(base+"Contents-amd64.gz", staleContents, nil)

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, sf, 2, 25*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "Contents-amd64.gz"); err != nil {
		t.Errorf("Contents-amd64.gz member missing after retry heal: %v", err)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("repairable rows = %d, want 0 (retry healed in-adoption)", len(rows))
	}
	if len(sleeps) != 1 || sleeps[0] != 25*time.Millisecond {
		t.Errorf("sleeps = %v, want [25ms]", sleeps)
	}
	if got := sf.countFor(contentsByHash); got != 2 {
		t.Errorf("by-hash attempts = %d, want 2", got)
	}
}

// TestAdopter_MemberRetryExhaustedStillSkips: when every retry fails,
// the tolerant skip path runs exactly as before — adoption succeeds,
// and the skip is recorded for the freshness-tick repair pass.
func TestAdopter_MemberRetryExhaustedStillSkips(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("X", 1000)),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short")) // wrong size, every attempt

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 10*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("repairable rows = %d, want 1", len(rows))
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"Contents-amd64.gz" {
			attempts++
		}
	}
	if attempts != 3 {
		t.Errorf("canonical attempts = %d, want 3 (1 + 2 retries)", attempts)
	}
	if len(sleeps) != 2 {
		t.Errorf("sleeps = %v, want 2 entries", sleeps)
	}
}

// TestAdopter_404SkipNotRetried: a 404/410 on a declared member is the
// permanent "declared but does not serve" publication artifact — it
// must skip immediately, never burn retry delay.
func TestAdopter_404SkipNotRetried(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.fail404(base + "Contents-amd64")

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 10*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"Contents-amd64" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Errorf("404 member attempts = %d, want 1 (no retry)", attempts)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
	snapID := currentSnapshotID(t, env)
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("4xx skip must not be recorded as repairable: %+v", rows)
	}
}

// TestAdopter_IndexTargetRetriedThenFatal: retries apply to
// IndexTargets too (they benefit most — a deferred adoption costs a
// whole tick), but exhaustion stays FATAL: the REC 1 fatality boundary
// is untouched.
func TestAdopter_IndexTargetRetriedThenFatal(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	good := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": good,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", []byte("short, wrong length"))

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 1, 10*time.Millisecond, &sleeps)

	err := ad.Run(ctx, env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("want ErrAdoptionMemberFetchFailed after exhausted retries, got %v", err)
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"main/binary-amd64/Packages" {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("IndexTarget attempts = %d, want 2 (1 + 1 retry)", attempts)
	}
}

// adoptWithIntegritySkip drives one tolerant adoption whose
// Contents-amd64.gz member fails the size gate (stale mirror), leaving
// a current snapshot with one repairable skip row. Returns the adopter
// (repair-enabled), the good bytes the member should eventually carry,
// and the snapshot id.
func adoptWithIntegritySkip(t *testing.T, env *adoptionTestEnv) (*Adopter, []byte, int64) {
	t.Helper()
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	goodContents := []byte(strings.Repeat("C", 100))
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          goodContents,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte(strings.Repeat("c", 90))) // stale generation

	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		RepairSkippedMembers:           true,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fixture: repairable rows = %d (err=%v), want 1", len(rows), err)
	}
	return ad, goodContents, snapID
}

// TestRepairSkippedMembers_PromotesMember: once the mirror finishes
// syncing (the canonical path now serves the declared bytes), the
// repair pass promotes the member — canonical AND by-hash alias —
// into the still-current snapshot and consumes the skip row.
func TestRepairSkippedMembers_PromotesMember(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad, goodContents, snapID := adoptWithIntegritySkip(t, env)

	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"Contents-amd64.gz", goodContents) // mirror synced

	ad.RepairSkippedMembers(ctx, env.suite, snapID)

	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "Contents-amd64.gz"); err != nil {
		t.Errorf("canonical member missing after repair: %v", err)
	}
	alias := "by-hash/SHA256/" + shaOf(goodContents)
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, alias); err != nil {
		t.Errorf("by-hash alias missing after repair: %v", err)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("skip row not consumed: %+v", rows)
	}
}

// TestRepairSkippedMembers_FailureBumpsRetryCount: while the mirror is
// still stale the pass must leave the snapshot untouched and record
// the attempt.
func TestRepairSkippedMembers_FailureBumpsRetryCount(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad, _, snapID := adoptWithIntegritySkip(t, env)

	// Mirror still serves the stale generation — no fetcher change.
	ad.RepairSkippedMembers(ctx, env.suite, snapID)

	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "Contents-amd64.gz"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("member must not be promoted from stale bytes (err=%v)", err)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("repairable rows = %d, want 1", len(rows))
	}
	if rows[0].RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", rows[0].RetryCount)
	}
}

// TestRepairSkippedMembers_DisabledByConfig: the kill switch must stop
// the pass before any upstream traffic.
func TestRepairSkippedMembers_DisabledByConfig(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	_, goodContents, snapID := adoptWithIntegritySkip(t, env)

	off, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		RepairSkippedMembers:           false,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"Contents-amd64.gz", goodContents)
	before := len(env.fetcher.requested())

	off.RepairSkippedMembers(ctx, env.suite, snapID)

	if got := len(env.fetcher.requested()); got != before {
		t.Errorf("disabled repair pass contacted upstream (%d new fetches)", got-before)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("skip row count = %d, want 1 (untouched)", len(rows))
	}
}

// TestChecker_FreshTickRepairsSkippedMembers is the end-to-end SPEC6_7
// §3 path: a freshness check that finds the suite unchanged (the
// steady state for slow-publishing suites) hands the per-suite mutex
// to a repair goroutine, which heals the degraded snapshot — the
// recovery the 2026-06-09 incident lacked (it had to wait ~17h for the
// next InRelease publication).
func TestChecker_FreshTickRepairsSkippedMembers(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	// The Checker validates upstream against a real HTTP server; the
	// suite host must be the loopback the test fetcher allowlists.
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"}

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	goodContents := []byte(strings.Repeat("C", 100))
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          goodContents,
	})
	base := "http://127.0.0.1/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte(strings.Repeat("c", 90))) // stale at adoption time

	// Upstream serves the SAME InRelease bytes — freshness sees
	// "unchanged", the no-adoption steady state.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(releaseText)
	}))
	defer srv.Close()

	// Anchor url_path row with the port-correct upstream URL; the
	// CommitAdoption anchor sync preserves it on conflict.
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http",
		CanonicalHost:   "127.0.0.1",
		Path:            "/dists/noble/InRelease",
		UpstreamURL:     srv.URL + "/dists/noble/InRelease",
		IsMetadata:      true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}

	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		RepairSkippedMembers:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	if rows, _ := env.cache.ListRepairableSkippedMembers(ctx, snapID); len(rows) != 1 {
		t.Fatalf("fixture: want 1 repairable row, got %d", len(rows))
	}

	// Mirror syncs; the next freshness tick should repair.
	env.fetcher.put(base+"Contents-amd64.gz", goodContents)

	checker, err := New(Config{
		Cache:       env.cache,
		Fetcher:     newTestFetcher(t),
		HostLimiter: hostsem.New(8),
		Adopter:     ad,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New checker: %v", err)
	}
	checker.Check(ctx, "http", "127.0.0.1", "/dists/noble")
	checker.WaitForAdoptions()

	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "Contents-amd64.gz"); err != nil {
		t.Errorf("member not repaired by fresh tick: %v", err)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("skip rows remain after tick repair: %+v", rows)
	}
}

// TestIndexTargetGroup pins the SPEC6_7 §6 group classifier: an
// IndexTarget's compression variants collapse to one group key (the
// path minus codec suffix); pdiff Index manifests and non-IndexTargets
// are not group members (a present pdiff with no actual index must not
// count as "the index is served").
func TestIndexTargetGroup(t *testing.T) {
	cases := []struct {
		path, group, arch string
		ok                bool
	}{
		{"main/binary-amd64/Packages", "main/binary-amd64/Packages", "amd64", true},
		{"main/binary-amd64/Packages.gz", "main/binary-amd64/Packages", "amd64", true},
		{"main/binary-amd64/Packages.xz", "main/binary-amd64/Packages", "amd64", true},
		{"universe/binary-armhf/Packages.zst", "universe/binary-armhf/Packages", "armhf", true},
		{"main/source/Sources.gz", "main/source/Sources", "source", true},
		{"main/binary-amd64/Packages.diff/Index", "", "", false},
		{"main/source/Sources.diff/Index", "", "", false},
		{"Contents-amd64.gz", "", "", false},
		{"main/dep11/Components-amd64.yml.gz", "", "", false},
		{"InRelease", "", "", false},
	}
	for _, tc := range cases {
		group, arch, ok := indexTargetGroup(tc.path)
		if group != tc.group || arch != tc.arch || ok != tc.ok {
			t.Errorf("indexTargetGroup(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.path, group, arch, ok, tc.group, tc.arch, tc.ok)
		}
	}
}

// newRequiredArchAdopter builds a tolerant adopter requiring the given
// architectures' IndexTarget groups to be served.
func newRequiredArchAdopter(t *testing.T, env *adoptionTestEnv, required []string) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		RequiredArchitectures:          required,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	return ad
}

// TestAdopter_RequiredArchGroupAllMissingDefersAdoption: when EVERY
// declared variant of a required arch's index group is missing, the
// adoption must fail (defer) — committing would hard-fail `apt update`
// for every client of that arch until the next InRelease publication.
// This closes the prod-observed hole where the 4xx-skip path applied
// to IndexTargets too.
func TestAdopter_RequiredArchGroupAllMissingDefersAdoption(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newRequiredArchAdopter(t, env, []string{"amd64"})

	contents := []byte("contents bytes")
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages":    []byte("decoy"),
		"main/binary-amd64/Packages.gz": []byte("decoy gz"),
		"Contents-amd64.gz":             contents,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.fail404(base + "main/binary-amd64/Packages")
	env.fetcher.fail404(base + "main/binary-amd64/Packages.gz")
	env.fetcher.put(base+"Contents-amd64.gz", contents)

	err := ad.Run(ctx, env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("want ErrAdoptionMemberFetchFailed (deferred), got %v", err)
	}
	memberPath, detail := memberErrorFields(err)
	if memberPath != "main/binary-amd64/Packages" {
		t.Errorf("failing member path = %q, want the group key", memberPath)
	}
	if !strings.Contains(detail, "required") {
		t.Errorf("detail %q should name the required-arch rule", detail)
	}
	// No snapshot adopted — the previous (here: none) keeps serving.
	sf, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err == nil && sf.CurrentSnapshotID != nil {
		t.Errorf("degraded snapshot was adopted (id=%d)", *sf.CurrentSnapshotID)
	}
}

// TestAdopter_RequiredArchGroupOneVariantSuffices: Ubuntu declares
// uncompressed Packages it never serves; one served variant proves the
// group and the sibling 404s stay harmless skips.
func TestAdopter_RequiredArchGroupOneVariantSuffices(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newRequiredArchAdopter(t, env, []string{"amd64"})

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages":    pkgs, // declared, 404s (decoy)
		"main/binary-amd64/Packages.gz": gzipBytes(pkgs),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.fail404(base + "main/binary-amd64/Packages")
	env.fetcher.put(base+"main/binary-amd64/Packages.gz", gzipBytes(pkgs))

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("one served variant must satisfy the group: %v", err)
	}
}

// TestAdopter_RequiredArchIgnoresForeignGroups: a foreign arch the
// operator did NOT require may be entirely absent (the ports-only
// publication artifact) without failing adoption.
func TestAdopter_RequiredArchIgnoresForeignGroups(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newRequiredArchAdopter(t, env, []string{"amd64"})

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/binary-armhf/Packages": []byte("ports-only"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.fail404(base + "main/binary-armhf/Packages")

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("foreign-arch absence must not defer adoption: %v", err)
	}
}

// TestAdopter_RequiredArchGuardIsOptIn: with no required_architectures
// the pre-SPEC6_7 behavior holds — even a fully-missing amd64 group
// adopts (with package coverage dropped). The guard is opt-in because
// upstreams legitimately declare arches they never serve, and the
// operator is the only party who knows which arches their fleet needs.
func TestAdopter_RequiredArchGuardIsOptIn(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newTolerantAdopter(t, env)

	contents := []byte("contents bytes")
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": []byte("never served"),
		"Contents-amd64.gz":          contents,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.fail404(base + "main/binary-amd64/Packages")
	env.fetcher.put(base+"Contents-amd64.gz", contents)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("without required_architectures the guard must not fire: %v", err)
	}
}

// TestAdopter_4xxIndexTargetSkipReason: a 404-skipped IndexTarget is a
// categorically worse signal than a 404-skipped optional member — it
// gets its own reason value so operators can alert on it without
// drowning in Ubuntu's ~160 ordinary publication-artifact skips.
func TestAdopter_4xxIndexTargetSkipReason(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	var logBuf bytes.Buffer
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		Logger:                         slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/binary-armhf/Packages": []byte("ports-only"),
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.fail404(base + "main/binary-armhf/Packages")
	env.fetcher.fail404(base + "Contents-amd64")

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logBuf.String()
	var armhfLine, contentsLine string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=adoption_member_skipped") {
			continue
		}
		if strings.Contains(line, "path=main/binary-armhf/Packages") {
			armhfLine = line
		}
		if strings.Contains(line, "path=Contents-amd64 ") {
			contentsLine = line
		}
	}
	if !strings.Contains(armhfLine, "reason=4xx_index_target") {
		t.Errorf("IndexTarget 404 skip should carry reason=4xx_index_target: %s", armhfLine)
	}
	if !strings.Contains(contentsLine, "reason=4xx") || strings.Contains(contentsLine, "index_target") {
		t.Errorf("optional 404 skip should keep reason=4xx: %s", contentsLine)
	}
}

func TestNewAdopter_RejectsNegativeRetryConfig(t *testing.T) {
	env := newAdoptionTestEnv(t)
	if _, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(1), MemberRetryCount: -1,
	}); err == nil {
		t.Error("MemberRetryCount=-1 accepted, want error")
	}
	if _, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(1), MemberRetryDelay: -time.Second,
	}); err == nil {
		t.Error("MemberRetryDelay=-1s accepted, want error")
	}
}
