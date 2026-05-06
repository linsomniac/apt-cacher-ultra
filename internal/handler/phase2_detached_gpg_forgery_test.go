package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// TestPhase2DetachedGPGForgery_RejectionLeavesSnapshotIntact is the
// SPEC2 §12.4 gate for detached-form suites: a cache adopted at
// snapshot A in detached form; upstream then publishes a Release +
// Release.gpg whose Release bytes parse cleanly but whose signature
// fails verification (modeled here as a chaos2GateVerifier whose
// `accept` is set to snapA's release bytes and rejects everything
// else with the test analogue of gpg.ErrUntrustedSigner). The
// adoption attempt must abort with `result=gpg_failed`, the cache
// continues serving A, and `inrelease_change_seen_at` is set so
// operators see the divergence.
//
// Verifier semantics for the detached path live in internal/gpg
// (TestVerifier_Detached_*); this test exercises the integration:
// handler → freshness.Check → checkLockedDetached → Adopter.RunDetached
// → Verifier.VerifyDetached → categorized error → no flip.
func TestPhase2DetachedGPGForgery_RejectionLeavesSnapshotIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 detached gpg-forgery test skipped in -short mode")
	}

	snapA := makeChaos2DetachedSnapshot("A")
	snapB := makeChaos2DetachedSnapshot("B")

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

	// Verifier accepts only snapA's release bytes — the detached
	// analogue of the inline forgery test's "key A pinned for this
	// suite". snapB's release bytes will land in the rejected counter.
	gateVerifier := &chaos2GateVerifier{accept: snapA.release}

	stack := newPhase2GPGForgeryStack(t, upstreamURL, gateVerifier)
	defer stack.handler.Close()

	allPaths := chaos2DetachedAllPaths()

	// Phase 1: prime + directly adopt A in detached form.
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
	if err := stack.adopter.RunDetached(context.Background(), suiteRef, snapA.release, snapA.releaseGPG, "\"A\"", ""); err != nil {
		t.Fatalf("adopt A via RunDetached: %v", err)
	}

	suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-A detached adoption: suite_freshness=%+v err=%v", suite, err)
	}
	snapAID := *suite.CurrentSnapshotID

	// Phase 2: swap upstream to B. The verifier will reject B's
	// release bytes when adoption runs.
	current.Store(&snapB)

	// Phase 3: trigger a freshness check by issuing a Release GET.
	// detectForm sees the prior snapshot's release_hash != nil →
	// detached. checkLockedDetached conditional-GETs Release, sees
	// changed, fetches Release.gpg, spawns the adoption goroutine.
	// The goroutine calls VerifyDetached → gateVerifier rejects →
	// ErrAdoptionGPGFailed → no flip.
	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/Release"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger detached freshness: status=%d body=%q", rec.Code, rec.Body.String())
	}

	if err := waitForGPGFailure(t, gateVerifier, 5*time.Second); err != nil {
		t.Fatalf("detached verifier never received the rejected payload: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Assertion 1: current_snapshot_id is still A.
	suite, err = stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-rejection: suite_freshness=%+v err=%v", suite, err)
	}
	if *suite.CurrentSnapshotID != snapAID {
		t.Errorf("current_snapshot_id = %d, want %d (A) — detached adoption appeared to flip despite GPG failure",
			*suite.CurrentSnapshotID, snapAID)
	}

	// Assertion 2: divergence diagnostic set. The detached freshness
	// check uses the same inrelease_change_seen_at column to record
	// "upstream has new metadata that we couldn't adopt" (the column
	// name reflects inline-mode legacy; the semantic is form-agnostic).
	if suite.InReleaseChangeSeenAt == nil {
		t.Errorf("inrelease_change_seen_at is nil after detached forgery — operator divergence diagnostic missing")
	}

	// Assertion 3: subsequent metadata GETs continue to serve A's
	// bytes. The cache must not 502 or fall through to upstream — the
	// adopted detached snapshot is still the contract.
	for _, p := range []string{
		chaos2Suite + "/Release",
		chaos2Suite + "/Release.gpg",
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

	// Assertion 4: VerifyDetached was actually exercised. Without
	// this, an upstream-side short-circuit (e.g. a 404 on Release.gpg
	// returning before VerifyDetached runs) could silently turn this
	// test into coverage theater.
	if got := atomic.LoadInt32(&gateVerifier.rejected); got == 0 {
		t.Errorf("gateVerifier received 0 rejected detached payloads — adoption never reached VerifyDetached?")
	}
}
