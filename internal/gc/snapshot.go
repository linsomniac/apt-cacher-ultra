package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// snapshotPassResult counts one snapshot pass's reaps by class.
//
// AIDEV-NOTE: batch is a named field, never embedded (deferred minor
// F-3): embedded, SnapshotGCBatchResult's Total() was promoted and
// silently left out retired.
type snapshotPassResult struct {
	batch cache.SnapshotGCBatchResult
	// retired counts the retired snapshots sub-job C deleted (spec C9).
	retired int
	// packageRowsStripped counts the package_hash rows of displaced
	// snapshots sub-job D deleted (issue #14 follow-up, Tier 2).
	packageRowsStripped int
	// failureLogged: a sub-job's failure was logged (WARN gc_tick_failed)
	// and counted (subJobRun.next), so runTick names the pass in
	// FailedPasses, even when a shutdown followed (issue #14 follow-up,
	// Tier 1c).
	failureLogged bool
}

// runSnapshotPass implements SPEC4 §9.6.3: a per-tick batched reap
// loop over orphan candidate snapshots (sub-job A) ∪ displaced
// snapshots beyond keep-N (sub-job B). Each batch is a single writer-
// tx that runs SELECT-inside-tx + cascade DELETE of the orphans, and
// retires the displaced snapshots (one UPDATE; at a start too since
// 2026-11-09, issue #14 follow-up Tier 1b), for sub-job C to delete (DN-7
// of the GC and upstream hardening plan). The SELECT-DELETE (or -UPDATE)
// liveness race is closed by single-writer ordering (heartbeat /
// CommitAdoption / InsertCandidateSnapshot writes serialize through
// the same writer goroutine — they cannot interleave between SELECT
// and DELETE inside this tx). Once A ∪ B is drained, sub-job C (spec C9)
// deletes the retired snapshots — those of reaped, evicted and forgotten
// suites, those the legacy trust audit quarantined, and the displaced
// snapshots B retired — in chunks of at most 10,000
// rows per writer transaction, in the startup pass and the periodic ticks
// alike (ruling PF-15, re-ruled for issue #14), as does retiredRun, an
// hour after the start and hourly while a backlog lasts. Then sub-job D
// (issue #14 follow-up Tier 2) strips the displaced snapshots'
// package_hash rows, 10,000 rows a writer transaction, keeping their
// headers, members and skipped members, as does retiredRun after its C.
//
// AIDEV-NOTE: sub-job D and ruling HD-15 within the pass (issue #14
// follow-up Tier 2, 2026-11-09; the maintainer approved keep_displaced's
// meaning change the same day). The pass runs A ∪ B, then C, then D, all
// under the snapshot deadline and its start deadline; D comes last because
// C frees disk (its snapshots' blobs) and D frees only rows. A sub-job that
// fails logs its own WARN gc_tick_failed and the sub-jobs after it still
// run (subJobRun): HD-15, which already kept a failed pass from ending its
// tick, now applies within the pass, because C fails on the oldest retired
// snapshot at every call when that snapshot is the fault, and a C stuck so
// would otherwise stop D forever, defeating the strip (G1). The pass then
// returns its first failure, so it counts as failed (a failed D too), and
// runTick records it without logging it again (recordFailure). A deadline
// or a shutdown still stops the sub-jobs after it: a shutdown is not a
// failure, and logs nothing.
//
// AIDEV-NOTE: ruling PF-15 (final review concurrency m1), re-ruled by the
// maintainer, 2026-11-05 (issue #14; the final review 2's G2-I2). PF-15
// had the startup pass end its snapshot pass once A ∪ B drained, as if C
// were done: StartupPass runs before any listener serves, and C's backlog
// after a burst of reaps (a large suite's snapshots hold some 70-100k
// package_hash rows each, and a reap retires its current and displaced
// ones) could keep every listener closed up to max_tick_duration. But
// since DN-7 a periodic B only retires the displaced snapshots (the
// startup B did too from 2026-11-09, Tier 1b; until then it deleted them),
// and the startup B selects none that is retired, so with no C at a start a
// restart deleted nothing a periodic B had retired, nor a reaped suite's,
// and under a sustained displacement rate the retired backlog grew without
// bound where 1.0.x and main kept up, and a restart, the 1.0.x users'
// remedy, no longer cleared it. Now the startup pass runs C after A ∪ B,
// within the snapshot pass's deadline (passDeadlines: four fifths of
// max_tick_duration), and its blob pass reclaims their blobs in the same
// start. The cost: a start with a retired backlog may wait up to
// max_tick_duration before the listeners open, the bound ruling HD-7
// already accepted for B's one-transaction deletes at a start; C only
// fills the budget B leaves. Since 2026-11-09 (issue #14 follow-up, Tier
// 1b) the startup B only retires too, so the HD-7 bound now covers C's
// chunks, not B's deletes, and the start deadline bounds every startup
// write. The startup C deletes the snapshots the
// startup trust audit quarantined, as the periodic C does under spec C9
// (a quarantine retires its snapshot; the audit runs before the startup
// pass, so they now go at the start that quarantined them; a quarantined
// snapshot never serves or vouches again): the same outcome, earlier, with
// no new rule (the controller's ruling of 2026-11-05: "leaves quarantined
// ones" in the task's brief was an over-reach, not the maintainer's
// decision). Like every C it never deletes a snapshot that is not
// retired, such as a displaced one the audit still holds queued (B never
// ranks those: ruling R-H1). The downgrade cost, accepted by the
// maintainer, 2026-11-05: after a second 1.1.0 start, a 1.0.x restored from
// the pre-upgrade copy may miss a quarantined current snapshot's pool files
// and answer 502 to its suite's index requests until it re-adopts the suite
// (its member refetch fails closed when upstream has changed); the 1.1.0
// release notes say so. What a
// start leaves, the periodic ticks and retiredRun delete: hourly while a
// backlog lasts (Run). Retired rows never serve meanwhile.
//
// AIDEV-NOTE: rulings HD-4, HD-7 (DN-7 of the GC and upstream hardening
// plan; the H3 final review R3 #4), HD-7 superseded in part 2026-11-09
// (issue #14 follow-up Tier 1b). B only retires the displaced snapshots
// beyond keep_displaced, and C deletes them in this same pass, 10,000 rows a
// transaction: B's cascade DELETE held the writer — every miss's store,
// every flip's commit — for seconds on a large suite while the listeners
// served. HD-7 had the startup pass's B still delete them (phase ==
// phaseStartup chose that): it runs before the freshness scheduler and the
// listeners (main), so its long transactions held up no other writer and
// no request, and a daemon that never lived until C ran (min(1 h,
// gc.interval)) still deleted them, their blobs in that start's blob pass.
// Since PF-15's re-ruling (issue #14) the startup pass runs C after B, and
// the start deadline (Tier 1a) bounds every startup write, so the startup B
// retires too (one code path) and that start's C deletes them, their
// blobs in the same start's blob pass; they count twice at a start as in a
// periodic tick. When a periodic tick's pass stops before its C
// has deleted what its B retired — at its deadline, at a shutdown (ctx),
// on a C chunk that failed, or on a batch or chunk that panicked (a batch
// that fails no longer stops the pass before its C since 2026-11-09: HD-15
// within the pass, subJobRun) — those snapshots are no
// longer B-selectable (retired_at); C deletes them: retiredRun an hour
// later when the pass stopped at its deadline (issue #14), the next start's
// startup pass (PF-15 re-ruled), or a later periodic tick's snapshot pass
// (the next one, after a failure too, since a failed pass no longer ends
// its tick: ruling HD-15; after a panic, the one after it, since the
// rotation moves past the snapshot pass). Such a snapshot counts twice: in
// displaced_reaped when B retires it, in retired_reaped when C deletes it.
//
// Returns:
//   - the cumulative counts (orphan, displaced, retired, package rows
//     stripped),
//   - whether the deadline tripped (true means the pass exited
//     between batches with backlog remaining, or at a batch or chunk
//     the writer never started before the deadline, its start deadline
//     (issue #14 Tier 1a: notStarted); runTick then moves the
//     rotation to the blob pass, which runs in the same tick, under its
//     own share of the budget, and the next tick that reaches this pass
//     re-selects — A ∪ B, then C, then D: issue #14),
//   - the first sub-job's DB-level failure, nil if none failed; each failed
//     sub-job has logged its own gc_tick_failed line, and the sub-jobs after
//     it ran (subJobRun); runTick then runs the blob pass all the same
//     (ruling HD-15).
func (g *GC) runSnapshotPass(ctx context.Context, deadline time.Time, phase string) (snapshotPassResult, bool, error) {
	var (
		acc        snapshotPassResult
		batchesRun int
	)
	dl := g.snapshotDeadline(deadline, phase, &batchesRun)
	// Every write of the pass, A ∪ B's batches and C's and D's chunks, must
	// start by the deadline (issue #14 Tier 1a: notStarted's AIDEV-NOTE).
	wctx := cache.WithWriteStartDeadline(ctx, deadline)
	sj := subJobRun{g: g, ctx: ctx, pass: "snapshot"}

	hit, err := g.runSubJobsAB(wctx, dl, &acc.batch, &batchesRun)
	if !sj.next(hit, err) {
		acc.failureLogged = sj.failureLogged
		return acc, hit, sj.firstErr
	}
	// Sub-job C, at a start too (ruling PF-15 re-ruled, issue #14).
	retired, hit, err := g.runSubJobC(wctx, dl, &batchesRun)
	acc.retired += retired
	if !sj.next(hit, err) {
		acc.failureLogged = sj.failureLogged
		return acc, hit, sj.firstErr
	}
	// Sub-job D, last: C frees disk (its snapshots' blobs), D only rows.
	stripped, hit, err := g.runSubJobD(wctx, dl, &batchesRun)
	acc.packageRowsStripped += stripped
	sj.next(hit, err)
	acc.failureLogged = sj.failureLogged
	return acc, hit, sj.firstErr
}

