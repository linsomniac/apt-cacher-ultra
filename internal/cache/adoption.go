package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// ErrSnapshotAlreadyAdopted is returned by CommitAdoption when invoked
// on a snapshot whose adopted_at column is already set. Re-committing
// would double-bump refcounts for every member, so the writer refuses.
var ErrSnapshotAlreadyAdopted = errors.New("cache: snapshot already adopted")

// ErrSnapshotRetired: the candidate was retired — by the legacy trust
// audit's quarantine, or the reaper's retirement path (a reap, POST
// /forget, cap eviction) — and must never become current.
var ErrSnapshotRetired = errors.New("cache: snapshot is retired")

// ErrUnsignedDowngradeRefused is the never-downgrade refusal of an unsigned
// candidate whose suite has signed history (spec C5 safeguard 3,
// refuseUnsignedDowngrade): CommitAdoption's recheck inside the writer
// transaction returns it with nothing written, and CheckUnsignedDowngrade
// returns it for the early exits. A refusal state, not a failure: the
// adopter records it as unsigned_downgrade_refused.
var ErrUnsignedDowngradeRefused = errors.New("cache: unsigned adoption refused: the suite has signed history")

// ErrSnapshotNaturalKeyAdopted is returned by InsertCandidateSnapshot
// when a non-retired row matching the natural key (scheme, host,
// suite_path, COALESCE(inrelease_hash, release_hash), anchor_form)
// already exists *and* is already adopted (adopted_at IS NOT NULL). The adoption pipeline
// surfaces this as a distinct WARN; auto-reusing an adopted row is
// out of scope (would bypass the lifecycle and refcount accounting
// in CommitAdoption).
// It arrives wrapped in a *NaturalKeyAdoptedError (errors.Is matches it; errors.As recovers the snapshot id).
var ErrSnapshotNaturalKeyAdopted = errors.New("cache: snapshot natural key already adopted")

// NaturalKeyAdoptedError names the adopted snapshot a candidate collided
// with, so the adopter can tell a rollback to a displaced snapshot (spec
// C5 rollback_refused) from a race with the current one.
type NaturalKeyAdoptedError struct{ SnapshotID int64 }

func (e *NaturalKeyAdoptedError) Error() string {
	return fmt.Sprintf("%v: snapshot_id=%d", ErrSnapshotNaturalKeyAdopted, e.SnapshotID)
}

func (e *NaturalKeyAdoptedError) Unwrap() error { return ErrSnapshotNaturalKeyAdopted }

// sqliteConstraintUnique is the extended result code SQLITE_CONSTRAINT_UNIQUE.
// We use the literal here rather than depending on modernc.org/sqlite/lib
// for one constant — the value is fixed by SQLite's public ABI.
const sqliteConstraintUnique = 2067

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint
// failure. Used by InsertCandidateSnapshot to recover an existing
// candidate id on natural-key collision instead of bailing.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code() == sqliteConstraintUnique
}

// HeartbeatSnapshot updates suite_snapshot.heartbeat_at to "now" for the
// given candidate snapshot id. SPEC4 §7.5.2 sites 2–5: the adoption
// goroutine calls this after each member fetch, after Packages parsing,
// after each hot-prefetch deb fetch, and immediately before
// CommitAdoption. A periodic ticker (site 6) runs in parallel.
//
// The UPDATE matches zero rows when the candidate has been reaped (or its
// id never existed); that is benign — the next heartbeat or the
// successful CommitAdoption restores liveness, or the row stays gone.
// Heartbeat-write failures are logged at adoption_heartbeat_failed Warn
// by the caller; they never abort the adoption.
//
// Runs as a small standalone write outside any larger transaction; it
// does not block on or serialize with CommitAdoption's atomic flip.
func (c *Cache) HeartbeatSnapshot(ctx context.Context, snapshotID int64) error {
	const q = `UPDATE suite_snapshot SET heartbeat_at = ? WHERE snapshot_id = ?`
	now := nowUnix()
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, q, now, snapshotID)
		if err != nil {
			return fmt.Errorf("HeartbeatSnapshot: %w", err)
		}
		return nil
	})
}

// candidateForm derives (or validates) the anchor form of a candidate.
// Inline and unsigned use the single-anchor slot (InReleaseHash);
// detached uses Release + Release.gpg.
//
// AIDEV-NOTE: deriving an empty Form exists for the unambiguous shapes
// only (test fixtures and legacy callers). FormUnsigned is NEVER derived:
// an unsigned candidate that forgot its Form would be stored as inline
// and vouch .debs. Production candidate construction (runShared) always
// sets Form, and phase E's tests assert that an unsigned adoption stores
// "unsigned".
func candidateForm(sc SnapshotCandidate) (AnchorForm, error) {
	single := sc.InReleaseHash != nil && sc.ReleaseHash == nil && sc.ReleaseGPGHash == nil
	detached := sc.InReleaseHash == nil && sc.ReleaseHash != nil && sc.ReleaseGPGHash != nil
	switch sc.Form {
	case "":
		if single {
			return FormInline, nil
		}
		if detached {
			return FormDetached, nil
		}
	case FormInline, FormUnsigned:
		if single {
			return sc.Form, nil
		}
	case FormDetached:
		if detached {
			return FormDetached, nil
		}
	}
	return "", fmt.Errorf("%w: form=%q", ErrInvalidAnchorForm, sc.Form)
}

