package freshness

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
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
// prefetch or a reconstruction: request_count 0), a flat Contents-*, a
// Release.gpg alone (plan design note 14), or — under a dists suite — a
// root Packages, stays dormant: no request.
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
		{"a flat Release.gpg alone", "/fury", "Release.gpg", 1, false}, // plan design note 14
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

// Ruling PG-20 (spec C1: new probe code derives sibling URLs from a stored
// upstream_url where one exists): a flat suite active only through a
// client-fetched root index row probes its anchors beside that row's
// stored upstream_url, so a non-default port survives — the canonical
// host is port-less, and urlReconstruct would ask port 80, a service the
// client never named. The anchorless refresh then asks the index at its
// own stored URL.
func TestCheck_AnIndexOnlyFlatSuiteProbesBesideItsIndexRow(t *testing.T) {
	r := newFlatRig(t, "/fury")
	h := poolBlob(t, r.cache, flatGz)
	fetched := int64(1_799_999_000)
	if err := r.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost,
		Path: "/fury/Packages.gz", BlobHash: &h, UpstreamURL: "http://fh:8080/fury/Packages.gz", IsMetadata: true,
		RequestCount: 1, LastFetchedAt: &fetched, LastRequestedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	r.check()
	for _, u := range []string{"http://fh:8080/fury/InRelease", "http://fh:8080/fury/Release", "http://fh:8080/fury/Packages.gz"} {
		if n := len(r.up.etagsSent(u)); n != 1 {
			t.Errorf("%s asked %d times, want once", u, n)
		}
	}
	for _, name := range []string{"InRelease", "Release"} {
		if n := r.requests(name); n != 0 {
			t.Errorf("%s asked %d times at the port-less %s, want never", name, n, r.url(name))
		}
	}
}

// Plan design note 8's flat read through the adopter's cache: when it
// fails, checkFirst follows its anchor-row read's convention — one WARN
// naming the suite, no request and no write (no record, no backoff), a
// local fault retried at the next check — and never treats the fault as
// interest (a probe) or as dormancy (a Debug line).
func TestCheck_AFailedFlatIndexReadIsALocalFault(t *testing.T) {
	r := newFlatRig(t, "/fury")
	r.cacheRow("Packages.gz", flatGz, 1, 1_799_999_000, "")
	closed, err := cache.Open(context.Background(), t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	r.checker.adopter.cache = closed // the checker's own reads stay on r.cache
	r.check()
	warns := 0
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m["msg"] {
		case "freshness: read anchor rows failed":
			if m["level"] != "WARN" || m["suite_path"] != "/fury" {
				t.Fatalf("read failure line %v, want a WARN naming /fury", m)
			}
			warns++
		case "freshness: never-adopted suite without a client-created anchor or flat index row, dormant":
			t.Fatalf("the read fault was taken for dormancy: %v", m)
		}
	}
	if warns != 1 {
		t.Fatalf("%d read failure WARNs, want 1", warns)
	}
	if n := len(r.up.sentURLs()); n != 0 {
		t.Fatalf("%d upstream requests, want none", n)
	}
	if row := r.row(); row.LastCheckAt != nil || row.CheckFailCount != 0 || row.AdoptionFailReason != nil {
		t.Fatalf("suite row %+v, want no check stamp, no streak and no record", row)
	}
}

// sentURLs lists every URL the rig upstream was asked, in no order.
func (u *rigUpstream) sentURLs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for url := range u.sent {
		out = append(out, url)
	}
	return out
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
