package main

// SPEC6 §11 F18 + §15 #2 — CA expires mid-runtime, all subsequent
// client TLS handshakes fail.
//
// §11 F18: "CA expires mid-lifetime. All client TLS handshakes
// fail; `mitm_connect` Warn `outcome=tls_failed` rate spikes;
// operator's `acu_mitm_ca_not_after_unixtime` alert (set to fire
// 30 days before expiry) catches this before the spike."
//
// §12.4 maps F18 to "12.3 chaos (CA expiry mid-runtime)". §12.3
// describes the test as: "Set the CA `not_after` to 60 seconds
// out, run a CONNECT every 10 seconds, verify successful
// handshakes until the moment of expiry, then every CONNECT
// after fails with `outcome=tls_failed`."
//
// This implementation compresses the timeline (8-second NotAfter,
// two CONNECTs spanning the expiry boundary instead of a
// long-running poll) so the test fits a normal `go test` budget
// while still pinning the same invariants:
//
//   1. A handshake BEFORE expiry succeeds (CA still valid; chain
//      validation passes; daemon emits no tls_failed).
//   2. A handshake AFTER expiry fails (CA NotAfter < now → client
//      chain validation rejects with x509 expiry error → cache
//      sees a TLS error during tls.Server.Handshake → emits
//      mitm_connect Warn with outcome=tls_failed).
//
// Driven via the operator-supplied CA path (TlsMitm.CaCert /
// CaKey) because the auto-gen path's CACertLifetime has a 24h
// minimum (§5.2 validation rejects anything shorter) — a few-second
// lifetime would never load. The supplied path runs through
// validateSuppliedCA at internal/proxy/tlsmitm/ca.go:240 which
// only requires `now.Before(cert.NotAfter)`; that holds the
// instant after the CA is minted, lets the daemon boot, and the
// expiry comes into effect a few seconds into runtime.
//
// Mutates package-level shutdownTimeout — NOT t.Parallel.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mintSuppliedCA writes an ECDSA-P-256 CA pair into dir and
// returns (certPath, keyPath, *x509.Certificate). NotAfter is
// caller-controlled so the test can drive the F18 scenario by
// putting NotAfter close to now and waiting for expiry mid-test.
func mintSuppliedCA(t *testing.T, dir string, notAfter time.Time) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "f18-test-CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	certPath := filepath.Join(dir, "supplied-ca.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyPath := filepath.Join(dir, "supplied-ca.key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath, cert
}

