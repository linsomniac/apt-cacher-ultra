package handler

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// fakeAdoptions is a handler.AdoptionWaiter: inFlight answers
// AdoptionsInFlight (asks counts the calls: one per strict-mode refusal
// the miss path's runFetch considers); WaitHostAdoptions records the
// request's age, runs during (the adoption that finishes while the request
// waits) and returns ok.
type fakeAdoptions struct {
	inFlight bool
	ok       bool
	during   func()
	waits    atomic.Int32
	asks     atomic.Int32
	mu       sync.Mutex
	ages     []time.Duration // time.Since(start) at each wait
}

func (f *fakeAdoptions) AdoptionsInFlight(string) bool { f.asks.Add(1); return f.inFlight }

func (f *fakeAdoptions) WaitHostAdoptions(_ context.Context, _ string, start time.Time) bool {
	f.mu.Lock()
	f.ages = append(f.ages, time.Since(start))
	f.mu.Unlock()
	f.waits.Add(1)
	if f.during != nil {
		f.during()
	}
	return f.ok
}

// heldSlots is holdHostSlotsUntilJoined's hold: call is the singleflight
// call whose joiner released it (set before the slots come back).
type heldSlots struct {
	mu   sync.Mutex
	call *sfCall
}

// waitGone waits until the joined call has left h's singleflight map (its
// leader deletes it just after waking its joiners), bounded so a
// regression fails instead of hanging.
func (s *heldSlots) waitGone(h *Handler) {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.mu.Lock()
		c := s.call
		s.mu.Unlock()
		h.sf.mu.Lock()
		mapped := false
		for _, m := range h.sf.calls {
			mapped = mapped || m == c
		}
		h.sf.mu.Unlock()
		if c != nil && !mapped {
			return
		}
	}
}

// holdHostSlotsUntilJoined takes every host-limiter slot of host, so a
// miss's runFetch waits at its slot and concurrent misses of one path
// coalesce in singleflight (strict mode refuses before any upstream
// fetch), and gives them back once a singleflight call of h that was not
// in flight when it took them has a joiner, and delay has passed since:
// the call's leader waits at its host slot until the second request has
// joined it — no wall-clock window to race (final fix wave FW-4 (d); the
// 300 ms window failed under load) — and both requests are at least delay
// old when the slots come back. The hold also ends with the test (and
// after 30 s), so a hold no request joins never outlives it.
func holdHostSlotsUntilJoined(t *testing.T, h *Handler, host string, delay time.Duration) *heldSlots {
	t.Helper()
	var rels []func()
	for range 4 { // the strict rig's hostsem.New(4)
		rel, err := h.sem.Acquire(context.Background(), host)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	h.sf.mu.Lock()
	before := make(map[*sfCall]bool, len(h.sf.calls))
	for _, c := range h.sf.calls {
		before[c] = true
	}
	h.sf.mu.Unlock()
	held := &heldSlots{}
	joined := func() bool {
		h.sf.mu.Lock()
		defer h.sf.mu.Unlock()
		for _, c := range h.sf.calls {
			if !before[c] && c.waiters > 0 {
				held.mu.Lock()
				held.call = c
				held.mu.Unlock()
				return true
			}
		}
		return false
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		defer func() {
			for _, rel := range rels {
				rel()
			}
		}()
		// Bounded, so a regression that never coalesces fails the
		// assertions below instead of hanging at the held slot.
		for deadline := time.Now().Add(30 * time.Second); !joined() && time.Now().Before(deadline); {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
		select {
		case <-done:
		case <-time.After(delay):
		}
	}()
	return held
}

// commitVouching adopts a complete-coverage inline snapshot of suite whose
// package_hash vouches debPath at debHash (a dists/ row).
func commitVouching(t *testing.T, h *Handler, scheme, host, suite, debPath, debHash string) {
	t.Helper()
	commitStrictSnap(t, h, scheme, host, strictSnap{suite: suite, complete: true, debs: map[string]string{debPath: ""}, declared: debHash})
}

// strictWaitRig is a strict-mode handler on a host whose only current
// snapshot has complete coverage, so an unvouched .deb is refused.
func strictWaitRig(t *testing.T, a *fakeAdoptions) (*Handler, *strictLogBuf, string, string, string) {
	t.Helper()
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
	scheme, host, port := splitURL(t, srv.URL)
	canon := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canon, "/dists/noble", true)
	h.adoptions = a
	return h, logs, srv.URL, scheme, canon
}

const waitDebPath = "/pool/main/h/hello_1_amd64.deb"

// Spec C10 on the miss path: strict mode would refuse an unvouched .deb
// while a signed adoption is in flight on its host; runFetch gives its
// host slot back, serveCacheMiss waits (outside singleflight) and retries
// once — the adoption that finished meanwhile vouches the .deb.
func TestServeHTTP_StrictMissWaitsForAnAdoptionInFlight(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	a.during = func() {
		commitVouching(t, h, scheme, canon, "/dists/new", waitDebPath, sha256Hex([]byte("upstream bytes")))
		a.inFlight = false
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream bytes" {
		t.Fatalf("status %d body %q, want the .deb the new snapshot vouches", rec.Code, rec.Body.String())
	}
	if a.waits.Load() != 1 {
		t.Fatalf("waited %d times, want 1", a.waits.Load())
	}
}

// Spec C10 on the hit path: the refusal of a cached unvouched .deb first
// waits for the host's adoption in flight, then re-runs the lookup once.
func TestServeHTTP_StrictHitWaitsForAnAdoptionInFlight(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	deb := []byte("a cached .deb")
	blob := writeBlob(t, h, deb)
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
		Path: waitDebPath, BlobHash: &blob, UpstreamURL: srvURL + waitDebPath}); err != nil {
		t.Fatal(err)
	}
	a.during = func() { commitVouching(t, h, scheme, canon, "/dists/new", waitDebPath, sha256Hex(deb)) }
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusOK || rec.Body.String() != string(deb) {
		t.Fatalf("status %d body %q, want the cached .deb, now vouched", rec.Code, rec.Body.String())
	}
	if a.waits.Load() != 1 {
		t.Fatalf("waited %d times, want 1", a.waits.Load())
	}
}

