package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
)

const testDNSName = "cache.example.net"

// freeAddr reserves and releases a loopback port, returning its address.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// logCapture records structured log events.
type logCapture struct {
	mu     sync.Mutex
	events []string
}

func (l *logCapture) fn() func(string, string, map[string]any) {
	return func(_level, event string, _fields map[string]any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.events = append(l.events, event)
	}
}

func (l *logCapture) has(event string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e == event {
			return true
		}
	}
	return false
}

// fakeClock is an injectable clock for the renewal-window tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testManager wires a Manager to a fake ACME server whose challenge
// validation fetches the challenge server directly (the simulated public
// port 80 maps 1:1 to acme.listen).
func testManager(t *testing.T, srv *fakeacme.Server, mutate func(*Config)) *Manager {
	t.Helper()
	challengeAddr := freeAddr(t)
	srv.SetChallengeBaseURL("http://" + challengeAddr)
	cfg := Config{
		DirectoryURL:  srv.DirectoryURL(),
		DNSName:       testDNSName,
		Listen:        challengeAddr,
		StorageDir:    t.TempDir(),
		RenewBefore:   24 * time.Hour,
		CheckInterval: 20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// deadTransport fails every request: an unreachable ACME authority.
type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("authority unreachable")
}

func leafFingerprint(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:])
}

// TestManager_EnsureCertificate_IssuesAndPersists runs the full issuance
// against the fake ACME server and pins the storage layout, permissions,
// chain validity and observability snapshot.
func TestManager_EnsureCertificate_IssuesAndPersists(t *testing.T) {
	srv := fakeacme.New(t)
	logs := &logCapture{}
	mgr := testManager(t, srv, func(c *Config) { c.LogFn = logs.fn() })

	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1", got)
	}
	if got := srv.AccountCount(); got != 1 {
		t.Errorf("AccountCount = %d, want 1", got)
	}

	cert, err := mgr.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(cert.Certificate) != 2 {
		t.Errorf("chain length = %d, want 2 (leaf + CA)", len(cert.Certificate))
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if err := leaf.VerifyHostname(testDNSName); err != nil {
		t.Errorf("leaf does not cover %s: %v", testDNSName, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(srv.CACertPEM())
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: testDNSName, Roots: pool}); err != nil {
		t.Errorf("leaf does not verify against the fake CA: %v", err)
	}

	// Storage layout + permissions.
	dir := mgr.cfg.StorageDir
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("storage dir perm = %v (err %v), want 0700", info, err)
	}
	for name, want := range map[string]os.FileMode{
		"account.key":  0o600,
		"privkey.pem":  0o600,
		"cert.pem":     0o644,
		"account.json": 0o644,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s perm = %o, want %o", name, info.Mode().Perm(), want)
		}
	}

	// Observability snapshot: no secrets, the documented fields.
	snap := mgr.Snapshot()
	if !snap.HasCert || snap.CertFingerprintSHA256 == "" {
		t.Errorf("snapshot missing cert: %+v", snap)
	}
	if snap.DNSName != testDNSName || snap.DirectoryURL != srv.DirectoryURL() {
		t.Errorf("snapshot identity = %q/%q", snap.DNSName, snap.DirectoryURL)
	}
	if snap.CertNotAfter.Before(time.Now().Add(89 * 24 * time.Hour)) {
		t.Errorf("not_after = %s, want ~90 days out", snap.CertNotAfter)
	}
	if snap.LastError != "" {
		t.Errorf("LastError = %q, want empty", snap.LastError)
	}
	if !logs.has("acme_account_created") || !logs.has("acme_cert_issued") {
		t.Errorf("missing log events, got %v", logs.events)
	}
}

