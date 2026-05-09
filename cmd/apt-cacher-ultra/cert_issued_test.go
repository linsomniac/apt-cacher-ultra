package main

// SPEC6 §10.2 mitm_cert_issued Debug log DoD pin.
//
// The leaf-cache miss path runs the gen function in main.go's
// wireTlsMitm; that function emits the Debug log BEFORE returning
// the freshly-generated cert to LeafCache.insert. The acu_mitm_
// cert_issued_total metric and this log share that single
// emit site so the counter and log line stay 1:1.
//
// Spec field set:
//
//   msg=mitm_cert_issued
//   host=<lower-cased CONNECT target literal>
//   algorithm=<ecdsa-p256 | rsa2048>
//   lifetime_seconds=<int64, matches LeafCertLifetime>
//   gen_duration_ms=<int64, ≥0>
//
// Driven by a single CONNECT — a fresh cache always misses on the
// first lookup, which fires the gen function. Reading the 200 line
// is proof that ServeCONNECT reached LeafCache.Get.
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func TestServe_LeafCertIssuance_EmitsMITMCertIssuedLog(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^cert-issued\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 32
	cfg.TlsMitm.LeafCertLifetime.Duration = 6 * time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	// Capture Debug-level JSON logs. mitm_cert_issued is Debug per
	// §10.2 — the standard Info-level handler used elsewhere would
	// drop the line.
	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// One CONNECT triggers one cache miss + one gen + one log line.
	conn := openCONNECT(t, cacheAddr, "cert-issued.test:443")
	defer conn.Close()

	// Shutdown so all log writes have completed before we read.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		msg, _ := rec["msg"].(string)
		if msg != "mitm_cert_issued" {
			continue
		}
		if found {
			t.Errorf("more than one mitm_cert_issued emitted; spec mandates one per cert generation\n%s", line)
		}
		found = true

		host, _ := rec["host"].(string)
		if host != "cert-issued.test" {
			t.Errorf("issued log host = %q, want %q\n%s", host, "cert-issued.test", line)
		}
		algorithm, _ := rec["algorithm"].(string)
		if algorithm != "ecdsa-p256" {
			t.Errorf("issued log algorithm = %q, want %q\n%s", algorithm, "ecdsa-p256", line)
		}
		// JSON numbers decode as float64; we emitted int64s but the
		// JSON wire format does not preserve that distinction.
		lifeRaw, present := rec["lifetime_seconds"]
		if !present {
			t.Errorf("issued log missing lifetime_seconds\n%s", line)
		} else if life, ok := lifeRaw.(float64); !ok {
			t.Errorf("issued log lifetime_seconds not a number: %T %v\n%s", lifeRaw, lifeRaw, line)
		} else if int64(life) != int64((6 * time.Hour).Seconds()) {
			t.Errorf("issued log lifetime_seconds = %v, want %v\n%s",
				int64(life), int64((6 * time.Hour).Seconds()), line)
		}
		genRaw, present := rec["gen_duration_ms"]
		if !present {
			t.Errorf("issued log missing gen_duration_ms\n%s", line)
		} else if gen, ok := genRaw.(float64); !ok {
			t.Errorf("issued log gen_duration_ms not a number: %T %v\n%s", genRaw, genRaw, line)
		} else if gen < 0 {
			t.Errorf("issued log gen_duration_ms = %v, want ≥0\n%s", gen, line)
		}
		// SPEC §10.2 contract: exact field set {host, algorithm,
		// lifetime_seconds, gen_duration_ms}. Anything beyond that
		// — except slog-builtin keys (`time`, `level`, `msg`) —
		// would be a spec drift.
		for k := range rec {
			switch k {
			case "msg", "level", "time",
				"host", "algorithm", "lifetime_seconds", "gen_duration_ms":
				// ok
			default:
				t.Errorf("issued log carries unexpected field %q\n%s", k, line)
			}
		}
		level, _ := rec["level"].(string)
		if level != "DEBUG" {
			t.Errorf("issued log level = %q, want %q\n%s", level, "DEBUG", line)
		}
	}
	if !found {
		t.Errorf("no mitm_cert_issued log line emitted; one CONNECT should trigger exactly one cache miss + gen\nlogs:\n%s", sb.String())
	}
}
