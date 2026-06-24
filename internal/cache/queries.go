package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/debversion"
)

// ErrNotFound is returned when a row is absent. Callers should distinguish
// "miss" (caller's normal flow) from real DB errors.
var ErrNotFound = errors.New("cache: not found")

// ErrHotSetCandidateMismatch is returned by ComputeHotSet when a
// caller-supplied candidate row's (CanonicalScheme, CanonicalHost,
// SnapshotID) tuple does not match the (scheme, host,
// candidateSnapshotID) inputs. The old SQL Stage 2 enforced this
// implicitly via the WHERE clause; the in-memory form must surface
// the contract violation explicitly so a future caller mismatch
// fails loudly instead of warming the wrong rows.
var ErrHotSetCandidateMismatch = errors.New("cache: hot-set candidate row metadata mismatch")

// LookupURL returns the url_path row for (canonicalScheme, canonicalHost,
// path), or ErrNotFound. Reads use the connection pool directly; this is
// the hot path on every cache hit.
func (c *Cache) LookupURL(ctx context.Context, scheme, host, path string) (*URLPath, error) {
	const q = `
SELECT canonical_scheme, canonical_host, path, blob_hash, upstream_url,
       is_metadata, last_requested_at, request_count, last_fetched_at,
       upstream_etag, upstream_lastmod
FROM url_path
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`
	var u URLPath
	var isMD int64
	err := c.db.QueryRowContext(ctx, q, scheme, host, path).Scan(
		&u.CanonicalScheme, &u.CanonicalHost, &u.Path,
		&u.BlobHash, &u.UpstreamURL, &isMD,
		&u.LastRequestedAt, &u.RequestCount, &u.LastFetchedAt,
		&u.UpstreamETag, &u.UpstreamLastMod,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("LookupURL: %w", err)
	}
	u.IsMetadata = isMD != 0
	return &u, nil
}

// PutURLPath inserts-or-replaces the url_path row. blob_hash, last_*,
// upstream_etag, and upstream_lastmod are all carried through — pass nil
// pointers for fields that should remain SQL NULL.
func (c *Cache) PutURLPath(ctx context.Context, u URLPath) error {
	const q = `
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, path) DO UPDATE SET
  blob_hash         = excluded.blob_hash,
  upstream_url      = excluded.upstream_url,
  is_metadata       = excluded.is_metadata,
  last_requested_at = excluded.last_requested_at,
  request_count     = excluded.request_count,
  last_fetched_at   = excluded.last_fetched_at,
  upstream_etag     = excluded.upstream_etag,
  upstream_lastmod  = excluded.upstream_lastmod`
	isMD := int64(0)
	if u.IsMetadata {
		isMD = 1
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q,
			u.CanonicalScheme, u.CanonicalHost, u.Path, u.BlobHash, u.UpstreamURL,
			isMD, u.LastRequestedAt, u.RequestCount, u.LastFetchedAt,
			u.UpstreamETag, u.UpstreamLastMod)
		if err != nil {
			return fmt.Errorf("PutURLPath: %w", err)
		}
		return nil
	})
}

// TouchURLPath records that a request was just served from cache for this
// (scheme, host, path). Increments request_count and updates
// last_requested_at. Cheap, hot-path operation; safe to fire-and-forget
// on cache hits.
func (c *Cache) TouchURLPath(ctx context.Context, scheme, host, path string) error {
	const q = `
UPDATE url_path
SET request_count = request_count + 1,
    last_requested_at = ?
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, now, scheme, host, path)
		if err != nil {
			return fmt.Errorf("TouchURLPath: %w", err)
		}
		return nil
	})
}

// HostHasBlob reports whether (scheme, host) has any url_path row
// pointing at blobHash. Used by the by-hash content-addressed fallback
// to verify that the blob was previously fetched through this host's
// allowlist before serving it under a new URL — without this gate, a
// caller who learns a SHA256 from one host's content could request it
// under any other allowlisted host and receive bytes that were never
// vouched for by that host. ErrInvalidHash on a malformed hash; nil
// (false, no error) when the row simply does not exist.
func (c *Cache) HostHasBlob(ctx context.Context, scheme, host, blobHash string) (bool, error) {
	if !validBlobHash(blobHash) {
		return false, fmt.Errorf("%w: %q", ErrInvalidHash, blobHash)
	}
	const q = `
SELECT 1 FROM url_path
WHERE canonical_scheme = ? AND canonical_host = ? AND blob_hash = ?
LIMIT 1`
	var one int
	err := c.db.QueryRowContext(ctx, q, scheme, host, blobHash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("HostHasBlob: %w", err)
	}
	return true, nil
}

// GetBlob returns the blob row for hash, or ErrNotFound. Returns
// ErrInvalidHash without touching the DB if the hash is malformed.
func (c *Cache) GetBlob(ctx context.Context, hash string) (*Blob, error) {
	if !validBlobHash(hash) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `SELECT hash, size, created_at, refcount FROM blob WHERE hash = ?`
	var b Blob
	err := c.db.QueryRowContext(ctx, q, hash).Scan(&b.Hash, &b.Size, &b.CreatedAt, &b.RefCount)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetBlob: %w", err)
	}
	return &b, nil
}

// PutBlob inserts a blob row. New rows are born at refcount=0 with
// refcount_zeroed_at = created_at — the grace clock starts at birth so a
// fetch that completes the blob write but whose FK-bearing INSERT never
// commits is reaped one grace later, never on the very next tick.
//
// On hash conflict the existing row's refcount, size, and created_at are
// preserved; only refcount_zeroed_at is refreshed, and only when the
// existing row is at refcount <= 0. This closes the "reuse an orphan
// blob" race (SPEC4 §7.5.1 Rule 1): an existing blob row sitting at
// refcount <= 0 with a stale refcount_zeroed_at could otherwise be reaped
// between the PutBlob ExecContext returning and the caller's FK-bearing
// INSERT committing. Restarting the grace clock to "now" gives the caller
// a full gc.blob_grace window. The conflict's WHERE blob.refcount <= 0
// predicate skips the UPDATE entirely on the positive-refcount path — no
// journal write, no row mutation.
//
// The schema also CHECKs that hash matches sha256-hex shape, so this Go
// validation is defense-in-depth. We surface ErrInvalidHash before
// submitting to the writer goroutine so a buggy caller fails immediately.
func (c *Cache) PutBlob(ctx context.Context, hash string, size int64) error {
	if !validBlobHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `
INSERT INTO blob (hash, size, created_at, refcount, refcount_zeroed_at)
VALUES (?, ?, ?, 0, ?)
ON CONFLICT(hash) DO UPDATE
   SET refcount_zeroed_at = excluded.refcount_zeroed_at
 WHERE blob.refcount <= 0`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, hash, size, now, now)
		if err != nil {
			return fmt.Errorf("PutBlob: %w", err)
		}
		return nil
	})
}

// GetSuiteFreshness returns the suite_freshness row for the suite, or
// ErrNotFound when no check has ever been recorded.
func (c *Cache) GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*SuiteFreshness, error) {
	const q = `
SELECT canonical_scheme, canonical_host, suite_path,
       last_check_at, last_success_at,
       inrelease_etag, inrelease_lastmod, inrelease_change_seen_at,
       current_snapshot_id
FROM suite_freshness
WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	var s SuiteFreshness
	err := c.db.QueryRowContext(ctx, q, scheme, host, suitePath).Scan(
		&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
		&s.LastCheckAt, &s.LastSuccessAt,
		&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
		&s.CurrentSnapshotID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSuiteFreshness: %w", err)
	}
	return &s, nil
}

