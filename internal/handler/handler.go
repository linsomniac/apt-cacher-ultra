// Package handler is the http.Handler that wires proxy + fetch + cache.
//
// SPEC §6.1 (cache hit fast path) and §6.2 (singleflight miss path) live
// here. Every other internal package — proxy for canonicalization, cache
// for storage, fetch for upstream I/O — is composed at this layer into
// the request behavior an apt client sees.
package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Compile-time assertion: *cache.BlobWriter satisfies fetch.FetchDst.
// The handler relies on this — runFetch hands the BlobWriter directly
// to fetch.Fetch as the destination — so an interface drift in either
// package surfaces here at build time rather than as a runtime panic.
var _ fetch.FetchDst = (*cache.BlobWriter)(nil)

// FreshnessChecker is the subset of *freshness.Checker the handler uses
// to fire SPEC §7.1 T1 triggers. Defined as an interface so handler
// tests can supply a recorder without spinning up the real freshness
// package.
type FreshnessChecker interface {
	Check(ctx context.Context, scheme, host, suitePath string)
}

// Config carries handler dependencies. All non-pointer fields are
// optional and defaulted in New.
type Config struct {
	Parser *proxy.Parser
	Cache  *cache.Cache
	Fetch  *fetch.Client
	Logger *slog.Logger

	// HostLimiter bounds concurrent upstream connections to a single
	// canonical host. SPEC §9.3. Required; New returns an error on a
	// nil value. Sharing one limiter with the freshness checker
	// keeps both code paths under the same per-host budget.
	HostLimiter *hostsem.Sem

	// Freshness is the SPEC §7 checker. Optional: when nil, T1
	// triggers are disabled (tests, or operators who explicitly
	// disabled freshness in config).
	Freshness FreshnessChecker
}

// Handler is the apt-cacher-ultra http.Handler. Construct via New.
//
// Close drains in-flight fetches at shutdown — see SPEC §9.5 step 3. The
// handler keeps a lifecycle ctx (lifecycleCtx) that miss fetches are
// rooted at instead of the request ctx, so a leader's client disconnect
// does not abort an in-flight fetch that other waiters are still
// blocked on. Close cancels that lifecycle ctx and waits on activeWG
// for currently-running ServeHTTP invocations to complete.
type Handler struct {
	parser    *proxy.Parser
	cache     *cache.Cache
	fetch     *fetch.Client
	sf        *sfGroup
	sem       *hostsem.Sem
	freshness FreshnessChecker
	logger    *slog.Logger

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	activeWG        sync.WaitGroup
}

