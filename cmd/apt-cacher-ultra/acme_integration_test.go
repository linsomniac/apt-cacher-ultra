package main

// Integration tests for the ACME certificate wiring
// (docs/acme-certificates-spec.md), driven through the production
// serve() path against a fake RFC 8555 authority
// (internal/testutil/fakeacme):
//
//   - TestServe_ACME_TwoSurfacesSharedCert: one certificate obtained
//     at boot is served by BOTH the proxy HTTPS listener and the admin
//     listener, persisted under <cache.dir>/acme with restrictive
//     permissions, and exposed on the status page's acme section.
//     MITM is enabled alongside to pin the independence of the two
//     TLS subsystems.
//   - TestServe_ACME_RestartReusesStoredCert: a restart on the same
//     cache dir serves the stored certificate without contacting the
//     authority again.
//   - TestServe_ACME_IssuanceFailure_FailsBoot: a failed first
//     issuance makes serve() return an error before any listener is
//     bound — no surface is served.
//   - TestServe_ACME_CacheOnly_AdminPlainHTTP: cache.use_acme alone
//     selects ACME for the proxy listener; admin stays plain HTTP.
//   - TestServe_ManualTLSProxy_NoACME: non-regression — a manual PEM
//     pair on cache.listen_tls keeps working with ACME disabled.
//
// None of these mutate package-level tunables, but they register
// gauges into the global metrics.Default registry, so they are NOT
// t.Parallel.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
	testutil "github.com/linsomniac/apt-cacher-ultra/internal/testutil/tlscert"
)

const acmeTestDNSName = "cache.example.net"

// acmeFreeAddr reserves and releases a loopback port, returning its
// address. serve() binds its listeners itself, so the test learns the
// ports up front this way (same trade-off as freeAddr in internal/acme).
func acmeFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("acmeFreeAddr: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// acmeTestAdminConfig returns the presence-sensitive admin defaults
// that Load() applies via TOML's md.IsDefined but Defaults() does not
// (same rationale as the other integration tests).
func acmeTestAdminConfig(listen string, useAcme bool) config.AdminConfig {
	return config.AdminConfig{
		Enabled:         true,
		Listen:          listen,
		UseAcme:         useAcme,
		GaugeRefresh:    config.Duration{Duration: 50 * time.Millisecond},
		ReadTimeout:     config.Duration{Duration: 5 * time.Second},
		IdleTimeout:     config.Duration{Duration: 30 * time.Second},
		MetricSeriesCap: 1024,
	}
}

// acmeTestConfig builds a daemon config with ACME enabled against the
// fake authority, both surfaces selecting ACME. The fake authority's
// simulated public port 80 maps 1:1 to acme.listen (no reverse proxy).
func acmeTestConfig(t *testing.T, cacheDir string, srv *fakeacme.Server) *config.Config {
	t.Helper()
	challengeAddr := acmeFreeAddr(t)
	srv.SetChallengeBaseURL("http://" + challengeAddr)
	cfg := minimalCfg(cacheDir, nil)
	cfg.Cache.Listen = acmeFreeAddr(t)
	cfg.Cache.ListenTLS = acmeFreeAddr(t)
	cfg.Cache.UseAcme = true
	cfg.Admin = acmeTestAdminConfig(acmeFreeAddr(t), true)
	cfg.Acme = config.AcmeConfig{
		Enabled:         true,
		DirectoryURL:    srv.DirectoryURL(),
		DNSName:         acmeTestDNSName,
		Listen:          challengeAddr,
		RenewBeforeDays: 7,
		CheckInterval:   config.Duration{Duration: 12 * time.Hour},
	}
	return cfg
}

// acmeHandshake dials addr over TLS (skipping verification — the fake
// CA is not in the system roots) and returns the presented leaf.
func acmeHandshake(t *testing.T, addr string) *x509.Certificate {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatalf("no peer certificates presented by %s", addr)
	}
	return certs[0]
}

func acmeFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// acmeStoredFingerprint reads the persisted certificate chain and
// returns the leaf fingerprint — the value the status page must
// advertise and both ACME surfaces must present.
func acmeStoredFingerprint(t *testing.T, cacheDir string) string {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(cacheDir, "acme", "cert.pem"))
	if err != nil {
		t.Fatalf("read stored cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("no PEM block in stored cert.pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse stored cert: %v", err)
	}
	return acmeFingerprint(cert)
}

func acmeCheckPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s perm = %o, want %o", path, got, want)
	}
}

// acmeStatusPayload is the subset of the status-page JSON contract
// these tests assert on.
type acmeStatusPayload struct {
	ACME struct {
		Enabled               bool     `json:"enabled"`
		DNSName               string   `json:"dns_name"`
		DirectoryURL          string   `json:"directory_url"`
		Surfaces              []string `json:"surfaces"`
		CertFingerprintSHA256 string   `json:"cert_fingerprint_sha256"`
		CertNotAfterUnixTime  *int64   `json:"cert_not_after_unixtime"`
		LastError             string   `json:"last_error"`
	} `json:"acme"`
	TLSMITM struct {
		Enabled bool `json:"enabled"`
	} `json:"tls_mitm"`
	Listeners []struct {
		Role string `json:"role"`
		Addr string `json:"addr"`
	} `json:"listeners"`
}

func acmeFetchStatus(t *testing.T, adminAddr string, useHTTPS bool) acmeStatusPayload {
	t.Helper()
	scheme := "http"
	if useHTTPS {
		scheme = "https"
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(scheme + "://" + adminAddr + "/?format=json")
	if err != nil {
		t.Fatalf("GET %s admin status: %v", scheme, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read status body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status JSON status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	var payload acmeStatusPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode status JSON: %v\nbody:\n%s", err, body)
	}
	return payload
}

// acmeStartServe runs serve() in a goroutine and returns the
// completion channel.
func acmeStartServe(ctx context.Context, cfg *config.Config) chan error {
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, newTestLogger())
	}()
	return done
}

// acmeStopServe cancels the serve ctx and requires serve() to return
// cleanly within the drain budget.
func acmeStopServe(t *testing.T, cancel context.CancelFunc, done chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Errorf("serve did not return on shutdown")
	}
}

