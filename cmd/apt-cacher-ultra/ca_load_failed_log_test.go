package main

// SPEC6 §15 #10 DoD pin — mitm_ca_load_failed integration
// reachability through the daemon boot path.
//
// §10.2 contract:
//
//   mitm_ca_load_failed: {path, err}        Error
//
// Fires when §4.2 case 3 detects "any state where at least one
// daemon-managed real file (ca.crt, ca.key, ca.ready) is present
// but the trio is not self-consistent." The daemon refuses to
// bind. Operator-explicit recovery is the only path forward —
// the daemon NEVER silently regenerates over an existing real
// file (§4.2 invariant 1).
//
// Triggered by pre-creating just `ca.crt` in <cache_dir>/ca/
// before boot. scanCAState detects the partial state and emits
// the Error; no need for a valid cert PEM since case detection
// runs before parse.
//
// internal/proxy/tlsmitm/ca_test.go pins the LogFn callback at
// unit scope. This test pins the daemon-side wiring through
// emitTlsMitmLog to slog.
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServe_TlsMitmStartup_PartialCADir_EmitsLoadFailed(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	caDir := filepath.Join(cacheDir, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("mkdir ca dir: %v", err)
	}
	// Write only `ca.crt` — `ca.key` and `ca.ready` absent. This
	// is the §4.2 case 3 signature ("real file present without
	// committed siblings"). Bytes don't need to parse; case
	// detection runs before any PEM parse.
	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), []byte("not a real cert"), 0o600); err != nil {
		t.Fatalf("write partial ca.crt: %v", err)
	}

	cfg := minimalCfg(cacheDir, nil)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelError}))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = cacheLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// serveListeners is expected to return an error from
	// wireTlsMitm because the partial CA dir is unrecoverable
	// without operator intervention.
	serveErr := serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	if serveErr == nil {
		t.Fatalf("serveListeners returned nil; expected partial-CA refusal")
	}
	if !strings.Contains(strings.ToLower(serveErr.Error()), "inconsistent") {
		t.Errorf("serveListeners err = %v; expected to mention \"inconsistent\"", serveErr)
	}

	rec := findEventByMsg(t, sb.String(), "mitm_ca_load_failed")
	if rec == nil {
		t.Fatalf("mitm_ca_load_failed not emitted; logs:\n%s", sb.String())
	}
	if level, _ := rec["level"].(string); level != "ERROR" {
		t.Errorf("mitm_ca_load_failed level = %q, want %q", level, "ERROR")
	}
	pth, _ := rec["path"].(string)
	if pth == "" {
		t.Errorf("mitm_ca_load_failed.path missing/empty")
	} else if pth != caDir {
		t.Errorf("mitm_ca_load_failed.path = %q, want %q (the StorageDir, not cache_dir)", pth, caDir)
	}
	errStr, _ := rec["err"].(string)
	if errStr == "" {
		t.Errorf("mitm_ca_load_failed.err missing/empty")
	} else if !strings.Contains(errStr, "inconsistent") {
		t.Errorf("mitm_ca_load_failed.err = %q; expected scanCAState's \"inconsistent CA state\" diagnostic", errStr)
	}
	assertExactFields(t, rec, "mitm_ca_load_failed",
		[]string{"path", "err"})
}