// runSubJobsAB runs snapshot-GC sub-jobs A ∪ B (SPEC4 §9.6.3): batches until
// one finds nothing (drained), ctx ends, or dl's deadline comes
// (deadlineHit): dl expired between batches, or a batch the writer never
// started before it (notStarted). ctx is the pass's wctx. It adds each
// batch's counts to acc.
func (g *GC) runSubJobsAB(ctx context.Context, dl snapshotPassDeadline, acc *cache.SnapshotGCBatchResult, batchesRun *int) (deadlineHit bool, err error) {
	staleGraceSeconds := int64(g.cfg.HeartbeatStaleGrace.Seconds())
	for {
		if err := ctx.Err(); err != nil {
			return false, nil
		}
		if dl.expired() {
			return true, nil
		}
		batch, err := g.store.RunSnapshotGCBatch(ctx,
			g.cfg.SnapshotBatchSize,
			staleGraceSeconds,
			g.cfg.KeepDisplaced,
		)
		if notStarted(err) {
			// The batch never ran: the deadline, not a batch that found
			// nothing (A ∪ B drained), so neither counted nor followed by C.
			return dl.reached(), nil
		}
		if err != nil {
			return false, fmt.Errorf("snapshot gc batch: %w", err)
		}
		*batchesRun++
		acc.OrphanReaped += batch.OrphanReaped
		acc.DisplacedReaped += batch.DisplacedReaped
		if batch.Total() == 0 {
			return false, nil // A ∪ B drained
		}
	}
}