// TestServe_ACME_TwoSurfacesSharedCert is the main end-to-end ACME
// test: full daemon boot through serve() against the fake authority,
// both surfaces presenting the one issued certificate, the status
// page's acme section, the on-disk layout and permissions, and a
// restart serving the stored certificate without re-issuance.
func TestServe_ACME_TwoSurfacesSharedCert(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	srv := fakeacme.New(t)
	cacheDir := t.TempDir()
	cfg := acmeTestConfig(t, cacheDir, srv)
	// MITM independence: the local MITM CA and the ACME certificate
	// are separate subsystems; enabling MITM must not change what the
	// ACME surfaces present (asserted via issuer + fingerprint below).
	// Same MITM test settings as cert_issued_test.go.
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowUnconstrainedCA = true
	cfg.TlsMitm.CertCacheSize = 32
	cfg.TlsMitm.LeafCertLifetime.Duration = 6 * time.Hour
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"

	ctx, cancel := context.WithCancel(context.Background())
	done := acmeStartServe(ctx, cfg)

	if err := waitForDaemonReady(t, cfg.Cache.Listen, 15*time.Second); err != nil {
		cancel()
		<-done
		t.Fatalf("daemon never became ready: %v", err)
	}

	// The plain proxy listener serves HTTP (readiness probe already
	// proved it; here a real GET for the record).
	plainClient := &http.Client{Timeout: 5 * time.Second}
	plainResp, err := plainClient.Get("http://" + cfg.Cache.Listen + "/?format=json")
	if err != nil {
		t.Fatalf("GET plain proxy: %v", err)
	}
	_ = plainResp.Body.Close()
	if plainResp.StatusCode < 200 || plainResp.StatusCode >= 600 {
		t.Errorf("plain proxy GET status = %d, want a real HTTP response", plainResp.StatusCode)
	}

	// Both TLS surfaces present the ACME certificate for dns_name,
	// issued by the fake authority (NOT the MITM CA).
	proxyLeaf := acmeHandshake(t, cfg.Cache.ListenTLS)
	adminLeaf := acmeHandshake(t, cfg.Admin.Listen)
	if got, want := acmeFingerprint(adminLeaf), acmeFingerprint(proxyLeaf); got != want {
		t.Errorf("admin and proxy TLS present different certificates: admin=%s proxy=%s", got, want)
	}
	if got, want := proxyLeaf.Issuer.CommonName, "fakeacme test CA"; got != want {
		t.Errorf("proxy TLS cert issuer = %q, want the ACME authority %q (a MITM-issued leaf would prove CA mixing)", got, want)
	}
	if !slices.Contains(proxyLeaf.DNSNames, acmeTestDNSName) {
		t.Errorf("proxy TLS cert DNSNames = %v, want it to contain %q", proxyLeaf.DNSNames, acmeTestDNSName)
	}
	if got, want := acmeFingerprint(proxyLeaf), acmeStoredFingerprint(t, cacheDir); got != want {
		t.Errorf("presented cert fingerprint = %s, want the persisted cert %s", got, want)
	}
	if got := srv.IssueCount(); got != 1 {
		t.Errorf("fake authority IssueCount = %d, want 1", got)
	}

	// Storage layout: persistent dir, restrictive permissions, no
	// secret world-readable.
	acmeCheckPerm(t, filepath.Join(cacheDir, "acme"), 0o700)
	acmeCheckPerm(t, filepath.Join(cacheDir, "acme", "cert.pem"), 0o644)
	acmeCheckPerm(t, filepath.Join(cacheDir, "acme", "privkey.pem"), 0o600)
	acmeCheckPerm(t, filepath.Join(cacheDir, "acme", "account.key"), 0o600)
	if _, err := os.Stat(filepath.Join(cacheDir, "acme", "account.json")); err != nil {
		t.Errorf("stat account.json: %v", err)
	}

	// Status page: acme section populated, tls_mitm section present
	// and independent, admin listener advertised over https.
	st := acmeFetchStatus(t, cfg.Admin.Listen, true)
	if !st.ACME.Enabled {
		t.Fatalf("status acme.enabled = false, want true; payload: %+v", st.ACME)
	}
	if st.ACME.DNSName != acmeTestDNSName {
		t.Errorf("status acme.dns_name = %q, want %q", st.ACME.DNSName, acmeTestDNSName)
	}
	if st.ACME.DirectoryURL != srv.DirectoryURL() {
		t.Errorf("status acme.directory_url = %q, want %q", st.ACME.DirectoryURL, srv.DirectoryURL())
	}
	if want := []string{"cache", "admin"}; !slices.Equal(st.ACME.Surfaces, want) {
		t.Errorf("status acme.surfaces = %v, want %v", st.ACME.Surfaces, want)
	}
	if got, want := st.ACME.CertFingerprintSHA256, acmeStoredFingerprint(t, cacheDir); got != want {
		t.Errorf("status acme.cert_fingerprint_sha256 = %s, want %s", got, want)
	}
	if st.ACME.CertNotAfterUnixTime == nil {
		t.Errorf("status acme.cert_not_after_unixtime = null, want the current cert's expiration")
	}
	if st.ACME.LastError != "" {
		t.Errorf("status acme.last_error = %q, want empty", st.ACME.LastError)
	}
	if !st.TLSMITM.Enabled {
		t.Errorf("status tls_mitm.enabled = false, want true (MITM enabled alongside ACME)")
	}
	adminAdvertisedHTTPS := false
	for _, l := range st.Listeners {
		if l.Role == "admin" && l.Addr == "https://"+cfg.Admin.Listen {
			adminAdvertisedHTTPS = true
		}
	}
	if !adminAdvertisedHTTPS {
		t.Errorf("status listeners do not advertise admin as https://%s: %+v", cfg.Admin.Listen, st.Listeners)
	}

	acmeStopServe(t, cancel, done)
}

