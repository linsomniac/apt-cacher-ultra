package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// ReapRegardless is ReapIdleSuite's cutoff for a reap regardless of
// idleness (spec C9: non-canonical rows, ruling R-F3's junk rows): every
// row qualifies, a NULL or future interest stamp included. ForgetSuite
// deletes with it too.
const ReapRegardless int64 = math.MaxInt64

// reapMetadataBatch is spec C9's batch: metadata url_path rows deleted per
// writer transaction.
//
// AIDEV-NOTE: measured (phase F design review, M-15): one 1,000-row batch
// held the writer about 110 ms over a 215,000-row url_path and blob table,
// so a reap is a few such slices, taken in turn with the request path's
// writes. Re-measure before raising it, or if a slower disk shows request
// latency during reaps.
const reapMetadataBatch = 1000

// deleteOutcome is what deleteSuiteTx did.
type deleteOutcome int

const (
	// suiteKept: the row no longer qualifies; nothing was written.
	suiteKept deleteOutcome = iota
	// suiteDeleted: the row was deleted.
	suiteDeleted
	// suiteNoRow: there was no row.
	suiteNoRow
)

// deleteSuiteTx is the single suite-row delete — cap eviction (spec C3),
// the reaper's ReapIdleSuite and POST /forget's ForgetSuite (spec C9) —
// run inside the caller's writer transaction as a compare-and-delete: it
// re-reads the row and deletes it only if the row still qualifies —
// last_requested_at at or before cutoff (a stamp in the future, after a
// backward clock step, never qualifies; nor does a NULL one), unless
// cutoff is ReapRegardless, which every row passes — and
// current_snapshot_id equal to wantCurrent (nil: no current snapshot). It
// releases the current snapshot's pins and retires every snapshot of the
// suite (retireSuiteSnapshotsTx), and deletes the row.
//
// AIDEV-NOTE: retiring the snapshots is load-bearing: CommitAdoption
// refuses a retired candidate, and retired rows leave the natural-key,
// rollback and retained-by-hash lookups, so a returning suite re-adopts
// unchanged content at once (spec C9). Every statement is keyed by the
// full (scheme, host, suite_path): a same-path suite of another host, or
// the other scheme's twin, is never touched
// (TestCreateSuite_EvictionTouchesOnlyTheEvictee,
// TestCreateSuite_EvictionLeavesTheOtherSchemesTwin). Keep it the single
// DELETE FROM suite_freshness (TestSuiteFreshnessHasOneDeleteSite, ruling
// R-F1; moved here from suite_rows.go, deferred minor D-14).
func deleteSuiteTx(ctx context.Context, tx *sql.Tx, key SuiteKey, wantCurrent *int64, cutoff, now int64) (deleteOutcome, error) {
	var current, requested sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT current_snapshot_id, last_requested_at FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&current, &requested)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return suiteNoRow, nil
	case err != nil:
		return suiteKept, fmt.Errorf("delete suite %s%s: read row: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if cutoff != ReapRegardless && (!requested.Valid || requested.Int64 > cutoff) {
		return suiteKept, nil
	}
	if current.Valid != (wantCurrent != nil) || (current.Valid && current.Int64 != *wantCurrent) {
		return suiteKept, nil
	}
	if err := retireSuiteSnapshotsTx(ctx, tx, key, current, now); err != nil {
		return suiteKept, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return suiteKept, fmt.Errorf("delete suite %s%s: %w", key.CanonicalHost, key.SuitePath, err)
	}
	return suiteDeleted, nil
}

