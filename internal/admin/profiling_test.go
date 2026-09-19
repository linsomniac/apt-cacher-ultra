package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func withProfiling(cfg *Config) { cfg.Admin.PprofEnabled = true }

func TestProfilingRoutesOptInAndPrivate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			s, _, cleanup := startAdminServer(t, func(cfg *Config) { cfg.Admin.PprofEnabled = enabled })
			defer cleanup()
			for _, path := range []string{"/debug/pprof/heap", "/debug/pprof/goroutine"} {
				rec := httptest.NewRecorder()
				s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if !enabled {
					if rec.Code != http.StatusNotFound {
						t.Fatalf("disabled %s: status %d", path, rec.Code)
					}
					continue
				}
				checkBinaryProfile(t, rec.Code, rec.Header(), rec.Body.Bytes())
			}
			for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/trace", "/debug/pprof/allocs"} {
				rec := httptest.NewRecorder()
				s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusNotFound {
					t.Errorf("unregistered %s: status %d", path, rec.Code)
				}
			}
			if !enabled {
				rec := httptest.NewRecorder()
				s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile", nil))
				if rec.Code != http.StatusNotFound {
					t.Errorf("disabled CPU profile: status %d", rec.Code)
				}
			}
		})
	}
	// Importing this implementation must not populate the global mux.
	for _, path := range []string{"/debug/pprof/profile", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		_, pattern := http.DefaultServeMux.Handler(httptest.NewRequest(http.MethodGet, path, nil))
		if pattern != "" {
			t.Errorf("global mux unexpectedly registered %q", pattern)
		}
	}
}

func TestProfilingRequiresGETAndAdminAuthentication(t *testing.T) {
	passwordFile := makeBcryptHtpasswd(t, "alice", "secret")
	s, _, cleanup := startAdminServer(t, withProfiling, withHtpasswd(passwordFile))
	defer cleanup()
	for _, path := range []string{"/debug/pprof/profile", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		rec := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s: status %d", path, rec.Code)
		}
		for _, method := range []string{http.MethodHead, http.MethodPost} {
			req := httptest.NewRequest(method, path, nil)
			req.SetBasicAuth("alice", "secret")
			rec = httptest.NewRecorder()
			s.server.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, OPTIONS" {
				t.Errorf("%s %s: status %d Allow %q", method, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine", nil)
	req.SetBasicAuth("alice", "secret")
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, req)
	checkBinaryProfile(t, rec.Code, rec.Header(), rec.Body.Bytes())
}

func TestCPUProfileDuration(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  time.Duration
	}{
		{"", 30 * time.Second}, {"?seconds=1", time.Second}, {"?seconds=300", 300 * time.Second},
		{"?seconds=", 0}, {"?seconds=0", 0}, {"?seconds=-1", 0}, {"?seconds=301", 0},
		{"?seconds=1.5", 0}, {"?seconds=abc", 0}, {"?seconds=999999999999999999999", 0},
		{"?seconds=1&seconds=2", 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := cpuProfileDuration(httptest.NewRequest(http.MethodGet, "/debug/pprof/profile"+tc.query, nil))
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Errorf("duration = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestCPUProfileInvalidDurationAndCapture(t *testing.T) {
	s, _, cleanup := startAdminServer(t, withProfiling)
	defer cleanup()
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=301", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid duration: status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
	checkBinaryProfile(t, rec.Code, rec.Header(), rec.Body.Bytes())
}

func TestCPUProfileCancellationAndExclusion(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "client_disconnect", true: "shutdown"}[shutdown], func(t *testing.T) {
			logs := &lockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s, base, cleanup := startAdminServer(t, withProfiling, withLogger(logger))
			defer cleanup()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/debug/pprof/profile?seconds=300", nil)
			if err != nil {
				t.Fatal(err)
			}
			type response struct {
				status int
				header http.Header
				body   []byte
				err    error
			}
			done := make(chan response, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- response{err: err}
					return
				}
				defer func() { _ = resp.Body.Close() }()
				body, err := io.ReadAll(resp.Body)
				done <- response{status: resp.StatusCode, header: resp.Header, body: body, err: err}
			}()
			waitForProfileLog(t, logs, "admin_cpu_profile_started")
			// A second capture must not stop or replace the active capture.
			rec := httptest.NewRecorder()
			s.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
			if rec.Code != http.StatusConflict {
				t.Fatalf("concurrent capture: status %d", rec.Code)
			}
			if shutdown {
				shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := s.Shutdown(shutdownCtx); err != nil {
					t.Fatalf("shutdown with 300s capture: %v", err)
				}
			} else {
				cancel()
			}
			select {
			case got := <-done:
				if shutdown {
					if got.err != nil {
						t.Fatal(got.err)
					}
					if got.status != http.StatusServiceUnavailable {
						t.Errorf("interrupted profile status = %d, want 503", got.status)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("300s capture did not stop after cancellation")
			}
			waitForProfileLog(t, logs, "admin_cpu_profile_finished")
		})
	}
}

func waitForProfileLog(t *testing.T, logs *lockedBuffer, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), message) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing %s: %s", message, logs.String())
}