// New constructs a Handler from validated dependencies. Returns an error
// if any required dependency is nil.
func New(cfg Config) (*Handler, error) {
	if cfg.Parser == nil {
		return nil, errors.New("handler: nil Parser")
	}
	if cfg.Cache == nil {
		return nil, errors.New("handler: nil Cache")
	}
	if cfg.Fetch == nil {
		return nil, errors.New("handler: nil Fetch")
	}
	if cfg.HostLimiter == nil {
		return nil, errors.New("handler: nil HostLimiter")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &Handler{
		parser:          cfg.Parser,
		cache:           cfg.Cache,
		fetch:           cfg.Fetch,
		sf:              newSFGroup(),
		sem:             cfg.HostLimiter,
		freshness:       cfg.Freshness,
		logger:          logger,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}, nil
}

// Close implements SPEC §9.5 step 3: cancel any in-flight upstream
// fetches and wait for active ServeHTTP invocations to return. Safe to
// call multiple times; lifecycleCancel is idempotent and Wait is too.
//
// Contract: Close MUST be called only after the embedding *http.Server
// has been Shutdown (or otherwise stopped accepting new requests).
// Otherwise activeWG.Add(1) at the top of ServeHTTP can race
// activeWG.Wait() here, which is undefined behavior. Calling Close
// after Server.Shutdown returns guarantees no new ServeHTTP starts.
func (h *Handler) Close() {
	h.lifecycleCancel()
	h.activeWG.Wait()
}

// X-Cache outcome strings written to the response. SPEC §2.7.
const (
	cacheHit         = "HIT"
	cacheMiss        = "MISS"
	cacheHitStale    = "HIT-STALE"
	cacheCoalesced   = "HIT-COALESCED"
	hdrXCache        = "X-Cache"
	hdrXCacheAge     = "X-Cache-Age"
	hdrXUpstreamStat = "X-Upstream-Status"
)

// ServeHTTP routes one apt request through the cache.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// SPEC §9.5: track this invocation so Close() can wait for the
	// drain before main returns and the cache is torn down. Add
	// happens at entry so by the time Server.Shutdown returns there
	// is no goroutine that could still call Add later.
	h.activeWG.Add(1)
	defer h.activeWG.Done()

	start := time.Now()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		h.logRequest(r, "", "", "method_not_allowed", http.StatusMethodNotAllowed, 0, start)
		return
	}

	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.logRequest(r, "", "", "bad_request", http.StatusBadRequest, 0, start)
		return
	}

	// Fast path: SPEC §6.1.
	if served, status, body := h.tryCacheHit(w, r, req); served {
		h.logRequest(r, req.CanonicalHost, req.Path, "hit", status, body, start)
		return
	}

	// SPEC §6.6 short-circuit: reject disallowed hosts before allocating
	// per-host bookkeeping (singleflight entry, semaphore slot). The
	// fetch layer would also reject this host once we got there, but
	// without this pre-check an attacker could send requests for many
	// distinct disallowed hostnames and grow handler-side maps before
	// the fetch path's allowlist fires.
	if !h.fetch.HostAllowed(req.CanonicalHost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		h.logRequest(r, req.CanonicalHost, req.Path, "forbidden", http.StatusForbidden, 0, start)
		return
	}

	// Miss: SPEC §6.2.
	h.serveCacheMiss(w, r, req, start)
}

// tryCacheHit attempts to serve from disk. Returns served=true if the
// response was sent (success or 5xx during streaming). status and
// bytesWritten are best-effort for logging — http.ServeContent does the
// real header writes.
func (h *Handler) tryCacheHit(w http.ResponseWriter, r *http.Request, req *proxy.Request) (served bool, status int, bytesWritten int64) {
	row, err := h.cache.LookupURL(r.Context(), req.CanonicalScheme, req.CanonicalHost, req.Path)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return false, 0, 0
	case err != nil:
		h.logger.Warn("cache lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		return false, 0, 0
	}
	if row.BlobHash == nil || *row.BlobHash == "" {
		return false, 0, 0
	}
	exists, err := h.cache.BlobExists(*row.BlobHash)
	if err != nil || !exists {
		// Row points at a blob that's no longer on disk (manual delete,
		// staging mishap). Drop into the miss path so we re-fetch.
		if err != nil {
			h.logger.Warn("blob existence check failed",
				"err", err,
				"hash", *row.BlobHash,
			)
		}
		return false, 0, 0
	}

	hash := *row.BlobHash
	path := h.cache.BlobPath(hash)
	f, err := os.Open(path)
	if err != nil {
		h.logger.Warn("blob open failed", "err", err, "hash", hash)
		return false, 0, 0
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		h.logger.Warn("blob stat failed", "err", err, "hash", hash)
		return false, 0, 0
	}

	w.Header().Set(hdrXCache, cacheHit)
	if row.LastFetchedAt != nil {
		age := time.Now().Unix() - *row.LastFetchedAt
		if age < 0 {
			age = 0
		}
		w.Header().Set(hdrXCacheAge, strconv.FormatInt(age, 10))
	}

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)

	go h.touchAsync(req)
	h.maybeFireFreshness(req)
	return true, cw.statusCode(), cw.bytes
}