// InsertCandidateSnapshot inserts a fresh suite_snapshot row with
// adopted_at = NULL and returns the auto-assigned snapshot_id. SPEC2
// §7.5 step 4. Caller uses the returned id to key the snapshot_member
// and package_hash rows it builds during prefetch (steps 5-8), then
// finalizes via CommitAdoption (step 9 / §7.5.1).
//
// Phase 4 (SPEC4 §7.5.2 site 1): heartbeat_at is set to created_at on
// insert and refreshed on the reused-orphan path. The orphan-candidate
// reap predicate keys on heartbeat_at, so a candidate row that exists
// from this INSERT through the eventual CommitAdoption flip remains
// "alive" only as long as the adoption goroutine keeps writing
// heartbeats — the row's age alone cannot keep it alive (which is the
// whole point: a stalled adoption with no heartbeats ages out).
//
// All non-nil *Hash fields must point at blob.hash values the caller
// has already persisted via PutBlob; the FK constraints fail closed
// otherwise. candidateForm checks the hash shape against sc.Form before
// any SQL runs: both modes, all-NULL, or a Form that does not fit the
// hashes returns ErrInvalidAnchorForm. The schema CHECK on
// suite_snapshot still enforces the "exactly one of single-anchor or
// detached" invariant at the DB layer, and the v7 trigger rejects any
// row without an anchor_form.
//
// AIDEV-NOTE: idempotent on natural-key collision — reuses an existing
// unadopted candidate row so a Step-5/6/7/8 failure in runShared does
// not poison subsequent adoption attempts. The natural key
// (scheme, host, suite_path, COALESCE(inrelease_hash, release_hash),
// anchor_form) among non-retired rows (spec §6; the partial unique
// index idx_suite_snapshot_natural_v7 ... WHERE retired_at IS NULL)
// already requires "one live row per (suite, content, form)"; this
// method makes the API surface that fact instead of choking with a
// UNIQUE error. The collision lookup carries the same predicates as
// the index: without anchor_form an unsigned candidate could reuse an
// inline row with the same hash (and vouch .debs); without
// retired_at IS NULL it could match a retired incarnation.
// reused == true on the second-and-later attempts at the same content;
// callers can log it once for diagnostics. CommitAdoption is its own
// transaction guarded by adopted_at IS NULL, so retrying with a reused
// snapshot_id is safe — there can be no leftover snapshot_member rows
// from a partial CommitAdoption (it rolls back on any internal error).
// If a row matching the natural key is already adopted, we return
// ErrSnapshotNaturalKeyAdopted instead — reusing an adopted row would
// bypass the snapshot lifecycle and refcount bookkeeping.
func (c *Cache) InsertCandidateSnapshot(ctx context.Context, sc SnapshotCandidate) (id int64, reused bool, err error) {
	form, err := candidateForm(sc)
	if err != nil {
		return 0, false, err
	}
	const insertQ = `
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, inrelease_etag, inrelease_lastmod,
   release_hash, release_gpg_hash, anchor_form, release_date, created_at, adopted_at,
   package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`
	const lookupQ = `
SELECT snapshot_id, adopted_at FROM suite_snapshot
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
   AND COALESCE(inrelease_hash, release_hash) = ?
   AND anchor_form = ?
   AND retired_at IS NULL`
	now := nowUnix()
	coverage := int64(0)
	if sc.PackageCoverageComplete {
		coverage = 1
	}
	// AIDEV-NOTE: the op writes gotID/gotReused, never the named results:
	// on a ctx/close path submitWrite returns before the writer goroutine
	// finishes the op (cache.go submitWrite), and `return 0, false, werr`
	// would then race the op's writes. Read them only after a nil werr
	// (the RescheduleFailingAdoptions pattern). The adoption deadline
	// (adoption.max_duration) makes that path reachable mid-op.
	var (
		gotID     int64
		gotReused bool
	)
	werr := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		res, execErr := conn.ExecContext(ctx, insertQ,
			sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath,
			sc.InReleaseHash, sc.InReleaseETag, sc.InReleaseLastMod,
			sc.ReleaseHash, sc.ReleaseGPGHash, string(form), sc.ReleaseDate, now, coverage, now)
		if execErr == nil {
			gotID, execErr = res.LastInsertId()
			if execErr != nil {
				return fmt.Errorf("InsertCandidateSnapshot: last id: %w", execErr)
			}
			return nil
		}
		if !isUniqueViolation(execErr) {
			return fmt.Errorf("InsertCandidateSnapshot: %w", execErr)
		}
		// Natural-key collision: the schema CHECK guarantees exactly one
		// of inrelease_hash or release_hash is non-nil. Use the populated
		// one to look up the existing row.
		var coalesceHash string
		switch {
		case sc.InReleaseHash != nil:
			coalesceHash = *sc.InReleaseHash
		case sc.ReleaseHash != nil:
			coalesceHash = *sc.ReleaseHash
		default:
			// No hash supplied — collision came from somewhere else (or
			// the caller violated the inline-xor-detached invariant).
			// Surface the original error untouched.
			return fmt.Errorf("InsertCandidateSnapshot: %w", execErr)
		}
		var (
			existingID int64
			adoptedAt  sql.NullInt64
		)
		lookupErr := conn.QueryRowContext(ctx, lookupQ,
			sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath, coalesceHash, string(form)).
			Scan(&existingID, &adoptedAt)
		if lookupErr != nil {
			return fmt.Errorf("InsertCandidateSnapshot: lookup colliding row: %w (orig: %v)",
				lookupErr, execErr)
		}
		if adoptedAt.Valid {
			return &NaturalKeyAdoptedError{SnapshotID: existingID}
		}
		// Refresh mutable, non-natural-key columns so a retry that
		// supplies (e.g.) a re-signed Release.gpg or fresher validators
		// is reflected on the reused row. Without this, in detached
		// mode the same Release bytes paired with a different fresh
		// Release.gpg on retry would commit a snapshot_member row at
		// "Release.gpg" pointing at the new sig hash, while the
		// suite_snapshot.release_gpg_hash column still recorded the
		// old (failed-attempt) signature — an internal inconsistency.
		// inrelease_hash / release_hash are part of the natural key
		// and cannot have changed (we only got here because they
		// matched); leave them. release_date (spec C5) is refreshed
		// alongside the validators for the same reason: a retry may
		// have parsed a Date the earlier attempt could not.
		// SPEC4 §7.5.2 site 1 (reused-orphan path): refresh heartbeat_at
		// alongside the other mutable columns. Without this, a reused
		// candidate inherits its prior incarnation's heartbeat_at; if
		// that value is past grace, the very next snapshot-GC tick can
		// reap the freshly-restarted adoption.
		const refreshQ = `
UPDATE suite_snapshot
   SET release_gpg_hash   = ?,
       inrelease_etag     = ?,
       inrelease_lastmod  = ?,
       release_date       = ?,
       heartbeat_at       = ?
 WHERE snapshot_id = ?`
		if _, err := conn.ExecContext(ctx, refreshQ,
			sc.ReleaseGPGHash, sc.InReleaseETag, sc.InReleaseLastMod, sc.ReleaseDate, now,
			existingID); err != nil {
			return fmt.Errorf("InsertCandidateSnapshot: refresh mutable cols on reuse: %w", err)
		}
		gotID = existingID
		gotReused = true
		return nil
	})
	if werr != nil {
		return 0, false, werr
	}
	return gotID, gotReused, nil
}

// PrefetchedURLPath carries one Phase 3 hot-deb prefetch outcome into
// CommitAdoption. The atomic flip transaction inserts (or upserts) a
// url_path row pointing at the warmed pool blob *inside the same
// transaction* that flips current_snapshot_id, so readers never observe
// a warmed deb whose blob_hash is visible while the prior snapshot is
// still current. SPEC3 §7.5.1.
type PrefetchedURLPath struct {
	CanonicalScheme string
	CanonicalHost   string
	Path            string // canonical .deb path
	BlobHash        string // sha256 hex of the warmed pool blob
	UpstreamURL     string
}

