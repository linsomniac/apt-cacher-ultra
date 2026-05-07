package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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

// PutBlob inserts a blob row. If a row with this hash already exists, it
// is left untouched (created_at and refcount must stay stable). Use this
// after a successful BlobWriter.Finalize.
//
// The schema also CHECKs that hash matches sha256-hex shape, so this Go
// validation is defense-in-depth. We surface ErrInvalidHash before
// submitting to the writer goroutine so a buggy caller fails immediately.
func (c *Cache) PutBlob(ctx context.Context, hash string, size int64) error {
	if !validBlobHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	const q = `
INSERT INTO blob (hash, size, created_at, refcount)
VALUES (?, ?, ?, 0)
ON CONFLICT(hash) DO NOTHING`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, hash, size, now)
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
	defer rows.Close()
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
	now int64) ([]HotSetEntry, error) {
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
	defer rows.Close()

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
		matches, ok := candIdx[key{pa.pkg, pa.arch}]
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
		for _, ph := range matches {
			out = append(out, HotSetEntry{Path: ph.Path, DeclaredSHA256: ph.DeclaredSHA256})
		}
	}
	return out, nil
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
	defer rows.Close()
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
