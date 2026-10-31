package freshness

import (
	"context"
	"errors"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Re-adopting the current snapshot's exact content collides with the
// adopted natural key; the cause must be visible through the chain.
func TestRun_NaturalKeyCollisionIsWrapped(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	release, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	if err := env.adopter.Run(ctx, env.suite, release, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	err := env.adopter.Run(ctx, env.suite, release, "", "")
	if !errors.Is(err, ErrAdoptionDBFailed) || !errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want both ErrAdoptionDBFailed and ErrSnapshotNaturalKeyAdopted in the chain", err)
	}
}

func TestErrAdoptionGuardFailed_KeepsMemberFetchOutcome(t *testing.T) {
	err := newMemberErr(ErrAdoptionGuardFailed, "main/binary-all/Packages", "absent")
	if !errors.Is(err, ErrAdoptionGuardFailed) || !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("chain = %v", err)
	}
	if got := classifyAdoptionOutcome(err); got != "member_fetch_failed" {
		t.Fatalf("outcome = %q, want member_fetch_failed", got)
	}
}
