package freshness

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
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