func checkBinaryProfile(t *testing.T, status int, header http.Header, body []byte) {
	t.Helper()
	if status != http.StatusOK || header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("profile response: status=%d headers=%v body=%q", status, header, body)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Error("profile response is cacheable")
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("invalid gzip profile: %v", err)
	}
	defer func() { _ = zr.Close() }()
	data, err := io.ReadAll(zr)
	if err != nil || len(data) == 0 {
		t.Fatalf("invalid or empty profile payload: bytes=%d err=%v", len(data), err)
	}
}

// A nonreading diagnostic client leaves its HTTP write blocked. Simulate the
// connection's deadline behavior while passing through the logging wrapper.
type blockedProfileWriter struct {
	header   http.Header
	started  chan struct{}
	unblock  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
}

func (w *blockedProfileWriter) Header() http.Header { return w.header }
func (w *blockedProfileWriter) WriteHeader(int)     {}
func (w *blockedProfileWriter) Write([]byte) (int, error) {
	close(w.started)
	<-w.unblock
	return 0, os.ErrDeadlineExceeded
}
func (w *blockedProfileWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	if !deadline.After(time.Now()) {
		w.once.Do(func() { close(w.unblock) })
	}
	return nil
}

func TestProfileBlockedWriteInterrupted(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "client_disconnect", true: "shutdown"}[shutdown], func(t *testing.T) {
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			serverCtx, cancelServer := context.WithCancel(context.Background())
			defer cancelServer()
			s := &Server{profilingCtx: serverCtx, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			writer := &blockedProfileWriter{header: make(http.Header), started: make(chan struct{}), unblock: make(chan struct{})}
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil).WithContext(requestCtx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.writeProfile(&countingWriter{ResponseWriter: writer}, req, "heap", func(w io.Writer) error {
					_, err := w.Write([]byte("profile"))
					return err
				})
			}()
			select {
			case <-writer.started:
			case <-time.After(time.Second):
				t.Fatal("profile write did not start")
			}
			writer.mu.Lock()
			remaining := time.Until(writer.deadline)
			writer.mu.Unlock()
			if remaining <= 0 || remaining > 5*time.Second {
				t.Errorf("profile output deadline is not bounded to five seconds: %s", remaining)
			}
			if shutdown {
				cancelServer()
			} else {
				cancelRequest()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("canceled profile is blocked on a nonreading client")
			}
		})
	}
}

func TestProfileWriteCleanupJoinsRunningInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	stop := interruptProfileWrite(ctx, func() {
		close(started)
		<-release
	})
	cancel()
	<-started
	cleaned := make(chan struct{})
	go func() {
		stop()
		close(cleaned)
	}()
	select {
	case <-cleaned:
		t.Fatal("cleanup returned while deadline callback could still touch the connection")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after the deadline callback exited")
	}
}