// subJobRun carries one snapshot pass's (or retired run's) sub-jobs from one
// to the next: next takes a sub-job's outcome and reports whether the
// sub-job after it runs, and firstErr is the first sub-job failure, which
// the pass returns.
//
// AIDEV-NOTE: ruling HD-15 within the pass (issue #14 follow-up Tier 2,
// 2026-11-09). Until then a sub-job that failed ended its pass; kept so, a C
// that failed on the same retired snapshot at every call (its chunks take
// the oldest first) would keep D from ever running, and the displaced
// snapshots' package_hash rows — the strip's whole point (G1) — would never
// be deleted. Now a failed sub-job logs its own WARN gc_tick_failed here, as it fails,
// and the sub-jobs after it run under the same deadline; a sub-job that
// reached the deadline, or a shutdown (ctx ended: no line, it did not
// fail), stops the sub-jobs after it. The pass still returns its first
// failure, so runTick counts the pass as failed (passFailed moves the
// rotation) without logging it again (runTick's recordFailure, which every
// failed pass goes through, logged or not). Updated 2026-11-09 (issue #14
// follow-up Tier 1c): the per-pass failure count is here, not in
// recordFailure, at the first line next logs (failureLogged), so it counts
// one per failed pass or run however many sub-jobs failed, and exactly the
// failures that were logged: one logged before a shutdown still counts, and
// one a shutdown caused (no line) never does.
type subJobRun struct {
	g   *GC
	ctx context.Context // the pass's ctx, not its wctx
	// pass is the acu_gc_pass_failures_total label: "snapshot" or
	// "retired".
	pass     string
	firstErr error
	// failureLogged: next logged a gc_tick_failed line, and counted the
	// pass's failure once.
	failureLogged bool
}