// ListSuitesWithAdoption returns every suite_freshness row LEFT JOINed
// with suite_snapshot on current_snapshot_id = snapshot_id. The extra
// adopted_at column is what the SPEC5 §9.7.3 status-page render needs
// to show "this suite was last adopted at <timestamp>" without an
// N+1 GetSuiteSnapshot lookup.
//
// AIDEV-NOTE: SPEC5 §9.7.8 — exists alongside ListSuites (which is
// untouched and continues to back the periodic freshness scheduler);
// the two helpers have different return shapes. Use ListSuites when
// you only need freshness columns; use ListSuitesWithAdoption when
// you need the snapshot's adopted_at too.
//
// CurrentAdoptedAt is nil when:
//   - suite_freshness.current_snapshot_id IS NULL (suite never
//     adopted), OR
//   - suite_freshness.current_snapshot_id points at a snapshot_id
//     that no longer exists (data-corruption case — should not
//     happen under normal operation but the LEFT JOIN tolerates it
//     so the status page renders), OR
//   - the matching suite_snapshot row has adopted_at IS NULL (the
//     candidate-but-not-adopted case — which would not be a
//     "current" snapshot, but the LEFT JOIN doesn't filter on
//     adopted_at to keep the query simple).
func (c *Cache) ListSuitesWithAdoption(ctx context.Context) ([]SuiteWithAdoption, error) {
	const q = `
SELECT sf.canonical_scheme, sf.canonical_host, sf.suite_path,
       sf.last_check_at, sf.last_success_at,
       sf.inrelease_etag, sf.inrelease_lastmod, sf.inrelease_change_seen_at,
       sf.current_snapshot_id, ss.adopted_at
FROM suite_freshness sf
LEFT JOIN suite_snapshot ss
       ON ss.snapshot_id = sf.current_snapshot_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ListSuitesWithAdoption: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteWithAdoption
	for rows.Next() {
		var s SuiteWithAdoption
		if err := rows.Scan(
			&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
			&s.LastCheckAt, &s.LastSuccessAt,
			&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
			&s.CurrentSnapshotID, &s.CurrentAdoptedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSuitesWithAdoption scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuitesWithAdoption iter: %w", err)
	}
	return out, nil
}

// GetCacheStats returns the five DB-derived numeric fields the
// SPEC5 §10.5 status-page cache.* block needs: blob_count,
// total_bytes, url_path_count, zero_refcount_backlog, and
// actually_reapable_blobs. Called by both the status-page handler
// (per-render) and the §9.7.6 refresher goroutine (every
// admin.gauge_refresh).
//
// AIDEV-NOTE: COUNT(*) on blob and url_path is sub-millisecond at
// the row-counts a typical deployment carries (tens of
// thousands), and SUM(size) FROM blob runs from the b-tree leaf
// scan SQLite already maintains for the table. The
// zero-refcount-backlog query uses idx_blob_gc (SPEC4 §4.3) which
// is built exactly for this reachability filter.
//
// AIDEV-NOTE: actually_reapable_blobs applies the full GC reap
// predicate (refcount<=0 + refcount_zeroed_at IS NOT NULL + three
// NOT EXISTS reachability clauses against url_path/snapshot_member/
// suite_snapshot — see RunBlobGCBatch). It does NOT apply the
// grace-period cutoff (gc.blob_grace) so the gauge surfaces the
// operationally-meaningful "GC would consider these on its next
// tick" set, not "GC will definitely reap on its next tick" (which
// also depends on heartbeat-stale grace, batch size, and the
// per-tick deadline). The difference between zero_refcount_backlog
// and actually_reapable_blobs is the count of blobs whose refcount
// column drifted to 0 (a denormalized hint that gets out-of-sync
// because url_path INSERTs on cache-miss do not bump refcount) but
// which remain reachable via url_path / snapshot_member /
// suite_snapshot — those bytes are NOT slack, they are still
// servable from the cache.
//
// Returns (CacheStats, nil) on success. Per-query errors bubble up
// individually so callers can decide on partial-data behavior;
// the §9.7.6 refresher logs and keeps the prior gauge value, the
// status page returns 503 (SPEC5 §9.7.3).
func (c *Cache) GetCacheStats(ctx context.Context) (CacheStats, error) {
	var s CacheStats
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(size), 0) FROM blob`,
	).Scan(&s.BlobCount, &s.TotalBytes); err != nil {
		return CacheStats{}, fmt.Errorf("GetCacheStats blob: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM url_path`,
	).Scan(&s.URLPathCount); err != nil {
		return CacheStats{}, fmt.Errorf("GetCacheStats url_path: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM blob
		   WHERE refcount <= 0 AND refcount_zeroed_at IS NOT NULL`,
	).Scan(&s.ZeroRefcountBacklog); err != nil {
		return CacheStats{}, fmt.Errorf("GetCacheStats zero_refcount: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM blob
		   WHERE refcount <= 0
		     AND refcount_zeroed_at IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM url_path
		                      WHERE blob_hash = blob.hash)
		     AND NOT EXISTS (SELECT 1 FROM snapshot_member
		                      WHERE blob_hash = blob.hash)
		     AND NOT EXISTS (SELECT 1 FROM suite_snapshot
		                      WHERE inrelease_hash   = blob.hash
		                         OR release_hash     = blob.hash
		                         OR release_gpg_hash = blob.hash)`,
	).Scan(&s.ActuallyReapableBlobs); err != nil {
		return CacheStats{}, fmt.Errorf("GetCacheStats actually_reapable: %w", err)
	}
	return s, nil
}

// GetRepoCoverage returns the SPEC6_5 §2.4 repo_coverage payload
// (architectures observed, source/pdiff snapshot counts, per-kind
// package_hash row totals). All counts are scoped to current snapshots
// — the join against suite_freshness.current_snapshot_id ensures
// displaced snapshots' rows don't inflate the totals.
//
// AIDEV-NOTE: the per-kind row classifier mirrors handler.classifyPath:
// architecture="source" + non-pdiff path → kind=source; path under
// Packages.diff/ or Sources.diff/ → kind=pdiff; everything else with a
// non-empty architecture → kind=binary. Architecture-empty rows
// (legacy adoptions before SPEC3 v3, or future kind extensions) fall
// into the "other" bucket and are excluded from the binary tally.
//
// AIDEV-NOTE: pdiff path predicates use SQLite GLOB rather than LIKE so
// they match the case-sensitive serve-time classifier in handler.go.
// SQLite's default LIKE is ASCII case-insensitive, which would let a
// lowercase "packages.diff/" path slip into the pdiff bucket here while
// classifyPath would not call it pdiff at serve time.
//
// AIDEV-NOTE: the four reads run inside a single read-only transaction
// so a concurrent CommitAdoption between statements cannot make the
// architectures_seen / snapshot counts / row totals describe different
// moments. Without the transaction, callers could observe a row total
// that is internally inconsistent with the architectures list.
//
// AIDEV-NOTE: this method runs on the SPEC5 §9.7.6 refresher
// goroutine (admin.refreshRepoCoverage), NOT on every /?format=json
// request. The renderer reads from an atomic.Pointer that the
// refresher Store()s after each recompute — values can be up to
// admin.gauge_refresh (default 30s) stale, but per-kind row counts
// only change at adoption-time (snapshot flip), so the staleness is
// operationally fine. The refresher also feeds the SPEC6_5 §10.3
// acu_package_hash_rows_by_kind gauge from the same Snapshot, so the
// JSON and the Prometheus exposition are always in sync.
func (c *Cache) GetRepoCoverage(ctx context.Context) (RepoCoverage, error) {
	var r RepoCoverage

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// architectures_seen: distinct arch values across current snapshots'
	// package_hash rows, excluding empty (Phase 2 pre-v3 rows have ""
	// arch and shouldn't surface).
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT p.architecture
FROM package_hash p
JOIN suite_freshness sf
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
WHERE p.architecture != ''
ORDER BY p.architecture`)
	if err != nil {
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage architectures_seen: %w", err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			_ = rows.Close()
			return RepoCoverage{}, fmt.Errorf("GetRepoCoverage architectures_seen scan: %w", err)
		}
		r.ArchitecturesSeen = append(r.ArchitecturesSeen, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage architectures_seen iter: %w", err)
	}
	_ = rows.Close()

	// snapshots_with_sources: current snapshots having >=1 row with
	// architecture=source.
	if err := tx.QueryRowContext(ctx, `
SELECT count(DISTINCT sf.current_snapshot_id)
FROM package_hash p
JOIN suite_freshness sf
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
WHERE p.architecture = 'source'`).Scan(&r.SnapshotsWithSources); err != nil {
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage snapshots_with_sources: %w", err)
	}

	// snapshots_with_pdiff: current snapshots having >=1 *.diff/Index
	// in snapshot_member. GLOB (case-sensitive) used so a lowercase
	// "packages.diff/index" cannot inflate the count.
	if err := tx.QueryRowContext(ctx, `
SELECT count(DISTINCT sf.current_snapshot_id)
FROM snapshot_member m
JOIN suite_freshness sf
  ON sf.current_snapshot_id = m.snapshot_id
WHERE m.path GLOB '*/Packages.diff/Index'
   OR m.path GLOB '*/Sources.diff/Index'`).Scan(&r.SnapshotsWithPdiff); err != nil {
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage snapshots_with_pdiff: %w", err)
	}

	// package_hash_rows by kind: bucket on the same predicate as
	// handler.classifyPath. The CASE order matters: pdiff paths live
	// under /<thing>.diff/ regardless of arch label, so the pdiff
	// branch must check the path BEFORE the arch=source branch (a
	// hypothetical Sources.diff/<patch>.gz would otherwise be
	// double-counted as both source and pdiff). GLOB is used (rather
	// than LIKE) for case-sensitivity parity with handler.classifyPath.
	rowsByKind, err := tx.QueryContext(ctx, `
SELECT
  CASE
    WHEN p.path GLOB '*/Packages.diff/*' OR p.path GLOB '*/Sources.diff/*' THEN 'pdiff'
    WHEN p.architecture = 'source' THEN 'source'
    WHEN p.architecture != '' THEN 'binary'
    ELSE 'other'
  END AS kind,
  count(*) AS n
FROM package_hash p
JOIN suite_freshness sf
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
GROUP BY kind`)
	if err != nil {
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage rows_by_kind: %w", err)
	}
	for rowsByKind.Next() {
		var kind string
		var n int64
		if err := rowsByKind.Scan(&kind, &n); err != nil {
			_ = rowsByKind.Close()
			return RepoCoverage{}, fmt.Errorf("GetRepoCoverage rows_by_kind scan: %w", err)
		}
		switch kind {
		case "binary":
			r.PackageHashRowsBinary = n
		case "source":
			r.PackageHashRowsSource = n
		case "pdiff":
			r.PackageHashRowsPdiff = n
		}
		// "other" rows (empty arch, non-pdiff path) are not surfaced
		// to keep the §2.4 contract clean (binary/source/pdiff/total).
		// They DO contribute to total below.
		r.PackageHashRowsTotal += n
	}
	if err := rowsByKind.Err(); err != nil {
		_ = rowsByKind.Close()
		return RepoCoverage{}, fmt.Errorf("GetRepoCoverage rows_by_kind iter: %w", err)
	}
	_ = rowsByKind.Close()

	return r, nil
}

// GetCacheSummaryByHostArch returns one CacheSummaryEntry per
// (canonical_host, architecture) tuple observed in current snapshots'
// package_hash rows. SPEC6_5 §2.4 cache_summary.by_host[*].by_architecture.
//
// Two queries run inside one read-only transaction so the
// package_hash_count and the blob_count/blob_bytes describe the same
// instant (a concurrent CommitAdoption between them could otherwise
// surface internally-inconsistent (count, bytes) pairs for a given
// (host, arch)).
//
// Query 1: package_hash row count per (host, arch). Counts every row,
// including ones whose path has no url_path yet — operationally
// "how many paths the snapshot vouches for under this arch".
//
// Query 2: distinct (blob_hash, blob_size) pairs reachable from the
// (host, arch)'s package_hash rows through the url_path → blob join.
// The DISTINCT pre-aggregate inside the subquery ensures a blob
// referenced by multiple package_hash rows in the same (host, arch)
// bucket is counted ONCE — without it, count(*) and sum(size) would
// over-count by row multiplicity. A package_hash row whose path has
// no url_path (URL never requested, no blob yet) does not appear in
// Query 2 at all; its (host, arch) bucket still exists from Query 1
// with zero BlobCount / BlobBytes.
//
// AIDEV-NOTE: pdiff path rows DO contribute to their architecture's
// bucket here — the (host, arch) attribution is more useful to
// operators than splitting pdiff/source out a second time (the
// repo_coverage block already breaks rows down by kind). The SPEC6_5
// §2.4 example shows "amd64", "arm64", "source" as the keys; pdiff
// rows under binary-<arch>/Packages.diff/<ts>.gz fold into their
// <arch> bucket, matching the package_hash.architecture column value.
//
// AIDEV-NOTE: rows with architecture="" (pre-v3 package_hash rows
// adopted before the SPEC3 v3 schema gained the column) are excluded
// from both queries — they cannot be attributed to any arch bucket
// without rewinding to the adoption that produced them. These rows
// fall off naturally as their snapshots are displaced.
func (c *Cache) GetCacheSummaryByHostArch(ctx context.Context) (map[string]map[string]CacheSummaryEntry, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("GetCacheSummaryByHostArch begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	out := map[string]map[string]CacheSummaryEntry{}
	getEntry := func(host, arch string) CacheSummaryEntry {
		if m, ok := out[host]; ok {
			return m[arch]
		}
		return CacheSummaryEntry{}
	}
	setEntry := func(host, arch string, e CacheSummaryEntry) {
		m, ok := out[host]
		if !ok {
			m = map[string]CacheSummaryEntry{}
			out[host] = m
		}
		m[arch] = e
	}

	rows, err := tx.QueryContext(ctx, `
SELECT p.canonical_host, p.architecture, count(*)
FROM package_hash p
JOIN suite_freshness sf
  ON sf.canonical_scheme = p.canonical_scheme
 AND sf.canonical_host   = p.canonical_host
 AND sf.current_snapshot_id = p.snapshot_id
WHERE p.architecture != ''
GROUP BY p.canonical_host, p.architecture`)
	if err != nil {
		return nil, fmt.Errorf("GetCacheSummaryByHostArch counts: %w", err)
	}
	for rows.Next() {
		var host, arch string
		var n int64
		if err := rows.Scan(&host, &arch, &n); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("GetCacheSummaryByHostArch counts scan: %w", err)
		}
		e := getEntry(host, arch)
		e.PackageHashCount = n
		setEntry(host, arch, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("GetCacheSummaryByHostArch counts iter: %w", err)
	}
	_ = rows.Close()

	// AIDEV-NOTE: drive this join from url_path, NOT package_hash.
	// package_hash holds one row per .deb per snapshot (≈1M rows on a
	// full Ubuntu mirror), while url_path holds only the artifacts
	// actually cached (low thousands), and the inner join to url_path is
	// what gates the result. Left to its own row-count estimates SQLite
	// drives from package_hash and SCANs all ~1M rows, probing url_path
	// per row — ~1.5s in C sqlite and >10s under the pure-Go modernc
	// driver, blowing the §9.7.6 gauge-refresh deadline (the
	// cache_summary panel then stays perpetually empty). The CROSS JOIN
	// pins url_path as the outer table so SQLite probes package_hash by
	// its PK prefix (canonical_scheme, canonical_host, path) instead —
	// ~40× faster with byte-identical results. The `blob_hash IS NOT
	// NULL` predicate lets the partial index idx_url_path_blob drive the
	// scan; it is also semantically a no-op (the JOIN to blob already
	// drops NULL-blob rows). Do not reorder these joins without
	// re-checking EXPLAIN QUERY PLAN against a large cache.
	blobRows, err := tx.QueryContext(ctx, `
SELECT canonical_host, architecture,
       count(*) AS blob_count,
       COALESCE(sum(size), 0) AS blob_bytes
FROM (
    SELECT DISTINCT p.canonical_host, p.architecture, b.hash, b.size
    FROM url_path u
    JOIN blob b ON b.hash = u.blob_hash
    CROSS JOIN package_hash p
      ON p.canonical_scheme = u.canonical_scheme
     AND p.canonical_host   = u.canonical_host
     AND p.path             = u.path
    JOIN suite_freshness sf
      ON sf.canonical_scheme = p.canonical_scheme
     AND sf.canonical_host   = p.canonical_host
     AND sf.current_snapshot_id = p.snapshot_id
    WHERE u.blob_hash IS NOT NULL AND p.architecture != ''
)
GROUP BY canonical_host, architecture`)
	if err != nil {
		return nil, fmt.Errorf("GetCacheSummaryByHostArch blobs: %w", err)
	}
	for blobRows.Next() {
		var host, arch string
		var bc, bb int64
		if err := blobRows.Scan(&host, &arch, &bc, &bb); err != nil {
			_ = blobRows.Close()
			return nil, fmt.Errorf("GetCacheSummaryByHostArch blobs scan: %w", err)
		}
		e := getEntry(host, arch)
		e.BlobCount = bc
		e.BlobBytes = bb
		setEntry(host, arch, e)
	}
	if err := blobRows.Err(); err != nil {
		_ = blobRows.Close()
		return nil, fmt.Errorf("GetCacheSummaryByHostArch blobs iter: %w", err)
	}
	_ = blobRows.Close()

	return out, nil
}

// GetSuiteStats returns the suite/snapshot count triple the SPEC5
// §9.7.6 refresher feeds into the acu_suites_tracked /
// acu_snapshots_current / acu_snapshots_displaced gauges. Three
// cheap COUNT(*) queries against suite_freshness and suite_snapshot.
//
// AIDEV-NOTE: AdoptedTotal counts every suite_snapshot row whose
// adopted_at IS NOT NULL — including snapshots no longer current
// (i.e. displaced). The displaced gauge is computed in the caller as
// AdoptedTotal - WithCurrentSnapshot, which equals the number of
// snapshots still on disk that are no longer the current adoption
// for their suite.
func (c *Cache) GetSuiteStats(ctx context.Context) (SuiteStats, error) {
	var s SuiteStats
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM suite_freshness`,
	).Scan(&s.Tracked); err != nil {
		return SuiteStats{}, fmt.Errorf("GetSuiteStats tracked: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM suite_freshness WHERE current_snapshot_id IS NOT NULL`,
	).Scan(&s.WithCurrentSnapshot); err != nil {
		return SuiteStats{}, fmt.Errorf("GetSuiteStats current: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*) FROM suite_snapshot WHERE adopted_at IS NOT NULL`,
	).Scan(&s.AdoptedTotal); err != nil {
		return SuiteStats{}, fmt.Errorf("GetSuiteStats adopted: %w", err)
	}
	return s, nil
}

// ListHotURLPaths returns up to `limit` url_path rows ordered by
// request_count DESC then last_requested_at DESC. Used by the
// SPEC5 §10.5 status-page hot_url_paths section. Rows whose
// last_requested_at IS NULL are excluded — they have not been
// requested since the row was seeded.
//
// AIDEV-NOTE: limit is enforced by SQL LIMIT, so the result set
// is bounded regardless of url_path table size. Phase 5 status
// page caps at 20 (SPEC5 §10.5); a future endpoint may want a
// bigger limit, hence the parameter rather than a hard-coded 20.
//
// AIDEV-NOTE: the schema indexes url_path(last_requested_at) but
// not the compound (request_count DESC, last_requested_at DESC)
// this query orders by — SQLite picks idx_url_path_last_req for
// the WHERE filter and sorts the matching rows in-memory. At
// typical url_path table sizes (tens of thousands) this is a
// sub-millisecond sort. SPEC5 §4.3 commits to no schema changes
// in Phase 5; a Phase 6 partial covering index
// (idx_url_path_hot ON url_path(request_count DESC,
// last_requested_at DESC) WHERE last_requested_at IS NOT NULL)
// would eliminate the sort if profiling at multi-million-row scale
// shows it matters. The §9.7.3 5s per-query timeout caps the worst
// case in any event.
func (c *Cache) ListHotURLPaths(ctx context.Context, limit int) ([]HotURLPath, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
SELECT canonical_host, path, is_metadata, request_count, last_requested_at
  FROM url_path
 WHERE last_requested_at IS NOT NULL
 ORDER BY request_count DESC, last_requested_at DESC
 LIMIT ?`
	rows, err := c.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("ListHotURLPaths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []HotURLPath
	for rows.Next() {
		var h HotURLPath
		if err := rows.Scan(&h.Host, &h.Path, &h.IsMetadata,
			&h.RequestCount, &h.LastRequestedAtUnix); err != nil {
			return nil, fmt.Errorf("ListHotURLPaths scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListHotURLPaths iter: %w", err)
	}
	return out, nil
}

// ListSuites returns every suite_freshness row, in no guaranteed order.
// Used by the periodic freshness scheduler (SPEC §7.4) to pick suites
// whose last_success_at is older than periodic_refresh.
//
// AIDEV-NOTE: this is a full-table scan with no LIMIT. Phase 1 deployments
// are expected to track tens — at worst low hundreds — of suites
// (canonical_host × suite_codename), so the scan is cheap. If a future
// phase pushes this number into the thousands, the caller should switch
// to a chunked scan keyed on (canonical_scheme, canonical_host, suite_path).
func (c *Cache) ListSuites(ctx context.Context) ([]SuiteFreshness, error) {
	const q = `
SELECT canonical_scheme, canonical_host, suite_path,
       last_check_at, last_success_at,
       inrelease_etag, inrelease_lastmod, inrelease_change_seen_at,
       current_snapshot_id
FROM suite_freshness`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ListSuites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteFreshness
	for rows.Next() {
		var s SuiteFreshness
		if err := rows.Scan(
			&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
			&s.LastCheckAt, &s.LastSuccessAt,
			&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
			&s.CurrentSnapshotID,
		); err != nil {
			return nil, fmt.Errorf("ListSuites scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSuites iter: %w", err)
	}
	return out, nil
}

// ComputeHotSet runs the SPEC3 §7.5.3 two-stage hot-set query. Stage 1
// finds (package_name, architecture) tuples that the *prior* snapshot
// covered AND that have a hot url_path row (last_requested_at within
// the hot window). Stage 2 resolves those tuples against the
// candidate's in-memory `[]PackageHash` rows — they are not yet in
// the DB (CommitAdoption inserts them transactionally with the
// current_snapshot_id flip), so the freshness adopter passes them
// here directly. Returns the hot set ordered deterministically by
// (package_name, architecture) so the SPEC3 §7.5 step-10 prefetch
// loop visits entries in a predictable sequence — needed by the
// §12.3 chaos tests to pin which deb is FIRST vs LAST in iteration
// when budget elapses mid-loop.
//
// candidateSnapshotID is required so Stage 2 can validate the
// caller-supplied rows belong to the snapshot being adopted. The old
// SQL form scoped Stage 2 by snapshot_id implicitly; the in-memory
// form must enforce the same invariant explicitly. A row whose
// (CanonicalScheme, CanonicalHost, SnapshotID) does not match the
// (scheme, host, candidateSnapshotID) inputs is a programming error
// — return ErrHotSetCandidateMismatch so a future misuse fails loud
// rather than warming the wrong rows.
//
// Empty inputs are handled gracefully:
//   - priorSnapshotID == 0: returns an empty slice (a fresh suite with
//     no prior adoption has nothing to mine).
//   - hotWindow == 0: returns an empty slice (operator disabled hot
//     prefetch).
//   - Stage-1 produced no hot pairs: returns an empty slice without
//     building the Stage-2 map.
//   - candidatePackageHashes empty / no (Package, Arch) match: drops
//     the unmatchable pair from the hot set.
//
// Pre-v3 package_hash rows in the DB are excluded automatically —
// they have empty package_name / architecture columns; Stage 1's
// predicate filters them out. Pre-v3 entries in the candidate slice
// are also skipped (Stage 2 only indexes non-empty (Package, Arch)
// keys). The first post-upgrade adoption populates name+arch on its
// candidate's rows; hot prefetch first kicks in on the *second*
// snapshot transition after the v2→v3 migration.
func (c *Cache) ComputeHotSet(ctx context.Context,
	scheme, host string,
	priorSnapshotID int64,
	candidateSnapshotID int64,
	candidatePackageHashes []PackageHash,
	hotWindowSeconds int64,
	now int64,
	maxVersionsPerPackage int) ([]HotSetEntry, error) {
	if priorSnapshotID == 0 || hotWindowSeconds == 0 {
		return nil, nil
	}
	const stage1Q = `
SELECT DISTINCT ph.package_name, ph.architecture
  FROM package_hash ph
  JOIN url_path up
    ON up.canonical_scheme = ph.canonical_scheme
   AND up.canonical_host   = ph.canonical_host
   AND up.path             = ph.path
 WHERE ph.snapshot_id        = ?
   AND ph.package_name      <> ''
   AND ph.architecture      <> ''
   AND up.last_requested_at IS NOT NULL
   AND up.last_requested_at >= ?
 ORDER BY ph.package_name, ph.architecture`
	hotSince := now - hotWindowSeconds
	rows, err := c.db.QueryContext(ctx, stage1Q, priorSnapshotID, hotSince)
	if err != nil {
		return nil, fmt.Errorf("ComputeHotSet stage 1: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type pkgArch struct {
		pkg, arch string
	}
	var pairs []pkgArch
	for rows.Next() {
		var pa pkgArch
		if err := rows.Scan(&pa.pkg, &pa.arch); err != nil {
			return nil, fmt.Errorf("ComputeHotSet stage 1 scan: %w", err)
		}
		pairs = append(pairs, pa)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ComputeHotSet stage 1 iter: %w", err)
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	// Stage 2: build an index of the candidate's in-memory rows by
	// (Package, Arch) and resolve each Stage-1 hot pair. SPEC3 §7.5.3
	// Stage 2 is `SELECT path, declared_sha256 ... WHERE (package_name,
	// architecture) IN (...)` — every matching row is returned, so a
	// single hot pair can map to multiple debPaths and all of them get
	// prefetched. Within a single candidate the same debPath cannot
	// appear twice (buildPackageHashes dedups by Filename), but two
	// distinct paths sharing one (Package, Arch) is allowed.
	type key struct{ pkg, arch string }
	candIdx := make(map[key][]PackageHash, len(candidatePackageHashes))
	for _, ph := range candidatePackageHashes {
		if ph.CanonicalScheme != scheme ||
			ph.CanonicalHost != host ||
			ph.SnapshotID != candidateSnapshotID {
			return nil, fmt.Errorf("%w: row {%s/%s/%d} does not match candidate {%s/%s/%d}",
				ErrHotSetCandidateMismatch,
				ph.CanonicalScheme, ph.CanonicalHost, ph.SnapshotID,
				scheme, host, candidateSnapshotID)
		}
		if ph.PackageName == "" || ph.Architecture == "" {
			continue
		}
		k := key{ph.PackageName, ph.Architecture}
		candIdx[k] = append(candIdx[k], ph)
	}
	out := make([]HotSetEntry, 0, len(pairs))
	for _, pa := range pairs {
		matches, ok := candIdx[key(pa)]
		if !ok {
			// Hot pair (Package, Arch) is no longer in the candidate
			// snapshot — upstream removed the package between
			// snapshots. Cannot prefetch a path that doesn't exist;
			// drop from the hot set.
			continue
		}
		// Sort within-pair matches by path so the budget-limited hot
		// loop's chop is deterministic. The candidate slice arrives in
		// adoption.go's `dedup` map iteration order, which Go
		// randomizes; without this sort, two adoptions of the same
		// Release could warm a different subset of paths under the
		// same budget. Stage 1 already returns pairs ordered by
		// (package_name, architecture) — pairing that with
		// path-sorted matches makes the whole hot set deterministic.
		if len(matches) > 1 {
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].Path < matches[j].Path
			})
		}
		// Version-aware retention §4: warm only the newest N distinct
		// versions of this hot (Package, Arch). A fat-index repo lists
		// hundreds of versions of one package in a single snapshot;
		// warming them all is the leak. The kept-version set matches the
		// §3 retention mirror rule so prefetch and GC agree on "kept".
		// Version-less rows (malformed binary stanza) are NOT prefetched —
		// they have no rankable version and warming many of them would
		// reopen the leak; the GC mirror rule likewise won't retain them.
		versions := make([]string, 0, len(matches))
		for _, ph := range matches {
			if ph.Version == "" {
				continue
			}
			versions = append(versions, ph.Version)
		}
		keep := keepNewestNVersionSet(versions, maxVersionsPerPackage)
		for _, ph := range matches {
			if ph.Version == "" {
				continue
			}
			if _, ok := keep[ph.Version]; !ok {
				continue
			}
			out = append(out, HotSetEntry{Path: ph.Path, DeclaredSHA256: ph.DeclaredSHA256})
		}
	}
	return out, nil
}

