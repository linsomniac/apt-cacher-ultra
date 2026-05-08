package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeMinimalConfig writes a TOML config to t.TempDir() that:
//   - points cache.dir at a fresh tempdir (so daemon-side paths exist),
//   - sets tls_mitm.enabled per the caller's wish.
//
// Returns the config path.
func writeMinimalConfig(t *testing.T, mitmEnabled bool, extra string) string {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[cache]\ndir = \"" + cacheDir + "\"\nlisten = \"127.0.0.1:3142\"\n\n"
	if mitmEnabled {
		body += "[tls_mitm]\nenabled = true\n" + extra
	} else {
		body += "[tls_mitm]\nenabled = false\n" + extra
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// writeSelfSignedCAPEM generates a throwaway CA cert + private key
// and writes them to (certPath, keyPath). Returns the cert PEM bytes
// for assertion comparison.
func writeSelfSignedCAPEM(t *testing.T, certPath, keyPath string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPEM
}

func TestCAPrint_DisabledReturns1(t *testing.T) {
	cfg := writeMinimalConfig(t, false, "")
	var stdout, stderr bytes.Buffer
	code := runCAPrint([]string{"-config", cfg}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty in disabled mode, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "tls_mitm.enabled = false") {
		t.Errorf("stderr should explain why exit=1, got %q", stderr.String())
	}
}

func TestCAPrint_ConfigUnreadableReturns3(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCAPrint([]string{"-config", "/no/such/path.toml"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("exit code = %d, want 3; stderr=%s", code, stderr.String())
	}
}

func TestCAPrint_SuppliedHappyPath(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	wantPEM := writeSelfSignedCAPEM(t, certPath, keyPath)

	extra := "ca_cert = \"" + certPath + "\"\nca_key = \"" + keyPath + "\"\n"
	cfg := writeMinimalConfig(t, true, extra)

	var stdout, stderr bytes.Buffer
	code := runCAPrint([]string{"-config", cfg}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), wantPEM) {
		t.Errorf("stdout PEM doesn't match supplied cert PEM\n got %q\nwant %q", stdout.String(), string(wantPEM))
	}
}

func TestCAPrint_SuppliedKeyMode_AuditWarn(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writeSelfSignedCAPEM(t, certPath, keyPath)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod key 0644: %v", err)
	}

	extra := "ca_cert = \"" + certPath + "\"\nca_key = \"" + keyPath + "\"\n"
	cfg := writeMinimalConfig(t, true, extra)

	var stdout, stderr bytes.Buffer
	code := runCAPrint([]string{"-config", cfg}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARNING") || !strings.Contains(stderr.String(), "0644") {
		t.Errorf("expected audit warning naming 0644 in stderr, got %q", stderr.String())
	}
}

func TestCAPrint_SuppliedParseFailReturns2(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, []byte("not a real PEM\n"), 0o644); err != nil {
		t.Fatalf("write garbage cert: %v", err)
	}
	// Validate refuses an unreadable supplied key, so write a real
	// (matching) key to make config.Load happy — only the cert content
	// drives the parse-fail branch under test.
	if err := os.WriteFile(keyPath, []byte("not a real key\n"), 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}

	extra := "ca_cert = \"" + certPath + "\"\nca_key = \"" + keyPath + "\"\n"
	cfg := writeMinimalConfig(t, true, extra)

	var stdout, stderr bytes.Buffer
	code := runCAPrint([]string{"-config", cfg}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
}

// TestCAPrint_AutoGenHappyPath_FreshDir proves the SPEC §14.1 case 3
// branch: no `<cache_dir>/ca/ca.crt` exists, so `ca print` runs the
// §4.2.1 atomic generate path and prints the freshly-generated cert.
// A subsequent invocation must take case 2 (load, not regenerate).
func TestCAPrint_AutoGenHappyPath_FreshDir(t *testing.T) {
	cfg := writeMinimalConfig(t, true, "allow_unconstrained_ca = true\n")

	var stdout1, stderr1 bytes.Buffer
	code := runCAPrint([]string{"-config", cfg}, &stdout1, &stderr1)
	if code != 0 {
		t.Fatalf("first invocation: exit code = %d, want 0; stderr=%s", code, stderr1.String())
	}
	if !strings.HasPrefix(stdout1.String(), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("stdout should start with PEM cert header, got %q", stdout1.String())
	}

	var stdout2, stderr2 bytes.Buffer
	code = runCAPrint([]string{"-config", cfg}, &stdout2, &stderr2)
	if code != 0 {
		t.Fatalf("second invocation: exit code = %d, want 0; stderr=%s", code, stderr2.String())
	}
	if !bytes.Equal(stdout1.Bytes(), stdout2.Bytes()) {
		t.Errorf("second invocation should print the SAME cert (load, not regenerate)\n  first: %q\n second: %q", stdout1.String(), stdout2.String())
	}
}

// TestCAPrint_LockContention_Returns4 spawns a goroutine that holds
// the §4.2.2 flock past the LockTimeout, then runs `ca print`. The
// flock contention must produce exit code 4 with a stderr message
// pointing at the lock path.
func TestCAPrint_LockContention_Returns4(t *testing.T) {
	cfg := writeMinimalConfig(t, true, "allow_unconstrained_ca = true\n")

	// Resolve cfg → cache_dir/ca/.ca.lock. Simplest: run a no-op
	// `ca print` first to materialize the dir + ca files, but we
	// want a fresh directory so the contention triggers BEFORE
	// ca.crt exists. Instead, parse the cfg path directly to find
	// the cache dir.
	cacheDir := mustReadCacheDir(t, cfg)
	caDir := filepath.Join(cacheDir, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("mkdir ca dir: %v", err)
	}
	lockPath := filepath.Join(caDir, ".ca.lock")

	// Open the lockfile and hold an exclusive flock from a goroutine.
	// We block until `ca print` has had a chance to time out (the
	// default LockTimeout is 30s — too long for tests, so we pass
	// a temporarily-shortened timeout via a test seam if available).
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lockfile: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	// Override the LockTimeout used by `ca print` via a runtime
	// override. The subcommand wires a hardcoded LoadOrGenerate call,
	// so we shorten the lock timeout via the test-only seam exposed
	// by the tlsmitm package.
	restore := setCAPrintLockTimeoutForTest(150 * time.Millisecond)
	defer restore()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- runCAPrint([]string{"-config", cfg}, &stdout, &stderr)
	}()

	select {
	case code := <-done:
		if code != 4 {
			t.Errorf("exit code = %d, want 4; stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "lock contention") {
			t.Errorf("stderr should mention lock contention, got %q", stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("`ca print` did not return within 5s; lock-timeout seam wired wrong")
	}
	wg.Wait()
}

// mustReadCacheDir parses the TOML config at cfgPath enough to
// extract `cache.dir`. We avoid pulling in the full config.Load here
// because Load applies defaults + validates — we need just the raw
// path before any of that runs. A two-line search through the file
// suffices for this test fixture's well-known shape.
func mustReadCacheDir(t *testing.T, cfgPath string) string {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read cfg: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "dir") {
			continue
		}
		// `dir = "..."`
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"`)
		if val != "" {
			return val
		}
	}
	t.Fatalf("could not parse cache.dir from cfg %q", cfgPath)
	return ""
}
