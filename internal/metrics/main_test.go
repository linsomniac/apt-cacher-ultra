package metrics

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the default logger for this package's tests (the H1
// final tests review M-8): the series-cap tests make the registry log WARN
// metrics_series_cap_reached to slog.Default(), once per metric.
// TestGauge_CapWarningStaysOneShotAcrossResets, which counts that line,
// sets its own default logger and restores this one.
//
// AIDEV-NOTE: this is the package's only TestMain; a test that needs the
// default logger's output sets slog.SetDefault itself and restores it.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
