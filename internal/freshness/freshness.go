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
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
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
	// HasSuiteRow is I-4's row gate before a lock-map entry is stored: one
	// indexed existence read (SELECT 1), cheaper than GetSuiteFreshness.
	HasSuiteRow(ctx context.Context, key cache.SuiteKey) (bool, error)
	// UpdateSuiteFreshness writes a check's result to an existing row
	// (UPDATE-only, spec C3: a vanished row is left alone).
	UpdateSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error
	ListSuitesWithAdoption(ctx context.Context) ([]cache.SuiteWithAdoption, error)
	LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error)
	GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*cache.SuiteSnapshot, error)
	// PutURLPath re-seeds a reaped metadata anchor url_path row during
	// freshness recovery (SPEC2 §7.4). See checkInline's
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
	// MaxSuites is freshness.max_suites (spec C4, C11): the scheduler
	// keeps at most this many suites queued, waiting for a slot or in
	// flight. Zero is unbounded (tests).
	MaxSuites int

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
	maxSuites           int
	inflight            *hostAdoptions // spec C10: signed adoptions in flight per host
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
	// lock is in-memory; it is held for a check and for the adoption,
	// repair or reconcile the check hands it to.
	//
	// AIDEV-NOTE: I-4 (spec C3) bounds the map by the suite table: Check
	// and Reconcile read the suite's row before LoadOrStore, and every
	// acquisition (check, Reconcile, HoldSuite) goes through lockSuite,
	// which re-validates after TryLock that the map still holds the same
	// mutex; a check or Reconcile that then finds the row gone drops its
	// entry, and cap eviction (phase F's reaper too) drops the evictee's
	// through HoldSuite's release(true) — every drop through unlockDropped
	// (drop, then unlock). An entry outlives its row only while a check or
	// Reconcile that loaded it unwinds, or after a read error (LockCount).
	locks sync.Map

	// absentAnswered holds the keys of suites whose last answered check
	// ended anchor_absent (ruling PE-10 b, pacedAbsentAnswer): set and
	// cleared by the check tails (finishCheck, failCheck), dropped with the
	// suite's lock-map entry (unlockDropped), so bounded by the suite table
	// as locks is. In memory only; a restart costs one check that asks.
	absentAnswered sync.Map

	// lateralHolds holds, per suite key, the memo of its last failed
	// lateral attempt (ruling PE-11, lateralHold, lateralHeld): written by
	// recordAdoptionFailure for a lateral request, read by lateralInline,
	// dropped with the suite's lock-map entry (unlockDropped). Failures of
	// the detached form never touch it. In memory only; a restart costs at
	// most one extra attempt per form (see lateralInline).
	lateralHolds sync.Map

	// lockLoadedHook, when set (tests only), runs in lockSuite — so in
	// check, Reconcile and HoldSuite — between loading a suite's mutex and
	// TryLock: the window I-4's re-validation closes.
	lockLoadedHook func(key string)

	// offerHook, when set (tests only), runs on the Run goroutine for each
	// finished offer — a waiting suite's slot-driven re-check — with its
	// report; a queued result is a lost offer (another admission took the
	// slot first). The upgrade-burst simulations count lost offers with it.
	offerHook func(ref SuiteRef, rep checkReport)

	// unlockDroppedHook, when set (tests only), runs in unlockDropped
	// between dropping a suite's lock-map entry and unlocking its mutex:
	// the window whose order (drop, then unlock) keeps two holders apart.
	unlockDroppedHook func(key string)

	// recordHook, when set (tests only), runs in recordAdoptionFailure
	// right before the record is written.
	recordHook func()

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
		maxSuites:           cfg.MaxSuites,
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
		inflight:            newHostAdoptions(),
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

	// crossForm marks a request whose form is not the suite's current
	// form (a lateral move, an unsigned suite's upgrade, the probe tree of
	// an absent anchor): its validators belong to another anchor file than
	// the one the check conditional-GETs, so a failure record keeps none
	// (design review I-1). A successful adoption still stores them.
	crossForm bool

	// lateral marks lateralInline's request (a detached suite's InRelease):
	// its failure is also remembered in Checker.lateralHolds, the
	// lateral's own hold (ruling PE-11).
	lateral bool
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
	// signed: the current snapshot is signed, so the repair may take the
	// slot the reserved-slot rule keeps for signed suites (spec C4;
	// overview "E:" carry-forward).
	signed bool
}