// TestManager_EnsureCertificate_LoadsStoredWithoutNetwork pins that a
// stored certificate is served without contacting the authority: the
// second manager points at a dead directory URL and still succeeds.
func TestManager_EnsureCertificate_LoadsStoredWithoutNetwork(t *testing.T) {
	srv := fakeacme.New(t)
	challengeAddr := freeAddr(t)
	storageDir := t.TempDir()

	mgr1 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	if err := mgr1.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("first EnsureCertificate: %v", err)
	}
	wantFP := leafFingerprint(t, mustGetCert(t, mgr1))

	// "Restart": same storage and directory, authority unreachable.
	mgr2, err := NewManager(Config{
		DirectoryURL:  srv.DirectoryURL(),
		DNSName:       testDNSName,
		Listen:        challengeAddr,
		StorageDir:    storageDir,
		RenewBefore:   24 * time.Hour,
		CheckInterval: time.Hour,
		HTTPClient:    &http.Client{Transport: deadTransport{}},
	})
	if err != nil {
		t.Fatalf("NewManager (restart): %v", err)
	}
	if err := mgr2.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("second EnsureCertificate (stored cert, dead CA): %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1 (no re-issuance)", got)
	}
	if got := leafFingerprint(t, mustGetCert(t, mgr2)); got != wantFP {
		t.Errorf("fingerprint changed across restart: %s != %s", got, wantFP)
	}
}

