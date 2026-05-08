package main

// SPEC6 §15 #10 DoD pin — mitm_ca_generated + mitm_ca_loaded
// integration reachability through the daemon boot path.
//
// §10.2 contract:
//   mitm_ca_generated: {path, algorithm, lifetime_seconds}        Info
//   mitm_ca_loaded:    {source, fingerprint_sha256,
//                       not_after_unixtime, name_constraints}     Info
//
// internal/proxy/tlsmitm/ca_test.go pins the field set when calling
// LoadOrGenerate directly. This test pins the OTHER half: that
// main.go's wireTlsMitm threads the LogFn callback through to the
// daemon's slog.Logger via emitTlsMitmLog so operators see the
// lines in journal. Catches a regression where someone removes
// the LogFn arg, drops level="info" handling in emitTlsMitmLog,
// or otherwise breaks the bridge between LoadOrGenerate and slog.
//
// Two boots against the SAME cache_dir/ca/ tree:
//
//   1. Fresh CA dir → §4.2 case 1: BOTH mitm_ca_generated AND
//      mitm_ca_loaded fire.
//   2. CA already committed → §4.2 case 2: ONLY mitm_ca_loaded
//      fires (no silent regenerate). Same fingerprint as boot 1
//      proves no rotation happened.
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServe_TlsMitmStartup_EmitsCaGeneratedAndCaLoaded(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()

	// Boot 1 — fresh CA dir.
	out1 := bootDaemonCaptureStartup(t, cacheDir)
	gen1, loaded1 := findCaLifecycleEvents(t, out1)
	if gen1 == nil {
		t.Fatalf("boot 1 (fresh dir) did not emit mitm_ca_generated; logs:\n%s", out1)
	}
	if loaded1 == nil {
		t.Fatalf("boot 1 (fresh dir) did not emit mitm_ca_loaded; logs:\n%s", out1)
	}
	assertGeneratedFields(t, gen1, cacheDir)
	assertLoadedFields(t, loaded1, "generated")
	assertExactFields(t, gen1, "mitm_ca_generated",
		[]string{"path", "algorithm", "lifetime_seconds"})
	assertExactFields(t, loaded1, "mitm_ca_loaded",
		[]string{"source", "fingerprint_sha256", "not_after_unixtime", "name_constraints"})

	// Boot 2 — CA already on disk. §4.2 case 2: load and use.
	out2 := bootDaemonCaptureStartup(t, cacheDir)
	gen2, loaded2 := findCaLifecycleEvents(t, out2)
	if gen2 != nil {
		t.Errorf("boot 2 (existing CA) re-emitted mitm_ca_generated; §4.2 case 2 mandates load-and-use, no regenerate\nlogs:\n%s", out2)
	}
	if loaded2 == nil {
		t.Fatalf("boot 2 did not emit mitm_ca_loaded; logs:\n%s", out2)
	}
	assertLoadedFields(t, loaded2, "generated")
	assertExactFields(t, loaded2, "mitm_ca_loaded",
		[]string{"source", "fingerprint_sha256", "not_after_unixtime", "name_constraints"})

	// Same on-disk CA → identical fingerprint across boots. A
	// mismatch would mean boot 2 silently rotated, breaking
	// §4.2 case 2's NEVER-silent-regeneration invariant.
	fp1, _ := loaded1["fingerprint_sha256"].(string)
	fp2, _ := loaded2["fingerprint_sha256"].(string)
	if fp1 != fp2 {
		t.Errorf("CA fingerprint changed across boots; boot 2 silently regenerated\n  boot1: %s\n  boot2: %s", fp1, fp2)
	}
}

// bootDaemonCaptureStartup brings up the daemon with TLS MITM
// enabled against cacheDir, waits for the listener to be live (so
// startup logs have flushed), then drives a clean shutdown and
// returns the captured JSON log output. Reusable across both
// boots in the test.
func bootDaemonCaptureStartup(t *testing.T, cacheDir string) string {
	t.Helper()

	var sb captureBuilder
	cfg := minimalCfg(cacheDir, nil)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	logger := slog.New(slog.NewJSONHandler(&sb, nil))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	}()

	// sync.Once gates explicit-shutdown vs. backstop-cleanup. Each
	// boot in the test calls bootDaemonCaptureStartup twice, and
	// the second boot must not deadlock waiting for a serveDone
	// that's already drained.
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-serveDone:
				if err != nil {
					t.Errorf("serveListeners returned: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Errorf("serveListeners did not return")
			}
		})
	}
	t.Cleanup(shutdown)

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Daemon is fully up; the §10.2 startup logs were emitted
	// during wireTlsMitm BEFORE the listener bound, so they're
	// already flushed to the captureBuilder.
	shutdown()
	return sb.String()
}

