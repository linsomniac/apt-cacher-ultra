package freshness

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// unlogged returns acu_freshness_unlogged_panics_total.
func unlogged() float64 {
	v, _ := strconv.ParseFloat(renderedMetric("acu_freshness_unlogged_panics_total"), 64)
	return v
}

// Deferred minor A-37: the adopter's heartbeat logs its panic through the
// package's one helper — adoption_panicked, where=heartbeat, the stack the
// first time — and the adoption still commits.
func TestHeartbeatPanicIsLoggedWithItsStackOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		panicStackLogged.Delete("heartbeat")
		c := simtest.OpenCache(t)
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put(rigPkgsURL, pkgs)
		members.setDelay(rigPkgsURL, 3*time.Second) // the heartbeat ticks while the member arrives
		logs := &lockedBuffer{}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), HeartbeatInterval: time.Second, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		ad.heartbeatHook = func() { panic("heartbeat bug") }
		ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
		trackSuite(t, c, ref)
		if err := ad.Run(context.Background(), ref, signedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}), "", ""); err != nil {
			t.Fatalf("the adoption failed after a heartbeat panic: %v", err)
		}
		recs := logRecords(t, logs, "adoption_panicked")
		if len(recs) != 1 || recs[0]["where"] != "heartbeat" || recs[0]["level"] != "ERROR" || recs[0]["stack"] == nil ||
			recs[0]["suite_path"] != rigSuite {
			t.Fatalf("adoption_panicked records %v, want one ERROR where=heartbeat with the suite and the stack", recs)
		}
	})
}

// Deferred minor A-47: the audit counts an unsigned snapshot it keeps as
// audited, not as signed.
func TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	id := *r.row().CurrentSnapshotID
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Signed != 0 || res.Quarantined != 0 {
		t.Fatalf("audit = %+v (%v), want 1 audited, 0 signed, 0 quarantined", res, err)
	}
}

// Deferred minor C-29: every error RunLegacyTrustAudit returns starts
// "legacy trust audit:" (main's refusal line relies on it).
func TestRunLegacyTrustAudit_ErrorsCarryThePrefix(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, *r.row().CurrentSnapshotID); err != nil {
		t.Fatal(err)
	}
	prev := legacyAuditAfterBatch
	t.Cleanup(func() { legacyAuditAfterBatch = prev })
	boom := errors.New("batch hook failed")
	legacyAuditAfterBatch = func(int) error { return boom }
	_, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "legacy trust audit: ") {
		t.Fatalf("err = %v, want the batch error with the prefix", err)
	}
}

// Deferred minor B-17: a random value outside [0, 1) or NaN cannot push
// the delay past ±25 %.
func TestJitterDelay_ClampsTheRandomValue(t *testing.T) {
	d := 4 * time.Minute
	for _, tc := range []struct {
		u    float64
		want time.Duration
	}{{-1, 3 * time.Minute}, {math.NaN(), 3 * time.Minute}, {0, 3 * time.Minute}, {0.5, d}, {1, 5 * time.Minute}, {7, 5 * time.Minute}} {
		if got := jitterDelay(d, tc.u); got != tc.want {
			t.Errorf("jitterDelay(%v, %v) = %v, want %v", d, tc.u, got, tc.want)
		}
	}
}

// Deferred minor B-18: when the logger itself panics while an adoption
// panic is reported, the panic does not escape, the mutex is released, the
// failure is still recorded, and the unloggable panic is counted.
func TestAdoptionPanicWithABrokenLoggerIsContained(t *testing.T) {
	r := newRetryRig(t, 0)
	body := r.release("v1", 0)
	r.ver.panicOn(hashOf(body))
	r.up.set(body)
	r.checker.logger = slog.New(panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "adoption_panicked" }})
	before := unlogged()
	r.check() // returns: WaitForAdoptions saw the goroutine end
	if got := unlogged() - before; got < 1 {
		t.Fatalf("unlogged panics rose by %v, want at least 1", got)
	}
	if strOf(r.row().AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record reason %q, want panic", strOf(r.row().AdoptionFailReason))
	}
	// Past the record's retry: before it, a check of the deferred
	// never-adopted suite asks upstream nothing (ruling PE-3).
	r.clock.advance(backoffDelay(1) + time.Minute)
	r.check() // the suite mutex was released: a second check runs
	if n := len(r.up.etagsSent(rigAnchorURL)); n != 2 {
		t.Fatalf("%d checks reached upstream, want 2", n)
	}
}

// Deferred minor B-20: a shutdown that cuts a failure record's write short
// is not a WARN: it is logged once, at DEBUG (deferred minor E-43).
func TestAdoptionFailureRecordCutByShutdownIsNotAWarning(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.checker.recordHook = r.cancelLifetime // shutdown begins right before the write
	r.check()
	recs := r.events("adoption_failure_record_failed")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" {
		t.Fatalf("adoption_failure_record_failed lines %v; want one at DEBUG for the write shutdown cut short", recs)
	}
	if r.row().AdoptionFailCount != 0 {
		t.Fatal("a record was written after shutdown began")
	}
}

// Deferred minor E-36 (B-20's remainder): a shutdown that cuts the failure
// record's read short is not a WARN either; it is logged at DEBUG.
func TestAdoptionFailureRecordReadCutByShutdownIsNotAWarning(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.checker.recordReadHook = r.cancelLifetime // shutdown begins right before the read
	r.check()
	recs := r.events("adoption_failure_record_failed")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" {
		t.Fatalf("adoption_failure_record_failed lines %v; want one at DEBUG for the read shutdown cut short", recs)
	}
	if r.row().AdoptionFailCount != 0 {
		t.Fatal("a record was written after shutdown began")
	}
}

// Deferred minor B-28: a tick's heal parses the snapshot's pinned Release
// once — the decision's parse is reused.
func TestCheck_ReconcileOnTheTickParsesTheReleaseOnce(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.repairSkippedMembers = true
	ctx := context.Background()
	all := fakePackagesStanzas(map[string]string{"pool/main/c/c/c_1_all.deb": testHex64})
	const allPath = "main/binary-all/Packages" // a required group (arch all) the snapshot lacks
	r.members.put("http://h/r/dists/x/"+allPath, all)
	in := signedRelease("degraded", 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs, allPath: all})
	ah, err := r.adopter.writeBlobBytes(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := r.adopter.writeBlobBytes(ctx, r.pkgs)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := r.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	var parses atomic.Int32
	r.adopter.reparseHook = func() { parses.Add(1) }
	r.up.set(in) // unchanged: the tick decides and heals
	r.check()
	if _, err := r.cache.GetSnapshotMember(ctx, id, allPath); err != nil {
		t.Fatalf("the missing member was not healed: %v", err)
	}
	if n := parses.Load(); n != 1 {
		t.Fatalf("the Release was parsed %d times for one heal, want 1", n)
	}
}

// Deferred minor C-17 (phase C design note 14): without a stop context
// (Run/RunDetached), a cancelled ctx before the commit is a shutdown — the
// adoption ends and commits nothing.
func TestRunRequest_CancelledWithoutAStopContextCommitsNothing(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := r.release("v1", 0)
	req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite},
		form: adoptionFormInline, bytes: body, key: hashOf(body)}
	err := r.adopter.runRequest(ctx, req, runHooks{membersDone: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the shutdown (context.Canceled)", err)
	}
	if r.currentSnapshot() != nil {
		t.Fatal("an adoption cancelled before its commit committed")
	}
}
