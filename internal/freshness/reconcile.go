package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// ReconcileSnapshot heals a degraded current snapshot in place: it re-parses
// the snapshot's adopted Release (the trust anchor, pinned + refcounted:
// GPG-verified for the inline and detached forms; for an unsigned snapshot
// the Release nothing verifies, which is why such a snapshot never vouches),
// fetches every requestable IndexTarget the Release declared but the snapshot
// lacks, validates each against the re-parsed declaration, and inserts it into
// the SAME snapshot. Returns the number of members healed. force=false honors
// the reconciledSnapshots memo (the per-tick caller); force=true always
// re-checks (the on-demand caller).
//
// AIDEV-NOTE: trust-sensitive path — three invariants must hold:
//  1. Trust anchor: all declared members are validated against the declared
//     set reparseSnapshotRelease returns for this snapshotID, which re-reads
//     the original adopted Release blob from the pool (GPG-verified unless
//     the snapshot is unsigned, phase E: then it is exactly the bytes the
//     unsigned adoption accepted, and its package_hash rows are never
//     built, buildPackageHashes taking the form) — parsed here, or
//     handed down by reconcilePending, which parsed the same snapshot on the
//     same tick (reconcileSnapshot's declared, deferred minor B-28). Never
//     accept a declared set from any other source, and never pass down a set
//     parsed for another snapshot.
//  2. Displacement guard: InsertReconciledMembers re-checks current_snapshot_id
//     inside its transaction. A displaced snapshot returns ErrSnapshotNotCurrent
//     and this function returns (0, nil) — benign race, not an error.
//  3. Memoization: once all requestable members are present, the snapshot ID
//     is stored in reconciledSnapshots so the per-tick caller skips it cheaply.
//     force=true (on-demand path) bypasses the memo; force=false (tick path) does not.
func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, force bool) (int, error) {
	return a.reconcileSnapshot(ctx, suite, snapshotID, nil, force)
}

// sweepReconcileMemo drops every reconciledSnapshots and
// reconcileFailedSnapshots entry whose snapshot is not in current — the
// suites' current snapshots, as the reaper's run listed them (spec C9) —
// and reports how many it dropped. A displaced snapshot never becomes
// current again, so its entry is dead; one adopted after the listing may
// lose a fresh entry, which costs it one extra parse of its Release on its
// next unchanged check (or one more WARN of a failing decision).
func (a *Adopter) sweepReconcileMemo(current map[int64]bool) int {
	n := 0
	for _, memo := range []*sync.Map{&a.reconciledSnapshots, &a.reconcileFailedSnapshots} {
		memo.Range(func(k, _ any) bool {
			if id, ok := k.(int64); !ok || !current[id] {
				memo.Delete(k)
				n++
			}
			return true
		})
	}
	return n
}

// firstReconcileFailure records that snapshotID's reconcile decision failed
// and reports whether it is the first failure since the process started or
// since a decision on it last succeeded (the one logged at WARN).
func (a *Adopter) firstReconcileFailure(snapshotID int64) bool {
	_, seen := a.reconcileFailedSnapshots.LoadOrStore(snapshotID, struct{}{})
	return !seen
}

// clearReconcileFailure forgets snapshotID's failed decision: a decision on
// it succeeded, so a failure that recurs is said again.
func (a *Adopter) clearReconcileFailure(snapshotID int64) {
	a.reconcileFailedSnapshots.Delete(snapshotID)
}

