package freshness

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// stageCounter returns the value of the counter name for stage, as
// metrics.Default renders it (0 when absent).
func stageCounter(t *testing.T, name, stage string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, name+"{") && strings.Contains(line, `stage="`+stage+`"`) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// newReconcileFailureRig is TestCheck_UnchangedTick_LogsReconcileFailure's
// harness: a current inline snapshot whose stored envelope no longer decodes
// (its END marker lost), served byte-identical upstream, so every check is a
// confirmed-unchanged tick whose reconcile decision fails. The checker logs
// JSON at DEBUG and up into the returned buffer.
func newReconcileFailureRig(t *testing.T) (*Checker, int64, *lockedBuffer) {
	t.Helper()
	return newReconcileRig(t, false)
}

// newReconcileRig is newReconcileFailureRig; with decodable, the envelope is
// whole, so the decision succeeds and finds the declared Packages missing
// (the snapshot holds its InRelease alone): the tick heals.
func newReconcileRig(t *testing.T, decodable bool) (*Checker, int64, *lockedBuffer) {
	t.Helper()
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/ubuntu/dists/noble"}
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                env.cache,
		Fetcher:              env.fetcher,
		Verifier:             passThroughVerifier{},
		HostLimiter:          hostsem.New(8),
		Architectures:        []string{"amd64"},
		RepairSkippedMembers: true, // gates the tick-path reconcile too
		Logger:               discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	signed := clearsignForTest(t, "Origin: Test\nSuite: noble\nSHA256:\n "+
		strings.Repeat("a", 64)+" 10 main/binary-amd64/Packages\n")
	anchor := append([]byte{}, signed[:bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))]...)
	if decodable {
		anchor = signed
	}
	snapID := storeInlineSnapshot(t, env, anchor)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubuntu/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(anchor)
	}))
	t.Cleanup(srv.Close)
	anchorHash := hashOf(anchor)
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/ubuntu/dists/noble/InRelease", BlobHash: &anchorHash,
		UpstreamURL: srv.URL + "/ubuntu/dists/noble/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatalf("PutURLPath: %v", err)
	}
	logBuf := &lockedBuffer{}
	checker, err := New(Config{
		Cache: env.cache, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now: func() time.Time { return time.Unix(11000, 0) },
	})
	if err != nil {
		t.Fatalf("New checker: %v", err)
	}
	return checker, snapID, logBuf
}

