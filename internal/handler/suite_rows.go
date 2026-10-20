package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// suiteReq is what one request knows about its suite (spec C3), threaded
// from ServeHTTP through the hit, miss and stale paths.
type suiteReq struct {
	key  cache.SuiteKey
	file aptpath.SuiteFile
	// tracked: the suite has a suite_freshness row, as this request last
	// read or wrote it.
	tracked bool
	// tok is this request's flip-barrier registration; ServeHTTP's
	// deferred Done covers a registration made after the suite's row was
	// created (phase C carry-forward).
	tok *flipgate.Token
}

func newSuiteReq(req *proxy.Request) *suiteReq {
	sr := &suiteReq{}
	if req.IsMetadata && req.SuitePath != "" {
		sr.key = suiteKeyOf(req)
		sr.file = aptpath.ClassifySuiteFile(req.SuitePath, req.Path)
	}
	return sr
}

// suiteKeyOf is the key of req's suite.
func suiteKeyOf(req *proxy.Request) cache.SuiteKey {
	return cache.SuiteKey{CanonicalScheme: req.CanonicalScheme, CanonicalHost: req.CanonicalHost, SuitePath: req.SuitePath}
}

// seedRejectTTL, seedRejectMax and seedRejectSweep shape spec C3's
// cap-rejection cache: a suite refused a row is not retried for 10
// minutes (spec C11 constant); the cache holds at most seedRejectMax
// suites and drops expired ones at most once a minute. A refusal while
// every eviction candidate was busy (cache.CreateResult.CandidatesBusy)
// is transient and cached for seedRejectBusyTTL only (final review,
// concurrency M2).
const (
	seedRejectTTL     = 10 * time.Minute
	seedRejectBusyTTL = time.Minute
	seedRejectMax     = 4096
	seedRejectSweep   = time.Minute
)

// seedRejections caches cap rejections per suite (spec C3), so an over-cap
// suite costs no writer operation per request. The key is the suite alone,
// whichever of its files was refused (InRelease, Release, a flat index):
// phase G's flat files share the entry. Monotonic time. At seedRejectMax
// entries, expired ones are dropped; if it is still full it is cleared
// (each suite then costs one more writer operation). While it holds
// entries a timer sweeps it once per seedRejectSweep, so expired refusals
// — and acu_suites_seed_rejected — fall back without traffic (design
// review M10).
type seedRejections struct {
	mu        sync.Mutex
	until     map[cache.SuiteKey]time.Time
	lastSweep time.Time
	// timer is the pending sweep while entries remain; closed (by
	// Handler.Close) stops the sweeps for good.
	timer  *time.Timer
	closed bool
	// onPanic reports a panic recovered in the timer's goroutine.
	onPanic func(v any)
}

// rejected reports whether key was refused a row within seedRejectTTL.
func (s *seedRejections) rejected(key cache.SuiteKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(false)
	t, ok := s.until[key]
	if ok && !time.Now().Before(t) {
		delete(s.until, key)
		ok = false
	}
	seedRejectedSuites.Set(float64(len(s.until)))
	return ok
}

// add caches a rejection of key for ttl (seedRejectTTL, or
// seedRejectBusyTTL for a transient refusal).
func (s *seedRejections) add(key cache.SuiteKey, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until == nil {
		s.until = make(map[cache.SuiteKey]time.Time)
	}
	if len(s.until) >= seedRejectMax {
		s.sweepLocked(true)
		if len(s.until) >= seedRejectMax {
			clear(s.until)
		}
	}
	s.until[key] = time.Now().Add(ttl)
	s.armLocked()
	seedRejectedSuites.Set(float64(len(s.until)))
}

// armLocked schedules the next sweep while entries remain.
func (s *seedRejections) armLocked() {
	if s.timer == nil && !s.closed && len(s.until) > 0 {
		s.timer = time.AfterFunc(seedRejectSweep, s.expire)
	}
}

// expire is the timer's sweep: it drops expired entries, sets the gauge
// and re-arms while entries remain. Deferred minor C-24: the timer's
// goroutine recovers its panics.
func (s *seedRejections) expire() {
	defer func() {
		if v := recover(); v != nil && s.onPanic != nil {
			s.onPanic(v)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timer = nil
	if s.closed {
		return
	}
	s.sweepLocked(true)
	seedRejectedSuites.Set(float64(len(s.until)))
	s.armLocked()
}

// close stops the sweeps (Handler.Close). Idempotent.
func (s *seedRejections) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// len is the number of suites cached as refused (tests).
func (s *seedRejections) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.until)
}

