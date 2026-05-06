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
	"strings"
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
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

	// Serve is the SPEC §6.4 / §8 stale-serve policy. Zero value is
	// safe (both flags off): the handler will always 502 on a metadata
	// miss with upstream down rather than serving a stale cached copy.
	// Production goes through config.Load, which pre-seeds both flags
	// to the SPEC §5.1 defaults (true) before the TOML decode — see
	// config.defaultConfig — so an operator's omitted [serve] section
	// keeps the documented SPEC behavior. Callers building a Config
	// programmatically (e.g. tests) must seed the bools by hand.
	Serve config.ServeConfig
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
	serve     config.ServeConfig
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
		serve:           cfg.Serve,
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
	// hdrXCacheSnapshot identifies the suite_snapshot.snapshot_id under
	// which a metadata response was resolved. SPEC2 §3 mandates this
	// diagnostic header on every Phase 2 snapshot-scoped hit so operators
	// (and the e2e test harness) can confirm that the response came from
	// the verified set rather than the trust-upstream Phase 1 path.
	hdrXCacheSnapshot = "X-Cache-Snapshot"
)

// SPEC2 §6.2 .deb miss-path validation sentinels. The handler maps
// these onto the same fail-closed shape (502 + Retry-After: 60) the
// hit-path uses, but with separate identities so respondError can log
// the right outcome string and operators can grep for either category.
var (
	// ErrPackageHashMismatch fires when a successful fetch's bytes
	// disagree with the *single* declared hash recorded by some current
	// snapshot's package_hash. Mirrors the "hash_validation_failure"
	// log keyword from SPEC2 §6.2.
	ErrPackageHashMismatch = errors.New("handler: fetched .deb hash disagrees with package_hash declaration")
	// ErrPackageHashConflict fires when two or more current snapshots
	// disagree on the declared hash for the same .deb path. Cache cannot
	// safely pick one; surfaces as 502 with the SPEC2 §6.1 step 6 log
	// keyword "package_hash_conflict".
	ErrPackageHashConflict = errors.New("handler: snapshots disagree on .deb declared hash")

	// ErrSnapshotMemberMismatch fires when SPEC2 §6.2 metadata recovery
	// (re-fetch of an adopted-suite metadata path whose pool blob is
	// missing) returns bytes whose sha256 disagrees with the
	// snapshot_member.declared_sha256. Indicates upstream has rolled
	// forward and the snapshot is stale; the next adoption flips
	// forward to the new InRelease and the path becomes serveable
	// again. respondError emits 502 + Retry-After: 30 (the same shape
	// writeFailClosed would have used had recovery not been attempted).
	ErrSnapshotMemberMismatch = errors.New("handler: re-fetched snapshot metadata hash disagrees with declared hash")
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
		h.logRequest(r, "", "", "method_not_allowed", http.StatusMethodNotAllowed, 0, false, 0, start)
		return
	}

	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		h.logRequest(r, "", "", "bad_request", http.StatusBadRequest, 0, false, 0, start)
		return
	}

	// Fast path: SPEC §6.1.
	if served, status, body := h.tryCacheHit(w, r, req, start); served {
		h.logRequest(r, req.CanonicalHost, req.Path, "hit", status, body, false, 0, start)
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
		h.logRequest(r, req.CanonicalHost, req.Path, "forbidden", http.StatusForbidden, 0, false, 0, start)
		return
	}

	// Miss: SPEC §6.2.
	h.serveCacheMiss(w, r, req, start)
}

// tryCacheHit attempts to serve from disk. Returns served=true if the
// response was sent (success, snapshot 404, package_hash conflict 502, or
// a 5xx during streaming). status and bytesWritten are best-effort for
// logging — http.ServeContent does the real header writes.
//
// SPEC2 §6.1 splits the lookup based on whether the request is metadata
// under a Phase-2-adopted suite. Three regimes flow out of this method:
//
//  1. Metadata under a suite with current_snapshot_id != NULL: lookup
//     goes through suite_freshness → snapshot_member, with NO Phase 1
//     url_path fallback. A miss in snapshot_member returns 404 — once
//     a suite is adopted, the snapshot is the contract.
//  2. Metadata without a snapshot pointer (pre-Phase-2 / never-adopted):
//     Phase 1 url_path lookup, identical to SPEC §6.1.
//  3. Non-metadata (.deb, etc.): Phase 1 url_path lookup, then
//     defense-in-depth check against package_hash. SPEC2 §6.1 step 5
//     evicts a stale row whose hash disagrees with the snapshot's
//     declared hash.
//
// The fast path on every successful return is one DB read, one stat, one
// open(2), and one ServeContent — order-of-microseconds.
func (h *Handler) tryCacheHit(w http.ResponseWriter, r *http.Request, req *proxy.Request, start time.Time) (served bool, status int, bytesWritten int64) {
	if req.IsMetadata && req.SuitePath != "" {
		if served, status, body, handled := h.trySnapshotHit(w, r, req, start); handled {
			return served, status, body
		}
	}
	return h.tryURLPathHit(w, r, req)
}

