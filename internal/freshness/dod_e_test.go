package freshness

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// dodVerifier passes every InRelease through as its own plaintext and
// records each verification per suite; content keys in reject fail as an
// untrusted signer would.
type dodVerifier struct {
	mu     sync.Mutex
	reject map[string]bool
	calls  map[SuiteRef][]time.Time
}

func (v *dodVerifier) VerifyInline(_ context.Context, s SuiteRef, b []byte) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[s] = append(v.calls[s], time.Now())
	if v.reject[contentKey(adoptionFormInline, b, nil, nil)] {
		return nil, errRigUntrusted
	}
	return b, nil
}

func (v *dodVerifier) VerifyDetached(_ context.Context, _ SuiteRef, rel, _ []byte) ([]byte, error) {
	return rel, nil
}

func (v *dodVerifier) attempts(s SuiteRef) []time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]time.Time(nil), v.calls[s]...)
}

// Phase E DoD (spec §9 row E; design review I-6): a production-shaped
// fixture under the real scheduler at a 15-minute refresh — 3 hosts × 8
// never-adopted suites; on each host half are dormant by the v7 backfill
// rule (interest stamped migration time minus the 7-day window), three are
// used by clients today, and one has interest but only a reconstructed
// anchor row (request_count 0). Asserted: every active suite adopts exactly
// once (verifications); every dormant suite costs zero upstream requests,
// also to a client check; a dormant suite whose client returns at hour 1 —
// its hit's T1 running before the touch is visible, the handler's race
// (design review M-12) — adopts once, within one scheduler tick of the
// touch; and an active suite whose first adoption is rejected keeps the
// spec C7 schedule: never retried before its backoff, and not frozen.
func TestDoD_ActiveSuitesAdoptOnceDormantSuitesStaySilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		members := newFakeFetcher()
		ver := &dodVerifier{reject: map[string]bool{}, calls: map[SuiteRef][]time.Time{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8),
			MaxConcurrent: 2, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		up := simtest.NewUpstream()
		suiteURL := func(s SuiteRef, name string) string { return "http://" + s.CanonicalHost + s.SuitePath + "/" + name }
		anchorURL := func(s SuiteRef) string { return suiteURL(s, "InRelease") }
		backfill := time.Now().Add(-interestWindow).Unix()
		var active, dormant []SuiteRef
		var rejected, returning SuiteRef
		for h := 0; h < 3; h++ {
			host := fmt.Sprintf("h%d", h)
			for i := 0; i < 8; i++ {
				s := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: fmt.Sprintf("/r/dists/s%d", i)}
				pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + host + s.SuitePath + ".deb": testHex64})
				members.put("http://"+host+s.SuitePath+"/main/binary-amd64/Packages", pkgs)
				in := signedRelease("v1 "+host+s.SuitePath, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
				up.Set(anchorURL(s), simtest.Serve(in))
				switch {
				case i < 3: // clients use it today: interest now, a client-created anchor
					seedStaleAnchorAt(t, c, host, s.SuitePath)
					active = append(active, s)
					if h == 0 && i == 2 {
						rejected = s
						ver.reject[contentKey(adoptionFormInline, in, nil, nil)] = true
					}
				case i == 3: // interest now, but its anchor row was reconstructed (request_count 0)
					stale := testHex64
					if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host,
						Path: s.SuitePath + "/InRelease", BlobHash: &stale, UpstreamURL: anchorURL(s), IsMetadata: true}); err != nil {
						t.Fatal(err)
					}
					if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
						SuitePath: s.SuitePath}); err != nil {
						t.Fatal(err)
					}
					dormant = append(dormant, s)
				default: // the v7 backfill: dormant
					seedStaleAnchorAt(t, c, host, s.SuitePath)
					if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host,
						SuitePath: s.SuitePath, LastRequestedAt: &backfill}); err != nil {
						t.Fatal(err)
					}
					if h == 0 && i == 4 {
						returning = s
					} else {
						dormant = append(dormant, s)
					}
				}
			}
		}
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
		time.Sleep(time.Hour + 7*time.Second)
		// The returning client: the hit's T1 runs before its touch is
		// visible (both are asynchronous in the handler), so it finds the
		// suite dormant; the scheduler's next tick sees the touch.
		chk.Check(ctx, "http", returning.CanonicalHost, returning.SuitePath)
		if n := len(up.Calls(anchorURL(returning))); n != 0 {
			t.Fatalf("returning %v: %d upstream requests before its touch, want 0", returning, n)
		}
		touched := time.Now()
		if err := c.TouchSuite(ctx, cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: returning.CanonicalHost,
			SuitePath: returning.SuitePath}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2*time.Hour - 7*time.Second)
		stop()
		for _, s := range dormant { // a client check (T1) of a dormant suite asks upstream nothing either
			chk.Check(ctx, "http", s.CanonicalHost, s.SuitePath)
		}

		for _, s := range append(active, returning) {
			if s == rejected {
				continue
			}
			if sf, err := c.GetSuiteFreshness(ctx, "http", s.CanonicalHost, s.SuitePath); err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%v: not adopted (err %v)", s, err)
			}
			if n := len(ver.attempts(s)); n != 1 {
				t.Fatalf("%v: %d verifications, want exactly one adoption", s, n)
			}
		}
		tick := tickInterval(15 * time.Minute) // the scheduler's own tick (deferred minor E-56)
		if at := ver.attempts(returning); at[0].Sub(touched) > tick+checkDeadline {
			t.Fatalf("returning %v: adopted %v after its touch, want within one scheduler tick (%v) and its check",
				returning, at[0].Sub(touched), tick)
		}
		for _, s := range dormant { // every anchor file, not InRelease alone (deferred minor E-55)
			n := 0
			for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
				n += len(up.Calls(suiteURL(s, name)))
			}
			if n != 0 {
				t.Fatalf("dormant %v: %d upstream requests, want 0", s, n)
			}
		}
		// The rejected suite keeps its record's schedule (spec C7): each
		// retry at least its backoff after the attempt before it, and the
		// record counts every attempt.
		at := ver.attempts(rejected)
		if len(at) < 5 {
			t.Fatalf("rejected %v: %d attempts in 3 h, want the C7 schedule's retries (not frozen)", rejected, len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < backoffDelay(int64(i)) {
				t.Fatalf("rejected %v: attempt %d came %v after the one before, want at least its %v backoff",
					rejected, i+1, gap, backoffDelay(int64(i)))
			}
		}
		sf, err := c.GetSuiteFreshness(ctx, "http", rejected.CanonicalHost, rejected.SuitePath)
		if err != nil || sf.CurrentSnapshotID != nil || sf.AdoptionFailCount != int64(len(at)) {
			t.Fatalf("rejected %v: row %+v (err %v), want no snapshot and a record of %d attempts", rejected, sf, err, len(at))
		}
	})
}

