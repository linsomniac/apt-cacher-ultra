//go:build e2ehooks

package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// e2eCommitDelayEnv names the environment variable that makes every
// adoption commit of an e2ehooks binary wait that long first (a Go duration;
// empty or unset is no delay). e2e/flat's commit-delay scenario sets it to
// 45s: a commit slower than the flip barrier's 20 s hold, which apt's 30 s
// timeout must never see (spec §7).
const e2eCommitDelayEnv = "ACU_E2E_COMMIT_DELAY"

// applyE2EHooks wires the e2e/flat test seams into a, from the environment.
//
// AIDEV-NOTE: compiled only with `go build -tags e2ehooks` (e2e/flat's
// Dockerfile); every other build gets e2ehooks_off.go's no-op, so no
// release binary reads ACU_E2E_COMMIT_DELAY (CI greps the release build for
// the string). The seam is the adopter's own test hook
// (Adopter.SetCommitDelayForTest), which the flip simulations already use.
// e2ehooks_test.go pins the parse and the log line; that the delay reaches
// the adopter is pinned by e2e/flat's commit-delay scenario
// (outcome=hold_expired).
func applyE2EHooks(a *freshness.Adopter, logger *slog.Logger) error {
	v := os.Getenv(e2eCommitDelayEnv)
	if v == "" || a == nil {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("%s=%q: want a nonnegative Go duration", e2eCommitDelayEnv, v)
	}
	a.SetCommitDelayForTest(d)
	logger.Warn("e2e_commit_delay_enabled", "commit_delay", d,
		"detail", "test binary: every adoption commit waits this long first")
	return nil
}
