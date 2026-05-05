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
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Compile-time assertion: *cache.BlobWriter satisfies fetch.FetchDst.
// The handler relies on this — runFetch hands the BlobWriter directly
// to fetch.Fetch as the destination — so an interface drift in either
// package surfaces here at build time rather than as a runtime panic.
var _ fetch.FetchDst = (*cache.BlobWriter)(nil)

// Config carries handler dependencies. All non-pointer fields are
// optional and defaulted in New.
type Config struct {
	Parser               *proxy.Parser
	Cache                *cache.Cache
	Fetch                *fetch.Client
	MaxConcurrentPerHost int
	Logger               *slog.Logger
}

// Handler is the apt-cacher-ultra http.Handler. Construct via New.
type Handler struct {
	parser *proxy.Parser
	cache  *cache.Cache
	fetch  *fetch.Client
	sf     *sfGroup
	sem    *hostSem
	logger *slog.Logger
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
	limit := cfg.MaxConcurrentPerHost
	if limit <= 0 {
		limit = 8
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		parser: cfg.Parser,
		cache:  cfg.Cache,
		fetch:  cfg.Fetch,
		sf:     newSFGroup(),
		sem:    newHostSem(limit),
		logger: logger,
	}, nil
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
	return true, cw.statusCode(), cw.bytes
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
		// Detach the leader's request ctx: a leader who disconnects must
		// not kill the fetch for the still-connected waiters. fetch
		// applies its own TotalTimeout so the work stays bounded.
		ctx := context.WithoutCancel(r.Context())
		return h.runFetch(ctx, req)
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
	release, err := h.sem.acquire(ctx, req.CanonicalHost)
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

	// Persist blob + url_path. Use a detached ctx with a small budget so
	// these survive the leader's request lifecycle (e.g. client hung up
	// after fetch completed).
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
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

	return sfResult{
		blobHash: hash,
		size:     fres.ContentLength,
		status:   fres.Status,
	}
}

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
		// Use Errorf to inject a body; the upstream's body is not
		// proxied through (Phase 1 simplification).
		http.Error(w, fmt.Sprintf("upstream status %d", res.status), res.status)
		h.logRequest(r, req.CanonicalHost, req.Path, "upstream_status", res.status, 0, start)
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
func (h *Handler) logRequest(r *http.Request, canonHost, path, outcome string, status int, bytesWritten int64, start time.Time) {
	h.logger.Info("request",
		"method", r.Method,
		"url", r.RequestURI,
		"canonical_host", canonHost,
		"path", path,
		"outcome", outcome,
		"status", status,
		"bytes_sent", bytesWritten,
		"duration_ms", time.Since(start).Milliseconds(),
		"client_addr", r.RemoteAddr,
	)
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

