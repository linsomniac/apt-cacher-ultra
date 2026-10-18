// Package freshness implements SPEC §7: per-suite freshness state
// machine.
//
// A Checker bridges the cache (suite_freshness + url_path tables) and a
// fetch client (conditional GET) to:
//
//   - On request-path triggers (T1): attempt a non-blocking check.
//   - On periodic ticks (T2): scan known suites and check those whose
//     last_success_at has aged past freshness.periodic_refresh.
//
// Phase 1 deliberately does not adopt newly-observed InRelease bytes:
// doing so without atomically refreshing every referenced index would
// open a hash-mismatch window for any client mid-update. Instead, the
// Checker records the observation in inrelease_change_seen_at and the
// cache keeps serving the consistent older set. Phase 2's atomic-flip
// transaction will adopt.
package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Suite-anchor filenames. Inline mode checks one file (InRelease,
// clearsigned). Detached mode checks two (Release for change
// detection + Release.gpg for the signature). SPEC2 §7.6.3.
const (
	inReleaseFilename  = "/InRelease"
	releaseFilename    = "/Release"
	releaseGPGFilename = "/Release.gpg"
)

// defaultMaxInReleaseBytes caps the body we read on a 200 response to
// the conditional GET. Real-world InRelease files are tens of KB; 4 MiB
// is comfortable headroom and still small enough that a hostile (but
// allowlisted) upstream cannot exhaust memory through the freshness
// path. Operators can override via Config.MaxInReleaseBytes. The same
// cap applies to detached Release fetches (Release files are smaller
// than InRelease — no inline signature — so the bound is conservative).
const defaultMaxInReleaseBytes int64 = 4 << 20

// defaultMaxReleaseGPGBytes caps the Release.gpg body read after a
// detached Release change is observed. Real Release.gpg files are
// 1–2 KiB; 64 KiB matches the ceiling decodeMaybeArmoredSignature
// uses post-armor and bounds the cost of a hostile upstream that
// pads the file.
const defaultMaxReleaseGPGBytes int64 = 64 << 10

// checkDeadline bounds the upstream part of one freshness check (spec C4,
// a constant): it is the D of the scheduler's latency bound. Adoption,
// repair and reconcile goroutines run under their own contexts.
const checkDeadline = 2 * time.Minute

// Cache is the subset of *cache.Cache the freshness checker uses.
// Defined as an interface so tests can supply a fake without standing
// up an on-disk cache.
type Cache interface {
	GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*cache.SuiteFreshness, error)
	PutSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error
	ListSuites(ctx context.Context) ([]cache.SuiteFreshness, error)
	LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error)
	GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*cache.SuiteSnapshot, error)
	// PutURLPath re-seeds a reaped metadata anchor url_path row during
	// freshness recovery (SPEC2 §7.4). See checkLockedInline's
	// missing-anchor recovery branch.
	PutURLPath(ctx context.Context, u cache.URLPath) error
	// Adoption failure record (spec C7). UPDATE-only on existing rows.
	RecordAdoptionFailure(ctx context.Context, scheme, host, suitePath string, f cache.AdoptionFailure) error
	ClearAdoptionFailure(ctx context.Context, scheme, host, suitePath string) error
	SetAdoptionNextRetry(ctx context.Context, scheme, host, suitePath string, next int64) error
	RescheduleFailingAdoptions(ctx context.Context, now int64, offset func() int64) (int, error)
}

// Fetcher is the subset of *fetch.Client the freshness checker uses.
type Fetcher interface {
	Conditional(ctx context.Context, target *fetch.Target, etag, lastmod string, maxBody int64) (*fetch.ConditionalResult, error)
}

// Config carries the dependencies for a Checker.
type Config struct {
	Cache    Cache
	Fetcher  Fetcher
	Cooldown time.Duration // SPEC §7.2 cooldown gate
	Refresh  time.Duration // SPEC §7.4 periodic_refresh interval
	Logger   *slog.Logger

	// HostLimiter bounds concurrent upstream conditional GETs to a
	// single canonical host. SPEC §9.3. Production wires this to the
	// same *hostsem.Sem the handler uses for cache-miss fetches so
	// the per-host budget is honored across both paths.
	//
	// Required: New rejects a nil HostLimiter. The previous
	// "optional" treatment let a caller silently bypass the
	// security invariant — each 200 response is read into memory
	// up to MaxInReleaseBytes, so unbounded concurrency is a
	// memory-exhaustion path. Tests that don't care about the
	// limiter pass hostsem.New(<some-large-number>).
	HostLimiter *hostsem.Sem

	// MaxInReleaseBytes caps the body read on a 200 response. Defaults
	// to defaultMaxInReleaseBytes when zero.
	MaxInReleaseBytes int64

	// Adopter is optional. When non-nil, the freshness checker hands
	// off observed-changed InRelease bodies to it and the §7.5
	// adoption flow runs to completion. When nil, behavior is
	// unchanged from Phase 1: log the divergence, persist the
	// diagnostic, and let the next periodic tick retry.
	Adopter *Adopter

	// LifetimeCtx is the ctx adoption goroutines use. SPEC2 §9.5
	// step 5 says shutdown cancels the lifecycle ctx, propagating
	// into the verifier and member fetcher; in-flight adoptions
	// abandon staging files for the start-up sweep. Defaults to
	// context.Background() when zero — production must pass the
	// real lifecycle ctx so adoption goroutines tear down on
	// shutdown.
	LifetimeCtx context.Context

	// AdoptionRing is the SPEC5 §9.7.7 process-local ring buffer.
	// When non-nil, every adoption attempt (success or failure)
	// emits one AdoptionEvent on completion so the admin status
	// page can display recent activity. Production passes the
	// shared ring constructed in main; tests may pass nil to
	// disable recording.
	AdoptionRing *observability.Ring

	// GPGReasonClassifier breaks the `gpg_failed` outcome bucket into
	// SPEC5 §10.5 `recent_adoptions[].reason` tags (untrusted_signer,
	// short_keyid, crypto_verify_failed, …). Production injects
	// gpg.ClassifyVerifyErr; tests may leave nil, in which case the
	// reason for any gpg_failed event is the bare "gpg_failed" bucket.
	// Decoupled from a direct gpg import to keep freshness above the
	// verifier in the dependency graph.
	GPGReasonClassifier func(error) string

	// now is a test seam; production uses wallclock.Now.
	//
	// AIDEV-NOTE: wall clock, for persisted stamps and comparisons with
	// them only. Never measure a duration or a deadline with it (it has no
	// monotonic reading and tests step it); use time.Now / time.Since.
	now func() time.Time

	// rand returns a uniform value in [0, 1) for retry jitter and the
	// restart spread (spec C7). Test seam; production uses math/rand/v2.
	rand func() float64

	// urlReconstruct rebuilds the upstream URL for a metadata anchor
	// (InRelease/Release/Release.gpg) when its url_path row has been
	// reaped but the suite still has a current snapshot (SPEC2 §7.4
	// freshness recovery). Test seam; production defaults to
	// aptpath.BuildUpstreamURL(scheme, host, path). AIDEV-NOTE: canonical host is port-less
	// (SPEC §3.2), so reconstruction drops a non-default upstream port;
	// the GC identity guard (SPEC4 §5 guard (d)) preserves the original
	// row's port-correct upstream_url whenever it still exists, so
	// reconstruction only fires for an already-reaped anchor.
	urlReconstruct func(scheme, host, path string) string
}