// TestManager_EnsureCertificate_ExpiredStoredCert_Renews pins the boot
// rule: an expired stored certificate is not usable, so startup renews
// before serving.
func TestManager_EnsureCertificate_ExpiredStoredCert_Renews(t *testing.T) {
	srv := fakeacme.New(t)
	storageDir := t.TempDir()
	expiredFP := writeExpiredCert(t, storageDir, testDNSName)

	mgr, err := NewManager(Config{
		DirectoryURL:  srv.DirectoryURL(),
		DNSName:       testDNSName,
		Listen:        freeAddr(t),
		StorageDir:    storageDir,
		RenewBefore:   24 * time.Hour,
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv.SetChallengeBaseURL("http://" + mgr.cfg.Listen)
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1 (renewal of expired cert)", got)
	}
	gotFP := leafFingerprint(t, mustGetCert(t, mgr))
	if gotFP == expiredFP {
		t.Error("certificate was not renewed: fingerprint unchanged")
	}
	leaf, _ := x509.ParseCertificate(mustGetCert(t, mgr).Certificate[0])
	if !time.Now().Before(leaf.NotAfter) {
		t.Errorf("renewed cert is already expired: %s", leaf.NotAfter)
	}
}

// TestManager_EnsureCertificate_ChallengeFailure_NoCert pins the
// explicit failure: when the authority rejects the challenge, no
// certificate is stored or served and the error is surfaced.
func TestManager_EnsureCertificate_ChallengeFailure_NoCert(t *testing.T) {
	srv := fakeacme.New(t)
	srv.SetFailChallenges(true)
	mgr := testManager(t, srv, nil)

	err := mgr.EnsureCertificate(context.Background())
	if err == nil {
		t.Fatal("EnsureCertificate succeeded, want challenge failure")
	}
	if !strings.Contains(err.Error(), "challenge validation") {
		t.Errorf("error %q does not mention challenge validation", err)
	}
	if _, err := mgr.GetCertificate(nil); err == nil {
		t.Error("GetCertificate succeeded with no certificate, want error")
	}
	if _, err := os.Stat(filepath.Join(mgr.cfg.StorageDir, "cert.pem")); !os.IsNotExist(err) {
		t.Errorf("cert.pem written despite failure (err %v)", err)
	}
	if snap := mgr.Snapshot(); snap.LastError == "" {
		t.Error("Snapshot.LastError empty, want the failure")
	}
	// The account is persisted even though issuance failed.
	if _, err := os.Stat(filepath.Join(mgr.cfg.StorageDir, "account.key")); err != nil {
		t.Errorf("account.key missing after failed issuance: %v", err)
	}
}

// TestManager_EnsureCertificate_ChallengeBindConflict pins the explicit
// failure when acme.listen is already occupied.
func TestManager_EnsureCertificate_ChallengeBindConflict(t *testing.T) {
	srv := fakeacme.New(t)
	occupied := freeAddr(t)
	blocker, err := net.Listen("tcp", occupied)
	if err != nil {
		t.Fatalf("occupy %s: %v", occupied, err)
	}
	defer func() { _ = blocker.Close() }()

	mgr, err := NewManager(Config{
		DirectoryURL:  srv.DirectoryURL(),
		DNSName:       testDNSName,
		Listen:        occupied,
		StorageDir:    t.TempDir(),
		RenewBefore:   24 * time.Hour,
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	err = mgr.EnsureCertificate(context.Background())
	if err == nil {
		t.Fatal("EnsureCertificate succeeded, want challenge bind conflict")
	}
	if !strings.Contains(err.Error(), "challenge listen") {
		t.Errorf("error %q does not mention the challenge listen failure", err)
	}
	if got := srv.IssueCount(); got != 0 {
		t.Errorf("IssueCount = %d, want 0 (no finalize reached)", got)
	}
}

// TestChallengeServer_ServesOnlyChallengePath pins the challenge
// server's surface: only the registered token path answers, everything
// else 404s, other methods 405.
func TestChallengeServer_ServesOnlyChallengePath(t *testing.T) {
	srv := newChallengeServer()
	addr := freeAddr(t)
	if err := srv.start(addr); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.stop()
	srv.set("tok123", "key-auth-value")
	base := "http://" + addr

	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get("/.well-known/acme-challenge/tok123"); code != 200 || body != "key-auth-value" {
		t.Errorf("token path = %d %q, want 200 key-auth-value", code, body)
	}
	if code, _ := get("/.well-known/acme-challenge/unknown"); code != 404 {
		t.Errorf("unknown token = %d, want 404", code)
	}
	if code, _ := get("/"); code != 404 {
		t.Errorf("root = %d, want 404", code)
	}
	if code, _ := get("/admin/healthz"); code != 404 {
		t.Errorf("admin path = %d, want 404 (no app interface exposed)", code)
	}
	resp, err := http.Post(base+"/.well-known/acme-challenge/tok123", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", resp.StatusCode)
	}
}

// TestManager_Run_RenewsAtThreshold_HotReload advances a fake clock into
// the renewal window and pins that the loop renews and that new
// handshakes (GetCertificate) receive the renewed certificate without a
// restart.
func TestManager_Run_RenewsAtThreshold_HotReload(t *testing.T) {
	srv := fakeacme.New(t)
	clock := &fakeClock{t: time.Now()}
	mgr := testManager(t, srv, func(c *Config) {
		c.Now = clock.Now
		c.RenewBefore = 89 * 24 * time.Hour // 90-day cert: not due at issue
		c.CheckInterval = 10 * time.Millisecond
		c.Backoff = func(int) time.Duration { return time.Millisecond }
	})
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	before := leafFingerprint(t, mustGetCert(t, mgr))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// Not due yet: no renewal within a few ticks.
	time.Sleep(50 * time.Millisecond)
	if got := srv.IssueCount(); got != 1 {
		t.Fatalf("IssueCount = %d before threshold, want 1", got)
	}
	// Advance into the renewal window (88 days left < 89-day threshold).
	clock.Advance(2 * 24 * time.Hour)
	// Wait for the hot reload itself, not the server-side finalize count:
	// the certificate swap happens after the chain download returns.
	waitFor(t, "hot reload", 5*time.Second, func() bool {
		return leafFingerprint(t, mustGetCert(t, mgr)) != before
	})
	snap := mgr.Snapshot()
	if snap.LastRenewalAt.IsZero() {
		t.Error("Snapshot.LastRenewalAt zero after renewal")
	}
	if snap.LastError != "" {
		t.Errorf("LastError = %q after successful renewal, want empty", snap.LastError)
	}
}

// TestManager_Run_BackoffOnFailure_KeepsServingOldCert pins the retry
// schedule (1m, doubling, capped at 1h — overridden here to stay fast,
// with the failure counter sequence asserted) and that a valid stored
// certificate keeps being served while renewals fail.
func TestManager_Run_BackoffOnFailure_KeepsServingOldCert(t *testing.T) {
	srv := fakeacme.New(t)
	clock := &fakeClock{t: time.Now()}
	var mu sync.Mutex
	var failures []int
	mgr := testManager(t, srv, func(c *Config) {
		c.Now = clock.Now
		c.RenewBefore = 89 * 24 * time.Hour
		c.CheckInterval = 10 * time.Millisecond
		c.Backoff = func(n int) time.Duration {
			mu.Lock()
			failures = append(failures, n)
			mu.Unlock()
			return time.Millisecond
		}
	})
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	goodFP := leafFingerprint(t, mustGetCert(t, mgr))

	// Enter the renewal window and break the authority.
	clock.Advance(2 * 24 * time.Hour)
	srv.SetFailChallenges(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	waitFor(t, "three failed retries", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(failures) >= 3
	})
	mu.Lock()
	got := append([]int(nil), failures...)
	mu.Unlock()
	if len(got) < 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("failure sequence = %v, want prefix [1 2 3]", got)
	}

	// The still-valid stored certificate keeps being served.
	if fp := leafFingerprint(t, mustGetCert(t, mgr)); fp != goodFP {
		t.Error("served certificate changed during the outage, want the stored one")
	}
	if snap := mgr.Snapshot(); snap.LastError == "" {
		t.Error("Snapshot.LastError empty during outage, want the failure")
	}

	// The authority recovers: the next retry succeeds and resets the
	// backoff + last error. Wait for the new certificate (setCert clears
	// lastErr), not the server-side finalize count.
	srv.SetFailChallenges(false)
	waitFor(t, "recovery renewal", 5*time.Second, func() bool {
		return leafFingerprint(t, mustGetCert(t, mgr)) != goodFP
	})
	if snap := mgr.Snapshot(); snap.LastError != "" {
		t.Errorf("LastError = %q after recovery, want empty", snap.LastError)
	}
}

// TestBackoffDelay_SpecSchedule pins the spec's retry schedule: 1
// minute after the first failure, doubling per failure, capped at 1 hour
// between two retries.
func TestBackoffDelay_SpecSchedule(t *testing.T) {
	m := &Manager{}
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour},  // 64m capped
		{20, time.Hour}, // stays capped
	}
	for _, tc := range cases {
		if got := m.backoffDelay(tc.failures); got != tc.want {
			t.Errorf("backoffDelay(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// TestManager_AccountReusedAcrossRestart pins that the ACME account
// survives a restart: with the certificate files removed but the account
// kept, a new issuance reuses the account instead of registering a new
// one.
func TestManager_AccountReusedAcrossRestart(t *testing.T) {
	srv := fakeacme.New(t)
	storageDir := t.TempDir()
	mgr1 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	if err := mgr1.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("first EnsureCertificate: %v", err)
	}
	// Simulate a restart that lost the certificate but kept the account.
	for _, name := range []string{"cert.pem", "privkey.pem"} {
		if err := os.Remove(filepath.Join(storageDir, name)); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
	mgr2 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	if err := mgr2.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("second EnsureCertificate: %v", err)
	}
	if got := srv.IssueCount(); got != 2 {
		t.Errorf("IssueCount = %d, want 2 (re-issuance)", got)
	}
	if got := srv.AccountCount(); got != 1 {
		t.Errorf("AccountCount = %d, want 1 (account reused, not re-registered)", got)
	}
}

// TestManager_ChallengeBehindReverseProxy pins the spec's deployment
// shape: a reverse proxy publishing the local challenge server on the
// simulated public port 80 — the authority validates through the proxy.
func TestManager_ChallengeBehindReverseProxy(t *testing.T) {
	srv := fakeacme.New(t)
	challengeAddr := freeAddr(t)

	// The "public port 80" reverse proxy forwards to the local challenge
	// server address.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get("http://" + challengeAddr + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	srv.SetChallengeBaseURL(proxy.URL)

	mgr, err := NewManager(Config{
		DirectoryURL:  srv.DirectoryURL(),
		DNSName:       testDNSName,
		Listen:        challengeAddr,
		StorageDir:    t.TempDir(),
		RenewBefore:   24 * time.Hour,
		CheckInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate behind reverse proxy: %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1", got)
	}
	cert, err := mgr.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if err := leaf.VerifyHostname(testDNSName); err != nil {
		t.Errorf("leaf does not cover %s: %v", testDNSName, err)
	}
}

func mustGetCert(t *testing.T, m *Manager) *tls.Certificate {
	t.Helper()
	cert, err := m.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	return cert
}

// writeExpiredCert stores a self-signed certificate that is already
// expired, simulating a stale on-disk certificate at boot.
func writeExpiredCert(t *testing.T, dir, dnsName string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o644); err != nil {
		t.Fatalf("write cert.pem: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), keyPEM, 0o600); err != nil {
		t.Fatalf("write privkey.pem: %v", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:])
}

// TestManager_Run_ChecksImmediatelyOnStart pins that Run checks the
// renewal window as soon as it starts instead of after a first
// CheckInterval: a daemon restarted more often than check_interval
// must still renew.
func TestManager_Run_ChecksImmediatelyOnStart(t *testing.T) {
	srv := fakeacme.New(t)
	clock := &fakeClock{t: time.Now()}
	mgr := testManager(t, srv, func(c *Config) {
		c.Now = clock.Now
		c.RenewBefore = 89 * 24 * time.Hour
		c.CheckInterval = time.Hour // never fires within the test
	})
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	before := leafFingerprint(t, mustGetCert(t, mgr))
	// Inside the renewal window but still usable: EnsureCertificate on
	// its own does not renew.
	clock.Advance(2 * 24 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	waitFor(t, "renewal on start", 5*time.Second, func() bool {
		return leafFingerprint(t, mustGetCert(t, mgr)) != before
	})
}

// TestManager_DeactivatedAccount_RegistersWithFreshKey pins RFC 8555
// §7.3.6 handling: the authority rejects every request signed by a
// deactivated account's key, so the next issuance registers a new
// account with a new key and persists it.
func TestManager_DeactivatedAccount_RegistersWithFreshKey(t *testing.T) {
	srv := fakeacme.New(t)
	storageDir := t.TempDir()
	mgr1 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	if err := mgr1.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("first EnsureCertificate: %v", err)
	}
	oldKey, err := os.ReadFile(filepath.Join(storageDir, "account.key"))
	if err != nil {
		t.Fatalf("read account.key: %v", err)
	}
	srv.DeactivateAccounts()
	for _, name := range []string{"cert.pem", "privkey.pem"} {
		if err := os.Remove(filepath.Join(storageDir, name)); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}

	logs := &logCapture{}
	mgr2 := testManager(t, srv, func(c *Config) {
		c.StorageDir = storageDir
		c.LogFn = logs.fn()
	})
	if err := mgr2.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate after deactivation: %v", err)
	}
	if got := srv.AccountCount(); got != 2 {
		t.Errorf("AccountCount = %d, want 2 (new account registered)", got)
	}
	newKey, err := os.ReadFile(filepath.Join(storageDir, "account.key"))
	if err != nil {
		t.Fatalf("read account.key: %v", err)
	}
	if string(newKey) == string(oldKey) {
		t.Error("account.key unchanged, want a fresh key replacing the deactivated one")
	}
	if !logs.has("acme_account_unusable") {
		t.Error("acme_account_unusable not logged")
	}
}

// TestManager_MismatchedStoredPair_Reissues pins that a chain/key pair
// that does not match (crash between the two renames in saveCert) is
// treated as "no certificate" and re-issued, instead of making the
// daemon refuse to start.
func TestManager_MismatchedStoredPair_Reissues(t *testing.T) {
	srv := fakeacme.New(t)
	storageDir := t.TempDir()
	mgr1 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	if err := mgr1.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("first EnsureCertificate: %v", err)
	}
	// A new key next to the old chain.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyPEM, err := encodePrivateKeyPEM(otherKey)
	if err != nil {
		t.Fatalf("encode key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "privkey.pem"), keyPEM, 0o600); err != nil {
		t.Fatalf("write privkey.pem: %v", err)
	}

	logs := &logCapture{}
	mgr2 := testManager(t, srv, func(c *Config) {
		c.StorageDir = storageDir
		c.LogFn = logs.fn()
	})
	if err := mgr2.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate over mismatched pair: %v", err)
	}
	if got := srv.IssueCount(); got != 2 {
		t.Errorf("IssueCount = %d, want 2 (re-issued)", got)
	}
	if !logs.has("acme_stored_cert_invalid") {
		t.Error("acme_stored_cert_invalid not logged")
	}
	if _, _, ok, err := loadCert(storageDir); err != nil || !ok {
		t.Errorf("stored pair after re-issuance: ok=%v err=%v, want a valid pair", ok, err)
	}
}

// TestManager_DirectoryChange_Reissues pins that a certificate and an
// account from another directory (Let's Encrypt staging before a switch
// to production) are not reused: the configured directory registers an
// account and issues a certificate, which replaces the stored one.
func TestManager_DirectoryChange_Reissues(t *testing.T) {
	staging := fakeacme.New(t)
	production := fakeacme.New(t)
	storageDir := t.TempDir()
	mgrStaging := testManager(t, staging, func(c *Config) { c.StorageDir = storageDir })
	if err := mgrStaging.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("staging EnsureCertificate: %v", err)
	}
	stagingFP := leafFingerprint(t, mustGetCert(t, mgrStaging))

	logs := &logCapture{}
	mgrProd := testManager(t, production, func(c *Config) {
		c.StorageDir = storageDir
		c.LogFn = logs.fn()
	})
	if err := mgrProd.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("production EnsureCertificate: %v", err)
	}
	if got := production.IssueCount(); got != 1 {
		t.Errorf("production IssueCount = %d, want 1", got)
	}
	if got := production.AccountCount(); got != 1 {
		t.Errorf("production AccountCount = %d, want 1", got)
	}
	if leafFingerprint(t, mustGetCert(t, mgrProd)) == stagingFP {
		t.Error("staging certificate still served after the directory change")
	}
	if !logs.has("acme_stored_cert_directory_mismatch") {
		t.Error("acme_stored_cert_directory_mismatch not logged")
	}
	_, meta, ok, err := loadAccount(storageDir)
	if err != nil || !ok {
		t.Fatalf("loadAccount: ok=%v err=%v", ok, err)
	}
	if meta.DirectoryURL != production.DirectoryURL() {
		t.Errorf("account.json directory_url = %q, want %q", meta.DirectoryURL, production.DirectoryURL())
	}
}