// TestCheck_UnchangedTick_WarnsOnceAReconcileFailure: the re-review's
// V-R2M1. A snapshot whose reconcile decision can never succeed (a stored
// anchor apt's split refuses, which the legacy audit keeps under ruling
// R-D2) failed it on every confirmed-unchanged check, and planRepair
// logged adoption_reconcile_failed at WARN each time, forever on a frozen
// suite. It warns once per snapshot now, logs the repeats at DEBUG, and
// counts every failure (acu_adoption_reconcile_failed_total{stage}).
func TestCheck_UnchangedTick_WarnsOnceAReconcileFailure(t *testing.T) {
	checker, snapID, logBuf := newReconcileFailureRig(t)
	before := stageCounter(t, "acu_adoption_reconcile_failed_total", "decision")
	for range 3 {
		checker.Check(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble")
		checker.WaitForAdoptions()
	}
	var warn, debug int
	for _, rec := range logRecords(t, logBuf, "adoption_reconcile_failed") {
		if rec["stage"] != "decision" || rec["snapshot_id"] != float64(snapID) {
			t.Errorf("adoption_reconcile_failed record %v; want stage decision, snapshot_id %d", rec, snapID)
		}
		switch rec["level"] {
		case "WARN":
			warn++
		case "DEBUG":
			debug++
		default:
			t.Errorf("adoption_reconcile_failed at level %v", rec["level"])
		}
	}
	if warn != 1 || debug != 2 {
		t.Errorf("three failing decisions logged %d WARN and %d DEBUG lines, want 1 and 2; log:\n%s", warn, debug, logBuf.String())
	}
	if got := stageCounter(t, "acu_adoption_reconcile_failed_total", "decision") - before; got != 3 {
		t.Errorf("acu_adoption_reconcile_failed_total{stage=decision} rose by %v, want 3", got)
	}
	// The on-demand path (POST /reconcile) warns every time: an operator
	// asked.
	onDemandBefore := stageCounter(t, "acu_adoption_reconcile_failed_total", "on_demand")
	for range 2 {
		if !checker.Reconcile(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble") {
			t.Fatal("Reconcile did not start")
		}
		checker.WaitForAdoptions()
	}
	onDemand := 0
	for _, rec := range logRecords(t, logBuf, "adoption_reconcile_failed") {
		if rec["stage"] == "on_demand" {
			onDemand++
			if rec["level"] != "WARN" {
				t.Errorf("an on-demand failure logged at %v, want WARN", rec["level"])
			}
		}
	}
	if onDemand != 2 {
		t.Errorf("two failing on-demand reconciles logged %d lines, want 2", onDemand)
	}
	if got := stageCounter(t, "acu_adoption_reconcile_failed_total", "on_demand") - onDemandBefore; got != 2 {
		t.Errorf("acu_adoption_reconcile_failed_total{stage=on_demand} rose by %v, want 2", got)
	}
}

// TestCheck_UnchangedTick_WarnsAgainAfterASuccessfulDecision: a decision
// that succeeds between two failing ones (here the snapshot marked
// reconciled for one check, as a decision that found it complete marks it)
// clears the memo, so the failure that returns is said at WARN again.
func TestCheck_UnchangedTick_WarnsAgainAfterASuccessfulDecision(t *testing.T) {
	checker, snapID, logBuf := newReconcileFailureRig(t)
	check := func() {
		checker.Check(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble")
		checker.WaitForAdoptions()
	}
	check() // fails: WARN
	check() // fails: DEBUG
	checker.adopter.reconciledSnapshots.Store(snapID, struct{}{})
	check() // succeeds
	checker.adopter.reconciledSnapshots.Delete(snapID)
	check() // fails again: WARN
	var levels []any
	for _, rec := range logRecords(t, logBuf, "adoption_reconcile_failed") {
		levels = append(levels, rec["level"])
	}
	if len(levels) != 3 || levels[0] != "WARN" || levels[1] != "DEBUG" || levels[2] != "WARN" {
		t.Errorf("adoption_reconcile_failed levels %v, want WARN, DEBUG, then WARN after the successful decision", levels)
	}
}

// TestAdopter_ReconcileFailureMemo: the memo behind the WARN-once rule. A
// snapshot's first failing decision is the one said; another snapshot (a
// suite's next adoption) has its own; a decision that succeeds forgets the
// failure, so one that recurs is said again; and a reaper run drops the
// entries of snapshots no suite has as its current one, as it does the
// reconciled ones.
func TestAdopter_ReconcileFailureMemo(t *testing.T) {
	a := &Adopter{}
	if !a.firstReconcileFailure(1) {
		t.Error("snapshot 1's first failure: not first")
	}
	if a.firstReconcileFailure(1) {
		t.Error("snapshot 1's second failure: first again")
	}
	if !a.firstReconcileFailure(2) {
		t.Error("snapshot 2's first failure (a new snapshot): not first")
	}
	a.clearReconcileFailure(1)
	if !a.firstReconcileFailure(1) {
		t.Error("snapshot 1's failure after a successful decision: not first")
	}
	a.reconciledSnapshots.Store(int64(3), struct{}{})
	a.firstReconcileFailure(4)
	if n := a.sweepReconcileMemo(map[int64]bool{2: true, 3: true}); n != 2 {
		t.Errorf("the sweep dropped %d entries, want 2 (failures 1 and 4)", n)
	}
	if a.firstReconcileFailure(2) {
		t.Error("snapshot 2, still current, swept")
	}
	if _, ok := a.reconciledSnapshots.Load(int64(3)); !ok {
		t.Error("snapshot 3's reconciled entry, still current, swept")
	}
	if !a.firstReconcileFailure(4) {
		t.Error("snapshot 4, no longer current, kept")
	}
}

// TestCheck_UnchangedTick_CountsAHealFailure: a heal that fails (its run's
// deadline here past before it reads the snapshot's members) is logged at
// WARN each time, as before, and counted under stage heal.
func TestCheck_UnchangedTick_CountsAHealFailure(t *testing.T) {
	checker, snapID, logBuf := newReconcileRig(t, true)
	checker.maxAdoptionDuration = time.Nanosecond
	before := stageCounter(t, "acu_adoption_reconcile_failed_total", "heal")
	for range 2 {
		checker.Check(context.Background(), "http", "127.0.0.1", "/ubuntu/dists/noble")
		checker.WaitForAdoptions()
	}
	heal := 0
	for _, rec := range logRecords(t, logBuf, "adoption_reconcile_failed") {
		if rec["stage"] != "heal" || rec["level"] != "WARN" || rec["snapshot_id"] != float64(snapID) {
			t.Errorf("adoption_reconcile_failed record %v; want a WARN of stage heal, snapshot_id %d", rec, snapID)
			continue
		}
		heal++
	}
	if heal != 2 {
		t.Errorf("two failing heals logged %d lines, want 2; log:\n%s", heal, logBuf.String())
	}
	if got := stageCounter(t, "acu_adoption_reconcile_failed_total", "heal") - before; got != 2 {
		t.Errorf("acu_adoption_reconcile_failed_total{stage=heal} rose by %v, want 2", got)
	}
}