// Checker is the SPEC §7 freshness state machine.
type Checker struct {
	cache               Cache
	fetcher             Fetcher
	hostSem             *hostsem.Sem
	cooldown            time.Duration
	refresh             time.Duration
	maxBody             int64
	logger              *slog.Logger
	now                 func() time.Time
	rand                func() float64
	urlReconstruct      func(scheme, host, path string) string
	adopter             *Adopter
	lifetimeCtx         context.Context
	adoptionRing        *observability.Ring
	gpgReasonClassifier func(error) string

	// admission hands out the global and per-host slots for adoption,
	// repair and reconcile (spec C4); maxAdoptionDuration bounds each run.
	admission           *admission
	maxAdoptionDuration time.Duration

	// adoptionWg tracks in-flight adoption goroutines spawned via
	// Check. Production graceful shutdown (SPEC2 §9.5 step 5) calls
	// WaitForAdoptions after cancelling lifetimeCtx; tests use it to
	// synchronize against the asynchronous adoption flow.
	adoptionWg sync.WaitGroup

	// locks holds *sync.Mutex per suite key. SPEC §7.3 specifies the
	// lock is in-memory and held only for the duration of the upstream
	// call. Memory upper bound is the number of distinct
	// (canonical_scheme, canonical_host, suite_path) values that have
	// ever been the subject of a Check call — i.e. metadata-bearing
	// suite paths the cache has either cached (T1 spawn) or written
	// to suite_freshness (T2 scan). An unauthenticated attacker
	// cannot grow this map without first producing successful upstream
	// metadata fetches on an allowlisted host, so the practical bound
	// is the cache's actual data set, not request volume.
	//
	// AIDEV-NOTE: entries are not actively reaped. A suite that was
	// once cached but later evicted (Phase 4 GC) will leave its
	// mutex in this map for the lifetime of the process. Per-entry
	// memory is small (one *sync.Mutex pointer plus map overhead),
	// so the long-tail growth is acceptable for Phase 1.
	locks sync.Map

	lastClockStepLog atomic.Int64 // monotonic ns since processStart of the last clock_step_detected line
}

// New constructs a Checker. Returns an error on missing required fields.
func New(cfg Config) (*Checker, error) {
	if cfg.Cache == nil {
		return nil, errors.New("freshness: nil Cache")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("freshness: nil Fetcher")
	}
	if cfg.HostLimiter == nil {
		return nil, errors.New("freshness: nil HostLimiter")
	}
	if cfg.Cooldown < 0 {
		return nil, fmt.Errorf("freshness: cooldown must not be negative, got %v", cfg.Cooldown)
	}
	if cfg.Refresh < 0 {
		return nil, fmt.Errorf("freshness: refresh must not be negative, got %v", cfg.Refresh)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBody := cfg.MaxInReleaseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxInReleaseBytes
	}
	now := cfg.now
	if now == nil {
		now = wallclock.Now
	}
	rnd := cfg.rand
	if rnd == nil {
		rnd = mrand.Float64
	}
	lifeCtx := cfg.LifetimeCtx
	if lifeCtx == nil {
		lifeCtx = context.Background()
	}
	reconstruct := cfg.urlReconstruct
	if reconstruct == nil {
		reconstruct = func(scheme, host, path string) string {
			return aptpath.BuildUpstreamURL(scheme, host, path)
		}
	}
	maxConc, maxDur := 0, defaultMaxAdoptionDuration
	if cfg.Adopter != nil {
		maxConc, maxDur = cfg.Adopter.maxConcurrent, cfg.Adopter.maxDuration
	}
	return &Checker{
		cache:               cfg.Cache,
		fetcher:             cfg.Fetcher,
		hostSem:             cfg.HostLimiter,
		cooldown:            cfg.Cooldown,
		refresh:             cfg.Refresh,
		maxBody:             maxBody,
		logger:              logger,
		now:                 now,
		rand:                rnd,
		urlReconstruct:      reconstruct,
		adopter:             cfg.Adopter,
		lifetimeCtx:         lifeCtx,
		adoptionRing:        cfg.AdoptionRing,
		gpgReasonClassifier: cfg.GPGReasonClassifier,
		admission:           newAdmission(maxConc),
		maxAdoptionDuration: maxDur,
	}, nil
}

// adoptionRequest is what checkLocked emits when a Phase 2 atomic
// flip should be triggered. The freshness checker has already fetched
// the form-appropriate body (or pair of bodies) by the time it
// constructs an adoptionRequest; the goroutine in Check just routes
// to Run vs RunDetached based on form.
type adoptionRequest struct {
	suite SuiteRef
	form  adoptionForm

	// Inline form (form == adoptionFormInline): bytes is the
	// freshness-fetched clearsigned InRelease body.
	bytes []byte

	// Detached form (form == adoptionFormDetached):
	releaseBytes []byte
	sigBytes     []byte

	// Validators captured from whichever metadata file the freshness
	// checker conditional-GETs next time (Release in detached mode,
	// InRelease in inline mode). The Adopter persists these to
	// suite_snapshot's inrelease_etag / inrelease_lastmod columns.
	etag    string
	lastmod string

	// key is the spec C7 content key of the bytes this request carries;
	// a failure is recorded under it.
	key string
}

// repairRequest is what checkLocked emits when the suite is CONFIRMED
// unchanged at upstream (304 or byte-identical 200) and has a current
// snapshot — the SPEC6_7 §3 repair window. Re-adoption only fires on
// "changed", so this is the only signal that can heal a snapshot whose
// adoption skipped integrity-class members; planRepair asks whether
// anything needs healing (a healthy suite, the steady state, needs
// nothing) and spawnRepair routes it to Adopter.RepairSkippedMembers
// and Adopter.ReconcileSnapshot.
type repairRequest struct {
	suite      SuiteRef
	snapshotID int64
}

// checkOutcome is what one check produces. lease is the admission slot
// taken for req or for the repair; on a panic checkLockedSafe releases
// it. needRepair/needReconcile say what the repair goroutine heals.
type checkOutcome struct {
	req           *adoptionRequest
	repair        *repairRequest
	needRepair    bool
	needReconcile bool
	lease         *lease
	// report is what the scheduler learns from this check.
	report checkReport
}

// checkOpts tunes one check. bypassCooldown is set only by the
// scheduler's re-check of a suite that waited for an admission slot
// (spec C4): that retry is bounded by slot turnover (one per released
// slot), so the cooldown's job — absorbing client-triggered storms — does
// not apply to it.
type checkOpts struct {
	bypassCooldown bool
}

// checkReport is what one check tells the scheduler: the freshness_check
// result it logged (or resultBusy / resultSkipped / "failed" when no
// result line was logged) and, with a queued result, whether the current
// snapshot is signed, i.e. which kind of slot the suite may take.
type checkReport struct {
	result        string
	signedCurrent bool
}

const (
	resultBusy    = "busy"    // the suite mutex was held (a T1 check or an adoption)
	resultSkipped = "skipped" // cooldown, no row, no anchor, cancelled: nothing learned
)

// isQueuedResult reports whether result means "new content, no slot".
// Phase E adds first_adoption_queued here.
func isQueuedResult(result string) bool { return result == "changed_queued" }

// Check runs one freshness check of the suite. It is the entry point for
// client-triggered (T1) checks; see check.
//
// Callers on the request path (T1) should typically invoke Check from a
// goroutine: the request has already been served by the time T1 fires,
// so blocking the request goroutine on a slow upstream gains nothing.
func (c *Checker) Check(ctx context.Context, scheme, host, suitePath string) {
	c.check(ctx, SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, checkOpts{})
}