// maybeFireFreshness fires the SPEC §7.1 T1 trigger after a metadata
// cache hit. Runs the check off the request goroutine — it has its own
// in-memory TryLock + cooldown gate, so spawning unconditionally is
// safe, but the request has already been served and there is no value
// in blocking the response goroutine on a slow upstream conditional GET.
//
// The goroutine registers with activeWG so Handler.Close drains it on
// shutdown; lifecycleCtx is what the goroutine carries, so cancel
// propagates through fetch.Conditional.
func (h *Handler) maybeFireFreshness(req *proxy.Request) {
	if h.freshness == nil || !req.IsMetadata || req.SuitePath == "" {
		return
	}
	// Increment must happen here (synchronously, before the goroutine
	// is spawned) so Handler.Close — which is contracted to run after
	// Server.Shutdown stops new ServeHTTP — never observes a counter
	// of zero while a freshness check is still in flight.
	h.activeWG.Add(1)
	go func() {
		defer h.activeWG.Done()
		h.freshness.Check(h.lifecycleCtx, req.CanonicalScheme, req.CanonicalHost, req.SuitePath)
	}()
}

// touchAsync updates last_requested_at + request_count without blocking
// the response. Uses a fresh ctx so an already-disconnected client does
// not orphan the write before the writer goroutine picks it up.
func (h *Handler) touchAsync(req *proxy.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.cache.TouchURLPath(ctx, req.CanonicalScheme, req.CanonicalHost, req.Path); err != nil {
		h.logger.Debug("touch failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
	}
}

// serveCacheMiss runs the singleflight fetch and serves the cached file
// (or an error response) afterward.
func (h *Handler) serveCacheMiss(w http.ResponseWriter, r *http.Request, req *proxy.Request, start time.Time) {
	key := req.CanonicalScheme + "|" + req.CanonicalHost + "|" + req.Path

	res, shared := h.sf.Do(key, func() sfResult {
		// Use the handler's lifecycle ctx, not the request ctx. Two
		// goals: (1) a leader who disconnects must not kill the fetch
		// for waiters that are still connected, and (2) on graceful
		// shutdown (SPEC §9.5 step 3) the lifecycle ctx is cancelled,
		// which lets fetch return promptly instead of riding out
		// fetch.TotalTimeout. Without this, a hung upstream would
		// keep the cache from closing for several minutes after the
		// drain budget elapses.
		return h.runFetch(h.lifecycleCtx, req)
	})

	if res.err != nil {
		h.respondError(w, r, req, res, start)
		return
	}

	hash := res.blobHash
	path := h.cache.BlobPath(hash)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		h.logRequest(r, req.CanonicalHost, req.Path, "error", http.StatusInternalServerError, 0, start)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "cache stat failed", http.StatusInternalServerError)
		h.logRequest(r, req.CanonicalHost, req.Path, "error", http.StatusInternalServerError, 0, start)
		return
	}

	outcome := cacheMiss
	logOutcome := "miss"
	if shared {
		outcome = cacheCoalesced
		logOutcome = "hit_coalesced"
	}
	w.Header().Set(hdrXCache, outcome)
	w.Header().Set(hdrXCacheAge, "0")
	if res.status > 0 {
		w.Header().Set(hdrXUpstreamStat, strconv.Itoa(res.status))
	}

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)
	h.logRequest(r, req.CanonicalHost, req.Path, logOutcome, cw.statusCode(), cw.bytes, start)
}