// CommitAdoption performs the SPEC2 §7.5.1 atomic flip transaction:
//
//  1. Verify the snapshot is still a candidate (adopted_at IS NULL).
//  2. Insert all snapshot_member rows for snapshotID.
//  3. Insert all package_hash rows.
//     3a. (Phase 3, SPEC3 §7.5.1) Insert/upsert url_path rows for the
//     hot debs warmed during prefetch. Hotness signal preservation:
//     DO UPDATE intentionally omits last_requested_at and
//     request_count, diverging from PutURLPath, so the next
//     adoption's hot-set query still sees the path as hot.
//     3b. (Phase 3) Stamp suite_snapshot.package_coverage_complete from
//     the caller's coverageComplete bit — set under the same
//     transaction as the flip so readers see coverage atomically
//     with the snapshot becoming current.
//  4. Bump blob.refcount for each distinct blob_hash referenced by the
//     new snapshot's members. Each blob is counted once even if many
//     member rows point at it.
//  5. Read the prior current_snapshot_id from suite_freshness (NULL if
//     this is the first adoption for the suite).
//  6. UPDATE suite_freshness.current_snapshot_id to snapshotID, set the
//     row's validators to the candidate's, and clear
//     inrelease_change_seen_at and the adoption failure record (spec C4,
//     C7). UPDATE-only (spec C3): no row rolls everything back with
//     ErrSuiteGone.
//  7. Stamp suite_snapshot.adopted_at (adoptionStampTx: max(now, prior + 1)).
//  8. If prior_id was non-NULL, decrement refcount for each distinct
//     blob_hash referenced by the prior snapshot's members. A blob
//     shared between old and new nets to zero (+1 then -1) so the
//     bookkeeping is correct under stable membership.
//
// All steps run in a single SQLite transaction on the writer
// connection. Either every reader after COMMIT sees the new snapshot
// (and every member it implies), or every reader sees the prior — no
// half-flipped state is observable.
//
// Members list each (path, blob_hash, declared_sha256) triple to
// insert. Caller is responsible for ensuring unique paths within the
// list; the snapshot_member primary key (snapshot_id, path) will
// surface duplicates as a constraint error and abort the transaction.
//
// Skipped lists the SPEC6_7 §2 skipped-member records — declared
// Release members the adoption did NOT fetch — inserted into
// snapshot_skipped_member atomically with the flip (same rationale as
// coverage in Step 3b: no "snapshot is current but its skip record is
// not yet visible" mid-state for the repair pass to mis-read). Pass
// nil when the adoption skipped nothing or the caller does not record
// skips.
//
// PackageHashes is the per-.deb declared-hash assertion set. Pass an
// empty slice for suites with no .debs (e.g. metadata-only suites).
//
// PrefetchedURLPaths is the SPEC3 §7.5.1 hot-prefetch outcome list.
// Pass nil (Phase 2 callers, or Phase 3 callers whose hot-prefetch
// loop produced no successfully warmed debs) for the unchanged Phase 2
// flip behavior.
//
// CoverageComplete is the SPEC3 §7.5.4 per-snapshot coverage proof for
// the strict-mode predicate (§6.1) to key on. Phase 2 callers and
// pre-/dists/-layout adoptions pass false.
//
// AIDEV-NOTE: this is the load-bearing transaction of Phase 2 (and
// Phase 3). Any SQL error inside the body causes a Rollback — no
// partial flip can be observed by any reader. The prior_id lookup
// happens *inside* the transaction so a concurrent flip cannot race
// the bookkeeping.
func (c *Cache) CommitAdoption(ctx context.Context, snapshotID int64,
	members []SnapshotMember, skipped []SkippedMember, packageHashes []PackageHash,
	prefetchedURLPaths []PrefetchedURLPath, coverageComplete bool) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("CommitAdoption: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Step 1: candidate state guard. Read suite identification too —
		// CommitAdoption needs canonical_scheme/host/suite_path to flip
		// the suite_freshness pointer, and reading them from the row is
		// strictly safer than trusting them from a separate caller
		// argument.
		var (
			scheme, host, suite, form string
			adoptedAt, retiredAt      sql.NullInt64
			candETag, candLastMod     sql.NullString
		)
		err = tx.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path, anchor_form, adopted_at, retired_at,
       inrelease_etag, inrelease_lastmod
FROM suite_snapshot
WHERE snapshot_id = ?`, snapshotID).Scan(&scheme, &host, &suite, &form, &adoptedAt, &retiredAt, &candETag, &candLastMod)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("CommitAdoption: snapshot %d not found", snapshotID)
		case err != nil:
			return fmt.Errorf("CommitAdoption: read candidate: %w", err)
		}
		if adoptedAt.Valid {
			return ErrSnapshotAlreadyAdopted
		}
		// AIDEV-NOTE: a retired snapshot (quarantined by the legacy trust
		// audit, spec §6.1, or reaped from phase F) must never become
		// current again — its pins were released when it was retired.
		if retiredAt.Valid {
			return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotRetired, snapshotID)
		}

		// Step 1a (spec C5 safeguard 3, never-downgrade): an unsigned
		// candidate is refused, before anything is written, when the
		// suite has signed history (refuseUnsignedDowngrade: a signed-seen
		// marker for its base, or an adopted signed snapshot, under either
		// scheme).
		//
		// AIDEV-NOTE: this in-transaction re-read is the authority; the
		// check's eligibility read and the adopter's are only early exits
		// (CheckUnsignedDowngrade, the same rule). The suite mutex cannot
		// close the race on its own: the handler and the checker write
		// markers in their own writer operations, and the /. twin shares
		// the base without sharing the mutex. The writer is FIFO, so a
		// marker write enqueued before this commit is visible here
		// (TestStoreFetched_MarkerPrecedesALaterWriterOperation; the
		// adopter's re-mark before an unsigned commit, ruling R-E1).
		if AnchorForm(form) == FormUnsigned {
			if err := refuseUnsignedDowngrade(ctx, tx, host, suite); err != nil {
				// The prefix tells this authoritative read from the early
				// exits' (deferred minor E-4); errors.Is still finds
				// ErrUnsignedDowngradeRefused.
				return fmt.Errorf("CommitAdoption: %w", err)
			}
		}

		// Step 2: insert snapshot_member rows. Each member is validated
		// against its declared invariants before we touch SQLite — a bad
		// row should fail loud, not be smuggled past hash validation.
		for _, m := range members {
			if !validBlobHash(m.BlobHash) {
				return fmt.Errorf("CommitAdoption: member %q blob_hash %w", m.Path, ErrInvalidHash)
			}
			if !validBlobHash(m.DeclaredSHA256) {
				return fmt.Errorf("CommitAdoption: member %q declared_sha256 %w", m.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256)
VALUES (?, ?, ?, ?)`,
				snapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("CommitAdoption: insert member %q: %w", m.Path, err)
			}
		}

		// Step 2a (SPEC6_7 §2): record skipped members. The declared
		// sha256 is validated for shape here too — it is the trust
		// anchor a later repair fetch verifies bytes against, so a
		// malformed value must abort the flip, not ride along.
		skipNow := nowUnix()
		for _, s := range skipped {
			if !validBlobHash(s.DeclaredSHA256) {
				return fmt.Errorf("CommitAdoption: skipped member %q declared_sha256 %w", s.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_skipped_member
  (snapshot_id, path, declared_sha256, size, reason, detail, skipped_at, retry_count)
VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
				snapshotID, s.Path, s.DeclaredSHA256, s.Size, s.Reason, s.Detail, skipNow); err != nil {
				return fmt.Errorf("CommitAdoption: insert skipped member %q: %w", s.Path, err)
			}
		}

		// Step 3: insert package_hash rows. Phase 3 (SPEC3 §4.3.1)
		// added package_name and architecture columns; rows whose
		// stanza didn't declare both end up with empty strings, which
		// the SPEC3 §7.5.3 hot-set query filters out.
		for _, p := range packageHashes {
			if !validBlobHash(p.DeclaredSHA256) {
				return fmt.Errorf("CommitAdoption: package_hash %q declared_sha256 %w", p.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO package_hash (canonical_scheme, canonical_host, path,
                          declared_sha256, snapshot_id,
                          package_name, architecture, version, flat_filename)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.CanonicalScheme, p.CanonicalHost, p.Path,
				p.DeclaredSHA256, snapshotID,
				p.PackageName, p.Architecture, p.Version, p.FlatFilename); err != nil {
				return fmt.Errorf("CommitAdoption: insert package_hash %q: %w", p.Path, err)
			}
		}

		now := nowUnix()
		// Spec C8 (monotonic Last-Modified): the adoption stamp, taken
		// before Step 3c re-seeds the url_path anchor rows with now.
		stamp, err := adoptionStampTx(ctx, tx, scheme, host, suite, now)
		if err != nil {
			return fmt.Errorf("CommitAdoption: %w", err)
		}

		// Step 3a (Phase 3, SPEC3 §7.5.1): insert/upsert url_path rows
		// for the hot debs warmed during the SPEC3 §7.5 prefetch loop.
		// This **deliberately diverges** from PutURLPath
		// (putURLPathSQL, queries.go): the DO UPDATE omits
		// last_requested_at and request_count so the existing row's
		// hotness signal survives the upsert. Hot prefetch is a
		// cache-warming write, not a client-served write; overwriting the
		// hotness columns here would erase the very evidence that made
		// this path hot in the first place, dropping it from the next
		// adoption's hot-set computation (deferred minor C-14 moved the
		// stamp above this comment).
		for _, up := range prefetchedURLPaths {
			if !validBlobHash(up.BlobHash) {
				return fmt.Errorf("CommitAdoption: prefetched %q blob_hash %w", up.Path, ErrInvalidHash)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES (?, ?, ?, ?, ?, 0, NULL, 0, ?, NULL, NULL)
ON CONFLICT(canonical_scheme, canonical_host, path) DO UPDATE SET
  blob_hash       = excluded.blob_hash,
  upstream_url    = excluded.upstream_url,
  last_fetched_at = excluded.last_fetched_at,
  -- Re-warming re-establishes the row with a fresh blob; clear any
  -- hold-grace drop stamp so it isn't reaped at a stale pre-re-warm
  -- deadline (matches PutURLPath/TouchURLPath). last_requested_at /
  -- request_count are still intentionally omitted to preserve hotness.
  dropped_at      = NULL`,
				up.CanonicalScheme, up.CanonicalHost, up.Path,
				up.BlobHash, up.UpstreamURL, now); err != nil {
				return fmt.Errorf("CommitAdoption: prefetched url_path %q: %w", up.Path, err)
			}
		}

		// Step 3b (Phase 3, SPEC3 §7.5.4): stamp coverage on the
		// candidate row before the flip. Atomic with the suite_freshness
		// pointer flip below — strict mode (§6.1) reads
		// package_coverage_complete via the current snapshot, so making
		// this write part of the same transaction prevents a brief
		// "current snapshot exists but coverage column not yet set"
		// window from leaking into the strict-mode predicate.
		coverageInt := int64(0)
		if coverageComplete {
			coverageInt = 1
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET package_coverage_complete = ?
 WHERE snapshot_id = ?`,
			coverageInt, snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: stamp coverage: %w", err)
		}

		// Step 3c (freshness-freeze fix, SPEC3 §7.5.1 / SPEC2 §7.4):
		// seed/sync the metadata anchor url_path row(s) — InRelease
		// (inline), Release + Release.gpg (detached) or Release
		// (unsigned) — to point at the blob this snapshot adopted.
		// Historically adoption left the anchor's blob_hash pointing at
		// whatever bytes a client cache-miss last stored, so it diverged
		// from the snapshot's anchor hash and the SPEC4 §5 GC guards
		// (b)/(c) could never vouch for it; the anchor was then reaped on
		// a low-traffic lull and the suite froze (the checker silently
		// skips when the anchor is absent, SPEC2 §7.4). Syncing it here
		// keeps the hash-based guards matching — the SPEC4 §5 identity
		// guard (d) is the belt to this suspenders.
		//
		// AIDEV-NOTE: ON CONFLICT preserves the existing (port-correct)
		// upstream_url, last_requested_at, and request_count — only
		// blob_hash / is_metadata / last_fetched_at are (re)written, plus
		// dropped_at cleared to NULL (re-establish-clears-hold invariant).
		// Mirrors Step 3a's hotness-preservation rationale. The INSERT
		// branch (anchor was already reaped) reconstructs upstream_url
		// with aptpath.BuildUpstreamURL(scheme, host, path), which is
		// port-less (SPEC §3.2) — acceptable because it only fires when
		// the original row is already gone.
		//
		// AIDEV-NOTE: the files seeded are the form's anchors
		// (aptpath.AnchorPaths, anchorHash): an unsigned snapshot keeps
		// its Release hash in inrelease_hash, and its anchor is
		// <suite>/Release — seeding <suite>/InRelease with it would serve
		// a Release body as an InRelease (spec C5, phase E).
		var anchorInRel, anchorRel, anchorRelGPG *string
		if err := tx.QueryRowContext(ctx, `
SELECT inrelease_hash, release_hash, release_gpg_hash
  FROM suite_snapshot WHERE snapshot_id = ?`, snapshotID).
			Scan(&anchorInRel, &anchorRel, &anchorRelGPG); err != nil {
			return fmt.Errorf("CommitAdoption: read anchor hashes: %w", err)
		}
		seedAnchor := func(name string) error {
			h := anchorHash(AnchorForm(form), name, anchorInRel, anchorRel, anchorRelGPG)
			if h == nil || *h == "" {
				return nil
			}
			anchorPath := suite + "/" + name
			_, err := tx.ExecContext(ctx, `
INSERT INTO url_path
  (canonical_scheme, canonical_host, path, blob_hash, upstream_url,
   is_metadata, last_requested_at, request_count, last_fetched_at,
   upstream_etag, upstream_lastmod)
VALUES (?, ?, ?, ?, ?, 1, NULL, 0, ?, NULL, NULL)
ON CONFLICT(canonical_scheme, canonical_host, path) DO UPDATE SET
  blob_hash       = excluded.blob_hash,
  is_metadata     = 1,
  last_fetched_at = excluded.last_fetched_at,
  -- Re-synced anchor points at fresh bytes; clear any drop stamp so the
  -- invariant "re-establishing a row clears hold-grace" holds at every
  -- url_path upsert site (the metadata guards already protect anchors,
  -- so this is defense-in-depth, not the primary keep).
  dropped_at      = NULL`,
				scheme, host, anchorPath, *h,
				aptpath.BuildUpstreamURL(scheme, host, anchorPath), now)
			return err
		}
		for _, name := range aptpath.AnchorPaths(form) {
			if err := seedAnchor(name); err != nil {
				return fmt.Errorf("CommitAdoption: seed %s anchor: %w", name, err)
			}
		}

		// Step 4 (SPEC4 §7.5.1 Rule 2): bump refcount for blobs
		// referenced by the new snapshot. The IN-subquery dedupes
		// blob_hash values automatically: each blob row matches once
		// regardless of how many member rows point at it.
		//
		// IIF clears refcount_zeroed_at only on the strictly-positive
		// crossing — a -1 → 0 bump leaves the existing
		// refcount_zeroed_at intact so the grace clock continues from
		// where it was, while a 0 → 1 (or -1 → 0 followed by 0 → 1
		// — but each UPDATE sees the post-update value) bump clears
		// the column. The check is on `refcount + 1` (the post-update
		// value) so SQLite evaluates it against the row's pre-update
		// refcount and decides correctly which side of zero we land on.
		if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?)`,
			snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: bump new refcounts: %w", err)
		}

		// Step 5: read prior current_snapshot_id from suite_freshness. A
		// missing row leaves prior NULL here and fails Step 6.
		var prior sql.NullInt64
		err = tx.QueryRowContext(ctx, `
SELECT current_snapshot_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			scheme, host, suite).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("CommitAdoption: read prior: %w", err)
		}

		// Step 6: flip the pointer and make the row describe the adopted
		// anchor (spec C4 "Validators"): its validators become the
		// candidate's, the pending-change stamp and the adoption failure
		// record are cleared (spec C4, C7). Check and success stamps,
		// check_fail_count and last_requested_at stay as they are.
		//
		// AIDEV-NOTE: UPDATE-only (spec C3). A suite whose row is gone
		// rolls the whole flip back with ErrSuiteGone: an adoption never
		// resurrects a suite that cap eviction (or phase F's reaper)
		// deleted, and createSuiteTx stays the only INSERT.
		res, err := tx.ExecContext(ctx, `
UPDATE suite_freshness
   SET current_snapshot_id      = ?,
       inrelease_etag           = ?,
       inrelease_lastmod        = ?,
       inrelease_change_seen_at = NULL,
       `+clearAdoptionFailureSet+`
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			snapshotID, candETag, candLastMod, scheme, host, suite)
		if err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		} else if n == 0 {
			return fmt.Errorf("%w: %s://%s%s", ErrSuiteGone, scheme, host, suite)
		}

		// Step 7: mark the new snapshot adopted with the spec C8 stamp:
		// `now` (the commit moment Step 3a also uses) unless the previous
		// anchor's stamp is not older, which keeps a client's
		// If-Modified-Since from hiding the flip.
		if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`,
			stamp, snapshotID); err != nil {
			return fmt.Errorf("CommitAdoption: stamp adopted_at: %w", err)
		}

		// Step 7a (spec C3): a signed snapshot records signed-seen history
		// for its base in the same transaction, so every binary from phase
		// A on keeps the never-downgrade authority current even before the
		// handler writes markers (phase D).
		if AnchorForm(form) == FormInline || AnchorForm(form) == FormDetached {
			if err := markSignedSeen(ctx, tx, scheme, host, aptpath.MarkerBase(suite), now); err != nil {
				return fmt.Errorf("CommitAdoption: %w", err)
			}
		}

		// Step 8 (SPEC4 §7.5.1 Rule 3): decrement refcounts for blobs
		// the prior snapshot pinned. A blob shared between old and new
		// gets +1 then -1 (net 0) — exactly the bookkeeping Phase 4 GC
		// relies on. The decrement itself (and its refcount_zeroed_at
		// rule) lives in decrementSnapshotRefcountsTx, shared with the
		// legacy trust audit's quarantine.
		if prior.Valid {
			if err := decrementSnapshotRefcountsTx(ctx, tx, prior.Int64, now); err != nil {
				return fmt.Errorf("CommitAdoption: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CommitAdoption: commit: %w", err)
		}
		return nil
	})
}

