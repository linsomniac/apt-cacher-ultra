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
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
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

	// now is a test seam; production uses time.Now.
	now func() time.Time

	// urlReconstruct rebuilds the upstream URL for a metadata anchor
	// (InRelease/Release/Release.gpg) when its url_path row has been
	// reaped but the suite still has a current snapshot (SPEC2 §7.4
	// freshness recovery). Test seam; production defaults to
	// scheme://host + path. AIDEV-NOTE: canonical host is port-less
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
	urlReconstruct      func(scheme, host, path string) string
	adopter             *Adopter
	lifetimeCtx         context.Context
	adoptionRing        *observability.Ring
	gpgReasonClassifier func(error) string

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
		now = time.Now
	}
	lifeCtx := cfg.LifetimeCtx
	if lifeCtx == nil {
		lifeCtx = context.Background()
	}
	reconstruct := cfg.urlReconstruct
	if reconstruct == nil {
		reconstruct = func(scheme, host, path string) string {
			return scheme + "://" + host + path
		}
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
		urlReconstruct:      reconstruct,
		adopter:             cfg.Adopter,
		lifetimeCtx:         lifeCtx,
		adoptionRing:        cfg.AdoptionRing,
		gpgReasonClassifier: cfg.GPGReasonClassifier,
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
}

// repairRequest is what checkLocked emits when the suite is CONFIRMED
// unchanged at upstream (304 or byte-identical 200) and has a current
// snapshot — the SPEC6_7 §3 repair window. Re-adoption only fires on
// "changed", so this is the only signal that can heal a snapshot whose
// adoption skipped integrity-class members; the goroutine in Check
// routes it to Adopter.RepairSkippedMembers, which no-ops cheaply when
// the snapshot has no repairable rows (every healthy suite).
type repairRequest struct {
	suite      SuiteRef
	snapshotID int64
}

// Check runs the SPEC §7.2 algorithm for one suite, synchronously. It
// returns immediately (without contacting upstream) if another goroutine
// holds the in-memory check lock or if the suite is still in cooldown.
//
// On observed change with an Adopter wired in, Check spawns a goroutine
// to run the §7.5 adoption flow. The per-suite mutex is HANDED OFF to
// that goroutine: it serializes the entire adoption against any
// subsequent Check on the same suite, matching SPEC2 §7.5's "the same
// per-suite Mutex from §7.3 guards the entire adoption."
//
// Callers on the request path (T1) should typically invoke Check from a
// goroutine: the request has already been served by the time T1 fires,
// so blocking the request goroutine on a slow upstream gains nothing.
func (c *Checker) Check(ctx context.Context, scheme, host, suitePath string) {
	key := suiteKey(scheme, host, suitePath)
	muVal, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		// Another goroutine is on it; SPEC §7.3 says skip.
		return
	}

	req, repair := c.checkLocked(ctx, scheme, host, suitePath)
	if req == nil || c.adopter == nil {
		// SPEC6_7 §3 + SPEC6_8 §6: an unchanged suite with a current
		// snapshot gets a repair pass (integrity-skip heal) AND a
		// reconcile pass (declared-but-absent requestable IndexTarget
		// heal) in the same goroutine. Both are gated by
		// repairSkippedMembers so a single config switch covers both.
		// The reconcile runs even when there are no repairable skip rows
		// (its own memo short-circuits on a healthy snapshot cheaply).
		//
		// AIDEV-NOTE: this goroutine heals two classes of degradation on
		// the same unchanged-tick signal:
		//   1. RepairSkippedMembers: integrity-class skip rows left by
		//      a stale-mirror adoption (SPEC6_7 §3).
		//   2. ReconcileSnapshot: requestable IndexTargets declared in the
		//      snapshot's InRelease but absent from snapshot_member rows —
		//      e.g. binary-all that was 4xx-skipped during adoption and
		//      was later served by the mirror (SPEC6_8 §6).
		// Mutex handoff (mu.Unlock via defer) and adoptionWg mirror the
		// adoption goroutine above; WaitForAdoptions drains both.
		if c.adopter != nil && c.adopter.repairSkippedMembers {
			snapID, suite, hasSnap := c.currentSnapshotRef(ctx, scheme, host, suitePath)
			if repair != nil || hasSnap {
				c.adoptionWg.Add(1)
				go func() {
					defer mu.Unlock()
					defer c.adoptionWg.Done()
					if repair != nil {
						c.adopter.RepairSkippedMembers(c.lifetimeCtx, repair.suite, repair.snapshotID)
					}
					if hasSnap {
						_, _ = c.adopter.ReconcileSnapshot(c.lifetimeCtx, suite, snapID, false)
					}
				}()
				return
			}
		}
		mu.Unlock()
		return
	}

	// Hand off mutex to the adoption goroutine. The goroutine runs
	// against c.lifetimeCtx (the lifecycle ctx — possibly different
	// from the request ctx that triggered T1) so a request closing
	// before adoption finishes does not abort the adoption.
	c.adoptionWg.Add(1)
	go func() {
		defer mu.Unlock()
		defer c.adoptionWg.Done()
		// SPEC5 §10.4.3 / §9.7.7: time the adoption from goroutine
		// entry, not from inside Run/RunDetached, so the duration
		// reflects what the operator observes (queueing + run).
		start := c.now()
		var err error
		switch req.form {
		case adoptionFormInline:
			err = c.adopter.Run(c.lifetimeCtx, req.suite, req.bytes, req.etag, req.lastmod)
		case adoptionFormDetached:
			err = c.adopter.RunDetached(c.lifetimeCtx, req.suite, req.releaseBytes, req.sigBytes, req.etag, req.lastmod)
		}
		duration := c.now().Sub(start)
		outcome := classifyAdoptionOutcome(err)
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
		}
	}()
}