// trySnapshotHit is the SPEC2 §6.1 metadata fast path. Returns
// handled=true when the request was satisfied (served from a snapshot,
// 404 not-in-snapshot, or 502 fail-closed). Returns handled=false only
// when the suite has not been adopted (no suite_freshness row, or
// current_snapshot_id IS NULL), in which case the caller falls through
// to the Phase 1 url_path lookup.
//
// AIDEV-NOTE: every failure mode after a suite has been confirmed
// adopted (DB error on snapshot_member, etc.) must fail closed with a
// 502 — never drop into serveCacheMiss. The miss path does not
// validate metadata against snapshot_member, so any fall-through
// would let unverified upstream bytes masquerade as the adopted
// snapshot's content. SPEC2 §6.1 / §6.2: "the snapshot is the
// contract." A SuiteFreshness DB error is the one ambiguous case —
// we don't know whether the suite is adopted, so we fail closed too;
// the realistic failure mode is "DB is broken everywhere", and a 502
// across the board is the right operator signal.
//
// The "blob missing" case is the one place we *can* recover safely:
// snapshot_member.declared_sha256 is itself a trust anchor, so a
// re-fetch validated against that hash is as safe as the original
// adoption fetch. SPEC2 §6.2 names this as the local-fault recovery
// path. serveSnapshotMemberMiss owns it.
func (h *Handler) trySnapshotHit(w http.ResponseWriter, r *http.Request, req *proxy.Request, start time.Time) (served bool, status int, bytesWritten int64, handled bool) {
	suite, err := h.cache.GetSuiteFreshness(r.Context(),
		req.CanonicalScheme, req.CanonicalHost, req.SuitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// Suite has never been seen — pre-Phase-2 regime. Falling through
		// to url_path is safe because no §6.1 contract has been
		// established for this (host, suite).
		return false, 0, 0, false
	case err != nil:
		h.logger.Warn("suite_freshness lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"suite_path", req.SuitePath,
		)
		return h.writeFailClosed(w, "suite lookup failed"), http.StatusBadGateway, 0, true
	}
	if suite.CurrentSnapshotID == nil {
		// Suite known but never adopted — pre-Phase-2 regime.
		return false, 0, 0, false
	}
	snapshotID := *suite.CurrentSnapshotID

	memberPath := suiteRelativePath(req.SuitePath, req.Path)
	mem, err := h.cache.GetSnapshotMember(r.Context(), snapshotID, memberPath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		// SPEC2 §6.1: snapshot is the contract. Path not in the snapshot
		// → 404. No Phase 1 fallback would be allowed to satisfy this
		// request, regardless of whether url_path has a row for it.
		http.Error(w, "not in snapshot", http.StatusNotFound)
		return true, http.StatusNotFound, 0, true
	case err != nil:
		h.logger.Warn("snapshot_member lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"snapshot_id", snapshotID,
		)
		return h.writeFailClosed(w, "snapshot lookup failed"), http.StatusBadGateway, 0, true
	}

	served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, mem.BlobHash, snapshotMeta{
		snapshotID:    snapshotID,
		lastFetchedAt: nil, // snapshot rows don't carry per-blob last-fetched
	})
	if !served {
		// Pool blob is missing for a known snapshot member (operator
		// deletion, disk corruption removed by the integrity scanner).
		// SPEC2 §6.2 metadata recovery: hash-validated re-fetch against
		// snapshot_member.declared_sha256. Bytes whose sha256 disagrees
		// with the declaration are 502'd (upstream rolled forward; next
		// adoption flips us to the new InRelease).
		h.logger.Warn("snapshot_member_blob_missing",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"snapshot_id", snapshotID,
			"blob_hash", mem.BlobHash,
		)
		served, status, bytesWritten = h.serveSnapshotMemberMiss(w, r, req, mem, snapshotID, start)
		return served, status, bytesWritten, true
	}
	go h.touchAsync(req)
	h.maybeFireFreshness(req)
	return true, status, bytesWritten, true
}

// writeFailClosed writes a 502 + Retry-After: 30 response for an
// adopted-suite metadata request that cannot be served safely. Always
// returns true so the caller can fold it into the served-bool of
// trySnapshotHit's return tuple. SPEC2 §6.1 invariant: never drop into
// the unverified miss path on adopted suites.
func (h *Handler) writeFailClosed(w http.ResponseWriter, reason string) bool {
	w.Header().Set("Retry-After", "30")
	http.Error(w, reason, http.StatusBadGateway)
	return true
}

// tryURLPathHit is the Phase 1 / non-metadata url_path lookup. It also
// runs the SPEC2 §6.1 defense-in-depth package_hash check for .deb hits:
//
//   - 0 declared rows  → trust-upstream serve.
//   - 1 declared row matching url_path.blob_hash → serve.
//   - 1 declared row mismatching → evict url_path + decrement refcount,
//     log hit_path_hash_evicted, return false (caller falls through to
//     §6.2 miss path).
//   - 2+ distinct declared hashes → 502 + Retry-After 60, log
//     package_hash_conflict.
func (h *Handler) tryURLPathHit(w http.ResponseWriter, r *http.Request, req *proxy.Request) (served bool, status int, bytesWritten int64) {
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

	if !req.IsMetadata {
		if served, status, body, fellThrough := h.checkPackageHash(w, r, req, *row.BlobHash); fellThrough {
			return false, 0, 0
		} else if served {
			return true, status, body
		}
	}

	served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, *row.BlobHash, snapshotMeta{
		lastFetchedAt: row.LastFetchedAt,
	})
	if !served {
		return false, 0, 0
	}
	go h.touchAsync(req)
	h.maybeFireFreshness(req)
	return true, status, bytesWritten
}

