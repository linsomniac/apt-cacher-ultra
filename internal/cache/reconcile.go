package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// insertPackageHashTx adds one package row to list listID inside an open
// transaction, for the InsertReconciledMembers path only, and reports
// whether it wrote (an insert or a heal).
//
// AIDEV-NOTE: the commit writes its rows with a plain INSERT
// (writePackageListTx: a new list holds nothing, so a conflict there is a
// bug), while reconcile probes the list's primary key (package_list_id,
// path) first and is retry-safe: a byte-identical existing row is an
// idempotent no-op, an existing row with EMPTY identity columns
// (pre-v3/pre-v6, un-backfilled) is HEALED in place, and a same-path row
// whose declared_sha256 OR a non-empty identity field
// (package_name/architecture/version) DISAGREES is a real cross-index
// conflict and fails loud (SPEC6_5 §11 H7). It is NOT a silent ON CONFLICT
// DO NOTHING. If the package_list_entry column set ever changes, update
// insertPackageListEntrySQL, the probe and heal below, and in
// package_list.go the difference's UPDATE (runPackageListDiffTx),
// DiffPackageList's SELECT and digestPackageRows (samePackageRow follows
// the struct by itself); both writers call noteFlatRows before they
// submit (Cache.flatRows, ruling PG-31).
// flat_filename (phase G) is inserted but neither probed nor healed here,
// on purpose: it follows from the row's path and its suite (DotClean of
// the Filename relative to FlatBase), so a same-path row of the suite's
// list always agrees, and no released binary wrote a flat row with another
// value (v6 flat suites took non_dists_layout and wrote none). The row's
// scheme and host are the suite's for the same reason and are not probed.
// ph.SnapshotID is not stored (schema 11) and is ignored.
func insertPackageHashTx(ctx context.Context, tx *sql.Tx, listID int64, ph PackageHash) (wrote bool, err error) {
	if !validBlobHash(ph.DeclaredSHA256) {
		return false, fmt.Errorf("insertPackageHashTx: %q declared_sha256 %w", ph.Path, ErrInvalidHash)
	}
	// Idempotent on retry, loud on disagreement — mirroring the snapshot_member
	// path below and adoption's own same-path/different-hash rule (which fails
	// with ErrAdoptionParseFailed, SPEC6_5 §11 H7). A byte-identical existing
	// row is a no-op; a DIFFERENT declared_sha256 for the same .deb path in
	// this suite's list is a real cross-index disagreement (an already-present
	// index vs the healed one) and must NOT silently keep the stale row — that
	// would fail-closed every strict-mode fetch of that .deb via the healed
	// index. (The previous ON CONFLICT DO NOTHING silently kept the stale row.)
	var existing struct {
		sha256  string
		name    string
		arch    string
		version string
	}
	switch err := tx.QueryRowContext(ctx, `
SELECT declared_sha256, package_name, architecture, version FROM package_list_entry
 WHERE package_list_id = ? AND path = ?`, listID, ph.Path).
		Scan(&existing.sha256, &existing.name, &existing.arch, &existing.version); {
	case err == nil:
		if existing.sha256 != ph.DeclaredSHA256 {
			return false, fmt.Errorf("insertPackageHashTx: %q already declares %s, reconcile has %s",
				ph.Path, existing.sha256, ph.DeclaredSHA256)
		}
		// Identity columns (package_name, architecture, version): an existing
		// EMPTY column is a pre-v3/pre-v6 (un-backfilled) row, NOT a conflict —
		// heal it in place from the reconcile's parsed value so the documented
		// POST /reconcile recovery works on upgraded caches and the GC mirror
		// can rank the healed row correctly. Healing all three together avoids
		// leaving a (name='', arch='', version='X') row that would group under
		// the empty identity in newest-N ranking. A column that is non-empty on
		// BOTH sides and differs is a real cross-index disagreement and stays
		// loud (mirrors the H7 hash rule).
		for _, f := range []struct{ what, have, want string }{
			{"package_name", existing.name, ph.PackageName},
			{"architecture", existing.arch, ph.Architecture},
			{"version", existing.version, ph.Version},
		} {
			if f.have != "" && f.want != "" && f.have != f.want {
				return false, fmt.Errorf("insertPackageHashTx: %q already declares %s %q, reconcile has %q",
					ph.Path, f.what, f.have, f.want)
			}
		}
		needHeal := (existing.name == "" && ph.PackageName != "") ||
			(existing.arch == "" && ph.Architecture != "") ||
			(existing.version == "" && ph.Version != "")
		if needHeal {
			if _, err := tx.ExecContext(ctx, `
UPDATE package_list_entry
   SET package_name = CASE WHEN package_name = '' THEN ? ELSE package_name END,
       architecture = CASE WHEN architecture = '' THEN ? ELSE architecture END,
       version      = CASE WHEN version      = '' THEN ? ELSE version      END
 WHERE package_list_id = ? AND path = ?`,
				ph.PackageName, ph.Architecture, ph.Version, listID, ph.Path); err != nil {
				return false, fmt.Errorf("insertPackageHashTx: heal identity %q: %w", ph.Path, err)
			}
		}
		return needHeal, nil // identical, or healed — idempotent
	case errors.Is(err, sql.ErrNoRows):
		// not present — insert below
	default:
		return false, fmt.Errorf("insertPackageHashTx: probe %q: %w", ph.Path, err)
	}
	if _, err := tx.ExecContext(ctx, insertPackageListEntrySQL,
		listID, ph.CanonicalScheme, ph.CanonicalHost, ph.Path,
		ph.DeclaredSHA256, ph.PackageName, ph.Architecture, ph.Version, ph.FlatFilename); err != nil {
		return false, fmt.Errorf("insertPackageHashTx: insert %q: %w", ph.Path, err)
	}
	return true, nil
}

