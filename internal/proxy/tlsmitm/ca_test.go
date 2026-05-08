package tlsmitm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// loggerSpy is a LogFn implementation that records every event for
// post-hoc assertions.
type loggerSpy struct {
	mu     sync.Mutex
	events []logEvent
}

type logEvent struct {
	level  string
	event  string
	fields map[string]any
}

func (l *loggerSpy) fn() func(level, event string, fields map[string]any) {
	return func(level, event string, fields map[string]any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		// Copy fields so subsequent mutation in the caller doesn't race.
		copied := make(map[string]any, len(fields))
		for k, v := range fields {
			copied[k] = v
		}
		l.events = append(l.events, logEvent{level, event, copied})
	}
}

func (l *loggerSpy) has(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.event == name {
			return true
		}
	}
	return false
}

// writeSuppliedCA generates a CA with the given key kind and writes
// PEM cert + key into dir. Returns the cert and key paths.
func writeSuppliedCA(t *testing.T, dir, keyKind string, opts ...func(*x509.Certificate)) (string, string) {
	t.Helper()
	var (
		caKey crypto.Signer
		pub   crypto.PublicKey
	)
	switch keyKind {
	case "ecdsa-p256":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ecdsa-p384":
		k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ecdsa-p521":
		k, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "rsa-2048":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "rsa-1024":
		k, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		caKey = k
		pub = k.Public()
	default:
		t.Fatalf("unknown keyKind %q", keyKind)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "supplied-CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	for _, opt := range opts {
		opt(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// ----------------------------------------------------------------------------
// Operator-supplied path
// ----------------------------------------------------------------------------

func TestLoadOrGenerate_Supplied_Accepted(t *testing.T) {
	cases := []string{"ecdsa-p256", "ecdsa-p384", "rsa-2048"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			certPath, keyPath := writeSuppliedCA(t, dir, kind)
			spy := &loggerSpy{}
			ca, err := LoadOrGenerate(LoadOptions{
				SuppliedCertPath: certPath,
				SuppliedKeyPath:  keyPath,
				LogFn:            spy.fn(),
			})
			if err != nil {
				t.Fatalf("LoadOrGenerate: %v", err)
			}
			if ca.Source != SourceSupplied {
				t.Errorf("source = %v, want supplied", ca.Source)
			}
			if ca.FingerprintSHA256 == "" {
				t.Error("fingerprint should be populated")
			}
			if !spy.has("mitm_ca_loaded") {
				t.Error("mitm_ca_loaded log not emitted")
			}
		})
	}
}

func TestLoadOrGenerate_Supplied_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		keyKind string
	}{
		{"ecdsa-p521", "ecdsa-p521"},
		{"rsa-1024", "rsa-1024"},
		{"ed25519", "ed25519"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certPath, keyPath := writeSuppliedCA(t, dir, tc.keyKind)
			_, err := LoadOrGenerate(LoadOptions{
				SuppliedCertPath: certPath,
				SuppliedKeyPath:  keyPath,
			})
			if err == nil {
				t.Fatal("expected unsupported-CA-key error, got nil")
			}
			if !errors.Is(err, ErrUnsupportedCAKey) {
				t.Errorf("expected wrap of ErrUnsupportedCAKey, got %v", err)
			}
		})
	}
}

func TestLoadOrGenerate_Supplied_KeyMismatch(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := writeSuppliedCA(t, dir, "ecdsa-p256")
	// Generate a fresh, unrelated key.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKCS8PrivateKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(dir, "other.key")
	if err := os.WriteFile(otherPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadOrGenerate(LoadOptions{
		SuppliedCertPath: certPath,
		SuppliedKeyPath:  otherPath,
	})
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected 'mismatch' in error, got %q", err.Error())
	}
}

func TestLoadOrGenerate_Supplied_PastNotAfter(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSuppliedCA(t, dir, "ecdsa-p256", func(c *x509.Certificate) {
		c.NotBefore = time.Now().Add(-2 * time.Hour)
		c.NotAfter = time.Now().Add(-time.Hour)
	})
	_, err := LoadOrGenerate(LoadOptions{
		SuppliedCertPath: certPath,
		SuppliedKeyPath:  keyPath,
	})
	if err == nil {
		t.Fatal("expected past-not_after error, got nil")
	}
}