// sweepLocked drops expired entries, at most once per seedRejectSweep
// unless forced.
func (s *seedRejections) sweepLocked(force bool) {
	now := time.Now()
	if !force && now.Sub(s.lastSweep) < seedRejectSweep {
		return
	}
	s.lastSweep = now
	for k, t := range s.until {
		if !now.Before(t) {
			delete(s.until, k)
		}
	}
}

// seedWarnEvery rate-limits suite_seed_rejected: at most one line per
// minute process-wide; suppressed counts the rejections in between.
const seedWarnEvery = time.Minute

// seedWarn is suite_seed_rejected's rate limiter.
type seedWarn struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// suiteCaps is CreateSuite's bounds: freshness.max_suites globally and a
// tenth of it per canonical host (spec C3); 0 is unbounded (tests).
func (h *Handler) suiteCaps() cache.SuiteCaps {
	return cache.SuiteCaps{Global: h.maxSuites, PerHost: h.maxSuites / 10}
}

// suiteSeed is the row a creation inserts (spec C3). Only an
// adoption-disabled miss of <suite>/InRelease stamps the miss as a
// successful check with its validators — today's seed, byte for byte
// (TestServeHTTP_AdoptionDisabledSeedParity); every other creation leaves
// the stamps and validators NULL, so the suite's first check runs at once
// and unconditionally.
func (h *Handler) suiteSeed(sr *suiteReq, miss bool, etag, lastmod string, now int64) *cache.SuiteSeed {
	s := &cache.SuiteSeed{SuiteKey: sr.key, RequestedAt: now}
	if miss && !h.adoptionEnabled && sr.file == aptpath.FileInRelease {
		s.LastCheckAt, s.LastSuccessAt = &now, &now
		if etag != "" {
			s.InReleaseETag = &etag
		}
		if lastmod != "" {
			s.InReleaseLastMod = &lastmod
		}
	}
	return s
}

// noteCreate handles a creation's outcome: a cap rejection is cached,
// counted and logged (rate-limited WARN suite_seed_rejected); an eviction
// is logged at INFO (suite_evicted). A refusal while every eviction
// candidate was busy is transient: cached for seedRejectBusyTTL, and its
// line says it will be retried rather than that the suite will not
// refresh.
func (h *Handler) noteCreate(key cache.SuiteKey, res cache.CreateResult) {
	if res.Evicted != nil {
		h.logger.Info("suite_evicted",
			"canonical_host", res.Evicted.CanonicalHost,
			"suite_path", res.Evicted.SuitePath,
			"for_host", key.CanonicalHost,
			"for_suite", key.SuitePath,
		)
	}
	if !res.Outcome.Rejected() {
		return
	}
	ttl, detail := seedRejectTTL, "suite will not refresh"
	if res.CandidatesBusy {
		ttl, detail = seedRejectBusyTTL, "eviction candidates busy; retried after 1 minute"
	}
	h.rejections.add(key, ttl)
	suiteSeedRejectedTotal.Inc(key.CanonicalHost)
	h.seedWarn.mu.Lock()
	now := time.Now()
	if !h.seedWarn.last.IsZero() && now.Sub(h.seedWarn.last) < seedWarnEvery {
		h.seedWarn.suppressed++
		h.seedWarn.mu.Unlock()
		return
	}
	suppressed := h.seedWarn.suppressed
	h.seedWarn.last, h.seedWarn.suppressed = now, 0
	h.seedWarn.mu.Unlock()
	h.logger.Warn("suite_seed_rejected",
		"canonical_host", key.CanonicalHost,
		"suite_path", key.SuitePath,
		"cap", res.Outcome.String(),
		"max_suites", h.maxSuites,
		"suppressed", suppressed,
		"detail", detail,
	)
}

// readHead reads at most n leading bytes of a pool file.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// lastUse is a url_path row's last use: the newer of its last_requested_at
// and last_fetched_at (0 when neither is set), the stamp a signed-seen
// marker from it carries, as in the startup marker pass.
func lastUse(row *cache.URLPath) int64 {
	var seen int64
	if row.LastRequestedAt != nil {
		seen = *row.LastRequestedAt
	}
	if row.LastFetchedAt != nil && *row.LastFetchedAt > seen {
		seen = *row.LastFetchedAt
	}
	return seen
}