// TestServe_ACME_RestartReusesStoredCert covers the persistence
// contract: a restart on the same cache dir serves the stored
// certificate immediately, without contacting the authority again
// (no re-issuance, no new account).
func TestServe_ACME_RestartReusesStoredCert(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	srv := fakeacme.New(t)
	cacheDir := t.TempDir()
	cfg := acmeTestConfig(t, cacheDir, srv)

	ctx, cancel := context.WithCancel(context.Background())
	done := acmeStartServe(ctx, cfg)
	if err := waitForDaemonReady(t, cfg.Cache.Listen, 15*time.Second); err != nil {
		cancel()
		<-done
		t.Fatalf("first boot never became ready: %v", err)
	}
	firstLeaf := acmeHandshake(t, cfg.Cache.ListenTLS)
	acmeStopServe(t, cancel, done)

	// admin.New registers gauges into the global registry at every
	// boot; drop the first boot's registrations before the second so
	// the restart does not trip the duplicate-registration panic.
	metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	preMetrics = metrics.Default.SnapshotNamesForTest()

	// Second boot on the same cache dir.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := acmeStartServe(ctx2, cfg)
	t.Cleanup(func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(15 * time.Second):
			t.Errorf("restart: serve did not return on cleanup")
		}
	})
	if err := waitForDaemonReady(t, cfg.Cache.Listen, 15*time.Second); err != nil {
		t.Fatalf("restart never became ready: %v", err)
	}

	if got := srv.IssueCount(); got != 1 {
		t.Errorf("after restart IssueCount = %d, want 1 (stored cert must be reused, not re-issued)", got)
	}
	if got := srv.AccountCount(); got != 1 {
		t.Errorf("after restart AccountCount = %d, want 1 (account must be reused)", got)
	}
	restartLeaf := acmeHandshake(t, cfg.Cache.ListenTLS)
	if got, want := acmeFingerprint(restartLeaf), acmeFingerprint(firstLeaf); got != want {
		t.Errorf("after restart fingerprint = %s, want the persisted cert %s", got, want)
	}
}

// TestServe_ACME_IssuanceFailure_FailsBoot pins the startup contract:
// when no usable certificate is available at boot (first issuance
// fails), serve() returns an error and NO listener is bound — not even
// the plain proxy listener.
func TestServe_ACME_IssuanceFailure_FailsBoot(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Cache.Listen = acmeFreeAddr(t)
	cfg.Cache.ListenTLS = acmeFreeAddr(t)
	cfg.Cache.UseAcme = true
	cfg.Admin = acmeTestAdminConfig(acmeFreeAddr(t), true)
	cfg.Acme = config.AcmeConfig{
		Enabled:         true,
		DirectoryURL:    "http://" + acmeFreeAddr(t) + "/directory", // nothing listens there
		DNSName:         acmeTestDNSName,
		Listen:          acmeFreeAddr(t),
		RenewBeforeDays: 7,
		CheckInterval:   config.Duration{Duration: 12 * time.Hour},
	}

	err := serve(context.Background(), cfg, newTestLogger())
	if err == nil {
		t.Fatal("serve succeeded, want an ACME issuance failure at boot")
	}
	if !strings.Contains(err.Error(), "acme") {
		t.Errorf("serve error = %v, want it to name the acme failure", err)
	}

	// No surface is served: every configured listener address refuses
	// connections (ACME materialization runs before any bind).
	for name, addr := range map[string]string{
		"cache.listen":     cfg.Cache.Listen,
		"cache.listen_tls": cfg.Cache.ListenTLS,
		"admin.listen":     cfg.Admin.Listen,
	} {
		conn, dialErr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			t.Errorf("%s (%s) accepted a connection after the failed ACME boot — no surface may be served", name, addr)
		}
	}
}

// TestServe_ACME_CacheOnly_AdminPlainHTTP covers the partial
// selection: cache.use_acme serves the proxy HTTPS listener with the
// ACME certificate while admin stays plain HTTP and reports only the
// cache surface.
func TestServe_ACME_CacheOnly_AdminPlainHTTP(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	srv := fakeacme.New(t)
	cfg := acmeTestConfig(t, t.TempDir(), srv)
	cfg.Admin = acmeTestAdminConfig(cfg.Admin.Listen, false)

	ctx, cancel := context.WithCancel(context.Background())
	done := acmeStartServe(ctx, cfg)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Errorf("serve did not return on cleanup")
		}
	})

	if err := waitForDaemonReady(t, cfg.Cache.Listen, 15*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	leaf := acmeHandshake(t, cfg.Cache.ListenTLS)
	if got, want := leaf.Issuer.CommonName, "fakeacme test CA"; got != want {
		t.Errorf("proxy TLS cert issuer = %q, want %q", got, want)
	}

	// Admin is plain HTTP and answers.
	st := acmeFetchStatus(t, cfg.Admin.Listen, false)
	if !st.ACME.Enabled {
		t.Fatalf("status acme.enabled = false, want true")
	}
	if want := []string{"cache"}; !slices.Equal(st.ACME.Surfaces, want) {
		t.Errorf("status acme.surfaces = %v, want %v", st.ACME.Surfaces, want)
	}
	if got, want := st.ACME.CertFingerprintSHA256, acmeFingerprint(leaf); got != want {
		t.Errorf("status acme.cert_fingerprint_sha256 = %s, want the presented cert %s", got, want)
	}
	// The admin listener is advertised without an https:// scheme.
	for _, l := range st.Listeners {
		if l.Role == "admin" && l.Addr != cfg.Admin.Listen {
			t.Errorf("status admin listener addr = %q, want the plain address %q", l.Addr, cfg.Admin.Listen)
		}
	}
}