// runFetch is the body of the singleflight call. Acquires the per-host
// semaphore, opens a temp blob, fetches into it, finalizes into pool/,
// and inserts the url_path/blob rows. Returns sfResult with the cached
// blob hash on success.
func (h *Handler) runFetch(ctx context.Context, req *proxy.Request) sfResult {
	release, err := h.sem.Acquire(ctx, req.CanonicalHost)
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: acquire host slot: %w", err)}
	}
	defer release()

	bw, err := h.cache.NewTempBlob()
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: open temp blob: %w", err)}
	}

	upstreamURL := req.UpstreamURL
	target := &fetch.Target{
		CanonicalHost: req.CanonicalHost,
		URL:           upstreamURL,
	}
	fres, ferr := h.fetch.Fetch(ctx, target, bw)
	if ferr != nil {
		_ = bw.Abort()
		status := 0
		var se *fetch.StatusError
		if errors.As(ferr, &se) {
			status = se.Code
		}
		return sfResult{err: ferr, status: status}
	}

	hash, err := bw.Finalize(fres.ContentLength)
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: finalize blob: %w", err), status: fres.Status}
	}

	// Persist blob + url_path with a small budget. ctx here is the
	// handler lifecycle ctx (see serveCacheMiss), so a leader's client
	// disconnect does not propagate — but a shutdown cancel does, which
	// is intentional: if the cache is closing we'd rather abandon the
	// row (leaving an orphan blob in pool/, recoverable on the next
	// fetch) than ride out the 30s budget past the SPEC §9.5 drain.
	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := h.cache.PutBlob(dbCtx, hash, fres.ContentLength); err != nil {
		return sfResult{err: fmt.Errorf("handler: put blob row: %w", err), status: fres.Status}
	}

	now := time.Now().Unix()
	row := cache.URLPath{
		CanonicalScheme: req.CanonicalScheme,
		CanonicalHost:   req.CanonicalHost,
		Path:            req.Path,
		BlobHash:        &hash,
		UpstreamURL:     upstreamURL,
		IsMetadata:      req.IsMetadata,
		LastRequestedAt: &now,
		RequestCount:    1,
		LastFetchedAt:   &now,
	}
	if fres.ETag != "" {
		etag := fres.ETag
		row.UpstreamETag = &etag
	}
	if fres.LastModified != "" {
		lm := fres.LastModified
		row.UpstreamLastMod = &lm
	}
	if err := h.cache.PutURLPath(dbCtx, row); err != nil {
		return sfResult{err: fmt.Errorf("handler: put url row: %w", err), status: fres.Status}
	}

	// Seed suite_freshness on a successful InRelease miss. Without
	// this, a freshly-cached suite is invisible to the periodic
	// scheduler (which scans suite_freshness, not url_path) until the
	// first cache-hit T1 fires — and that first T1 has no validators,
	// so it does an unconditional GET when a 304 was achievable.
	// Seed failures are non-fatal (the file IS cached); we just lose
	// the periodic-scheduler benefit until the next miss/T1.
	if req.IsMetadata && req.SuitePath != "" && req.Path == req.SuitePath+inReleaseSuffix {
		seed := cache.SuiteFreshness{
			CanonicalScheme: req.CanonicalScheme,
			CanonicalHost:   req.CanonicalHost,
			SuitePath:       req.SuitePath,
			LastCheckAt:     &now,
			LastSuccessAt:   &now,
		}
		if fres.ETag != "" {
			etag := fres.ETag
			seed.InReleaseETag = &etag
		}
		if fres.LastModified != "" {
			lm := fres.LastModified
			seed.InReleaseLastMod = &lm
		}
		if err := h.cache.PutSuiteFreshness(dbCtx, seed); err != nil {
			h.logger.Warn("seed suite_freshness failed",
				"err", err,
				"canonical_host", req.CanonicalHost,
				"suite_path", req.SuitePath,
			)
		}
	}

	return sfResult{
		blobHash: hash,
		size:     fres.ContentLength,
		status:   fres.Status,
	}
}

// inReleaseSuffix is the path suffix that identifies the InRelease
// file under a suite path. Kept as a package constant so the same
// literal is used for the seed-detection check above and (in the
// freshness package) the check itself.
const inReleaseSuffix = "/InRelease"