// check runs the SPEC §7.2 algorithm for one suite, synchronously, and
// reports its result to the scheduler. It returns at once (resultBusy),
// without contacting upstream, if another goroutine holds the suite
// mutex; the cooldown is applied by checkLocked (resultSkipped) unless
// opts bypass it.
//
// On observed change with an Adopter wired in it takes an admission slot
// without waiting (spec C4) and spawns a goroutine to run the §7.5
// adoption flow. The per-suite mutex is HANDED OFF to that goroutine: it
// serializes the entire adoption against any subsequent check on the
// same suite, matching SPEC2 §7.5's "the same per-suite Mutex from §7.3
// guards the entire adoption."
func (c *Checker) check(ctx context.Context, ref SuiteRef, opts checkOpts) checkReport {
	scheme, host, suitePath := ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath
	key := suiteKey(scheme, host, suitePath)
	muVal, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		// Another goroutine is on it; SPEC §7.3 says skip.
		return checkReport{result: resultBusy}
	}
	// Spec C4: the upstream part of a check (the host slot and the
	// conditional GETs) runs under a 2-minute deadline. Cache reads and
	// writes keep the caller's context, so a check that hits the deadline
	// still records it (last_check_at, check_fail_count); the scheduler's
	// fairness depends on that write.
	//
	// AIDEV-NOTE: never pass fetchCtx to a cache call: an expired deadline
	// makes submitWrite fail, and the failure would go unrecorded.
	fetchCtx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()
	out, ok := c.checkLockedSafe(ctx, fetchCtx, scheme, host, suitePath, opts)
	if !ok {
		mu.Unlock()
		return checkReport{result: "failed"}
	}
	switch {
	case out.lease != nil && out.req != nil:
		c.spawnAdoption(mu, out.req, out.lease)
	case out.lease != nil && out.repair != nil:
		c.spawnRepair(mu, out)
	default:
		// A lease is only ever taken for a req or a repair; release
		// defensively so the invariant does not rest on inspection.
		out.lease.release()
		mu.Unlock()
	}
	return out.report
}

// spawnAdoption hands the suite mutex and the admission lease to the
// adoption goroutine, which runs against the lifetime context bounded by
// adoption.max_duration (a request closing must not abort it).
//
// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
// release, recoverAdoption (first). The failure record is written before
// the mutex is released on both the error and the panic path (spec C7).
// The host slot is freed earlier, when the member pipeline ends.
func (c *Checker) spawnAdoption(mu *sync.Mutex, req *adoptionRequest, l *lease) {
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		// SPEC5 §10.4.3 / §9.7.7: time the adoption from goroutine
		// entry, not from inside runShared, so the duration reflects what
		// the operator observes.
		// AIDEV-NOTE: monotonic time.Now/time.Since, never c.now(): a
		// wall-clock step mid-adoption must not produce a negative duration
		// in acu_adoption_duration_seconds.
		start := time.Now()
		defer c.recoverAdoption(req, start)
		actx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		err := c.adopter.runRequest(actx, req, l.releaseHost)
		duration := time.Since(start)
		outcome := classifyAdoptionOutcome(err)
		if err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && c.lifetimeCtx.Err() == nil {
			outcome = "timed_out" // a member fetch reports the deadline as its own failure
		}
		reason := classifyAdoptionReason(err, outcome, c.gpgReasonClassifier)
		adoptionTotal.Inc(outcome, req.suite.CanonicalHost)
		adoptionDurationSeconds.Observe(duration.Seconds(), outcome, req.suite.CanonicalHost)
		if c.adoptionRing != nil {
			// SPEC5 §10.5: surface the failing member + detail when the
			// error chain carries an *AdoptionMemberError (member_fetch_failed
			// / member_mismatch). Empty for success and non-member failures.
			memberPath, detail := memberErrorFields(err)
			c.adoptionRing.Record(observability.AdoptionEvent{
				Host:             req.suite.CanonicalHost,
				SuitePath:        req.suite.SuitePath,
				Outcome:          outcome,
				Reason:           reason,
				CompletedUnixSec: c.now().Unix(),
				DurationSeconds:  duration.Seconds(),
				MemberPath:       memberPath,
				Detail:           detail,
			})
		}
		if err != nil {
			// Several Adopter paths emit categorized log lines before
			// returning (adoption_gpg_failed, adoption_parse_failed,
			// adoption_member_mismatch, pool_corruption_during_adoption).
			// Others — content-length mismatch, fetch transport errors,
			// DB failures the categorized line didn't already cover —
			// propagate only as the wrapped sentinel. Without a backstop
			// log, those drop on the floor and the operator sees the
			// "metadata changed at upstream" line followed by silence.
			// Always surface a single line so any failure is grep-able.
			c.logger.Warn("adoption_run_failed",
				"canonical_host", req.suite.CanonicalHost,
				"suite_path", req.suite.SuitePath,
				"form", formName(req.form),
				"err", err,
			)
			c.recordAdoptionFailure(req, reason)
		}
	}()
}

// planRepair decides whether an unchanged suite has anything to heal
// (SPEC6_7 §3 repair, SPEC6_8 §6 reconcile) and, if so, takes an
// admission slot for it. It runs inside checkLockedSafe's recover: a
// panic here is a failed check that releases the mutex and the lease.
//
// AIDEV-NOTE: deliberate deviation from spec C4's "TryAcquire before
// spawning": the cheap "anything to do?" checks run first, so healthy
// suites (the steady state) never compete with adoptions for slots. The
// slot is still taken without waiting and before spawning.
func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
	rp := out.repair
	if c.adopter == nil || !c.adopter.repairSkippedMembers {
		out.repair = nil
		return
	}
	out.needRepair = c.adopter.repairPending(ctx, rp.suite, rp.snapshotID)
	needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
	if err != nil {
		// A fail-closed anchor (R-1 decode error) must be visible, not a
		// silent non-heal.
		c.logger.Warn("adoption_reconcile_failed",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
			"snapshot_id", rp.snapshotID, "err", err)
	}
	out.needReconcile = needReconcile
	if !out.needRepair && !out.needReconcile {
		out.repair = nil
		return
	}
	l, ok := c.admission.tryAcquire(rp.suite.CanonicalHost, true)
	if !ok {
		c.logger.Debug("freshness: repair deferred, no adoption slot",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath)
		out.repair = nil
		return
	}
	out.lease = l
}

// spawnRepair hands the suite mutex and the admission lease to the heal
// goroutine planRepair decided on.
//
// AIDEV-NOTE: this goroutine heals two classes of degradation on the
// same CONFIRMED-unchanged-tick signal (out.repair, built by
// repairIfUnchanged only on a 304 / byte-identical-200 result with a
// current snapshot — NOT on a cooldown skip or upstream failure):
//  1. RepairSkippedMembers: integrity-class skip rows left by a
//     stale-mirror adoption (SPEC6_7 §3).
//  2. ReconcileSnapshot: requestable IndexTargets declared in the
//     snapshot's InRelease but absent from snapshot_member rows — e.g.
//     binary-all that was 4xx-skipped during adoption and was later
//     served by the mirror (SPEC6_8 §6).
//
// Both are gated by repairSkippedMembers (one config switch). Gating
// reconcile on the unchanged signal (rather than "has a current
// snapshot") keeps it inside the freshness cooldown: a
// persistently-degraded suite whose member can't be healed must not
// re-parse + re-fetch on every client metadata request. The repair's
// suite/snapshotID ARE the current snapshot. Mutex handoff and
// adoptionWg mirror spawnAdoption; WaitForAdoptions drains both.
//
// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
// release, recover (first). See recoverGoroutine.
func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
	rp, l, needRepair, needReconcile := out.repair, out.lease, out.needRepair, out.needReconcile
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		defer c.recoverGoroutine("repair", rp.suite)
		rctx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		if needRepair {
			c.adopter.RepairSkippedMembers(rctx, rp.suite, rp.snapshotID)
		}
		if needReconcile {
			// ReconcileSnapshot leaves logging to its caller; log like the
			// on-demand path (Reconcile) so a fail-closed anchor (R-1
			// decode error) is visible, not a silent non-heal.
			if _, err := c.adopter.ReconcileSnapshot(rctx, rp.suite, rp.snapshotID, false); err != nil {
				c.logger.Warn("adoption_reconcile_failed",
					"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
					"snapshot_id", rp.snapshotID, "err", err)
			}
		}
	}()
}

// WaitForAdoptions blocks until every in-flight adoption goroutine
// spawned via Check has returned. Used by tests for deterministic
// assertions and by graceful shutdown to drain SPEC2 §9.5 step 5
// after cancelling the lifecycle ctx.
func (c *Checker) WaitForAdoptions() { c.adoptionWg.Wait() }