// keepNewestNVersionSet returns the set of raw version strings belonging to
// the newest n Debian-version EQUIVALENCE CLASSES among the input. dpkg
// considers some textually-distinct strings equal (e.g. "1.0" == "1.0-0" ==
// "1.00"); those must share a single slot of n and BOTH be retained — not
// consume two slots, and not have one arbitrarily reaped while the other is
// kept. Sorting is deterministic (Debian order, then raw-string tie-break)
// so the boundary selection is stable across runs. n < 1 is clamped to 1 so
// the caller never accidentally keeps/warms nothing.
//
// Shared by ComputeHotSet (bounded prefetch) and the url_path GC mirror rule
// so prefetch and retention agree on exactly which versions are "kept".
func keepNewestNVersionSet(versions []string, n int) map[string]struct{} {
	if n < 1 {
		n = 1
	}
	seen := make(map[string]struct{}, len(versions))
	distinct := make([]string, 0, len(versions))
	for _, v := range versions {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		distinct = append(distinct, v)
	}
	sort.Slice(distinct, func(i, j int) bool {
		if c := debversion.Compare(distinct[i], distinct[j]); c != 0 {
			return c > 0 // newest first
		}
		return distinct[i] < distinct[j] // stable tie-break for equal versions
	})
	keep := make(map[string]struct{}, len(distinct))
	classes := 0
	for i, v := range distinct {
		if i > 0 && debversion.Compare(distinct[i-1], v) != 0 {
			classes++ // crossed into a new Debian-equivalence class
		}
		if classes >= n {
			break
		}
		keep[v] = struct{}{}
	}
	return keep
}

