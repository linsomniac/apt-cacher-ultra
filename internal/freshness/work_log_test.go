package freshness

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func waitWorkRecord(t *testing.T, logs *lockedBuffer, event string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range logRecords(t, logs, event) {
			if match(r) {
				return r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("missing %s record while work is still blocked: %s", event, logs.String())
	return nil
}

// Hold the real database writer before candidate creation: the previous
// progress ticker had not even started here and later depended on this writer.
func TestAdoptionProgressSurvivesBlockedWriter(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	r.adopter.progressInterval = 20 * time.Millisecond
	release, err := r.cache.HoldWriterForTest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.adopter.Run(ctx, SuiteRef{"http", rigHost, rigSuite}, r.release("next", 0), "", "") }()
	defer func() { release(); cancel(); <-done }()
	waitWorkRecord(t, logs, "adoption_in_progress", func(rec map[string]any) bool {
		waits, _ := rec["waits"].(map[string]any)
		return waits["database_write"] == float64(1) && rec["phase"] == "prepare"
	})
}

// Verification happens before the snapshot heartbeat can exist.
func TestAdoptionProgressIncludesVerification(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	r.adopter.progressInterval = 20 * time.Millisecond
	r.ver.blockNext()
	r.up.set(r.release("next", 0))
	r.checker.Check(t.Context(), "http", rigHost, rigSuite)
	defer func() { r.cancelLifetime(); r.checker.WaitForAdoptions() }()
	waitWorkRecord(t, logs, "adoption_in_progress", func(rec map[string]any) bool { return rec["phase"] == "verify" })
	for range 10 {
		r.checker.Check(t.Context(), "http", rigHost, rigSuite)
	}
	busy := r.events("freshness_check_busy")
	if len(busy) != 1 || busy[0]["reason"] != "adoption" || busy[0]["phase"] != "verify" || busy[0]["adoption_id"] == nil {
		t.Fatalf("busy checks should name their owner once: %v", busy)
	}
}

func TestManualReconcileGivesWayToFreshness(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	r.adopt()
	r.clock.advance(time.Minute)
	r.adopter.architectureAllowlist = map[string]struct{}{"amd64": {}}
	snapshot := *r.row().CurrentSnapshotID
	if err := r.cache.DeleteSnapshotMembersForTest(t.Context(), snapshot, "main/binary-amd64/Packages"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.cache.BlobPath(shaOf(r.pkgs))); err != nil {
		t.Fatal(err)
	}
	hold := &holdFetcher{fakeFetcher: r.members, hold: rigPkgsURL,
		entered: make(chan struct{}, 1), release: make(chan struct{}), held: make(chan struct{})}
	r.adopter.fetcher = hold
	if !r.checker.Reconcile(t.Context(), "http", rigHost, rigSuite) {
		t.Fatal("reconcile not started")
	}
	select {
	case <-waitEntered(hold):
	case <-time.After(3 * time.Second):
		close(hold.release)
		t.Fatal("reconcile never reached its member")
	}
	r.up.set(r.release("next", 0))
	got := r.checker.CheckNow(t.Context(), "http", rigHost, rigSuite)
	settle(t, r, hold)
	if got != "changed" || *r.row().CurrentSnapshotID == snapshot {
		t.Fatalf("release stayed behind manual reconcile: %s", got)
	}
	yields := r.events("repair_yielded")
	if len(yields) != 1 || yields[0]["to"] != "check" {
		t.Fatalf("missing yield diagnostic: %v", yields)
	}
}

func TestSlowRepairSurvivesUnchangedRefreshes(t *testing.T) {
	r, hold, _ := degradedRig(t, time.Minute)
	startRepair(t, r, hold)
	defer func() { close(hold.release); r.checker.WaitForAdoptions() }()
	for range 3 {
		r.clock.advance(time.Minute)
		if got := r.checker.CheckNow(t.Context(), "http", rigHost, rigSuite); got != "unchanged" && got != "not_modified" {
			t.Fatalf("unchanged probe = %s", got)
		}
		select {
		case <-hold.held:
			t.Fatal("unchanged refresh cancelled a slow repair")
		default:
		}
		if *r.row().LastSuccessAt != r.clock.now().Unix() {
			t.Fatal("probe did not update last checked")
		}
	}
	if len(r.events("repair_yielded")) != 0 {
		t.Fatal("unchanged release should let maintenance finish")
	}
}

func TestMaintenanceProbeDoesNotYieldOnNoopOrCancellation(t *testing.T) {
	r, hold, _ := degradedRig(t, 0)
	startRepair(t, r, hold)
	defer func() { close(hold.release); r.checker.WaitForAdoptions() }()
	// The strict timestamp CAS is a no-op in the same second. That is not
	// evidence that maintenance needs to yield.
	r.checker.CheckNow(t.Context(), "http", rigHost, rigSuite)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.checker.CheckNow(ctx, "http", rigHost, rigSuite)
	select {
	case <-hold.held:
		t.Fatal("unchanged/no-op or cancelled caller interrupted repair")
	default:
	}
	if len(r.events("repair_yielded")) != 0 {
		t.Fatal("repair unexpectedly yielded")
	}
}

func TestMaintenanceProbeUsesReservedHostSlot(t *testing.T) {
	r, hold, _ := degradedRig(t, 0)
	startRepair(t, r, hold)
	defer func() { close(hold.release); r.checker.WaitForAdoptions() }()
	r.checker.hostSem = hostsem.New(1)
	release, err := r.checker.hostSem.Acquire(t.Context(), rigHost)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	key := suiteKey("http", rigHost, rigSuite)
	v, _ := r.checker.repairs.Load(key)
	start := time.Now()
	_, handled := r.checker.checkDuringRepair(t.Context(), SuiteRef{"http", rigHost, rigSuite}, key, checkOpts{bypassCooldown: true}, v.(*repairRun))
	if elapsed := time.Since(start); !handled || elapsed > time.Second {
		t.Fatalf("saturated bulk slots must not prevent the unchanged probe: handled=%v elapsed=%v", handled, elapsed)
	}
}

func TestMaintenanceProbePendingStateStillHonorsCooldown(t *testing.T) {
	r, hold, _ := degradedRig(t, time.Minute)
	startRepair(t, r, hold)
	defer func() { close(hold.release); r.checker.WaitForAdoptions() }()
	row := r.row()
	seen := r.clock.now().Unix()
	row.InReleaseChangeSeenAt = &seen
	if err := r.cache.UpdateSuiteFreshness(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	key := suiteKey("http", rigHost, rigSuite)
	v, _ := r.checker.repairs.Load(key)
	rep, handled := r.checker.checkDuringRepair(t.Context(), SuiteRef{"http", rigHost, rigSuite}, key, checkOpts{}, v.(*repairRun))
	if !handled || rep.result != resultSkipped {
		t.Fatalf("pending state before due check must not cancel repair: %+v %v", rep, handled)
	}
	_, handled = r.checker.checkDuringRepair(t.Context(), SuiteRef{"http", rigHost, rigSuite}, key, checkOpts{bypassCooldown: true}, v.(*repairRun))
	if handled {
		t.Fatal("explicit due retry must enter ordinary check")
	}
}

func TestMaintenanceProbeHonorsHeldLateralInline(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adoptDetached()
	inline := r.release("bad-inline", 0)
	r.up.set(inline)
	key := suiteKey("http", rigHost, rigSuite)
	run := &repairRun{snapshotID: *r.row().CurrentSnapshotID}
	r.checker.repairs.Store(key, run)
	defer r.checker.repairs.Delete(key)
	// A detached retry may have cleared the failure record while the
	// separate lateral memo still holds this exact inline content.
	r.checker.lateralHolds.Store(key, lateralHold{key: hashOf(inline), retryAt: r.clock.now().Add(time.Minute).Unix()})
	r.clock.advance(time.Second)
	_, handled := r.checker.checkDuringRepair(t.Context(), SuiteRef{"http", rigHost, rigSuite}, key, checkOpts{bypassCooldown: true}, run)
	if !handled {
		t.Fatal("held inline content should not repeatedly interrupt detached repair")
	}
	r.clock.advance(time.Minute)
	_, handled = r.checker.checkDuringRepair(t.Context(), SuiteRef{"http", rigHost, rigSuite}, key, checkOpts{bypassCooldown: true}, run)
	if handled {
		t.Fatal("due inline retry must enter ordinary verification")
	}
}

func TestRepairYieldDoesNotCancelReplacement(t *testing.T) {
	c := &Checker{}
	old := &repairRun{}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	replacement := &repairRun{cancel: cancel}
	c.repairs.Store("suite", replacement)
	if c.repairGivesWay("suite", old) || ctx.Err() != nil {
		t.Fatal("stale probe cancelled replacement maintenance")
	}
}

func TestStaleWarningsAreRateLimited(t *testing.T) {
	c := &Checker{}
	if !c.shouldWarnStale(1) {
		t.Fatal("first stale suite must warn")
	}
	for range 20 {
		if c.shouldWarnStale(1) {
			t.Fatal("unchanged outage spammed WARN")
		}
	}
	if !c.shouldWarnStale(2) {
		t.Fatal("changed count should warn")
	}
	if c.shouldWarnStale(0) {
		t.Fatal("recovery is not a warning")
	}
	if !c.shouldWarnStale(1) {
		t.Fatal("new outage must warn")
	}
}

func TestPanicFailureRecordKeepsWatchdogAndLockIdentity(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	r.adopter.progressInterval = 20 * time.Millisecond
	body := r.release("panic", 0)
	r.ver.panicOn(hashOf(body))
	r.up.set(body)
	held := make(chan func(), 1)
	r.checker.recordReadHook = func() {
		release, err := r.cache.HoldWriterForTest(t.Context())
		if err != nil {
			panic(err)
		}
		held <- release
	}
	r.checker.Check(t.Context(), "http", rigHost, rigSuite)
	var release func()
	select {
	case release = <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("failure record not reached")
	}
	defer func() { release(); r.checker.WaitForAdoptions() }()
	waitWorkRecord(t, logs, "adoption_in_progress", func(rec map[string]any) bool {
		waits, _ := rec["waits"].(map[string]any)
		return rec["phase"] == "record_result" && waits["database_write"] == float64(1)
	})
	r.checker.Check(t.Context(), "http", rigHost, rigSuite)
	if records := r.events("freshness_check_busy"); len(records) != 1 || records[0]["reason"] != "adoption" {
		t.Fatalf("lost panic-path owner: %v", records)
	}
	release()
	r.checker.WaitForAdoptions()
	if _, ok := r.checker.work.Load(suiteKey("http", rigHost, rigSuite)); ok {
		t.Fatal("work entry leaked")
	}
	if g, h := r.checker.admission.usage(); g != 0 || h != 0 {
		t.Fatal("admission lease leaked")
	}
}

func TestMaintenanceProbeStampDoesNotOverwriteSnapshotOrPendingChange(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	row := r.row()
	firstSeen := r.clock.now().Unix() - 600
	row.InReleaseChangeSeenAt = &firstSeen
	if err := r.cache.UpdateSuiteFreshness(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	now := r.clock.now().Unix() + 60
	if ok, err := r.cache.ConfirmSnapshotUnchanged(t.Context(), key, *row.CurrentSnapshotID+1, now); err != nil || ok {
		t.Fatalf("wrong snapshot updated: %v %v", ok, err)
	}
	if ok, err := r.cache.ConfirmSnapshotUnchanged(t.Context(), key, *row.CurrentSnapshotID, now); err != nil || ok {
		t.Fatalf("pending change incorrectly confirmed: %v %v", ok, err)
	}
	row.InReleaseChangeSeenAt = nil
	if err := r.cache.UpdateSuiteFreshness(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.cache.ConfirmSnapshotUnchanged(t.Context(), key, *row.CurrentSnapshotID, now); err != nil || !ok {
		t.Fatalf("current snapshot not stamped: %v %v", ok, err)
	}
	after := r.row()
	if *after.CurrentSnapshotID != *row.CurrentSnapshotID || after.InReleaseChangeSeenAt != nil || *after.LastSuccessAt != now {
		t.Fatalf("probe damaged state: %+v", after)
	}
	if ok, err := r.cache.ConfirmSnapshotUnchanged(t.Context(), key, *row.CurrentSnapshotID, now-30); err != nil || ok {
		t.Fatalf("older probe overwrote newer check: %v %v", ok, err)
	}
}
