package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency is the SPEC2
// §12.3 gate for detached-form suites: snapshot A is adopted via
// Release + Release.gpg, upstream then publishes a new (B) Release +
// Release.gpg, and during adoption of B 100 concurrent clients each
// issue an apt-shaped batch of {Release, Release.gpg, Packages.gz, 5
// .debs}. Per-response coherence (body bytes match either A's or B's
// authoritative content for that path — never a torn mix) must hold
// throughout, the flip must complete, and the final snapshot must
// carry B's release_hash + release_gpg_hash with NULL inrelease_hash.
//
// The shared chaos2 scaffolding (gate, rewriting fetcher, snapshot
// fixture builders, burst runner, body-coherence assertions) is
// inherited from phase2_chaos_test.go; this test reuses everything
// with two adjustments:
//
//   - chaos2Snapshot now carries release + releaseGPG, populated by
//     makeChaos2DetachedSnapshot. The upstream test handler dispatches
//     /Release and /Release.gpg from those fields.
//   - The direct adoption call uses RunDetached, and the freshness
//     trigger fetches /Release (which routes through the detached
//     branch of checkLocked because the prior snapshot has
//     release_hash != nil).
//
// AIDEV-NOTE: SPEC2 §12.3's per-client coherence claim is documented
// elsewhere as architecturally permitting straddle (see
// TestPhase2Chaos_AdoptionFlipUnderConcurrency); the same caveat
// applies here. We log straddle counts for regression tracking and
// strictly assert per-request coherence.
func TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("phase 2 detached chaos test skipped in -short mode")
	}

	snapA := makeChaos2DetachedSnapshot("A")
	snapB := makeChaos2DetachedSnapshot("B")

	var current atomic.Pointer[chaos2Snapshot]
	current.Store(&snapA)

	memberFetchGate := newChaos2Gate()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := current.Load()
		body, etag := chaos2BodyAndETagFor(snap, r.URL.Path)
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
	defer srv.Close()

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	stack := newPhase2ChaosStackGated(t, upstreamURL, memberFetchGate)
	defer stack.handler.Close()

	allPaths := chaos2DetachedAllPaths()

	// Phase 1: prime the cache. Detached form GETs Release +
	// Release.gpg + Packages.gz + .debs (no InRelease — upstream
	// doesn't serve it in this fixture).
	for _, p := range allPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("phase-1 prime %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if want := chaos2WantBody(&snapA, p); !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("phase-1 prime %s: body=%q, want %q", p, rec.Body.Bytes(), want)
		}
	}
	stack.checker.WaitForAdoptions()

	// Phase 1b: directly adopt A in detached form. Bypasses the
	// freshness conditional GET (which would 304 on the prime ETag).
	suiteRef := freshness.SuiteRef{
		CanonicalScheme: "http",
		CanonicalHost:   upstreamURL.Hostname(),
		SuitePath:       chaos2Suite,
	}
	if err := stack.adopter.RunDetached(context.Background(), suiteRef, snapA.release, snapA.releaseGPG, "\"A\"", ""); err != nil {
		t.Fatalf("adopt A directly via RunDetached: %v", err)
	}

	// Confirm A is current AND the snapshot is in detached form.
	suite, err := stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-A detached adoption: suite_freshness=%+v err=%v", suite, err)
	}
	snapAID := *suite.CurrentSnapshotID
	snapASnap, err := stack.handler.cache.GetSuiteSnapshot(context.Background(), snapAID)
	if err != nil {
		t.Fatalf("GetSuiteSnapshot(A): %v", err)
	}
	if snapASnap.InReleaseHash != nil {
		t.Errorf("post-A detached adoption: snapshot has unexpected inrelease_hash=%s", *snapASnap.InReleaseHash)
	}
	if snapASnap.ReleaseHash == nil {
		t.Error("post-A detached adoption: snapshot missing release_hash")
	}
	if snapASnap.ReleaseGPGHash == nil {
		t.Error("post-A detached adoption: snapshot missing release_gpg_hash")
	}

	// Phase 1c: warm a sample request against the adopted detached
	// snapshot. Catches any /Release or /Release.gpg snapshot-routing
	// regression early with a clear message.
	for _, p := range allPaths {
		rec := httptest.NewRecorder()
		stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, p))
		if rec.Code != http.StatusOK {
			t.Fatalf("post-A warm %s: status=%d body=%q", p, rec.Code, rec.Body.String())
		}
		if want := chaos2WantBody(&snapA, p); !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("post-A warm %s: body mismatch (want A's bytes)", p)
		}
	}
	stack.checker.WaitForAdoptions()

	// Phase 2: close gate, swap upstream to B, drive ONE Release GET
	// to start adoption. The freshness check sees snap.ReleaseHash !=
	// nil, picks the detached branch, conditional-GETs Release,
	// observes change, fetches Release.gpg, and spawns the adoption
	// goroutine. The goroutine VerifyDetaches → reaches member fetch
	// → blocks on the gate.
	memberFetchGate.Close()
	current.Store(&snapB)

	rec := httptest.NewRecorder()
	stack.handler.ServeHTTP(rec, proxyReq("GET", srv.URL, chaos2Suite+"/Release"))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger detached adoption: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if err := memberFetchGate.WaitForWaiter(5 * time.Second); err != nil {
		t.Fatalf("detached adoption never reached member-fetch gate: %v", err)
	}

	// Phase 3a: pre-flip burst, gate held closed.
	pre := runChaos2Burst(t, stack.handler, srv.URL, allPaths, 100, &snapA, &snapB)

	// Phase 3b: open gate, wait for atomic flip.
	memberFetchGate.Open()
	if err := waitForFlip(t, stack, upstreamURL, snapAID, srv.URL, 15*time.Second); err != nil {
		t.Fatalf("detached flip never happened after gate release: %v", err)
	}
	stack.checker.WaitForAdoptions()

	// Phase 3c: post-flip burst.
	post := runChaos2Burst(t, stack.handler, srv.URL, allPaths, 100, &snapA, &snapB)

	assertChaos2Coherence(t, "detached pre-flip", pre, allPaths)
	assertChaos2Coherence(t, "detached post-flip", post, allPaths)

	for _, s := range pre {
		if !s.ok || s.snapLabel != "A" {
			t.Errorf("detached pre-flip (gate closed): client=%d path=%s snap=%s status=%d (want A/200) — adoption flipped before member prefetch?",
				s.clientID, s.path, s.snapLabel, s.status)
		}
	}
	for _, s := range post {
		if !s.ok || s.snapLabel != "B" {
			t.Errorf("detached post-flip: client=%d path=%s snap=%s status=%d (want B/200)",
				s.clientID, s.path, s.snapLabel, s.status)
		}
	}

	preStraddles := chaos2CountMetadataStraddles(pre)
	postStraddles := chaos2CountMetadataStraddles(post)
	t.Logf("detached metadata straddle counts: pre-flip=%d post-flip=%d (architecturally allowed; inflation = regression)",
		preStraddles, postStraddles)

	// Final state: current_snapshot_id points at a detached snapshot
	// with B's release_hash + release_gpg_hash, and InReleaseHash is
	// still nil (a confused inline adoption against detached fixtures
	// would set inrelease_hash).
	suite, err = stack.handler.cache.GetSuiteFreshness(context.Background(),
		"http", upstreamURL.Hostname(), chaos2Suite)
	if err != nil || suite == nil || suite.CurrentSnapshotID == nil {
		t.Fatalf("post-chaos detached: suite_freshness=%+v err=%v", suite, err)
	}
	if *suite.CurrentSnapshotID == snapAID {
		t.Errorf("post-chaos detached: current_snapshot_id still = %d (A) — adoption never flipped", snapAID)
	}
	finalSnap, err := stack.handler.cache.GetSuiteSnapshot(context.Background(), *suite.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("post-chaos: GetSuiteSnapshot(%d): %v", *suite.CurrentSnapshotID, err)
	}
	if finalSnap.InReleaseHash != nil {
		t.Errorf("post-chaos: unexpected inrelease_hash=%s — flip went to inline form?", *finalSnap.InReleaseHash)
	}
	wantRH := chaos2Sha256Hex(snapB.release)
	if finalSnap.ReleaseHash == nil || *finalSnap.ReleaseHash != wantRH {
		gotHash := "<nil>"
		if finalSnap.ReleaseHash != nil {
			gotHash = *finalSnap.ReleaseHash
		}
		t.Errorf("post-chaos: release_hash=%s, want B's %s", gotHash, wantRH)
	}
	wantGH := chaos2Sha256Hex(snapB.releaseGPG)
	if finalSnap.ReleaseGPGHash == nil || *finalSnap.ReleaseGPGHash != wantGH {
		gotHash := "<nil>"
		if finalSnap.ReleaseGPGHash != nil {
			gotHash = *finalSnap.ReleaseGPGHash
		}
		t.Errorf("post-chaos: release_gpg_hash=%s, want B's %s", gotHash, wantGH)
	}
}

