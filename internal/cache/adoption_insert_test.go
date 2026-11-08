package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A failure after an earlier INSERT succeeded must roll back every batch and
// leave the writer usable for a retry of the same candidate.
func TestCommitAdoption_InsertFailureRollsBackAndRetries(t *testing.T) {
	for _, stage := range []string{"member", "skipped", "package"} {
		for _, failure := range []string{"invalid_hash", "duplicate_path"} {
			t.Run(stage+"/"+failure, func(t *testing.T) {
				c := openCache(t)
				ctx := context.Background()
				hash := seedBlob(t, c, "release and member content")
				priorHash := seedBlob(t, c, "prior release content")
				candidate := SnapshotCandidate{
					CanonicalScheme: "https", CanonicalHost: "mirror.example",
					SuitePath: "/dists/stable", InReleaseHash: &priorHash,
				}
				prior, _, err := c.InsertTrackedCandidateForTest(ctx, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.CommitAdoption(ctx, prior, nil, nil, nil, nil, false); err != nil {
					t.Fatal(err)
				}
				candidate.InReleaseHash = &hash
				id, _, err := c.InsertTrackedCandidateForTest(ctx, candidate)
				if err != nil {
					t.Fatal(err)
				}
				members := []SnapshotMember{
					{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash},
					{Path: "main/binary-amd64/Packages", BlobHash: hash, DeclaredSHA256: hash},
				}
				skipped := []SkippedMember{
					{Path: "main/Contents-amd64.gz", DeclaredSHA256: hash, Size: 42, Reason: "4xx"},
					{Path: "main/Contents-arm64.gz", DeclaredSHA256: hash, Size: 84, Reason: "arch_not_in_allowlist"},
				}
				packages := []PackageHash{
					{CanonicalScheme: "https", CanonicalHost: "mirror.example", Path: "/pool/a.deb", DeclaredSHA256: hash},
					{CanonicalScheme: "https", CanonicalHost: "mirror.example", Path: "/pool/b.deb", DeclaredSHA256: strings.Repeat("b", 64)},
				}
				badMembers := append([]SnapshotMember(nil), members...)
				badSkipped := append([]SkippedMember(nil), skipped...)
				badPackages := append([]PackageHash(nil), packages...)
				switch stage {
				case "member":
					if failure == "invalid_hash" {
						badMembers[1].DeclaredSHA256 = "invalid"
					} else {
						badMembers[1].Path = badMembers[0].Path
					}
				case "skipped":
					if failure == "invalid_hash" {
						badSkipped[1].DeclaredSHA256 = "invalid"
					} else {
						badSkipped[1].Path = badSkipped[0].Path
					}
				case "package":
					if failure == "invalid_hash" {
						badPackages[1].DeclaredSHA256 = "invalid"
					} else {
						badPackages[1].Path = badPackages[0].Path
					}
				}
				err = c.CommitAdoption(ctx, id, badMembers, badSkipped, badPackages, nil, true)
				if err == nil {
					t.Fatal("invalid batch adopted")
				}
				if failure == "invalid_hash" && !errors.Is(err, ErrInvalidHash) {
					t.Fatalf("got %v, want ErrInvalidHash", err)
				}
				assertCounts := func(want int) {
					t.Helper()
					for _, table := range []string{"snapshot_member", "snapshot_skipped_member", "package_hash"} {
						var got int
						if err := c.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE snapshot_id = ?", id).Scan(&got); err != nil {
							t.Fatal(err)
						}
						if got != want {
							t.Errorf("%s rows = %d, want %d", table, got, want)
						}
					}
				}
				assertCounts(0)
				snapshot, err := c.GetSuiteSnapshot(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.AdoptedAt != nil || snapshot.PackageCoverageComplete {
					t.Fatal("failed adoption changed candidate state")
				}
				suite, err := c.GetSuiteFreshness(ctx, candidate.CanonicalScheme, candidate.CanonicalHost, candidate.SuitePath)
				if err != nil {
					t.Fatal(err)
				}
				if suite.CurrentSnapshotID == nil || *suite.CurrentSnapshotID != prior {
					t.Fatal("failed adoption displaced the prior snapshot")
				}
				if got := blobRefcount(t, c, hash); got != 0 {
					t.Fatalf("refcount after rollback = %d, want 0", got)
				}
				if err := c.CommitAdoption(ctx, id, members, skipped, packages, nil, true); err != nil {
					t.Fatalf("retry after rollback: %v", err)
				}
				assertCounts(2)
				for _, want := range packages {
					got, err := c.GetPackageHash(ctx, want.CanonicalScheme, want.CanonicalHost, want.Path, id)
					if err != nil {
						t.Fatal(err)
					}
					if got.DeclaredSHA256 != want.DeclaredSHA256 {
						t.Errorf("%s hash = %s, want %s", want.Path, got.DeclaredSHA256, want.DeclaredSHA256)
					}
				}
			})
		}
	}
}
