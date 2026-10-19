// Package fakeacme provides a minimal in-process RFC 8555 ACME server
// for tests. It implements just enough of the protocol for
// golang.org/x/crypto/acme clients: directory, nonces, account
// registration (with reuse via kid), orders, HTTP-01 challenges
// (validated by fetching the challenge over HTTP through a configurable
// base URL — simulating the public port 80), CSR finalization and
// certificate download. It issues certificates signed by a test CA.
//
// It does NOT verify JWS signatures; it validates nonce freshness and
// the HTTP-01 key authorization, which are the behaviors under test.
package fakeacme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

// Server is a fake ACME authority.
type Server struct {
	srv *httptest.Server

	mu         sync.Mutex
	nonces     map[string]struct{}
	accounts   map[string]*fakeAccount // by account ID
	orders     map[string]*fakeOrder
	authzs     map[string]*fakeAuthz
	challenges map[string]*fakeChallenge

	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	certLifetime time.Duration
	// orderReadyPolls: see WithOrderReadyAfterPolls.
	orderReadyPolls int

	// challengeBaseURL is where the authority fetches
	// http://<dns_name>/.well-known/acme-challenge/<token> from — the
	// simulated public port 80 (either the challenge listener directly
	// or a reverse proxy in front of it).
	challengeBaseURL atomic.Value // string
	// failChallenges makes every HTTP-01 validation fail (tests of the
	// renewal-failure path).
	failChallenges atomic.Bool
	// issueCount counts finalize calls (renewal tests).
	issueCount atomic.Int64
}

type fakeAccount struct {
	id          string
	jwk         map[string]any
	pub         *ecdsa.PublicKey
	deactivated bool
}

type fakeOrder struct {
	id          string
	dnsName     string
	status      string
	authzIDs    []string
	certDER     [][]byte
	finalizeURL string
	certURL     string
	// readyPolls counts order GETs answered "pending" after every
	// authorization became valid (WithOrderReadyAfterPolls).
	readyPolls int
}

type fakeAuthz struct {
	id          string
	dnsName     string
	status      string
	challengeID string
}

type fakeChallenge struct {
	id        string
	token     string
	status    string
	accountID string
	authzID   string
}

// Option configures a Server.
type Option func(*Server)

// WithCertLifetime sets the validity of issued certificates (default 90
// days, like Let's Encrypt).
func WithCertLifetime(d time.Duration) Option {
	return func(s *Server) { s.certLifetime = d }
}

// WithOrderReadyAfterPolls makes an order whose authorizations are all
// valid stay "pending" for n order GETs before turning "ready", like an
// authority that moves orders to ready asynchronously. Finalizing an
// order that is not ready fails with orderNotReady. Default 0: ready as
// soon as the authorizations are valid.
func WithOrderReadyAfterPolls(n int) Option {
	return func(s *Server) { s.orderReadyPolls = n }
}

// WithChallengeBaseURL sets the base URL the authority uses to fetch
// HTTP-01 challenge responses.
func WithChallengeBaseURL(url string) Option {
	return func(s *Server) { s.challengeBaseURL.Store(url) }
}

// New starts a fake ACME server.
func New(t *testing.T, opts ...Option) *Server {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("fakeacme: CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fakeacme test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("fakeacme: CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("fakeacme: parse CA cert: %v", err)
	}
	s := &Server{
		nonces:       make(map[string]struct{}),
		accounts:     make(map[string]*fakeAccount),
		orders:       make(map[string]*fakeOrder),
		authzs:       make(map[string]*fakeAuthz),
		challenges:   make(map[string]*fakeChallenge),
		caCert:       caCert,
		caKey:        caKey,
		certLifetime: 90 * 24 * time.Hour,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.srv.Close)
	return s
}

// DirectoryURL is the ACME directory endpoint of the fake server.
func (s *Server) DirectoryURL() string { return s.srv.URL + "/directory" }

// CACertPEM returns the test CA certificate PEM (trust anchor for issued
// certificates).
func (s *Server) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})
}

