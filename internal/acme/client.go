package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"os"

	"golang.org/x/crypto/acme"
)

// classify never returns authority-controlled text, URLs, headers or tokens.
func classify(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ae *acme.Error
	if errors.As(err, &ae) {
		switch ae.ProblemType {
		case "urn:ietf:params:acme:error:rateLimited":
			return "rate_limited"
		case "urn:ietf:params:acme:error:userActionRequired":
			return "action_required"
		case "urn:ietf:params:acme:error:unauthorized":
			return "authorization_failed"
		case "urn:ietf:params:acme:error:accountDoesNotExist":
			return "account_missing"
		}
	}
	var auth *acme.AuthorizationError
	if errors.As(err, &auth) {
		return "authorization_failed"
	}
	return "issuance_failed"
}

func safeProblem(s string) bool {
	switch s {
	case "", "issuance_interrupted", "certificate_invalid", "storage_failed", "cancelled", "timeout", "rate_limited", "action_required", "authorization_failed", "account_missing", "issuance_failed":
		return true
	}
	return false
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func (m *Manager) account(ctx context.Context, hc *http.Client) (*acme.Client, error) {
	var record accountRecord
	err := m.store.read("account.json", &record)
	if errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		keyPEM, err := encodeKey(key)
		if err != nil {
			return nil, err
		}
		record = accountRecord{Version: 1, Directory: m.cfg.DirectoryURL, Key: keyPEM}
		// Save the pending key before any registration request. Register is
		// idempotent by key and can recover an interrupted registration.
		if err := m.store.write("account.json", record); err != nil {
			return nil, m.stopOnStorageError()
		}
	} else if err != nil {
		return nil, m.stopOnStorageError()
	}
	if record.Version != 1 || record.Directory != m.cfg.DirectoryURL {
		return nil, m.stopOnStorageError()
	}
	block, _ := pem.Decode(record.Key)
	if block == nil {
		return nil, m.stopOnStorageError()
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, m.stopOnStorageError()
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, m.stopOnStorageError()
	}
	client := &acme.Client{Key: key, DirectoryURL: m.cfg.DirectoryURL, HTTPClient: hc, UserAgent: "apt-cacher-ultra", KID: acme.KeyID(record.URL)}
	if record.URL != "" {
		u, err := url.Parse(record.URL)
		directory, _ := url.Parse(m.cfg.DirectoryURL)
		if err != nil || !allowedURL(u, origin(directory)) {
			return nil, m.stopOnStorageError()
		}
		reg, err := client.GetReg(ctx, "")
		if err != nil {
			var problem *acme.Error
			if errors.As(err, &problem) && problem.ProblemType == "urn:ietf:params:acme:error:unauthorized" {
				return nil, &acme.Error{ProblemType: "urn:ietf:params:acme:error:userActionRequired"}
			}
			return nil, err
		}
		if reg.Status != acme.StatusValid {
			// Confirmed deactivation requires operator action. Do not silently
			// replace accounts to bypass an authority's administrative decision.
			return nil, &acme.Error{ProblemType: "urn:ietf:params:acme:error:userActionRequired"}
		}
		return client, nil
	}
	dir, err := client.Discover(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := client.Register(ctx, &acme.Account{}, func(string) bool { return m.cfg.AcceptTerms })
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		reg, err = client.GetReg(ctx, "")
	}
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(reg.URI)
	directory, _ := url.Parse(m.cfg.DirectoryURL)
	if err != nil || !allowedURL(u, origin(directory)) {
		return nil, errors.New("invalid account URL")
	}
	record.URL, record.Terms = reg.URI, dir.Terms
	if err := m.store.write("account.json", record); err != nil {
		return nil, m.stopOnStorageError()
	}
	return client, nil
}

func (m *Manager) issue(ctx context.Context) ([]byte, []byte, error) {
	hc := newHTTPClient(m.cfg.DirectoryURL, m.cfg.Transport, func(resp *http.Response) error {
		if resp.StatusCode < 400 {
			return nil
		}
		next := retryAfter(resp.Header.Get("Retry-After"), m.cfg.Now())
		if next.After(m.retry.Next) {
			m.retry.Next = next
			if err := m.store.write(m.store.retryName, m.retry); err != nil {
				return m.stopOnStorageError()
			}
			m.publishStatus()
		}
		return nil
	})
	defer hc.CloseIdleConnections()
	client, err := m.account(ctx, hc)
	if err != nil {
		return nil, nil, err
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(m.cfg.DNSName))
	if err != nil {
		return nil, nil, err
	}
	if len(order.AuthzURLs) != 1 {
		return nil, nil, errors.New("unexpected authorization count")
	}
	defer m.cfg.Challenge.clear()
	for _, authURL := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return nil, nil, err
		}
		if auth.Identifier.Type != "dns" || auth.Identifier.Value != m.cfg.DNSName || auth.Wildcard {
			return nil, nil, errors.New("unexpected authorization identifier")
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		var challenge *acme.Challenge
		for _, c := range auth.Challenges {
			if c.Type == "http-01" {
				challenge = c
				break
			}
		}
		if challenge == nil {
			return nil, nil, errors.New("HTTP-01 unavailable")
		}
		response, err := client.HTTP01ChallengeResponse(challenge.Token)
		if err != nil {
			return nil, nil, err
		}
		if err := m.cfg.Challenge.set(challenge.Token, response); err != nil {
			return nil, nil, err
		}
		if _, err := client.Accept(ctx, challenge); err != nil {
			return nil, nil, err
		}
		if _, err := client.WaitAuthorization(ctx, auth.URI); err != nil {
			return nil, nil, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{m.cfg.DNSName}}, key)
	if err != nil {
		return nil, nil, err
	}
	ready, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, nil, err
	}
	der, _, err := client.CreateCertFromOrder(ctx, ready, csr, true)
	if err != nil {
		return nil, nil, err
	}
	var certPEM []byte
	for _, c := range der {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	keyPEM, err := encodeKey(key)
	return certPEM, keyPEM, err
}
