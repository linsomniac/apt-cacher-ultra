package acme

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// challengePathPrefix is the only path the challenge server serves
// (RFC 8555 §8.3). Every other path 404s: the transient listener must
// never expose any application interface.
const challengePathPrefix = "/.well-known/acme-challenge/"

// challengeServer is the standalone HTTP-01 challenge listener. It is
// started just before an issuance or renewal, serves only
// /.well-known/acme-challenge/<token> for the tokens registered during
// the current attempt, and is stopped right after the authority has
// finished the challenge. It never logs tokens or key authorizations.
type challengeServer struct {
	mu     sync.Mutex
	tokens map[string]string // token -> key authorization
	ln     net.Listener
	srv    *http.Server
}

func newChallengeServer() *challengeServer {
	return &challengeServer{tokens: make(map[string]string)}
}

// start binds addr and begins serving. A bind failure (address already in
// use — including by one of the daemon's own listeners) is an explicit
// error: the issuance attempt fails loudly instead of silently skipping
// the challenge.
func (s *challengeServer) start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("acme: HTTP-01 challenge listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.mu.Lock()
	s.ln = ln
	s.tokens = make(map[string]string)
	s.srv = srv
	s.mu.Unlock()
	// The goroutine uses the local srv/ln, never the struct fields:
	// stop() nils the fields under s.mu and must not race this read.
	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

// set registers the key authorization for a challenge token. Called
// before the authority is notified (Accept).
func (s *challengeServer) set(token, keyAuth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = keyAuth
}

// stop shuts the listener down immediately; in-flight challenge fetches
// are cut. Safe to call when not started.
func (s *challengeServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.ln = nil
	s.srv = nil
	s.tokens = make(map[string]string)
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// boundAddr reports the listener address once started (the effective
// address when :0 was requested), empty otherwise.
func (s *challengeServer) boundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *challengeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, challengePathPrefix) {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, challengePathPrefix)
	if token == "" || strings.ContainsAny(token, "/?#") {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	keyAuth, ok := s.tokens[token]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(keyAuth))
}