// Spec C10: no wait without an adoption in flight; beyond 64 waiters per
// host (the waiter says no) the refusal is immediate; and a .deb still
// unvouched after the wait is refused, its refusal logged once.
func TestServeHTTP_StrictWaitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		a         *fakeAdoptions
		wantWaits int32
	}{
		{"nothing in flight", &fakeAdoptions{}, 0},
		{"the waiter cap", &fakeAdoptions{inFlight: true, ok: false}, 1},
		{"still unvouched after the wait", &fakeAdoptions{inFlight: true, ok: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, logs, srvURL, _, _ := strictWaitRig(t, tc.a)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
			if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") != "60" {
				t.Fatalf("status %d Retry-After %q, want the strict-mode refusal", rec.Code, rec.Header().Get("Retry-After"))
			}
			if got := tc.a.waits.Load(); got != tc.wantWaits {
				t.Fatalf("waited %d times, want %d", got, tc.wantWaits)
			}
			n := 0
			for _, rec := range logs.records() {
				if rec["msg"] == "unvouched_deb_refused" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events, want 1", n)
			}
		})
	}
	// Design review O1: two coalesced requests share one fetch, so the
	// refusal is logged once — by the singleflight leader — also after the
	// retry, whose requests coalesce again. Each request's wait is bounded
	// by its own age (design review M-10: the miss path passes the
	// request's start, so the time spent before the wait counts).
	for _, tc := range []struct {
		name string
		a    *fakeAdoptions
	}{
		{"the waiter cap, two coalesced requests", &fakeAdoptions{inFlight: true, ok: false}},
		{"still unvouched after the wait, two coalesced requests", &fakeAdoptions{inFlight: true, ok: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, logs, srvURL, _, canon := strictWaitRig(t, tc.a)
			const delay = 300 * time.Millisecond
			first := holdHostSlotsUntilJoined(t, h, canon, delay) // the first call's leader waits until the second request joins it
			var once sync.Once
			tc.a.during = func() {
				// Each request's retry starts only once the first call has
				// left the singleflight map: a retry that still finds the
				// finished call mapped joins it and gets its stale result
				// (E-deferred E-57), which under load left the retry
				// uncoalesced and this test's setup to scheduling.
				first.waitGone(h)
				if tc.a.ok { // the requests retry: so does the retry's leader wait for its joiner
					once.Do(func() { holdHostSlotsUntilJoined(t, h, canon, delay) })
				}
			}
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
					if rec.Code != http.StatusBadGateway {
						t.Errorf("status %d, want the strict-mode refusal", rec.Code)
					}
				}()
			}
			wg.Wait()
			shared := 0 // the leader's line, for the request's last singleflight call
			for _, rec := range logs.records() {
				if rec["msg"] == "singleflight coalesced" {
					shared++
				}
			}
			if shared != 1 {
				t.Fatalf("%d singleflight coalesced lines, want 1 (setup: the requests must coalesce)", shared)
			}
			if got := tc.a.waits.Load(); got != 2 {
				t.Fatalf("waited %d times, want 2 (each request waits)", got)
			}
			for _, age := range tc.a.ages {
				if age < delay {
					t.Fatalf("a wait began %v into its request, want at least the %v at the host slot: the request's start, not the wait's", age, delay)
				}
			}
			n := 0
			for _, rec := range logs.records() {
				if rec["msg"] == "unvouched_deb_refused" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d unvouched_deb_refused events for one fetch, want 1", n)
			}
		})
	}
}

