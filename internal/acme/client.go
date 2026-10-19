package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
)

// acquireLock takes an exclusive flock on the ACME lock file, polling at
// 100ms granularity until acquired, timeout elapses or ctx is cancelled
// (so shutdown is not delayed by a peer holding the lock). It serializes
// issuance/renewal across daemon instances sharing the same cache dir
// (e.g. overlapping systemd restarts). The returned release function
// unlocks and closes the file descriptor.
func acquireLock(ctx context.Context, path string, timeout time.Duration) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acme: open flock %q: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("acme: flock %q: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("acme: issuance lock %q not acquired after %s — another daemon instance may be issuing", path, timeout)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("acme: waiting for issuance lock %q: %w", path, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// obtain runs one full issuance/renewal attempt: account load-or-create,
// order, HTTP-01 challenges via the transient challenge server, CSR,
// finalize, download, persist. The caller holds m.mu; obtain takes the
// interprocess flock for the duration.
func (m *Manager) obtain(ctx context.Context) error {
	release, err := acquireLock(ctx, filepath.Join(m.cfg.StorageDir, lockFile), m.cfg.LockTimeout)
	if err != nil {
		m.setLastErr(err)
		return err
	}
	defer release()

	// Another instance sharing the storage dir may have issued while this
	// one waited for the lock: serve its certificate instead of issuing a
	// duplicate (authorities rate-limit duplicate certificates).
	if st, err := m.loadStored(); err != nil {
		m.setLastErr(err)
		return err
	} else if m.usable(st) && !m.renewalDue(st) {
		m.adopt(st)
		m.lastErr.Store("")
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()

	client, accountURL, err := m.loadOrCreateAccount(ctx)
	if err != nil {
		m.setLastErr(err)
		return fmt.Errorf("acme: account: %w", err)
	}
	m.accountURL.Store(accountURL)

	cert, leaf, err := m.requestCertificate(ctx, client)
	if err != nil {
		m.setLastErr(err)
		return err
	}
	renewed := m.cert.Load() != nil
	if err := m.setCert(cert, leaf, renewed); err != nil {
		m.setLastErr(err)
		return err
	}
	return nil
}

// loadOrCreateAccount returns an ACME client bound to a usable account.
// A stored account (key + URL) is verified against the authority and
// reused; otherwise a new account is registered and persisted. The
// returned account URL is stored for observability.
func (m *Manager) loadOrCreateAccount(ctx context.Context) (*acme.Client, string, error) {
	newClient := func(key *ecdsa.PrivateKey, kid string) *acme.Client {
		c := &acme.Client{
			DirectoryURL: m.cfg.DirectoryURL,
			Key:          key,
			UserAgent:    m.cfg.UserAgent,
			HTTPClient:   m.cfg.HTTPClient,
		}
		if kid != "" {
			c.KID = acme.KeyID(kid)
		}
		return c
	}

	if key, meta, ok, err := loadAccount(m.cfg.StorageDir); err != nil {
		return nil, "", err
	} else if ok && meta.DirectoryURL != m.cfg.DirectoryURL {
		// An account belongs to one directory (e.g. Let's Encrypt staging
		// before a switch to production): register on the configured one.
		m.log("info", "acme_account_directory_changed", map[string]any{
			"dns_name":             m.cfg.DNSName,
			"directory_url":        m.cfg.DirectoryURL,
			"stored_directory_url": meta.DirectoryURL,
		})
	} else if ok {
		client := newClient(key, meta.AccountURL)
		acct, err := client.GetReg(ctx, "")
		switch {
		case err == nil && acct.Status == acme.StatusValid:
			return client, meta.AccountURL, nil
		case err == nil, errors.Is(err, acme.ErrNoAccount), isUnauthorized(err):
			// The stored account is gone, deactivated or revoked. RFC 8555
			// §7.3.6: the authority rejects every request signed by a
			// deactivated account's key, so register a new account with a
			// fresh key below (it replaces the stored one).
			status := ""
			if acct != nil {
				status = acct.Status
			}
			m.log("warn", "acme_account_unusable", map[string]any{
				"dns_name":    m.cfg.DNSName,
				"account_url": meta.AccountURL,
				"status":      status,
			})
		default:
			// Transient (network, 5xx): keep the stored account and let
			// the caller retry.
			return nil, "", err
		}
	}

	key, err := generateAccountKey()
	if err != nil {
		return nil, "", err
	}
	client := newClient(key, "")
	acct, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS)
	if err != nil {
		return nil, "", err
	}
	if err := saveAccount(m.cfg.StorageDir, key, accountMeta{
		AccountURL:   acct.URI,
		DirectoryURL: m.cfg.DirectoryURL,
		DNSName:      m.cfg.DNSName,
	}); err != nil {
		return nil, "", err
	}
	m.log("info", "acme_account_created", map[string]any{"dns_name": m.cfg.DNSName})
	return client, acct.URI, nil
}

// requestCertificate runs the RFC 8555 order flow for the configured
// dns_name: order, one HTTP-01 challenge per authorization, CSR,
// finalize, download. The transient challenge server is started before
// the first Accept and stopped after the last authorization resolves
// (or the attempt fails / times out).
func (m *Manager) requestCertificate(ctx context.Context, client *acme.Client) (*tls.Certificate, *x509.Certificate, error) {
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(m.cfg.DNSName))
	if err != nil {
		return nil, nil, fmt.Errorf("acme: new order: %w", err)
	}

	started := false
	defer func() {
		if started {
			m.chal.stop()
		}
	}()

	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, nil, fmt.Errorf("acme: get authorization: %w", err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		if err := m.answerHTTP01(ctx, client, authz, &started); err != nil {
			return nil, nil, err
		}
	}

	// CSR for the configured dns_name, signed by a fresh ECDSA P-256 key.
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("acme: generate certificate key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: m.cfg.DNSName},
		DNSNames: []string{m.cfg.DNSName},
	}, certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("acme: create CSR: %w", err)
	}

	// RFC 8555 §7.4: finalize only once the order is "ready". Authorities
	// may move it there asynchronously after the last authorization is
	// validated; finalizing a still-pending order fails with orderNotReady.
	if _, err := client.WaitOrder(ctx, order.URI); err != nil {
		return nil, nil, fmt.Errorf("acme: wait for order ready: %w", err)
	}
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return nil, nil, fmt.Errorf("acme: finalize order: %w", err)
	}
	if len(der) == 0 {
		return nil, nil, errors.New("acme: authority returned an empty certificate chain")
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, nil, fmt.Errorf("acme: parse issued certificate: %w", err)
	}
	if err := leaf.VerifyHostname(m.cfg.DNSName); err != nil {
		return nil, nil, fmt.Errorf("acme: issued certificate does not cover %s: %w", m.cfg.DNSName, err)
	}
	return &tls.Certificate{
		Certificate: der,
		PrivateKey:  certKey,
		Leaf:        leaf,
	}, leaf, nil
}

// answerHTTP01 completes one HTTP-01 authorization: it registers the
// key authorization with the challenge server (starting it on first
// use), notifies the authority, and waits for the validation result.
func (m *Manager) answerHTTP01(ctx context.Context, client *acme.Client, authz *acme.Authorization, started *bool) error {
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return fmt.Errorf("acme: authority offers no http-01 challenge for %s", authz.Identifier.Value)
	}
	keyAuth, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return fmt.Errorf("acme: key authorization: %w", err)
	}
	if !*started {
		if err := m.chal.start(m.cfg.Listen); err != nil {
			return err
		}
		*started = true
	}
	m.chal.set(chal.Token, keyAuth)
	if _, err := client.Accept(ctx, chal); err != nil {
		return fmt.Errorf("acme: notify challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("acme: challenge validation for %s: %w", authz.Identifier.Value, err)
	}
	return nil
}

// isUnauthorized reports whether err is the ACME "unauthorized" problem,
// returned for requests signed by a deactivated account's key.
func isUnauthorized(err error) bool {
	var ae *acme.Error
	return errors.As(err, &ae) && ae.ProblemType == "urn:ietf:params:acme:error:unauthorized"
}