// checkPackageHash runs the SPEC2 §6.1 defense-in-depth on a .deb hit.
//
// Returns served=true when the function wrote a 502 (conflict) directly.
// Returns fellThrough=true when the row was evicted and the caller
// should drop to the miss path. Returns both false when the hash check
// passed (or no rows exist) and the caller should serve normally.
func (h *Handler) checkPackageHash(w http.ResponseWriter, r *http.Request, req *proxy.Request, blobHash string) (served bool, status int, bytesWritten int64, fellThrough bool) {
	rows, err := h.cache.DeclaredHashesForPath(r.Context(),
		req.CanonicalScheme, req.CanonicalHost, req.Path)
	if err != nil {
		h.logger.Warn("package_hash lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		// Fall through to a normal serve — the on-disk url_path row is
		// our best Phase-1-grade answer. A hard fail-closed here would
		// turn a transient SQLite hiccup into a global .deb 502 storm.
		return false, 0, 0, false
	}
	distinct := distinctDeclared(rows)
	switch len(distinct) {
	case 0:
		// No snapshot covers this .deb (Phase 1 trust-upstream regime,
		// or this .deb's Packages member did not list it).
		return false, 0, 0, false
	case 1:
		if distinct[0] == blobHash {
			return false, 0, 0, false
		}
		// One row mismatches: stale Phase 1 row covered by a Phase 2
		// snapshot has diverged. Evict + drop into miss path.
		evictCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if eerr := h.cache.EvictURLPath(evictCtx,
			req.CanonicalScheme, req.CanonicalHost, req.Path); eerr != nil {
			h.logger.Warn("evict url_path failed",
				"err", eerr,
				"canonical_host", req.CanonicalHost,
				"path", req.Path,
			)
		}
		h.logger.Info("hit_path_hash_evicted",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"row_blob_hash", blobHash,
			"declared_sha256", distinct[0],
			"snapshot_id", rows[0].SnapshotID,
		)
		return false, 0, 0, true
	default:
		// Two or more distinct hashes: snapshots disagree. SPEC2 §6.1
		// step 6 / §6.2 share fail-closed behavior — 502 + Retry-After
		// 60 + log package_hash_conflict. Serving an arbitrary one of
		// the conflicting hashes is worse than refusing.
		w.Header().Set("Retry-After", "60")
		http.Error(w, "package hash conflict", http.StatusBadGateway)
		h.logger.Error("package_hash_conflict",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"row_blob_hash", blobHash,
			"declared", declaredAttrs(rows),
		)
		return true, http.StatusBadGateway, 0, false
	}
}

// snapshotMeta carries the metadata used by serveBlobWithHeaders to set
// the response headers. snapshotID = 0 marks a Phase 1 / non-snapshot
// serve (X-Cache-Snapshot suppressed); a positive value emits the
// header. lastFetchedAt is the source for X-Cache-Age and may be nil.
type snapshotMeta struct {
	snapshotID    int64
	lastFetchedAt *int64
}

// serveBlobWithHeaders opens pool/<blobHash>, sets the SPEC2 §3 headers,
// and streams the file via http.ServeContent. Returns served=false on
// any prerequisite failure (blob missing on disk, open or stat error)
// so the caller can drop into the miss path.
func (h *Handler) serveBlobWithHeaders(w http.ResponseWriter, r *http.Request, req *proxy.Request, blobHash string, meta snapshotMeta) (served bool, status int, bytesWritten int64) {
	exists, err := h.cache.BlobExists(blobHash)
	if err != nil || !exists {
		if err != nil {
			h.logger.Warn("blob existence check failed",
				"err", err,
				"hash", blobHash,
			)
		}
		return false, 0, 0
	}
	path := h.cache.BlobPath(blobHash)
	f, err := os.Open(path)
	if err != nil {
		h.logger.Warn("blob open failed", "err", err, "hash", blobHash)
		return false, 0, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		h.logger.Warn("blob stat failed", "err", err, "hash", blobHash)
		return false, 0, 0
	}

	w.Header().Set(hdrXCache, cacheHit)
	if meta.snapshotID > 0 {
		w.Header().Set(hdrXCacheSnapshot, strconv.FormatInt(meta.snapshotID, 10))
	}
	if meta.lastFetchedAt != nil {
		age := time.Now().Unix() - *meta.lastFetchedAt
		if age < 0 {
			age = 0
		}
		w.Header().Set(hdrXCacheAge, strconv.FormatInt(age, 10))
	}

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)
	return true, cw.statusCode(), cw.bytes
}

// suiteRelativePath strips a suitePath prefix from a request path,
// returning the suite-relative form snapshot_member.path uses
// (e.g. "/ubuntu/dists/noble" + "/ubuntu/dists/noble/InRelease" →
// "InRelease"). Defensive against the no-trailing-slash case where the
// request matches the suite path itself; that path is not metadata
// anyway and would not reach this function via tryCacheHit's gate.
func suiteRelativePath(suitePath, fullPath string) string {
	prefix := suitePath + "/"
	if strings.HasPrefix(fullPath, prefix) {
		return strings.TrimPrefix(fullPath, prefix)
	}
	// Path doesn't share the suite prefix — return as-is so the caller
	// sees a well-defined "not in snapshot" miss rather than a partial
	// match.
	return fullPath
}