// cachedAnchors reads the suite's signed-anchor url_path rows —
// <suite>/InRelease and <suite>/Release.gpg, but not except, the path this
// request stores itself — and sniffs each blob here, outside the writer,
// for a creation to mark in its transaction (cache.SuiteSeed.Cached,
// ruling PD-5). A missing row is listed without a blob, so one that
// appears before the transaction makes the creation unproven. A row whose
// url_path read or pool file read fails is logged WARN and listed without
// a blob too: if the transaction's re-read finds it holding one, the
// creation is unproven and the backstop retries it after the commit —
// never "not signed, hash unchanged", which would leave a tracked suite's
// signed anchor unmarked (Task 9 review, ruling b).
//
// AIDEV-NOTE: a row creation marks the suite's cached signed anchors in
// the creating transaction, so a tracked suite never serves an unmarked
// signed anchor (spec C3, the hit-path invariant at every committed
// state): markers are written only for suites with rows, and serving an
// existing tracked suite's anchor writes none.
func (h *Handler) cachedAnchors(ctx context.Context, key cache.SuiteKey, except string) []cache.CachedAnchor {
	var out []cache.CachedAnchor
	for _, name := range []string{"/InRelease", "/Release.gpg"} {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		a := cache.CachedAnchor{Path: p}
		row, err := h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
		switch {
		case errors.Is(err, cache.ErrNotFound):
		case err != nil:
			h.logger.Warn("suite cached anchor read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
		case row.BlobHash != nil:
			file := aptpath.ClassifySuiteFile(key.SuitePath, p)
			head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
			if err != nil {
				h.logger.Warn("suite cached anchor read failed",
					"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
				break
			}
			a.BlobHash, a.SeenAt, a.Signed = *row.BlobHash, lastUse(row), file.Sniff(p, head)
		}
		out = append(out, a)
	}
	if h.cachedAnchorsHook != nil {
		h.cachedAnchorsHook()
	}
	return out
}

// backstopTimeout bounds markCachedSignedAnchors where it runs before the
// body — after an unproven creation on the miss path and after onboarding
// (which shares one deadline with its creation, onboardTimeout) — under
// apt's 30 s inactivity timeout (Task 9 review, controller addition d). It
// bounds the database steps; the pool reads (readHead, at most HeadBytes
// of a local file) take no context (deferred minor D-37). On a timeout the
// backstop's reads or its write fail and are logged WARN, and the body is
// served.
const backstopTimeout = 5 * time.Second

// missBodyBudget bounds, from a miss's start, how long it may wait before
// its body (design review M-15): the fetch, the store and a flip's hold of
// the just-created suite (at most flipgate.HoldMax) come first, then the
// backstop of an unproven creation, and apt gives up after 30 s without a
// byte.
const missBodyBudget = 25 * time.Second

// missBackstopDeadline is the miss path's backstop deadline:
// backstopTimeout from now, but never past missBodyBudget from the
// request's start (an expired deadline fails the backstop at once; it is
// logged WARN and the body is served).
func missBackstopDeadline(start time.Time) time.Time {
	d := time.Now().Add(backstopTimeout)
	if limit := start.Add(missBodyBudget); limit.Before(d) {
		return limit
	}
	return d
}

// markCachedSignedAnchors is the backstop for an unproven creation
// (cache.CreateResult.Unproven): a cached anchor row that changed or
// appeared between cachedAnchors' read and the creating transaction — or
// that cachedAnchors could not read — was never sniffed, so it is read,
// sniffed and marked after the commit (SeenAt is its last use, or now for
// a row with neither stamp). Every failure — a url_path read other than
// "no row", a pool file read, the mark — is logged WARN; the startup
// marker pass heals it at the next start. Its callers run it right after
// the late registration, before any early return (miss path) or the
// response (onboarding).
//
// AIDEV-NOTE: this is the one marker write outside a creating or storing
// transaction, and its window is the hit-path invariant's one residual
// (spec C3): an unproven creation's suite is tracked with the anchor
// unmarked from the commit until this runs, and until the next start if
// it fails. backstopHook (tests) runs first, under the bounded context.
func (h *Handler) markCachedSignedAnchors(ctx context.Context, key cache.SuiteKey, except string) {
	if h.backstopHook != nil {
		h.backstopHook(ctx)
	}
	var marks []cache.SignedSeenMark
	for _, name := range []string{"/InRelease", "/Release.gpg"} {
		p := key.SuitePath + name
		if p == except {
			continue
		}
		row, err := h.cache.LookupURL(ctx, key.CanonicalScheme, key.CanonicalHost, p)
		switch {
		case errors.Is(err, cache.ErrNotFound):
			continue
		case err != nil:
			h.logger.Warn("suite signed-seen backstop read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
			continue
		case row.BlobHash == nil:
			continue
		}
		file := aptpath.ClassifySuiteFile(key.SuitePath, p)
		head, err := readHead(h.cache.BlobPath(*row.BlobHash), file.HeadBytes())
		if err != nil {
			h.logger.Warn("suite signed-seen backstop read failed",
				"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath, "path", p)
			continue
		}
		if !file.Sniff(p, head) {
			continue
		}
		seen := lastUse(row)
		if seen <= 0 {
			// A row with neither stamp is marked now, as the creating
			// transaction's marks fall back to the creation's stamp
			// (cache markCreated): never left unmarked (final review,
			// concurrency M6).
			seen = wallclock.Unix()
		}
		marks = append(marks, cache.SignedSeenMark{CanonicalScheme: key.CanonicalScheme,
			CanonicalHost: key.CanonicalHost, Base: aptpath.MarkerBase(key.SuitePath), SeenAt: seen})
	}
	if len(marks) == 0 {
		return
	}
	if err := h.cache.MarkSignedSeenBatch(ctx, marks); err != nil {
		h.logger.Warn("suite signed-seen mark failed",
			"err", err, "canonical_host", key.CanonicalHost, "suite_path", key.SuitePath)
	}
}

// registerLate registers, with the flip barrier, the anchor serve of a
// suite whose row this request created or found created after ServeHTTP's
// anchorStart (phase C carry-forward: the serve that creates a row is
// tracked like any other), and returns its token — nil when there is
// nothing to register. ServeHTTP's deferred Done covers it.
func (h *Handler) registerLate(r *http.Request, req *proxy.Request, sr *suiteReq) *flipgate.Token {
	if sr.tok != nil || !sr.tracked {
		return nil
	}
	sr.tok = h.anchorStart(r, req)
	return sr.tok
}

// onboardTimeout bounds onboarding's writer operation and its backstop
// together (design review M8; deferred minor D-35): the body waits on
// both, and between them on a flip's hold of the just-created suite (at
// most flipgate.HoldMax, 20 s, reachable since eager first adoption), so
// the worst case stays at 25 s, under apt's 30 s inactivity timeout. On a
// timeout the hit serves untracked, as on any onboarding error.
const onboardTimeout = 5 * time.Second

// onboard creates, before the body is served, the row of an untracked
// suite whose cached anchor (or flat index) a url_path hit serves — the
// legacy onboarding of spec C3, for anchors cached before this release or
// while the suite was refused a row: sniff the blob, CreateSuite (marking
// the base in the same transaction for a sniff-passing InRelease in hand
// and for the suite's other cached signed anchors, ruling PD-5), and
// register the serve with the flip barrier right after it. It returns that
// registration's token (nil: nothing registered); after a registration
// the snapshot path may have changed — a flip may have committed, in a
// hold the request waited in or just before it — so the caller always
// re-dispatches through it (FW-4 (c)); the singleflight leader then runs
// the backstop of an unproven creation.
//
// AIDEV-NOTE: concurrent hits on one untracked suite share one creation
// through the handler's singleflight under its own key namespace (design
// review M8: a herd after an upgrade, or when a refusal expires, costs one
// writer operation), and each request then registers its own serve.
func (h *Handler) onboard(r *http.Request, req *proxy.Request, sr *suiteReq, blobHash string) *flipgate.Token {
	if sr.tracked || !sr.file.CreatesRow(h.adoptionEnabled) || h.rejections.rejected(sr.key) {
		return nil
	}
	head, err := readHead(h.cache.BlobPath(blobHash), sr.file.HeadBytes())
	if err != nil || !sr.file.Sniff(req.Path, head) {
		return nil
	}
	key := "onboard|" + sr.key.CanonicalScheme + "|" + sr.key.CanonicalHost + "|" + sr.key.SuitePath
	// One deadline for the creation and the backstop (D-35), under the
	// handler's lifecycle so a graceful shutdown cuts both short (D-34).
	deadline := time.Now().Add(onboardTimeout)
	res, shared, _ := h.sf.Do(key, func() sfResult { return h.runOnboard(r, req, sr, deadline) })
	if !res.suiteTracked {
		return nil
	}
	sr.tracked = true
	t := h.registerLate(r, req, sr)
	if res.unproven && !shared {
		bctx, cancel := context.WithDeadline(h.lifecycleCtx, deadline)
		h.markCachedSignedAnchors(bctx, sr.key, req.Path)
		cancel()
	}
	return t
}

// runOnboard is one onboarding's creation, shared by the concurrent hits
// on the suite: it reports in suiteTracked whether the suite has a row now
// (created, or found), and in unproven whether the creation needs the
// backstop.
func (h *Handler) runOnboard(r *http.Request, req *proxy.Request, sr *suiteReq, deadline time.Time) sfResult {
	now := wallclock.Unix()
	seed := h.suiteSeed(sr, false, "", "", now)
	if sr.file.Marks() {
		seed.SignedSeenAt = now
	}
	ctx, cancel := context.WithDeadline(h.lifecycleCtx, deadline)
	defer cancel()
	seed.Cached = h.cachedAnchors(ctx, sr.key, req.Path)
	res, err := h.cache.CreateSuite(ctx, *seed, h.suiteCaps(), h.suiteGuard)
	if err != nil {
		// Untracked suites need no marker: serving on is safe, and the next
		// hit tries again.
		h.logger.Warn("suite onboarding failed",
			"err", err, "canonical_host", req.CanonicalHost, "suite_path", req.SuitePath)
		return sfResult{err: err}
	}
	h.noteCreate(sr.key, res)
	return sfResult{
		suiteTracked: res.Outcome == cache.SuiteCreated || res.Outcome == cache.SuiteExists,
		unproven:     res.Outcome == cache.SuiteCreated && res.Unproven,
	}
}

// touchAsync updates the url_path row's last_requested_at and
// request_count (touchURL) and records client interest in suites (spec C3
// TouchSuite, coalesced in the cache) without blocking the response: the
// caller runs it as a goroutine. A fresh context keeps a disconnected
// client from orphaning the writes. Deferred minor C-24: it recovers its
// panics.
func (h *Handler) touchAsync(req *proxy.Request, touchURL bool, suites []cache.SuiteKey) {
	defer func() {
		if v := recover(); v != nil {
			h.logGoroutinePanic("handler_goroutine_panicked", &touchPanicStackLogged, v,
				"where", "touch", "canonical_host", req.CanonicalHost, "path", req.Path)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if touchURL {
		if err := h.cache.TouchURLPath(ctx, req.CanonicalScheme, req.CanonicalHost, req.Path); err != nil {
			h.logger.Debug("touch failed", "err", err, "canonical_host", req.CanonicalHost, "path", req.Path)
		}
	}
	for _, k := range suites {
		if err := h.cache.TouchSuite(ctx, k); err != nil {
			h.logger.Debug("suite touch failed", "err", err, "canonical_host", k.CanonicalHost, "suite_path", k.SuitePath)
		}
	}
}

// touchPanicStackLogged limits the touch goroutine's panic stack to one
// per process.
var touchPanicStackLogged atomic.Bool

// servedSuite returns the suite a served suite file records interest in
// (spec C3): the request's own suite when it is tracked and the response
// status is below 400 (304, HEAD and stale serves included).
func servedSuite(sr *suiteReq, status int) []cache.SuiteKey {
	if !sr.file.Touches() || !sr.tracked || status >= http.StatusBadRequest {
		return nil
	}
	return []cache.SuiteKey{sr.key}
}

// vouchingSuites returns the distinct suite paths of the rows that declare
// hash (the suites a validated .deb serve keeps alive).
func vouchingSuites(rows []cache.DeclaredHash, hash string) []string {
	var out []string
	for _, r := range rows {
		if r.DeclaredSHA256 == hash && !slices.Contains(out, r.SuitePath) {
			out = append(out, r.SuitePath)
		}
	}
	return out
}

// vouchedSuites returns the suites whose snapshots vouched a served .deb
// (spec C3: install-only clients keep their suite alive).
func vouchedSuites(req *proxy.Request, v packageValidation, status int) []cache.SuiteKey {
	if !v.Validated || status >= http.StatusBadRequest {
		return nil
	}
	out := make([]cache.SuiteKey, 0, len(v.Suites))
	for _, s := range v.Suites {
		out = append(out, cache.SuiteKey{CanonicalScheme: req.CanonicalScheme, CanonicalHost: req.CanonicalHost, SuitePath: s})
	}
	return out
}
