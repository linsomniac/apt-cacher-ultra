package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// TestPhase2GPGForgery_RejectionLeavesSnapshotIntact is the SPEC2
// §12.4 gate: a cache adopted at snapshot A; upstream then publishes
// an InRelease whose bytes parse cleanly but whose signature is from
// a key NOT in the host keyring. The adoption attempt must abort with
// `result=gpg_failed`, the cache continues serving A, and
// `inrelease_change_seen_at` is set so operators see the divergence.
//
// Verifier semantics live in internal/gpg (TestVerifier_*); this test
// exercises the integration: handler → freshness.Check → Adopter.Run
// → Verifier.VerifyInline → categorized error → no flip.
func TestPhase2GPGForgery_RejectionLeavesSnapshotIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 gpg-forgery test skipped in -short mode")
	}

	snapA := makeChaos2Snapshot("A")
	snapB := makeChaos2Snapshot("B")

	var current atomic.Pointer[chaos2Snapshot]
	current.Store(&snapA)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := current.Load()
		body, etag := chaos2BodyAndETagFor(snap, r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Verifier accepts only snapA's bytes — represents a real GPG
	// trust set that has key A pinned for this suite. snapB is signed
	// by a key NOT in the trust set, modeled here as a verifier
	// rejection with the production gpg.ErrUntrustedSigner-class
	// error wrapping.
	gateVerifier := &chaos2GateVerifier{accept: snapA.inRelease}

	stack := newPhase2GPGForgeryStack(t, upstreamURL, gateVerifier)
	defer stack.handler.Close()

	allPaths := chaos2AllPaths()

	// Phase 1: prime + adopt A. Adoption uses the gateVerifier, which
	// accepts snapA.inRelease.
	for _, p := range allPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("phase-1 prime %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
	}
	stack.checker.WaitForAdoptions()

	suiteRef := freshness.SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   upstreamURL.Hostname(),
		SuitePath:       chaos2Suite,
	}
	if err := stack.adopter.Run(context.Background(), suiteRef, snapA.inRelease, "\"A\"", ""); err != nil {
		t.Fatalf("adopt A: %v", err)
	}

	suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-A adoption: suite_freshness=%+v err=%v", suite, err)
	}
	snapAID := *suite.CurrentSnapshotID

	// Phase 2: swap upstream to B. The verifier will reject B's
	// InRelease bytes when adoption runs.
	current.Store(&snapB)

	// Phase 3: trigger a freshness check by issuing an InRelease GET.
	// The conditional GET sees the new ETag, observes "changed", spawns
	// the adoption goroutine. The adoption goroutine calls VerifyInline
	// → gateVerifier rejects → ErrAdoptionGPGFailed → no flip.
	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/InRelease"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger freshness: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Drain the adoption goroutine. It will return ErrAdoptionGPGFailed.
	// WaitForAdoptions waits for the goroutine to return regardless of
	// outcome, so the assertions below see the post-failure state.
	if err := waitForGPGFailure(t, gateVerifier, 5*time.Second); err != nil {
		t.Fatalf("verifier never received the rejected payload: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Assertion 1: current_snapshot_id is still A. The flip never ran
	// because GPG verification aborts adoption before InsertCandidateSnapshot.
	suite, err = stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-rejection: suite_freshness=%+v err=%v", suite, err)
	}
	if *suite.CurrentSnapshotID != snapAID {
		t.Errorf("current_snapshot_id = %d, want %d (A) — adoption appeared to flip despite GPG failure",
			*suite.CurrentSnapshotID, snapAID)
	}

	// Assertion 2: inrelease_change_seen_at is set. SPEC2 §10.2 names
	// this as the operator-visible diagnostic for "upstream has new
	// metadata that we couldn't adopt." It's set by checkLocked at
	// the "changed" branch, which runs BEFORE the adoption goroutine
	// is spawned, so we expect it set even though the adoption
	// itself failed.
	if suite.InReleaseChangeSeenAt == nil {
		t.Errorf("inrelease_change_seen_at is nil — operator divergence diagnostic is missing")
	}

	// Assertion 3: subsequent metadata GET continues to serve A's
	// bytes. The cache must not 502 or fall through to upstream — the
	// adopted snapshot is still the contract.
	for _, p := range []string{
		chaos2Suite + "/InRelease",
		chaos2Suite + "/" + chaos2PackagesGzPath,
	} {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Errorf("post-rejection %s: status=%d body=%q (want 200)", p, rec.Code, rec.Body.String())
			continue
		}
		want := chaos2WantBody(&snapA, p)
		if got := rec.Body.Bytes(); !bytesEqualForTest(got, want) {
			t.Errorf("post-rejection %s: body length=%d, want A's bytes (length %d) — cache appears to be serving B",
				p, len(got), len(want))
		}
	}

	// Assertion 4: the verifier was actually exercised — it received
	// at least one rejected payload. Without this, an adoption-side
	// short-circuit (e.g. ErrNoUpdate before VerifyInline) could
	// silently turn this test into coverage theater.
	if got := atomic.LoadInt32(&gateVerifier.rejected); got == 0 {
		t.Errorf("gateVerifier received 0 rejected payloads — adoption never reached VerifyInline?")
	}
}

// chaos2GateVerifier accepts only `accept` bytes verbatim and rejects
// any other input with ErrUntrustedSigner-equivalent semantics. The
// accept count and reject count are tracked so tests can assert the
// verifier was actually exercised.
type chaos2GateVerifier struct {
	accept   []byte
	accepted atomic.Int32
	rejected int32 // accessed via atomic
}

// errChaos2UntrustedSigner mirrors gpg.ErrUntrustedSigner in shape —
// the integration test only cares that the Adopter wraps this in
// ErrAdoptionGPGFailed, not the specific underlying sentinel.
var errChaos2UntrustedSigner = errors.New("test: signing key not in trust set")

func (v *chaos2GateVerifier) VerifyInline(ctx context.Context, suite freshness.SuiteRef, in []byte) ([]byte, error) {
	if bytesEqualForTest(in, v.accept) {
		v.accepted.Add(1)
		return in, nil
	}
	atomic.AddInt32(&v.rejected, 1)
	return nil, errChaos2UntrustedSigner
}

// VerifyDetached is unused by this inline-mode forgery test. The stub
// rejects unconditionally — if a future caller routes a detached
// adoption through this gate, they should write a dedicated detached
// forgery test rather than reuse this one.
func (v *chaos2GateVerifier) VerifyDetached(ctx context.Context, suite freshness.SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error) {
	return nil, errChaos2UntrustedSigner
}

func bytesEqualForTest(a, b []byte) bool {
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

// waitForGPGFailure polls the verifier's rejected counter, returning
// when it goes positive or timeout elapses. Used to synchronize on
// "adoption attempted and was rejected" without sleeping for a fixed
// duration.
func waitForGPGFailure(t *testing.T, v *chaos2GateVerifier, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&v.rejected) > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("verifier never saw a rejected payload")
}

// newPhase2GPGForgeryStack mirrors newPhase2ChaosStackGated but with
// the verifier swappable. The fetch.Client is the production one
// (loopback-allow); the AdoptionFetcher is the same port-rewriting
// wrapper without a gate.
func newPhase2GPGForgeryStack(t *testing.T, upstream *url.URL, verifier freshness.Verifier) *phase2ChaosStack {
	t.Helper()
	return newPhase2ChaosStackWithVerifier(t, upstream, nil, verifier)
}
