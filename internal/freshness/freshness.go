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

	// now is a test seam; production uses time.Now.
	now func() time.Time
}

// Checker is the SPEC §7 freshness state machine.
type Checker struct {
	cache    Cache
	fetcher  Fetcher
	hostSem  *hostsem.Sem
	cooldown time.Duration
	refresh  time.Duration
	maxBody  int64
	logger   *slog.Logger
	now      func() time.Time

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
	return &Checker{
		cache:    cfg.Cache,
		fetcher:  cfg.Fetcher,
		hostSem:  cfg.HostLimiter,
		cooldown: cfg.Cooldown,
		refresh:  cfg.Refresh,
		maxBody:  maxBody,
		logger:   logger,
		now:      now,
	}, nil
}

// Check runs the SPEC §7.2 algorithm for one suite, synchronously. It
// returns immediately (without contacting upstream) if another goroutine
// holds the in-memory check lock or if the suite is still in cooldown.
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
	defer mu.Unlock()

	c.checkLocked(ctx, scheme, host, suitePath)
}

// checkLocked is the body of Check, run with the per-suite mutex held.
//
// Outcomes (all paths must update suite_freshness so cooldown applies
// to the next attempt):
//
//   - Cooldown gate fails: no DB write, return.
//   - InRelease url_path row absent: no DB write (we have nothing to
//     check against), return — first request for this suite will land
//     in url_path through the normal miss path.
//   - 304: last_check_at = last_success_at = now.
//   - 200, body hash matches cached: bump last_check_at, last_success_at,
//     and refresh validators (upstream may have rotated etag/lastmod
//     while bytes are unchanged).
//   - 200, body hash differs: bump last_check_at, last_success_at,
//     record inrelease_change_seen_at = now. Phase 1 does NOT adopt;
//     Phase 2 atomic-flip transaction will.
//   - Error (network, 4xx, 5xx, ctx cancel): bump last_check_at only —
//     don't hammer a broken upstream — and log.
func (c *Checker) checkLocked(ctx context.Context, scheme, host, suitePath string) {
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

	// Cooldown gate.
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		elapsed := now.Sub(time.Unix(*cur.LastCheckAt, 0))
		if elapsed < c.cooldown {
			return
		}
	}

	// Locate the InRelease url_path row to get the upstream URL plus
	// the cached blob hash for byte-equality comparison on 200.
	inReleasePath := suitePath + inReleaseFilename
	urlRow, err := c.cache.LookupURL(ctx, scheme, host, inReleasePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has freshness state but no cached InRelease. Either we
		// hit T1 from a non-InRelease metadata file before InRelease
		// was ever fetched, or the row was administratively cleared.
		// Either way, nothing to validate against — skip without
		// writing.
		c.logger.Debug("freshness: no cached InRelease, skipping",
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
	case err != nil:
		c.logger.Warn("freshness: lookup InRelease url_path failed",
			"err", err,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
	}
	if urlRow.BlobHash == nil || *urlRow.BlobHash == "" {
		c.logger.Debug("freshness: InRelease url_path has no blob, skipping",
			"canonical_host", host,
			"suite_path", suitePath,
		)
		return
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
		return
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
		)
		return
	}

	cur.LastCheckAt = &nowUnix
	cur.LastSuccessAt = &nowUnix

	switch res.Status {
	case 304:
		// Validators by definition unchanged. If a previous check had
		// observed an upstream-ahead InRelease, clear the diagnostic
		// — upstream is once again serving the cached version (e.g.
		// after a rollback, or after the cached file caught up via
		// some other path). Leaving it set would permanently flag
		// "upstream has newer" even after recovery.
		cur.InReleaseChangeSeenAt = nil
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		if newHash == *urlRow.BlobHash {
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
		} else {
			// Upstream has a new InRelease. Phase 1 does NOT adopt.
			// Record observation; cache keeps serving the consistent
			// older set.
			cur.InReleaseChangeSeenAt = &nowUnix
			c.logger.Info("InRelease changed at upstream; awaiting Phase 2 atomic flip",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", *urlRow.BlobHash,
				"upstream_hash", newHash,
			)
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
		return
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
}

// suiteKey is the in-memory lock map key. The pipe separator matches the
// singleflight convention in handler.serveCacheMiss; canonical scheme,
// host, and suite_path never contain a literal pipe (canonicalization
// rejects userinfo and the path is URL-decoded).
func suiteKey(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}