// commitLegacyInline commits an inline snapshot of the rig suite whose
// InRelease blob is anchor, with the rig's Packages member — what a v6
// binary adopted.
func commitLegacyInline(t *testing.T, r *retryRig, anchor []byte) int64 {
	t.Helper()
	ctx := context.Background()
	ah, err := r.adopter.writeBlobBytes(ctx, anchor)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := r.adopter.writeBlobBytes(ctx, r.pkgs)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := r.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// Phase E DoD (overview "E:" line, review finding 2; spec §6.1): a suite a
// v6 binary adopted from a plain-text "InRelease" (the old
// require_signature = false fallback), quarantined by phase A's audit,
// adopts in the unsigned form through Release with no upstream change and
// never meets unsigned_downgrade_refused.
func TestDoD_QuarantinedPlainTextSuiteAdoptsUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	ctx := context.Background()
	text := r.releaseText("plain", 0)
	old := commitLegacyInline(t, r, text)
	// A pre-v7 cache has no markers; the v7 migration queues the audit.
	if err := r.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger()); err != nil || res.Quarantined != 1 {
		t.Fatalf("audit = %+v (%v), want the plain-text snapshot quarantined", res, err)
	}
	if r.row().CurrentSnapshotID != nil {
		t.Fatal("setup: the quarantined snapshot is still current")
	}
	// Upstream as it was: the same plain text as "InRelease" and as
	// Release, no signature.
	r.up.set(text)
	r.unsignedRelease("plain", nil)
	for _, step := range []time.Duration{0, 2 * time.Minute, 4 * time.Minute} {
		r.clock.advance(step)
		r.check()
		if strOf(r.row().AdoptionFailReason) == cache.ReasonUnsignedDowngradeRefused {
			t.Fatal("the quarantined plain-text suite was refused as a downgrade")
		}
	}
	snap := r.currentSnapshot()
	if snap == nil || snap.Form != cache.FormUnsigned || strOf(snap.SingleAnchorHash) != hashOf(text) {
		t.Fatalf("snapshot %+v, want the unsigned adoption of the same Release", snap)
	}
}

// Phase E DoD (overview "E:" line, review finding 2): a never-adopted
// suite whose signed InRelease a v6 binary served (cached, no marker) is
// refused unsigned adoption on its first post-upgrade check: phase A's
// startup pass marks the cached signed anchor.
func TestDoD_V6ServedSignedAnchorRefusesUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.adoptUnsigned = true
	ctx := context.Background()
	signed, err := r.adopter.writeBlobBytes(ctx, r.release("served by v6", 0))
	if err != nil {
		t.Fatal(err)
	}
	served := int64(1_799_000_000)
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
		Path: rigSuite + "/InRelease", BlobHash: &signed, UpstreamURL: rigAnchorURL, IsMetadata: true,
		RequestCount: 1, LastRequestedAt: &served}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger()); err != nil { // the startup pass
		t.Fatal(err)
	}
	// Upstream now publishes only an unsigned Release.
	r.unsignedRelease("unsigned now", nil)
	r.check()
	if row := r.row(); strOf(row.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || row.CurrentSnapshotID != nil {
		t.Fatalf("record %q current %v, want unsigned_downgrade_refused and nothing adopted",
			strOf(row.AdoptionFailReason), row.CurrentSnapshotID)
	}
}

// Phase E DoD (spec §3, libnvidia-container): an InRelease-only suite
// whose Release text has only a SHA512 block and a Date frozen in 2018,
// with a SHA256 Packages, adopts on its first check and vouches its .deb.
func TestDoD_NvidiaShapedSuiteAdopts(t *testing.T) {
	r := newRetryRig(t, 0)
	text := fmt.Sprintf("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Label: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\n"+
		"Date: Wed, 09 May 2018 22:33:58 GMT\n"+
		"SHA512:\n %s %d main/binary-amd64/Packages\n", sha512Of(r.pkgs), len(r.pkgs))
	r.up.set(simtest.ClearsignedFixture(text))
	r.check()
	snap := r.currentSnapshot()
	if lastResult(r) != "first_adoption" || snap == nil || snap.Form != cache.FormInline {
		t.Fatalf("result %q snapshot %+v, want an inline first adoption", lastResult(r), snap)
	}
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, "/r/pool/main/f/foo/foo_1.deb")
	if err != nil || len(rows) != 1 || rows[0].DeclaredSHA256 != testHex64 {
		t.Fatalf("package_hash rows %+v (%v), want the .deb vouched by its SHA256", rows, err)
	}
}
