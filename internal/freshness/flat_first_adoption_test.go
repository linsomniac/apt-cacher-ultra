package freshness

import (
	"context"
	"maps"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// flatGz is a body that passes the flat index sniff for a .gz name: the
// gzip magic and a header.
var flatGz = []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03}

// Spec C4 Dispatch 3 (the E carry-forward; plan design note 8): a flat
// suite's client-created root Packages* or Sources* row is evidence of
// interest beside the InRelease and Release rows, so a Release-less
// repository's suite — created by its index's serve — runs the probe tree
// on its first check. One whose only row is an index no client fetched (a
// prefetch or a reconstruction: request_count 0), a flat Contents-*, or —
// under a dists suite — a root Packages, stays dormant: no request.
func TestCheck_FlatIndexRowsCountAsClientCreated(t *testing.T) {
	for _, tc := range []struct {
		name, suite, file string
		requests          int64
		probes            bool
	}{
		{"a client-fetched Packages.gz", "/fury", "Packages.gz", 1, true},
		{"a client-fetched Sources.gz", "/fury", "Sources.gz", 3, true},
		{"its ./ twin's Packages.gz", "/fury/.", "Packages.gz", 1, true},
		{"an index no client fetched", "/fury", "Packages.gz", 0, false},
		{"a flat Contents-*", "/fury", "Contents-amd64.gz", 1, false},
		{"a dists suite's root Packages.gz", "/r/dists/x", "Packages.gz", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFlatRig(t, tc.suite)
			r.cacheRow(tc.file, flatGz, tc.requests, 1_799_999_000, "")
			r.check()
			if got := r.requests("InRelease") > 0; got != tc.probes {
				t.Fatalf("InRelease probed = %v, want %v", got, tc.probes)
			}
		})
	}
}

// Spec C4 Dispatch 3 for a signed flat suite: its client-created InRelease
// makes it interested, and its first check adopts (eager first adoption)
// with the flat layout's members — the root Packages fetched under the
// suite — and C6's rows at its FlatBase. The `./` twin adopts its own
// snapshot the same way (ruling R-G4).
func TestCheck_AFlatSuiteAdoptsOnItsFirstCheck(t *testing.T) {
	for _, suitePath := range []string{"/core:/stable:/v1.34/deb", "/core:/stable:/v1.34/deb/."} {
		t.Run(suitePath, func(t *testing.T) {
			r := newFlatRig(t, suitePath)
			pkgs := fakePackagesStanzas(map[string]string{"amd64/kubectl_1.34.1-1.1_amd64.deb": testHex64})
			rel, _ := makeRelease(map[string][]byte{"Packages": pkgs})
			in := simtest.ClearsignedFixture(string(rel))
			r.cacheRow("InRelease", in, 1, 1_799_999_000, "")
			r.up.setAt(r.url("InRelease"), in)
			r.members.put(r.url("Packages"), pkgs)
			r.check()
			row := r.row()
			if r.lastResult() != "first_adoption" || row.CurrentSnapshotID == nil {
				t.Fatalf("result %q, current %v; want a first adoption", r.lastResult(), row.CurrentSnapshotID)
			}
			got := packageHashRows(t, r.cache, *row.CurrentSnapshotID)
			want := map[string]string{"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb": "amd64/kubectl_1.34.1-1.1_amd64.deb"}
			if !maps.Equal(got, want) {
				t.Fatalf("package_hash rows = %v, want %v", got, want)
			}
			if rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", flatRigHost,
				"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.1-1.1_amd64.deb"); err != nil || len(rows) != 1 || rows[0].SuitePath != suitePath {
				t.Fatalf("the .deb is vouched by %v (%v), want one row naming %s", rows, err, suitePath)
			}
		})
	}
}
