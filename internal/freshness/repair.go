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
// repair of the same suite. Across suites, a pass with work to do
// counts against the same global [adoption].max_concurrent gate as
// adoptions — per-suite mutexes and per-host limits bound neither the
// total fan-out of repair fetches nor their cache-write pressure.
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
// operator signal that a member needs investigation.
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

	// Global concurrency cap, mirroring runShared Step 0. Acquired
	// only past the no-op checks so healthy suites' ticks never touch
	// the gate.
	if a.concurrencySem != nil {
		select {
		case a.concurrencySem <- struct{}{}:
			defer func() { <-a.concurrencySem }()
		case <-ctx.Done():
			return
		}
	}

	repaired := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		m := ReleaseMember{
			Path:   row.Path,
			Size:   row.Size,
			SHA256: row.DeclaredSHA256,
		}
		// acquireByHash unconditionally: the probe is silent and
		// side-effect-free, and we no longer hold the Release text to
		// re-read the Acquire-By-Hash flag. A repo without by-hash
		// costs one extra 404 per attempt; the by-hash URL is the very
		// reason this repair can succeed while the canonical path is
		// still serving the previous generation.
		blobHash, ferr := a.adoptMember(ctx, suite, m, true)
		if ferr != nil {
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

		members := []cache.SnapshotMember{{
			SnapshotID:     snapshotID,
			Path:           row.Path,
			BlobHash:       blobHash,
			DeclaredSHA256: row.DeclaredSHA256,
		}}
		if alias := byHashAliasPath(row.Path, row.DeclaredSHA256); alias != "" {
			members = append(members, cache.SnapshotMember{
				SnapshotID:     snapshotID,
				Path:           alias,
				BlobHash:       blobHash,
				DeclaredSHA256: row.DeclaredSHA256,
			})
		}
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
		"remaining", len(rows)-repaired,
	)
}
