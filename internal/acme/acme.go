package acme

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// issueTimeout bounds one issuance/renewal attempt (order + challenges +
// finalize + download) so a hung authority cannot wedge the daemon's
// startup or renewal loop forever. The challenge server is stopped when
// the deadline expires, per the spec's "délai d'expiration".
const issueTimeout = 10 * time.Minute

// Config configures a Manager. All fields except LockTimeout,
// HTTPClient, LogFn, Now and Backoff are required (config.Validate
// enforces them for file-loaded configs).
type Config struct {
	DirectoryURL  string
	DNSName       string
	Listen        string // HTTP-01 challenge bind address
	StorageDir    string // <cache.dir>/acme
	RenewBefore   time.Duration
	CheckInterval time.Duration

	// LockTimeout bounds the interprocess flock acquisition during an
	// issuance/renewal. Default 30s.
	LockTimeout time.Duration
	// UserAgent is sent to the ACME authority. Default "apt-cacher-ultra".
	UserAgent string
	// HTTPClient is used for ACME API calls. Default: 30s-timeout client.
	HTTPClient *http.Client
	// LogFn receives structured log events. Never receives keys, tokens
	// or signed URLs. nil discards.
	LogFn func(level, event string, fields map[string]any)
	// Now returns the current time. nil = time.Now. Tests override.
	Now func() time.Time
	// Backoff computes the delay before retry n (1-based) after a failed
	// renewal. nil = the spec schedule: 1m doubling per failure, capped
	// at 1h. Tests override to keep the loop fast.
	Backoff func(failures int) time.Duration
}

// certState is the hot-reloadable certificate holder: GetCertificate
// reads it without locking, so a renewed certificate is picked up by new
// TLS handshakes without a restart and without touching existing
// connections.
type certState struct {
	cert *tls.Certificate
	leaf *x509.Certificate
}

// Snapshot is the observability view of the manager. It carries no
// secret: no keys, no tokens, no signed URLs.
type Snapshot struct {
	DNSName               string
	DirectoryURL          string
	HasCert               bool
	CertFingerprintSHA256 string
	CertNotBefore         time.Time
	CertNotAfter          time.Time
	LastError             string
	LastRenewalAt         time.Time
}

// Manager owns the ACME account, the current certificate and the
// renewal loop for one dns_name shared by every selecting surface.
type Manager struct {
	cfg  Config
	now  func() time.Time
	log  func(level, event string, fields map[string]any)
	chal *challengeServer

	// mu serializes issuance/renewal in-process; the interprocess flock
	// (storage.go, lockFile) serializes across daemon instances sharing
	// the same cache dir.
	mu          sync.Mutex
	cert        atomic.Pointer[certState]
	lastErr     atomic.Value // string
	lastRenewal atomic.Value // time.Time
	accountURL  atomic.Value // string
}

// NewManager creates the manager and loads any stored certificate. It
// does NOT contact the ACME authority: issuance happens in
// EnsureCertificate so the caller controls when the daemon blocks on the
// network.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.DirectoryURL == "" {
		return nil, errors.New("acme: DirectoryURL is required")
	}
	if cfg.DNSName == "" {
		return nil, errors.New("acme: DNSName is required")
	}
	if cfg.Listen == "" {
		return nil, errors.New("acme: Listen is required")
	}
	if cfg.StorageDir == "" {
		return nil, errors.New("acme: StorageDir is required")
	}
	if cfg.RenewBefore <= 0 {
		return nil, errors.New("acme: RenewBefore must be > 0")
	}
	if cfg.CheckInterval <= 0 {
		return nil, errors.New("acme: CheckInterval must be > 0")
	}
	if cfg.LockTimeout == 0 {
		cfg.LockTimeout = 30 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "apt-cacher-ultra"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	m := &Manager{
		cfg:  cfg,
		now:  cfg.Now,
		chal: newChallengeServer(),
	}
	if m.now == nil {
		m.now = time.Now
	}
	if cfg.LogFn != nil {
		m.log = cfg.LogFn
	} else {
		m.log = func(string, string, map[string]any) {}
	}
	if err := ensurePrivateDir(cfg.StorageDir); err != nil {
		return nil, err
	}
	// Load a stored certificate so a restart serves it immediately,
	// without contacting the authority.
	if st, err := m.loadStored(); err != nil {
		return nil, err
	} else if st != nil {
		m.adopt(st)
	}
	if _, meta, ok, err := loadAccount(cfg.StorageDir); err != nil {
		return nil, err
	} else if ok && meta.DirectoryURL == cfg.DirectoryURL {
		m.accountURL.Store(meta.AccountURL)
	}
	return m, nil
}