// HostCurrentSnapshotsCoverage returns one row per (scheme, host)
// suite whose suite_freshness.current_snapshot_id is non-NULL — the
// snapshots presently in force on this host. Each row carries the
// snapshot id and its package_coverage_complete bit (SPEC3 §4.3.1,
// §7.5.4). The handler's SPEC3 §6.1 strict-mode predicate uses this
// to decide whether to refuse unvouched .deb requests (every current
// snapshot must be coverage_complete = 1) or fall through to
// trust-upstream and log unvouched_deb_passthrough_no_coverage.
//
// Returns an empty slice + nil error when (scheme, host) has no
// adopted suites (every suite_freshness row's current_snapshot_id is
// NULL). The strict-mode predicate treats that case as "no snapshot
// is the contract on this host" — falls through to trust-upstream.
func (c *Cache) HostCurrentSnapshotsCoverage(ctx context.Context, scheme, host string) ([]SnapshotCoverage, error) {
	const q = `
SELECT ss.snapshot_id, ss.package_coverage_complete
  FROM suite_snapshot ss
  JOIN suite_freshness sf
    ON sf.canonical_scheme   = ss.canonical_scheme
   AND sf.canonical_host     = ss.canonical_host
   AND sf.suite_path         = ss.suite_path
   AND sf.current_snapshot_id = ss.snapshot_id
 WHERE ss.canonical_scheme = ?
   AND ss.canonical_host   = ?`
	rows, err := c.db.QueryContext(ctx, q, scheme, host)
	if err != nil {
		return nil, fmt.Errorf("HostCurrentSnapshotsCoverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotCoverage
	for rows.Next() {
		var (
			sc       SnapshotCoverage
			coverage int64
		)
		if err := rows.Scan(&sc.SnapshotID, &coverage); err != nil {
			return nil, fmt.Errorf("HostCurrentSnapshotsCoverage scan: %w", err)
		}
		sc.PackageCoverageComplete = coverage != 0
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("HostCurrentSnapshotsCoverage iter: %w", err)
	}
	return out, nil
}

// PutSuiteFreshness upserts the per-suite freshness state.
func (c *Cache) PutSuiteFreshness(ctx context.Context, s SuiteFreshness) error {
	const q = `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at,
   inrelease_etag, inrelease_lastmod, inrelease_change_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  last_check_at            = excluded.last_check_at,
  last_success_at          = excluded.last_success_at,
  inrelease_etag           = excluded.inrelease_etag,
  inrelease_lastmod        = excluded.inrelease_lastmod,
  inrelease_change_seen_at = excluded.inrelease_change_seen_at`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q,
			s.CanonicalScheme, s.CanonicalHost, s.SuitePath,
			s.LastCheckAt, s.LastSuccessAt,
			s.InReleaseETag, s.InReleaseLastMod, s.InReleaseChangeSeenAt)
		if err != nil {
			return fmt.Errorf("PutSuiteFreshness: %w", err)
		}
		return nil
	})
}

