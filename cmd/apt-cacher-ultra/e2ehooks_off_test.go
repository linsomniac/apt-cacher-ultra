//go:build !e2ehooks

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A release binary has no e2e seam: ACU_E2E_COMMIT_DELAY is ignored, even
// when it holds a value e2ehooks.go would refuse.
func TestBuildAdopter_ReleaseBinaryIgnoresTheE2ECommitDelay(t *testing.T) {
	for _, v := range []string{"45s", "soon"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("ACU_E2E_COMMIT_DELAY", v)
			withKeyringDirs(t, []string{t.TempDir()})
			cfg := newAdoptionEnabledCfg(t, true)
			c, fetcher, hosts := newAdoptionWiringDeps(t)
			var buf bytes.Buffer
			a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewTextHandler(&buf, nil)))
			if err != nil || a == nil {
				t.Fatalf("buildAdopter: adopter %v, err %v", a, err)
			}
			if strings.Contains(buf.String(), "e2e_commit_delay") {
				t.Fatalf("a release binary read ACU_E2E_COMMIT_DELAY:\n%s", buf.String())
			}
		})
	}
}
