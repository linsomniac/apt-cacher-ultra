package freshness

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

func TestMissingRequestableMembers(t *testing.T) {
	rm := func(path, sha string) ReleaseMember { return ReleaseMember{Path: path, SHA256: sha} }
	declared := []ReleaseMember{
		rm("main/binary-amd64/Packages", "a"),
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
		rm("main/binary-arm64/Packages", "d"), // foreign, allowlisted-out
	}
	present := []cache.SnapshotMember{{Path: "main/binary-amd64/Packages"}}
	allow := map[string]struct{}{"amd64": {}}

	got := missingRequestableMembers(declared, present, allow)
	want := []ReleaseMember{
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingRequestableMembers() = %v, want %v", got, want)
	}
}

// newReconcileAdopter constructs an Adopter over env.cache with the given
// architecture allowlist. TolerateOptionalMemberFailures is enabled so
// optional-member 404s don't abort adoption. The adopter shares env.fetcher
// so callers can seed URLs before calling Run or ReconcileSnapshot.
func newReconcileAdopter(t *testing.T, env *adoptionTestEnv, arches []string) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		Architectures:                  arches,
		TolerateOptionalMemberFailures: true,
	})
	if err != nil {
		t.Fatalf("newReconcileAdopter: %v", err)
	}
	return ad
}

// adoptCompleteSnapshot adopts a snapshot that declares AND serves both
// main/binary-amd64/Packages and main/binary-all/Packages. Returns the
// current snapshot id. The by-hash alias for binary-all is removed from
// the fetcher after adoption so reconcile tests that re-fetch it will use
// the canonical path.
func adoptCompleteSnapshot(t *testing.T, env *adoptionTestEnv, ad *Adopter) int64 {
	t.Helper()
	ctx := context.Background()
	pkgsAmd64 := fakePackagesStanzas(map[string]string{
		"pool/main/a/a/a_1_amd64.deb": strings.Repeat("a", 64),
	})
	pkgsAll := fakePackagesStanzas(map[string]string{
		"pool/main/c/c/c_1_all.deb": strings.Repeat("c", 64),
	})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgsAmd64,
		"main/binary-all/Packages":   pkgsAll,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgsAmd64)
	env.fetcher.put(base+"main/binary-all/Packages", pkgsAll)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoptCompleteSnapshot: Run: %v", err)
	}
	sf, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("adoptCompleteSnapshot: GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("adoptCompleteSnapshot: no current snapshot after Run")
	}
	return *sf.CurrentSnapshotID
}

// TestReconcileSnapshot_HealsBinaryAllInPlace verifies that ReconcileSnapshot
// heals a degraded snapshot — one missing main/binary-all/Packages — in place:
// the member is fetched, inserted into the existing snapshot, and the
// current_snapshot_id is NOT changed (no new snapshot is created).
func TestReconcileSnapshot_HealsBinaryAllInPlace(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})

	// Step 1: Adopt a COMPLETE snapshot (binary-amd64 + binary-all both served;
	// the Layer-2 guard is satisfied). Capture the snapshot id.
	snapID := adoptCompleteSnapshot(t, env, ad)

	// Step 2: Make the snapshot DEGRADED by deleting the binary-all member
	// rows (canonical path + by-hash alias). This simulates the state a
	// pre-fix adoption would have left — the snapshot is current, declares
	// binary-all in its InRelease blob, but lacks the member rows.
	//
	// The by-hash alias path for binary-all/Packages is:
	//   main/binary-all/by-hash/SHA256/<sha256hex>
	// We need to know the sha256 to derive it. Get it from the member row.
	binaryAllMember, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages")
	if err != nil {
		t.Fatalf("setup: GetSnapshotMember binary-all: %v", err)
	}
	aliasPath := "main/binary-all/by-hash/SHA256/" + binaryAllMember.DeclaredSHA256

	if err := env.cache.DeleteSnapshotMembersForTest(ctx, snapID,
		"main/binary-all/Packages", aliasPath); err != nil {
		t.Fatalf("setup: DeleteSnapshotMembersForTest: %v", err)
	}

	// Also delete the package_hash row for the arch:all .deb so the test
	// proves reconcile builds it from scratch (not inherits it from the
	// initial complete adoption). Without this, the assertion below would
	// pass trivially even with nil package hashes.
	allDeb := "/ubuntu/pool/main/c/c/c_1_all.deb"
	if err := env.cache.DeletePackageHashForTest(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, allDeb, snapID); err != nil {
		t.Fatalf("setup: DeletePackageHashForTest: %v", err)
	}

	// Verify the degraded state: binary-all must be absent now.
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err == nil {
		t.Fatal("setup: expected binary-all ABSENT after delete, but GetSnapshotMember succeeded")
	}

	// Step 3: upstream now serves binary-all (already in fetcher from adoptCompleteSnapshot).
	// Call ReconcileSnapshot — it should re-fetch and heal binary-all in place.
	healed, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false)
	if err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	if healed == 0 {
		t.Fatal("healed 0 members, want >=1")
	}

	// binary-all member must now exist in the snapshot.
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("binary-all not healed: %v", err)
	}

	// The current_snapshot_id must be UNCHANGED — reconcile is in place, not
	// a new snapshot.
	sf2, err := env.cache.GetSuiteFreshness(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness after reconcile: %v", err)
	}
	if *sf2.CurrentSnapshotID != snapID {
		t.Errorf("snapshot id changed %d -> %d (reconcile must be IN PLACE)", snapID, *sf2.CurrentSnapshotID)
	}

	// The healed arch:all index also yields package_hash rows, so arch:all
	// .debs are snapshot-hash-validated, not just served trust-upstream.
	// (allDeb was declared above in the setup section)
	if _, err := env.cache.GetPackageHash(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, allDeb, snapID); err != nil {
		t.Errorf("arch:all .deb has no package_hash after reconcile: %v", err)
	}
}