// TestServe_ManualTLSProxy_NoACME is the manual-PEM non-regression
// test: cache.listen_tls with cache.tls_cert / cache.tls_key and ACME
// disabled keeps serving the operator-supplied certificate, and the
// status page reports the acme section as disabled.
func TestServe_ManualTLSProxy_NoACME(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	cacheDir := t.TempDir()
	certPath, keyPath := testutil.WriteSelfSignedCert(t, t.TempDir())
	cfg := minimalCfg(cacheDir, nil)
	cfg.Cache.Listen = acmeFreeAddr(t)
	cfg.Cache.ListenTLS = acmeFreeAddr(t)
	cfg.Cache.TLSCert = certPath
	cfg.Cache.TLSKey = keyPath
	cfg.Admin = acmeTestAdminConfig(acmeFreeAddr(t), false)

	ctx, cancel := context.WithCancel(context.Background())
	done := acmeStartServe(ctx, cfg)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("serve did not return on cleanup")
		}
	})

	if err := waitForDaemonReady(t, cfg.Cache.Listen, 15*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// The proxy HTTPS listener verifies against the manual cert (full
	// chain validation, not InsecureSkipVerify).
	conn, err := tls.Dial("tcp", cfg.Cache.ListenTLS, &tls.Config{
		RootCAs:    testutil.CertPoolFromFile(t, certPath),
		ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("tls dial proxy with manual cert trust: %v", err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	_ = conn.Close()

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read manual cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	manualCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse manual cert: %v", err)
	}
	if got, want := acmeFingerprint(leaf), acmeFingerprint(manualCert); got != want {
		t.Errorf("proxy TLS presented fingerprint = %s, want the manual cert %s", got, want)
	}

	// No ACME storage is created when disabled.
	if _, err := os.Stat(filepath.Join(cacheDir, "acme")); !os.IsNotExist(err) {
		t.Errorf("acme storage dir exists with acme disabled (stat err = %v)", err)
	}

	st := acmeFetchStatus(t, cfg.Admin.Listen, false)
	if st.ACME.Enabled {
		t.Errorf("status acme.enabled = true with acme disabled; payload: %+v", st.ACME)
	}
}

// acmeFailingListener is a listener whose Accept fails permanently, so
// the server using it exits unexpectedly (serveListeners' errCh path).
type acmeFailingListener struct{ net.Listener }

func (acmeFailingListener) Accept() (net.Conn, error) {
	return nil, errors.New("simulated accept failure")
}

// TestServeListeners_ACME_ListenerFailureShutsDown pins that a listener
// failure with ACME enabled shuts the daemon down and returns the
// listener error: the renewal loop must stop with it, not keep the
// final wait blocked on a serve ctx nobody cancels.
func TestServeListeners_ACME_ListenerFailureShutsDown(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })

	srv := fakeacme.New(t)
	cfg := acmeTestConfig(t, t.TempDir(), srv)
	logger := newTestLogger()
	mgr, err := newACMEManager(cfg, logger)
	if err != nil {
		t.Fatalf("newACMEManager: %v", err)
	}
	if err := mgr.EnsureCertificate(context.Background()); err != nil {
		t.Fatalf("EnsureCertificate: %v", err)
	}

	listen := func(addr string) net.Listener {
		t.Helper()
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listen %s: %v", addr, err)
		}
		return ln
	}
	plainLn := listen(cfg.Cache.Listen)
	tlsLn := acmeFailingListener{listen(cfg.Cache.ListenTLS)}
	adminLn := listen(cfg.Admin.Listen)

	// The serve ctx is never cancelled: only the listener failure may
	// end serveListeners.
	done := make(chan error, 1)
	go func() {
		done <- serveListeners(context.Background(), cfg, logger, plainLn, tlsLn, adminLn, nil, mgr)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "simulated accept failure") {
			t.Errorf("serveListeners = %v, want the https listener error", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serveListeners did not return after a listener failure")
	}
}