// InsertReconciledMembers inserts snapshot_member (+ by-hash alias) rows
// into an EXISTING current snapshot and package rows into its suite's live
// package list — the SPEC6_8 in-place reconcile path. Unlike RepairSkippedMember it consumes no
// snapshot_skipped_member row; the caller (Adopter.ReconcileSnapshot) has
// already validated each member's bytes against the re-parsed, GPG-verified
// Release declaration. The transaction re-checks the snapshot is still the
// suite's current snapshot, so a reconcile that races a re-adoption is a safe
// no-op (ErrSnapshotNotCurrent). Member inserts are idempotent on the PK; a
// byte-identical row is skipped, a conflicting blob_hash on the same path is a
// loud error (mirrors the by-hash-alias invariant in CommitAdoption).
//
// AIDEV-NOTE: this is the SPEC6_8 §3 cache transaction — trust-sensitive.
// Two invariants must hold for every caller:
//  1. CURRENT-SNAPSHOT GUARD: the SELECT on suite_freshness below is the sole
//     authority that this snapshot is still serving clients; if it misses
//     (ErrSnapshotNotCurrent), the whole function returns without touching any
//     rows. Do not remove or weaken this check.
//  2. IDEMPOTENCY: the probe-before-insert on BOTH snapshot_member and
//     package_hash (insertPackageHashTx) means the entire function is safe to
//     retry — a byte-identical row is a no-op. A conflicting blob_hash (or
//     conflicting package_hash declared_sha256) on the same path is an explicit
//     error, not a silent no-op, because it would mean the caller is trying to
//     substitute a different blob than what already serves — that is corruption.
//
// AIDEV-NOTE: the package rows go into the list the suite names (schema
// 11), under three rules. A suite that names none — its current snapshot
// is unsigned — is refused its rows: an unsigned snapshot never vouches.
// A reconcile that wrote a row bumps package_list_generation, so an
// adoption commit holding a difference computed before it rewrites the
// list instead of applying a stale difference. And it records hot-package
// interest for the rows it wrote (recordListHotPackages): a later commit
// visits only rows that changed.
func (c *Cache) InsertReconciledMembers(ctx context.Context, snapshotID int64, members []SnapshotMember, packageHashes []PackageHash) error {
	c.noteFlatRows(packageHashes) // ruling PG-31: before the transaction can commit
	return c.submitWrite(ctx, writeReconcileMembers, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("InsertReconciledMembers: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// AIDEV-NOTE: current-snapshot guard — mirrors RepairSkippedMember
		// (skipped_member.go). A snapshot that is no longer current must not
		// receive new member rows: the displacement decrement (CommitAdoption
		// Step 8) already ran, so any blob refcount bumps here would leak.
		// ErrSnapshotNotCurrent is the sentinel callers check to distinguish
		// "lost the race" (benign) from a real error.
		var (
			key    SuiteKey
			listID sql.NullInt64
		)
		switch err := tx.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path, package_list_id
  FROM suite_freshness WHERE current_snapshot_id = ?`, snapshotID).
			Scan(&key.CanonicalScheme, &key.CanonicalHost, &key.SuitePath, &listID); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotNotCurrent, snapshotID)
		case err != nil:
			return fmt.Errorf("InsertReconciledMembers: current guard: %w", err)
		}
		if len(packageHashes) > 0 && !listID.Valid {
			return fmt.Errorf("InsertReconciledMembers: snapshot %d's suite has no package list (an unsigned snapshot holds no package rows)", snapshotID)
		}

		for _, m := range members {
			if m.SnapshotID != snapshotID {
				return fmt.Errorf("InsertReconciledMembers: member %q references snapshot %d, reconciling %d",
					m.Path, m.SnapshotID, snapshotID)
			}
			// Shape-check the hashes before any SQL — mirrors CommitAdoption
			// Step 4 (adoption.go). Defense-in-depth: a malformed hash should
			// fail loud here, not be smuggled past the schema CHECK or into a
			// BlobPath computation.
			if !validBlobHash(m.BlobHash) {
				return fmt.Errorf("InsertReconciledMembers: member %q blob_hash %w", m.Path, ErrInvalidHash)
			}
			if !validBlobHash(m.DeclaredSHA256) {
				return fmt.Errorf("InsertReconciledMembers: member %q declared_sha256 %w", m.Path, ErrInvalidHash)
			}
			// AIDEV-NOTE: idempotency probe — check for an existing row before
			// inserting. A byte-identical row (same blob_hash) is silently
			// skipped (safe retry) and must NOT bump refcount. A different
			// blob_hash on the same path is corruption and fails loud (the
			// caller tried to substitute a different blob than the one already
			// serving under this path).
			var existing string
			switch err := tx.QueryRowContext(ctx,
				`SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ? AND path = ?`,
				snapshotID, m.Path).Scan(&existing); {
			case err == nil:
				if existing != m.BlobHash {
					return fmt.Errorf("InsertReconciledMembers: path %q exists with blob %s, reconcile has %s",
						m.Path, existing, m.BlobHash)
				}
				continue // byte-identical: idempotent skip, no refcount bump
			case errors.Is(err, sql.ErrNoRows):
				// fall through to insert
			default:
				return fmt.Errorf("InsertReconciledMembers: probe %q: %w", m.Path, err)
			}

			// AIDEV-NOTE: refcount bump — mirrors RepairSkippedMember Step 3
			// (skipped_member.go ~174) and CommitAdoption Step 4 (adoption.go
			// ~484). Each new snapshot_member is a fresh pin on its blob, so
			// blob.refcount MUST rise to match, or the displacement decrement
			// (CommitAdoption Step 8, ~541) over-decrements this snapshot's
			// blobs to below their true count when it is later displaced —
			// corrupting the GC ledger. The NOT EXISTS guard bumps at most
			// ONCE per distinct blob within this snapshot: it runs BEFORE this
			// iteration's own INSERT, so "already pins" means a row from the
			// original adoption, an earlier reconcile, or an earlier member in
			// THIS loop that shares the blob (e.g. a path + its by-hash alias).
			// The IIF clears refcount_zeroed_at only on the strictly-positive
			// crossing, identical to the mirrored functions.
			if _, err := tx.ExecContext(ctx, `
UPDATE blob
   SET refcount = refcount + 1,
       refcount_zeroed_at = IIF(refcount + 1 > 0, NULL, refcount_zeroed_at)
 WHERE hash = ?
   AND NOT EXISTS (SELECT 1 FROM snapshot_member
                    WHERE snapshot_id = ? AND blob_hash = ?)`,
				m.BlobHash, snapshotID, m.BlobHash); err != nil {
				return fmt.Errorf("InsertReconciledMembers: bump refcount %s: %w", m.BlobHash, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256) VALUES (?,?,?,?)`,
				m.SnapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("InsertReconciledMembers: insert member %q: %w", m.Path, err)
			}
		}

		// Package rows — retry-safe via a probe-then-insert/heal that fails
		// loud on a real conflict (see insertPackageHashTx), not a silent ON
		// CONFLICT DO NOTHING.
		var written []string
		for _, ph := range packageHashes {
			wrote, err := insertPackageHashTx(ctx, tx, listID.Int64, ph)
			if err != nil {
				return fmt.Errorf("InsertReconciledMembers: package_hash %q: %w", ph.Path, err)
			}
			if wrote {
				written = append(written, ph.Path)
			}
		}
		if len(written) > 0 {
			if _, err := tx.ExecContext(ctx, `
UPDATE suite_freshness SET package_list_generation = package_list_generation + 1
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
				key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
				return fmt.Errorf("InsertReconciledMembers: bump the list's generation: %w", err)
			}
			if err := recordListHotPackages(ctx, tx, listID.Int64, key, written); err != nil {
				return fmt.Errorf("InsertReconciledMembers: %w", err)
			}
		}
		return tx.Commit()
	})
}
