package main

// SPEC6 §15 #10 DoD pin — mitm_clock_skew integration
// reachability through the daemon CONNECT path.
//
// §10.2 contract:
//
//   mitm_clock_skew: {host, not_before, now}        Warn
//
// Fires when a freshly-generated leaf cert's NotBefore is in the
// future relative to the cache's current wall clock at the
// moment of issuance. GenerateLeaf backdates NotBefore by 5
// minutes per §5.1.3, so this should be impossible under normal
// NTP — but a backward system-clock jump AFTER the leaf was
// generated and BEFORE the cached entry expires leaves apt
// rejecting the server cert until the next eviction (§11 F17).
//
// The check fires for both freshly-generated leafs AND cache-
// reused leafs. This test exercises the freshly-generated path:
// drive one CONNECT to a fresh cache → leaf is generated with
// real-now backdating, then CheckLeafClockSkew runs against
// clockSkewNowForTest() which returns now-10m, so the leaf's
// NotBefore (now-5m) > skew-test-now (now-10m) → Warn fires.
//
// internal/proxy/clock_skew_test.go pins the primitive
// (CheckLeafClockSkew with constructed leafs and arbitrary
// `now`). This test pins the daemon-side wiring: that
// HandlerDeps.ClockSkewNowFn is plumbed through wireTlsMitm AND
// that ServeCONNECT actually calls CheckLeafClockSkew on every
// leaf returned by LeafCache.Get.
//
// Mutates package-level shutdownTimeout AND clockSkewNowForTest,
// so NOT t.Parallel.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func TestServe_BackwardClockJump_EmitsClockSkewWarn(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// Stub the clock-skew check to read 10 minutes earlier than
	// real wall time. GenerateLeaf produces NotBefore = realNow-5m,
	// so NotBefore > stub-now → skew detected. ONLY the skew check
	// uses this; handshake deadline + everything else still read
	// real time.Now (see HandlerDeps.ClockSkewNowFn doc in
	// internal/proxy/connect.go).
	oldFn := clockSkewNowForTest
	clockSkewNowForTest = func() time.Time { return time.Now().Add(-10 * time.Minute) }
	t.Cleanup(func() { clockSkewNowForTest = oldFn })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^skew-pin\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	// Capture Warn JSON. mitm_clock_skew is Warn per §10.2.
	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// One CONNECT triggers one leaf-cache miss → one GenerateLeaf
	// → one CheckLeafClockSkew call. ServeCONNECT writes the 200
	// line BEFORE the LeafCache.Get + skew-check step, so reading
	// the 200 only confirms the CONNECT was hijacked and accepted.
	// The serveDone wait below is what guarantees the handler ran
	// to completion (post-skew-check, post-handshake-timeout) so
	// the mitm_clock_skew line is in sb by the time we inspect it.
	conn := openCONNECT(t, cacheAddr, "skew-pin.test:443")
	defer conn.Close()

	// Capture roughly when the skew check ran so we can sanity-
	// check the `now` field in the emit. The check fires shortly
	// after ServeCONNECT writes 200 (LeafCache.Get is the very
	// next step), so this is within tens of microseconds of the
	// real emit time on the server side.
	capturedAt := time.Now()

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
		if msg != "mitm_clock_skew" {
			continue
		}
		if found {
			t.Errorf("more than one mitm_clock_skew emitted; one CONNECT should produce one skew check\n%s", line)
		}
		found = true

		host, _ := rec["host"].(string)
		if host != "skew-pin.test" {
			t.Errorf("mitm_clock_skew.host = %q, want %q\n%s", host, "skew-pin.test", line)
		}
		// not_before is RFC3339Nano string per CheckLeafClockSkew.
		nbStr, _ := rec["not_before"].(string)
		if nbStr == "" {
			t.Errorf("mitm_clock_skew missing not_before\n%s", line)
		} else {
			nb, err := time.Parse(time.RFC3339Nano, nbStr)
			if err != nil {
				t.Errorf("mitm_clock_skew.not_before = %q, parse error: %v\n%s", nbStr, err, line)
			} else {
				// NotBefore should be ~5m in the past relative to the
				// real wall clock (GenerateLeaf's backdating).
				delta := time.Since(nb)
				if delta < 4*time.Minute || delta > 6*time.Minute {
					t.Errorf("mitm_clock_skew.not_before = %v; expected ~5m before real now (delta=%v)\n%s",
						nb, delta, line)
				}
			}
		}
		// now is RFC3339Nano string of clockSkewNowForTest() output
		// (real-now - 10m).
		nowStr, _ := rec["now"].(string)
		if nowStr == "" {
			t.Errorf("mitm_clock_skew missing now\n%s", line)
		} else {
			nowParsed, err := time.Parse(time.RFC3339Nano, nowStr)
			if err != nil {
				t.Errorf("mitm_clock_skew.now = %q, parse error: %v\n%s", nowStr, err, line)
			} else {
				// Stub returns real-now - 10m, so the emit's now
				// should be roughly that far in the past.
				delta := capturedAt.Sub(nowParsed)
				if delta < 9*time.Minute || delta > 11*time.Minute {
					t.Errorf("mitm_clock_skew.now = %v; expected ~10m before captured-real-now (delta=%v)\n%s",
						nowParsed, delta, line)
				}
				// Sanity: NotBefore must be after now (the skew
				// invariant — otherwise the check wouldn't have
				// fired).
				if nb, err := time.Parse(time.RFC3339Nano, nbStr); err == nil {
					if !nb.After(nowParsed) {
						t.Errorf("mitm_clock_skew invariant violated: not_before %v not strictly after now %v",
							nb, nowParsed)
					}
				}
			}
		}
		// Field-set guard.
		for k := range rec {
			switch k {
			case "msg", "level", "time",
				"host", "not_before", "now":
				// ok
			default:
				t.Errorf("mitm_clock_skew carries unexpected field %q\n%s", k, line)
			}
		}
		if level, _ := rec["level"].(string); level != "WARN" {
			t.Errorf("mitm_clock_skew level = %q, want %q\n%s", level, "WARN", line)
		}
	}
	if !found {
		t.Errorf("no mitm_clock_skew log line emitted; CONNECT should trigger CheckLeafClockSkew\nlogs:\n%s", sb.String())
	}
}