// ListCachedDebs returns every cached .deb whose filename basename
// contains substring (empty = all). Rows are deduped by
// (Filename, BlobHash): when the same .deb basename resolves to the
// same blob through multiple canonical hosts (mirrors sharing content),
// one row is returned listing every host in Hosts. Different blob
// hashes under the same filename surface as separate rows. Output is
// sorted by (Filename, BlobHash).
//
// Read-only; safe to call alongside the running daemon — the underlying
// query is a single SELECT over url_path JOIN blob and runs against the
// shared pool of readers (SQLite WAL).
//
// Only url_path rows with a non-NULL blob_hash AND a path ending in
// ".deb" (case-sensitive GLOB) are considered, matching the
// management-command scope decision.
func (c *Cache) ListCachedDebs(ctx context.Context, substring string) ([]CachedDeb, error) {
	rows, err := c.queryCachedDebRows(ctx)
	if err != nil {
		return nil, err
	}
	return collapseCachedDebs(rows, func(filename string) bool {
		return substring == "" || strings.Contains(filename, substring)
	}), nil
}

// LookupCachedDebByName returns every cached .deb whose filename
// basename equals filename exactly. Same dedup rules as ListCachedDebs.
// Used by `packages copy` to resolve the user's exact-name argument:
//
//   - len(result) == 0 → not cached.
//   - len(result) == 1 → unambiguous; copy the named blob.
//   - len(result) >  1 → same filename resolved to multiple distinct
//     blob hashes across hosts (upstream divergence); caller surfaces
//     the ambiguity rather than guessing.
//
// Read-only.
func (c *Cache) LookupCachedDebByName(ctx context.Context, filename string) ([]CachedDeb, error) {
	rows, err := c.queryCachedDebRows(ctx)
	if err != nil {
		return nil, err
	}
	return collapseCachedDebs(rows, func(f string) bool {
		return f == filename
	}), nil
}