// makeChaos2DetachedSnapshot builds an A-or-B detached-form fixture.
// The Release file declares Packages.gz + member SHA256s exactly like
// makeChaos2Snapshot's inRelease, but the inRelease field stays nil
// and the Release.gpg field carries a label-distinguishing placeholder
// (chaos2PassVerifier.VerifyDetached returns releaseBytes verbatim and
// ignores sigBytes, so the placeholder bytes only need to be stable
// per label so the snapshot's release_gpg_hash is reproducible).
func makeChaos2DetachedSnapshot(label string) chaos2Snapshot {
	debs := make(map[string][]byte, len(chaos2DebRels))
	pkgEntries := make(map[string]string, len(chaos2DebRels))
	for _, rel := range chaos2DebRels {
		body := []byte(fmt.Sprintf("snapshot-%s|deb|%s", label, rel))
		debs[rel] = body
		pkgEntries[rel] = chaos2Sha256Hex(body)
	}
	pkgsTxt := chaos2BuildPackagesText(pkgEntries)
	pkgsGz := chaos2Gzip(pkgsTxt)

	rel := chaos2BuildRelease(map[string][]byte{
		chaos2PackagesGzPath: pkgsGz,
	})
	sig := []byte(fmt.Sprintf("--detached-sig-%s--", label))

	return chaos2Snapshot{
		label:      label,
		release:    rel,
		releaseGPG: sig,
		packagesGz: pkgsGz,
		debBodies:  debs,
	}
}

// chaos2DetachedAllPaths returns the absolute apt-shaped paths each
// detached-mode client requests in order: Release, Release.gpg,
// Packages.gz, then five .debs. Including Release.gpg in the burst
// exercises its metadata-self snapshot_member row and confirms it
// flips alongside Release.
func chaos2DetachedAllPaths() []string {
	out := make([]string, 0, 3+len(chaos2DebRels))
	out = append(out,
		chaos2Suite+"/Release",
		chaos2Suite+"/Release.gpg",
		chaos2Suite+"/"+chaos2PackagesGzPath,
	)
	for _, rel := range chaos2DebRels {
		out = append(out, "/ubuntu/"+rel)
	}
	return out
}