// distinctDeclared collapses a DeclaredHash slice down to its unique
// declared_sha256 values, in stable insertion order. Used by the §6.1
// step 4/5/6 dispatch — the *count* of unique hashes is what drives the
// outcome, regardless of how many snapshots independently agreed on a
// hash.
func distinctDeclared(rows []cache.DeclaredHash) []string {
	if len(rows) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.DeclaredSHA256]; dup {
			continue
		}
		seen[r.DeclaredSHA256] = struct{}{}
		out = append(out, r.DeclaredSHA256)
	}
	return out
}

// declaredAttrs renders a DeclaredHash slice as a structured
// log-attribute value for the package_hash_conflict log line. The
// repeated (declared_sha256, snapshot_id) tuples let an operator trace
// which specific snapshots disagreed.
func declaredAttrs(rows []cache.DeclaredHash) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"declared_sha256": r.DeclaredSHA256,
			"snapshot_id":     r.SnapshotID,
		})
	}
	return out
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

	res, shared, waiters := h.sf.Do(key, func() sfResult {
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

	// SPEC §10: leader emits a structured log line whenever waiters
	// joined the call (i.e. coalescing actually occurred). A leader
	// running solo gets no log line — the per-request line is enough.
	if !shared && waiters > 0 {
		h.logger.Info("singleflight coalesced",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"waiters", waiters,
		)
	}

	if res.err != nil {
		h.respondError(w, r, req, res, start)
		return
	}

	hash := res.blobHash
	path := h.cache.BlobPath(hash)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		h.logRequest(r, req.CanonicalHost, req.Path, "error", http.StatusInternalServerError, 0, true, res.status, start)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "cache stat failed", http.StatusInternalServerError)
		h.logRequest(r, req.CanonicalHost, req.Path, "error", http.StatusInternalServerError, 0, true, res.status, start)
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
	h.logRequest(r, req.CanonicalHost, req.Path, logOutcome, cw.statusCode(), cw.bytes, true, res.status, start)
}

// serveSnapshotMemberMiss runs the SPEC2 §6.2 metadata recovery: an
// adopted-suite snapshot_member exists, but the pool blob it references
// has gone missing (operator deletion, integrity-scanner removal of
// at-rest corruption). Fetch from upstream, validate the bytes against
// snapshot_member.declared_sha256, and on match serve from pool/. No
// url_path row is inserted — snapshot_member is the trust anchor for
// adopted metadata, and a Phase 1 row would reintroduce the unverified
// path.
//
// On hash mismatch (upstream rolled forward, our snapshot is stale)
// the response is 502 + Retry-After: 30. The next adoption flip
// updates snapshot_member to the new declared hash; that is the
// recovery, not this miss path.
//
// Singleflight-coalesced under a separate key namespace so it cannot
// collide with the regular miss path. Use the handler lifecycle ctx
// for the inner fetch — same rationale as serveCacheMiss: a leader's
// client disconnect must not kill the fetch for waiters.
//
// Returns (served, status, bytesWritten) so the trySnapshotHit caller
// can fold the result into its own return tuple. The access-log line
// is emitted upstream by ServeHTTP under the existing tryCacheHit
// path.
func (h *Handler) serveSnapshotMemberMiss(
	w http.ResponseWriter, r *http.Request, req *proxy.Request,
	mem *cache.SnapshotMember, snapshotID int64, start time.Time,
) (served bool, status int, bytesWritten int64) {
	cw := &countingWriter{ResponseWriter: w}

	// AIDEV-NOTE: the key embeds mem.DeclaredSHA256 so two requests
	// arriving for the same path under different snapshots — e.g. an
	// adoption flip occurred between the two GetSnapshotMember reads
	// and the new snapshot declares a different hash for the same
	// path — cannot coalesce. Without this, the leader's fetch would
	// validate against one declared_sha256 while the waiter expected
	// the other; the waiter would receive bytes that pass the
	// leader's hash check but not its own, with X-Cache-Snapshot
	// reflecting the waiter's snapshot id (read separately by
	// trySnapshotHit). Including snapshotID would also work; the
	// declared hash is more conservative — two snapshots that *agree*
	// on a path's hash safely coalesce.
	key := "snapshot:" + req.CanonicalScheme + "|" + req.CanonicalHost + "|" + req.Path + "|" + mem.DeclaredSHA256
	res, shared, waiters := h.sf.Do(key, func() sfResult {
		return h.runSnapshotMemberFetch(h.lifecycleCtx, req, mem, snapshotID)
	})
	if !shared && waiters > 0 {
		h.logger.Info("singleflight coalesced",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"waiters", waiters,
		)
	}

	if res.err != nil {
		h.respondRecoveryError(cw, r, req, res, start)
		return true, cw.statusCode(), cw.bytes
	}

	// Open the freshly-finalized blob and stream it via ServeContent
	// directly, with X-Cache: MISS (recovery just fetched the bytes)
	// and X-Cache-Snapshot identifying the trust anchor. Avoids
	// serveBlobWithHeaders, which always emits X-Cache: HIT and is
	// the wrong surface here.
	path := h.cache.BlobPath(res.blobHash)
	f, err := os.Open(path)
	if err != nil {
		// Vanishingly unlikely: blob just finalized and now the file is
		// gone. The at-rest scanner running mid-request between
		// Finalize and Open is the only realistic cause. Treat as a
		// local fault and 502.
		h.logger.Warn("post-recovery blob open failed",
			"err", err, "hash", res.blobHash, "path", req.Path)
		cw.Header().Set("Retry-After", "30")
		http.Error(cw, "post-fetch blob missing", http.StatusBadGateway)
		return true, http.StatusBadGateway, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		h.logger.Warn("post-recovery blob stat failed",
			"err", err, "hash", res.blobHash)
		cw.Header().Set("Retry-After", "30")
		http.Error(cw, "post-fetch blob stat failed", http.StatusBadGateway)
		return true, http.StatusBadGateway, 0
	}

	cw.Header().Set(hdrXCache, cacheMiss)
	cw.Header().Set(hdrXCacheSnapshot, strconv.FormatInt(snapshotID, 10))
	cw.Header().Set(hdrXCacheAge, "0")
	if res.status > 0 {
		cw.Header().Set(hdrXUpstreamStat, strconv.Itoa(res.status))
	}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)
	return true, cw.statusCode(), cw.bytes
}