// next takes a sub-job that returned (deadlineHit, err) and reports whether
// the next sub-job runs: after a failure, logged (and, the first, counted)
// here while ctx lives, yes; at the deadline or once ctx has ended (a
// shutdown), no.
func (s *subJobRun) next(deadlineHit bool, err error) bool {
	if err != nil && s.firstErr == nil {
		s.firstErr = err
	}
	if s.ctx.Err() != nil {
		return false
	}
	if err != nil {
		s.g.cfg.Logger.Warn("gc_tick_failed", "err", err)
		if !s.failureLogged {
			s.failureLogged = true
			gcPassFailuresTotal.Inc(s.pass)
		}
		return true
	}
	return !deadlineHit
}

// snapshotPassDeadline is the snapshot pass's deadline (and retiredRun's),
// with its gc_tick_deadline_reached line: expired is its test between
// writes, reached its branch for a write the writer never started before
// it (notStarted).
type snapshotPassDeadline struct {
	g          *GC
	at         time.Time
	phase      string
	batchesRun *int
}

// snapshotDeadline returns the snapshot pass's deadline at deadline, for
// phase, whose line reports *batchesRun.
func (g *GC) snapshotDeadline(deadline time.Time, phase string, batchesRun *int) snapshotPassDeadline {
	return snapshotPassDeadline{g: g, at: deadline, phase: phase, batchesRun: batchesRun}
}

// expired is false before the deadline; after it, true, once
// gc_tick_deadline_reached is logged.
func (d snapshotPassDeadline) expired() bool {
	if time.Now().Before(d.at) {
		return false
	}
	return d.reached()
}

// reached logs gc_tick_deadline_reached and returns true: the pass ends at
// its deadline. The pass calls it directly for a write the writer never
// started before the deadline (notStarted): that deadline has passed, so
// the clock is not consulted again.
func (d snapshotPassDeadline) reached() bool {
	d.g.cfg.Logger.Info("gc_tick_deadline_reached",
		"phase", d.phase,
		"which", "snapshot",
		"batches_completed", *d.batchesRun,
		"bytes_reclaimed_this_tick", int64(0),
	)
	return true
}