// reconcileSnapshot is ReconcileSnapshot; a non-nil declared must be
// reparseSnapshotRelease's output for this same snapshotID, which its
// caller already parsed from the same pinned anchor (reconcilePending on
// the tick, through spawnRepair), so the Release is parsed once per heal
// (deferred minor B-28). Nothing ties declared to snapshotID but that
// contract: a set from any other source, or another snapshot's, would
// heal members the snapshot's own Release never declared.
func (a *Adopter) reconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, declared []ReleaseMember, force bool) (int, error) {
	if !force {
		if _, done := a.reconciledSnapshots.Load(snapshotID); done {
			return 0, nil
		}
	}
	// AIDEV-NOTE: reparseSnapshotRelease is the trust anchor — it reads the
	// original adopted InRelease/Release blob from the pool and re-parses
	// the declared member set. This is the same blob the original adoption
	// verified (or, for an unsigned snapshot, accepted unverified: it heals
	// members but never vouches a .deb); re-reading it from the pinned pool
	// means the declared set cannot be tampered with without changing the
	// blob hash (which would
	// invalidate the snapshot_member.declared_sha256 = blob.hash invariant).
	// The set is parsed here, or it is the one reconcilePending parsed for
	// this snapshotID on the same tick (B-28) — never a set from any other
	// source.
	if declared == nil {
		var err error
		if declared, err = a.reparseSnapshotRelease(ctx, snapshotID); err != nil {
			return 0, err // logged by caller; do not memoize
		}
	}
	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list members: %w", err)
	}
	missing := missingRequestableMembers(suite.SuitePath, declared, present, a.architectureAllowlist)
	if len(missing) == 0 {
		a.reconciledSnapshots.Store(snapshotID, struct{}{})
		return 0, nil
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
		// Spec C5: the member and an alias per declared algorithm, bound
		// to the bytes adoptMember verified against every declared hash.
		rows = append(rows, memberRows(snapshotID, m, blobHash)...)
		healedDecl = append(healedDecl, verifiedMember(m, blobHash))
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// Build package_hash rows for the healed indexes (declared set = full
	// re-parsed Release so coverage math matches adoption; fetched set =
	// the indexes we just healed). The snapshot's form decides: an
	// unsigned snapshot gets none (spec C5 safeguard 5).
	snap, err := a.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: read snapshot: %w", err)
	}
	phRes, err := a.buildPackageHashes(suite, snapshotID, snap.Form, declared, healedDecl)
	if err != nil {
		return 0, fmt.Errorf("reconcile: build package hashes: %w", err)
	}
	if err := a.cache.InsertReconciledMembers(ctx, snapshotID, rows, phRes.rows); err != nil {
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
	// Increment per healed member, keyed by the arch declared in the Release
	// (the trust anchor — GPG-verified, or for an unsigned snapshot the
	// Release its adoption accepted — not client-controlled, so no label
	// bound needed).
	for _, m := range healedDecl {
		if _, arch, ok := indexTargetGroup(suite.SuitePath, m.Path); ok {
			adoptionReconciledTotal.Inc(arch)
		}
	}
	return len(healedDecl), nil
}

// reconcilePending reports whether ReconcileSnapshot(force=false) would
// fetch anything and, when it would, the declared set it parsed (the heal
// reuses it: B-28). A memoised snapshot costs nothing; otherwise the
// trust-anchored Release is re-parsed once, and a complete snapshot is
// memoised so later checks skip it. The Checker asks before it takes an
// admission slot, inside the check's recover. suite is the snapshot's
// suite: it makes the requestable groups layout-aware (a flat suite's
// root Packages is required, spec C5).
func (a *Adopter) reconcilePending(ctx context.Context, suite SuiteRef, snapshotID int64) ([]ReleaseMember, bool, error) {
	if a.reconcilePendingHook != nil {
		a.reconcilePendingHook()
	}
	if _, done := a.reconciledSnapshots.Load(snapshotID); done {
		return nil, false, nil
	}
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		return nil, false, err
	}
	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return nil, false, fmt.Errorf("reconcile: list members: %w", err)
	}
	if len(missingRequestableMembers(suite.SuitePath, declared, present, a.architectureAllowlist)) == 0 {
		a.reconciledSnapshots.Store(snapshotID, struct{}{})
		return nil, false, nil
	}
	return declared, true, nil
}