// runSnapshotMemberFetch is the singleflight body for SPEC2 §6.2
// metadata recovery. Fetches the upstream URL into a temp blob,
// finalizes into pool/, and verifies the resulting hash against
// mem.DeclaredSHA256. On mismatch the file is discarded and
// ErrSnapshotMemberMismatch is returned; respondError converts it to
// 502 + Retry-After: 30.
//
// No url_path row is written — snapshot_member already references the
// blob, and a Phase 1 url_path row would let an unverified Phase 1
// hit-path serve this metadata for non-adopted suites that share the
// same canonical (host, path) (rare but legal).
//
// blob row is upserted (PutBlob is INSERT OR IGNORE) so that a missing
// blob row from any earlier abnormal teardown becomes consistent
// again. The ContentLength comes from the fetch result so the row's
// size column is accurate even if the original adoption row predates
// any size schema change.
func (h *Handler) runSnapshotMemberFetch(ctx context.Context, req *proxy.Request, mem *cache.SnapshotMember, snapshotID int64) sfResult {
	release, err := h.sem.Acquire(ctx, req.CanonicalHost)
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: acquire host slot: %w", err)}
	}
	defer release()

	bw, err := h.cache.NewTempBlob()
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: open temp blob: %w", err)}
	}

	target := &fetch.Target{CanonicalHost: req.CanonicalHost, URL: req.UpstreamURL}
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

	// FinalizeExpectingHash gates on declared_sha256 *before* the
	// rename into pool/. A mismatch removes the temp without touching
	// pool/, so an upstream serving bytes whose hash collides with an
	// unrelated cached blob (real-world: misrouted Remap, mirror
	// confusion) cannot cause the unrelated blob to be deleted.
	hash, err := bw.FinalizeExpectingHash(mem.DeclaredSHA256, fres.ContentLength)
	if errors.Is(err, cache.ErrHashMismatch) {
		// SPEC2 §10.2 names adoption_member_mismatch as the keyword for
		// adoption-time mismatches; for the post-adoption recovery
		// surface we use a sibling keyword so operators can distinguish
		// the two.
		h.logger.Error("snapshot_member_refetch_mismatch",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"declared_sha256", mem.DeclaredSHA256,
			"observed_sha256", hash,
			"snapshot_id", snapshotID,
		)
		return sfResult{err: ErrSnapshotMemberMismatch, status: fres.Status}
	}
	if err != nil {
		return sfResult{err: fmt.Errorf("handler: finalize blob: %w", err), status: fres.Status}
	}

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.cache.PutBlob(dbCtx, hash, fres.ContentLength); err != nil {
		return sfResult{err: fmt.Errorf("handler: put blob row: %w", err), status: fres.Status}
	}
	return sfResult{blobHash: hash, size: fres.ContentLength, status: fres.Status}
}