// findCaLifecycleEvents scans JSON log output for the two §10.2
// startup events and returns the first record of each. Duplicate
// emits are flagged because §10.2 mandates exactly-once for both.
func findCaLifecycleEvents(t *testing.T, out string) (gen, loaded map[string]any) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		switch msg {
		case "mitm_ca_generated":
			if gen != nil {
				t.Errorf("mitm_ca_generated emitted more than once; spec mandates one per cache_dir/ca/ lifecycle\n%s", line)
			}
			gen = rec
		case "mitm_ca_loaded":
			if loaded != nil {
				t.Errorf("mitm_ca_loaded emitted more than once per boot\n%s", line)
			}
			loaded = rec
		}
	}
	return gen, loaded
}

func assertGeneratedFields(t *testing.T, rec map[string]any, cacheDir string) {
	t.Helper()
	pth, _ := rec["path"].(string)
	if pth == "" {
		t.Errorf("mitm_ca_generated.path missing/empty")
	} else if !strings.HasPrefix(pth, cacheDir) {
		t.Errorf("mitm_ca_generated.path = %q; expected to start with cache_dir %q", pth, cacheDir)
	}
	alg, _ := rec["algorithm"].(string)
	if alg != "ecdsa-p256" {
		t.Errorf("mitm_ca_generated.algorithm = %q, want %q", alg, "ecdsa-p256")
	}
	lifeRaw, present := rec["lifetime_seconds"]
	if !present {
		t.Errorf("mitm_ca_generated.lifetime_seconds missing")
	} else if life, ok := lifeRaw.(float64); !ok {
		t.Errorf("mitm_ca_generated.lifetime_seconds not numeric: %T %v", lifeRaw, lifeRaw)
	} else if int64(life) != int64((30 * 24 * time.Hour).Seconds()) {
		t.Errorf("mitm_ca_generated.lifetime_seconds = %v, want %v",
			int64(life), int64((30 * 24 * time.Hour).Seconds()))
	}
	if level, _ := rec["level"].(string); level != "INFO" {
		t.Errorf("mitm_ca_generated level = %q, want %q", level, "INFO")
	}
}

func assertLoadedFields(t *testing.T, rec map[string]any, wantSource string) {
	t.Helper()
	src, _ := rec["source"].(string)
	if src != wantSource {
		t.Errorf("mitm_ca_loaded.source = %q, want %q", src, wantSource)
	}
	fp, _ := rec["fingerprint_sha256"].(string)
	if len(fp) != 64 {
		t.Errorf("mitm_ca_loaded.fingerprint_sha256 length = %d, want 64; got %q", len(fp), fp)
	}
	for _, c := range fp {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			t.Errorf("mitm_ca_loaded.fingerprint_sha256 has non-hex char %q in %q", c, fp)
			break
		}
	}
	naRaw, present := rec["not_after_unixtime"]
	if !present {
		t.Errorf("mitm_ca_loaded.not_after_unixtime missing")
	} else if na, ok := naRaw.(float64); !ok {
		t.Errorf("mitm_ca_loaded.not_after_unixtime not numeric: %T %v", naRaw, naRaw)
	} else if int64(na) <= time.Now().Unix() {
		t.Errorf("mitm_ca_loaded.not_after_unixtime = %d, want > now (CA freshly generated with 30d lifetime)", int64(na))
	}
	if _, present := rec["name_constraints"]; !present {
		t.Errorf("mitm_ca_loaded.name_constraints missing")
	} else if _, ok := rec["name_constraints"].(bool); !ok {
		t.Errorf("mitm_ca_loaded.name_constraints not boolean: %T %v",
			rec["name_constraints"], rec["name_constraints"])
	}
	if level, _ := rec["level"].(string); level != "INFO" {
		t.Errorf("mitm_ca_loaded level = %q, want %q", level, "INFO")
	}
}

// assertExactFields verifies that rec carries exactly the union of
// `want` plus slog builtins (msg, level, time) — no missing, no
// extras. Catches both spec drift (a field renamed in code but not
// here) and accidental field additions.
func assertExactFields(t *testing.T, rec map[string]any, name string, want []string) {
	t.Helper()
	builtin := map[string]bool{"msg": true, "level": true, "time": true}
	expected := map[string]bool{}
	for _, k := range want {
		expected[k] = true
	}
	for k := range rec {
		if builtin[k] || expected[k] {
			continue
		}
		t.Errorf("%s carries unexpected field %q (spec lists %v)", name, k, want)
	}
	for _, k := range want {
		if _, present := rec[k]; !present {
			t.Errorf("%s missing required field %q", name, k)
		}
	}
}