// reparseSnapshotRelease loads the snapshot's signed metadata member
// (InRelease or Release) and parses it into the declared member set.
//
// AIDEV-NOTE: this is the trust anchor for ReconcileSnapshot. The blob it
// reads was pinned at adoption time (refcounted via snapshot_member), and
// the hash check below proves the stored envelope is intact at rest: the
// exact bytes adoption verified. Intact is not the same as signed: an
// InRelease envelope also carries regions outside the signature (armor
// headers, lines after the armor CRC), so for InRelease
// inReleaseMessage limits the parse to the signed text, M, as adoption
// parsed it (R-1; V-RI1). A detached Release is covered whole by its
// Release.gpg and is parsed as-is.
// Callers MUST treat the returned []ReleaseMember as the authoritative
// declared set — do not accept member declarations from any other source.
// Handing the set down within one tick (reconcilePending → spawnRepair →
// reconcileSnapshot, B-28) is not another source: it is this function's
// output for the same snapshotID.
func (a *Adopter) reparseSnapshotRelease(ctx context.Context, snapshotID int64) ([]ReleaseMember, error) {
	if a.reparseHook != nil {
		a.reparseHook()
	}
	for _, name := range []string{"InRelease", "Release"} {
		mem, err := a.cache.GetSnapshotMember(ctx, snapshotID, name)
		if errors.Is(err, cache.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile: get %s member: %w", name, err)
		}
		blob, rerr := os.ReadFile(a.cache.BlobPath(mem.BlobHash))
		if rerr != nil {
			return nil, fmt.Errorf("reconcile: read %s blob: %w", name, rerr)
		}
		// Trust-anchor integrity: the pool file must still hash to the pinned
		// content address. Refcounting prevents GC, not at-rest corruption or
		// local tampering — an anchor whose bytes no longer match its hash
		// could define FORGED member declarations, so refuse to parse it.
		// mem.BlobHash is the InRelease/Release member's recorded content
		// address, == the suite_snapshot anchor hash by adoption's
		// construction (GPG-verified, unless the snapshot is unsigned), so
		// this binds the parse to the anchor adoption accepted.
		sum := sha256.Sum256(blob)
		if got := hex.EncodeToString(sum[:]); got != mem.BlobHash {
			return nil, fmt.Errorf("reconcile: %s anchor blob hash mismatch (file=%s pinned=%s) — corrupt at rest",
				name, got, mem.BlobHash)
		}
		if name == "InRelease" {
			text, err := inReleaseMessage(blob)
			if err != nil {
				return nil, err
			}
			return ParseRelease(text)
		}
		return ParseRelease(blob)
	}
	return nil, fmt.Errorf("reconcile: snapshot %d has no InRelease/Release member", snapshotID)
}

// inReleaseMessage returns M, the signed text of a stored InRelease as
// apt's splitter reads it (internal/apttext, layer T), without
// re-verifying the signature (the bytes were verified at adoption and are
// content-addressed; re-verification would also fail after a legitimate
// keyring change). It is what the verifier returned at adoption and the
// adopter parsed the snapshot's members from: layer T's Inline adds
// refusals to apt's split and never changes M.
//
// AIDEV-NOTE: R-1. Never ParseRelease the raw envelope: lines placed
// after a forged "=CRC" line inside the signature armor, or as
// colon-bearing armor headers, are part of the stored bytes but were never
// signed. M is the signed text alone: the split stops at the signature's
// BEGIN line and drops the cleartext headers. A body without a clearsign
// marker on its first line is an error (the plain-text fallback is gone
// since phase D: VerifyInline never adopts plain text, and the startup
// audit quarantines legacy plain-text snapshots — one it deferred fails
// its reconcile here, closed, until a later start audits it), and so is
// any other envelope apt's split refuses: never a raw parse.
//
// AIDEV-NOTE: the re-review's V-RI1. Until Task 9b's fix round this read
// go-crypto's clearsign plaintext (with aptpath.DecodableClearsign for a
// marker line with trailing blanks, ruling R-D2) while adoption parsed
// layer T's M: they disagreed on anchors the hybrid verifier adopts (a
// cleartext "Comment:" header, CRs before a line's end, blanks after the
// signature's BEGIN line), so reconcile failed on every tick of such a
// snapshot and never healed its missing members
// (TestReconcile_ReadsEveryAdoptedCorpusAnchorAsAdoptionDid). A snapshot
// adopted before phase D with whitespace before its marker, which apt's
// split refuses (and so does every client's apt), no longer reconciles:
// it fails closed, as above, until the next adoption replaces it. The
// startup audit still reads legacy envelopes as their adoption did
// (legacy_audit.go, inReleasePlaintext). The split runs under recover():
// a stored blob cannot make it panic, but reconcile must never crash the
// daemon.
func inReleaseMessage(blob []byte) (text []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = nil, fmt.Errorf("reconcile: splitting the stored InRelease panicked: %v", r)
		}
	}()
	cs, err := apttext.SplitClearsigned(blob)
	switch {
	case errors.Is(err, apttext.ErrNotClearsigned):
		return nil, errors.New("reconcile: stored InRelease is not clearsigned")
	case err != nil:
		return nil, fmt.Errorf("reconcile: stored InRelease is not a valid clearsigned message: %w", err)
	}
	return cs.Message, nil
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
// safe because missingRequestableIndexGroups only calls
// indexTargetGroup(suitePath, m.Path). suitePath makes the groups
// layout-aware (spec C5: a flat suite's root Packages is required).
func missingRequestableMembers(suitePath string, declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(suitePath, declared, presentMembers, allowlist)
	if len(missingGroups) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(missingGroups))
	for _, g := range missingGroups {
		want[g] = struct{}{}
	}
	var out []ReleaseMember
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(suitePath, m.Path); ok {
			if _, missing := want[group]; missing {
				out = append(out, m)
			}
		}
	}
	return out
}