// loadStored reads the stored certificate. It returns nil (no error) when
// none is stored or when the stored one must not be served: invalid
// (unparseable, chain/key mismatch) or issued by another directory than
// the configured one. Only I/O failures are errors.
func (m *Manager) loadStored() (*certState, error) {
	cert, meta, ok, err := loadCert(m.cfg.StorageDir)
	if errors.Is(err, errStoredCertInvalid) {
		m.log("warn", "acme_stored_cert_invalid", map[string]any{
			"dns_name": m.cfg.DNSName,
			"err":      err.Error(),
		})
		return nil, nil
	}
	if err != nil || !ok {
		return nil, err
	}
	if meta.DirectoryURL != m.cfg.DirectoryURL {
		m.log("warn", "acme_stored_cert_directory_mismatch", map[string]any{
			"dns_name":             m.cfg.DNSName,
			"directory_url":        m.cfg.DirectoryURL,
			"stored_directory_url": meta.DirectoryURL,
		})
		return nil, nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		m.log("warn", "acme_stored_cert_invalid", map[string]any{
			"dns_name": m.cfg.DNSName,
			"err":      err.Error(),
		})
		return nil, nil
	}
	return &certState{cert: cert, leaf: leaf}, nil
}

// adopt serves a certificate read from storage.
func (m *Manager) adopt(st *certState) {
	m.cert.Store(st)
	m.log("info", "acme_cert_loaded", map[string]any{
		"dns_name":           m.cfg.DNSName,
		"fingerprint_sha256": fingerprintSHA256(st.leaf),
		"not_after_unixtime": st.leaf.NotAfter.Unix(),
	})
	m.warnIfWindowClamped(st.leaf)
}

// EnsureCertificate makes sure a usable certificate (parseable, not
// expired, matching dns_name) is available, obtaining one from the ACME
// authority when needed. It blocks until the first certificate is
// obtained or fails — callers run it before serving ACME surfaces, and
// its error means the daemon must not start (docs/acme-certificates-spec.md
// § "Cycle de vie et stockage").
func (m *Manager) EnsureCertificate(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usable(m.cert.Load()) {
		return nil
	}
	if st := m.cert.Load(); st != nil {
		m.log("warn", "acme_cert_expired", map[string]any{
			"dns_name":           m.cfg.DNSName,
			"not_after_unixtime": st.leaf.NotAfter.Unix(),
		})
	}
	return m.obtain(ctx)
}

// usable reports whether st can be served: present, not expired, and
// valid for the configured dns_name.
func (m *Manager) usable(st *certState) bool {
	if st == nil || st.leaf == nil {
		return false
	}
	if !m.now().Before(st.leaf.NotAfter) {
		return false
	}
	return st.leaf.VerifyHostname(m.cfg.DNSName) == nil
}

// renewalDue reports whether st has entered the renewal window
// (expiration within renewWindow).
func (m *Manager) renewalDue(st *certState) bool {
	if st == nil || st.leaf == nil {
		return true
	}
	window, _ := m.renewWindow(st.leaf)
	return !m.now().Add(window).Before(st.leaf.NotAfter)
}

// renewWindow is RenewBefore, clamped to a third of the certificate
// lifetime when RenewBefore covers the whole lifetime (e.g.
// renew_before_days = 7 with 6-day certificates): otherwise every
// certificate would be due as soon as issued, and each check would
// re-issue until the authority's rate limits lock the daemon out.
func (m *Manager) renewWindow(leaf *x509.Certificate) (window time.Duration, clamped bool) {
	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	if m.cfg.RenewBefore < lifetime {
		return m.cfg.RenewBefore, false
	}
	return lifetime / 3, true
}

// warnIfWindowClamped logs once per certificate when renewWindow had to
// clamp RenewBefore.
func (m *Manager) warnIfWindowClamped(leaf *x509.Certificate) {
	window, clamped := m.renewWindow(leaf)
	if !clamped {
		return
	}
	m.log("warn", "acme_renew_before_clamped", map[string]any{
		"dns_name":         m.cfg.DNSName,
		"renew_before":     m.cfg.RenewBefore.String(),
		"cert_lifetime":    leaf.NotAfter.Sub(leaf.NotBefore).String(),
		"effective_window": window.String(),
	})
}