// TestManager_PeerIssuedWhileWaiting_NoDuplicate pins that an instance
// that did not have the certificate when it started, but finds one
// issued by a peer sharing the storage dir once it holds the lock,
// serves that certificate instead of issuing a duplicate.
func TestManager_PeerIssuedWhileWaiting_NoDuplicate(t *testing.T) {
	srv := fakeacme.New(t)
	storageDir := t.TempDir()
	// Both managers start before any certificate exists.
	mgr1 := testManager(t, srv, func(c *Config) { c.StorageDir = storageDir })
	mgr2 := testManager(t, srv, func(c *Config) {
		c.StorageDir = storageDir
		c.Listen = mgr1.cfg.Listen
	})
	// testManager pointed the authority at mgr2's own challenge address:
	// both now share mgr1's.
	srv.SetChallengeBaseURL("http://" + mgr1.cfg.Listen)
	if err := mgr1.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("mgr1 EnsureCertificate: %v", err)
	}
	if err := mgr2.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("mgr2 EnsureCertificate: %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1 (peer's certificate reused)", got)
	}
	if leafFingerprint(t, mustGetCert(t, mgr1)) != leafFingerprint(t, mustGetCert(t, mgr2)) {
		t.Error("managers serve different certificates, want the shared one")
	}
}

