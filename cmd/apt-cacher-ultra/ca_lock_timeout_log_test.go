package main

// SPEC6 §15 #10 DoD pin — mitm_ca_lock_timeout + paired
// mitm_ca_generation_failed integration reachability through the
// daemon boot path.
//
// §10.2 contracts:
//
//   mitm_ca_lock_timeout:      {path}            Error
//   mitm_ca_generation_failed: {path, err}       Error
//
// Per §10.2: lock_timeout is "Followed immediately by a
// mitm_ca_generation_failed Error so the operator sees both the
// cause and the effect." The two-event pairing is the contract;
// either event missing in the post-incident journal would be a
// regression in operator alerting.
//
// Triggered by pre-acquiring the §4.2.2 interprocess flock at
// <cache_dir>/ca/.ca.lock from the test before booting the
// daemon. caLockTimeoutForTest overrides the LoadOrGenerate
// 30s default with 250 ms so the test runs in well under a
// second.
//
// internal/proxy/tlsmitm/ca_test.go pins LogFn callback at unit
// scope. This pin asserts the daemon-side wiring through
// emitTlsMitmLog → slog AND the spec ordering (lock_timeout
// before generation_failed in the log stream).
//
// Mutates package-level shutdownTimeout AND caLockTimeoutForTest,
// so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServe_TlsMitmStartup_FlockHeld_EmitsLockTimeout(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	oldLockTimeout := caLockTimeoutForTest
	caLockTimeoutForTest = 250 * time.Millisecond
	t.Cleanup(func() { caLockTimeoutForTest = oldLockTimeout })

	cacheDir := t.TempDir()
	caDir := filepath.Join(cacheDir, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("mkdir ca dir: %v", err)
	}
	lockPath := filepath.Join(caDir, ".ca.lock")

	// Pre-acquire the flock — mirrors the unit-side pattern in
	// internal/proxy/tlsmitm/ca_test.go's
	// TestLoadOrGenerate_Auto_LockTimeoutLogsBoth.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("external flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

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
	// wireTlsMitm because the flock acquire times out.
	serveErr := serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	if serveErr == nil {
		t.Fatalf("serveListeners returned nil; expected lock-timeout error")
	}
	if !strings.Contains(strings.ToLower(serveErr.Error()), "lock") {
		t.Errorf("serveListeners err = %v; expected to mention \"lock\"", serveErr)
	}

	// Walk the log stream once to find indices and records for
	// both events. Spec ordering: lock_timeout BEFORE
	// generation_failed.
	out := sb.String()
	lockIdx, genFailedIdx := -1, -1
	var lockRec, genFailedRec map[string]any
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		switch msg, _ := rec["msg"].(string); msg {
		case "mitm_ca_lock_timeout":
			if lockIdx >= 0 {
				t.Errorf("mitm_ca_lock_timeout emitted more than once\n%s", line)
			}
			lockIdx = i
			lockRec = rec
		case "mitm_ca_generation_failed":
			if genFailedIdx >= 0 {
				t.Errorf("mitm_ca_generation_failed emitted more than once\n%s", line)
			}
			genFailedIdx = i
			genFailedRec = rec
		}
	}

	if lockRec == nil {
		t.Fatalf("mitm_ca_lock_timeout not emitted; logs:\n%s", out)
	}
	if genFailedRec == nil {
		t.Fatalf("mitm_ca_generation_failed not emitted; logs:\n%s", out)
	}
	if lockIdx >= genFailedIdx {
		t.Errorf("event ordering violated: lock_timeout at line %d, generation_failed at line %d (expected lock_timeout first per §10.2)",
			lockIdx, genFailedIdx)
	}

	// mitm_ca_lock_timeout: {path}, Error.
	if level, _ := lockRec["level"].(string); level != "ERROR" {
		t.Errorf("mitm_ca_lock_timeout level = %q, want %q", level, "ERROR")
	}
	pth, _ := lockRec["path"].(string)
	if pth == "" {
		t.Errorf("mitm_ca_lock_timeout.path missing/empty")
	} else if pth != lockPath {
		t.Errorf("mitm_ca_lock_timeout.path = %q, want %q (the lock file path, not StorageDir)", pth, lockPath)
	}
	assertExactFields(t, lockRec, "mitm_ca_lock_timeout",
		[]string{"path"})

	// mitm_ca_generation_failed: {path, err}, Error. Already
	// pinned in ca_constraints_log_test.go but the pairing
	// matters here — re-assert key invariants.
	if level, _ := genFailedRec["level"].(string); level != "ERROR" {
		t.Errorf("mitm_ca_generation_failed level = %q, want %q", level, "ERROR")
	}
	if pth, _ := genFailedRec["path"].(string); pth == "" {
		t.Errorf("mitm_ca_generation_failed.path missing/empty")
	} else if pth != caDir {
		t.Errorf("mitm_ca_generation_failed.path = %q, want %q (StorageDir)", pth, caDir)
	}
	if errStr, _ := genFailedRec["err"].(string); errStr == "" {
		t.Errorf("mitm_ca_generation_failed.err missing/empty")
	}
	assertExactFields(t, genFailedRec, "mitm_ca_generation_failed",
		[]string{"path", "err"})
}
