// Package health provides local readiness checks shared by the proxy and admin.
package health

import (
	"context"
	"net/http"
	"os"
	"time"
)

type Cache interface {
	Dir() string
	Ping(context.Context) error
}

// Check returns a stable failure code, or an empty string when ready. Ready
// must be concurrency-safe; it reports startup, shutdown and listener/TLS state.
type Check struct {
	Cache Cache
	Ready func() string
}

func (c Check) Failure(ctx context.Context) string {
	if c.Ready != nil {
		if reason := c.Ready(); reason != "" {
			return reason
		}
	}
	if err := writable(c.Cache.Dir()); err != nil {
		return "cache_dir"
	}
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := c.Cache.Ping(pingCtx); err != nil {
		return "db_ping"
	}
	// A shutdown or listener failure during the I/O must not produce a 200.
	if c.Ready != nil {
		return c.Ready()
	}
	return ""
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".acu-healthz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write([]byte{0}); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

// Respond exposes no configuration, credentials or underlying error strings.
func Respond(w http.ResponseWriter, r *http.Request, failure string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	status, body := http.StatusOK, "ok\n"
	if failure != "" {
		status, body = http.StatusServiceUnavailable, "degraded\n"
		w.Header().Set("X-Acu-Check-Failed", failure)
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// ProxyHandler reserves only origin-form /healthz. Absolute-form upstream
// requests (even for /healthz) and CONNECT still follow the proxy's policy.
func (c Check) ProxyHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.IsAbs() || r.URL.Host != "" || r.URL.Path != "/healthz" || r.Method == http.MethodConnect {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		Respond(w, r, c.Failure(r.Context()))
	})
}