// rescheduleFailingAdoptions gives every failing suite one retry spread
// uniformly over the next periodic_refresh (spec C7 "Restart"): the
// recovery path after an operator installs a key or changes pins,
// without a thundering herd. Refusal states and panic records keep
// their schedule.
func (c *Checker) rescheduleFailingAdoptions(ctx context.Context) {
	if c.adopter == nil {
		return
	}
	spread := int64(c.refresh / time.Second)
	n, err := c.cache.RescheduleFailingAdoptions(ctx, c.now().Unix(), func() int64 {
		if spread <= 0 {
			return 0
		}
		return int64(c.rand() * float64(spread))
	})
	if err != nil {
		c.logger.Warn("adoption_retries_reschedule_failed", "err", err)
		return
	}
	if n > 0 {
		c.logger.Info("adoption_retries_rescheduled", "suites", n, "spread_seconds", spread)
	}
}

// ErrAdoptionPanicked is recorded (phase B) when an adoption, repair or
// reconcile goroutine panics (security finding I-1).
var ErrAdoptionPanicked = errors.New("adoption_panicked")

// panicStackLogged holds the goroutine kinds ("adoption", "repair",
// "reconcile", "check", "scheduler") whose stack was already logged.
var panicStackLogged sync.Map

// reportPanic logs a recovered panic. The stack is attached to the first
// occurrence per goroutine kind per process only (spec C5 "log the stack
// once"): a deterministic panic on every attempt must not flood the log.
func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
	attrs := []any{
		"where", where,
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"panic", fmt.Sprint(r),
	}
	if _, seen := panicStackLogged.LoadOrStore(where, struct{}{}); !seen {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	c.logger.Error(event, attrs...)
}

// recoverGoroutine is deferred by the repair and on-demand reconcile
// goroutines as their LAST defer, so it runs first: the panic is logged
// while the suite mutex is still held, then the goroutine's other defers
// release the mutex and the WaitGroup. The adoption goroutine defers
// recoverAdoption instead, which also counts and records the failure.
//
// AIDEV-NOTE: recover() works only because this method IS the deferred
// function (`defer c.recoverGoroutine(...)`). Wrapping it in a closure
// (`defer func() { c.recoverGoroutine(...) }()`) makes recover() return
// nil and the panic kills the daemon. An adoption panic is recorded as an
// adoption failure by recoverAdoption, before the mutex is released
// (spec C5).
func (c *Checker) recoverGoroutine(where string, suite SuiteRef) {
	r := recover()
	if r == nil {
		return
	}
	c.reportPanic("adoption_panicked", where, suite, r)
}

// checkLockedSafe runs checkLocked and converts a panic into a FAILED
// check (spec C4): logged, and last_check_at bumped like any failed check
// so the cooldown gates the next T1/T2 trigger. An admission lease the
// check took before panicking is released here; the caller releases the
// suite mutex.
//
// AIDEV-NOTE: nothing in check between TryLock and this call, or between
// it and the goroutine hand-off, may panic: checkRecovered (scheduler)
// and the T1 recover cannot release a mutex they do not know about.
func (c *Checker) checkLockedSafe(ctx, fetchCtx context.Context, scheme, host, suitePath string, opts checkOpts) (out *checkOutcome, ok bool) {
	out = &checkOutcome{report: checkReport{result: resultSkipped}}
	defer func() {
		if r := recover(); r != nil {
			out.lease.release()
			c.reportPanic("freshness_check_panicked", "check",
				SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, r)
			c.bumpFailedCheck(ctx, scheme, host, suitePath)
			freshnessCheckTotal.Inc("failed", host)
			out, ok = nil, false
		}
	}()
	c.checkLocked(ctx, fetchCtx, out, scheme, host, suitePath, opts)
	return out, true
}

// bumpFailedCheck mirrors checkLocked's unknown-check bookkeeping after a
// panic: last_check_at = now and one more on the streak (spec C4);
// last_success_at untouched. Best effort; it never panics its caller.
// AIDEV-NOTE: phase D turns the Put into the UPDATE-only writer.
func (c *Checker) bumpFailedCheck(ctx context.Context, scheme, host, suitePath string) {
	defer func() { _ = recover() }()
	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		cur = &cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	case err != nil:
		c.logger.Warn("freshness: read suite row after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return
	}
	now := c.now().Unix()
	cur.LastCheckAt = &now
	cur.CheckFailCount++
	if err := c.cache.PutSuiteFreshness(ctx, *cur); err != nil {
		c.logger.Warn("freshness: persist failure bump after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
	}
}

// clockStepTolerance: stamps are whole seconds, so a sub-second backward
// NTP step can make a just-written stamp look up to 1 s in the future.
// Only a stamp further ahead than this counts as a clock step.
const clockStepTolerance = 2 * time.Second

// clockStepLogEvery rate-limits clock_step_detected to one line per
// minute per checker (monotonic).
const clockStepLogEvery = time.Minute

var processStart = time.Now()

// clockStep records a past-event stamp found in the future (spec C4).
//
// AIDEV-NOTE: only PAST-EVENT stamps (last_check_at, last_success_at,
// last_requested_at, adoption_fail_first_at/last_at, adopted_at) are
// "infinitely old" when in the future. adoption_next_retry_at is a
// FUTURE-event stamp: a detection is logged through this function, but
// the stamp is clamped to now+1h (retryGateFor), never treated as
// infinitely old.
func (c *Checker) clockStep(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	clockStepsTotal.Inc()
	last := c.lastClockStepLog.Load()
	mono := int64(time.Since(processStart))
	if last != 0 && time.Duration(mono-last) < clockStepLogEvery {
		return
	}
	if !c.lastClockStepLog.CompareAndSwap(last, mono) {
		return
	}
	c.logger.Warn("clock_step_detected",
		"stamp", stamp,
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"stored_unixtime", stored,
		"now_unixtime", now.Unix(),
	)
}

// checkLocked is the body of Check, run with the per-suite mutex held.
// It fills out: out.req is non-nil iff the observed result is "changed",
// the caller has an Adopter wired in (Phase 2 path) AND an admission slot
// was free (out.lease); the caller uses this signal to spawn the adoption
// goroutine. On a confirmed-unchanged result planRepair sets out.repair
// and out.lease when there is something to heal and a slot was free.
//
// Form dispatch (SPEC2 §7.6.3):
//   - If the suite has a current snapshot, the form is read from the
//     snapshot row: snapshot.release_hash != nil → detached, otherwise
//     inline. The check then issues a conditional GET against
//     InRelease (inline) or Release (detached).
//   - For first-ever checks (no current snapshot), inline is tried
//     first. If the inline conditional GET 404s AND a Release url_path
//     row exists, we fall back to the detached path. This is what
//     bootstraps detached-mode adoption for upstreams that ship only
//     Release + Release.gpg.
//
// Outcomes (all paths must update suite_freshness so cooldown applies
// to the next attempt):
//
//   - Cooldown gate fails: no DB write, return nil (reported as
//     skipped). The scheduler's slot-driven re-check of a waiting suite
//     bypasses the gate (opts.bypassCooldown, spec C4).
//   - Metadata url_path row absent: no DB write (we have nothing to
//     check against), return nil — first request for this suite will
//     land in url_path through the normal miss path.
//   - 304: last_check_at = last_success_at = now.
//   - 200, body hash matches cached: bump last_check_at, last_success_at,
//     and refresh validators (upstream may have rotated etag/lastmod
//     while bytes are unchanged); clear any adoption failure record.
//   - 200, body hash differs: bump last_check_at, last_success_at,
//     stamp inrelease_change_seen_at on first sighting only, return
//     *adoptionRequest.
//   - Same, but no admission slot is free (spec C4): "changed_queued",
//     no request, last_success_at left at its previous value; the
//     scheduler parks the suite until a slot it can use is released.
//   - Spec C7 gate (retryGateFor, adopter wired only): while a failure
//     record exists and next_retry_at has not come, the GET carries the
//     failing content's validators, and a 304 or the same content key
//     is "changed_deferred" (no adoption); new content is attempted at
//     once; at next_retry_at the GET carries no validators.
//   - Error (network, 4xx, 5xx, check deadline — also one hit while
//     waiting for the host slot): bump last_check_at and extend
//     check_fail_count — don't hammer a broken upstream — and log
//     (recordUnknownEnd). An unexpected success status does the same;
//     last_success_at keeps its previous value either way. Every
//     answered check above resets check_fail_count to 0 (spec C4).
//   - Cancelled caller (shutdown), at the host slot or the GET: no DB
//     write; it says nothing about upstream.
//
// SPEC2 wiring: the body-hash comparison uses
// suite_snapshot.inrelease_hash (inline) or .release_hash (detached)
// when the suite has an adopted snapshot. Without this, every
// freshness check after a successful adoption would observe "changed"
// against the stale url_path.blob_hash and thrash the adoption
// candidate-uniqueness constraint forever.
func (c *Checker) checkLocked(ctx, fetchCtx context.Context, out *checkOutcome, scheme, host, suitePath string, opts checkOpts) {
	now := c.now()
	nowUnix := now.Unix()

	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		cur = &cache.SuiteFreshness{
			CanonicalScheme: scheme,
			CanonicalHost:   host,
			SuitePath:       suitePath,
		}
	case err != nil:
		c.logger.Warn("freshness: read suite row failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
	}

	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
	// the future (backward wall-clock step) is treated as infinitely old,
	// never as "just checked" — otherwise the suite freezes for the size
	// of the step (security finding I-7).
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		last := time.Unix(*cur.LastCheckAt, 0)
		switch {
		case last.After(now.Add(clockStepTolerance)):
			c.clockStep("last_check_at", scheme, host, suitePath, *cur.LastCheckAt, now)
		case opts.bypassCooldown:
			// The scheduler's slot-driven re-check (spec C4).
		case now.Sub(last) < c.cooldown:
			return // out.report stays resultSkipped
		}
	}

	suite := SuiteRef{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suitePath,
	}

	// Form preference: if there's a current snapshot, the form is
	// determined by which hash columns it has set. Otherwise default
	// to inline; the inline path triggers a detached fallback on 404.
	form := c.detectForm(ctx, cur)
	if form == adoptionFormDetached {
		out.req, out.repair = c.checkLockedDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
	} else {
		req, repair, fellBack := c.checkLockedInline(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		if fellBack {
			req, repair = c.checkLockedDetached(ctx, fetchCtx, out, cur, suite, now, nowUnix)
		}
		out.req, out.repair = req, repair
	}
	if out.req == nil && out.repair != nil {
		c.planRepair(ctx, out)
	}
}

// detectForm reads the suite's current snapshot (if any) and returns
// the form whose hash columns are populated. First-ever suites
// (cur.CurrentSnapshotID == nil) and snapshots that fail to load
// default to inline; the inline-path fallback handles the boot-strap
// case for detached-only upstreams.
//
// AIDEV-TODO(phase E): unsigned form dispatch.
func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) adoptionForm {
	if cur.CurrentSnapshotID == nil {
		return adoptionFormInline
	}
	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	if err != nil || snap == nil {
		return adoptionFormInline
	}
	if snap.Form == cache.FormDetached {
		return adoptionFormDetached
	}
	return adoptionFormInline
}