// SetChallengeBaseURL updates the simulated public base URL.
func (s *Server) SetChallengeBaseURL(url string) { s.challengeBaseURL.Store(url) }

// SetFailChallenges makes every HTTP-01 validation fail.
func (s *Server) SetFailChallenges(fail bool) { s.failChallenges.Store(fail) }

// IssueCount returns how many certificates were issued (finalize calls).
func (s *Server) IssueCount() int64 { return s.issueCount.Load() }

// AccountCount returns how many ACME accounts were registered.
func (s *Server) AccountCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

// DeactivateAccounts deactivates every registered account: like Let's
// Encrypt (RFC 8555 §7.3.6), further requests signed by their keys are
// rejected as unauthorized.
func (s *Server) DeactivateAccounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, acct := range s.accounts {
		acct.deactivated = true
	}
}

// base returns the server's base URL.
func (s *Server) base() string { return s.srv.URL }

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	s.setNonce(w)
	switch {
	case r.URL.Path == "/directory" && r.Method == http.MethodGet:
		s.handleDirectory(w, r)
	case r.URL.Path == "/nonce":
		// newNonce: HEAD (and GET) with a fresh Replay-Nonce.
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/account/") && r.Method == http.MethodPost:
		s.handleGetAccount(w, r)
	case r.URL.Path == "/account" && r.Method == http.MethodPost:
		s.handleNewAccount(w, r)
	case strings.HasPrefix(r.URL.Path, "/order/") && r.Method == http.MethodPost:
		s.handleGetOrder(w, r)
	case r.URL.Path == "/order" && r.Method == http.MethodPost:
		s.handleNewOrder(w, r)
	case strings.HasPrefix(r.URL.Path, "/authz/") && r.Method == http.MethodPost:
		s.handleGetAuthz(w, r)
	case strings.HasPrefix(r.URL.Path, "/challenge/") && r.Method == http.MethodPost:
		s.handleChallengeNotify(w, r)
	case strings.HasPrefix(r.URL.Path, "/finalize/") && r.Method == http.MethodPost:
		s.handleFinalize(w, r)
	case strings.HasPrefix(r.URL.Path, "/cert/") && r.Method == http.MethodPost:
		s.handleGetCert(w, r)
	default:
		http.NotFound(w, r)
	}
}

// setNonce attaches a fresh Replay-Nonce to every response (the client
// collects nonces from any response header).
func (s *Server) setNonce(w http.ResponseWriter) {
	nonce := randomToken()
	s.mu.Lock()
	s.nonces[nonce] = struct{}{}
	s.mu.Unlock()
	w.Header().Set("Replay-Nonce", nonce)
}

func (s *Server) handleDirectory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"newNonce":   s.base() + "/nonce",
		"newAccount": s.base() + "/account",
		"newOrder":   s.base() + "/order",
	})
}

// --- JWS helpers ---

type jwsEnvelope struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type jwsProtected struct {
	Alg   string          `json:"alg"`
	Nonce string          `json:"nonce"`
	URL   string          `json:"url"`
	JWK   json.RawMessage `json:"jwk"`
	KID   string          `json:"kid"`
}

// parseJWS decodes a flattened JWS request body and checks the nonce is
// fresh (single-use). It returns the protected header and the decoded
// payload (nil for POST-as-GET, whose payload is empty).
func (s *Server) parseJWS(w http.ResponseWriter, r *http.Request) (*jwsProtected, []byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, false
	}
	var env jwsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return nil, nil, false
	}
	protectedRaw, err := base64.RawURLEncoding.DecodeString(env.Protected)
	if err != nil {
		http.Error(w, "bad protected", http.StatusBadRequest)
		return nil, nil, false
	}
	var prot jwsProtected
	if err := json.Unmarshal(protectedRaw, &prot); err != nil {
		http.Error(w, "bad protected", http.StatusBadRequest)
		return nil, nil, false
	}
	s.mu.Lock()
	_, fresh := s.nonces[prot.Nonce]
	delete(s.nonces, prot.Nonce)
	s.mu.Unlock()
	if !fresh {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:badNonce", "bad nonce")
		return nil, nil, false
	}
	var payload []byte
	if env.Payload != "" {
		payload, err = base64.RawURLEncoding.DecodeString(env.Payload)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return nil, nil, false
		}
	}
	return &prot, payload, true
}