// WaitForAdoptions blocks until every in-flight adoption goroutine
// spawned via Check has returned. Used by tests for deterministic
// assertions and by graceful shutdown to drain SPEC2 §9.5 step 5
// after cancelling the lifecycle ctx.
func (c *Checker) WaitForAdoptions() { c.adoptionWg.Wait() }

// checkLocked is the body of Check, run with the per-suite mutex held.
// Returns a non-nil *adoptionRequest iff the observed result is "changed"
// AND the caller has an Adopter wired in (Phase 2 path); the caller
// uses this signal to spawn the adoption goroutine.
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
//   - Cooldown gate fails: no DB write, return nil.
//   - Metadata url_path row absent: no DB write (we have nothing to
//     check against), return nil — first request for this suite will
//     land in url_path through the normal miss path.
//   - 304: last_check_at = last_success_at = now.
//   - 200, body hash matches cached: bump last_check_at, last_success_at,
//     and refresh validators (upstream may have rotated etag/lastmod
//     while bytes are unchanged).
//   - 200, body hash differs: bump last_check_at, last_success_at,
//     record inrelease_change_seen_at = now, return *adoptionRequest.
//   - Error (network, 4xx, 5xx, ctx cancel): bump last_check_at only —
//     don't hammer a broken upstream — and log.
//
// SPEC2 wiring: the body-hash comparison uses
// suite_snapshot.inrelease_hash (inline) or .release_hash (detached)
// when the suite has an adopted snapshot. Without this, every
// freshness check after a successful adoption would observe "changed"
// against the stale url_path.blob_hash and thrash the adoption
// candidate-uniqueness constraint forever.
func (c *Checker) checkLocked(ctx context.Context, scheme, host, suitePath string) (*adoptionRequest, *repairRequest) {
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
		return nil, nil
	}

	// Cooldown gate.
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		elapsed := now.Sub(time.Unix(*cur.LastCheckAt, 0))
		if elapsed < c.cooldown {
			return nil, nil
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
		return c.checkLockedDetached(ctx, cur, suite, now, nowUnix)
	}
	req, repair, fellBack := c.checkLockedInline(ctx, cur, suite, now, nowUnix)
	if !fellBack {
		return req, repair
	}
	return c.checkLockedDetached(ctx, cur, suite, now, nowUnix)
}