// runSubJobC is snapshot GC sub-job C (spec C9): it deletes the retired
// snapshots, one per writer transaction in bounded chunks, until none is
// left, ctx ends, or dl's deadline comes (deadlineHit: backlog left): dl
// expired between chunks, or a chunk the writer never started before it
// (notStarted). ctx is the caller's wctx: it carries dl's deadline as its
// writes' start deadline (cache.WithWriteStartDeadline). The snapshot pass
// runs it after sub-jobs A and B, at a start and in the periodic ticks;
// retiredRun runs it before sub-job D, an hour after the start and hourly
// while a backlog lasts.
func (g *GC) runSubJobC(ctx context.Context, dl snapshotPassDeadline, batchesRun *int) (retired int, deadlineHit bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return retired, false, nil
		}
		if dl.expired() {
			return retired, true, nil
		}
		// 0: the cache's own chunk (retiredChunkRows, 10,000 rows a
		// transaction), never a larger number (deferred minor F-2).
		reaped, more, err := g.store.RunRetiredSnapshotGCChunk(ctx, 0)
		if notStarted(err) {
			return retired, dl.reached(), nil // the chunk never ran: backlog may be left
		}
		if err != nil {
			return retired, false, fmt.Errorf("retired snapshot gc chunk: %w", err)
		}
		*batchesRun++
		retired += reaped
		if !more {
			return retired, false, nil
		}
	}
}

// runSubJobD is snapshot GC sub-job D (issue #14 follow-up, Tier 2): it
// strips the displaced snapshots' package_hash rows
// (cache.RunDisplacedPackageHashChunk), oldest snapshot first, in bounded
// chunks, until none is left, ctx ends, or dl's deadline comes (deadlineHit:
// rows left): dl expired between chunks, or a chunk the writer never
// started before it (notStarted). ctx is the caller's wctx (runSubJobC's
// note). The snapshot pass runs it after C, and so does retiredRun. Their
// headers, members and skipped members stay: gc.keep_displaced now keeps a
// displaced snapshot's headers and index files, not its package list.
func (g *GC) runSubJobD(ctx context.Context, dl snapshotPassDeadline, batchesRun *int) (stripped int, deadlineHit bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return stripped, false, nil
		}
		if dl.expired() {
			return stripped, true, nil
		}
		// 0: the cache's own chunk (retiredChunkRows, 10,000 rows a
		// transaction), as C's.
		rows, more, err := g.store.RunDisplacedPackageHashChunk(ctx, 0)
		if notStarted(err) {
			return stripped, dl.reached(), nil // the chunk never ran: rows may be left
		}
		if err != nil {
			return stripped, false, fmt.Errorf("displaced package_hash gc chunk: %w", err)
		}
		*batchesRun++
		stripped += rows
		if !more {
			return stripped, false, nil
		}
	}
}

// retiredRunAfterStart is when, after the start, Run deletes the retired
// snapshots (retiredRun), if gc.interval is longer, and how long after a
// run or a periodic tick that left a backlog it runs again (issue #14).
const retiredRunAfterStart = time.Hour

// phaseRetired labels retiredRun's gc_tick_deadline_reached line.
const phaseRetired = "retired"

