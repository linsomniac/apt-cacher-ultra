package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/gc"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"

	"golang.org/x/crypto/bcrypt"
)

// startAdminServer constructs an admin Server with a real cache
// and gc.GC, binds it to an ephemeral port, and returns the server
// + a base URL ("http://127.0.0.1:<port>") + a cleanup func.
func startAdminServer(t *testing.T, opts ...adminOpt) (*Server, string, func()) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	g, err := gc.New(gc.Config{
		Cache:               c,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled:             true,
		Interval:            time.Hour,
		BatchSize:           100,
		SnapshotBatchSize:   10,
		MaxTickDuration:     time.Minute,
		BlobGrace:           time.Hour,
		KeepDisplaced:       3,
		PoolScanWorkers:     2,
		HeartbeatStaleGrace: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("gc.New: %v", err)
	}

	cfg := Config{
		Cache:       c,
		GC:          g,
		HostLimiter: hostsem.New(8),
		Ring:        observability.NewRing(50),
		Registry:    metrics.NewRegistry(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BuildInfo: BuildInfo{
			Version:     "v0.test",
			GoVersion:   "go-test",
			VCSRevision: "deadbeef",
		},
		Admin: config.AdminConfig{
			Enabled:         true,
			GaugeRefresh:    config.Duration{Duration: 50 * time.Millisecond},
			ReadTimeout:     config.Duration{Duration: 5 * time.Second},
			IdleTimeout:     config.Duration{Duration: 30 * time.Second},
			MetricSeriesCap: 1024,
		},
		StartTime: time.Now(),
	}
	for _, o := range opts {
		o(&cfg)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.AdminAddr = ln.Addr().String()

	s, err := New(cfg)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("admin.New: %v", err)
	}

	go func() { _ = s.Serve(ln) }()

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		_ = c.Close()
	}
	return s, "http://" + cfg.AdminAddr, cleanup
}

type adminOpt func(*Config)

func withHtpasswd(path string) adminOpt {
	return func(cfg *Config) {
		cfg.Admin.HtpasswdFile = path
	}
}

// makeBcryptHtpasswd writes a temp htpasswd file with one user
// "alice" / password "secret" using bcrypt cost=4 (fast for tests
// — Apache's `htpasswd -B` defaults to 5; either works).
func makeBcryptHtpasswd(t *testing.T, user, pass string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), 4)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "htpasswd")
	content := user + ":" + string(hash) + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}
	return path
}

func TestEndpoint_Healthz_OK(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestEndpoint_Metrics_TextPlain(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8", ct)
	}
}

func TestEndpoint_Status_HTMLDefault(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
}

func TestEndpoint_Status_JSONViaQuery(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json...", ct)
	}
}

func TestStatusJSON_HasLockedSchemaKeys(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, body)
	}
	// SPEC5 §10.5: top-level keys are stable.
	required := []string{
		"process", "cache", "listeners", "suites",
		"hot_url_paths", "recent_adoptions", "active_hosts",
	}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON missing required top-level key %q", k)
		}
	}
	// gc is omitempty — present only when GC has run; an
	// empty-cache test means gc has not run yet, so absence is
	// expected. Don't assert presence.

	// Nested process keys.
	proc, ok := got["process"].(map[string]any)
	if !ok {
		t.Fatalf("process is not an object: %T", got["process"])
	}
	for _, k := range []string{"version", "started_unixtime",
		"uptime_seconds", "vcs_revision", "go_version"} {
		if _, ok := proc[k]; !ok {
			t.Errorf("process missing %q", k)
		}
	}

	// Empty arrays render as [] not null.
	for _, k := range []string{"suites", "hot_url_paths",
		"recent_adoptions", "active_hosts", "listeners"} {
		v := got[k]
		if v == nil {
			t.Errorf("%q is null; want [] (encoding/json renders empty slices as []) — schema says arrays are always arrays", k)
		}
	}
}

func TestStatusHTML_RendersWithoutPanic(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<title>apt-cacher-ultra status</title>") {
		t.Errorf("HTML missing title; body:\n%s", body)
	}
	if !strings.Contains(string(body), "View as JSON") {
		t.Errorf("HTML missing JSON link")
	}
	if !strings.Contains(string(body), "/metrics") {
		t.Errorf("HTML missing /metrics link")
	}
}