// Underspecified item 5 of the design review: a client that leaves while
// its miss waits gets no retry — no second attempt for nobody — and the
// request ends client_canceled, not as a logged refusal.
func TestServeHTTP_StrictMissClientLeftDuringTheWait(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	h, logs, srvURL, _, _ := strictWaitRig(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.during = cancel // the client leaves while it waits
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath).WithContext(ctx))
	if a.asks.Load() != 1 || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d attempts, status %d; want one attempt and the client_canceled answer", a.asks.Load(), rec.Code)
	}
	for _, rec := range logs.records() {
		if rec["msg"] == "unvouched_deb_refused" {
			t.Fatal("a client that left was logged as refused")
		}
	}
}

// Spec C10 on the hit path: one wait per request — a cached .deb still
// unvouched after the wait is refused, not waited for again (the miss
// path's "still unvouched" case is TestServeHTTP_StrictWaitRefusals).
// (Proposed by the executability review; kills X19. The fake stops
// reporting an adoption after four waits so the mutant fails cleanly
// instead of recursing forever.)
func TestServeHTTP_StrictHitWaitsOnce(t *testing.T) {
	a := &fakeAdoptions{inFlight: true, ok: true}
	a.during = func() {
		if a.waits.Load() >= 4 {
			a.inFlight = false
		}
	}
	h, _, srvURL, scheme, canon := strictWaitRig(t, a)
	deb := []byte("a cached .deb")
	blob := writeBlob(t, h, deb)
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
		Path: waitDebPath, BlobHash: &blob, UpstreamURL: srvURL + waitDebPath}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want the strict-mode refusal", rec.Code)
	}
	if got := a.waits.Load(); got != 1 {
		t.Fatalf("waited %d times, want 1", got)
	}
}

