package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustParsePEMCert decodes the first CERTIFICATE block in `pemBytes`
// and returns the parsed cert. Test helper.
func mustParsePEMCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(pemBytes)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatalf("no CERTIFICATE PEM block in input")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

// writeAptConfFixture builds a TOML config file under t.TempDir()
// with the given listen, advertise_host, mitmEnabled, and any extra
// [tls_mitm] body. Returns (cfgPath, cacheDir).
func writeAptConfFixture(t *testing.T, listen, advertiseHost string, mitmEnabled bool, extraTlsMitm string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[cache]\ndir = \"" + cacheDir + "\"\nlisten = \"" + listen + "\"\n"
	if advertiseHost != "" {
		body += "advertise_host = \"" + advertiseHost + "\"\n"
	}
	body += "\n[tls_mitm]\n"
	if mitmEnabled {
		body += "enabled = true\n"
	} else {
		body += "enabled = false\n"
	}
	body += extraTlsMitm
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, cacheDir
}

// TestPrintAptConf_UnicastListen_NoAdvertise emits the proxy lines
// using cache.listen verbatim. MITM disabled → no CA section.
func TestPrintAptConf_UnicastListen_NoAdvertise(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "127.0.0.1:3142", "", false, "")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, `Acquire::http::Proxy "http://127.0.0.1:3142"`) {
		t.Errorf("output missing proxy line:\n%s", out)
	}
	if !strings.Contains(out, `Acquire::https::Proxy "http://127.0.0.1:3142"`) {
		t.Errorf("output missing https proxy line:\n%s", out)
	}
	if strings.Contains(out, "fingerprint") {
		t.Errorf("disabled mode should NOT include CA fingerprint line:\n%s", out)
	}
}

// TestPrintAptConf_AdvertiseHostNoPort_AppendsListenPort: the
// operator wrote `advertise_host = "cache.example.com"` but the
// listen port is 3142, so the snippet must emit
// http://cache.example.com:3142.
func TestPrintAptConf_AdvertiseHostNoPort_AppendsListenPort(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "0.0.0.0:3142", "cache.example.com", false, "")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"http://cache.example.com:3142"`) {
		t.Errorf("expected http://cache.example.com:3142 in output:\n%s", stdout.String())
	}
}

// TestPrintAptConf_AdvertiseHostWithPort_UsedVerbatim: the
// operator wrote `advertise_host = "cache.example.com:9999"`; the
// snippet emits that port even though listen says 3142.
func TestPrintAptConf_AdvertiseHostWithPort_UsedVerbatim(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "0.0.0.0:3142", "cache.example.com:9999", false, "")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"http://cache.example.com:9999"`) {
		t.Errorf("expected http://cache.example.com:9999 in output:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), ":3142") {
		t.Errorf("listen port 3142 should not appear when advertise_host has its own port:\n%s", stdout.String())
	}
}

// TestPrintAptConf_UnspecifiedListen_NoAdvertise_Returns5: the
// 0.0.0.0 + no advertise_host case must exit 5 with a stderr
// diagnostic naming the listen address.
func TestPrintAptConf_UnspecifiedListen_NoAdvertise_Returns5(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "0.0.0.0:3142", "", false, "")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 5 {
		t.Errorf("exit = %d, want 5; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "0.0.0.0:3142") {
		t.Errorf("stderr should name the unspecified listen, got %q", stderr.String())
	}
}

// TestPrintAptConf_IPv6UnspecifiedListen_NoAdvertise_Returns5: the
// `[::]:3142` form is also unspecified.
func TestPrintAptConf_IPv6UnspecifiedListen_NoAdvertise_Returns5(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "[::]:3142", "", false, "")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 5 {
		t.Errorf("exit = %d, want 5; stderr=%s", code, stderr.String())
	}
}

// TestPrintAptConf_ConfigUnreadable_Returns1.
func TestPrintAptConf_ConfigUnreadable_Returns1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf("/no/such/path.toml", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
}

// TestPrintAptConf_MITMEnabled_CAOnDisk_EmitsFingerprint: when
// MITM is on AND a CA cert exists at the auto-gen path, the
// snippet must include the fingerprint + path comment block.
func TestPrintAptConf_MITMEnabled_CAOnDisk_EmitsFingerprint(t *testing.T) {
	cfg, cacheDir := writeAptConfFixture(t, "127.0.0.1:3142", "", true, "allow_unconstrained_ca = true\n")

	// Materialize a CA in-place by running `ca print` first.
	var capStdout, capStderr bytes.Buffer
	if rc := runCAPrint([]string{"-config", cfg}, &capStdout, &capStderr); rc != 0 {
		t.Fatalf("setup: ca print failed: rc=%d stderr=%s", rc, capStderr.String())
	}

	// Confirm the cert exists at the auto-gen location, then
	// compute the expected fingerprint from disk for assertion.
	caCertPath := filepath.Join(cacheDir, "ca", "ca.crt")
	pemBytes, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	if len(pemBytes) == 0 || !strings.HasPrefix(string(pemBytes), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("ca.crt is not PEM: %q", pemBytes)
	}

	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "CA fingerprint (SHA-256):") {
		t.Errorf("expected fingerprint line in output:\n%s", out)
	}
	if !strings.Contains(out, "CA path on the cache host: "+caCertPath) {
		t.Errorf("expected CA path %q in output:\n%s", caCertPath, out)
	}

	// Cross-check the fingerprint matches what we'd compute against
	// the on-disk cert. peekCAFingerprint sha256s the cert.Raw, which
	// equals sha256(blk.Bytes) for a single-block PEM.
	want := mustComputeCertFingerprint(t, pemBytes)
	if !strings.Contains(out, want) {
		t.Errorf("fingerprint hex %q not in output:\n%s", want, out)
	}
}

// TestPrintAptConf_MITMEnabled_NoCAOnDisk_StderrNote: MITM enabled
// but the CA hasn't been materialized. Snippet must still emit the
// proxy lines (exit 0) and stderr must explain how to materialize.
func TestPrintAptConf_MITMEnabled_NoCAOnDisk_StderrNote(t *testing.T) {
	cfg, _ := writeAptConfFixture(t, "127.0.0.1:3142", "", true, "allow_unconstrained_ca = true\n")
	var stdout, stderr bytes.Buffer
	code := runPrintAptConf(cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Acquire::http::Proxy") {
		t.Errorf("proxy lines should still be emitted, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "CA fingerprint") {
		t.Errorf("fingerprint should NOT be in stdout when CA missing:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "ca print") {
		t.Errorf("stderr should advise running `ca print`, got %q", stderr.String())
	}
}

// mustComputeCertFingerprint parses the first PEM CERTIFICATE block
// in `pemBytes` and returns the lower-cased hex sha256 of cert.Raw.
// Helper for the cross-check test above.
func mustComputeCertFingerprint(t *testing.T, pemBytes []byte) string {
	t.Helper()
	// Find the first BEGIN/END block.
	begin := strings.Index(string(pemBytes), "-----BEGIN CERTIFICATE-----")
	end := strings.Index(string(pemBytes), "-----END CERTIFICATE-----")
	if begin < 0 || end < 0 {
		t.Fatalf("no CERTIFICATE block found in pem")
	}
	// Parse via stdlib for the Raw bytes.
	cert := mustParsePEMCert(t, pemBytes)
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
