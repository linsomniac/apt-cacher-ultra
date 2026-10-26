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
	if !strings.Contains(logs, "msg=e2e_commit_delay_enabled commit_delay=45s") {
		t.Fatalf("no e2e_commit_delay_enabled line with commit_delay=45s in:\n%s", logs)
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
	for _, v := range []string{"-1s", "soon", "45"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(e2eCommitDelayEnv, v)
			_, err := buildAdopterLogging(t)
			if err == nil || !strings.Contains(err.Error(), e2eCommitDelayEnv) {
				t.Fatalf("buildAdopter with %s=%q: err = %v, want one naming the variable", e2eCommitDelayEnv, v, err)
			}
		})
	}
}