func TestServe_SuppliedCAExpiresMidRuntime_HandshakeFails(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	caDir := t.TempDir()
	// 8-second window: must comfortably exceed the 10s readiness
	// timeout's worst case PLUS first-handshake latency, otherwise
	// a slow CI worker could let Phase 1 land AFTER expiry and the
	// "successful handshake before expiry" half of F18 would
	// silently fail to fire. The remaining-validity guard right
	// after waitForDaemonReady fails the test loudly with a
	// diagnostic if we somehow reached Phase 1 with too little
	// validity left, which is preferable to a misleading regression
	// signal.
	caNotAfter := time.Now().Add(8 * time.Second)
	caCertPath, caKeyPath, suppliedCert := mintSuppliedCA(t, caDir, caNotAfter)

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Upstream.AllowedHostRegex = append(cfg.Upstream.AllowedHostRegex, `^example\.test$`)
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CaCert = caCertPath
	cfg.TlsMitm.CaKey = caKeyPath
	cfg.TlsMitm.CertCacheSize = 16
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

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

	// Remaining-validity guard. waitForDaemonReady can take up to
	// 10s on a slow CI worker; if it ate most of the validity
	// window, Phase 1 would run past expiry and report a misleading
	// "before-expiry handshake failed" regression. Use
	// suppliedCert.NotAfter (not caNotAfter) since X.509 ASN.1 time
	// encoding is second-granular and the parsed cert is what was
	// actually written to disk and served by the daemon. Fail
	// loudly with a tuning hint rather than running an invalid
	// test: skip would silently never run F18.
	const phase1ValiditySlack = 2 * time.Second
	if remaining := time.Until(suppliedCert.NotAfter); remaining < phase1ValiditySlack {
		t.Fatalf("test infrastructure too slow: only %v of CA validity left after daemon boot (need ≥%v); bump the caNotAfter window or speed up boot", remaining, phase1ValiditySlack)
	}

	// Trust pool: just the supplied CA. Both handshakes use this —
	// the BEFORE handshake should succeed; the AFTER handshake
	// should fail because the CA itself is expired by handshake
	// time, regardless of what's in the trust store.
	pool := x509.NewCertPool()
	pool.AddCert(suppliedCert)

	// === Phase 1: BEFORE expiry — handshake must succeed ===
	if err := doConnectAndHandshake(t, cacheAddr, "example.test", pool, true); err != nil {
		t.Fatalf("phase 1 (BEFORE expiry): handshake should have succeeded but failed: %v", err)
	}

	// Wait until just past the cert's actual NotAfter. Sleeping a
	// fixed duration would force a flaky tradeoff: too short →
	// Phase 2 runs while CA is still valid; too long → wasted test
	// budget. time.Until measures from "now" rather than boot, so
	// the total wall clock budget compresses naturally regardless
	// of how long Phase 1 took.
	//
	// Use suppliedCert.NotAfter rather than caNotAfter: X.509
	// ASN.1 time encoding is second-granular, so the parsed cert's
	// NotAfter can differ from the template time by up to 1
	// second. The parsed value is what the daemon actually serves
	// and what the client validates against — that's the right
	// reference for the sleep target.
	time.Sleep(time.Until(suppliedCert.NotAfter) + 1*time.Second)

	// === Phase 2: AFTER expiry — handshake must fail ===
	hsErr := doConnectAndHandshake(t, cacheAddr, "example.test", pool, false)
	if hsErr == nil {
		t.Fatalf("phase 2 (AFTER expiry): handshake should have failed; got nil")
	}
	// The chain failure must be specifically the CA-expiry path.
	// errors.As walks the wrapper chain (current stdlib wraps the
	// CertificateInvalidError inside a *tls.CertificateVerificationError),
	// so a single errors.As call extracts the underlying x509
	// error regardless of how Go wrapped it. We then PIN
	// Reason == x509.Expired — without this, a different chain
	// failure (UnknownAuthority from a missing trust anchor,
	// hostname mismatch, etc.) would accidentally satisfy a
	// looser check and the F18 invariant wouldn't be pinned.
	var invalidErr x509.CertificateInvalidError
	if !errors.As(hsErr, &invalidErr) {
		t.Fatalf("phase 2 handshake err = %v (%T); want x509.CertificateInvalidError (possibly wrapped in *tls.CertificateVerificationError)", hsErr, hsErr)
	}
	if invalidErr.Reason != x509.Expired {
		t.Fatalf("phase 2 chain-validity reason = %v, want x509.Expired (the CA is past NotAfter)", invalidErr.Reason)
	}

	// Cancel the daemon to flush handler goroutines so any pending
	// mitm_connect Warn lines are in sb before we read.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return")
	}

	// Walk every captured mitm_connect record. Phase 1 emits a
	// Warn-level inner_stream_failed (handshake succeeded, then
	// the test client closed the conn without sending an inner
	// GET — ReadRequest returns EOF, daemon emits at Warn). Phase
	// 2 emits a Warn-level tls_failed. So exactly TWO records,
	// with exactly ONE outcome=tls_failed.
	//
	// Asserting EXACTLY 1 tls_failed (not ≥1) is load-bearing: a
	// regression where Phase 1 also fails the handshake — say,
	// because something in the daemon's CA-load path leaves the
	// signing key unusable from the start — would otherwise
	// silently produce 2 tls_failed records and pass under a
	// looser ≥1 check, masking a real bug. Pinning the EXACT
	// number cleanly disambiguates "F18 fired" from "every
	// handshake failed for an unrelated reason".
	var connectRecs []map[string]any
	var connectLines []string
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON log line: %v\n%s", err, line)
		}
		if msg, _ := rec["msg"].(string); msg != "mitm_connect" {
			continue
		}
		connectRecs = append(connectRecs, rec)
		connectLines = append(connectLines, line)
	}

	tlsFailedCount := 0
	var tlsFailedRec map[string]any
	var tlsFailedLine string
	for i, rec := range connectRecs {
		if outcome, _ := rec["outcome"].(string); outcome == "tls_failed" {
			tlsFailedCount++
			tlsFailedRec = rec
			tlsFailedLine = connectLines[i]
		}
	}
	if tlsFailedCount != 1 {
		t.Fatalf("got %d mitm_connect records with outcome=tls_failed, want 1; full set:\n%s",
			tlsFailedCount, strings.Join(connectLines, "\n"))
	}
	// Spot-check the tls_failed record for §10.2 field set.
	if level, _ := tlsFailedRec["level"].(string); level != "WARN" {
		t.Errorf("tls_failed level = %q, want WARN\n%s", level, tlsFailedLine)
	}
	if host, _ := tlsFailedRec["host"].(string); host != "example.test" {
		t.Errorf("tls_failed host = %q, want example.test\n%s", host, tlsFailedLine)
	}
	if reason, _ := tlsFailedRec["reason"].(string); !strings.HasPrefix(reason, "tls:") {
		t.Errorf("tls_failed reason = %q, want prefix \"tls:\"\n%s", reason, tlsFailedLine)
	}
}

// doConnectAndHandshake opens a CONNECT to cacheAddr targeting
// host:443, then runs a tls.Client handshake using pool as the
// trust store. Returns nil iff the handshake succeeded. When
// expectSuccess is true, intermediate failures (dial / CONNECT
// status) are reported via t.Fatalf; when false, every step
// short-circuits to returning the error so the caller can pin
// it.
func doConnectAndHandshake(t *testing.T, cacheAddr, host string, pool *x509.CertPool, expectSuccess bool) error {
	t.Helper()
	rawConn, err := net.Dial("tcp", cacheAddr)
	if err != nil {
		if expectSuccess {
			t.Fatalf("dial cache: %v", err)
		}
		return err
	}
	defer rawConn.Close()
	if err := rawConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set rawConn deadline: %v", err)
	}

	if _, err := rawConn.Write([]byte("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n")); err != nil {
		if expectSuccess {
			t.Fatalf("write CONNECT: %v", err)
		}
		return err
	}
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, nil)
	if err != nil {
		if expectSuccess {
			t.Fatalf("read CONNECT response: %v", err)
		}
		return err
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		err := errors.New("CONNECT non-200: " + connectResp.Status)
		if expectSuccess {
			t.Fatalf("%v", err)
		}
		return err
	}

	tlsClient := tls.Client(rawConn, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hsCancel()
	hsErr := tlsClient.HandshakeContext(hsCtx)
	if hsErr == nil {
		// Drain any inner-stream side state cleanly.
		_ = tlsClient.Close()
	}
	return hsErr
}