// TestManager_RenewBeforeCoversLifetime_NoReissueLoop pins that a
// renew_before window covering the whole certificate lifetime (7 days
// with 6-day certificates) is clamped, instead of making every check
// re-issue.
func TestManager_RenewBeforeCoversLifetime_NoReissueLoop(t *testing.T) {
	srv := fakeacme.New(t, fakeacme.WithCertLifetime(6*24*time.Hour))
	logs := &logCapture{}
	mgr := testManager(t, srv, func(c *Config) {
		c.RenewBefore = 7 * 24 * time.Hour
		c.CheckInterval = 5 * time.Millisecond
		c.LogFn = logs.fn()
	})
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1 (no re-issue loop)", got)
	}
	if !logs.has("acme_renew_before_clamped") {
		t.Error("acme_renew_before_clamped not logged")
	}
}

// TestManager_OrderReadyAsynchronously pins RFC 8555 §7.4: an authority
// may keep the order "pending" for a while after the last authorization
// is validated, and finalizing before it turns "ready" fails with
// orderNotReady — issuance must wait for the order instead.
func TestManager_OrderReadyAsynchronously(t *testing.T) {
	srv := fakeacme.New(t, fakeacme.WithOrderReadyAfterPolls(1))
	mgr := testManager(t, srv, nil)
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("IssueCount = %d, want 1", got)
	}
}

// TestAcquireLock_HonoursContext pins that waiting for the interprocess
// lock stops as soon as ctx is cancelled, instead of sleeping until
// LockTimeout and delaying shutdown.
func TestAcquireLock_HonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockFile)
	release, err := acquireLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	defer release()

	// A second descriptor contends with the first (flock is per open
	// file description).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = acquireLock(ctx, path, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireLock = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("acquireLock returned after %s, want shortly after ctx expiry", elapsed)
	}
}
