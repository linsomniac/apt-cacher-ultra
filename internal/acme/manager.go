package acme

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
)

const attemptTimeout = 5 * time.Minute
const minimumLifetime = time.Hour

type Config struct {
	DirectoryURL, DNSName, StateDir string
	AcceptTerms                     bool
	Challenge                       *Challenge
	// Transport and Now are injection seams. Production uses verified HTTPS
	// with system roots and the wall clock; URL policy also wraps test transports.
	Transport http.RoundTripper
	Now       func() time.Time
}

type managerStatus struct {
	problem       string
	next, success time.Time
}

type Manager struct {
	servertls.Holder
	cfg                           Config
	store                         *store
	mu                            sync.Mutex
	closeOnce                     sync.Once
	status                        atomic.Pointer[managerStatus]
	retry                         retryRecord
	renewAt, obtainedAt           time.Time
	storageFailed                 bool
	attempts, successes, failures atomic.Uint64
}

func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	if err := config.ValidateACMEDirectory(cfg.DirectoryURL); err != nil {
		return nil, err
	}
	if err := config.ValidateACMEName(cfg.DNSName); err != nil {
		return nil, err
	}
	if !cfg.AcceptTerms || cfg.StateDir == "" || cfg.Challenge == nil {
		return nil, errors.New("acme: terms acceptance, state directory and challenge listener are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s, err := openStore(cfg.StateDir, cfg.DirectoryURL, cfg.DNSName)
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, store: s}
	m.status.Store(&managerStatus{})
	// Reading under the same lock as writes also covers account/retry state.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.lock(ctx)
	if err != nil {
		s.close()
		return nil, err
	}
	defer release()
	if err := m.load(); err != nil {
		s.close()
		return nil, err
	}
	return m, nil
}

// Close is called after the owner cancels and joins Run/EnsureCertificate.
func (m *Manager) Close() { m.closeOnce.Do(m.store.close) }