// respondError maps a fetch error to an HTTP response.
//
// SPEC §6.6: allowlist + deny CIDR rejections → 403.
// SPEC §6.4: upstream unreachable on a miss → 502 + Retry-After.
// Upstream 4xx (e.g. apt probing for an index variant that does not
// exist) → passthrough. Any other failure → 502.
func (h *Handler) respondError(w http.ResponseWriter, r *http.Request, req *proxy.Request, res sfResult, start time.Time) {
	err := res.err
	if res.status > 0 {
		w.Header().Set(hdrXUpstreamStat, strconv.Itoa(res.status))
	}

	switch {
	case errors.Is(err, fetch.ErrHostNotAllowed), errors.Is(err, fetch.ErrTargetDenied):
		http.Error(w, "forbidden", http.StatusForbidden)
		h.logRequest(r, req.CanonicalHost, req.Path, "forbidden", http.StatusForbidden, 0, start)
		return
	case errors.Is(err, fetch.ErrUpstreamStatus):
		// SPEC §6.4 / failure-mode catalog: only upstream 4xx is a
		// "client said no" we can pass through (e.g. apt probing for
		// an index variant the archive does not publish — 404 must
		// reach the client so apt moves on). Anything else is treated
		// as cache-side unhealthy: a 3xx that escaped the redirect
		// guard, an unexpected 2xx classified as non-success by fetch,
		// or status==0 if classification raced with a parse failure.
		// All become 502.
		if res.status >= 400 && res.status < 500 {
			http.Error(w, fmt.Sprintf("upstream status %d", res.status), res.status)
			h.logRequest(r, req.CanonicalHost, req.Path, "upstream_status", res.status, 0, start)
			return
		}
		w.Header().Set("Retry-After", "60")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logger.Warn("non-4xx upstream status mapped to 502",
			"upstream_status", res.status,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, start)
		return
	case errors.Is(err, fetch.ErrUpstreamUnavailable),
		errors.Is(err, fetch.ErrInvalidURL),
		errors.Is(err, fetch.ErrRedirectBlocked),
		errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "60")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, start)
		return
	case errors.Is(err, context.Canceled):
		// Client almost certainly disconnected. 499 is non-standard;
		// 503 with Retry-After is the closest sensible response.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		h.logRequest(r, req.CanonicalHost, req.Path, "client_canceled", http.StatusServiceUnavailable, 0, start)
		return
	default:
		w.Header().Set("Retry-After", "60")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logger.Error("unclassified fetch error",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, start)
		return
	}
}

// logRequest emits the per-request slog line. SPEC §10.
//
// AIDEV-NOTE: never log r.RequestURI directly — proxy-form requests can
// (in principle) carry userinfo like http://user:pass@host/path. The
// parser rejects userinfo before it reaches the success path, but the
// 400/405 log calls fire before the parser has run, so we route the
// URL through urlForLog which strips userinfo unconditionally.
func (h *Handler) logRequest(r *http.Request, canonHost, path, outcome string, status int, bytesWritten int64, start time.Time) {
	h.logger.Info("request",
		"method", r.Method,
		"url", urlForLog(r),
		"canonical_host", canonHost,
		"path", path,
		"outcome", outcome,
		"status", status,
		"bytes_sent", bytesWritten,
		"duration_ms", time.Since(start).Milliseconds(),
		"client_addr", r.RemoteAddr,
	)
}

// urlForLog returns a sanitized representation of the request URL
// suitable for inclusion in a log line. Userinfo (which Go's
// http.Server faithfully parses out of an absolute-form request line)
// is stripped — never leak credentials into operator-readable output.
func urlForLog(r *http.Request) string {
	if r.URL == nil {
		// Defensive: net/http always populates r.URL, but if a future
		// caller hands us a hand-built request without one, fall back
		// to the literal request line. This path is also reached only
		// when r.URL is nil, so userinfo cannot be present here.
		return r.RequestURI
	}
	if r.URL.User == nil {
		return r.URL.String()
	}
	cp := *r.URL
	cp.User = nil
	return cp.String()
}

// countingWriter wraps an http.ResponseWriter to track the response
// status code and total body bytes for log lines.
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

// statusCode returns the actual response code, defaulting to 200 if the
// handler never explicitly wrote a header (the io.Writer code path).
func (c *countingWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

