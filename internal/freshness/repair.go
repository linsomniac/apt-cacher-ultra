package freshness

// SPEC6_7 §3: the freshness-tick skipped-member repair pass.
//
// An adoption that raced a mid-sync mirror commits its snapshot with
// integrity-class members skipped (recorded in snapshot_skipped_member
// with their signed declarations). Re-adoption only fires on a CHANGED
// InRelease, so without this pass a degraded snapshot stays degraded
// for its whole lifetime — ~17h for slow-publishing devel suites in
// the 2026-06-09 incident. The repair pass runs whenever a freshness
// check finds the suite unchanged at upstream (304 / byte-identical
// 200) and re-attempts only the integrity-class skips: their dominant
// cause heals within minutes once the lagging mirror backend syncs.

import (
	"context"
	"errors"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// RepairSkippedMembers re-attempts every repairable (integrity-class)
// skipped member of the suite's snapshot. Invoked by the freshness
// Checker with the per-suite mutex held (handed off exactly like an
// adoption goroutine), so it never overlaps an adoption or another
// repair of the same suite. Admission (a global and a host slot) is
// taken by the Checker before it spawns the repair; this method never
// waits for one.
//
// Per member: the full adoptMember sequence runs — pool reuse, by-hash
// probe (the content-addressed URL immune to the revision race; the
// expected heal path), canonical fetch — validating bytes against the
// recorded declared_sha256/size exactly as the original adoption
// would. On success the member is promoted into snapshot_member
// (canonical path + by-hash alias) via cache.RepairSkippedMember,
// which re-checks inside its transaction that the snapshot is still
// current. On failure (fetch or promotion) the row's retry_count is
// bumped and the next fresh tick tries again; the bookkeeping is for
// operator visibility, not a cap. Attempts recur for as long as the
// snapshot stays current — which can be indefinitely for a suite
// whose upstream never republishes — at a cost of 1-2 upstream
// requests per degraded member per fresh tick, with the tick cadence
// already cooldown-limited. A persistent non-zero retry_count is the
// operator signal that a member needs investigation. One cause of such
// a member is recognised and ends its attempts: an upstream that
// answers the member's URL with its compressed sibling does not
// publish it, and the record is retired (errAdoptionMemberSiblingServed).
//
// No-ops (cheaply) when the repair switch is off or the snapshot has
// no repairable rows — the steady state for every healthy suite.
func (a *Adopter) RepairSkippedMembers(ctx context.Context, suite SuiteRef, snapshotID int64) {
	if !a.repairSkippedMembers {
		return
	}
	rows, err := a.cache.ListRepairableSkippedMembers(ctx, snapshotID)
	if err != nil {
		a.logger.Warn("adoption_repair_list_failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return
	}
	if len(rows) == 0 {
		return
	}

	repaired, retired := 0, 0
	decl := a.repairDeclarations(ctx, suite, snapshotID)
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		m := ReleaseMember{
			Path:   row.Path,
			Size:   row.Size,
			SHA256: row.DeclaredSHA256,
		}
		// Spec C5: the record keeps only the SHA-256; the snapshot's
		// Release adds every other declared hash (a dual-hash member is
		// verified against both and gets its SHA512 alias).
		if d, ok := decl[row.Path]; ok && d.SHA256 == row.DeclaredSHA256 {
			m = d
		}
		// acquireByHash unconditionally: the probe is silent and
		// side-effect-free, and we no longer hold the Release text to
		// re-read the Acquire-By-Hash flag. A repo without by-hash
		// costs one extra 404 per attempt; the by-hash URL is the very
		// reason this repair can succeed while the canonical path is
		// still serving the previous generation.
		blobHash, _, ferr := a.adoptMemberFromRelease(ctx, suite, m, probeByHashFirst, decl)
		if errors.Is(ferr, errAdoptionMemberSiblingServed) {
			// Upstream answers the member's URL with its compressed
			// sibling: the plain file is not published, and no later pass
			// would find otherwise (servedAsSibling, which logged the
			// skip). Retire the record instead of counting a failure
			// against it at every unchanged check.
			//
			// AIDEV-NOTE: only this skip retires a record. A 404 here is
			// still a failed attempt, tried again: while a mirror syncs, a
			// backend that has not got a new file yet answers 404 for it,
			// which is the very race this pass exists to outlast.
			derr := a.cache.DropSkippedMember(ctx, snapshotID, row.Path)
			if derr == nil {
				adoptionMemberRepairsTotal.Inc("not_published")
				retired++
				continue
			}
			ferr = derr
		}
		if ferr != nil {
			if yielded(ctx) {
				// The repair gave way (repair_yield.go). Being stopped says
				// nothing about the member: no failure is logged or
				// counted, and the next unchanged check tries it again.
				return
			}
			adoptionMemberRepairsTotal.Inc("failure")
			a.logger.Warn("adoption_member_repair_failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"snapshot_id", snapshotID,
				"path", row.Path,
				"attempts", row.RetryCount+1,
				"err", ferr,
			)
			if berr := a.cache.BumpSkippedMemberRetry(ctx, snapshotID, row.Path); berr != nil {
				a.logger.Warn("adoption_repair_bump_failed",
					"snapshot_id", snapshotID, "path", row.Path, "err", berr)
			}
			continue
		}

		members := memberRows(snapshotID, m, blobHash)
		if rerr := a.cache.RepairSkippedMember(ctx, snapshotID, row.Path, members); rerr != nil {
			if errors.Is(rerr, cache.ErrSnapshotNotCurrent) {
				// A newer adoption displaced the snapshot mid-pass; its
				// own skip records (if any) belong to the next tick.
				a.logger.Info("adoption_repair_snapshot_displaced",
					"canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath,
					"snapshot_id", snapshotID,
				)
				return
			}
			if yielded(ctx) {
				return // the promotion was cut short as the repair gave way
			}
			adoptionMemberRepairsTotal.Inc("failure")
			a.logger.Warn("adoption_member_repair_failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"snapshot_id", snapshotID,
				"path", row.Path,
				"attempts", row.RetryCount+1,
				"err", rerr,
			)
			// Promotion failures count as attempts too — without the
			// bump a permanently stuck repair reads as forever on
			// attempt one. ErrNotFound rows make this a benign no-op.
			if berr := a.cache.BumpSkippedMemberRetry(ctx, snapshotID, row.Path); berr != nil {
				a.logger.Warn("adoption_repair_bump_failed",
					"snapshot_id", snapshotID, "path", row.Path, "err", berr)
			}
			continue
		}
		adoptionMemberRepairsTotal.Inc("success")
		repaired++
		a.logger.Info("adoption_member_repaired",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"path", row.Path,
			"attempts", row.RetryCount+1,
		)
	}
	a.logger.Info("adoption_repair_pass",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"attempted", len(rows),
		"repaired", repaired,
		"retired", retired,
		"remaining", len(rows)-repaired-retired,
	)
}

// repairDeclarations re-reads the members the snapshot's Release declares
// (reparseSnapshotRelease, the trust anchor), by path, so a repair checks
// every declared hash and writes an alias per declared algorithm (spec
// C5). nil when it cannot: the repair then verifies against the skip
// records' SHA-256 alone, as before phase E (logged at DEBUG; reconcile
// reports an unreadable anchor at WARN).
func (a *Adopter) repairDeclarations(ctx context.Context, suite SuiteRef, snapshotID int64) map[string]ReleaseMember {
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		a.logger.Debug("adoption_repair_declarations_unavailable",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
			"snapshot_id", snapshotID, "err", err)
		return nil
	}
	out := make(map[string]ReleaseMember, len(declared))
	for _, m := range declared {
		out[m.Path] = m
	}
	return out
}

// repairPending reports whether the snapshot has integrity-class skip
// rows a repair pass could heal: one indexed read, so the Checker asks
// before it takes an admission slot.
func (a *Adopter) repairPending(ctx context.Context, suite SuiteRef, snapshotID int64) bool {
	rows, err := a.cache.ListRepairableSkippedMembers(ctx, snapshotID)
	if err != nil {
		a.logger.Warn("adoption_repair_list_failed",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
			"snapshot_id", snapshotID, "err", err)
		return false
	}
	return len(rows) > 0
}