// accountFor resolves the account behind a request: via kid for
// authenticated endpoints, or creates the mapping for newAccount (jwk).
func (s *Server) accountFor(prot *jwsProtected) (*fakeAccount, bool) {
	if prot.KID != "" {
		id := strings.TrimPrefix(prot.KID, s.base()+"/account/")
		s.mu.Lock()
		defer s.mu.Unlock()
		acct, ok := s.accounts[id]
		return acct, ok
	}
	return nil, false
}

// authzsValid reports whether every authorization of o is valid. The
// caller holds s.mu.
func (s *Server) authzsValid(o *fakeOrder) bool {
	for _, id := range o.authzIDs {
		if a := s.authzs[id]; a == nil || a.status != "valid" {
			return false
		}
	}
	return true
}

// requireActiveAccount writes the matching problem and returns false when
// an authenticated request's kid is unknown or deactivated.
func (s *Server) requireActiveAccount(w http.ResponseWriter, prot *jwsProtected) bool {
	acct, ok := s.accountFor(prot)
	if !ok {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:accountDoesNotExist", "unknown account")
		return false
	}
	if acct.deactivated {
		writeProblem(w, http.StatusUnauthorized, "urn:ietf:params:acme:error:unauthorized", "account is deactivated")
		return false
	}
	return true
}

// accountByID looks up an account by its URL path ID.
func (s *Server) accountByID(id string) *fakeAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[id]
}