// retireSuiteSnapshotsTx is the reaper's retirement path (spec C9): it
// releases the pins of the suite's current snapshot, when it has one (the
// Step-8 decrement, decrementSnapshotRefcountsTx: once, since the snapshot
// stops being current here), and stamps retired_at on every snapshot of
// the suite not yet retired. Its caller, deleteSuiteTx, deletes the row.
//
// AIDEV-NOTE: retired snapshots are invisible to the natural key, the
// candidate lookup, rollback_refused, retained by-hash, strict mode and
// snapshot-GC sub-job B (spec C9), so the suite re-adopts unchanged or
// older content at once; sub-job C deletes them. Keyed by the full
// (scheme, host, suite_path): a /. twin or the other scheme's suite keeps
// its own.
func retireSuiteSnapshotsTx(ctx context.Context, tx *sql.Tx, key SuiteKey, current sql.NullInt64, now int64) error {
	if current.Valid {
		if err := decrementSnapshotRefcountsTx(ctx, tx, current.Int64, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET retired_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND retired_at IS NULL`,
		now, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
		return fmt.Errorf("retire snapshots: %w", err)
	}
	return nil
}

// suiteMetadataPaths returns the url_path rows of key's suite — paths of
// its scheme and host under the suite whose suite (aptpath.SuitePath) is
// exactly key's, never a /. twin's or a nested suite's (spec C9) — split
// into plain metadata and the suite's anchors (InRelease, Release,
// Release.gpg), each in path order. A read on the pool, outside the
// writer.
func (c *Cache) suiteMetadataPaths(ctx context.Context, key SuiteKey) (plain, anchors []string, err error) {
	prefix := key.SuitePath + "/"
	// The primary key's range [prefix, prefix with its '/' raised to '0').
	rows, err := c.db.QueryContext(ctx, `
SELECT path FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path >= ? AND path < ?
 ORDER BY path`, key.CanonicalScheme, key.CanonicalHost, prefix, key.SuitePath+"0")
	if err != nil {
		return nil, nil, fmt.Errorf("suite metadata paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, nil, fmt.Errorf("suite metadata paths: scan: %w", err)
		}
		if aptpath.SuitePath(p) != key.SuitePath {
			continue
		}
		switch strings.TrimPrefix(p, prefix) {
		case "InRelease", "Release", "Release.gpg":
			anchors = append(anchors, p)
		default:
			plain = append(plain, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("suite metadata paths: %w", err)
	}
	return plain, anchors, nil
}

// deleteCachedRowTx deletes the url_path row of scheme, host and path, if
// there is one, releasing its blob's pin as url_path GC does
// (deleteURLPathRowTx), and reports whether it deleted one: a path without
// a row is skipped, so a row is deleted — and its blob decremented — once.
func deleteCachedRowTx(ctx context.Context, tx *sql.Tx, scheme, host, path string, now int64) (bool, error) {
	var blob sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
		scheme, host, path).Scan(&blob)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %s://%s%s: %w", scheme, host, path, err)
	}
	if err := deleteURLPathRowTx(ctx, tx, scheme, host, path, blob, now); err != nil {
		return false, err
	}
	return true, nil
}

// deleteURLPathRows deletes, in one writer transaction, the url_path rows
// of scheme and host at paths (deleteCachedRowTx) and reports the rows
// deleted.
func (c *Cache) deleteURLPathRows(ctx context.Context, scheme, host string, paths []string) (int, error) {
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("delete url_path rows: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now, deleted := nowUnix(), 0
		for _, p := range paths {
			ok, err := deleteCachedRowTx(ctx, tx, scheme, host, p, now)
			if err != nil {
				return fmt.Errorf("delete url_path rows: %w", err)
			}
			if ok {
				deleted++
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("delete url_path rows: commit: %w", err)
		}
		n = deleted
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteSuiteMetadata is spec C9's step 1: it deletes key's suite metadata
// url_path rows (suiteMetadataPaths) in writer transactions of at most
// reapMetadataBatch rows, plain metadata first and the anchors last, never
// touching the suite row, and reports how many rows it deleted. After an
// error the batches before it stay deleted; a later call deletes the rest.
//
// AIDEV-NOTE: the anchors go last so that a run cut short (an error, a
// shutdown) leaves the suite its anchors, the rows that onboard it and
// serve it unadopted. Each row's pin is released with its row, once; the
// current snapshot's pins are ReapIdleSuite's (deleteSuiteTx), so a reap
// whose final step finds the suite requested again repeats no decrement
// (TestDeleteSuiteMetadata_NeverRepeatsADecrement).
func (c *Cache) DeleteSuiteMetadata(ctx context.Context, key SuiteKey) (int, error) {
	plain, anchors, err := c.suiteMetadataPaths(ctx, key)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, paths := range [][]string{plain, anchors} {
		for len(paths) > 0 {
			batch := paths[:min(len(paths), reapMetadataBatch)]
			paths = paths[len(batch):]
			n, err := c.deleteURLPathRows(ctx, key.CanonicalScheme, key.CanonicalHost, batch)
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// ReapIdleSuite is the reaper's final step (spec C9 step 2): one writer
// transaction running deleteSuiteTx's compare-and-delete with the current
// snapshot the reaper listed (wantCurrent) and its cutoff — an idle
// cutoff, or ReapRegardless for a non-canonical or junk row — which, when
// it deletes the row, also moves the base's signed-seen markers up to now.
// It reports deleted (this call deleted the row) and gone (the suite has
// no row: deleted, none was found, or — after a failed submit — none is
// found by a fresh read). The caller releases its SuiteGuard hold with
// release(gone), which drops the lock-map and flip-gate entries; a gone
// suite's touch-coalescing entry is dropped here.
//
// AIDEV-NOTE: design review M-2: submitWrite can fail after the
// transaction committed (its context ended between the commit and the
// hand-back), so a failed submit is settled by a fresh read of the row, as
// afterCreate settles a creation; under the caller's hold no other deleter
// runs, so a row found gone then was deleted by this call. Design review
// M-9: the base's markers (aptpath.MarkerBase, under both schemes, as
// never-downgrade reads them) are stamped MAX(last_seen_at, now), so the
// orphan sweep's year counts from the end of the suite's tracking — after
// a long downtime a reaped suite's marker would otherwise be swept a day
// after it became an orphan. MAX never lowers a stamp (ruling PF-1), and
// an UPDATE never creates a marker.
func (c *Cache) ReapIdleSuite(ctx context.Context, key SuiteKey, wantCurrent *int64, cutoff int64) (deleted, gone bool, err error) {
	var out deleteOutcome
	err = c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ReapIdleSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		o, err := deleteSuiteTx(ctx, tx, key, wantCurrent, cutoff, now)
		if err != nil {
			return fmt.Errorf("ReapIdleSuite: %w", err)
		}
		if o == suiteDeleted {
			if _, err := tx.ExecContext(ctx, `
UPDATE suite_signed_seen SET last_seen_at = MAX(last_seen_at, ?)
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?`,
				now, key.CanonicalHost, aptpath.MarkerBase(key.SuitePath)); err != nil {
				return fmt.Errorf("ReapIdleSuite: stamp markers: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ReapIdleSuite: commit: %w", err)
		}
		if c.committedHook != nil {
			c.committedHook()
		}
		out = o
		return nil
	})
	if err != nil {
		gone = c.suiteRowGone(key) // never read out after an error: the operation may still run
	} else {
		deleted, gone = out == suiteDeleted, out != suiteKept
	}
	if gone {
		c.forgetTouch(key)
	}
	return deleted, gone, err
}

// ForgetResult reports ForgetSuite's effects.
type ForgetResult struct {
	// Deleted: this call deleted the suite row.
	Deleted bool
	// Gone: the suite has no row — Deleted, none was found, or, after a
	// failed submit, none is found by a fresh read. The caller releases its
	// SuiteGuard hold with release(Gone).
	Gone bool
	// MarkersDeleted counts the base's signed-seen markers deleted with it.
	MarkersDeleted int
	// AnchorRowsDeleted counts the cached anchor url_path rows of the base
	// and its /. twin, under both schemes, deleted with the markers.
	AnchorRowsDeleted int
	// KeptBy lists the suite rows that kept the base's markers and anchor
	// rows — the host's rows at the base or its /. twin, under either
	// scheme — in key order; empty when nothing kept them.
	KeptBy []SuiteKey
}

// ForgetSuite is POST /forget's final step (spec C9, D9): one writer
// transaction running deleteSuiteTx with ReapRegardless (no idle check)
// and wantCurrent, then — only once no suite row of the host is left for
// the suite's base (aptpath.MarkerBase) or its /. twin, under either
// scheme — deleting the base's signed-seen markers under both schemes and
// the cached InRelease, Release and Release.gpg url_path rows of the base
// and its twin under both schemes; otherwise it reports the rows that kept
// them. A failed submit is settled as ReapIdleSuite settles one.
//
// AIDEV-NOTE: never-downgrade reads a base's markers under either scheme
// (spec C5 safeguard 3, design review M-7), so /forget, the escape hatch
// for unsigned_downgrade_refused, deletes both or neither: a marker left
// under the other scheme would keep refusing, and one deleted while a
// twin or the other scheme's suite still has a row would strip that
// suite's protection (spec C3: "only if no other suite with the same base
// has a row"). The anchor rows go with the markers (ruling PF-2): no suite
// owns them then, and they are what the unsigned commit's re-mark (ruling
// R-E1) reads — left in place, a signed one cached under the other scheme
// or at the twin path marks the base again, and the escape hatch stays
// shut with no row left to forget. At most 12 rows, each releasing its
// blob's pin.
func (c *Cache) ForgetSuite(ctx context.Context, key SuiteKey, wantCurrent *int64) (ForgetResult, error) {
	var got ForgetResult
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("ForgetSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		now := nowUnix()
		out, err := deleteSuiteTx(ctx, tx, key, wantCurrent, ReapRegardless, now)
		if err != nil {
			return fmt.Errorf("ForgetSuite: %w", err)
		}
		r := ForgetResult{Deleted: out == suiteDeleted, Gone: out != suiteKept}
		if r.Deleted {
			base := aptpath.MarkerBase(key.SuitePath)
			if r.KeptBy, err = baseSuiteRowsTx(ctx, tx, key.CanonicalHost, base); err != nil {
				return fmt.Errorf("ForgetSuite: %w", err)
			}
			if len(r.KeptBy) == 0 {
				if r.AnchorRowsDeleted, err = deleteBaseAnchorsTx(ctx, tx, key.CanonicalHost, base, now); err != nil {
					return fmt.Errorf("ForgetSuite: delete anchors: %w", err)
				}
				dr, err := tx.ExecContext(ctx, `
DELETE FROM suite_signed_seen
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND base = ?`, key.CanonicalHost, base)
				if err != nil {
					return fmt.Errorf("ForgetSuite: delete markers: %w", err)
				}
				n, err := dr.RowsAffected()
				if err != nil {
					return fmt.Errorf("ForgetSuite: delete markers: %w", err)
				}
				r.MarkersDeleted = int(n)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("ForgetSuite: commit: %w", err)
		}
		if c.committedHook != nil {
			c.committedHook()
		}
		got = r
		return nil
	})
	var res ForgetResult
	if err != nil {
		res.Gone = c.suiteRowGone(key) // never read got after an error: the operation may still run
	} else {
		res = got
	}
	if res.Gone {
		c.forgetTouch(key)
	}
	return res, err
}

// baseSuiteRowsTx returns the host's suite rows at base or at its /. twin,
// under either scheme, in key order: the rows whose suites share base's
// signed-seen markers.
func baseSuiteRowsTx(ctx context.Context, tx *sql.Tx, host, base string) ([]SuiteKey, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT canonical_scheme, suite_path FROM suite_freshness
 WHERE canonical_scheme IN ('http', 'https') AND canonical_host = ? AND suite_path IN (?, ?)
 ORDER BY canonical_scheme, suite_path`, host, base, base+"/.")
	if err != nil {
		return nil, fmt.Errorf("read the base's suites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SuiteKey
	for rows.Next() {
		k := SuiteKey{CanonicalHost: host}
		if err := rows.Scan(&k.CanonicalScheme, &k.SuitePath); err != nil {
			return nil, fmt.Errorf("read the base's suites: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the base's suites: %w", err)
	}
	return out, nil
}

// deleteBaseAnchorsTx deletes the cached InRelease, Release and
// Release.gpg url_path rows of base and of its /. twin under both schemes
// (deleteCachedRowTx) and reports how many it deleted.
func deleteBaseAnchorsTx(ctx context.Context, tx *sql.Tx, host, base string, now int64) (int, error) {
	n := 0
	for _, scheme := range []string{"http", "https"} {
		for _, suite := range []string{base, base + "/."} {
			for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
				ok, err := deleteCachedRowTx(ctx, tx, scheme, host, suite+"/"+name, now)
				if err != nil {
					return n, err
				}
				if ok {
					n++
				}
			}
		}
	}
	return n, nil
}