// checkOutcome is what one check produces. lease is the admission slot
// taken for req or for the repair; on a panic checkLockedSafe releases
// it. needRepair/needReconcile say what the repair goroutine heals.
type checkOutcome struct {
	req           *adoptionRequest
	repair        *repairRequest
	needRepair    bool
	needReconcile bool
	declared      []ReleaseMember // what reconcilePending parsed, for the heal (B-28)
	lease         *lease
	// report is what the scheduler learns from this check.
	report checkReport
	// vanished: the suite row was gone when the check read it (I-4).
	vanished bool
}

// checkOpts tunes one check. bypassCooldown is set only by the scheduler:
// for the re-check of a suite that waited for an admission slot (spec C4),
// bounded by slot turnover (one per released slot), and for the check of
// a suite queued with a pending change (Checker.pendingChange), at most
// one per tick until it is parked. The cooldown's job — absorbing
// client-triggered storms — does not apply to either.
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

// isQueuedResult reports whether result means "new content, no slot":
// changed_queued, or first_adoption_queued for a suite with no current
// snapshot (spec C4).
func isQueuedResult(result string) bool {
	return result == "changed_queued" || result == "first_adoption_queued"
}

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
	// A suite whose mutex is held answers busy with no database read: peek
	// at the lock map without storing (FW-6, final review concurrency M3).
	//
	// AIDEV-NOTE: T1 fires on every metadata hit of a tracked suite, so a
	// burst of N index fetches is N checks; before this peek each paid a
	// row read even when busy (N reads, against about one). The peek's
	// TryLock holds a free mutex for an instant: a concurrent lockSuite on
	// the same suite may then answer busy, which every caller already
	// treats as "someone else is on it". The peek never stores, so I-4's
	// "a row before LoadOrStore" stands; checkLocked's read after TryLock
	// stays the re-validation.
	if entry, ok := c.locks.Load(key); ok {
		mu := entry.(*sync.Mutex)
		if !mu.TryLock() {
			return checkReport{result: resultBusy}
		}
		mu.Unlock()
	}
	// I-4 (spec C3): only a suite with a row is checked, and only it gets
	// a lock-map entry; a minted path's T1 costs one indexed existence read.
	if !c.suiteRowExists(ctx, ref) {
		return checkReport{result: resultSkipped}
	}
	mu, st := c.lockSuite(key)
	switch st {
	case lockBusy:
		// Another goroutine is on it; SPEC §7.3 says skip.
		return checkReport{result: resultBusy}
	case lockStale:
		// I-4: the mutex counts only while the lock map still holds it. Cap
		// eviction (and phase F's reaper) drops the entry of a suite it
		// deleted while holding this mutex; a check that loaded it before
		// then must not run beside one that loads the new entry.
		return checkReport{result: resultSkipped}
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
	if out.vanished {
		// I-4: keep the lock map to suites with rows.
		c.unlockDropped(key, mu)
		return out.report
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

// suiteRowExists reports whether the suite has a suite_freshness row
// (I-4: the checker works only on tracked suites), with the cache's
// existence read (SELECT 1; FW-6). A read error counts as "no", logged:
// the check is skipped and the next trigger tries again.
func (c *Checker) suiteRowExists(ctx context.Context, ref SuiteRef) bool {
	ok, err := c.cache.HasSuiteRow(ctx, cache.SuiteKey{CanonicalScheme: ref.CanonicalScheme,
		CanonicalHost: ref.CanonicalHost, SuitePath: ref.SuitePath})
	if err != nil {
		c.logger.Warn("freshness: read suite row failed",
			"err", err, "canonical_host", ref.CanonicalHost, "suite_path", ref.SuitePath)
	}
	return err == nil && ok
}

// spawnAdoption hands the suite mutex and the admission lease to the
// adoption goroutine, which runs against the lifetime context bounded by
// adoption.max_duration (a request closing must not abort it).
//
// AIDEV-NOTE: defer order is load-bearing — in the order they run:
// cancel, recoverAdoption, lease release, Unlock, inflight.end (signed
// adoptions only), Done (last). The failure record is written before the
// mutex is released on both the error and the panic path (spec C7), and
// inflight.end runs after the commit and the Unlock, so a strict-mode
// request it wakes re-reads package_hash rows the new snapshot already
// wrote (spec C10, design note 27; final review, concurrency T9).
// The host slot is freed earlier, when the member pipeline ends; the
// global slot only here, after the flip barrier and the commit (ruling
// R-C1).
// A deadline that passes after the member pipeline, in the flip barrier
// (ruling R-C2), leaves the outcome to the commit's own error. An attempt
// ended by shutdown — a flip abandoned in the barrier (design note 18), or
// any error once the lifetime context has ended (deferred minor C-20) — is
// never counted, logged or recorded as a failure (INFO
// adoption_abandoned_at_shutdown).
func (c *Checker) spawnAdoption(mu *sync.Mutex, req *adoptionRequest, l *lease) {
	c.adoptionWg.Add(1)
	// Spec C10: a signed adoption is in flight from here (before the
	// goroutine runs, so a request right after the check sees it) until
	// the goroutine has returned, its commit done.
	signed := req.form != adoptionFormUnsigned
	if signed {
		c.inflight.begin(req.suite.CanonicalHost)
	}
	go func() {
		defer c.adoptionWg.Done()
		if signed {
			defer c.inflight.end(req.suite.CanonicalHost)
		}
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
		pipelineDone := false // set by membersDone, which runs on this goroutine (runShared Step 11)
		err := c.adopter.runRequest(actx, req, runHooks{
			membersDone: func() { pipelineDone = true; l.releaseHost() },
			stop:        c.lifetimeCtx,
		})
		// A flip abandoned in the barrier means shutdown: main closes the
		// gate at the signal, while the lifetime context still runs (or
		// that context ended). Shutdown is never a failure (spec C7,
		// design note 18): it is not counted in acu_adoption_total, not
		// shown in the admin ring, not recorded, and logged at INFO, so a
		// routine restart pages nobody (final review, tests Minor 3). So
		// does any error once the lifetime context has ended: a
		// verification or member fetch cut short, or runShared's
		// shutdown-before-the-commit check (deferred minor C-20).
		if errors.Is(err, flipgate.ErrAborted) || (err != nil && c.lifetimeCtx.Err() != nil) {
			c.logger.Info("adoption_abandoned_at_shutdown",
				"canonical_host", req.suite.CanonicalHost,
				"suite_path", req.suite.SuitePath,
				"form", formName(req.form),
				"err", err,
			)
			return
		}
		duration := time.Since(start)
		outcome := classifyAdoptionOutcome(err)
		// A deadline that fires in the member pipeline makes the attempt
		// timed_out (a member fetch reports it as its own failure). One that
		// passes in the flip barrier (ruling R-C2) does not: what failed then
		// is the commit, and its own error classifies it (a commit that hit
		// commitTimeout is still timed_out, through its chain).
		if err != nil && !pipelineDone && errors.Is(actx.Err(), context.DeadlineExceeded) && c.lifetimeCtx.Err() == nil {
			outcome = "timed_out"
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
			// A refusal is a state, not a failure (spec C7): its line is
			// INFO, next to the INFO adoption_refused that carries the
			// reason and timing, so operators alerting on WARN are not
			// paged for a flapping mirror.
			level := slog.LevelWarn
			if outcome == "refused" {
				level = slog.LevelInfo
			}
			c.logger.Log(context.Background(), level, "adoption_run_failed",
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
// AIDEV-NOTE: the reserved-slot rule follows the suite (rp.signed: its
// current snapshot is signed), so a signed suite's repair may take the
// last free slot and hold it for up to adoption.max_duration if a member
// stalls (deferred minor B-32, weighed in phase E: kept — spec C4 reserves
// the slot for suites with a signed current snapshot, not for adoptions,
// and the per-host slot keeps one such repair per host). A deferred
// repair is counted (acu_repair_deferred_total, B-31).
func (c *Checker) planRepair(ctx context.Context, out *checkOutcome) {
	rp := out.repair
	if c.adopter == nil || !c.adopter.repairSkippedMembers {
		out.repair = nil
		return
	}
	out.needRepair = c.adopter.repairPending(ctx, rp.suite, rp.snapshotID)
	declared, needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
	if err != nil {
		// A fail-closed anchor (R-1 decode error) must be visible, not a
		// silent non-heal.
		c.logger.Warn("adoption_reconcile_failed",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
			"snapshot_id", rp.snapshotID, "stage", "decision", "err", err)
	}
	out.needReconcile, out.declared = needReconcile, declared
	if !out.needRepair && !out.needReconcile {
		out.repair = nil
		return
	}
	l, ok := c.admission.tryAcquire(rp.suite.CanonicalHost, rp.signed)
	if !ok {
		c.logger.Debug("freshness: repair deferred, no adoption slot",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath)
		repairDeferredTotal.Inc(rp.suite.CanonicalHost)
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
// AIDEV-NOTE: a repair or reconcile that panics deterministically is
// logged (freshness_check_panicked, where=repair), not recorded: the spec
// C7 record is keyed on upstream content, and an unchanged suite's heal
// has none, so the next unchanged check runs it again — one ERROR line
// per cooldown at most (deferred minor B-37, phase B deviation 7; kept in
// phase E: v7 has no record keyed on a snapshot).
func (c *Checker) spawnRepair(mu *sync.Mutex, out *checkOutcome) {
	rp, l, needRepair, needReconcile, declared := out.repair, out.lease, out.needRepair, out.needReconcile, out.declared
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
			if _, err := c.adopter.reconcileSnapshot(rctx, rp.suite, rp.snapshotID, declared, false); err != nil {
				c.logger.Warn("adoption_reconcile_failed",
					"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
					"snapshot_id", rp.snapshotID, "stage", "heal", "err", err)
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
	// Without an adopter nothing adopts: records left by a run with
	// adoption enabled are inert (Phase 1 parity), so rescheduling them
	// would only cost writes.
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

// ErrAdoptionPanicked marks a panicked adoption (security finding I-1):
// the adoption goroutine records it as a failure with reason panic. Repair
// and reconcile panics are logged, not recorded (phase B plan, deviation 7).
var ErrAdoptionPanicked = errors.New("adoption_panicked")

// panicStackLogged holds the goroutine kinds ("adoption", "repair",
// "reconcile", "check", "scheduler") whose stack was already logged.
var panicStackLogged sync.Map

// reportPanic logs a recovered panic (logPanic, with the checker's logger).
func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
	logPanic(c.logger, event, where, suite, r)
}

// logPanic logs a recovered panic as event at ERROR, with where, the suite
// and the panic value. The stack is attached to the first occurrence per
// goroutine kind (where) per process only (spec C5 "log the stack once"): a
// deterministic panic on every attempt must not flood the log. One helper
// for every recover in the package — the checker's goroutines and the
// adopter's heartbeat (deferred minor A-37; the handler's counterpart is
// logGoroutinePanic).
func logPanic(logger *slog.Logger, event, where string, suite SuiteRef, r any) {
	attrs := []any{
		"where", where,
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"panic", fmt.Sprint(r),
	}
	if _, seen := panicStackLogged.LoadOrStore(where, struct{}{}); !seen {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	logger.Error(event, attrs...)
}

// unloggedPanic runs report under its own recover: a report that panics
// itself (a broken log handler) is counted, never escapes (deferred minor
// B-18).
func unloggedPanic(report func()) {
	defer func() {
		if recover() != nil {
			freshnessUnloggedPanics.Inc()
		}
	}()
	report()
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
// and the T1 recover cannot release a mutex they do not know about. The
// same holds for the recover handler itself: a panic in it (a panicking
// logger in reportPanic) would escape with the mutex held for the life
// of the process, so its logging and bookkeeping run under nested
// recovers and it always returns out=nil, ok=false.
func (c *Checker) checkLockedSafe(ctx, fetchCtx context.Context, scheme, host, suitePath string, opts checkOpts) (out *checkOutcome, ok bool) {
	out = &checkOutcome{report: checkReport{result: resultSkipped}}
	defer func() {
		if r := recover(); r != nil {
			out.lease.release()
			out, ok = nil, false
			guarded := func(f func()) {
				defer func() { _ = recover() }()
				f()
			}
			guarded(func() {
				c.reportPanic("freshness_check_panicked", "check",
					SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, r)
			})
			guarded(func() {
				c.bumpFailedCheck(ctx, scheme, host, suitePath)
				freshnessCheckTotal.Inc("failed", host)
			})
		}
	}()
	c.checkLocked(ctx, fetchCtx, out, scheme, host, suitePath, opts)
	return out, true
}

// bumpFailedCheck mirrors checkLocked's unknown-check bookkeeping after a
// panic: last_check_at = now and one more on the streak (spec C4);
// last_success_at untouched. Best effort; it never panics its caller.
// AIDEV-NOTE: UPDATE-only since phase D (spec C3): a row deleted since the
// panic records nothing, and nothing here may create one.
func (c *Checker) bumpFailedCheck(ctx context.Context, scheme, host, suitePath string) {
	defer func() { _ = recover() }()
	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return // the row is gone: nothing to bump (UPDATE-only, spec C3)
	case err != nil:
		c.logger.Warn("freshness: read suite row after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return
	}
	now := c.now().Unix()
	cur.LastCheckAt = &now
	cur.CheckFailCount++
	if err := c.cache.UpdateSuiteFreshness(ctx, *cur); err != nil {
		c.logger.Warn("freshness: persist failure bump after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
	}
}

// reconstructAnchor synthesizes — and best-effort re-seeds — a reaped
// metadata anchor url_path row (InRelease, or Release/Release.gpg for
// detached) for an adopted suite. SPEC2 §7.4 freshness recovery; used by
// both checkInline and checkDetached.
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
// filename ("/InRelease", "/Release", "/Release.gpg") from the suite's
// current snapshot, through its form (cache.SuiteSnapshot.AnchorHash: an
// unsigned snapshot keeps its Release in the single-anchor slot), or nil
// if unavailable. Used by the recovery paths to pin a reconstructed
// anchor's blob_hash to the snapshot so the SPEC4 §5 hash guards
// re-protect the re-seeded row.
func (c *Checker) snapshotAnchorHash(ctx context.Context, snapshotID int64, filename string) *string {
	snap, err := c.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil || snap == nil {
		return nil
	}
	h := snap.AnchorHash(strings.TrimPrefix(filename, "/"))
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
// HTTP 404. Used by checkInline to decide whether to fall back
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

// recordValidators reports whether the conditional GET sends the failure
// record's validators: a record is active, not due, and holds validators —
// the failing content's, from a request of the current form (a cross-form
// request records none).
func (g retryGate) recordValidators() bool {
	return g.active && !g.due && (g.etag != "" || g.lastmod != "")
}

// validators returns what the conditional GET sends: none at or after the
// record's next_retry_at; the failing content's before it when the record
// holds validators; otherwise — no record, or a record without validators —
// the current anchor's own.
//
// AIDEV-NOTE: design review I-1. A record's validators are those of the
// anchor file its request fetched; sent on another file's GET they can
// answer 304 for content never seen (a detached suite's Release behind a
// CDN that sends Last-Modified only, after its lateral InRelease failed),
// which froze the suite. Cross-form requests therefore record none, and a
// 304 is changed_deferred only when the record's own were sent
// (steadyResult).
func (g retryGate) validators(cur *cache.SuiteFreshness) (etag, lastmod string) {
	switch {
	case g.active && g.due:
		return "", ""
	case g.recordValidators():
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

// applyClamp persists a clamped next_retry_at (backward clock step) and
// keeps cur in step with it, so a later gate read in the same check (the
// inline check's detached fallback) does not clamp — and count — it again
// (deferred minor B-23).
func (c *Checker) applyClamp(ctx context.Context, cur *cache.SuiteFreshness, g retryGate, now time.Time) {
	if g.clampTo == 0 {
		return
	}
	c.clockStep("adoption_next_retry_at", cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, *cur.AdoptionNextRetryAt, now)
	if err := c.cache.SetAdoptionNextRetry(ctx, cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, g.clampTo); err != nil {
		c.logger.Warn("freshness: clamp adoption retry failed",
			"err", err, "canonical_host", cur.CanonicalHost, "suite_path", cur.SuitePath)
	}
	clamped := g.clampTo
	cur.AdoptionNextRetryAt = &clamped
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
// false if the suite is unknown / has no current snapshot (no lock allocated;
// a row that vanishes after that read has its entry dropped again)
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
// upstream adoption). Check does the same since phase D (I-4, spec C3), and
// both re-validate after TryLock (lockSuite): the map must still hold the
// mutex and the row must still name the snapshot read before; a row found
// gone has its entry dropped (unlockDropped), as in check.
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

	key := suiteKey(scheme, host, suitePath)
	// I-4 (spec C3): lockSuite re-validates that the lock map still holds
	// the mutex; then, under it, the suite must still have its row, with the
	// snapshot read above still current. Otherwise the suite was deleted
	// (cap eviction, phase F's reaper) or re-adopted meanwhile.
	mu, st := c.lockSuite(key)
	if st != lockOK {
		return false
	}
	sf, err = c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// The row vanished after the pre-read: drop the entry this call
		// may have stored, exactly as check does (the map stays bounded by
		// the suite table).
		c.unlockDropped(key, mu)
		return false
	case err != nil || sf.CurrentSnapshotID == nil || *sf.CurrentSnapshotID != snapID:
		mu.Unlock()
		return false
	}
	// The reserved-slot rule (spec C4): only a signed current snapshot may
	// take the last free slot; a snapshot that cannot be read counts as
	// unsigned.
	signed := false
	if snap, err := c.cache.GetSuiteSnapshot(ctx, snapID); err == nil {
		signed = snap.Form != cache.FormUnsigned
	}
	l, ok := c.admission.tryAcquire(host, signed)
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
				"snapshot_id", snapID, "stage", "on_demand", "err", err)
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