func TestEndpoint_Status_JSONOverridesAccept(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	req, _ := http.NewRequest("GET", base+"/?format=json", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("query ?format=json should win over Accept: text/html. Content-Type = %q", ct)
	}
}

func TestEndpoint_UnknownPath404(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/unknown")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 — /unknown must NOT match the / subtree (SPEC5 §9.7.1)", resp.StatusCode)
	}
}

func TestEndpoint_PostMetrics405(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp, err := http.Post(base+"/metrics", "text/plain", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST /metrics status = %d, want 405", resp.StatusCode)
	}
}

func TestAuth_Disabled_AllRequestsSucceed(t *testing.T) {
	_, base, cleanup := startAdminServer(t) // no htpasswd
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_Enabled_NoCredentials401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no-creds status = %d, want 401", resp.StatusCode)
	}
	if w := resp.Header.Get("WWW-Authenticate"); !strings.Contains(w, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want Basic realm=...", w)
	}
}

func TestAuth_Enabled_ValidCredentials200(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := getWithBasic(t, base+"/metrics", "alice", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("valid-creds status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_Enabled_WrongPassword401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := getWithBasic(t, base+"/metrics", "alice", "wrong")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong-pass status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_Enabled_UnknownUser401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := getWithBasic(t, base+"/metrics", "bob", "anything")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("unknown-user status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_HealthzAlsoRequiresAuth(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := mustGet(t, base+"/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("/healthz without auth = %d, want 401 (no carve-out per SPEC5 §9.7.4)", resp.StatusCode)
	}
}

func TestParseHtpasswd_RejectsApacheMD5(t *testing.T) {
	_, err := parseHtpasswd([]byte("alice:$apr1$abc$def\n"))
	if err == nil {
		t.Error("Apache MD5 ($apr1$) should be rejected")
	}
}

func TestParseHtpasswd_RejectsSHA1(t *testing.T) {
	_, err := parseHtpasswd([]byte("alice:{SHA}xyz=\n"))
	if err == nil {
		t.Error("SHA-1 ({SHA}) should be rejected")
	}
}

func TestParseHtpasswd_AcceptsAllBcryptVariants(t *testing.T) {
	cases := []string{
		"alice:$2a$04$abcdefghijklmnopqrstuvWxyZ0123456789ABCDEFGHIJ.\n",
		"bob:$2b$04$abcdefghijklmnopqrstuvWxyZ0123456789ABCDEFGHIJ.\n",
		"carol:$2y$04$abcdefghijklmnopqrstuvWxyZ0123456789ABCDEFGHIJ.\n",
	}
	for _, c := range cases {
		if _, err := parseHtpasswd([]byte(c)); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
}

func TestParseHtpasswd_SkipsCommentsAndBlanks(t *testing.T) {
	data := []byte(`# this is a comment

alice:$2a$04$abcdefghijklmnopqrstuvWxyZ0123456789ABCDEFGHIJ.
# another comment
`)
	users, err := parseHtpasswd(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := users["alice"]; !ok {
		t.Errorf("alice missing")
	}
	if len(users) != 1 {
		t.Errorf("got %d users, want 1", len(users))
	}
}

func TestParseHtpasswd_RejectsEmptyFile(t *testing.T) {
	_, err := parseHtpasswd([]byte("# only comments\n\n"))
	if err == nil {
		t.Error("empty htpasswd should be rejected")
	}
}

func TestIsNonLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6789", false},
		{"[::1]:6789", false},
		{"localhost:6789", false},
		{"0.0.0.0:6789", true},
		{":6789", true},
		{"10.0.0.5:6789", true},
		{"nonsense", true}, // err on the side of warning
	}
	for _, tc := range cases {
		if got := IsNonLoopback(tc.addr); got != tc.want {
			t.Errorf("IsNonLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// helpers

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func getWithBasic(t *testing.T, url, user, pass string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s with auth: %v", url, err)
	}
	return resp
}
