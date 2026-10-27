package fetch

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the default logger for this package's tests (the H1
// final tests review M-8): a Client built without Options.Logger logs its
// "fetch retry" lines to slog.Default(), and the httptest TLS servers,
// which have no ErrorLog, log their handshake errors through the log
// package, which slog.SetDefault routes to the same discarding handler.
// A test that checks a log line passes its own Options.Logger.
//
// AIDEV-NOTE: this is the package's only TestMain; a test that needs the
// default logger's output sets slog.SetDefault itself and restores it.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