// checkLockedInline runs the conditional GET against InRelease and
// processes the result. Returns:
//   - (req, false) on success (req may be nil for unchanged/304).
//   - (nil, true) when the inline path got 404 AND the suite has no
//     current snapshot AND a Release url_path row exists — the
//     dispatcher then retries via checkLockedDetached. We return
//     without persisting in the fallback case so the detached
//     attempt's persist is the only one that runs.
//
// The body of this function is the SPEC §7.2 inline algorithm; it has
// been factored out of checkLocked to give detached mode (SPEC2 §7.6.3)
// a parallel implementation. Mutates *cur in the non-fallback paths.
func (c *Checker) checkLockedInline(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest, bool) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	// Locate the InRelease url_path row to get the upstream URL plus
	// (Phase 1 fallback) the cached blob hash for byte-equality
	// comparison on 200.
	inReleasePath := suitePath + inReleaseFilename
	urlRow, err := c.cache.LookupURL(ctx, scheme, host, inReleasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has freshness state but no cached InRelease url_path row.
		if cur.CurrentSnapshotID == nil {
			// First-ever / never-adopted suite. The detached fallback
			// path triggers on 404 from the upstream's /InRelease, which
			// requires us to actually issue the GET. If we don't even
			// have a url_path row to attempt the GET, try the symmetric
			// detached lookup directly.
			if c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
				return nil, nil, true
			}
			c.logger.Debug("freshness: no cached InRelease url_path, skipping",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// SPEC2 §7.4 freshness recovery. The suite HAS a current
		// snapshot but its InRelease anchor row was reaped (SPEC4 §5 GC
		// TTL during a low-traffic request lull). Without this branch
		// the check dead-ends here and the suite freezes forever: it
		// keeps serving the stale snapshot from snapshot_member and
		// never observes upstream rolling forward — the production
		// freeze trap. Reconstruct the upstream URL from the suite
		// identity, re-seed the anchor (pinned to the snapshot's adopted
		// hash so the SPEC4 §5 guards re-protect it), and fall through
		// to the conditional GET with the synthesized row.
		anchorHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, inReleaseFilename)
		urlRow = c.reconstructAnchor(ctx, scheme, host, suitePath, inReleasePath, *cur.CurrentSnapshotID, anchorHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup InRelease url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil, false
	}

	// Determine the cached InRelease hash for the body comparison.
	// Phase 2: snapshot is authoritative (suite_snapshot.inrelease_hash).
	// Phase 1 fallback: url_path.blob_hash. Without the Phase 2
	// branch, every freshness check after a successful adoption sees
	// the stale url_path hash and reports "changed" forever.
	var cachedHash string
	signedCurrent := false
	if cur.CurrentSnapshotID != nil {
		snap, serr := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		if serr != nil {
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", serr,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
		} else {
			signedCurrent = snap.Form == cache.FormInline || snap.Form == cache.FormDetached
			if snap.Form == cache.FormInline && snap.SingleAnchorHash != nil {
				cachedHash = *snap.SingleAnchorHash
			}
		}
	}
	if cachedHash == "" {
		if urlRow.BlobHash == nil || *urlRow.BlobHash == "" {
			c.logger.Debug("freshness: no baseline blob hash, skipping",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		cachedHash = *urlRow.BlobHash
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           urlRow.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	// SPEC §9.3: bound concurrent upstream calls to host. Without
	// this, distinct suites on the same host all run Conditional in
	// parallel, each capable of pulling MaxInReleaseBytes into
	// memory — a resource-exhaustion path under a refresh storm or
	// an adversarial allowlisted upstream. Sharing the limiter with
	// the handler's miss path means cache-miss fetches and
	// freshness checks contend for the same per-host budget.
	release, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, false
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.recordUnknownEnd(ctx, cur, suite, nowUnix, "inline", err)
		out.report.result = "failed"
		return nil, nil, false
	}
	defer release()

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil, false
		}
		// 404 with no current snapshot AND a Release url_path row →
		// fall back to detached without persisting. The detached
		// attempt becomes the authoritative result for this Check.
		if cur.CurrentSnapshotID == nil && isStatusNotFound(err) &&
			c.hasReleaseURLRow(ctx, scheme, host, suitePath) {
			c.logger.Debug("freshness: InRelease 404 on first-ever check, falling back to detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil, true
		}
		c.recordUnknownEnd(ctx, cur, suite, nowUnix, "inline", err)
		out.report.result = "failed"
		return nil, nil, false
	}

	prevSuccess, prevFails := cur.LastSuccessAt, cur.CheckFailCount
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	// SPEC §10: every freshness attempt emits a structured log line. The
	// `result` enum is what operators pivot on — success-with-no-change
	// (the steady state) vs. success-with-change (the interesting one).
	var (
		result      string
		req         *adoptionRequest
		clearRecord bool
	)
	switch res.Status {
	case 304:
		if gate.active {
			// The failing content's validators matched: upstream still
			// serves what failed (spec C7 changed_deferred).
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		} else {
			// Upstream serves the cached anchor again; clear the pending
			// diagnostic (e.g. after an upstream rollback).
			cur.InReleaseChangeSeenAt = nil
			result = "not_modified"
		}
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		switch {
		case newHash == cachedHash:
			// Same bytes as the current anchor: refresh validators (the
			// only response they are ever taken from, spec C4), clear the
			// pending diagnostic and any failure record (spec C7).
			if res.ETag != "" {
				v := res.ETag
				cur.InReleaseETag = &v
			}
			if res.LastModified != "" {
				v := res.LastModified
				cur.InReleaseLastMod = &v
			}
			cur.InReleaseChangeSeenAt = nil
			clearRecord = gate.active
			result = "unchanged"
		case gate.active && !gate.due && newHash == gate.hash:
			// An upstream that ignores validators re-served the failing
			// bytes: deferred exactly like the 304 above.
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		default:
			// Upstream has a new InRelease. Record the diagnostic
			// regardless of whether an Adopter is wired — operators
			// monitoring a Phase 1 deployment still see the
			// divergence.
			setChangeSeen(cur, nowUnix)
			c.logger.Info("InRelease changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			result = "changed"
			// The body is bounded by maxBody (§7.2 cap, default 4 MiB),
			// so retaining it through the goroutine handoff is acceptable.
			req = &adoptionRequest{
				suite:   suite,
				form:    adoptionFormInline,
				key:     newHash,
				bytes:   res.Body,
				etag:    res.ETag,
				lastmod: res.LastModified,
			}
			if c.adopter != nil {
				l, ok := c.admission.tryAcquire(host, signedCurrent)
				if !ok {
					// Spec C4: no slot — spawn nothing and keep
					// last_success_at. A scheduler check reports
					// changed_queued and the scheduler parks the suite
					// until a slot it can use is released; a client
					// check leaves it due for the next tick.
					result, req = "changed_queued", nil
					cur.LastSuccessAt = prevSuccess
				} else {
					out.lease = l
				}
			}
		}
	default:
		// fetch.Conditional should only ever return 200 or 304 in the
		// success path. Anything else here is a contract violation —
		// log loudly and treat as failure-bump.
		c.logger.Error("freshness: unexpected success status",
			"status", res.Status,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		cur.LastSuccessAt = prevSuccess    // undo the optimistic success bump
		cur.CheckFailCount = prevFails + 1 // undo the optimistic reset (spec C4)
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed", "err", perr)
		}
		out.report.result = "failed"
		return nil, nil, false
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, scheme, host, suitePath); err != nil {
			c.logger.Warn("freshness: clear adoption failure failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}

	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "inline",
		"result", result,
		"upstream_status", res.Status,
	}
	if result == "changed_deferred" {
		logAttrs = append(logAttrs, "reason", gate.reason)
	}
	out.report = checkReport{result: result, signedCurrent: signedCurrent}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(result, host)
	return req, repairIfUnchanged(result, suite, cur), false
}

// checkLockedDetached runs the SPEC2 §7.6.3 detached-form freshness
// check: a conditional GET on Release for change detection, plus a
// fresh GET on Release.gpg when Release has changed. Mutates *cur.
//
// Logically symmetric to checkLockedInline. The differences:
//   - Two url_path rows are required (Release AND Release.gpg).
//     Either missing → skip.
//   - On observed change, fetch Release.gpg in a second call so the
//     Adopter can verify the detached signature.
//   - The adoptionRequest carries form=detached + both bodies.
func (c *Checker) checkLockedDetached(ctx, fetchCtx context.Context, out *checkOutcome, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
	scheme, host, suitePath := suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath

	releasePath := suitePath + releaseFilename
	releaseGPGPath := suitePath + releaseGPGFilename

	releaseURL, err := c.cache.LookupURL(ctx, scheme, host, releasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// SPEC2 §7.4 freshness recovery (detached): an adopted suite
		// whose Release anchor was reaped must recover, not freeze —
		// symmetric to the inline path. First-ever suites (no snapshot)
		// still skip.
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		relHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseFilename)
		releaseURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releasePath, *cur.CurrentSnapshotID, relHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}
	releaseGPGURL, err := c.cache.LookupURL(ctx, scheme, host, releaseGPGPath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		if cur.CurrentSnapshotID == nil {
			c.logger.Debug("freshness: no cached Release.gpg url_path, skipping detached",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		gpgHash := c.snapshotAnchorHash(ctx, *cur.CurrentSnapshotID, releaseGPGFilename)
		releaseGPGURL = c.reconstructAnchor(ctx, scheme, host, suitePath, releaseGPGPath, *cur.CurrentSnapshotID, gpgHash, nowUnix)
	case err != nil:
		c.logger.Warn("freshness: lookup Release.gpg url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}

	// Determine the cached Release hash for body comparison.
	// snapshot.release_hash is authoritative once an adoption has
	// landed; pre-adoption (or post-eviction) we fall back to
	// url_path.blob_hash.
	var cachedHash string
	signedCurrent := false
	if cur.CurrentSnapshotID != nil {
		snap, serr := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		if serr != nil {
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", serr,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
		} else {
			signedCurrent = snap.Form == cache.FormInline || snap.Form == cache.FormDetached
			if snap.ReleaseHash != nil {
				cachedHash = *snap.ReleaseHash
			}
		}
	}
	if cachedHash == "" {
		if releaseURL.BlobHash == nil || *releaseURL.BlobHash == "" {
			c.logger.Debug("freshness: no baseline Release blob hash, skipping",
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		cachedHash = *releaseURL.BlobHash
	}

	target := &fetch.Target{
		CanonicalHost: host,
		URL:           releaseURL.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)

	releaseSlot, err := c.hostSem.Acquire(fetchCtx, host)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.logger.Debug("freshness: host limiter acquire aborted",
				"err", err,
				"canonical_host", host,
				"suite_path", suitePath,
			)
			return nil, nil
		}
		// Spec C4: the check deadline hit while waiting for the slot.
		c.recordUnknownEnd(ctx, cur, suite, nowUnix, "detached", err)
		out.report.result = "failed"
		return nil, nil
	}
	defer releaseSlot()

	res, err := c.fetcher.Conditional(fetchCtx, target, etag, lastmod, c.maxBody)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil
		}
		c.recordUnknownEnd(ctx, cur, suite, nowUnix, "detached", err)
		out.report.result = "failed"
		return nil, nil
	}

	prevSuccess, prevFails := cur.LastSuccessAt, cur.CheckFailCount
	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix
	cur.CheckFailCount = 0

	var (
		result      string
		req         *adoptionRequest
		clearRecord bool
	)
	switch res.Status {
	case 304:
		if gate.active {
			// Release still matches the failing content's validators: a
			// re-signature behind this 304 waits for the retry (spec C7).
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		} else {
			cur.InReleaseChangeSeenAt = nil
			result = "not_modified"
		}
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		if newHash == cachedHash {
			if res.ETag != "" {
				v := res.ETag
				cur.InReleaseETag = &v
			}
			if res.LastModified != "" {
				v := res.LastModified
				cur.InReleaseLastMod = &v
			}
			cur.InReleaseChangeSeenAt = nil
			clearRecord = gate.active
			result = "unchanged"
			break
		}
		setChangeSeen(cur, nowUnix)
		// The content key covers the signature, so Release.gpg is fetched
		// before the gate can compare (no validators: the signature is
		// bound to this Release).
		gpgTarget := &fetch.Target{
			CanonicalHost: host,
			URL:           releaseGPGURL.UpstreamURL,
		}
		gpgRes, gerr := c.fetcher.Conditional(fetchCtx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
		if gerr != nil || gpgRes.Status != 200 {
			// As before: record the change and retry on a later check;
			// last_success_at stays bumped because Release answered.
			if gerr != nil {
				c.logger.Warn("freshness: Release.gpg fetch failed; deferring adoption",
					"err", gerr, "canonical_host", host, "suite_path", suitePath)
			} else {
				c.logger.Warn("freshness: Release.gpg fetch returned non-200; deferring adoption",
					"status", gpgRes.Status, "canonical_host", host, "suite_path", suitePath)
			}
			result = "changed"
			break
		}
		key := contentKey(adoptionFormDetached, nil, res.Body, gpgRes.Body)
		if gate.active && !gate.due && key == gate.hash {
			result = "changed_deferred"
			break
		}
		c.logger.Info("Release changed at upstream",
			"canonical_host", host,
			"suite_path", suitePath,
			"cached_hash", cachedHash,
			"upstream_hash", newHash,
		)
		result = "changed"
		req = &adoptionRequest{
			suite:        suite,
			form:         adoptionFormDetached,
			key:          key,
			releaseBytes: res.Body,
			sigBytes:     gpgRes.Body,
			etag:         res.ETag,
			lastmod:      res.LastModified,
		}
		if c.adopter != nil {
			l, ok := c.admission.tryAcquire(host, signedCurrent)
			if !ok {
				// Spec C4: no slot — spawn nothing and keep
				// last_success_at. A scheduler check reports
				// changed_queued and the scheduler parks the suite
				// until a slot it can use is released; a client
				// check leaves it due for the next tick.
				result, req = "changed_queued", nil
				cur.LastSuccessAt = prevSuccess
			} else {
				out.lease = l
			}
		}
	default:
		c.logger.Error("freshness: unexpected success status",
			"status", res.Status,
			"canonical_host", host,
			"suite_path", suitePath,
			"form", "detached",
		)
		cur.LastSuccessAt = prevSuccess    // undo the optimistic success bump
		cur.CheckFailCount = prevFails + 1 // undo the optimistic reset (spec C4)
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed", "err", perr)
		}
		out.report.result = "failed"
		return nil, nil
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, scheme, host, suitePath); err != nil {
			c.logger.Warn("freshness: clear adoption failure failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}

	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "detached",
		"result", result,
		"upstream_status", res.Status,
	}
	if result == "changed_deferred" {
		logAttrs = append(logAttrs, "reason", gate.reason)
	}
	out.report = checkReport{result: result, signedCurrent: signedCurrent}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(result, host)
	return req, repairIfUnchanged(result, suite, cur)
}

// recordUnknownEnd is the bookkeeping of a check that ended unknown (spec
// C4): a Conditional error, or the check deadline hit while waiting for
// the host slot. Used by both checkLockedInline and checkLockedDetached.
// SPEC §7.2: last_check_at is bumped to space out the next attempt and
// the streak extends; last_success_at is left alone — it carries the "we
// know upstream is fine" signal that periodic_refresh uses.
//
// AIDEV-NOTE: ctx is the caller's context, never fetchCtx: the deadline
// that ended the check must not fail the write that records it.
// AIDEV-NOTE: in phase B every Conditional error extends the streak,
// including 401/403/404/410, which spec C4 classifies as absent, not
// unknown. Phase E's probe classification must reset the streak on
// absent, or anchor_absent suites inherit the hourly backoff.
func (c *Checker) recordUnknownEnd(ctx context.Context, cur *cache.SuiteFreshness, suite SuiteRef, nowUnix int64, form string, err error) {
	host, suitePath := suite.CanonicalHost, suite.SuitePath
	cur.LastCheckAt = &nowUnix
	cur.CheckFailCount++
	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist failure-bump failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	c.logger.Info("freshness check failed",
		"err", err,
		"canonical_host", host,
		"suite_path", suitePath,
		"form", form,
		"result", "failed",
	)
	freshnessCheckTotal.Inc("failed", host)
}

// reconstructAnchor synthesizes — and best-effort re-seeds — a reaped
// metadata anchor url_path row (InRelease, or Release/Release.gpg for
// detached) for an adopted suite. SPEC2 §7.4 freshness recovery; used by
// both checkLockedInline and checkLockedDetached.
//
// The returned in-memory row is what the checker uses for the conditional
// GET this tick; the DB re-seed restores the row so the next tick takes
// the normal path and the SPEC4 §5 GC identity guard (guard (d)) protects
// it going forward. blobHash is the current snapshot's adopted hash so
// the hash-based guards (b)/(c) also match.
//
// AIDEV-TODO(port-correctness): upstream_url is reconstructed via the
// (test-seam) policy aptpath.BuildUpstreamURL(scheme, host, path). Canonical host is port-less
// (SPEC §3.2), so a non-default upstream port (e.g. http://host:8080/...)
// is lost here. A reaped anchor on a non-default-port upstream therefore
// recovers to the wrong authority — the conditional GET fails (logged,
// last_check bumped, last_success NOT bumped → retry loop, no silent
// freeze) and GPG/parse verification blocks any wrong-port content from
// being adopted, so this degrades safely rather than corrupting. It only
// bites a non-default-port suite whose anchor was already reaped (the
// port-correct row is gone). The durable fix is to persist the anchor's
// upstream URL on suite_snapshot in a future schema migration and read
// it here; deferred (v5 shipped snapshot_skipped_member instead).
// In steady state this path does not run: CommitAdoption Step 3c keeps
// the anchor present with its real upstream_url and the GC identity guard
// (SPEC4 §5 guard (d)) stops it being reaped.
//
// AIDEV-NOTE: the LookupURL-miss → PutURLPath re-seed is not a clobber
// race. All writers of an ADOPTED suite's anchor serialize on the
// per-suite mutex (this Check plus the adoption goroutine it hands the
// mutex to); the handler miss-path never writes an adopted suite's anchor
// because that request is served from the snapshot (trySnapshotHit),
// never a miss. So no concurrent writer can interleave a fresher,
// port-correct row between the miss read and this re-seed.
func (c *Checker) reconstructAnchor(ctx context.Context, scheme, host, suitePath, anchorPath string, snapshotID int64, blobHash *string, nowUnix int64) *cache.URLPath {
	upstreamURL := c.urlReconstruct(scheme, host, anchorPath)
	row := cache.URLPath{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		Path:            anchorPath,
		BlobHash:        blobHash,
		UpstreamURL:     upstreamURL,
		IsMetadata:      true,
		LastRequestedAt: &nowUnix,
		LastFetchedAt:   &nowUnix,
	}
	// Warn (not Debug): the silent skip this replaces hid a week-long
	// freeze of ~20 suites in the field. SPEC §10.
	c.logger.Warn("freshness: metadata anchor missing for adopted suite; reconstructing",
		"canonical_host", host,
		"suite_path", suitePath,
		"anchor", anchorPath,
		"snapshot_id", snapshotID,
		"upstream_url", upstreamURL,
	)
	freshnessAnchorReconstructedTotal.Inc(host)
	if perr := c.cache.PutURLPath(ctx, row); perr != nil {
		c.logger.Warn("freshness: re-seed metadata anchor failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
			"anchor", anchorPath,
		)
	}
	return &row
}

// snapshotAnchorHash returns the adopted hash for the given anchor
// filename from the suite's current snapshot, or nil if unavailable. Used
// by the recovery paths to pin a reconstructed anchor's blob_hash to the
// snapshot so the SPEC4 §5 hash guards re-protect the re-seeded row.
func (c *Checker) snapshotAnchorHash(ctx context.Context, snapshotID int64, filename string) *string {
	snap, err := c.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil || snap == nil {
		return nil
	}
	var h *string
	switch filename {
	case inReleaseFilename:
		if snap.Form == cache.FormInline {
			h = snap.SingleAnchorHash
		}
	case releaseFilename:
		h = snap.ReleaseHash
	case releaseGPGFilename:
		h = snap.ReleaseGPGHash
	}
	if h == nil {
		return nil
	}
	v := *h
	return &v
}

// hasReleaseURLRow reports whether url_path has rows for both Release
// and Release.gpg under (scheme, host, suitePath). Used by the inline
// path's 404 fallback to decide whether a detached retry is even
// possible — without both rows, the detached fetch would skip with a
// "no cached Release url_path" debug log anyway.
func (c *Checker) hasReleaseURLRow(ctx context.Context, scheme, host, suitePath string) bool {
	if _, err := c.cache.LookupURL(ctx, scheme, host, suitePath+releaseFilename); err != nil {
		return false
	}
	if _, err := c.cache.LookupURL(ctx, scheme, host, suitePath+releaseGPGFilename); err != nil {
		return false
	}
	return true
}

// isStatusNotFound reports whether err is a fetch.StatusError carrying
// HTTP 404. Used by checkLockedInline to decide whether to fall back
// to detached mode on a missing /InRelease.
func isStatusNotFound(err error) bool {
	var se *fetch.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Code == 404
}

// repairIfUnchanged builds the SPEC6_7 §3 repair signal: the check
// CONFIRMED upstream is unchanged ("not_modified" 304 or byte-identical
// "unchanged" 200) and the suite has a current snapshot. Any other
// result returns nil — "changed" routes to adoption (whose snapshot
// supersedes the old skip records), and failures prove nothing about
// upstream having synced.
func repairIfUnchanged(result string, suite SuiteRef, cur *cache.SuiteFreshness) *repairRequest {
	if result != "not_modified" && result != "unchanged" {
		return nil
	}
	if cur.CurrentSnapshotID == nil {
		return nil
	}
	return &repairRequest{suite: suite, snapshotID: *cur.CurrentSnapshotID}
}

// retryGate is a check's view of the suite's adoption failure record
// (spec C7 "Gate").
type retryGate struct {
	active  bool   // a failure or refusal record exists
	due     bool   // now >= next_retry_at (after clamping)
	hash    string // content key of the failing content
	etag    string // the failing content's validators
	lastmod string
	reason  string
	// clampTo is non-zero when the stored next_retry_at lay beyond
	// retryHorizon (a backward clock step); applyClamp persists it.
	clampTo int64
}

// retryGateFor reads the gate from the suite row.
//
// AIDEV-NOTE: next_retry_at is a FUTURE-event stamp. It is never
// "infinitely old" (that would disable backoff after a backward step);
// a value beyond the legitimate horizon is clamped to now + 1 h instead.
func (c *Checker) retryGateFor(cur *cache.SuiteFreshness, now time.Time) retryGate {
	// Phase 1 parity: with no adopter nothing is ever attempted, so a
	// record left by an earlier run with adoption enabled is ignored.
	if c.adopter == nil || cur.AdoptionFailCount < 1 || cur.AdoptionFailHash == nil {
		return retryGate{}
	}
	g := retryGate{active: true, due: true, hash: *cur.AdoptionFailHash}
	if cur.AdoptionFailETag != nil {
		g.etag = *cur.AdoptionFailETag
	}
	if cur.AdoptionFailLastMod != nil {
		g.lastmod = *cur.AdoptionFailLastMod
	}
	if cur.AdoptionFailReason != nil {
		g.reason = *cur.AdoptionFailReason
	}
	if cur.AdoptionNextRetryAt != nil {
		next := time.Unix(*cur.AdoptionNextRetryAt, 0)
		if next.After(now.Add(retryHorizon)) {
			next = now.Add(retryMaxDelay)
			g.clampTo = next.Unix()
		}
		g.due = !now.Before(next)
	}
	return g
}

// validators returns what the conditional GET sends: the current
// anchor's validators with no record, the failing content's before
// next_retry_at, none at or after it.
func (g retryGate) validators(cur *cache.SuiteFreshness) (etag, lastmod string) {
	switch {
	case g.active && g.due:
		return "", ""
	case g.active:
		return g.etag, g.lastmod
	}
	if cur.InReleaseETag != nil {
		etag = *cur.InReleaseETag
	}
	if cur.InReleaseLastMod != nil {
		lastmod = *cur.InReleaseLastMod
	}
	return etag, lastmod
}

// applyClamp persists a clamped next_retry_at (backward clock step).
func (c *Checker) applyClamp(ctx context.Context, cur *cache.SuiteFreshness, g retryGate, now time.Time) {
	if g.clampTo == 0 {
		return
	}
	c.clockStep("adoption_next_retry_at", cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, *cur.AdoptionNextRetryAt, now)
	if err := c.cache.SetAdoptionNextRetry(ctx, cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, g.clampTo); err != nil {
		c.logger.Warn("freshness: clamp adoption retry failed",
			"err", err, "canonical_host", cur.CanonicalHost, "suite_path", cur.SuitePath)
	}
}

// setChangeSeen stamps the first sighting of a pending upstream change;
// later sightings keep it, so "lagging" measures from first sight
// (spec C4).
func setChangeSeen(cur *cache.SuiteFreshness, now int64) {
	if cur.InReleaseChangeSeenAt == nil {
		v := now
		cur.InReleaseChangeSeenAt = &v
	}
}

// Reconcile forces an in-place reconcile of one suite's current snapshot
// (SPEC6_8 on-demand recovery). Returns true if a reconcile was triggered;
// false if the suite is unknown / has no current snapshot (no lock allocated)
// or another check holds the suite lock. It also returns false when no
// admission slot is free (spec C4: POST /reconcile then answers 409). The
// reconcile runs the full trust-validated heal asynchronously — callers
// watch adoption_snapshot_reconciled.
//
// AIDEV-NOTE: validate-before-LoadOrStore ordering is the SPEC6_8 lock-growth
// mitigation. GetSuiteFreshness is called FIRST; if the suite is unknown or
// has no CurrentSnapshotID we return false WITHOUT calling c.locks.LoadOrStore.
// This bounds the lock map to real adopted suites: an unauthenticated caller
// who forges (host, suite) pairs cannot grow c.locks unless the suite is
// already in the cache's suite_freshness table (which requires a successful
// upstream adoption). Compare Check, which does LoadOrStore unconditionally
// because T1 callers have already verified the host via the handler's
// allowlist and the suite path via a cache hit.
func (c *Checker) Reconcile(ctx context.Context, scheme, host, suitePath string) bool {
	if c.adopter == nil {
		return false
	}
	// Validate-before-LoadOrStore: confirm the suite has a current snapshot
	// before touching the lock map. Unknown / unadopted suites are rejected
	// here without allocating a per-suite mutex.
	sf, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	if err != nil || sf == nil || sf.CurrentSnapshotID == nil {
		return false
	}
	snapID := *sf.CurrentSnapshotID
	suite := SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}

	muVal, _ := c.locks.LoadOrStore(suiteKey(scheme, host, suitePath), &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		return false
	}
	l, ok := c.admission.tryAcquire(host, true)
	if !ok {
		mu.Unlock()
		return false // POST /reconcile answers 409 (spec C4)
	}
	// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
	// release, recover (first). See recoverGoroutine.
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		defer c.recoverGoroutine("reconcile", suite)
		rctx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		if _, err := c.adopter.ReconcileSnapshot(rctx, suite, snapID, true); err != nil {
			c.logger.Warn("adoption_reconcile_failed",
				"canonical_host", host, "suite_path", suitePath,
				"snapshot_id", snapID, "err", err)
		}
	}()
	return true
}

// suiteKey is the in-memory lock map key. The pipe separator matches the
// singleflight convention in handler.serveCacheMiss; canonical scheme,
// host, and suite_path never contain a literal pipe (canonicalization
// rejects userinfo and the path is URL-decoded).
func suiteKey(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}