// Spec C10 on the hit path, design note 31 (review fix round 1, Task 9): a
// client whose request context ends during the strict wait gets no retry —
// 503, outcome client_canceled — and never the unvouched .deb. The lookup
// is not re-run with the dead context (it would fail, and its error fails
// open: the hit was served). Both wait results are covered: the real wait
// returns false on ctx.Done, a waiter whose adoptions ended at that moment
// true. The MITM leg is the shape that delivered the bytes: the tunnel
// manager's Drain cancels the inner request's context while its writer
// stays live (the recorder stands in for it).
func TestServeHTTP_StrictHitClientLeftDuringTheWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mitm   bool
		waitOK bool
	}{
		{"plain, the wait ends false", false, false},
		{"plain, the wait ends true", false, true},
		{"MITM, the wait ends false", true, false},
		{"MITM, the wait ends true", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdoptions{inFlight: true, ok: tc.waitOK}
			h, logs, srvURL, scheme, canon := strictWaitRig(t, a)
			deb := []byte("a cached unvouched .deb")
			blob := writeBlob(t, h, deb)
			if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canon,
				Path: waitDebPath, BlobHash: &blob, UpstreamURL: srvURL + waitDebPath}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.mitm {
				ctx = proxy.WithMITMContext(ctx)
			}
			a.during = cancel // the client leaves (or Drain cancels the tunnel) while it waits
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq("GET", srvURL, waitDebPath).WithContext(ctx))
			if rec.Code != http.StatusServiceUnavailable || rec.Body.String() == string(deb) {
				t.Fatalf("status %d body %q; want 503 and never the unvouched .deb", rec.Code, rec.Body.String())
			}
			if a.waits.Load() != 1 {
				t.Fatalf("waited %d times, want 1", a.waits.Load())
			}
			var lines []map[string]any
			for _, r := range logs.records() {
				switch r["msg"] {
				case "package_hash lookup failed", "unvouched_deb_refused":
					t.Fatalf("logged %q for a client that left: %v", r["msg"], r)
				case "request":
					lines = append(lines, r)
				}
			}
			if len(lines) != 1 || lines[0]["outcome"] != "client_canceled" || lines[0]["status"] != float64(http.StatusServiceUnavailable) {
				t.Fatalf("request lines %v, want one client_canceled 503", lines)
			}
			if m, _ := lines[0]["mitm"].(bool); m != tc.mitm {
				t.Fatalf("request line mitm = %v, want %v", lines[0]["mitm"], tc.mitm)
			}
		})
	}
}

// Final fix wave FW-5 (a) (final spec review M1, its probe): on the hit
// path, strict mode's first package_hash lookup on a dead context — a MITM
// tunnel's inner request that Drain cancelled while the tunnel's writer
// stays live — answers client_canceled (503), never the unvouched .deb its
// fail-open used to serve, and logs no database fault.
func TestServeHTTP_StrictHitLookupOnADeadContextIsClientCanceled(t *testing.T) {
	a := &fakeAdoptions{inFlight: false}
	h, logs, srvURL, _, _ := strictWaitRig(t, a)
	r := proxyReq("GET", srvURL, waitDebPath)
	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(proxy.WithMITMContext(context.Background()))
	cancel() // Drain cancelled the inner request's context
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	served, outcome, status, _, fellThrough, _ := h.checkPackageHash(rec, r, req, sha256Hex([]byte("cached")), time.Now())
	if !served || fellThrough || outcome != "client_canceled" || status != http.StatusServiceUnavailable || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("served %v fellThrough %v outcome %q status %d (written %d); want client_canceled 503, never the fail-open serve",
			served, fellThrough, outcome, status, rec.Code)
	}
	for _, rec := range logs.records() {
		if rec["msg"] == "package_hash lookup failed" {
			t.Fatalf("logged %q for a client that left: %v", rec["msg"], rec)
		}
	}
}

// FW-5 (a) for classifyStrictMode, and the documented fail-open it keeps:
// a coverage lookup that fails on a dead context is strictCanceled, with no
// WARN; one that fails on a live context (a database fault) fails open
// (strictInactive) with WARN "strict-mode coverage lookup failed".
func TestClassifyStrictMode_LookupErrors(t *testing.T) {
	h, logs, _, scheme, canon := strictWaitRig(t, &fakeAdoptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sm, _, _ := h.classifyStrictMode(ctx, scheme, canon); sm != strictCanceled {
		t.Fatalf("dead context: outcome %v, want strictCanceled", sm)
	}
	warns := func() int {
		n := 0
		for _, rec := range logs.records() {
			if rec["msg"] == "strict-mode coverage lookup failed" {
				n++
			}
		}
		return n
	}
	if n := warns(); n != 0 {
		t.Fatalf("%d coverage-lookup WARNs for a dead context, want 0", n)
	}
	db, err := sql.Open("sqlite", filepath.Join(h.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE suite_snapshot RENAME TO suite_snapshot_unreadable`); err != nil {
		t.Fatal(err)
	}
	if sm, _, _ := h.classifyStrictMode(context.Background(), scheme, canon); sm != strictInactive {
		t.Fatalf("database fault: outcome %v, want strictInactive (fail open)", sm)
	}
	if n := warns(); n != 1 {
		t.Fatalf("%d coverage-lookup WARNs for a database fault, want 1", n)
	}
}