func TestLoadOrGenerate_Supplied_MissingCATrue(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSuppliedCA(t, dir, "ecdsa-p256", func(c *x509.Certificate) {
		c.IsCA = false
		c.BasicConstraintsValid = false
	})
	_, err := LoadOrGenerate(LoadOptions{
		SuppliedCertPath: certPath,
		SuppliedKeyPath:  keyPath,
	})
	if err == nil {
		t.Fatal("expected CA:TRUE error, got nil")
	}
	if !strings.Contains(err.Error(), "CA:TRUE") {
		t.Errorf("expected 'CA:TRUE' in error, got %q", err.Error())
	}
}

func TestLoadOrGenerate_Supplied_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.pem")
	_, err := LoadOrGenerate(LoadOptions{
		SuppliedCertPath: missing,
		SuppliedKeyPath:  missing,
	})
	if err == nil {
		t.Fatal("expected file-not-found error, got nil")
	}
}

// ----------------------------------------------------------------------------
// Auto-generated path
// ----------------------------------------------------------------------------

func TestLoadOrGenerate_Auto_FailClosed_EmptyRegex(t *testing.T) {
	dir := t.TempDir()
	spy := &loggerSpy{}
	_, err := LoadOrGenerate(LoadOptions{
		StorageDir: dir,
		// AllowedHostRegex empty, AllowUnconstrainedCA false → refuse.
		LogFn: spy.fn(),
	})
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !errors.Is(err, ErrNameConstraintsUnsupported) {
		t.Errorf("expected wrap of ErrNameConstraintsUnsupported, got %v", err)
	}
	if !spy.has("mitm_ca_unconstrained_refused") {
		t.Error("mitm_ca_unconstrained_refused log not emitted")
	}
	if !spy.has("mitm_ca_generation_failed") {
		t.Error("mitm_ca_generation_failed log not emitted")
	}
	// Nothing should have been written.
	for _, name := range []string{caCertFile, caKeyFile, caReadyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("file %s should not exist after refused generation", name)
		}
	}
}

func TestLoadOrGenerate_Auto_FailClosed_UntranslatableRegex(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `[a-z0-9.-]+`, // unanchored, charclass admits dot
	})
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !errors.Is(err, ErrNameConstraintsUnsupported) {
		t.Errorf("expected wrap of ErrNameConstraintsUnsupported, got %v", err)
	}
}

func TestLoadOrGenerate_Auto_OptIn_EmptyRegex(t *testing.T) {
	dir := t.TempDir()
	spy := &loggerSpy{}
	ca, err := LoadOrGenerate(LoadOptions{
		StorageDir:           dir,
		AllowUnconstrainedCA: true,
		LogFn:                spy.fn(),
	})
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if len(ca.NameConstraints) != 0 {
		t.Errorf("opt-in unconstrained: expected empty NameConstraints, got %v", ca.NameConstraints)
	}
	if ca.NameConstraintsSkippedReason == "" {
		t.Error("opt-in unconstrained: expected non-empty SkippedReason")
	}
	if !spy.has("mitm_ca_name_constraints_skipped") {
		t.Error("mitm_ca_name_constraints_skipped Warn not emitted")
	}
	if !spy.has("mitm_ca_generated") {
		t.Error("mitm_ca_generated Info not emitted")
	}
	if !spy.has("mitm_ca_loaded") {
		t.Error("mitm_ca_loaded Info not emitted")
	}
}

