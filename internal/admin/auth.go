package admin

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

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
// (user, ok, reason). On success ok=true and reason="". On failure
// reason is "unknown_user" or "wrong_password" — surfaced to the
// metric path only, not the response (which is identical for both
// to preserve timing parity).
//
// AIDEV-NOTE: SPEC5 §10.4.8 splits auth_failures into
// unknown_user vs wrong_password. The reason MUST be derived from
// the same path that runs the bcrypt comparison so the metric label
// can never disagree with the actual code path; do not separately
// re-check user existence at the call site.
func (a *htpasswdAuthenticator) authenticate(user, pass string) (string, bool, string) {
	a.mu.RLock()
	hash, ok := a.users[user]
	a.mu.RUnlock()
	if !ok {
		// Fixed-time no-such-user path — sentinel comparison runs
		// the same bcrypt cost as a real user mismatch.
		_ = bcrypt.CompareHashAndPassword(a.sentinel, []byte(pass))
		return "", false, "unknown_user"
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)); err != nil {
		return "", false, "wrong_password"
	}
	return user, true, ""
}

// middleware wraps next with HTTP Basic auth. SPEC5 §9.7.5: 401 on
// missing/invalid credentials, with WWW-Authenticate; the success
// path mutates the per-request state (seeded by the outer
// request-log middleware) so the outer logger can read auth_user
// after ServeHTTP returns. A pointer-in-context is used because
// auth's `r.WithContext(ctx)` substitution is invisible at the
// outer scope; the pointed-at struct survives any context swap.
//
// onAuthFailure is invoked with the SPEC5 §10.4.8 reason label
// (`no_credentials`, `unknown_user`, `wrong_password`) before the
// 401 is written, so the caller can drive
// acu_admin_auth_failures_total without leaking the reason in the
// response body or headers (timing-parity is preserved either way —
// the bcrypt comparison runs in both no-such-user and
// wrong-password paths).
func (a *htpasswdAuthenticator) middleware(next http.Handler, onAuthFailure func(reason string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.reloadIfChanged()

		user, pass, ok := r.BasicAuth()
		if !ok {
			if onAuthFailure != nil {
				onAuthFailure("no_credentials")
			}
			a.unauthorized(w)
			return
		}
		authedUser, ok, reason := a.authenticate(user, pass)
		if !ok {
			if onAuthFailure != nil {
				onAuthFailure(reason)
			}
			a.unauthorized(w)
			return
		}
		if state, _ := r.Context().Value(reqStateKey{}).(*reqState); state != nil {
			state.authUser = authedUser
		}
		next.ServeHTTP(w, r)
	})
}

// unauthorized writes the SPEC5 §9.7.5 401 response with
// WWW-Authenticate. The response body is identical for every
// failure reason (timing parity); the operator-visible distinction
// flows through acu_admin_auth_failures_total instead.
func (a *htpasswdAuthenticator) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="apt-cacher-ultra admin"`)
	http.Error(w, "auth required", http.StatusUnauthorized)
}

// CountHtpasswdUsers reads path and returns the number of bcrypt
// users defined. Used by cmd at startup so the startup config-dump
// can report admin_htpasswd_users BEFORE admin.New parses the file
// for actual auth. Validation is identical to the request-time
// parse — Apache MD5, SHA-1, and crypt(3) hashes return an error,
// naming the offending line. The admin_authenticated Info line
// uses Server.UserCount() (post-admin.New parse) instead so a
// hypothetical mid-startup htpasswd swap surfaces as an admin.New
// failure rather than a stale "authenticated" log line.
func CountHtpasswdUsers(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	users, err := parseHtpasswd(data)
	if err != nil {
		return 0, err
	}
	return len(users), nil
}

// userCount returns the number of users currently in the parsed
// authenticator map. Read under the same RLock that authenticate()
// uses so a concurrent reload sees a consistent count.
func (a *htpasswdAuthenticator) userCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.users)
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
