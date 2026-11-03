package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

// Exercise real listener wiring, repeated wire headers, keep-alive isolation,
// error logs, and CONNECT identity propagation with the real tunnel manager.
func TestServeForwardedFor(t *testing.T) {
	for _, policy := range []struct {
		name    string
		trusted []string
		enabled bool
	}{
		{name: "disabled"},
		{name: "untrusted peer", trusted: []string{"192.0.2.10"}},
		{name: "trusted peer", trusted: []string{"127.0.0.1", "10.0.0.0/24"}, enabled: true},
	} {
		t.Run(policy.name, func(t *testing.T) {
			pre := metrics.Default.SnapshotNamesForTest()
			t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(pre) })
			cfg := minimalCfg(t.TempDir(), nil)
			cfg.Cache.TrustedProxies = policy.trusted
			cfg.TlsMitm.Enabled = true
			cfg.TlsMitm.AllowUnconstrainedCA = true
			cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"
			cfg.TlsMitm.CertCacheSize = 16
			cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
			cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
			cfg.Upstream.AllowedHostRegex = []string{`^example\.test$`}
			cert := certfixture.New(t)
			cfg.Cache.TLSCert, cfg.Cache.TLSKey = cert.Write(t, t.TempDir())
			roots := cert.Roots
			listen := func() net.Listener {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				return ln
			}
			plain, secure := listen(), listen()
			cfg.Cache.ListenTLS = secure.Addr().String()
			var logs captureBuilder
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {
				return serveListeners(ctx, cfg, logger, plain, secure, nil, nil)
			})
			if err := waitForDaemonReady(t, plain.Addr().String(), 10*time.Second); err != nil {
				t.Fatal(err)
			}

			type identity struct{ client, peer string }
			expected := make(map[string]identity)
			expect := func(key, forwarded, peer string) {
				addr := identity{client: peer}
				if policy.enabled && forwarded != "" {
					addr = identity{client: forwarded, peer: peer}
				}
				expected[key] = addr
			}
			dial := func(ln net.Listener) net.Conn {
				conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				return conn
			}
			readResponse := func(br *bufio.Reader, want int) {
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != want {
					t.Errorf("status = %d, want %d", resp.StatusCode, want)
				}
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, ln := range []net.Listener{plain, secure} {
				conn := dial(ln)
				label := "http"
				if ln == secure {
					label = "https"
					conn = tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
				}
				br := bufio.NewReader(conn)
				for i, tc := range []struct{ header, want string }{
					{header: "X-Forwarded-For: 203.0.113.99\r\nX-Forwarded-For: 198.51.100.7, 10.0.0.3\r\n", want: "198.51.100.7"},
					{header: "X-Forwarded-For: 198.51.100.8\r\n", want: "198.51.100.8"},
					{},
					{header: "X-Forwarded-For: 198.51.100.7, unknown\r\n"},
					{header: "X-Forwarded-For: " + strings.Repeat("unknown,", 40) + "198.51.100.7, 10.0.0.3\r\n", want: "198.51.100.7"},
					{header: "X-Forwarded-For: " + strings.Repeat("x", 9000) + "\r\nX-Forwarded-For: 198.51.100.7, 10.0.0.3\r\n", want: "198.51.100.7"},
				} {
					// Relative mirror-mode requests fail parsing without a mirror route.
					path := fmt.Sprintf("/xff-%s-%d", label, i)
					if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: cache.example.net\r\n%s\r\n", path, tc.header); err != nil {
						t.Fatal(err)
					}
					readResponse(br, http.StatusBadRequest)
					expect("request "+path, tc.want, conn.LocalAddr().String())
				}
				// Absolute forward-proxy URI, rejected before any upstream I/O.
				target := "http://denied.test/xff-" + label
				if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: denied.test\r\nX-Forwarded-For: 198.51.100.9\r\n\r\n", target); err != nil {
					t.Fatal(err)
				}
				readResponse(br, http.StatusForbidden)
				expect("request "+target, "198.51.100.9", conn.LocalAddr().String())
				_ = conn.Close()
			}
			// A rejected CONNECT must still log the trusted outer identity.
			denied := dial(plain)
			if _, err := io.WriteString(denied, "CONNECT denied.test:443 HTTP/1.1\r\nHost: denied.test:443\r\nX-Forwarded-For: 198.51.100.10\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			readResponse(bufio.NewReader(denied), http.StatusForbidden)
			expect("mitm_connect denied.test", "198.51.100.10", denied.LocalAddr().String())
			_ = denied.Close()

			// Even if the trusted proxy reports another trusted address (or sends no
			// XFF), the inner client must never be allowed to supply a new identity.
			for i, outer := range []string{"198.51.100.11", "10.0.0.4", ""} {
				raw := dial(plain)
				header := ""
				if outer != "" {
					header = "X-Forwarded-For: " + outer + "\r\n"
				}
				if _, err := fmt.Fprintf(raw, "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n%s\r\n", header); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(raw)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("CONNECT status = %d", resp.StatusCode)
				}
				inner := tls.Client(&prereadInnerConn{Conn: raw, br: br}, &tls.Config{RootCAs: loadAutoCAPool(t, cfg.Cache.Dir), ServerName: "example.test", MinVersion: tls.VersionTLS12})
				// Encoded NUL causes a deterministic parser rejection; no upstream needed.
				path := fmt.Sprintf("/xff-inner-%d/%%00", i)
				if _, err := fmt.Fprintf(inner, "GET %s HTTP/1.1\r\nHost: example.test\r\nX-Forwarded-For: 203.0.113.99\r\n\r\n", path); err != nil {
					t.Fatal(err)
				}
				readResponse(bufio.NewReader(inner), http.StatusBadRequest)
				expect("request "+path, outer, raw.LocalAddr().String())
				expect("mitm_connect "+raw.LocalAddr().String(), outer, raw.LocalAddr().String())
				_ = inner.Close()
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("daemon did not stop")
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatal(err)
				}
				key := ""
				switch rec["msg"] {
				case "request":
					key = "request " + rec["url"].(string)
				case "mitm_connect":
					key = "mitm_connect " + rec["host"].(string)
					if rec["host"] == "example.test" {
						peer := rec["peer_addr"]
						if peer == nil {
							peer = rec["client_addr"]
						}
						key = "mitm_connect " + peer.(string)
					}
				}
				want, ok := expected[key]
				if !ok {
					continue
				}
				if rec["client_addr"] != want.client {
					t.Errorf("%s client_addr = %v, want %q", key, rec["client_addr"], want.client)
				}
				if want.peer == "" {
					if _, ok := rec["peer_addr"]; ok {
						t.Errorf("unexpected peer_addr: %s", line)
					}
				} else if rec["peer_addr"] != want.peer {
					t.Errorf("%s peer_addr = %v, want %q", key, rec["peer_addr"], want.peer)
				}
				if strings.HasPrefix(key, "request /xff-inner-") && rec["mitm"] != true {
					t.Errorf("missing inner marker: %s", line)
				}
				delete(expected, key)
			}
			for key := range expected {
				t.Errorf("missing log: %s\n%s", key, logs.String())
			}
		})
	}
}
