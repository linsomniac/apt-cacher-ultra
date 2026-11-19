package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkCommitAdoptionLargeCatalog models the 420,000 package rows from
// issue #27. Each iteration uses a fresh database; setup and row construction
// are excluded so comparisons measure the adoption transaction itself.
func BenchmarkCommitAdoptionLargeCatalog(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	packages := make([]PackageHash, 420000)
	for i := range packages {
		packages[i] = PackageHash{
			CanonicalScheme: "https", CanonicalHost: "mirror.example",
			Path: fmt.Sprintf("/pool/pkg-%06d.deb", i), DeclaredSHA256: hash,
			PackageName: fmt.Sprintf("pkg-%06d", i), Architecture: "amd64", Version: "1.0",
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c, err := Open(ctx, b.TempDir(), discardSlog())
		if err != nil {
			b.Fatal(err)
		}
		// Also close on an early benchmark failure; Close is idempotent.
		b.Cleanup(func() { _ = c.Close() })
		if err := c.PutBlob(ctx, hash, 1); err != nil {
			b.Fatal(err)
		}
		id, _, err := c.InsertTrackedCandidateForTest(ctx, SnapshotCandidate{
			CanonicalScheme: "https", CanonicalHost: "mirror.example",
			SuitePath: "/dists/stable", InReleaseHash: &hash,
		})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		err = c.CommitAdoption(ctx, id, nil, nil, packages, nil, true)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		rows, err := c.PackageRowsOfSnapshotForTest(ctx, id)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != len(packages) {
			b.Fatalf("package rows = %d, want %d", len(rows), len(packages))
		}
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