// decrementSnapshotRefcountsTx releases one pin per distinct blob the
// snapshot's members reference (SPEC4 §7.5.1 Rule 3). It must run exactly
// once per snapshot, when it stops being current: CommitAdoption Step 8
// (displacement), the legacy trust audit's quarantine (spec §6.1) and the
// reaper's retirement path (retireSuiteSnapshotsTx: cap eviction, the
// reaper and POST /forget, spec C9) are its callers.
//
// AIDEV-NOTE: COALESCE keeps an existing refcount_zeroed_at on a 0 → -1
// transition; the IIF writes `now` only on the first <= 0 crossing.
func decrementSnapshotRefcountsTx(ctx context.Context, tx *sql.Tx, snapshotID, now int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount - 1,
       refcount_zeroed_at = COALESCE(
         refcount_zeroed_at,
         IIF(refcount - 1 <= 0, ?, NULL)
       )
 WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?)`,
		now, snapshotID); err != nil {
		return fmt.Errorf("decrement refcounts of snapshot %d: %w", snapshotID, err)
	}
	return nil
}

// adoptionStampTx returns the adopted_at a commit at now stamps (spec C8,
// monotonic Last-Modified): max(now, the newest adopted_at of any snapshot
// of the suite + 1) — current, displaced or retired, while it is still
// there — and, for a suite with no current snapshot (a first adoption, or
// one after a quarantine or a reap), also above the newest last_fetched_at
// of the suite's InRelease/Release/Release.gpg url_path rows + 1 — those
// rows are what clients were served, with that Last-Modified or older.
// The handler serves snapshot anchors with adopted_at as their
// Last-Modified (never the pool file's mtime), so a client's
// If-Modified-Since from the previous anchor gets a 200 after a flip,
// whatever the clock does — within the residual below.
//
// AIDEV-NOTE: call it before CommitAdoption Step 3c, which re-seeds the
// anchor rows' last_fetched_at with now. adopted_at can now lead the wall
// clock (by 1 s per same-second flip, by up to a backward step's length
// after one), so the floor is every snapshot of the suite, not only the
// current one (deferred minor C-11): once current_snapshot_id is cleared
// (a quarantine) or the row deleted (a reap, an eviction, POST /forget —
// the re-created suite starts without one), the suite's remaining
// snapshots may lead the clock, and a stamp below theirs would misorder
// GC's keep_displaced ranking and retained by-hash's newest-first window,
// and could answer a client's If-Modified-Since from a retired snapshot
// 304. Stamps are strictly increasing per suite, so those readers need
// only their order; the legacy audit's markSnapshotBaseTx
// (legacy_audit.go) reads one as a wall-clock stamp, and the clock-step
// rule (internal/freshness/clockstep.go, spec C4) lists adopted_at as a
// past-event stamp — no gate reads it that way, so a stamp in the future
// is harmless there. Residual (the Task 5 review's minor 1): the floor
// covers only the snapshots still present. Sub-job C deletes a retired
// snapshot at a periodic GC tick (normally the next; a startup GC pass
// deletes none: ruling PF-15), a reap or /forget deletes the anchor rows,
// and Step 3c seeds them with now, so after a backward step longer than
// the time from a reap (or /forget) to the re-adoption, with that periodic
// tick in between, the re-adoption can stamp below the reaped snapshot
// again and a client holding that anchor gets a 304 until a later flip is
// stamped above it. C-11's own readers do not care then (the row is
// gone); a durable per-suite floor needs a schema change, which phase F
// forbids.
//
// AIDEV-NOTE: ruling R-G3 (phase G). url_path-served metadata answers
// Last-Modified = max(pool mtime, last_fetched_at) (the handler's
// urlPathModTime), so the first-adoption floor below (the anchors'
// MAX(last_fetched_at) + 1) passes every url_path anchor's Last-Modified
// only while no pool file is newer than its row's last_fetched_at:
// runFetch and the anchorless refresh (RefreshURLPathBlob) finalize the
// pool file before they stamp the row. Keep that order; blob mtimes are
// not folded into this floor
// (TestServeHTTP_AMissStampsItsRowAfterItsPoolFile).
func adoptionStampTx(ctx context.Context, tx *sql.Tx, scheme, host, suite string, now int64) (int64, error) {
	var newest, current sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT MAX(adopted_at) FROM suite_snapshot
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?),
       (SELECT current_snapshot_id FROM suite_freshness
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?)`,
		scheme, host, suite, scheme, host, suite).Scan(&newest, &current); err != nil {
		return 0, fmt.Errorf("read the suite's newest adopted_at: %w", err)
	}
	stamp := now
	if newest.Valid {
		stamp = max(stamp, newest.Int64+1)
	}
	if current.Valid {
		return stamp, nil
	}
	var fetched sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT MAX(last_fetched_at) FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path IN (?, ?, ?)`,
		scheme, host, suite+"/InRelease", suite+"/Release", suite+"/Release.gpg").Scan(&fetched); err != nil {
		return 0, fmt.Errorf("read anchor last_fetched_at: %w", err)
	}
	if fetched.Valid {
		stamp = max(stamp, fetched.Int64+1)
	}
	return stamp, nil
}

// ListSnapshotMembers returns every snapshot_member row for the snapshot,
// in no guaranteed order. Used by tests and adoption diagnostics.
func (c *Cache) ListSnapshotMembers(ctx context.Context, snapshotID int64) ([]SnapshotMember, error) {
	const q = `
