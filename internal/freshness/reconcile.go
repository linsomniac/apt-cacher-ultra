package freshness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
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
	return a.reconcileSnapshot(ctx, suite, snapshotID, nil, force)
}

// reconcileSnapshot is ReconcileSnapshot; a non-nil declared is the
// snapshot's declared set its caller already parsed from the same pinned
// anchor (reconcilePending on the tick), so the Release is parsed once per
// heal (deferred minor B-28).
func (a *Adopter) reconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, declared []ReleaseMember, force bool) (int, error) {
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
	missing := missingRequestableMembers(declared, present, a.architectureAllowlist)
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
	// (trust anchor, GPG-verified — not client-controlled, so no label bound needed).
	for _, m := range healedDecl {
		if _, arch, ok := indexTargetGroup(m.Path); ok {
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
// admission slot, inside the check's recover.
func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) ([]ReleaseMember, bool, error) {
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
	if len(missingRequestableMembers(declared, present, a.architectureAllowlist)) == 0 {
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
// inReleasePlaintext limits the parse to the signed plaintext (R-1). A
// detached Release is covered whole by its Release.gpg and is parsed
// as-is.
// Callers MUST treat the returned []ReleaseMember as the authoritative
// declared set — do not accept member declarations from any other source.
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
		// address, == the GPG-verified suite_snapshot anchor hash by adoption's
		// construction, so this binds the parse to the verified anchor.
		sum := sha256.Sum256(blob)
		if got := hex.EncodeToString(sum[:]); got != mem.BlobHash {
			return nil, fmt.Errorf("reconcile: %s anchor blob hash mismatch (file=%s pinned=%s) — corrupt at rest",
				name, got, mem.BlobHash)
		}
		if name == "InRelease" {
			text, err := inReleasePlaintext(blob)
			if err != nil {
				return nil, err
			}
			return ParseRelease(text)
		}
		return ParseRelease(blob)
	}
	return nil, fmt.Errorf("reconcile: snapshot %d has no InRelease/Release member", snapshotID)
}

// inReleasePlaintext returns the signed plaintext of a stored InRelease
// without re-verifying the signature (the bytes were verified at adoption
// and are content-addressed; re-verification would also fail after a
// legitimate keyring change).
//
// AIDEV-NOTE: R-1. Never ParseRelease the raw envelope: lines placed
// after a forged "=CRC" line inside the signature armor, or as
// colon-bearing armor headers, are part of the stored bytes but were never
// signed (both verify with go-crypto v1.4.1). A body without a clearsign
// marker is an error since phase D (the plain-text fallback served only
// test fixtures; VerifyInline never adopts plain text, and the startup
// audit quarantines legacy plain-text snapshots — one it deferred fails
// its reconcile here, closed, until a later start audits it). The marker
// test is bytes.Contains, never HasPrefix: snapshots adopted before phase
// D may hold whitespace before the marker (ruling R-D2 now refuses that at
// adoption). A decode failure is an error, never a raw parse; the decode
// sees aptpath.DecodableClearsign(blob) (R-D2) and runs under recover():
// a stored blob cannot trigger I-1, but reconcile must never crash the
// daemon.
func inReleasePlaintext(blob []byte) (text []byte, err error) {
	if !bytes.Contains(blob, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		return nil, errors.New("reconcile: stored InRelease is not clearsigned")
	}
	defer func() {
		if r := recover(); r != nil {
			text, err = nil, fmt.Errorf("reconcile: clearsign decode panicked: %v", r)
		}
	}()
	block, _ := clearsign.Decode(aptpath.DecodableClearsign(blob))
	if block == nil {
		return nil, errors.New("reconcile: stored InRelease is not a valid clearsigned message")
	}
	return block.Plaintext, nil
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
