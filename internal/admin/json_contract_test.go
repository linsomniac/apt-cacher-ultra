package admin

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AIDEV-NOTE: TestJSONContractPreserved is the DoD #2 regression gate
// for docs/admin-ui-spec.md §0.1 / §0.4 — the HTML redesign must not
// bleed into the JSON wire shape. The test calls writeStatusJSON
// directly (the same helper renderStatus's JSON branch uses) so a
// regression in the encoder configuration is caught here as well as in
// /?format=json. Goldens live under testdata/; regenerate with `-update`.

var jsonContractUpdate = flag.Bool("update.jsoncontract", false,
	"regenerate testdata/json_contract_*_golden.json from the in-test models")

func TestJSONContractPreserved(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		model  func() statusModel
	}{
		{"populated", "json_contract_golden.json", populatedJSONContractFixture},
		{"minimal", "json_contract_minimal_golden.json", minimalJSONContractFixture},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeStatusJSON(&buf, c.model()); err != nil {
				t.Fatalf("writeStatusJSON: %v", err)
			}
			got := buf.Bytes()

			goldenPath := filepath.Join("testdata", c.golden)
			if *jsonContractUpdate {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("updated %s (%d bytes)", goldenPath, len(got))
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (regenerate with -update.jsoncontract): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("JSON contract drifted (DoD #2 / §0.1).\n%s\nRegenerate intentionally with: go test ./internal/admin -run TestJSONContractPreserved -update.jsoncontract",
					unifiedLineDiff(string(want), string(got), goldenPath, "actual"))
			}
		})
	}

	// Belt-and-suspenders: HTML-only wrapper fields (htmlRenderModel)
	// must NEVER appear in the JSON. If a future refactor accidentally
	// passes htmlRenderModel to the JSON encoder, this trips.
	t.Run("no_html_only_fields", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeStatusJSON(&buf, populatedJSONContractFixture()); err != nil {
			t.Fatalf("writeStatusJSON: %v", err)
		}
		body := buf.String()
		for _, leaked := range []string{"AdoptionEnabled", "GCIntervalSeconds"} {
			if strings.Contains(body, leaked) {
				t.Errorf("HTML-only field %q leaked into JSON wire shape", leaked)
			}
		}
	})
}

