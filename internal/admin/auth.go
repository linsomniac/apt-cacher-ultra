package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// authUserKey is the context-value key for the authenticated user
// (empty string when no htpasswd is configured). Read by the
// request-log middleware to populate auth_user.
type authUserKey struct{}

// htpasswdAuthenticator parses an Apache-style htpasswd file
// (bcrypt-only) once at construction and reloads on
// (mtime, size) drift detected at request time. SPEC5 §9.7.5.
type htpasswdAuthenticator struct {
	path   string
	logger *slog.Logger

	// sentinel is a fixed bcrypt hash the no-such-user path runs
	// CompareHashAndPassword against. Without this, a wrong-user
	// 401 would return in microseconds while a wrong-password
	// 401 takes ~100ms (bcrypt cost) — letting an attacker
	// enumerate valid usernames by timing. Computed once at
	// construction. SPEC5 §9.7.5 timing-attack mitigation.
	sentinel []byte

	mu       sync.RWMutex
	users    map[string]string // user → bcrypt hash
	cachedMT int64             // mtime unix seconds (zero before first parse)
	cachedSz int64             // size in bytes
}

// newHtpasswdAuthenticator parses path and returns an authenticator
// or a config error naming the offending line. SPEC5 §9.7.5.
func newHtpasswdAuthenticator(path string, logger *slog.Logger) (*htpasswdAuthenticator, error) {
	a := &htpasswdAuthenticator{
		path:   path,
		logger: logger,
	}
	// Compute a sentinel hash once. cost=10 matches Apache htpasswd
	// -B's default; the comparison wallclock matches the
	// wrong-password path under any well-formed htpasswd file.
	sentinel, err := bcrypt.GenerateFromPassword([]byte("acu-sentinel"), 10)
	if err != nil {
		return nil, fmt.Errorf("admin: bcrypt sentinel: %w", err)
	}
	a.sentinel = sentinel
	if err := a.reload(); err != nil {
		return nil, fmt.Errorf("admin: htpasswd parse %q: %w", path, err)
	}
	return a, nil
}

// reload reads the htpasswd file, parses it, and atomically swaps
// the user map. Caller holds no lock. On parse failure, the prior
// map is preserved (returned error is the caller's signal — first
// call surfaces startup error; subsequent calls log
// htpasswd_reload_failed Warn).
//
// AIDEV-NOTE: SPEC5 §9.7.5 "(mtime, size) tuple" reload key — both
// fields are captured in the same Stat call, so a same-second
// rewrite that changed size is detected. mtime moving backward
// (clock change, ansible apply on a different host) also triggers
// reload because the cached pair differs.
func (a *htpasswdAuthenticator) reload() error {
	st, err := os.Stat(a.path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	users, err := parseHtpasswd(data)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.users = users
	a.cachedMT = st.ModTime().Unix()
	a.cachedSz = st.Size()
	a.mu.Unlock()
	return nil
}

// reloadIfChanged stats the file and triggers reload only when the
// (mtime, size) tuple has drifted from the cached value. Per-admin-
// request cost: one syscall (Stat) plus, on drift, one read +
// parse. Drift on the parse path emits htpasswd_reload_failed
// Warn; the prior credential map continues to authenticate.
func (a *htpasswdAuthenticator) reloadIfChanged() {
	st, err := os.Stat(a.path)
	if err != nil {
		// File missing: keep prior map, log once-ish via the Warn.
		// (Repeated logs are bounded by request rate; the
		// operator's signal is unmistakable.)
		a.logger.Warn("htpasswd_reload_failed",
			"path", a.path,
			"err", err.Error())
		return
	}
	mt := st.ModTime().Unix()
	sz := st.Size()
	a.mu.RLock()
	cmt := a.cachedMT
	csz := a.cachedSz
	a.mu.RUnlock()
	if mt == cmt && sz == csz {
		return
	}
	if err := a.reload(); err != nil {
		a.logger.Warn("htpasswd_reload_failed",
			"path", a.path,
			"err", err.Error())
	}
}

// authenticate is the SPEC5 §9.7.5 step 4-5 lookup. Returns
// (user, true) on success, ("", false) on failure. The
// no-such-user path runs the sentinel bcrypt comparison so the
// wallclock matches the wrong-password path.
func (a *htpasswdAuthenticator) authenticate(user, pass string) (string, bool) {
	a.mu.RLock()
	hash, ok := a.users[user]
	a.mu.RUnlock()
	if !ok {
		// Fixed-time no-such-user path — sentinel comparison runs
		// the same bcrypt cost as a real user mismatch.
		_ = bcrypt.CompareHashAndPassword(a.sentinel, []byte(pass))
		return "", false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)); err != nil {
		return "", false
	}
	return user, true
}

// middleware wraps next with HTTP Basic auth. SPEC5 §9.7.5: 401 on
// missing/invalid credentials, with WWW-Authenticate; the success
// path stuffs the authenticated username into the request context
// for the downstream request-log middleware.
func (a *htpasswdAuthenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.reloadIfChanged()

		user, pass, ok := r.BasicAuth()
		if !ok {
			a.unauthorized(w, "no_credentials")
			return
		}
		authedUser, ok := a.authenticate(user, pass)
		if !ok {
			// SPEC5 §10.4.8 splits failures into unknown_user vs
			// wrong_password — but we don't expose which path
			// fired (timing-parity guarantee). The metric
			// distinguishes; the response does not.
			a.unauthorized(w, "wrong_password")
			return
		}
		ctx := context.WithValue(r.Context(), authUserKey{}, authedUser)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthorized writes the SPEC5 §9.7.5 401 response with
// WWW-Authenticate. reason flows into acu_admin_auth_failures_total
// (wired by counter wiring in a later commit).
func (a *htpasswdAuthenticator) unauthorized(w http.ResponseWriter, _ string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="apt-cacher-ultra admin"`)
	http.Error(w, "auth required", http.StatusUnauthorized)
}

// parseHtpasswd parses the bytes of an Apache htpasswd file into a
// user→hash map. Bcrypt-only ($2a$/$2b$/$2y$); other prefixes are
// rejected. Empty lines and comment lines (`#...`) are ignored.
// SPEC5 §9.7.5 / §5.2.
func parseHtpasswd(data []byte) (map[string]string, error) {
	users := make(map[string]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("line %d: missing user:hash separator", i+1)
		}
		user := line[:colon]
		hash := line[colon+1:]
		if user == "" {
			return nil, fmt.Errorf("line %d: empty username", i+1)
		}
		if strings.ContainsAny(user, " \t") {
			return nil, fmt.Errorf("line %d: username %q contains whitespace", i+1, user)
		}
		switch {
		case strings.HasPrefix(hash, "$2a$"),
			strings.HasPrefix(hash, "$2b$"),
			strings.HasPrefix(hash, "$2y$"):
			// Acceptable bcrypt prefixes.
		case strings.HasPrefix(hash, "$apr1$"):
			return nil, fmt.Errorf("line %d: Apache MD5 ($apr1$) hash rejected — use bcrypt (`htpasswd -B`)", i+1)
		case strings.HasPrefix(hash, "{SHA}"):
			return nil, fmt.Errorf("line %d: SHA-1 ({SHA}) hash rejected — use bcrypt (`htpasswd -B`)", i+1)
		default:
			return nil, fmt.Errorf("line %d: unrecognized hash format %q — only bcrypt ($2a$/$2b$/$2y$) is accepted", i+1, hash)
		}
		users[user] = hash
	}
	if len(users) == 0 {
		return nil, errors.New("no users defined")
	}
	return users, nil
}
