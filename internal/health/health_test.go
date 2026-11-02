package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type testCache struct {
	dir  string
	ping func(context.Context) error
}

func (c testCache) Dir() string { return c.dir }
func (c testCache) Ping(ctx context.Context) error {
	if c.ping != nil {
		return c.ping(ctx)
	}
	return nil
}

func TestProxyHealthRouting(t *testing.T) {
	check := Check{Cache: testCache{dir: t.TempDir()}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418) })
	for _, tc := range []struct {
		method, target string
		status         int
		body           string
	}{
		{"GET", "/healthz", 200, "ok\n"},
		{"HEAD", "/healthz", 200, ""},
		{"POST", "/healthz", 405, "method not allowed\n"},
		{"OPTIONS", "/healthz", 405, "method not allowed\n"},
		{"GET", "/healthz/", 418, ""},
		{"GET", "/repo/healthz", 418, ""},
		{"GET", "http://upstream.example/healthz", 418, ""},
		{"CONNECT", "upstream.example:443", 418, ""},
	} {
		t.Run(tc.method+tc.target, func(t *testing.T) {
			rr := httptest.NewRecorder()
			check.ProxyHandler(next).ServeHTTP(rr, httptest.NewRequest(tc.method, tc.target, nil))
			if rr.Code != tc.status || rr.Body.String() != tc.body {
				t.Fatalf("got %d %q", rr.Code, rr.Body.String())
			}
			if tc.status != 418 && rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable probe")
			}
			if tc.status == 405 && rr.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("missing Allow")
			}
		})
	}
}

func TestFailuresAndRecovery(t *testing.T) {
	dir := t.TempDir()
	check := Check{Cache: testCache{dir: dir}}
	state := "startup"
	check.Ready = func() string { return state }
	probe := func(want string) {
		t.Helper()
		rr := httptest.NewRecorder()
		check.ProxyHandler(http.NotFoundHandler()).ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
		status := 200
		if want != "" {
			status = 503
		}
		if rr.Code != status || rr.Header().Get("X-Acu-Check-Failed") != want {
			t.Fatalf("got %d %v, want %s", rr.Code, rr.Header(), want)
		}
	}
	probe("startup")
	state = ""
	probe("")
	check.Cache = testCache{dir: dir, ping: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("missing bounded DB check")
		}
		return errors.New("private database details")
	}}
	probe("db_ping")
	check.Cache = testCache{dir: dir, ping: func(context.Context) error { state = "shutdown"; return nil }}
	probe("shutdown") // shutdown during I/O must override a successful check
	state = ""
	check.Cache = testCache{dir: dir}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	probe("cache_dir")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	probe("")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe leaked files: %v %v", entries, err)
	}
}