// respondRecoveryError handles fetch errors from a SPEC2 §6.2 metadata
// recovery (serveSnapshotMemberMiss). Unlike respondError, every error
// flow lands at 502 + Retry-After: 30 — never as 4xx-passthrough,
// never with a stale URL-path fallback. The §6.1 invariant
// "snapshot is the contract" forbids satisfying an adopted-suite
// metadata request with anything other than bytes validated against
// snapshot_member.declared_sha256, so:
//
//   - upstream 4xx during recovery would otherwise pass through as
//     404/410 etc., letting the client see a stale "not found" for a
//     path the snapshot still vouches for. Surface as 502.
//   - upstream-unreachable would otherwise route through
//     respondUpstreamUnreachable, which can serve HIT-STALE from
//     url_path. The url_path row may not even exist for adopted-suite
//     metadata (snapshot_member is the trust anchor); even if it did
//     exist, serving it would substitute Phase 1 trust-upstream bytes
//     for Phase 2 verified bytes. Surface as 502.
//
// The outcome string distinguishes the failure category for ops
// dashboards while the wire response is uniform.
func (h *Handler) respondRecoveryError(w http.ResponseWriter, r *http.Request, req *proxy.Request, res sfResult, start time.Time) {
	if res.status > 0 {
		w.Header().Set(hdrXUpstreamStat, strconv.Itoa(res.status))
	}
	err := res.err
	outcome := "snapshot_recovery_failed"
	switch {
	case errors.Is(err, ErrSnapshotMemberMismatch):
		outcome = "snapshot_member_refetch_mismatch"
	case errors.Is(err, fetch.ErrUpstreamStatus):
		outcome = "snapshot_recovery_upstream_status"
	case errors.Is(err, fetch.ErrUpstreamUnavailable),
		errors.Is(err, fetch.ErrUpstreamServerError),
		errors.Is(err, context.DeadlineExceeded):
		outcome = "snapshot_recovery_upstream_unreachable"
	case errors.Is(err, fetch.ErrCacheWriteFailed):
		outcome = "snapshot_recovery_cache_write_failed"
	case errors.Is(err, fetch.ErrHostNotAllowed),
		errors.Is(err, fetch.ErrTargetDenied):
		outcome = "snapshot_recovery_target_denied"
	}
	w.Header().Set("Retry-After", "30")
	http.Error(w, "snapshot member recovery failed", http.StatusBadGateway)
	h.logRequest(r, req.CanonicalHost, req.Path, outcome, http.StatusBadGateway, 0, true, res.status, start)
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

	// SPEC2 §6.2 .deb hash validation. For non-metadata paths covered by
	// any current snapshot's package_hash, the fetched bytes must match
	// the declared hash before they can enter pool/ or url_path. The
	// query is the same shape as §6.1 step 2; conflicting declarations
	// (≥ 2 distinct hashes) fail closed exactly like the hit-path.
	//
	// Skipped for metadata: §6.2 metadata validation under an adopted
	// suite needs a different trust anchor (snapshot_member.declared_sha256
	// keyed by suite-relative path), and trySnapshotHit fails closed before
	// metadata can reach this code path on an adopted suite. Metadata under
	// non-adopted suites is the Phase 1 trust-upstream path with no
	// snapshot to validate against.
	if !req.IsMetadata {
		// AIDEV-TODO: this Finalize-then-validate-then-DiscardFinalizedBlob
		// pattern has the same blob-collision risk runSnapshotMemberFetch
		// fixed via FinalizeExpectingHash: a fetched .deb whose hash
		// happens to match an unrelated cached blob (mirror confusion,
		// misrouted Remap rule, content-identical .deb under a different
		// path) would have its file preserved by Finalize's dedup branch
		// and then removed by DiscardFinalizedBlob, evicting the
		// unrelated valid blob. The single-declared-row case can be
		// migrated to FinalizeExpectingHash; the conflict case
		// (≥ 2 distinct declared hashes) needs a more general
		// "FinalizeIfHashIn" because there is no single expected hash
		// to gate on. Tracking as a follow-up to the codex review of
		// commit a1162c5.
		if verr := h.validateDebAgainstPackageHash(ctx, req, hash); verr != nil {
			if rerr := h.cache.DiscardFinalizedBlob(hash); rerr != nil {
				h.logger.Warn("discard mismatched blob failed",
					"err", rerr,
					"hash", hash,
				)
			}
			return sfResult{err: verr, status: fres.Status}
		}
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

// validateDebAgainstPackageHash runs the SPEC2 §6.2 .deb miss-path
// validation: after a fetched non-metadata blob is finalized into pool/,
// query DeclaredHashesForPath and compare against the fetched hash.
//
//   - 0 declared rows  → no current snapshot covers this path; trust-
//     upstream (returns nil).
//   - 1 declared row, hash matches → OK.
//   - 1 declared row, hash mismatches → return ErrPackageHashMismatch
//     with the declared/observed pair logged. SPEC2 §6.2 keyword
//     hash_validation_failure.
//   - 2+ distinct declared rows → return ErrPackageHashConflict with
//     all (declared_sha256, snapshot_id) pairs logged. SPEC2 §6.2 +
//     §6.1 step 6 share the keyword package_hash_conflict.
//
// The caller (runFetch) is responsible for discarding the on-disk pool
// blob and returning a sfResult with this error, which respondError
// surfaces as 502 + Retry-After: 60.
func (h *Handler) validateDebAgainstPackageHash(ctx context.Context, req *proxy.Request, fetchedHash string) error {
	rows, err := h.cache.DeclaredHashesForPath(ctx,
		req.CanonicalScheme, req.CanonicalHost, req.Path)
	if err != nil {
		// DB error during validation — fail open, like the hit-path
		// helper. A blanket fail-closed would turn a transient SQLite
		// hiccup into a global .deb 502 storm.
		h.logger.Warn("package_hash validate lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		return nil
	}
	distinct := distinctDeclared(rows)
	switch len(distinct) {
	case 0:
		// SPEC2 §6.2 keyword package_hash_miss is Debug-level diagnostic
		// for monitoring coverage gaps; emitted only when a request
		// reaches the miss path with no covering snapshot.
		h.logger.Debug("package_hash_miss",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		return nil
	case 1:
		if distinct[0] == fetchedHash {
			return nil
		}
		h.logger.Error("hash_validation_failure",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"declared_sha256", distinct[0],
			"observed_sha256", fetchedHash,
			"snapshot_id", rows[0].SnapshotID,
		)
		return fmt.Errorf("%w: declared %s, observed %s",
			ErrPackageHashMismatch, distinct[0], fetchedHash)
	default:
		h.logger.Error("package_hash_conflict",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"observed_sha256", fetchedHash,
			"declared", declaredAttrs(rows),
		)
		return ErrPackageHashConflict
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
// SPEC §6.4: upstream unreachable on a miss → HIT-STALE if eligible,
// otherwise 502 + Retry-After (30s for metadata, 60s for blobs).
// Upstream 4xx (e.g. apt probing for an index variant that does not
// exist) → passthrough. Any other failure → 502.
func (h *Handler) respondError(w http.ResponseWriter, r *http.Request, req *proxy.Request, res sfResult, start time.Time) {
	err := res.err
	if res.status > 0 {
		w.Header().Set(hdrXUpstreamStat, strconv.Itoa(res.status))
	}

	switch {
	case errors.Is(err, fetch.ErrHostNotAllowed):
		// Pre-flight allowlist rejection — no dial happened. Match
		// the convention used by the handler-level pre-flight host
		// check at line 192 (fetchAttempted=false): operators reading
		// audit logs see "no upstream attempt" presence-of-field for
		// every host-rejected request, regardless of which layer
		// fired the rejection.
		http.Error(w, "forbidden", http.StatusForbidden)
		h.logRequest(r, req.CanonicalHost, req.Path, "forbidden", http.StatusForbidden, 0, false, 0, start)
		return
	case errors.Is(err, fetch.ErrTargetDenied):
		// Deny-CIDR rejection fires *during* the dial — DNS resolved,
		// connect attempted, dialer rejected the post-resolution IP.
		// fetchAttempted=true so operators can distinguish this from
		// the pre-flight host rejection above.
		http.Error(w, "forbidden", http.StatusForbidden)
		h.logRequest(r, req.CanonicalHost, req.Path, "forbidden", http.StatusForbidden, 0, true, res.status, start)
		return
	case errors.Is(err, fetch.ErrUpstreamStatus):
		// SPEC §6.4: upstream 4xx is a "client said no" passthrough
		// (apt probes for index variants the archive does not
		// publish — 404 must reach the client so apt moves on).
		// StatusError.Is(ErrUpstreamStatus) is now narrowed to 4xx,
		// so 5xx exhaustion routes to ErrUpstreamServerError below
		// instead of falling through this case.
		http.Error(w, fmt.Sprintf("upstream status %d", res.status), res.status)
		h.logRequest(r, req.CanonicalHost, req.Path, "upstream_status", res.status, 0, true, res.status, start)
		return
	case errors.Is(err, fetch.ErrUpstreamUnavailable),
		errors.Is(err, fetch.ErrUpstreamServerError),
		errors.Is(err, context.DeadlineExceeded):
		h.respondUpstreamUnreachable(w, r, req, res.status, start)
		return
	case errors.Is(err, fetch.ErrCacheWriteFailed):
		// SPEC §11 row 14: a cache-side write failure (disk full, I/O
		// error) is operationally distinct from an upstream outage —
		// re-fetch will fail identically until the disk is healed. Log
		// loudly so the operator sees the actual condition rather than
		// chasing a phantom upstream issue, and emit the same 502 +
		// Retry-After the upstream-down path uses (the client behavior
		// is the same; only the server-side signal differs).
		h.logger.Error("cache write failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		w.Header().Set("Retry-After", retryAfterForRequest(req))
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "cache_write_failed", http.StatusBadGateway, 0, true, res.status, start)
		return
	case errors.Is(err, ErrPackageHashMismatch):
		// SPEC2 §6.2: fetched .deb bytes disagree with the snapshot's
		// declared hash. Discard + 502 + Retry-After: 60. The detailed
		// log line was already emitted in validateDebAgainstPackageHash.
		w.Header().Set("Retry-After", "60")
		http.Error(w, "package hash mismatch", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "package_hash_mismatch", http.StatusBadGateway, 0, true, res.status, start)
		return
	case errors.Is(err, ErrPackageHashConflict):
		// SPEC2 §6.1 step 6 / §6.2: snapshots disagree. 502 + Retry-After: 60.
		w.Header().Set("Retry-After", "60")
		http.Error(w, "package hash conflict", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "package_hash_conflict", http.StatusBadGateway, 0, true, res.status, start)
		return
	case errors.Is(err, ErrSnapshotMemberMismatch):
		// SPEC2 §6.2 metadata recovery hash mismatch: re-fetched bytes
		// don't match the snapshot's declared hash. Upstream rolled
		// forward; the next adoption flips us forward. 502 + Retry-After: 30
		// (local-fault category, same shape as writeFailClosed).
		w.Header().Set("Retry-After", "30")
		http.Error(w, "snapshot member hash mismatch", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "snapshot_member_refetch_mismatch", http.StatusBadGateway, 0, true, res.status, start)
		return
	case errors.Is(err, fetch.ErrInvalidURL):
		// URL parse / scheme / host-mismatch failure — fired before
		// any network I/O. fetchAttempted=false (no upstream dial
		// occurred). HIT-STALE would mask the real config bug; fail
		// loud with 502 so the operator sees the malformed URL.
		w.Header().Set("Retry-After", retryAfterForRequest(req))
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, false, 0, start)
		return
	case errors.Is(err, fetch.ErrRedirectBlocked):
		// Upstream emitted a 3xx that we refuse to follow. A response
		// *was* received (the 3xx) — fetchAttempted=true with the
		// upstream's redirect status preserved by fetch.doAttempt's
		// StatusError-wrap. HIT-STALE would mask an upstream that
		// moved; fail loud so the operator configures a Remap rule.
		w.Header().Set("Retry-After", retryAfterForRequest(req))
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, true, res.status, start)
		return
	case errors.Is(err, context.Canceled):
		// Client almost certainly disconnected. 499 is non-standard;
		// 503 with Retry-After is the closest sensible response.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		h.logRequest(r, req.CanonicalHost, req.Path, "client_canceled", http.StatusServiceUnavailable, 0, true, res.status, start)
		return
	default:
		w.Header().Set("Retry-After", retryAfterForRequest(req))
		http.Error(w, "bad gateway", http.StatusBadGateway)
		h.logger.Error("unclassified fetch error",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, true, res.status, start)
		return
	}
}

// respondUpstreamUnreachable resolves a "couldn't talk to upstream" error.
// SPEC §6.4: when policy allows AND the request is for metadata, serve a
// stale cached copy with X-Cache: HIT-STALE; otherwise 502 with the
// SPEC-mandated Retry-After (30s for metadata, 60s for blobs — apt
// retries metadata aggressively, so a longer cool-off on blobs spreads
// the load when upstream comes back).
func (h *Handler) respondUpstreamUnreachable(w http.ResponseWriter, r *http.Request, req *proxy.Request, upstreamStatus int, start time.Time) {
	if h.tryServeStale(w, r, req, upstreamStatus, start) {
		return
	}
	w.Header().Set("Retry-After", retryAfterForRequest(req))
	http.Error(w, "bad gateway", http.StatusBadGateway)
	h.logRequest(r, req.CanonicalHost, req.Path, "bad_gateway", http.StatusBadGateway, 0, true, upstreamStatus, start)
}

// tryServeStale serves the cached blob for req with X-Cache: HIT-STALE
// when one is available, returning true if it wrote the response.
//
// SPEC §6.4 + §8: only metadata is stale-eligible. .deb fetches are not
// (apt verifies index hashes against InRelease, so a hash-mismatched .deb
// would reach the client and fail loudly — better to 502 and let apt
// retry). Returns false on any error or missing prerequisite (no row, no
// blob, blob open/stat fails) so the caller falls through to 502.
//
// Note: tryServeStale runs after the singleflight fetch returned an
// error, which means tryCacheHit was already consulted at request entry
// and either (a) returned false because the row was missing, or (b)
// returned false because the row pointed at a blob no longer on disk.
// Case (b) cannot recover here — the blob is still missing — so the
// only way this method actually serves is if a row+blob materialized
// between the two lookups (a benign race with a concurrent successful
// fetch under a different singleflight key, or an operator-restored
// blob). In Phase 2 this method becomes the centerpiece of "served from
// frozen consistent set during freshness divergence."
func (h *Handler) tryServeStale(w http.ResponseWriter, r *http.Request, req *proxy.Request, upstreamStatus int, start time.Time) bool {
	if !req.IsMetadata {
		return false
	}
	if !h.serve.ServeStaleWhenUpstreamDown {
		return false
	}
	row, err := h.cache.LookupURL(r.Context(), req.CanonicalScheme, req.CanonicalHost, req.Path)
	if err != nil || row.BlobHash == nil || *row.BlobHash == "" {
		return false
	}
	hash := *row.BlobHash
	exists, err := h.cache.BlobExists(hash)
	if err != nil || !exists {
		return false
	}
	f, err := os.Open(h.cache.BlobPath(hash))
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}

	w.Header().Set(hdrXCache, cacheHitStale)
	if row.LastFetchedAt != nil {
		age := time.Now().Unix() - *row.LastFetchedAt
		if age < 0 {
			age = 0
		}
		w.Header().Set(hdrXCacheAge, strconv.FormatInt(age, 10))
	}

	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, st.ModTime(), f)

	if h.serve.LogStaleServes {
		h.logger.Info("stale_serve",
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
			"blob_hash", hash,
		)
	}
	h.logRequest(r, req.CanonicalHost, req.Path, "hit_stale", cw.statusCode(), cw.bytes, true, upstreamStatus, start)
	return true
}

// retryAfterForRequest returns the SPEC §6.4 Retry-After value: 30s for
// metadata, 60s for everything else. The differentiation matters because
// apt retries metadata fetches with much shorter delays than blob
// fetches, so a single Retry-After value either hammers the cache during
// metadata recovery or wastes minutes of clock time on a transient blob
// failure.
func retryAfterForRequest(req *proxy.Request) string {
	if req.IsMetadata {
		return "30"
	}
	return "60"
}

// logRequest emits the per-request slog line. SPEC §10.
//
// AIDEV-NOTE: never log r.RequestURI directly — proxy-form requests can
// (in principle) carry userinfo like http://user:pass@host/path. The
// parser rejects userinfo before it reaches the success path, but the
// 400/405 log calls fire before the parser has run, so we route the
// URL through urlForLog which strips userinfo unconditionally.
//
// SPEC §10: upstream_status is logged "when a fetch was attempted",
// including 0 when no upstream response arrived (timeout, connection
// refused, dial denied). Field presence is the operator's signal for
// "did this request reach upstream"; the value distinguishes "got a
// response" (status code) from "fetch attempted but no response" (0).
// Use fetchAttempted=false only on pre-fetch outcomes (hit, 400, 405,
// pre-fetch allowlist 403); use true for every miss-path outcome,
// including HIT-STALE (which fired after a fetch failed).
func (h *Handler) logRequest(r *http.Request, canonHost, path, outcome string, status int, bytesWritten int64, fetchAttempted bool, upstreamStatus int, start time.Time) {
	attrs := []any{
		"method", r.Method,
		"url", urlForLog(r),
		"canonical_host", canonHost,
		"path", path,
		"outcome", outcome,
		"status", status,
		"bytes_sent", bytesWritten,
		"duration_ms", time.Since(start).Milliseconds(),
		"client_addr", r.RemoteAddr,
	}
	if fetchAttempted {
		attrs = append(attrs, "upstream_status", upstreamStatus)
	}
	h.logger.Info("request", attrs...)
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