// detectForm reads the suite's current snapshot (if any) and returns
// the form whose hash columns are populated. First-ever suites
// (cur.CurrentSnapshotID == nil) and snapshots that fail to load
// default to inline; the inline-path fallback handles the boot-strap
// case for detached-only upstreams.
func (c *Checker) detectForm(ctx context.Context, cur *cache.SuiteFreshness) adoptionForm {
	if cur.CurrentSnapshotID == nil {
		return adoptionFormInline
	}
	snap, err := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
	if err != nil || snap == nil {
		return adoptionFormInline
	}
	if snap.ReleaseHash != nil {
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
func (c *Checker) checkLockedInline(ctx context.Context, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest, bool) {
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
	if cur.CurrentSnapshotID != nil {
		snap, serr := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		switch {
		case serr == nil && snap.InReleaseHash != nil:
			cachedHash = *snap.InReleaseHash
		case serr != nil:
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", serr,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
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
	var etag, lastmod string
	if cur.InReleaseETag != nil {
		etag = *cur.InReleaseETag
	}
	if cur.InReleaseLastMod != nil {
		lastmod = *cur.InReleaseLastMod
	}

	// SPEC §9.3: bound concurrent upstream calls to host. Without
	// this, distinct suites on the same host all run Conditional in
	// parallel, each capable of pulling MaxInReleaseBytes into
	// memory — a resource-exhaustion path under a refresh storm or
	// an adversarial allowlisted upstream. Sharing the limiter with
	// the handler's miss path means cache-miss fetches and
	// freshness checks contend for the same per-host budget.
	release, err := c.hostSem.Acquire(ctx, host)
	if err != nil {
		c.logger.Debug("freshness: host limiter acquire aborted",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil, false
	}
	defer release()

	res, err := c.fetcher.Conditional(ctx, target, etag, lastmod, c.maxBody)
	if err != nil {
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

		// SPEC §7.2: bump last_check_at on failure to space out the
		// next attempt. Don't bump last_success_at — it carries the
		// "we know upstream is fine" signal that periodic_refresh
		// uses.
		cur.LastCheckAt = &nowUnix
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
			"result", "failed",
		)
		freshnessCheckTotal.Inc("failed", host)
		return nil, nil, false
	}

	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix

	// SPEC §10: every freshness attempt emits a structured log line. The
	// `result` enum is what operators pivot on — success-with-no-change
	// (the steady state) vs. success-with-change (the interesting one).
	var (
		result string
		req    *adoptionRequest
	)
	switch res.Status {
	case 304:
		// Validators by definition unchanged. If a previous check had
		// observed an upstream-ahead InRelease, clear the diagnostic
		// — upstream is once again serving the cached version (e.g.
		// after a rollback, or after the cached file caught up via
		// some other path). Leaving it set would permanently flag
		// "upstream has newer" even after recovery.
		cur.InReleaseChangeSeenAt = nil
		result = "not_modified"
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		if newHash == cachedHash {
			// Bytes unchanged despite no 304 (upstream didn't honor
			// the conditional GET). SPEC §7.2 says refresh validators
			// — upstream might have rotated an etag while bytes
			// stayed the same.
			if res.ETag != "" {
				v := res.ETag
				cur.InReleaseETag = &v
			}
			if res.LastModified != "" {
				v := res.LastModified
				cur.InReleaseLastMod = &v
			}
			// Recovery: see the 304 branch comment.
			cur.InReleaseChangeSeenAt = nil
			result = "unchanged"
		} else {
			// Upstream has a new InRelease. Record the diagnostic
			// regardless of whether an Adopter is wired — operators
			// monitoring a Phase 1 deployment still see the
			// divergence.
			cur.InReleaseChangeSeenAt = &nowUnix
			c.logger.Info("InRelease changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			result = "changed"
			// Stash the body bytes for the caller to hand off to the
			// adoption goroutine. The body is bounded by maxBody
			// (§7.2 cap, default 4 MiB), so retaining one
			// InRelease's worth of bytes through the goroutine
			// handoff is acceptable.
			req = &adoptionRequest{
				suite:   suite,
				form:    adoptionFormInline,
				bytes:   res.Body,
				etag:    res.ETag,
				lastmod: res.LastModified,
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
		cur.LastSuccessAt = nil // undo the optimistic success bump
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed",
				"err", perr,
			)
		}
		return nil, nil, false
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}

	c.logger.Info("freshness_check",
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "inline",
		"result", result,
		"upstream_status", res.Status,
	)
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
func (c *Checker) checkLockedDetached(ctx context.Context, cur *cache.SuiteFreshness, suite SuiteRef, now time.Time, nowUnix int64) (*adoptionRequest, *repairRequest) {
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
	if cur.CurrentSnapshotID != nil {
		snap, serr := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		switch {
		case serr == nil && snap.ReleaseHash != nil:
			cachedHash = *snap.ReleaseHash
		case serr != nil:
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", serr,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
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
	var etag, lastmod string
	if cur.InReleaseETag != nil {
		etag = *cur.InReleaseETag
	}
	if cur.InReleaseLastMod != nil {
		lastmod = *cur.InReleaseLastMod
	}

	releaseSlot, err := c.hostSem.Acquire(ctx, host)
	if err != nil {
		c.logger.Debug("freshness: host limiter acquire aborted",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil, nil
	}
	defer releaseSlot()

	res, err := c.fetcher.Conditional(ctx, target, etag, lastmod, c.maxBody)
	if err != nil {
		cur.LastCheckAt = &nowUnix
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
			"form", "detached",
			"result", "failed",
		)
		freshnessCheckTotal.Inc("failed", host)
		return nil, nil
	}

	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix

	var (
		result string
		req    *adoptionRequest
	)
	switch res.Status {
	case 304:
		cur.InReleaseChangeSeenAt = nil
		result = "not_modified"
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
			result = "unchanged"
		} else {
			cur.InReleaseChangeSeenAt = &nowUnix
			c.logger.Info("Release changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			result = "changed"

			// Fetch Release.gpg fresh — no validators, since the
			// signature is bound to the (now-changed) Release content.
			gpgTarget := &fetch.Target{
				CanonicalHost: host,
				URL:           releaseGPGURL.UpstreamURL,
			}
			gpgRes, gerr := c.fetcher.Conditional(ctx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
			if gerr != nil || gpgRes.Status != 200 {
				// Release.gpg fetch failed. Don't return an
				// adoptionRequest; record the change, persist, and
				// rely on the next periodic tick to retry. We
				// already bumped LastSuccessAt because the Release
				// fetch succeeded — keep that.
				if gerr != nil {
					c.logger.Warn("freshness: Release.gpg fetch failed; deferring adoption",
						"err", gerr,
						"canonical_host", host,
						"suite_path", suitePath,
					)
				} else {
					c.logger.Warn("freshness: Release.gpg fetch returned non-200; deferring adoption",
						"status", gpgRes.Status,
						"canonical_host", host,
						"suite_path", suitePath,
					)
				}
			} else {
				req = &adoptionRequest{
					suite:        suite,
					form:         adoptionFormDetached,
					releaseBytes: res.Body,
					sigBytes:     gpgRes.Body,
					etag:         res.ETag,
					lastmod:      res.LastModified,
				}
			}
		}
	default:
		c.logger.Error("freshness: unexpected success status",
			"status", res.Status,
			"canonical_host", host,
			"suite_path", suitePath,
			"form", "detached",
		)
		cur.LastSuccessAt = nil
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed",
				"err", perr,
			)
		}
		return nil, nil
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}

	c.logger.Info("freshness_check",
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "detached",
		"result", result,
		"upstream_status", res.Status,
	)
	freshnessCheckTotal.Inc(result, host)
	return req, repairIfUnchanged(result, suite, cur)
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
// (test-seam) policy scheme://host + path. Canonical host is port-less
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
		h = snap.InReleaseHash
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

// currentSnapshotRef returns the current snapshot id and SuiteRef for the
// given suite, performing one GetSuiteFreshness lookup. Returns ok=false
// when the suite has no current snapshot (unadopted, or DB error). Used by
// the unchanged-tick goroutine to gate the SPEC6_8 §6 ReconcileSnapshot
// call without re-fetching the freshness row that checkLocked already read
// (that row is already committed; we just need the id for the reconcile).
func (c *Checker) currentSnapshotRef(ctx context.Context, scheme, host, suitePath string) (snapshotID int64, suite SuiteRef, ok bool) {
	sf, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	if err != nil || sf == nil || sf.CurrentSnapshotID == nil {
		return 0, SuiteRef{}, false
	}
	return *sf.CurrentSnapshotID, SuiteRef{
		CanonicalScheme: scheme,
		CanonicalHost:   host,
		SuitePath:       suitePath,
	}, true
}

// suiteKey is the in-memory lock map key. The pipe separator matches the
// singleflight convention in handler.serveCacheMiss; canonical scheme,
// host, and suite_path never contain a literal pipe (canonicalization
// rejects userinfo and the path is URL-decoded).
func suiteKey(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}