// TestReconcileSnapshot_GateAndForce locks the reconciledSnapshots memoization
// and force-bypass behavior introduced in Task 3:
//  1. Reconciling a COMPLETE snapshot (nothing missing) stores the id in the
//     memo and returns (0, nil).
//  2. A second call with force=true still runs the full check (returns (0, nil)
//     here because nothing is missing) — it does NOT short-circuit on the memo.
func TestReconcileSnapshot_GateAndForce(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})

	// Adopt a COMPLETE snapshot (binary-amd64 + binary-all both served).
	snapID := adoptCompleteSnapshot(t, env, ad)

	// First reconcile (force=false): nothing missing → memoizes and returns 0.
	if n, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err != nil || n != 0 {
		t.Fatalf("first reconcile = (%d,%v), want (0,nil)", n, err)
	}
	if _, done := ad.reconciledSnapshots.Load(snapID); !done {
		t.Error("complete snapshot not memoized after first reconcile")
	}

	// Second call with force=true: must bypass the memo and re-check.
	// The snapshot is still complete, so it should return (0, nil) without panic.
	if n, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, true); err != nil || n != 0 {
		t.Errorf("forced reconcile = (%d,%v), want (0,nil)", n, err)
	}
}

// adoptDegradedBinaryAll_ID creates a degraded snapshot — one where binary-all
// is declared in the Release but missing from snapshot_member rows — and returns
// the snapshot id. It does so by adopting a complete snapshot then deleting the
// binary-all member rows, exactly as TestReconcileSnapshot_HealsBinaryAllInPlace
// does in its setup phase. The env fetcher already has binary-all seeded from
// adoptCompleteSnapshot, so callers can reconcile immediately.
// TestReconcileSnapshot_RejectsCorruptedAnchor: the re-parsed Release is the
// reconcile trust anchor. Refcounting prevents GC, not at-rest corruption or
// tampering — so reconcile must verify the anchor pool file still hashes to
// the pinned (GPG-verified) blob hash before trusting its declarations.
func TestReconcileSnapshot_RejectsCorruptedAnchor(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})
	snapID := adoptDegradedBinaryAll_ID(t, env, ad) // degraded + healable (fetcher serves binary-all)

	// Corrupt the pinned InRelease anchor at rest: overwrite its pool file with
	// DIFFERENT but still-parseable Release bytes (so the unfixed path would
	// parse them and act on forged declarations). They no longer hash to the
	// recorded blob hash.
	inRel, err := env.cache.GetSnapshotMember(ctx, snapID, "InRelease")
	if err != nil {
		t.Fatalf("get InRelease member: %v", err)
	}
	corrupt, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": fakePackagesStanzas(map[string]string{
			"pool/main/x/x/x_9_amd64.deb": strings.Repeat("9", 64),
		}),
	})
	if err := os.WriteFile(env.cache.BlobPath(inRel.BlobHash), corrupt, 0o644); err != nil {
		t.Fatalf("corrupt anchor: %v", err)
	}

	if _, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err == nil {
		t.Error("ReconcileSnapshot accepted a corrupted (hash-mismatched) anchor blob — must reject")
	}
}

func adoptDegradedBinaryAll_ID(t *testing.T, env *adoptionTestEnv, ad *Adopter) int64 {
	t.Helper()
	ctx := context.Background()

	// Adopt a complete snapshot first.
	snapID := adoptCompleteSnapshot(t, env, ad)

	// Get the binary-all member to derive the by-hash alias path.
	binaryAllMember, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages")
	if err != nil {
		t.Fatalf("adoptDegradedBinaryAll_ID: GetSnapshotMember binary-all: %v", err)
	}
	aliasPath := "main/binary-all/by-hash/SHA256/" + binaryAllMember.DeclaredSHA256

	// Delete binary-all rows to simulate degraded state.
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, snapID,
		"main/binary-all/Packages", aliasPath); err != nil {
		t.Fatalf("adoptDegradedBinaryAll_ID: DeleteSnapshotMembersForTest: %v", err)
	}

	// Also delete the package_hash row for the arch:all .deb.
	allDeb := "/ubuntu/pool/main/c/c/c_1_all.deb"
	if err := env.cache.DeletePackageHashForTest(ctx,
		env.suite.CanonicalScheme, env.suite.CanonicalHost, allDeb, snapID); err != nil {
		t.Fatalf("adoptDegradedBinaryAll_ID: DeletePackageHashForTest: %v", err)
	}

	return snapID
}

// TestReconcileMetric_Increments verifies that acu_adoption_reconciled_total is
// registered in metrics.Default and incremented after a successful heal.
// Counter is process-global, so we assert presence + nonzero rather than an
// exact value.
func TestReconcileMetric_Increments(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})
	snapID := adoptDegradedBinaryAll_ID(t, env, ad) // Task-3 helper (id only)
	// env.fetcher already has binary-all seeded from adoptCompleteSnapshot.
	if _, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	if !strings.Contains(buf.String(), "acu_adoption_reconciled_total") {
		t.Errorf("acu_adoption_reconciled_total not present after a heal:\n%s", buf.String())
	}
}
