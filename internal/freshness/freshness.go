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
)

// inReleaseFilename is the file that anchors a suite. Every freshness
// check is a conditional GET on <suite_path>/InRelease.
const inReleaseFilename = "/InRelease"

// defaultMaxInReleaseBytes caps the body we read on a 200 response to
// the conditional GET. Real-world InRelease files are tens of KB; 4 MiB
// is comfortable headroom and still small enough that a hostile (but
// allowlisted) upstream cannot exhaust memory through the freshness
// path. Operators can override via Config.MaxInReleaseBytes.
const defaultMaxInReleaseBytes int64 = 4 << 20

// Cache is the subset of *cache.Cache the freshness checker uses.
// Defined as an interface so tests can supply a fake without standing
// up an on-disk cache.
type Cache interface {
	GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*cache.SuiteFreshness, error)
	PutSuiteFreshness(ctx context.Context, s cache.SuiteFreshness) error
	ListSuites(ctx context.Context) ([]cache.SuiteFreshness, error)
	LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error)
	GetSuiteSnapshot(ctx context.Context, snapshotID int64) (*cache.SuiteSnapshot, error)
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

	// now is a test seam; production uses time.Now.
	now func() time.Time
}

// Checker is the SPEC §7 freshness state machine.
type Checker struct {
	cache       Cache
	fetcher     Fetcher
	hostSem     *hostsem.Sem
	cooldown    time.Duration
	refresh     time.Duration
	maxBody     int64
	logger      *slog.Logger
	now         func() time.Time
	adopter     *Adopter
	lifetimeCtx context.Context

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
	return &Checker{
		cache:       cfg.Cache,
		fetcher:     cfg.Fetcher,
		hostSem:     cfg.HostLimiter,
		cooldown:    cfg.Cooldown,
		refresh:     cfg.Refresh,
		maxBody:     maxBody,
		logger:      logger,
		now:         now,
		adopter:     cfg.Adopter,
		lifetimeCtx: lifeCtx,
	}, nil
}

// adoptionRequest is what checkLocked emits when a Phase 2 atomic
// flip should be triggered for the observed-changed InRelease.
type adoptionRequest struct {
	suite   SuiteRef
	bytes   []byte // verified-input candidate (clearsigned InRelease)
	etag    string
	lastmod string
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

	req := c.checkLocked(ctx, scheme, host, suitePath)
	if req == nil || c.adopter == nil {
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
		if err := c.adopter.Run(c.lifetimeCtx, req.suite, req.bytes, req.etag, req.lastmod); err != nil {
			// Several Adopter.Run paths emit categorized log lines
			// before returning (adoption_gpg_failed,
			// adoption_parse_failed, adoption_member_mismatch,
			// pool_corruption_during_adoption). Others — content-
			// length mismatch, fetch transport errors, DB failures
			// the categorized line didn't already cover — propagate
			// only as the wrapped sentinel. Without a backstop log,
			// those drop on the floor and the operator sees the
			// "InRelease changed at upstream" line followed by
			// silence. Always surface a single line so any failure
			// is grep-able.
			c.logger.Warn("adoption_run_failed",
				"canonical_host", req.suite.CanonicalHost,
				"suite_path", req.suite.SuitePath,
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
// Outcomes (all paths must update suite_freshness so cooldown applies
// to the next attempt):
//
//   - Cooldown gate fails: no DB write, return nil.
//   - InRelease url_path row absent: no DB write (we have nothing to
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
// SPEC2 wiring: the body-hash comparison uses suite_snapshot.inrelease_hash
// when the suite has an adopted snapshot (CurrentSnapshotID set). Without
// this, every freshness check after a successful adoption would observe
// "changed" against the stale url_path.blob_hash and thrash the
// adoption candidate-uniqueness constraint forever.
func (c *Checker) checkLocked(ctx context.Context, scheme, host, suitePath string) *adoptionRequest {
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
		return nil
	}

	// Cooldown gate.
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		elapsed := now.Sub(time.Unix(*cur.LastCheckAt, 0))
		if elapsed < c.cooldown {
			return nil
		}
	}

	// Locate the InRelease url_path row to get the upstream URL plus
	// (Phase 1 fallback) the cached blob hash for byte-equality
	// comparison on 200.
	inReleasePath := suitePath + inReleaseFilename
	urlRow, err := c.cache.LookupURL(ctx, scheme, host, inReleasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has freshness state but no cached InRelease. Either we
		// hit T1 from a non-InRelease metadata file before InRelease
		// was ever fetched, or the row was administratively cleared.
		// Either way, nothing to validate against — skip without
		// writing.
		c.logger.Debug("freshness: no cached InRelease url_path, skipping",
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil
	case err != nil:
		c.logger.Warn("freshness: lookup InRelease url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return nil
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
			return nil
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
		return nil
	}
	defer release()

	res, err := c.fetcher.Conditional(ctx, target, etag, lastmod, c.maxBody)
	if err != nil {
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
		return nil
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
				suite: SuiteRef{
					CanonicalScheme: scheme,
					CanonicalHost:   host,
					SuitePath:       suitePath,
				},
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
		return nil
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
		"result", result,
		"upstream_status", res.Status,
	)
	return req
}

// suiteKey is the in-memory lock map key. The pipe separator matches the
// singleflight convention in handler.serveCacheMiss; canonical scheme,
// host, and suite_path never contain a literal pipe (canonicalization
// rejects userinfo and the path is URL-decoded).
func suiteKey(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}
