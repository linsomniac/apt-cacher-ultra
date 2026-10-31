//go:build e2ehooks

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// buildAdopterLogging runs buildAdopter with adoption enabled and the
// bundled keys, and returns its log text and error.
func buildAdopterLogging(t *testing.T) (string, error) {
	t.Helper()
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, true)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	var buf bytes.Buffer
	_, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewTextHandler(&buf, nil)))
	return buf.String(), err
}

func TestBuildAdopter_E2ECommitDelayFromTheEnvironment(t *testing.T) {
	t.Setenv(e2eCommitDelayEnv, "45s")
	logs, err := buildAdopterLogging(t)
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if !strings.Contains(logs, "level=WARN msg=e2e_commit_delay_enabled commit_delay=45s") {
		t.Fatalf("no WARN e2e_commit_delay_enabled line with commit_delay=45s in:\n%s", logs)
	}
}

func TestBuildAdopter_E2ECommitDelayUnsetIsNoDelay(t *testing.T) {
	t.Setenv(e2eCommitDelayEnv, "")
	logs, err := buildAdopterLogging(t)
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if strings.Contains(logs, "e2e_commit_delay_enabled") {
		t.Fatalf("an unset %s enabled the delay:\n%s", e2eCommitDelayEnv, logs)
	}
}

func TestBuildAdopter_E2ECommitDelayRejectsABadValue(t *testing.T) {
	// reason is the refusal's own text, or time.ParseDuration's kept in it.
	for _, tc := range []struct{ v, reason string }{
		{"-1s", "want a nonnegative Go duration"},
		{"soon", `invalid duration "soon"`},
		{"45", `missing unit in duration "45"`},
	} {
		t.Run(tc.v, func(t *testing.T) {
			t.Setenv(e2eCommitDelayEnv, tc.v)
			_, err := buildAdopterLogging(t)
			if err == nil || !strings.Contains(err.Error(), e2eCommitDelayEnv) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("buildAdopter with %s=%q: err = %v, want one naming the variable and %q", e2eCommitDelayEnv, tc.v, err, tc.reason)
			}
		})
	}
}