func (s *Server) handleNewAccount(w http.ResponseWriter, r *http.Request) {
	prot, payload, ok := s.parseJWS(w, r)
	if !ok {
		return
	}
	var req struct {
		OnlyReturnExisting bool     `json:"onlyReturnExisting"`
		Contact            []string `json:"contact"`
	}
	_ = json.Unmarshal(payload, &req)

	// Resolve the account key: via kid for a known account, or via the
	// jwk embedded in a fresh newAccount request.
	var pub *ecdsa.PublicKey
	var jwkMap map[string]any
	if acct, ok := s.accountFor(prot); ok {
		pub = acct.pub
	} else if len(prot.JWK) > 0 {
		if err := json.Unmarshal(prot.JWK, &jwkMap); err != nil {
			http.Error(w, "bad jwk", http.StatusBadRequest)
			return
		}
		var err error
		pub, err = ecPublicKeyFromJWK(jwkMap)
		if err != nil {
			http.Error(w, "bad jwk: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:malformed", "newAccount requires jwk or kid")
		return
	}
	thumb, err := acme.JWKThumbprint(pub)
	if err != nil {
		http.Error(w, "thumbprint: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	var existing *fakeAccount
	for _, acct := range s.accounts {
		if t, err := acme.JWKThumbprint(acct.pub); err == nil && t == thumb {
			existing = acct
			break
		}
	}
	s.mu.Unlock()
	if existing != nil && existing.deactivated {
		writeProblem(w, http.StatusUnauthorized, "urn:ietf:params:acme:error:unauthorized", "account is deactivated")
		return
	}
	if existing != nil {
		// Like Let's Encrypt: 200 + existing account URL whether the
		// request was a plain newAccount (accountExists) or an
		// onlyReturnExisting probe.
		w.Header().Set("Location", s.base()+"/account/"+existing.id)
		writeJSON(w, http.StatusOK, map[string]any{"status": "valid"})
		return
	}
	if req.OnlyReturnExisting {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:accountDoesNotExist", "no existing account for key")
		return
	}
	id := randomToken()[:16]
	s.mu.Lock()
	s.accounts[id] = &fakeAccount{id: id, jwk: jwkMap, pub: pub}
	s.mu.Unlock()
	w.Header().Set("Location", s.base()+"/account/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"status": "valid", "contact": req.Contact})
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	prot, _, ok := s.parseJWS(w, r)
	if !ok {
		return
	}
	if !s.requireActiveAccount(w, prot) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "valid"})
}

func (s *Server) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	prot, payload, ok := s.parseJWS(w, r)
	if !ok {
		return
	}
	if !s.requireActiveAccount(w, prot) {
		return
	}
	var req struct {
		Identifiers []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"identifiers"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || len(req.Identifiers) == 0 {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:malformed", "identifiers required")
		return
	}
	dnsName := req.Identifiers[0].Value
	orderID := randomToken()[:16]
	authzID := randomToken()[:16]
	challengeID := randomToken()[:16]
	acct, _ := s.accountFor(prot)
	s.mu.Lock()
	s.authzs[authzID] = &fakeAuthz{id: authzID, dnsName: dnsName, status: "pending", challengeID: challengeID}
	s.challenges[challengeID] = &fakeChallenge{
		id:        challengeID,
		token:     randomToken(),
		status:    "pending",
		accountID: acct.id,
		authzID:   authzID,
	}
	s.orders[orderID] = &fakeOrder{
		id:          orderID,
		dnsName:     dnsName,
		status:      "pending",
		authzIDs:    []string{authzID},
		finalizeURL: s.base() + "/finalize/" + orderID,
		certURL:     s.base() + "/cert/" + orderID,
	}
	s.mu.Unlock()
	w.Header().Set("Location", s.base()+"/order/"+orderID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":         "pending",
		"expires":        time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"identifiers":    req.Identifiers,
		"authorizations": []string{s.base() + "/authz/" + authzID},
		"finalize":       s.base() + "/finalize/" + orderID,
	})
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.parseJWS(w, r); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/order/")
	s.mu.Lock()
	order, ok := s.orders[id]
	if ok && order.status == "pending" && s.authzsValid(order) {
		if order.readyPolls >= s.orderReadyPolls {
			order.status = "ready"
		} else {
			order.readyPolls++
		}
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Location", s.base()+"/order/"+order.id)
	body := map[string]any{
		"status":         order.status,
		"identifiers":    []map[string]string{{"type": "dns", "value": order.dnsName}},
		"authorizations": []string{},
		"finalize":       order.finalizeURL,
	}
	for _, authzID := range order.authzIDs {
		body["authorizations"] = append(body["authorizations"].([]string), s.base()+"/authz/"+authzID)
	}
	if order.status == "valid" {
		body["certificate"] = order.certURL
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleGetAuthz(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.parseJWS(w, r); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/authz/")
	s.mu.Lock()
	authz, ok := s.authzs[id]
	var chal *fakeChallenge
	if ok {
		chal = s.challenges[authz.challengeID]
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	challenges := []any{}
	if chal != nil {
		challenges = append(challenges, map[string]any{
			"type":   "http-01",
			"url":    s.base() + "/challenge/" + chal.id,
			"token":  chal.token,
			"status": chal.status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     authz.status,
		"identifier": map[string]string{"type": "dns", "value": authz.dnsName},
		"challenges": challenges,
	})
}

// handleChallengeNotify is the client's "I am ready" POST. The authority
// validates immediately: it fetches the key authorization over HTTP from
// the simulated public base URL and compares it to the expected value
// (token + "." + account JWK thumbprint).
func (s *Server) handleChallengeNotify(w http.ResponseWriter, r *http.Request) {
	prot, _, ok := s.parseJWS(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/challenge/")
	s.mu.Lock()
	chal, ok := s.challenges[id]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.requireActiveAccount(w, prot) {
		return
	}
	acct := s.accountByID(chal.accountID)
	if acct == nil {
		http.Error(w, "unknown challenge account", http.StatusInternalServerError)
		return
	}

	thumb, err := acme.JWKThumbprint(acct.pub)
	if err != nil {
		http.Error(w, "thumbprint: "+err.Error(), http.StatusInternalServerError)
		return
	}
	expected := chal.token + "." + thumb

	valid := !s.failChallenges.Load()
	if valid {
		base, _ := s.challengeBaseURL.Load().(string)
		if base == "" {
			valid = false
		} else {
			valid = s.fetchChallenge(base, chal.token, expected)
		}
	}

	s.mu.Lock()
	if valid {
		chal.status = "valid"
		s.authzs[chal.authzID].status = "valid"
	} else {
		chal.status = "invalid"
		s.authzs[chal.authzID].status = "invalid"
	}
	status := chal.status
	s.mu.Unlock()

	body := map[string]any{
		"type":   "http-01",
		"url":    s.base() + "/challenge/" + chal.id,
		"token":  chal.token,
		"status": status,
	}
	if !valid {
		body["error"] = map[string]any{
			"type":   "urn:ietf:params:acme:error:unauthorized",
			"detail": "challenge fetch did not return the expected key authorization",
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// fetchChallenge retrieves the key authorization from the simulated
// public endpoint and compares it to the expected value.
func (s *Server) fetchChallenge(base, token, expected string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/.well-known/acme-challenge/" + token)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	return string(body) == expected
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	prot, payload, ok := s.parseJWS(w, r)
	if !ok {
		return
	}
	if !s.requireActiveAccount(w, prot) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/finalize/")
	s.mu.Lock()
	order, ok := s.orders[id]
	ready := false
	if ok {
		if order.status == "pending" && s.orderReadyPolls == 0 && s.authzsValid(order) {
			order.status = "ready"
		}
		ready = order.status == "ready"
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !ready {
		writeProblem(w, http.StatusForbidden, "urn:ietf:params:acme:error:orderNotReady", "order is not ready")
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:malformed", "csr required")
		return
	}
	csrDER, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:badCSR", "bad csr encoding")
		return
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:badCSR", "bad csr")
		return
	}
	if err := csr.CheckSignature(); err != nil {
		writeProblem(w, http.StatusBadRequest, "urn:ietf:params:acme:error:badCSR", "bad csr signature")
		return
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		http.Error(w, "serial", http.StatusInternalServerError)
		return
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(s.certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		http.Error(w, "sign: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	order.certDER = [][]byte{leafDER, s.caCert.Raw}
	order.status = "valid"
	s.mu.Unlock()
	s.issueCount.Add(1)

	w.Header().Set("Location", s.base()+"/order/"+order.id)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "valid",
		"identifiers":    []map[string]string{{"type": "dns", "value": order.dnsName}},
		"authorizations": []string{},
		"finalize":       order.finalizeURL,
		"certificate":    order.certURL,
	})
}

// handleGetCert serves the PEM chain (leaf + CA) for POST-as-GET, as
// x/crypto's fetchCertRFC expects.
func (s *Server) handleGetCert(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.parseJWS(w, r); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/cert/")
	s.mu.Lock()
	order, ok := s.orders[id]
	var chain [][]byte
	if ok {
		chain = order.certDER
	}
	s.mu.Unlock()
	if !ok || len(chain) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	w.WriteHeader(http.StatusOK)
	for _, der := range chain {
		_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   typ,
		"detail": detail,
		"status": status,
	})
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ecPublicKeyFromJWK reconstructs an ECDSA P-256 public key from its JWK.
func ecPublicKeyFromJWK(jwk map[string]any) (*ecdsa.PublicKey, error) {
	xStr, _ := jwk["x"].(string)
	yStr, _ := jwk["y"].(string)
	crv, _ := jwk["crv"].(string)
	if crv != "P-256" || xStr == "" || yStr == "" {
		return nil, fmt.Errorf("unsupported jwk (crv=%q)", crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(xStr)
	if err != nil {
		return nil, fmt.Errorf("bad x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(yStr)
	if err != nil {
		return nil, fmt.Errorf("bad y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}