SELECT snapshot_id, path, blob_hash, declared_sha256
FROM snapshot_member
WHERE snapshot_id = ?`
	rows, err := c.db.QueryContext(ctx, q, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("ListSnapshotMembers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotMember
	for rows.Next() {
		var m SnapshotMember
		if err := rows.Scan(&m.SnapshotID, &m.Path, &m.BlobHash, &m.DeclaredSHA256); err != nil {
			return nil, fmt.Errorf("ListSnapshotMembers scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSnapshotMembers iter: %w", err)
	}
	return out, nil
}

// GetPackageHash returns the package_hash row for (scheme, host, path,
// snapshot_id), or ErrNotFound. Hot path: §6.5 .deb hash validation.
func (c *Cache) GetPackageHash(ctx context.Context, scheme, host, path string, snapshotID int64) (*PackageHash, error) {
	const q = `
SELECT canonical_scheme, canonical_host, path, declared_sha256, snapshot_id
FROM package_hash
WHERE canonical_scheme = ? AND canonical_host = ? AND path = ? AND snapshot_id = ?`
	var p PackageHash
	err := c.db.QueryRowContext(ctx, q, scheme, host, path, snapshotID).Scan(
		&p.CanonicalScheme, &p.CanonicalHost, &p.Path, &p.DeclaredSHA256, &p.SnapshotID,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetPackageHash: %w", err)
	}
	return &p, nil
}

// DeclaredHash is the (snapshot_id, declared_sha256) result of the §6.1 /
// §6.2 declared-hash query: every distinct hash that any current snapshot
// asserts for the .deb at (scheme, host, path), with one snapshot id per
// row. The same .deb path can legitimately appear in multiple suites'
// snapshots (e.g. noble + noble-updates referencing the same package), so
// callers must surface the conflict — fail closed rather than picking one
// — when more than one *distinct* declared_sha256 is returned.
type DeclaredHash struct {
	DeclaredSHA256 string
	SnapshotID     int64
	// PackageName is the package_hash.package_name column (Phase 3 v3),
	// surfaced for SPEC6_5 §2.3 per-request log emission. Empty when
	// the row was inserted before v3 (legacy adoption) or for entries
	// whose source format does not carry a package name (e.g. pdiff
	// patches). Callers must tolerate the empty case.
	PackageName string
	// SuitePath is the suite whose current snapshot the row belongs to
	// (spec C3: a vouched .deb serve touches that suite).
	SuitePath string
}

// DeclaredHashesForPath returns every package_hash row covering this .deb
// path under any signed (inline or detached) current snapshot — an
// unsigned one never vouches (deferred minor E-2). Joins package_hash →
// suite_freshness on current_snapshot_id so a row whose snapshot was
// displaced (orphaned rows from a prior adoption) does not count. SPEC2
// §6.1 step 2 / §6.2.
//
// Returns rows in no defined order. Empty slice + nil error means "no
// snapshot covers this path" — Phase 1 trust-upstream regime.
//
// AIDEV-NOTE: this is the shared helper between §6.1 (hit-path validation)
// and §6.2 (miss-path validation). Both call sites need declared+snapshot
// pairs (the snapshot id is required for fail-closed conflict logging).
// The DISTINCT clause is over (declared_sha256, snapshot_id), so two
// snapshots that *agree* on a hash still produce two rows — one per
// snapshot. The handler-side helper distinctDeclared collapses those
// duplicate hashes when classifying "0 / 1 / 2+ distinct hashes" for
// the §6.1 hit-path dispatch; the conflict log surface shows every row
// (snapshot id included) so an operator can trace which adoptions
// disagreed.
//
// Spec C6: the exact lookup runs first, and only when it finds no row the
// suffix fallback (declaredHashesBySuffix) looks the path up by a flat
// suite's flat_filename — apt requests a flat repository's .deb at
// <uri>/<Filename>, <uri> any ancestor-or-self of the Release directory.
//
// AIDEV-NOTE: the fallback is a constraint lookup, not a grant: a row it
// finds only lets bytes through that hash to a signed declaration, and it
// never runs when an exact row exists (so a dists suite's vouching is
// unchanged). Its cost — one partial-index query of at most 32 names — is
// paid by every non-metadata request no exact row covers, including every
// .deb of a host with no flat suite (an empty index range per name): on
// the hit path once, on a miss twice (the pre-fetch gate and the
// post-fetch dispatch), and once more for a strict-mode re-run after an
// adoption wait.
func (c *Cache) DeclaredHashesForPath(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	out, err := c.declaredHashesExact(ctx, scheme, host, path)
	if err != nil || len(out) > 0 {
		return out, err
	}
	return c.declaredHashesBySuffix(ctx, scheme, host, path)
}

// declaredExactSQL is the exact lookup's query (spec C6 step 1): the
// current signed snapshots' rows of (scheme, host) whose path is the
// request path, with the row's suite path.
//
// AIDEV-NOTE: spec C5 safeguard 5 — an unsigned snapshot never vouches
// a .deb. Adoption builds no package_hash rows for one; the form
// filter keeps any that exist out (defence in depth).
//
// AIDEV-NOTE: design review M-2 (phase G). Both lookups (this query and
// declaredBySuffixSQL) join suite_freshness on its primary key — the
// snapshot's own suite, read from suite_snapshot — and current_snapshot_id,
// never on current_snapshot_id alone: that column has no index, so the
// bare join scanned suite_freshness once per matched row (up to
// max_suites rows for each current, displaced or retired snapshot that
// declares the name). A snapshot belongs to its suite_snapshot row's
// suite, so the key terms change no answer.
// TestDeclaredHashesForPath_TheExactLookupProbesSuiteFreshnessByItsKey and
// TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex pin both plans.
const declaredExactSQL = `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path
  FROM package_hash p
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
  JOIN suite_freshness sf ON sf.canonical_scheme    = ss.canonical_scheme
                         AND sf.canonical_host      = ss.canonical_host
                         AND sf.suite_path          = ss.suite_path
                         AND sf.current_snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.path             = ?
   AND ss.anchor_form    <> 'unsigned'`

// declaredHashesExact is DeclaredHashesForPath's exact lookup (spec C6
// step 1): the rows whose path is path.
func (c *Cache) declaredHashesExact(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	rows, err := c.db.QueryContext(ctx, declaredExactSQL, scheme, host, path)
	if err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeclaredHash
	for rows.Next() {
		var d DeclaredHash
		if err := rows.Scan(&d.DeclaredSHA256, &d.SnapshotID, &d.PackageName, &d.SuitePath); err != nil {
			return nil, fmt.Errorf("DeclaredHashesForPath scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath iter: %w", err)
	}
	return out, nil
}

// maxFallbackSegments bounds spec C6's suffix fallback: a dot-cleaned
// request path of more segments is never looked up by suffix (a constant;
// it also bounds the fallback query's IN list).
const maxFallbackSegments = 32

// flatSplits returns spec C6's splits of a request path for the suffix
// fallback: P' = aptpath.DotClean(path) cut as B + "/" + F at every
// segment boundary, from the host root (B = "") to the parent of the
// last segment, as parallel slices. It returns nothing when P' is not
// absolute, has an empty or ".." segment, or has more than
// maxFallbackSegments segments (a ".." could step a request outside the
// base its row is kept for). The ".." and empty-segment stops are defence
// in depth — with valid rows no such split could match (a B holding them
// is never an ancestor of a canonical FlatBase, an F never equals a
// validated flat_filename) — so TestFlatSplits pins them on this function.
func flatSplits(path string) (bases, names []string) {
	p := aptpath.DotClean(path)
	if !strings.HasPrefix(p, "/") {
		return nil, nil
	}
	segs := strings.Split(p[1:], "/")
	if len(segs) > maxFallbackSegments {
		return nil, nil
	}
	for _, s := range segs {
		if s == "" || s == ".." {
			return nil, nil
		}
	}
	for i := range segs {
		b := ""
		if i > 0 {
			b = "/" + strings.Join(segs[:i], "/")
		}
		bases = append(bases, b)
		names = append(names, strings.Join(segs[i:], "/"))
	}
	return bases, names
}

// declaredBySuffixSQL is the suffix fallback's query for n names: the
// current signed snapshots' flat rows of (scheme, host) whose
// flat_filename is one of them, with the row's suite path.
//
// AIDEV-NOTE: the term requiring a non-empty flat_filename is what lets
// SQLite use the partial index idx_package_hash_flat_filename (whose WHERE
// is the same term); without it every lookup scans the host's rows. The
// suite_freshness join is declaredExactSQL's: by its primary key (design
// review M-2).
// TestDeclaredHashesForPath_TheFallbackUsesThePartialIndex pins the plan.
func declaredBySuffixSQL(n int) string {
	return `
