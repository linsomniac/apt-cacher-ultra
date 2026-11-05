package acme

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/netutil"
)

const challengePrefix = "/.well-known/acme-challenge/"

var challengeToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`)

// Challenge owns a dedicated listener for its lifetime, avoiding renewal-time
// port races. It never serves an application handler or logs request paths.
type Challenge struct {
	mu        sync.RWMutex
	name      string
	tokens    map[string]string
	server    *http.Server
	done      chan struct{}
	err       chan error
	closeOnce sync.Once
}

func StartChallenge(ln net.Listener, name string) *Challenge {
	c := &Challenge{name: name, tokens: make(map[string]string), done: make(chan struct{}), err: make(chan error, 1)}
	c.server = &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192,
		ErrorLog: log.New(io.Discard, "", 0)}
	c.server.SetKeepAlivesEnabled(false)
	go func() {
		defer close(c.done)
		err := c.server.Serve(netutil.LimitListener(ln, 64))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.err <- errors.New("acme challenge listener failed")
		}
	}()
	return c
}

func (c *Challenge) Errors() <-chan error { return c.err }
func (c *Challenge) Close() {
	c.closeOnce.Do(func() { _ = c.server.Close(); <-c.done; c.clear() })
}
func (c *Challenge) clear() { c.mu.Lock(); clear(c.tokens); c.mu.Unlock() }
func (c *Challenge) set(token, response string) error {
	if !challengeToken.MatchString(token) || len(response) > 512 {
		return errors.New("invalid challenge")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[token] = response
	return nil
}

func (c *Challenge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, c.name) || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, challengePrefix) || r.URL.EscapedPath() != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, challengePrefix)
	c.mu.RLock()
	response, ok := c.tokens[token]
	c.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, response)
	}
}
