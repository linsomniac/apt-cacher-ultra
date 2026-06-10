package config

// SPEC6_7 config-surface coverage: the [adoption] member-retry and
// skip-repair knobs born from the 2026-06-09 stale-mirror incident.

import (
	"strings"
	"testing"
	"time"
)

// TestLoad_MemberRetryDefaultsApplied: omitted member_retry_count /
// member_retry_delay get the SPEC6_7 §5 defaults (2 retries, 30s).
func TestLoad_MemberRetryDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Adoption.MemberRetryCount != 2 {
		t.Errorf("member_retry_count default = %d, want 2", cfg.Adoption.MemberRetryCount)
	}
	if cfg.Adoption.MemberRetryDelay.Duration != 30*time.Second {
		t.Errorf("member_retry_delay default = %s, want 30s", cfg.Adoption.MemberRetryDelay.Duration)
	}
}

// TestLoad_MemberRetryExplicitZeroRespected: an operator-written 0
// disables in-adoption retries (the pre-SPEC6_7 single-attempt
// behavior) and must survive Load's defaulting.
func TestLoad_MemberRetryExplicitZeroRespected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[adoption]
member_retry_count = 0
member_retry_delay = "0s"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Adoption.MemberRetryCount != 0 {
		t.Errorf("explicit member_retry_count=0 was clobbered to %d", cfg.Adoption.MemberRetryCount)
	}
	if cfg.Adoption.MemberRetryDelay.Duration != 0 {
		t.Errorf("explicit member_retry_delay=0s was clobbered to %s", cfg.Adoption.MemberRetryDelay.Duration)
	}
}

// TestLoad_RepairSkippedMembersDefaultTrue: the SPEC6_7 §3 repair pass
// is on by default (just-works posture); an explicit false survives
// Load (bool pre-populate pattern).
func TestLoad_RepairSkippedMembersDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Adoption.RepairSkippedMembers {
		t.Error("repair_skipped_members default = false, want true")
	}

	path2 := writeTOML(t, dir, "config2.toml", `
[cache]
dir = "`+dir+`"
[adoption]
repair_skipped_members = false
`)
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load explicit-false: %v", err)
	}
	if cfg2.Adoption.RepairSkippedMembers {
		t.Error("explicit repair_skipped_members=false was clobbered to true")
	}
}

// TestLoad_RequiredArchitectures: SPEC6_7 §6 validation — shape rules
// match [adoption].architectures; when the architectures allowlist is
// set, required_architectures must be a subset of it (a required arch
// the filter pre-skips would fail EVERY adoption).
func TestLoad_RequiredArchitectures(t *testing.T) {
	load := func(t *testing.T, adoption string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[adoption]
`+adoption+`
`)
		return Load(path)
	}

	t.Run("default_empty", func(t *testing.T) {
		cfg, err := load(t, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Adoption.RequiredArchitectures) != 0 {
			t.Errorf("required_architectures default = %v, want empty", cfg.Adoption.RequiredArchitectures)
		}
	})
	t.Run("valid_standalone", func(t *testing.T) {
		cfg, err := load(t, `required_architectures = ["amd64", "source"]`)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Adoption.RequiredArchitectures) != 2 {
			t.Errorf("got %v", cfg.Adoption.RequiredArchitectures)
		}
	})
	t.Run("valid_subset_of_allowlist", func(t *testing.T) {
		if _, err := load(t, `architectures = ["amd64", "i386"]
required_architectures = ["amd64"]`); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("rejects_non_subset_of_allowlist", func(t *testing.T) {
		_, err := load(t, `architectures = ["amd64"]
required_architectures = ["arm64"]`)
		if err == nil {
			t.Fatal("required arch outside allowlist accepted; want error")
		}
		if !strings.Contains(err.Error(), "required_architectures") {
			t.Errorf("error %q does not name the key", err)
		}
	})
	t.Run("rejects_invalid_shape", func(t *testing.T) {
		if _, err := load(t, `required_architectures = ["AMD64"]`); err == nil {
			t.Fatal("uppercase arch accepted; want error")
		}
	})
}

// TestLoad_MemberRetryNegativeRejected: negative values fail
// validation with a key-named error.
func TestLoad_MemberRetryNegativeRejected(t *testing.T) {
	cases := []struct{ name, body, wantSubstr string }{
		{
			"negative_count",
			"member_retry_count = -1",
			"member_retry_count",
		},
		{
			"negative_delay",
			`member_retry_delay = "-5s"`,
			"member_retry_delay",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
[adoption]
`+tc.body+`
`)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s; want validation error", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not name %q", err, tc.wantSubstr)
			}
		})
	}
}