func (m *Manager) load() error {
	var rec certificateRecord
	if err := m.store.read(m.store.certName, &rec); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err == nil {
		if rec.Version != 1 || rec.Directory != m.cfg.DirectoryURL || rec.Name != m.cfg.DNSName {
			return errors.New("acme storage: certificate identity mismatch")
		}
		cert, err := servertls.Parse(rec.Certificate, rec.Key, m.cfg.DNSName, m.cfg.Now())
		if err == nil {
			m.Install(cert)
			m.obtainedAt = rec.ObtainedAt
			// Clamp corrupt or obsolete scheduling metadata to the safe bound.
			latest := renewalTime(cert, 0)
			m.renewAt = rec.RenewAt
			if m.renewAt.IsZero() || m.renewAt.After(latest) {
				m.renewAt = latest
			}
		}
		// Expired/invalid certificate material is never published. Preserve
		// the bundle until a validated replacement is durably committed.
	}
	var retry retryRecord
	if err := m.store.read(m.store.retryName, &retry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err == nil {
		if retry.Version != 1 || retry.Failures < 0 || retry.Failures > 64 || !safeProblem(retry.Problem) {
			return errors.New("acme storage: invalid retry state")
		}
		m.retry = retry
	}
	m.publishStatus()
	return nil
}

func (m *Manager) publishStatus() {
	next := m.renewAt
	if m.retry.Next.After(next) {
		next = m.retry.Next
	}
	m.status.Store(&managerStatus{problem: m.retry.Problem, next: next, success: m.obtainedAt})
}

func (m *Manager) Snapshot() servertls.Snapshot {
	s := m.status.Load()
	return m.Holder.Snapshot("acme", s.problem, m.cfg.Now())
}

type Status struct {
	servertls.Snapshot
	DNSName      string `json:"dns_name"`
	DirectoryURL string `json:"directory_url"`
	NextAttempt  int64  `json:"next_attempt_unixtime,omitempty"`
	LastSuccess  int64  `json:"last_success_unixtime,omitempty"`
	Attempts     uint64 `json:"attempts"`
	Successes    uint64 `json:"successes"`
	Failures     uint64 `json:"failures"`
}

func (m *Manager) Status() Status {
	s := m.status.Load()
	out := Status{Snapshot: m.Snapshot(), DNSName: m.cfg.DNSName, DirectoryURL: m.cfg.DirectoryURL,
		Attempts: m.attempts.Load(), Successes: m.successes.Load(), Failures: m.failures.Load()}
	if !s.next.IsZero() {
		out.NextAttempt = s.next.Unix()
	}
	if !s.success.IsZero() {
		out.LastSuccess = s.success.Unix()
	}
	return out
}

func (m *Manager) EnsureCertificate(ctx context.Context) error { return m.ensure(ctx, false) }

// ensure serializes account, retry and certificate state under the directory
// lock. A restart observes the same cooldown as a running renewal worker.
func (m *Manager) ensure(ctx context.Context, renewal bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.storageFailed {
		return errors.New("acme: storage_failed")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := m.store.lock(lockCtx)
	if err != nil {
		return errors.New("acme: storage_lock_failed")
	}
	defer release()
	if err := m.load(); err != nil {
		return m.stopOnStorageError()
	}
	now := m.cfg.Now()
	if m.Available(now) == nil && (!renewal || now.Before(m.renewAt)) {
		return nil
	}
	if now.Before(m.retry.Next) {
		return errors.New("acme: retry_deferred")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Reserve a cooldown BEFORE the order: crashes/kills during an attempt
	// cannot repeatedly place orders without recording their next retry.
	count := min(m.retry.Failures+1, 64)
	m.retry = retryRecord{Version: 1, Failures: count, Next: now.Add(attemptTimeout + backoff(count)), Problem: "issuance_interrupted"}
	if err := m.store.write(m.store.retryName, m.retry); err != nil {
		return m.stopOnStorageError()
	}
	m.publishStatus()
	m.attempts.Add(1)
	attemptCtx, stop := context.WithTimeout(ctx, attemptTimeout)
	defer stop()
	cert, key, err := m.issue(attemptCtx)
	if err != nil {
		m.failures.Add(1)
		if m.storageFailed {
			return errors.New("acme: storage_failed")
		}
		m.retry.Problem = classify(err)
		deadline := m.cfg.Now().Add(backoff(count))
		if deadline.After(m.retry.Next) {
			m.retry.Next = deadline
		}
		if err := m.store.write(m.store.retryName, m.retry); err != nil {
			return m.stopOnStorageError()
		}
		m.publishStatus()
		return errors.New("acme: " + m.retry.Problem)
	}
	now = m.cfg.Now()
	parsed, err := servertls.Parse(cert, key, m.cfg.DNSName, now)
	before, after := servertls.Validity(parsed)
	if err != nil || after.Sub(now) < minimumLifetime {
		m.failures.Add(1)
		m.retry.Problem = "certificate_invalid"
		if err := m.store.write(m.store.retryName, m.retry); err != nil {
			return m.stopOnStorageError()
		}
		m.publishStatus()
		return errors.New("acme: certificate_invalid")
	}
	renewAt := renewalTime(parsed, jitter(after.Sub(before)/20))
	if !renewAt.After(now) { // Do not accept a certificate already needing renewal.
		m.failures.Add(1)
		m.retry.Problem = "certificate_invalid"
		if err := m.store.write(m.store.retryName, m.retry); err != nil {
			return m.stopOnStorageError()
		}
		m.publishStatus()
		return errors.New("acme: certificate_invalid")
	}
	record := certificateRecord{Version: 1, Directory: m.cfg.DirectoryURL, Name: m.cfg.DNSName, Certificate: cert, Key: key, RenewAt: renewAt, ObtainedAt: now}
	if err := m.store.write(m.store.certName, record); err != nil {
		return m.stopOnStorageError()
	}
	m.Install(parsed)
	m.renewAt, m.obtainedAt = renewAt, now
	m.retry = retryRecord{Version: 1}
	if err := m.store.write(m.store.retryName, m.retry); err != nil {
		return m.stopOnStorageError()
	}
	m.successes.Add(1)
	m.publishStatus()
	return nil
}

func (m *Manager) stopOnStorageError() error {
	m.storageFailed = true
	m.retry.Problem = "storage_failed"
	m.publishStatus()
	return errors.New("acme: storage_failed")
}

func renewalTime(c *tls.Certificate, early time.Duration) time.Time {
	before, after := servertls.Validity(c)
	life := after.Sub(before)
	remaining := life / 3
	if life < 10*24*time.Hour {
		remaining = life / 2
	}
	return after.Add(-remaining - early)
}

func jitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
func backoff(count int) time.Duration {
	return min(time.Minute*time.Duration(1<<min(max(count-1, 0), 6)), time.Hour) + jitter(15*time.Second)
}

func (m *Manager) Run(ctx context.Context, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.ensure(ctx, true); err != nil && ctx.Err() == nil {
			logger.Warn("acme_renewal_failed", "category", m.status.Load().problem)
		}
		if ctx.Err() != nil {
			return
		}
		s := m.status.Load()
		delay := s.next.Sub(m.cfg.Now())
		if delay < time.Minute {
			delay = time.Minute
		}
		if delay > 12*time.Hour {
			delay = 12 * time.Hour
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