// retiredRun runs sub-job C, then sub-job D (issue #14 follow-up Tier 2),
// retiredRunAfterStart after the start (Run), within max_tick_duration, and
// logs INFO gc_retired_run_complete. It reports backlog: C stopped at its
// deadline with retired snapshots left, or D with displaced package rows
// left, so Run runs it again an hour later (issue #14: hourly while a
// backlog lasts); false when it drained them, D failed, it panicked or was
// stopped by ctx (a failed C goes on to D, whose outcome decides). A panic is recovered (gc_panicked, where=retired); the
// periodic ticks are not affected.
//
// AIDEV-NOTE: the final review's R3 #2. Sub-job C ran only in the periodic
// ticks (the startup pass skipped it, ruling PF-15, until issue #14's
// re-ruling: it runs C now, within its budget), and the first tick comes
// gc.interval (24 h by default) after the start, so a daemon restarted
// more often — a laptop, a container recreated nightly, a host rebooted
// daily — never deleted retired snapshots: their members, skipped members
// and package_hash rows (some 70–100k a large suite's snapshot) and the
// blobs those keep reachable stayed for good. Not an earlier first
// periodic tick: a tick also runs the url_path pass, a full scan of
// url_path with the version-aware mirror guard, and the blob pass, both of
// which the startup pass ran an hour before — work a restart would then
// pay twice. This run is only C, bounded and chunked as in a tick, on Run's
// goroutine (never beside a tick). Its blobs are reclaimed by the next
// blob pass: the next start's or the next periodic tick's. C also deletes
// the displaced snapshots a periodic tick's sub-job B retired (DN-7 of the
// GC and upstream hardening plan), or, since 2026-11-09 (issue #14
// follow-up, Tier 1b), the startup pass's B: normally that pass's own C
// does, and this run takes those still retired when a start's or a tick's
// snapshot pass stopped before its C had deleted them (its deadline, a shutdown, a C chunk that failed, or a
// batch or chunk that panicked; a failed batch no longer stops it before C
// since 2026-11-09), which the startup pass's B no longer selects (its C
// does, within its budget). Since issue #14 the startup pass also runs C,
// within its snapshot deadline, so this run takes what that start's budget
// left and what was retired since.
// Since issue #14 it also repeats hourly while a backlog lasts (Run's
// AIDEV-NOTE): once a process could not keep up with a sustained
// displacement rate.
//
// AIDEV-NOTE: issue #14 follow-up Tier 2, 2026-11-09. The run is no longer
// C alone: D runs after C, so the hourly runs strip the displaced
// snapshots' package_hash rows too (an upgrade's backlog, or one a periodic
// tick's budget left), and the run re-arms when either stops at its
// deadline. A failed C no longer ends the run (subJobRun: HD-15 within the
// run, so a C stuck on one snapshot never stops D); each failure logs its
// own gc_tick_failed. The cost, accepted: while D still has a backlog a C
// that fails at every call is retried with it hourly, one WARN an hour,
// until D drains; a run whose last sub-job failed still does not re-arm.
func (g *GC) retiredRun(ctx context.Context) (backlog bool) {
	defer g.recoverPanic("retired")
	start := time.Now()
	batchesRun := 0
	deadline := start.Add(g.cfg.MaxTickDuration)
	// Its chunks must start by its own deadline (issue #14 Tier 1a: a chunk
	// queued behind the writer then never runs; the run ends there and
	// reports a backlog, so Run re-arms it).
	wctx := cache.WithWriteStartDeadline(ctx, deadline)
	dl := g.snapshotDeadline(deadline, phaseRetired, &batchesRun)
	// AIDEV-NOTE: passFailed's test: a run stopped by the end of ctx (a
	// shutdown) did not fail, whatever its error's shape (subJobRun.next
	// logs no gc_tick_failed then, and runs no D). Since 2026-11-09 (issue
	// #14 follow-up Tier 2) a failed C no longer ends the run: D runs after
	// it (subJobRun's AIDEV-NOTE), and each logs its own line.
	// A failed run counts once, pass="retired", as next logs its first
	// failure; it reaches no last-run summary (issue #14 follow-up Tier 1c).
	sj := subJobRun{g: g, ctx: ctx, pass: phaseRetired}
	retired, deadlineHit, err := g.runSubJobC(wctx, dl, &batchesRun)
	var stripped int
	if sj.next(deadlineHit, err) {
		stripped, deadlineHit, err = g.runSubJobD(wctx, dl, &batchesRun)
		sj.next(deadlineHit, err)
	}
	if retired > 0 {
		gcRetiredReapedTotal.Add(float64(retired))
	}
	if stripped > 0 {
		gcDisplacedPackageRowsDeletedTotal.Add(float64(stripped))
	}
	g.cfg.Logger.Info("gc_retired_run_complete",
		"retired_reaped", retired,
		"displaced_package_rows_deleted", stripped,
		"deadline_reached", deadlineHit,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	// deadlineHit is the last sub-job's that ran: C's when it stopped at the
	// deadline (D never ran), else D's. It is false after a failed chunk or
	// a shutdown (runSubJobC, runSubJobD): no hourly retry storm. It is true
	// after a chunk the writer never started before the deadline
	// (notStarted): whether a backlog is left is unknown, and a re-armed run
	// that finds none costs one SELECT per sub-job.
	return deadlineHit
}