// cachedDebRow is a single (host, path, blob_hash, size, created_at)
// tuple from the SELECT below — collapsed in Go into one CachedDeb per
// (basename, blob_hash) group.
type cachedDebRow struct {
	Host      string
	Path      string
	BlobHash  string
	Size      int64
	CreatedAt int64
}

// queryCachedDebRows runs the .deb-filter SELECT and returns the raw
// per-host rows. SQLite has no convenient basename function, so the
// extraction + dedup happens in Go. The result set is bounded by the
// number of cached .deb files (typically thousands, occasionally tens
// of thousands) — well within the size where an in-process group-by is
// cheaper than a SQLite UDF.
func (c *Cache) queryCachedDebRows(ctx context.Context) ([]cachedDebRow, error) {
	const q = `
SELECT u.canonical_host, u.path, u.blob_hash, b.size, b.created_at
  FROM url_path u
  JOIN blob b ON b.hash = u.blob_hash
 WHERE u.blob_hash IS NOT NULL
   AND u.path GLOB '*.deb'`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("queryCachedDebRows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []cachedDebRow
	for rows.Next() {
		var r cachedDebRow
		if err := rows.Scan(&r.Host, &r.Path, &r.BlobHash, &r.Size, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("queryCachedDebRows scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queryCachedDebRows iter: %w", err)
	}
	return out, nil
}

// collapseCachedDebs groups raw rows by (basename, blob_hash) and
// applies the filename predicate. Hosts within a group are deduped and
// sorted; the final slice is sorted by (Filename, BlobHash).
func collapseCachedDebs(rows []cachedDebRow, keep func(filename string) bool) []CachedDeb {
	type key struct{ filename, hash string }
	groups := make(map[key]*CachedDeb)
	for _, r := range rows {
		name := path.Base(r.Path)
		if !keep(name) {
			continue
		}
		k := key{name, r.BlobHash}
		g, ok := groups[k]
		if !ok {
			g = &CachedDeb{
				Filename:  name,
				BlobHash:  r.BlobHash,
				Size:      r.Size,
				CreatedAt: r.CreatedAt,
			}
			groups[k] = g
		}
		g.Hosts = append(g.Hosts, r.Host)
	}
	out := make([]CachedDeb, 0, len(groups))
	for _, g := range groups {
		// Dedup hosts: a single (host, path) is the url_path PK so
		// duplicates can only arise from defensive callers, but
		// sort+compact here keeps Hosts canonical regardless.
		sort.Strings(g.Hosts)
		g.Hosts = compactSortedStrings(g.Hosts)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Filename != out[j].Filename {
			return out[i].Filename < out[j].Filename
		}
		return out[i].BlobHash < out[j].BlobHash
	})
	return out
}

// compactSortedStrings removes consecutive duplicates from a sorted
// slice in place. Cheaper than building a set when the typical input
// has 1–3 entries.
func compactSortedStrings(s []string) []string {
	if len(s) < 2 {
		return s
	}
	w := 1
	for i := 1; i < len(s); i++ {
		if s[i] != s[w-1] {
			s[w] = s[i]
			w++
		}
	}
	return s[:w]
}