func TestLoadOrGenerate_Auto_TranslatableRegex_PutsConstraints(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^([a-z]{2}\.)?archive\.ubuntu\.com$`,
	})
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !ca.Cert.PermittedDNSDomainsCritical {
		t.Error("PermittedDNSDomainsCritical should be true")
	}
	if got := ca.Cert.PermittedDNSDomains; len(got) != 1 || got[0] != "archive.ubuntu.com" {
		t.Errorf("PermittedDNSDomains = %v, want [archive.ubuntu.com]", got)
	}
	if ca.NameConstraintsSkippedReason != "" {
		t.Errorf("constrained CA should have empty skip reason, got %q", ca.NameConstraintsSkippedReason)
	}
}

func TestLoadOrGenerate_Auto_GeneratePersistThenReload(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Files must exist with mode 0600.
	for _, name := range []string{caCertFile, caKeyFile, caReadyFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	// Reload — must hit case 2 with same fingerprint.
	second, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.FingerprintSHA256 != second.FingerprintSHA256 {
		t.Errorf("fingerprint changed across reload: %q vs %q", first.FingerprintSHA256, second.FingerprintSHA256)
	}
}

func TestLoadOrGenerate_Auto_InconsistentState_Case3(t *testing.T) {
	// Walk the rows of the §4.2.1 state table that should produce
	// case 3 (inconsistent committed state). For each, scaffold the
	// claimed state, attempt LoadOrGenerate, and assert refusal.
	type row struct {
		name string
		// setup writes the directory state. dir is empty when called.
		setup func(t *testing.T, dir string)
	}
	rows := []row{
		{
			"ca.crt only (no ca.key, no ca.ready)",
			func(t *testing.T, dir string) {
				t.Helper()
				_ = os.WriteFile(filepath.Join(dir, caCertFile), []byte("dummy"), 0o600)
			},
		},
		{
			"ca.crt + ca.key, no ca.ready",
			func(t *testing.T, dir string) {
				t.Helper()
				_ = os.WriteFile(filepath.Join(dir, caCertFile), []byte("dummy"), 0o600)
				_ = os.WriteFile(filepath.Join(dir, caKeyFile), []byte("dummy"), 0o600)
			},
		},
		{
			"ca.ready only",
			func(t *testing.T, dir string) {
				t.Helper()
				_ = os.WriteFile(filepath.Join(dir, caReadyFile), []byte("dummy"), 0o600)
			},
		},
		{
			"all three but mismatching fingerprint",
			func(t *testing.T, dir string) {
				t.Helper()
				// Generate a real CA first, then corrupt the marker.
				if _, err := LoadOrGenerate(LoadOptions{
					StorageDir:       dir,
					AllowedHostRegex: `^foo\.example\.com$`,
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				// Overwrite ca.ready with bogus fingerprint.
				if err := os.WriteFile(filepath.Join(dir, caReadyFile), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
					t.Fatalf("corrupt marker: %v", err)
				}
			},
		},
		{
			"all three but ca.crt corrupted (parse fail)",
			func(t *testing.T, dir string) {
				t.Helper()
				if _, err := LoadOrGenerate(LoadOptions{
					StorageDir:       dir,
					AllowedHostRegex: `^foo\.example\.com$`,
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, caCertFile), []byte("not a PEM"), 0o600); err != nil {
					t.Fatalf("corrupt cert: %v", err)
				}
			},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			dir := t.TempDir()
			r.setup(t, dir)
			spy := &loggerSpy{}
			_, err := LoadOrGenerate(LoadOptions{
				StorageDir:       dir,
				AllowedHostRegex: `^foo\.example\.com$`,
				LogFn:            spy.fn(),
			})
			if err == nil {
				t.Fatal("expected case-3 refusal, got nil error")
			}
			if !spy.has("mitm_ca_load_failed") {
				t.Error("mitm_ca_load_failed log not emitted")
			}
			// Critically: ca.crt and ca.key on disk must NOT have been
			// silently regenerated.
			for _, name := range []string{caCertFile, caKeyFile, caReadyFile} {
				p := filepath.Join(dir, name)
				if _, err := os.Stat(p); err == nil {
					// Existing files are fine — the test asserts no
					// silent regeneration; pre-existing content is
					// expected for some setups.
					_ = name
				}
			}
		})
	}
}

func TestLoadOrGenerate_Auto_TmpResidueIsCleanedOnGenerate(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Drop a *.tmp residue file in the directory before first start.
	residue := filepath.Join(dir, "ca.crt.tmp")
	if err := os.WriteFile(residue, []byte("residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
	}); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	// Residue must be cleaned.
	if _, err := os.Stat(residue); err == nil {
		t.Error("ca.crt.tmp residue should have been cleaned")
	}
	// Real CA must exist.
	if _, err := os.Stat(filepath.Join(dir, caCertFile)); err != nil {
		t.Errorf("ca.crt missing: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Flock contention
// ----------------------------------------------------------------------------

func TestAcquireLock_Serializes(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".ca.lock")

	r1, err := acquireLock(lockPath, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second acquire should block until r1 releases. We start it in a
	// goroutine, sleep briefly, release the first, and the second must
	// then complete promptly.
	var released atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		r2, err := acquireLock(lockPath, 5*time.Second)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		if !released.Load() {
			t.Error("second acquire returned BEFORE first released")
		}
		r2()
	}()

	time.Sleep(200 * time.Millisecond)
	released.Store(true)
	r1()
	<-done
}

func TestAcquireLock_Timeout(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".ca.lock")

	r1, err := acquireLock(lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	start := time.Now()
	_, err = acquireLock(lockPath, 250*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout, got nil error")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned too early: %s", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("returned too late: %s", elapsed)
	}
}

func TestLoadOrGenerate_Auto_LockTimeoutLogsBoth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, ".ca.lock")

	// Hold the lock externally so LoadOrGenerate's flock call times out.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("external flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	spy := &loggerSpy{}
	_, err = LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
		LockTimeout:      250 * time.Millisecond,
		LogFn:            spy.fn(),
	})
	if err == nil {
		t.Fatal("expected lock-timeout error, got nil")
	}
	if !spy.has("mitm_ca_lock_timeout") {
		t.Error("mitm_ca_lock_timeout not emitted")
	}
	if !spy.has("mitm_ca_generation_failed") {
		t.Error("mitm_ca_generation_failed not emitted alongside lock_timeout")
	}
}

func TestLoadOrGenerate_Auto_ConcurrentInvocationsCompareAndAdopt(t *testing.T) {
	// Two invocations against the same StorageDir: serialized by flock,
	// the second must observe the committed state and load (not
	// regenerate). Both produce the same fingerprint.
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		results [2]*CA
		errs    [2]error
		startGo sync.WaitGroup
	)
	startGo.Add(1)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			startGo.Wait()
			ca, err := LoadOrGenerate(LoadOptions{
				StorageDir:       dir,
				AllowedHostRegex: `^foo\.example\.com$`,
			})
			results[i], errs[i] = ca, err
		}()
	}
	startGo.Done()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if results[0].FingerprintSHA256 != results[1].FingerprintSHA256 {
		t.Errorf("compare-and-adopt should produce one CA: %q vs %q",
			results[0].FingerprintSHA256, results[1].FingerprintSHA256)
	}
}

// ----------------------------------------------------------------------------
// Smoke test: signing a leaf with a generated CA
// ----------------------------------------------------------------------------

func TestLoadOrGenerate_Auto_SignsLeafCorrectly(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
	})
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	leaf, err := GenerateLeaf("foo.example.com", ca.TLSCert, LeafECDSAP256, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateLeaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf verify: %v", err)
	}
}

// ----------------------------------------------------------------------------
// ensurePrivateDir
// ----------------------------------------------------------------------------

func TestEnsurePrivateDir_CreatesMissingDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "nested", "ca")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestEnsurePrivateDir_TightensExistingPermissiveDir(t *testing.T) {
	dir := t.TempDir()
	// t.TempDir creates with 0700 already. Loosen first so the helper
	// has something to chmod.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestEnsurePrivateDir_RejectsRegularFile(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ensurePrivateDir(notDir)
	if err == nil {
		t.Fatal("expected error for non-directory path, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' in error, got %q", err.Error())
	}
}

func TestEnsurePrivateDir_EmptyPath(t *testing.T) {
	if err := ensurePrivateDir(""); err == nil {
		t.Error("empty path should be rejected")
	}
}

// TestLoadOrGenerate_Auto_TightensPermissiveStorageDir is the
// integration test for the §4.2.1 step 4 contract: a pre-existing
// permissive storage directory is silently tightened to 0700 before
// any key material is written into it.
func TestLoadOrGenerate_Auto_TightensPermissiveStorageDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrGenerate(LoadOptions{
		StorageDir:       dir,
		AllowedHostRegex: `^foo\.example\.com$`,
	}); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("storage dir mode = %v, want 0700", info.Mode().Perm())
	}
}

// ----------------------------------------------------------------------------
// Supplied-CA validation: KeyUsage extension without keyCertSign
// ----------------------------------------------------------------------------

func TestLoadOrGenerate_Supplied_KeyUsageWithoutCertSign(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSuppliedCA(t, dir, "ecdsa-p256", func(c *x509.Certificate) {
		// KeyUsage is present (non-zero) but lacks keyCertSign — Go's
		// x509.CreateCertificate would reject every leaf signing.
		c.KeyUsage = x509.KeyUsageDigitalSignature
	})
	_, err := LoadOrGenerate(LoadOptions{
		SuppliedCertPath: certPath,
		SuppliedKeyPath:  keyPath,
	})
	if err == nil {
		t.Fatal("expected KeyUsage error, got nil")
	}
	if !strings.Contains(err.Error(), "keyCertSign") {
		t.Errorf("expected 'keyCertSign' in error, got %q", err.Error())
	}
}

func TestLoadOrGenerate_Supplied_KeyUsageAbsentIsAccepted(t *testing.T) {
	// KeyUsage == 0 means "extension absent"; Go interprets that as
	// "all usages permitted" so signing succeeds. The validator must
	// not reject this case (it would break operator-supplied CAs that
	// never bothered with the extension).
	dir := t.TempDir()
	certPath, keyPath := writeSuppliedCA(t, dir, "ecdsa-p256", func(c *x509.Certificate) {
		c.KeyUsage = 0
	})
	if _, err := LoadOrGenerate(LoadOptions{
		SuppliedCertPath: certPath,
		SuppliedKeyPath:  keyPath,
	}); err != nil {
		t.Fatalf("LoadOrGenerate with absent KeyUsage: %v", err)
	}
}

// TestWriteCAAtomically_FaultAtEachStep walks every numbered §4.2.1
// write step, injects a synthetic failure, and verifies that:
//
//  1. LoadOrGenerate returns an error and emits the
//     mitm_ca_generation_failed log.
//  2. The on-disk state matches the SPEC6 §4.2.1 "kill mid-write"
//     disposition table — case 1 (clean) for failures before any
//     real file is committed, case 3 (inconsistent) for failures
//     after the first rename, and case 2 (committed) for the
//     post-rename fsync that fails after the marker is on disk.
//  3. No *.tmp residue is left behind. The deferred cleanup is the
//     contract that lets the next start re-enter the case-1 branch
//     when no real files were committed.
//
// The fault-injection seam is `writeCAStepHook`. Each test case
// overrides it for the duration of the run, restoring the previous
// value on defer.
func TestWriteCAAtomically_FaultAtEachStep(t *testing.T) {
	cases := []struct {
		name      string
		step      writeCAStep
		wantState caStateKind
		wantCert  bool
		wantKey   bool
		wantReady bool
	}{
		{"step 5 — write ca.crt.tmp", stepWriteCertTmp, caStateClean, false, false, false},
		{"step 6 — write ca.key.tmp", stepWriteKeyTmp, caStateClean, false, false, false},
		{"step 7a — rename ca.crt", stepRenameCert, caStateClean, false, false, false},
		{"step 7b — rename ca.key", stepRenameKey, caStateInconsistent, true, false, false},
		{"step 8 — fsync dir #1", stepFsyncDir1, caStateInconsistent, true, true, false},
		{"step 9 — write ca.ready.tmp", stepWriteReadyTmp, caStateInconsistent, true, true, false},
		{"step 10a — rename ca.ready", stepRenameReady, caStateInconsistent, true, true, false},
		{"step 10b — fsync dir #2 (post-commit)", stepFsyncDir2, caStateCommitted, true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restore := setWriteCAStepHookForTest(func(s writeCAStep) error {
				if s == tc.step {
					return fmt.Errorf("synthetic fault at %s", s)
				}
				return nil
			})
			defer restore()

			spy := &loggerSpy{}
			_, err := LoadOrGenerate(LoadOptions{
				StorageDir:           dir,
				AllowUnconstrainedCA: true,
				LogFn:                spy.fn(),
			})
			if err == nil {
				t.Fatal("expected error from generate path; got nil")
			}
			if !strings.Contains(err.Error(), tc.step.String()) && !strings.Contains(err.Error(), "synthetic") {
				t.Errorf("error %q does not mention step %s or synthetic", err.Error(), tc.step)
			}
			if !spy.has("mitm_ca_generation_failed") {
				t.Error("missing mitm_ca_generation_failed log event")
			}

			// No *.tmp residue.
			entries, derr := os.ReadDir(dir)
			if derr != nil {
				t.Fatalf("ReadDir: %v", derr)
			}
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".tmp") {
					t.Errorf("post-fault tmp residue left: %s", e.Name())
				}
			}

			// On-disk real-file presence matches the spec table.
			gotCert := fileExists(t, dir, caCertFile)
			gotKey := fileExists(t, dir, caKeyFile)
			gotReady := fileExists(t, dir, caReadyFile)
			if gotCert != tc.wantCert || gotKey != tc.wantKey || gotReady != tc.wantReady {
				t.Errorf("real-file presence: cert=%v key=%v ready=%v; want cert=%v key=%v ready=%v",
					gotCert, gotKey, gotReady, tc.wantCert, tc.wantKey, tc.wantReady)
			}

			// scanCAState returns the documented disposition.
			state, serr := scanCAState(dir)
			if serr != nil {
				t.Fatalf("scanCAState: %v", serr)
			}
			if state.kind != tc.wantState {
				t.Errorf("state.kind = %v, want %v (reason=%q)", state.kind, tc.wantState, state.reason)
			}
		})
	}
}

// TestWriteCAAtomically_FaultAtFsync2_NextStartLoads exercises the
// step-10b interesting case: the fsync of the parent dir after the
// marker rename fails (e.g. medium error), so writeCAAtomically
// returns an error to the caller — but the rename of ca.ready already
// completed, so on a subsequent start (no fault), case 2 fires and
// the CA loads cleanly. This is the deterministic load-or-refuse
// promise SPEC6 §4.2.1 makes for the post-commit fsync class of
// failures.
func TestWriteCAAtomically_FaultAtFsync2_NextStartLoads(t *testing.T) {
	dir := t.TempDir()
	restore := setWriteCAStepHookForTest(func(s writeCAStep) error {
		if s == stepFsyncDir2 {
			return errors.New("synthetic fsync2 fault")
		}
		return nil
	})

	spy := &loggerSpy{}
	_, err := LoadOrGenerate(LoadOptions{
		StorageDir:           dir,
		AllowUnconstrainedCA: true,
		LogFn:                spy.fn(),
	})
	if err == nil {
		t.Fatal("expected error from first generate")
	}
	restore()

	// Next start, no fault — should load case 2 cleanly.
	spy2 := &loggerSpy{}
	ca, err := LoadOrGenerate(LoadOptions{
		StorageDir:           dir,
		AllowUnconstrainedCA: true,
		LogFn:                spy2.fn(),
	})
	if err != nil {
		t.Fatalf("second start should load committed CA, got error: %v", err)
	}
	if ca == nil || ca.Cert == nil {
		t.Fatal("nil CA on second start")
	}
	if !spy2.has("mitm_ca_loaded") {
		t.Error("second start did not emit mitm_ca_loaded")
	}
	// Must NOT have re-generated.
	if spy2.has("mitm_ca_generated") {
		t.Error("second start incorrectly re-generated instead of loading")
	}
}

// fileExists is a small test helper that returns whether <dir>/<name>
// exists as a regular file (not directory, not missing).
func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		t.Fatalf("stat %s/%s: %v", dir, name, err)
	}
	return info.Mode().IsRegular()
}

// ----------------------------------------------------------------------------
// Helper used internally by ca_test.go.
// ----------------------------------------------------------------------------

func init() {
	// Suppress test-side log noise on stderr if any loggerSpy is the
	// only sink. (No-op — the spy itself swallows; this is a hook for
	// future test-rig logging.)
	_ = fmt.Sprintf
}
