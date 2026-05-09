package main

// SPEC6 §10.2 + §11 F8 mitm_cert_cache_evicted DoD pin.
//
// The leaf cache emits one structured Info per eviction. main.go's
// SetOnEvict callback is the wiring that turns the cache's
// onEvict hook into both the §10.3 metric increment AND the §10.2
// log line. This test pins the LOG-side wiring with the spec
// field set:
//
//   msg=mitm_cert_cache_evicted
//   reason=lru (or expired)
//   host=<lower-cased CONNECT target literal>
//   age_seconds=<float, ≥0>
//
// Drives the eviction by setting CertCacheSize=1 and issuing two
// CONNECTs to different hosts. The first populates the cache; the
// second triggers LRU eviction of the first. ServeCONNECT calls
// LeafCache.Get BEFORE tls.Handshake, so reading the 200 line is
// proof that Get fired (and thus, on the second CONNECT, that
// eviction fired).
//
// Mutates the package-level shutdownTimeout, so NOT t.Parallel.

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureBuilder is a thread-safe writer that collects slog
// JSON output. Mirrors disabled_mode_parity_test.go's
// lockedBuilder pattern: the assertion reads after the request
// burst, but background goroutines (touchAsync, etc.) may still
// be writing — the mutex closes the race-detector window.
type captureBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (c *captureBuilder) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *captureBuilder) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

func TestServe_LeafCacheEviction_EmitsMITMCertCacheEvictedLog(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	// Match both CONNECT target hosts through the §6.6 fetch gate.
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex,
		`^host-a\.test$`, `^host-b\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	// Capacity 1 forces an eviction on the second CONNECT.
	cfg.TlsMitm.CertCacheSize = 1
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	// Capture Info-level JSON logs. mitm_cert_cache_evicted is
	// Info per §10.2; main.go's slog handler filters by level so
	// we install one set to Info explicitly.
	var sb captureBuilder
	logger := slog.New(slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	// First CONNECT populates the cache.
	connA := openCONNECT(t, cacheAddr, "host-a.test:443")
	defer connA.Close()
	// Second CONNECT triggers LRU eviction of host-a.test.
	connB := openCONNECT(t, cacheAddr, "host-b.test:443")
	defer connB.Close()

	// Shutdown the daemon (drains hijacked tunnels via §9.4
	// manager). After serveListeners returns, all log writes are
	// done, so reading the captured output is race-free.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	// Walk captured log records, find mitm_cert_cache_evicted,
	// assert spec field set.
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
		if msg != "mitm_cert_cache_evicted" {
			continue
		}
		found = true

		reason, _ := rec["reason"].(string)
		if reason != "lru" {
			t.Errorf("evict log reason = %q, want %q\n%s", reason, "lru", line)
		}
		// host is whichever literal got inserted first and then
		// evicted by the second insert. The two CONNECTs race
		// independent singleflight gens; the one whose gen
		// completes first inserts first, the other one's insert
		// then triggers LRU eviction. Both orderings are
		// spec-equivalent — assert the field is one of the two
		// known CONNECT targets, lower-cased per §5.1.3.
		host, _ := rec["host"].(string)
		if host != "host-a.test" && host != "host-b.test" {
			t.Errorf("evict log host = %q, want one of {host-a.test, host-b.test}\n%s", host, line)
		}
		// age_seconds: slog encodes float64 as JSON number, which
		// json.Unmarshal decodes as float64. Must be present and
		// non-negative; the absolute value is non-deterministic
		// (microseconds between Get-A and Get-B), so just check
		// the lower bound and that the field exists with the
		// right type.
		ageRaw, present := rec["age_seconds"]
		if !present {
			t.Errorf("evict log missing age_seconds field\n%s", line)
			continue
		}
		age, ok := ageRaw.(float64)
		if !ok {
			t.Errorf("evict log age_seconds not a number: %T %v\n%s", ageRaw, ageRaw, line)
			continue
		}
		if age < 0 {
			t.Errorf("evict log age_seconds = %v, want ≥0\n%s", age, line)
		}
		// SPEC §10.2 contract: exact field set {host, reason,
		// age_seconds}. Anything beyond that — except the
		// slog-builtin keys (`time`, `level`, `msg`) — would be
		// a spec drift.
		for k := range rec {
			switch k {
			case "msg", "level", "time", "host", "reason", "age_seconds":
				// ok
			default:
				t.Errorf("evict log carries unexpected field %q\n%s", k, line)
			}
		}
	}
	if !found {
		t.Errorf("no mitm_cert_cache_evicted log line emitted; capacity=1 + 2 distinct CONNECTs should have triggered LRU\nlogs:\n%s", sb.String())
	}
}

// openCONNECT opens a TCP conn, writes a CONNECT line for `target`,
// reads the 200 response, and returns the conn open. ServeCONNECT
// will then block in tls.Handshake reading from us. Caller closes
// the conn at test end.
func openCONNECT(t *testing.T, addr, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		_ = conn.Close()
		t.Fatalf("write CONNECT %s: %v", target, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read CONNECT %s response: %v", target, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		t.Fatalf("CONNECT %s status=%d, want 200", target, resp.StatusCode)
	}
	return conn
}
