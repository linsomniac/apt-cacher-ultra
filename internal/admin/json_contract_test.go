package admin

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// AIDEV-NOTE: TestJSONContractPreserved is the DoD #2 regression gate
// for docs/admin-ui-spec.md §0.1 / §0.4 — the HTML redesign must not
// bleed into the JSON wire shape. The encoder path mirrors
// renderStatus's JSON branch (encoding/json.Encoder with two-space
// indent) so a divergence here is the same divergence an operator
// would see hitting `/?format=json`. The golden lives in
// testdata/json_contract_golden.json; regenerate with `-update`.

var jsonContractUpdate = flag.Bool("update.jsoncontract", false,
	"regenerate testdata/json_contract_golden.json from the in-test model")

func TestJSONContractPreserved(t *testing.T) {
	got := encodeStatusModelLikeRenderStatus(t, jsonContractFixture())
	goldenPath := filepath.Join("testdata", "json_contract_golden.json")

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
		gotPath := filepath.Join(t.TempDir(), "actual.json")
		_ = os.WriteFile(gotPath, got, 0o644)
		t.Fatalf("JSON contract drifted (DoD #2 / §0.1). Diff with:\n  diff %s %s\nRegenerate intentionally with: go test ./internal/admin -run TestJSONContractPreserved -update.jsoncontract",
			goldenPath, gotPath)
	}
}

// encodeStatusModelLikeRenderStatus runs the same encoder configuration
// renderStatus's JSON branch uses (SetIndent two spaces, default
// HTMLEscape). Kept in sync with renderStatus by code review — any
// change to renderStatus's encoder must mirror here or the golden
// diverges from the wire shape.
func encodeStatusModelLikeRenderStatus(t *testing.T, m statusModel) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		t.Fatalf("json.Encode: %v", err)
	}
	return buf.Bytes()
}

// jsonContractFixture is a representative, deterministic statusModel.
// It exercises every top-level key, every pointer-nullable field's
// populated branch, plus the keyring entry shape and a non-empty
// recent-adoptions slice (including a non-success outcome) so a
// regression in any block surfaces.
func jsonContractFixture() statusModel {
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
