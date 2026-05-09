package main

// SPEC6 §15 #10 DoD pin — name-constraints lifecycle integration
// reachability through the daemon boot path.
//
// §10.2 contracts:
//
//   mitm_ca_name_constraints_skipped: {regex, reason}     Warn
//     Fires when the regex cannot translate to RFC 5280 dNSName
//     NameConstraints AND the operator has opted in via
//     tls_mitm.allow_unconstrained_ca = true. CA generation
//     proceeds without constraints; runtime signing is still
//     gated by §5.1.2.
//
//   mitm_ca_unconstrained_refused: {regex, reason}        Error
//   mitm_ca_generation_failed:     {path, err}            Error
//     Fires (in order) when the regex cannot translate AND
//     opt-in is FALSE. The daemon refuses to bind. Both events
//     together are the contract — the second is the
//     "consequence" of the first per §10.2.
//
// internal/proxy/tlsmitm/ca_test.go pins the LogFn callback
// behavior at unit scope. This test pins the OTHER half: that
// main.go's wireTlsMitm threads these emits through to slog AND
// that serveListeners' boot-time error propagation matches the
// emit ordering (refused → generation_failed).
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestServe_TlsMitmStartup_EmptyRegex_SkipsNameConstraints pins the
// success-but-warn path: empty regex → translation fails →
// AllowUnconstrainedCA=true lets boot proceed → Warn line emitted.
// mitm_ca_loaded subsequently reports name_constraints=false.
func TestServe_TlsMitmStartup_EmptyRegex_SkipsNameConstraints(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.AllowedHostRegex = "" // empty → translation fails
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return on cleanup")
		}
	})

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Daemon is fully up; the §10.2 startup logs were emitted
	// during wireTlsMitm BEFORE the listener bound.
	skipRec := findEventByMsg(t, sb.String(), "mitm_ca_name_constraints_skipped")
	if skipRec == nil {
		t.Fatalf("mitm_ca_name_constraints_skipped not emitted; logs:\n%s", sb.String())
	}
	if level, _ := skipRec["level"].(string); level != "WARN" {
		t.Errorf("mitm_ca_name_constraints_skipped level = %q, want %q", level, "WARN")
	}
	// Empty regex → emit reflects the empty pattern verbatim.
	if regex, _ := skipRec["regex"].(string); regex != "" {
		t.Errorf("mitm_ca_name_constraints_skipped.regex = %q, want \"\"", regex)
	}
	if reason, _ := skipRec["reason"].(string); reason == "" {
		t.Errorf("mitm_ca_name_constraints_skipped.reason missing/empty")
	} else if !strings.Contains(reason, "empty regex") {
		t.Errorf("mitm_ca_name_constraints_skipped.reason = %q; expected to mention \"empty regex\"", reason)
	}
	assertExactFields(t, skipRec, "mitm_ca_name_constraints_skipped",
		[]string{"regex", "reason"})
}

// TestServe_TlsMitmStartup_EmptyRegex_RefusesUnconstrained pins the
// fail-closed path: empty regex + AllowUnconstrainedCA=false →
// daemon refuses to bind, both Error events emit in spec order
// (refused → generation_failed).
func TestServe_TlsMitmStartup_EmptyRegex_RefusesUnconstrained(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = false // fail-closed
	cfg.TlsMitm.AllowedHostRegex = ""        // empty → translation fails
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
	// serveListeners owns the listener and closes it on its way
	// out. We never need to touch it from the test side.
	defer func() {
		// In the unlikely case serveListeners didn't close the
		// listener (e.g., it errored before reaching that path),
		// close it here so the goroutine doesn't leak.
		_ = cacheLn.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// serveListeners is expected to return an error from
	// wireTlsMitm. Capture it.
	serveErr := serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	if serveErr == nil {
		t.Fatalf("serveListeners returned nil; expected unconstrained-CA refusal")
	}
	if !errors.Is(serveErr, serveErr) || // tautology to pin err non-nil
		!strings.Contains(strings.ToLower(serveErr.Error()), "unconstrained") {
		t.Errorf("serveListeners err = %v; expected to mention \"unconstrained\"", serveErr)
	}

	out := sb.String()
	// Spec ordering: mitm_ca_unconstrained_refused first, then
	// mitm_ca_generation_failed. findOrderedEvents walks the log
	// stream and verifies the indices.
	refusedIdx, genFailedIdx := -1, -1
	var refusedRec, genFailedRec map[string]any
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		switch msg, _ := rec["msg"].(string); msg {
		case "mitm_ca_unconstrained_refused":
			if refusedIdx >= 0 {
				t.Errorf("mitm_ca_unconstrained_refused emitted more than once\n%s", line)
			}
			refusedIdx = i
			refusedRec = rec
		case "mitm_ca_generation_failed":
			if genFailedIdx >= 0 {
				t.Errorf("mitm_ca_generation_failed emitted more than once\n%s", line)
			}
			genFailedIdx = i
			genFailedRec = rec
		}
	}

	if refusedRec == nil {
		t.Fatalf("mitm_ca_unconstrained_refused not emitted; logs:\n%s", out)
	}
	if genFailedRec == nil {
		t.Fatalf("mitm_ca_generation_failed not emitted; logs:\n%s", out)
	}
	if !(refusedIdx < genFailedIdx) {
		t.Errorf("event ordering violated: refused at line %d, generation_failed at line %d (expected refused first per §10.2)",
			refusedIdx, genFailedIdx)
	}

	// mitm_ca_unconstrained_refused field set: {regex, reason}, Error.
	if level, _ := refusedRec["level"].(string); level != "ERROR" {
		t.Errorf("mitm_ca_unconstrained_refused level = %q, want %q", level, "ERROR")
	}
	if regex, _ := refusedRec["regex"].(string); regex != "" {
		t.Errorf("mitm_ca_unconstrained_refused.regex = %q, want \"\"", regex)
	}
	if reason, _ := refusedRec["reason"].(string); reason == "" {
		t.Errorf("mitm_ca_unconstrained_refused.reason missing/empty")
	}
	assertExactFields(t, refusedRec, "mitm_ca_unconstrained_refused",
		[]string{"regex", "reason"})

	// mitm_ca_generation_failed field set: {path, err}, Error.
	if level, _ := genFailedRec["level"].(string); level != "ERROR" {
		t.Errorf("mitm_ca_generation_failed level = %q, want %q", level, "ERROR")
	}
	if pth, _ := genFailedRec["path"].(string); pth == "" {
		t.Errorf("mitm_ca_generation_failed.path missing/empty")
	} else if !strings.HasPrefix(pth, cacheDir) {
		t.Errorf("mitm_ca_generation_failed.path = %q; expected to start with %q", pth, cacheDir)
	}
	if errStr, _ := genFailedRec["err"].(string); errStr == "" {
		t.Errorf("mitm_ca_generation_failed.err missing/empty")
	}
	assertExactFields(t, genFailedRec, "mitm_ca_generation_failed",
		[]string{"path", "err"})
}

// findEventByMsg returns the first JSON-decoded log record whose
// msg matches name. Returns nil if not present.
func findEventByMsg(t *testing.T, out, name string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg == name {
			return rec
		}
	}
	return nil
}