SELECT DISTINCT p.declared_sha256, p.snapshot_id, p.package_name, sf.suite_path, p.flat_filename
  FROM package_hash p
  JOIN suite_snapshot ss  ON ss.snapshot_id = p.snapshot_id
  JOIN suite_freshness sf ON sf.canonical_scheme    = ss.canonical_scheme
                         AND sf.canonical_host      = ss.canonical_host
                         AND sf.suite_path          = ss.suite_path
                         AND sf.current_snapshot_id = p.snapshot_id
 WHERE p.canonical_scheme = ?
   AND p.canonical_host   = ?
   AND p.flat_filename   <> ''
   AND p.flat_filename IN (` + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)
   AND ss.anchor_form    <> 'unsigned'`
}

// declaredHashesBySuffix is spec C6's suffix fallback (step 2): for every
// split B + "/" + F of the dot-cleaned path (flatSplits), the current
// signed snapshots' rows whose flat_filename is F, kept iff B is an
// ancestor-or-self of the row's suite's FlatBase; of the kept rows, those
// with the longest B — the most specific base — are returned. A path
// flatSplits refuses has no fallback.
func (c *Cache) declaredHashesBySuffix(ctx context.Context, scheme, host, path string) ([]DeclaredHash, error) {
	bases, names := flatSplits(path)
	if len(names) == 0 {
		return nil, nil
	}
	baseOf := make(map[string]string, len(names))
	args := make([]any, 0, len(names)+2)
	args = append(args, scheme, host)
	for i, n := range names {
		baseOf[n] = bases[i]
		args = append(args, n)
	}
	rows, err := c.db.QueryContext(ctx, declaredBySuffixSQL(len(names)), args...)
	if err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath fallback: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []DeclaredHash
		best = -1
	)
	for rows.Next() {
		var (
			d    DeclaredHash
			name string
		)
		if err := rows.Scan(&d.DeclaredSHA256, &d.SnapshotID, &d.PackageName, &d.SuitePath, &name); err != nil {
			return nil, fmt.Errorf("DeclaredHashesForPath fallback scan: %w", err)
		}
		b, ok := baseOf[name]
		if !ok || !strings.HasPrefix(aptpath.FlatBase(d.SuitePath)+"/", b+"/") {
			continue
		}
		switch {
		case len(b) > best:
			best, out = len(b), []DeclaredHash{d}
		case len(b) == best:
			out = append(out, d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DeclaredHashesForPath fallback iter: %w", err)
	}
	return out, nil
}

// IntegrityCandidate is a single (blob_hash, declared_sha256, snapshot_id,
// source) tuple emitted by ListIntegrityCandidates for the SPEC2 §6.5
// at-rest scan. The scanner reads pool/<BlobHash>, hashes it, and
// compares the result against BlobHash itself. SourceTable is one of
// "snapshot_member" or "package_hash" — surfaced in the
// at_rest_corruption log line per SPEC2 §10.2 ("first-found is
// reported"). For snapshot_member rows DeclaredSHA256 == BlobHash;
// for package_hash rows blob_hash is not stored on the row, so the
// query reuses declared_sha256 as the expected pool filename.
type IntegrityCandidate struct {
	BlobHash       string
	DeclaredSHA256 string
	SnapshotID     int64
	SourceTable    string
}

// ListIntegrityCandidates returns one row per distinct blob hash pinned
// by any current snapshot, drawn from snapshot_member and package_hash.
// Rows whose snapshot is no longer current (displaced by a later
// adoption) are excluded — the §6.5 scanner only verifies blobs the
// current contract still relies on. SPEC §10.2's "first-found is
// reported" semantic governs duplicates: when the same blob is pinned
// by both a snapshot_member and a package_hash, snapshot_member wins
// (it appears first in the UNION).
//
// Returns rows in no defined order. Empty slice + nil error means no
// snapshot covers any blob — a fresh deploy with no adoptions has
// nothing to scan, which is correct.
//
// AIDEV-NOTE: the integrity scanner is the only caller. The query
// excludes blob.refcount-0 rows transitively via the suite_freshness
// join: only blobs reachable from a current snapshot show up. Phase 4
// GC will reap orphans separately; the scanner does not race it.
func (c *Cache) ListIntegrityCandidates(ctx context.Context) ([]IntegrityCandidate, error) {
	const q = `
SELECT blob_hash, declared_sha256, snapshot_id, source FROM (
  SELECT sm.blob_hash       AS blob_hash,
         sm.declared_sha256 AS declared_sha256,
         sm.snapshot_id     AS snapshot_id,
         'snapshot_member'  AS source
    FROM snapshot_member sm
    JOIN suite_freshness sf ON sf.current_snapshot_id = sm.snapshot_id
  UNION ALL
  SELECT p.declared_sha256 AS blob_hash,
         p.declared_sha256 AS declared_sha256,
         p.snapshot_id     AS snapshot_id,
         'package_hash'    AS source
    FROM package_hash p
    JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ListIntegrityCandidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// AIDEV-NOTE: dedup by blob_hash in Go rather than SQL DISTINCT to
	// preserve "first-found wins" — snapshot_member rows precede
	// package_hash rows in the UNION ALL, so the first occurrence of
	// each hash is the snapshot_member tuple when both apply. SQL
	// DISTINCT over (blob_hash, source, ...) would emit two rows;
	// DISTINCT on blob_hash alone is undefined under SQLite when other
	// columns differ.
	seen := make(map[string]struct{})
	var out []IntegrityCandidate
	for rows.Next() {
		var ic IntegrityCandidate
		if err := rows.Scan(&ic.BlobHash, &ic.DeclaredSHA256, &ic.SnapshotID, &ic.SourceTable); err != nil {
			return nil, fmt.Errorf("ListIntegrityCandidates scan: %w", err)
		}
		if _, dup := seen[ic.BlobHash]; dup {
			continue
		}
		seen[ic.BlobHash] = struct{}{}
		out = append(out, ic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListIntegrityCandidates iter: %w", err)
	}
	return out, nil
}

// EvictURLPath deletes the url_path row for (scheme, host, path) and
// decrements the prior blob's refcount in a single transaction. SPEC2
// §6.1 step 5: stale Phase 1 row covered by a Phase 2 snapshot has
// diverged from the snapshot's declared hash; the row must drop and the
// blob's refcount must fall by one for Phase 4 GC to reclaim the bytes.
//
// Idempotent: if the row is already gone (concurrent eviction, manual
// delete) the call is a no-op and returns nil — exactly one decrement
// must fire per refcount-bump, and racing the row delete is the only
// boundary at which the bookkeeping could double-count.
//
// Returns nil on success even when no row was deleted; callers that need
// to know whether they were the eviction winner should check before-and-
// after with LookupURL. Returns an error only on a real DB fault.
//
// AIDEV-NOTE: SPEC2 §4.3.1 names adoption as the only writer that bumps
// blob.refcount, while §6.1 step 5 (and §11) explicitly says eviction
// decrements. Taken together a Phase 1 blob whose url_path row is
// evicted via this path can land with a negative refcount, because
// PutURLPath does not currently bump on insert. Phase 4 GC checks
// `refcount <= 0` for sweep eligibility, so a transient -1 still reaps
// correctly; the literal decrement here matches what the SPEC text
// asks for. If a future phase tightens GC to an `= 0` predicate, this
// helper and PutURLPath will need to be adjusted in lockstep.
func (c *Cache) EvictURLPath(ctx context.Context, scheme, host, path string) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("EvictURLPath: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// Read the row's current blob_hash inside the tx so a concurrent
		// re-fetch that completes between read and delete does not race.
		var blobHash sql.NullString
		err = tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
			scheme, host, path).Scan(&blobHash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("EvictURLPath: read row: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
DELETE FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
			scheme, host, path)
		if err != nil {
			return fmt.Errorf("EvictURLPath: delete: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("EvictURLPath: rows affected: %w", err)
		}
		if affected == 0 {
			// Row vanished between SELECT and DELETE — concurrent eviction
			// won. Skip the refcount decrement: the winner already did it.
			return nil
		}
		if blobHash.Valid && blobHash.String != "" {
			// SPEC4 §7.5.1 Rule 3: same COALESCE/IIF pattern as
			// CommitAdoption Step 8 — preserve existing
			// refcount_zeroed_at on 0 → -1, set to now on the first
			// ≤ 0 crossing.
			now := nowUnix()
			if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount - 1,
       refcount_zeroed_at = COALESCE(
         refcount_zeroed_at,
         IIF(refcount - 1 <= 0, ?, NULL)
       )
 WHERE hash = ?`,
				now, blobHash.String); err != nil {
				return fmt.Errorf("EvictURLPath: decrement refcount: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("EvictURLPath: commit: %w", err)
		}
		return nil
	})
}