// GetCertificate implements tls.Config.GetCertificate: new handshakes
// receive the current certificate, renewed in place without restart.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	st := m.cert.Load()
	if st == nil || st.cert == nil {
		return nil, errors.New("acme: no certificate available")
	}
	return st.cert, nil
}

// Run is the renewal loop. It checks the certificate expiration as soon
// as it starts — so a daemon restarted more often than CheckInterval
// still renews — then every CheckInterval, and renews once the
// RenewBefore window is entered. After
// a failed renewal the delay between retries doubles per failure —
// 1 minute after the first, then 2, 4, 8 minutes, capped at 1 hour —
// and resets to 1 minute after a success. A valid stored certificate
// keeps being served (via GetCertificate) throughout. Run returns when
// ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	failures := 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !m.renewalDue(m.cert.Load()) {
			timer.Reset(m.cfg.CheckInterval)
			continue
		}
		if err := m.renew(ctx); err != nil {
			failures++
			delay := m.backoffDelay(failures)
			m.log("warn", "acme_renewal_failed", map[string]any{
				"dns_name": m.cfg.DNSName,
				"err":      err.Error(),
				"retry_in": delay.String(),
				"failures": failures,
			})
			timer.Reset(delay)
			continue
		}
		failures = 0
		timer.Reset(m.cfg.CheckInterval)
	}
}

// renew performs one renewal attempt, serialized against concurrent
// issuances. A no-op when another attempt already renewed the cert.
func (m *Manager) renew(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.renewalDue(m.cert.Load()) {
		return nil
	}
	return m.obtain(ctx)
}

// backoffDelay is the spec's retry schedule: 1m after the first failure,
// doubling per failure, capped at 1h between two retries.
func (m *Manager) backoffDelay(failures int) time.Duration {
	if m.cfg.Backoff != nil {
		return m.cfg.Backoff(failures)
	}
	d := time.Minute
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= time.Hour {
			return time.Hour
		}
	}
	return d
}

// Snapshot returns the observability view (no secrets).
func (m *Manager) Snapshot() Snapshot {
	snap := Snapshot{
		DNSName:      m.cfg.DNSName,
		DirectoryURL: m.cfg.DirectoryURL,
	}
	if st := m.cert.Load(); st != nil && st.leaf != nil {
		snap.HasCert = true
		snap.CertFingerprintSHA256 = fingerprintSHA256(st.leaf)
		snap.CertNotBefore = st.leaf.NotBefore
		snap.CertNotAfter = st.leaf.NotAfter
	}
	if v := m.lastErr.Load(); v != nil {
		snap.LastError = v.(string)
	}
	if v := m.lastRenewal.Load(); v != nil {
		snap.LastRenewalAt = v.(time.Time)
	}
	return snap
}

// setCert installs a freshly obtained certificate and persists it.
// Callers hold m.mu.
func (m *Manager) setCert(cert *tls.Certificate, leaf *x509.Certificate, renewed bool) error {
	if err := saveCert(m.cfg.StorageDir, cert, certMeta{
		DirectoryURL: m.cfg.DirectoryURL,
		DNSName:      m.cfg.DNSName,
	}); err != nil {
		return err
	}
	m.cert.Store(&certState{cert: cert, leaf: leaf})
	m.lastErr.Store("")
	m.lastRenewal.Store(m.now())
	m.log("info", map[bool]string{true: "acme_cert_renewed", false: "acme_cert_issued"}[renewed], map[string]any{
		"dns_name":            m.cfg.DNSName,
		"fingerprint_sha256":  fingerprintSHA256(leaf),
		"not_before_unixtime": leaf.NotBefore.Unix(),
		"not_after_unixtime":  leaf.NotAfter.Unix(),
	})
	m.warnIfWindowClamped(leaf)
	return nil
}

// setLastErr records the latest issuance/renewal error for observability.
func (m *Manager) setLastErr(err error) {
	if err == nil {
		m.lastErr.Store("")
		return
	}
	m.lastErr.Store(err.Error())
}

func fingerprintSHA256(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:])
}
