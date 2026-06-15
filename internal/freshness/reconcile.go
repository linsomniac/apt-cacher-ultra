package freshness

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// ReconcileSnapshot heals a degraded current snapshot in place: it re-parses
// the snapshot's GPG-verified Release (the trust anchor, pinned + refcounted),
// fetches every requestable IndexTarget the Release declared but the snapshot
// lacks, validates each against the re-parsed declaration, and inserts it into
// the SAME snapshot. Returns the number of members healed. force=false honors
// the reconciledSnapshots memo (the per-tick caller); force=true always
// re-checks (the on-demand caller).
//
// AIDEV-NOTE: trust-sensitive path — three invariants must hold:
//  1. Trust anchor: all declared members are validated against reparseSnapshotRelease,
//     which re-reads the original GPG-verified Release blob from the pool.
//     Never skip reparseSnapshotRelease or accept an un-verified declared set.
//  2. Displacement guard: InsertReconciledMembers re-checks current_snapshot_id
//     inside its transaction. A displaced snapshot returns ErrSnapshotNotCurrent
//     and this function returns (0, nil) — benign race, not an error.
//  3. Memoization: once all requestable members are present, the snapshot ID
//     is stored in reconciledSnapshots so the per-tick caller skips it cheaply.
//     force=true (on-demand path) bypasses the memo; force=false (tick path) does not.
func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, force bool) (int, error) {
	if !force {
		if _, done := a.reconciledSnapshots.Load(snapshotID); done {
			return 0, nil
		}
	}
	// AIDEV-NOTE: reparseSnapshotRelease is the trust anchor — it reads the
	// original GPG-verified InRelease/Release blob from the pool and re-parses
	// the declared member set. This is the same blob the original adoption
	// verified; re-reading it from the pinned pool means the declared set
	// cannot be tampered with without changing the blob hash (which would
	// invalidate the snapshot_member.declared_sha256 = blob.hash invariant).
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		return 0, err // logged by caller; do not memoize
	}
	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list members: %w", err)
	}
	missing := missingRequestableMembers(declared, present, a.architectureAllowlist)
	if len(missing) == 0 {
		a.reconciledSnapshots.Store(snapshotID, struct{}{})
		return 0, nil
	}

	if a.concurrencySem != nil {
		select {
		case a.concurrencySem <- struct{}{}:
			defer func() { <-a.concurrencySem }()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	var rows []cache.SnapshotMember
	healedDecl := make([]ReleaseMember, 0, len(missing))
	for _, m := range missing {
		blobHash, ferr := a.adoptMember(ctx, suite, m, true)
		if ferr != nil {
			a.logger.Warn("adoption_reconcile_member_failed",
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
				"snapshot_id", snapshotID, "path", m.Path, "err", ferr)
			continue // leave uncompleted; next tick retries
		}
		rows = append(rows, cache.SnapshotMember{
			SnapshotID: snapshotID, Path: m.Path, BlobHash: blobHash, DeclaredSHA256: m.SHA256,
		})
		if alias := byHashAliasPath(m.Path, m.SHA256); alias != "" {
			rows = append(rows, cache.SnapshotMember{
				SnapshotID: snapshotID, Path: alias, BlobHash: blobHash, DeclaredSHA256: m.SHA256,
			})
		}
		healedDecl = append(healedDecl, m)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := a.cache.InsertReconciledMembers(ctx, snapshotID, rows, nil); err != nil {
		if errors.Is(err, cache.ErrSnapshotNotCurrent) {
			// AIDEV-NOTE: displacement during reconcile is benign — a newer
			// adoption has taken over and will carry the correct member set.
			// Log at Info (not Warn/Error) so operators can distinguish the
			// "lost the race" case from a real error.
			a.logger.Info("adoption_reconcile_snapshot_displaced",
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
				"snapshot_id", snapshotID)
			return 0, nil
		}
		return 0, fmt.Errorf("reconcile: insert: %w", err)
	}
	a.logger.Info("adoption_snapshot_reconciled",
		"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
		"snapshot_id", snapshotID, "healed", len(healedDecl))
	return len(healedDecl), nil
}

// reparseSnapshotRelease loads the snapshot's signed metadata member
// (InRelease or Release) and parses it into the declared member set.
//
// AIDEV-NOTE: this is the trust anchor for ReconcileSnapshot. The blob it
// reads was pinned at adoption time (refcounted via snapshot_member), so
// os.ReadFile reads the exact bytes the original GPG signature covered.
// Callers MUST treat the returned []ReleaseMember as the authoritative
// declared set — do not accept member declarations from any other source.
func (a *Adopter) reparseSnapshotRelease(ctx context.Context, snapshotID int64) ([]ReleaseMember, error) {
	for _, name := range []string{"InRelease", "Release"} {
		mem, err := a.cache.GetSnapshotMember(ctx, snapshotID, name)
		if errors.Is(err, cache.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile: get %s member: %w", name, err)
		}
		bytes, rerr := os.ReadFile(a.cache.BlobPath(mem.BlobHash))
		if rerr != nil {
			return nil, fmt.Errorf("reconcile: read %s blob: %w", name, rerr)
		}
		return ParseRelease(bytes)
	}
	return nil, fmt.Errorf("reconcile: snapshot %d has no InRelease/Release member", snapshotID)
}

// missingRequestableMembers returns the declared ReleaseMembers belonging
// to a requestable IndexTarget GROUP that the snapshot does not currently
// serve. It reuses missingRequestableIndexGroups (the Layer-2 serve-contract
// predicate) for the group decision — "all" always required, allowlisted
// arches when filtered — then returns every declared variant of each missing
// group so apt can fetch whichever it asks for.
//
// AIDEV-NOTE: The conversion from []cache.SnapshotMember to []ReleaseMember
// is needed because missingRequestableIndexGroups only inspects m.Path, so
// only Path is populated in the synthetic presentMembers slice. This is
// safe because missingRequestableIndexGroups only calls indexTargetGroup(m.Path).
func missingRequestableMembers(declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(declared, presentMembers, allowlist)
	if len(missingGroups) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(missingGroups))
	for _, g := range missingGroups {
		want[g] = struct{}{}
	}
	var out []ReleaseMember
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(m.Path); ok {
			if _, missing := want[group]; missing {
				out = append(out, m)
			}
		}
	}
	return out
}