// unifiedLineDiff emits a minimal unified-style line diff so a failing
// test is self-diagnostic without writing a temp file the harness then
// cleans up. Lines unique to want are prefixed `-`; unique to got are
// `+`. Order is preserved by walking want and got in lockstep.
func unifiedLineDiff(want, got, wantLabel, gotLabel string) string {
	wl := strings.SplitAfter(want, "\n")
	gl := strings.SplitAfter(got, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", wantLabel, gotLabel)
	i, j := 0, 0
	context := 2
	for i < len(wl) || j < len(gl) {
		switch {
		case i < len(wl) && j < len(gl) && wl[i] == gl[j]:
			i++
			j++
		case i < len(wl) && (j >= len(gl) || wl[i] != gl[j]):
			start := max(0, i-context)
			for k := start; k < i; k++ {
				fmt.Fprintf(&b, " %s", wl[k])
			}
			fmt.Fprintf(&b, "-%s", wl[i])
			if j < len(gl) {
				fmt.Fprintf(&b, "+%s", gl[j])
				j++
			}
			i++
		case j < len(gl):
			fmt.Fprintf(&b, "+%s", gl[j])
			j++
		}
	}
	return b.String()
}

// populatedJSONContractFixture is the maximal representative model:
// every top-level key, every pointer-nullable field's populated branch,
// keyring entries from both bundled and custom sources, and a
// recent-adoptions slice with both success and gpg_failed outcomes.
func populatedJSONContractFixture() statusModel {
	gcLast := int64(1_700_000_000)
	hitRate := 92.5
	suiteCheck := int64(1_700_005_000)
	suiteSuccess := int64(1_700_004_000)
	snapID := int64(42)
	snapAdopted := int64(1_700_003_500)
	releaseSeen := int64(1_700_005_500)
	caExpiry := int64(1_900_000_000)
	certIssued := int64(1_700_006_000)

	return statusModel{
		Process: processInfo{
			Version:         "v0.test",
			StartedUnixTime: 1_700_000_000,
			UptimeSeconds:   7200,
			VCSRevision:     "deadbeef",
			GoVersion:       "go-test",
		},
		Cache: cacheInfo{
			Dir:                 "/var/cache/apt-cacher-ultra",
			BytesUsed:           1024 * 1024 * 512,
			BlobCount:           4321,
			URLPathCount:        4500,
			ZeroRefcountBacklog: 12,
		},
		CacheSummary: cacheSummary{
			ByHost: map[string]cacheSummaryHost{
				"archive.ubuntu.com": {
					ByArchitecture: map[string]cacheSummaryArchEntry{
						"amd64": {PackageHashCount: 100, BlobCount: 80, BlobBytes: 100_000_000},
						"arm64": {PackageHashCount: 50, BlobCount: 40, BlobBytes: 50_000_000},
					},
				},
			},
		},
		RepoCoverage: repoCoverageInfo{
			ArchitecturesSeen:    []string{"amd64", "arm64"},
			ArchitecturesFilter:  []string{"amd64"},
			SnapshotsWithSources: 12,
			SnapshotsWithPdiff:   3,
			PackageHashRows: packageHashRowsInfo{
				Binary: 150,
				Source: 8,
				Pdiff:  2,
				Total:  160,
			},
		},
		Listeners: []listenerInfo{
			{Role: "proxy", Addr: "0.0.0.0:3142"},
			{Role: "admin", Addr: "127.0.0.1:6789"},
		},
		TLSMITM: &tlsMITMInfo{
			Enabled:             true,
			CASource:            "generated",
			CAFingerprintSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			CANotAfterUnixTime:  caExpiry,
			EffectiveAllowlist:  ".*\\.example\\.com",
			CertCache:           certCacheInfo{Size: 5, Capacity: 1024},
			LastIssued:          &lastIssuedInfo{Host: "packages.example.com", AtUnixTime: certIssued},
			HitRate60sPercent:   &hitRate,
			HitRate60sObserved:  800,
		},
		Suites: []suiteEntry{
			{
				Host:                             "archive.ubuntu.com",
				SuitePath:                        "dists/noble",
				LastCheckUnixTime:                &suiteCheck,
				LastSuccessUnixTime:              &suiteSuccess,
				CurrentSnapshotID:                &snapID,
				CurrentSnapshotAdoptedAtUnixTime: &snapAdopted,
				InReleaseChangeSeenAtUnixTime:    &releaseSeen,
				Lagging:                          "(lagging 30m)",
			},
		},
		GC: &gcInfo{
			LastRunUnixTime:         &gcLast,
			LastRunPhase:            "periodic",
			LastRunBlobsReaped:      12,
			LastRunBytesReclaimed:   1024 * 1024,
			LastRunDeadlineReached:  false,
			LastRunDurationSeconds:  1.5,
			OrphanCandidatesReaped:  2,
			DisplacedReaped:         1,
			PoolOrphansRepaired:     0,
			PoolOrphanBytesRepaired: 0,
			PoolUnlinkErrors:        0,
		},
		HotURLPaths: []hotURLEntry{
			{
				Host:                  "archive.ubuntu.com",
				Path:                  "/ubuntu/dists/noble/InRelease",
				IsMetadata:            true,
				RequestCount:          500,
				LastRequestedUnixTime: 1_700_006_500,
			},
		},
		RecentAdoptions: []adoptionEntry{
			{Host: "archive.ubuntu.com", SuitePath: "dists/noble", Outcome: "success", CompletedUnixTime: 1_700_006_000, DurationSeconds: 0.45},
			{Host: "archive.ubuntu.com", SuitePath: "dists/noble-updates", Outcome: "gpg_failed", CompletedUnixTime: 1_700_006_100, DurationSeconds: 0.32},
		},
		ActiveHosts: []activeHostInfo{
			{Host: "10.0.0.5", Inflight: 1, SlotCapacity: 4},
		},
		Keyring: []keyringEntry{
			{
				PrimaryFingerprint: "F6ECB3762474EDA9D21B7022871920D1991BC93C",
				PrimaryUID:         "Ubuntu Archive Automatic Signing Key (2018) <ftpmaster@ubuntu.com>",
				SourcePath:         "embedded:ubuntu-archive-keyring.gpg",
				SubkeyFingerprints: []string{},
			},
			{
				PrimaryFingerprint: "AABBCCDDEEFF00112233445566778899AABBCCDD",
				PrimaryUID:         "Custom Operator Key <ops@example.com>",
				SourcePath:         "/etc/apt/keyrings/custom.gpg",
				SubkeyFingerprints: []string{"1122334455667788"},
			},
		},
	}
}

// minimalJSONContractFixture exercises the abbreviated/null wire
// shapes: TLSMITM disabled (custom MarshalJSON emits {"enabled":false}),
// no GC run yet (LastRunUnixTime nil → custom MarshalJSON emits the
// abbreviated form), every list empty, repo_coverage at zero values,
// no keyring. This is the "freshly started, MITM off" operator-day-1
// shape and the highest-risk shape for an accidental contract drift.
func minimalJSONContractFixture() statusModel {
	return statusModel{
		Process: processInfo{
			Version:         "v0.test",
			StartedUnixTime: 1_700_000_000,
			UptimeSeconds:   60,
			VCSRevision:     "deadbeef",
			GoVersion:       "go-test",
		},
		Cache: cacheInfo{
			Dir: "/var/cache/apt-cacher-ultra",
		},
		CacheSummary: cacheSummary{
			ByHost: map[string]cacheSummaryHost{},
		},
		RepoCoverage: repoCoverageInfo{
			ArchitecturesSeen:   []string{},
			ArchitecturesFilter: []string{},
		},
		Listeners:       []listenerInfo{{Role: "admin", Addr: "127.0.0.1:6789"}},
		TLSMITM:         &tlsMITMInfo{Enabled: false},
		Suites:          []suiteEntry{},
		GC:              &gcInfo{}, // LastRunUnixTime nil → abbreviated MarshalJSON
		HotURLPaths:     []hotURLEntry{},
		RecentAdoptions: []adoptionEntry{},
		ActiveHosts:     []activeHostInfo{},
		Keyring:         []keyringEntry{},
	}
}
