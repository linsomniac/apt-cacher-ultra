//go:build !e2ehooks

package main

import (
	"log/slog"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// applyE2EHooks is a no-op outside e2e/flat's test binary (built with
// -tags e2ehooks; see e2ehooks.go): a release binary has no test seam to
// wire and reads no ACU_E2E_* variable.
func applyE2EHooks(*freshness.Adopter, *slog.Logger) error { return nil }
