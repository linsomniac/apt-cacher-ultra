package admin

import (
	"bytes"
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
	"sync"
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

func withLogger(logger *slog.Logger) adminOpt {
	return func(cfg *Config) {
		cfg.Logger = logger
	}
}

func withTLSMITM(p TLSMITMProvider) adminOpt {
	return func(cfg *Config) {
		cfg.TLSMITM = p
	}
}

// stubTLSMITMProvider is a fixed-value TLSMITMProvider for tests.
// Returning the same snapshot on every call mirrors the production
// path's "stable inputs / live signals" split — tests compose the
// snapshot by hand.
type stubTLSMITMProvider struct{ snap TLSMITMSnapshot }

func (s *stubTLSMITMProvider) TLSMITMSnapshot() TLSMITMSnapshot { return s.snap }

// lockedBuffer is a sync.Mutex-guarded bytes.Buffer for use as a
// slog handler's io.Writer. The text handler can serialize concurrent
// records, so the underlying writer must accept concurrent Writes;
// bytes.Buffer alone does not.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json...", ct)
	}
}

func TestStatusJSON_HasLockedSchemaKeys(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, body)
	}
	// SPEC5 §10.5: top-level keys are stable. `gc` is also always
	// present — when no GC run has completed, the abbreviated form
	// {"last_run_unixtime": null} renders so JSON consumers see the
	// schema key reliably (asserted further below).
	required := []string{
		"process", "cache", "listeners", "suites",
		"hot_url_paths", "recent_adoptions", "active_hosts", "gc",
		"tls_mitm",
		"repo_coverage", // SPEC6_5 §2.4: top-level key always present
		"keyring",       // loaded GPG keys; empty list when adoption is disabled or keyring is empty
	}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON missing required top-level key %q", k)
		}
	}
	// SPEC5 §10.5 / §11: pre-first-run gc shape is exactly
	// {"last_run_unixtime": null} — last_run_unixtime present and
	// JSON-null, every other gc.* field omitted. The empty-cache
	// test exercises this branch.
	gcMap, ok := got["gc"].(map[string]any)
	if !ok {
		t.Fatalf("gc is not an object: %T", got["gc"])
	}
	if v, present := gcMap["last_run_unixtime"]; !present {
		t.Errorf("gc.last_run_unixtime missing; want JSON null")
	} else if v != nil {
		t.Errorf("gc.last_run_unixtime = %v, want JSON null (no GC run yet)", v)
	}
	for k := range gcMap {
		if k != "last_run_unixtime" {
			t.Errorf("gc has unexpected key %q before first run; spec mandates abbreviated shape", k)
		}
	}

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
		"recent_adoptions", "active_hosts", "listeners", "keyring"} {
		v := got[k]
		if v == nil {
			t.Errorf("%q is null; want [] (encoding/json renders empty slices as []) — schema says arrays are always arrays", k)
		}
	}
}

// TestStatusJSON_KeyringPopulates confirms the keyring section
// surfaces fingerprint + UID + source attribution when a
// KeyringProvider is wired into admin.Config.
func TestStatusJSON_KeyringPopulates(t *testing.T) {
	provider := &stubKeyringProvider{
		entries: []KeyringEntrySnapshot{
			{
				PrimaryFingerprint: "F6ECB3762474EDA9D21B7022871920D1991BC93C",
				PrimaryUID:         "Ubuntu Archive Automatic Signing Key (2018) <ftpmaster@ubuntu.com>",
				SourcePath:         "embedded:ubuntu-archive-keyring.gpg",
				SubkeyFingerprints: nil,
			},
			{
				PrimaryFingerprint: "AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555",
				PrimaryUID:         "Test Subkey Key <t@example.com>",
				SourcePath:         "/etc/apt/keyrings/test.gpg",
				SubkeyFingerprints: []string{"FFFF9999AAAA8888BBBB7777CCCC6666DDDD5555"},
			},
		},
	}
	_, base, cleanup := startAdminServer(t, func(cfg *Config) { cfg.Keyring = provider })
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	kr, ok := got["keyring"].([]any)
	if !ok {
		t.Fatalf("keyring is not a JSON array: %T", got["keyring"])
	}
	if len(kr) != 2 {
		t.Fatalf("keyring length = %d, want 2", len(kr))
	}
	// Entries are sorted by primary fingerprint — AAAA… comes
	// before F6EC….
	first, _ := kr[0].(map[string]any)
	if got, want := first["primary_fingerprint"], "AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555"; got != want {
		t.Errorf("first entry primary_fingerprint = %v, want %v", got, want)
	}
	if got := first["source_path"]; got != "/etc/apt/keyrings/test.gpg" {
		t.Errorf("first entry source_path = %v", got)
	}
	subs, _ := first["subkey_fingerprints"].([]any)
	if len(subs) != 1 || subs[0] != "FFFF9999AAAA8888BBBB7777CCCC6666DDDD5555" {
		t.Errorf("first entry subkey_fingerprints = %v", subs)
	}
	second, _ := kr[1].(map[string]any)
	if got, want := second["source_path"], "embedded:ubuntu-archive-keyring.gpg"; got != want {
		t.Errorf("second entry source_path = %v, want %v", got, want)
	}
	// Entries with no subkeys still emit an empty array (schema:
	// subkey_fingerprints is always present).
	secondSubs, ok := second["subkey_fingerprints"].([]any)
	if !ok {
		t.Errorf("second entry subkey_fingerprints not present or wrong type: %T", second["subkey_fingerprints"])
	} else if len(secondSubs) != 0 {
		t.Errorf("second entry subkey_fingerprints = %v, want []", secondSubs)
	}
}

// stubKeyringProvider is a fixed-value KeyringProvider for tests.
type stubKeyringProvider struct {
	entries []KeyringEntrySnapshot
}

func (s *stubKeyringProvider) KeyringSnapshot() []KeyringEntrySnapshot {
	return s.entries
}

// TestStatusJSON_CachePopulatesAfterSeed verifies the cache.* block
// reflects real DB row counts, not the Go zero-value (codex review
// finding 1: status page would otherwise mislead operators with
// blob_count=0 etc.).
func TestStatusJSON_CachePopulatesAfterSeed(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Seed two blobs by writing pool/ files + PutBlob row.
	for i, body := range []string{"first blob body", "second"} {
		w, err := s.cfg.Cache.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob[%d]: %v", i, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		hash, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatalf("Finalize[%d]: %v", i, err)
		}
		if err := s.cfg.Cache.PutBlob(context.Background(), hash, int64(len(body))); err != nil {
			t.Fatalf("PutBlob[%d]: %v", i, err)
		}
	}

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Cache struct {
			BlobCount           int64 `json:"blob_count"`
			BytesUsed           int64 `json:"bytes_used"`
			URLPathCount        int64 `json:"url_path_count"`
			ZeroRefcountBacklog int64 `json:"zero_refcount_backlog"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if got.Cache.BlobCount != 2 {
		t.Errorf("blob_count = %d, want 2", got.Cache.BlobCount)
	}
	if got.Cache.BytesUsed != int64(len("first blob body")+len("second")) {
		t.Errorf("bytes_used = %d, want %d", got.Cache.BytesUsed,
			len("first blob body")+len("second"))
	}
	// Both blobs are at refcount=0 with refcount_zeroed_at set
	// (PutBlob's default), so they are in the zero-refcount backlog.
	if got.Cache.ZeroRefcountBacklog != 2 {
		t.Errorf("zero_refcount_backlog = %d, want 2", got.Cache.ZeroRefcountBacklog)
	}
}

func TestStatusHTML_RendersWithoutPanic(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
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
	// The process StartedUnixTime is always non-zero, so a <time
	// …data-unix="…"…> element must appear at minimum for it.
	if !strings.Contains(string(body), `data-unix="`) || !strings.Contains(string(body), `<time `) {
		t.Errorf("HTML missing <time …data-unix=…> markup for client-side local-time rewrite")
	}
	// The inline script that rewrites <time> elements must be present.
	if !strings.Contains(string(body), "Intl.DateTimeFormat") {
		t.Errorf("HTML missing inline local-time rewrite script")
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
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("query ?format=json should win over Accept: text/html. Content-Type = %q", ct)
	}
}

func TestEndpoint_UnknownPath404(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/unknown")
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 405 {
		t.Errorf("POST /metrics status = %d, want 405", resp.StatusCode)
	}
	// SPEC5 §9.7.1 / §12.2: Allow header MUST list GET, HEAD,
	// OPTIONS — the 405 is meaningless to a client that doesn't
	// know which methods are accepted.
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, OPTIONS")
	}
}

func TestEndpoint_OptionsAnyPath204(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	// SPEC5 §9.7.1: OPTIONS on any path returns 204 with the Allow
	// header. Exercise both a known and an unknown path so the
	// catch-all behavior is pinned.
	for _, path := range []string{"/metrics", "/healthz", "/", "/nonexistent"} {
		req, err := http.NewRequest(http.MethodOptions, base+path, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Errorf("OPTIONS %s status = %d, want 204", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, OPTIONS" {
			t.Errorf("OPTIONS %s Allow = %q, want %q", path, got, "GET, HEAD, OPTIONS")
		}
	}
}

func TestAuth_Disabled_AllRequestsSucceed(t *testing.T) {
	_, base, cleanup := startAdminServer(t) // no htpasswd
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_Enabled_NoCredentials401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := mustGet(t, base+"/metrics")
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("valid-creds status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_Enabled_WrongPassword401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := getWithBasic(t, base+"/metrics", "alice", "wrong")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 {
		t.Errorf("wrong-pass status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_Enabled_UnknownUser401(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := getWithBasic(t, base+"/metrics", "bob", "anything")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 {
		t.Errorf("unknown-user status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_HealthzAlsoRequiresAuth(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	resp := mustGet(t, base+"/healthz")
	defer func() { _ = resp.Body.Close() }()
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

// TestStatusHTML_SuiteTable pins the suite-table column header
// rename (Lagging → InRelease changed) AND the SPEC5 §12.2.4
// lagging annotation rendering. Seeds a suite with
// inrelease_change_seen_at well past last_success_at so the
// "(lagging …)" muted suffix renders in the InRelease-changed cell.
func TestStatusHTML_SuiteTable(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Seed a lagging suite: last successful re-adoption was 2 hours
	// ago, but upstream's InRelease has been seen changing 30 minutes
	// ago — gap = 1h 30m.
	now := time.Now().Unix()
	lastSuccess := now - 2*3600
	seenAt := now - 30*60
	if err := s.cfg.Cache.PutSuiteFreshness(context.Background(),
		cache.SuiteFreshness{
			CanonicalScheme:       "http",
			CanonicalHost:         "archive.ubuntu.com",
			SuitePath:             "/ubuntu/dists/noble",
			LastSuccessAt:         &lastSuccess,
			InReleaseChangeSeenAt: &seenAt,
		}); err != nil {
		t.Fatalf("PutSuiteFreshness: %v", err)
	}

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	if !strings.Contains(html, "<th>InRelease changed</th>") {
		t.Errorf("HTML missing <th>InRelease changed</th>; suite table header drifted")
	}
	if strings.Contains(html, "<th>Lagging</th>") {
		t.Errorf("HTML still has <th>Lagging</th> — header rename did not stick")
	}
	if !strings.Contains(html, "(lagging 1h 30m)") {
		t.Errorf("HTML missing lagging annotation '(lagging 1h 30m)'; body:\n%s", html)
	}
}

// TestLaggingAnnotation pins SPEC5 §12.2.4 lagging-render rules:
// nil inputs and seenAt<=successAt produce empty; the gap is
// formatted as "Xh Ym" or just "Xm" when zero hours.
func TestLaggingAnnotation(t *testing.T) {
	mk := func(unix int64) *int64 { return &unix }
	cases := []struct {
		name    string
		seen    *int64
		success *int64
		want    string
	}{
		{"both_nil", nil, nil, ""},
		{"seen_nil", nil, mk(100), ""},
		{"success_nil", mk(100), nil, ""},
		{"in_sync", mk(100), mk(100), ""},
		{"adopted_after_seen", mk(100), mk(200), ""},
		{"lag_30m", mk(2000), mk(200), "(lagging 30m)"},
		{"lag_1h_5m", mk(3900), mk(0), "(lagging 1h 5m)"},
		{"lag_25h", mk(90000), mk(0), "(lagging 25h 0m)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := laggingAnnotation(tc.seen, tc.success); got != tc.want {
				t.Errorf("laggingAnnotation(%v, %v) = %q, want %q",
					tc.seen, tc.success, got, tc.want)
			}
		})
	}
}

// TestAdminRequest_AuthUserAndScrapeID pins the SPEC5 §10.1
// admin_request log fields:
//   - auth_user is empty when auth is disabled.
//   - auth_user carries the authenticated username on success.
//   - scrape_id is present on every request (random uint64).
//
// Exercises the per-request *reqState pointer plumbing the auth
// middleware uses to surface the username to the outer logger.
func TestAdminRequest_AuthUserAndScrapeID(t *testing.T) {
	t.Run("disabled_auth_empty_user", func(t *testing.T) {
		var buf lockedBuffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		_, base, cleanup := startAdminServer(t, withLogger(logger))
		defer cleanup()

		resp := mustGet(t, base+"/healthz")
		_ = resp.Body.Close()

		out := buf.String()
		if !strings.Contains(out, "msg=admin_request") {
			t.Fatalf("admin_request log line missing; got:\n%s", out)
		}
		if !strings.Contains(out, `auth_user=""`) {
			t.Errorf("auth_user should be empty when auth disabled; log:\n%s", out)
		}
		if !strings.Contains(out, "scrape_id=") {
			t.Errorf("scrape_id missing from admin_request line; log:\n%s", out)
		}
	})

	t.Run("authenticated_user_propagated", func(t *testing.T) {
		var buf lockedBuffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		htp := makeBcryptHtpasswd(t, "alice", "secret")
		_, base, cleanup := startAdminServer(t,
			withHtpasswd(htp), withLogger(logger))
		defer cleanup()

		resp := getWithBasic(t, base+"/healthz", "alice", "secret")
		_ = resp.Body.Close()

		out := buf.String()
		if !strings.Contains(out, "auth_user=alice") {
			t.Errorf("auth_user=alice should appear after successful auth; log:\n%s", out)
		}
	})

	t.Run("auth_failure_leaves_user_empty", func(t *testing.T) {
		var buf lockedBuffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		htp := makeBcryptHtpasswd(t, "alice", "secret")
		_, base, cleanup := startAdminServer(t,
			withHtpasswd(htp), withLogger(logger))
		defer cleanup()

		resp := mustGet(t, base+"/healthz") // no creds
		_ = resp.Body.Close()

		out := buf.String()
		if !strings.Contains(out, "msg=admin_request") {
			t.Fatalf("admin_request log line missing; got:\n%s", out)
		}
		if !strings.Contains(out, `auth_user=""`) {
			t.Errorf("auth_user should be empty when auth fails; log:\n%s", out)
		}
	})
}

// TestEndpoint_Options_AuthEnabled pins SPEC5 §9.7.1 + §9.7.5: the
// auth middleware wraps the dispatcher, so OPTIONS without
// credentials returns 401 and OPTIONS with valid credentials
// returns 204 + Allow. Without this pinning, an order swap (auth
// inside dispatcher instead of around it) would silently allow
// unauthenticated probes to fingerprint the listener.
func TestEndpoint_Options_AuthEnabled(t *testing.T) {
	htp := makeBcryptHtpasswd(t, "alice", "secret")
	_, base, cleanup := startAdminServer(t, withHtpasswd(htp))
	defer cleanup()

	// OPTIONS without creds → 401.
	for _, path := range []string{"/metrics", "/healthz", "/", "/nonexistent"} {
		req, _ := http.NewRequest(http.MethodOptions, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS %s no-creds: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("OPTIONS %s no-creds = %d, want 401", path, resp.StatusCode)
		}
	}

	// OPTIONS with valid creds → 204 + Allow.
	for _, path := range []string{"/metrics", "/healthz", "/", "/nonexistent"} {
		req, _ := http.NewRequest(http.MethodOptions, base+path, nil)
		req.Header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS %s authed: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Errorf("OPTIONS %s authed = %d, want 204", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, OPTIONS" {
			t.Errorf("OPTIONS %s authed Allow = %q, want %q",
				path, got, "GET, HEAD, OPTIONS")
		}
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

// TestStatusJSON_TLSMITM_Disabled pins SPEC6 §10.4: the JSON
// top-level `tls_mitm` key is ALWAYS present, abbreviated to
// `{"enabled": false}` when MITM is disabled (or no provider was
// supplied — both paths converge on the same shape).
func TestStatusJSON_TLSMITM_Disabled(t *testing.T) {
	cases := []struct {
		name string
		opt  adminOpt
	}{
		{"nil_provider", nil},
		{"explicit_disabled", withTLSMITM(&stubTLSMITMProvider{snap: TLSMITMSnapshot{Enabled: false}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []adminOpt
			if tc.opt != nil {
				opts = []adminOpt{tc.opt}
			}
			_, base, cleanup := startAdminServer(t, opts...)
			defer cleanup()

			resp := mustGet(t, base+"/?format=json")
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v\nbody: %s", err, body)
			}
			tm, ok := got["tls_mitm"].(map[string]any)
			if !ok {
				t.Fatalf("tls_mitm is not an object: %T", got["tls_mitm"])
			}
			if v, present := tm["enabled"]; !present {
				t.Errorf("tls_mitm.enabled missing")
			} else if v != false {
				t.Errorf("tls_mitm.enabled = %v, want false", v)
			}
			// Abbreviated shape: ONLY the "enabled" key.
			for k := range tm {
				if k != "enabled" {
					t.Errorf("tls_mitm has unexpected key %q in disabled mode; spec mandates abbreviated shape", k)
				}
			}
		})
	}
}

// TestStatusJSON_TLSMITM_Enabled_FullPayload checks the full
// SPEC6 §10.4 JSON payload: all fields present, last_cert_issued
// renders as a sub-object when populated, hit-rate percent is a
// float.
func TestStatusJSON_TLSMITM_Enabled_FullPayload(t *testing.T) {
	issuedAt := time.Unix(1_730_000_000, 0)
	snap := TLSMITMSnapshot{
		Enabled:             true,
		CASource:            "generated",
		CAFingerprintSHA256: "abc123",
		CANotAfterUnixTime:  1_900_000_000,
		EffectiveAllowlist:  `^archive\.ubuntu\.com$`,
		CertCacheSize:       4,
		CertCacheCapacity:   256,
		LastIssuedHost:      "archive.ubuntu.com",
		LastIssuedAt:        issuedAt,
		HitRate60sHits:      9,
		HitRate60sMisses:    1,
	}
	_, base, cleanup := startAdminServer(t, withTLSMITM(&stubTLSMITMProvider{snap: snap}))
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		TLSMITM struct {
			Enabled             bool   `json:"enabled"`
			CASource            string `json:"ca_source"`
			CAFingerprintSHA256 string `json:"ca_fingerprint_sha256"`
			CANotAfterUnixTime  int64  `json:"ca_not_after_unixtime"`
			EffectiveAllowlist  string `json:"effective_allowlist"`
			CertCache           struct {
				Size     int `json:"size"`
				Capacity int `json:"capacity"`
			} `json:"cert_cache"`
			LastIssued *struct {
				Host       string `json:"host"`
				AtUnixTime int64  `json:"at_unixtime"`
			} `json:"last_cert_issued"`
			HitRate60sPercent  *float64 `json:"cert_hit_rate_60s_percent"`
			HitRate60sObserved int      `json:"cert_hit_rate_60s_observed"`
		} `json:"tls_mitm"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	tm := got.TLSMITM
	if !tm.Enabled {
		t.Errorf("tls_mitm.enabled = false, want true")
	}
	if tm.CASource != "generated" {
		t.Errorf("ca_source = %q, want %q", tm.CASource, "generated")
	}
	if tm.CAFingerprintSHA256 != "abc123" {
		t.Errorf("ca_fingerprint_sha256 = %q", tm.CAFingerprintSHA256)
	}
	if tm.CANotAfterUnixTime != 1_900_000_000 {
		t.Errorf("ca_not_after_unixtime = %d", tm.CANotAfterUnixTime)
	}
	if tm.EffectiveAllowlist != `^archive\.ubuntu\.com$` {
		t.Errorf("effective_allowlist = %q", tm.EffectiveAllowlist)
	}
	if tm.CertCache.Size != 4 || tm.CertCache.Capacity != 256 {
		t.Errorf("cert_cache = %+v", tm.CertCache)
	}
	if tm.LastIssued == nil {
		t.Fatalf("last_cert_issued = nil; want populated")
	}
	if tm.LastIssued.Host != "archive.ubuntu.com" {
		t.Errorf("last_cert_issued.host = %q", tm.LastIssued.Host)
	}
	if tm.LastIssued.AtUnixTime != issuedAt.Unix() {
		t.Errorf("last_cert_issued.at_unixtime = %d, want %d", tm.LastIssued.AtUnixTime, issuedAt.Unix())
	}
	if tm.HitRate60sPercent == nil {
		t.Fatalf("cert_hit_rate_60s_percent = nil; want 90.0")
	}
	if *tm.HitRate60sPercent != 90.0 {
		t.Errorf("cert_hit_rate_60s_percent = %v, want 90.0", *tm.HitRate60sPercent)
	}
	if tm.HitRate60sObserved != 10 {
		t.Errorf("cert_hit_rate_60s_observed = %d, want 10", tm.HitRate60sObserved)
	}
}

// TestStatusJSON_TLSMITM_Enabled_NoIssuance pins the populated-but-empty
// edge cases: enabled, but no cert issued yet AND no lookups in the
// 60s window. last_cert_issued must be JSON null (consumers
// distinguish "no issuance" from "issuance recorded"), hit-rate
// percent must be JSON null (distinguishing "no data" from "0%").
func TestStatusJSON_TLSMITM_Enabled_NoIssuance(t *testing.T) {
	snap := TLSMITMSnapshot{
		Enabled:             true,
		CASource:            "supplied",
		CAFingerprintSHA256: "deadbeef",
		CANotAfterUnixTime:  1_900_000_000,
		EffectiveAllowlist:  "",
		CertCacheSize:       0,
		CertCacheCapacity:   128,
		// LastIssuedHost / LastIssuedAt zero; no lookups recorded.
	}
	_, base, cleanup := startAdminServer(t, withTLSMITM(&stubTLSMITMProvider{snap: snap}))
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	tm := got["tls_mitm"].(map[string]any)
	if v, ok := tm["last_cert_issued"]; !ok {
		t.Errorf("last_cert_issued missing; want present-but-null")
	} else if v != nil {
		t.Errorf("last_cert_issued = %v, want JSON null", v)
	}
	if v, ok := tm["cert_hit_rate_60s_percent"]; !ok {
		t.Errorf("cert_hit_rate_60s_percent missing; want present-but-null")
	} else if v != nil {
		t.Errorf("cert_hit_rate_60s_percent = %v, want JSON null", v)
	}
	if v, ok := tm["cert_hit_rate_60s_observed"]; !ok {
		t.Errorf("cert_hit_rate_60s_observed missing")
	} else if vf, _ := v.(float64); int(vf) != 0 {
		t.Errorf("cert_hit_rate_60s_observed = %v, want 0", v)
	}
}

// TestStatusJSON_TLSMITM_TopLevelKeyAlwaysPresent extends the
// SPEC5 §10.5 locked-keys assertion with the SPEC6 §10.4
// invariant that `tls_mitm` is among the always-present top-level
// keys.
func TestStatusJSON_TLSMITM_TopLevelKeyAlwaysPresent(t *testing.T) {
	_, base, cleanup := startAdminServer(t) // no provider
	defer cleanup()

	resp := mustGet(t, base+"/?format=json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if _, ok := got["tls_mitm"]; !ok {
		t.Errorf("tls_mitm top-level key missing; SPEC6 §10.4 mandates always-present")
	}
}

// TestAcceptsGzip pins RFC 9110 §12.5.3 q-value semantics: q=0
// suppresses an otherwise-supported coding. Codex-review iter-7
// caught the earlier naive token check that gzipped on
// `Accept-Encoding: gzip;q=0` and broke standards-compliant
// clients that explicitly disabled gzip.
func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip", true},
		{"GZIP", true},   // case-insensitive
		{" gzip ", true}, // whitespace tolerated
		{"gzip;q=1", true},
		{"gzip;q=1.0", true},
		{"gzip;q=0.1", true},
		{"gzip;q=0", false},
		{"gzip;q=0.0", false},
		{"gzip;q=0.00", false},
		{"gzip;q=0.000", false},
		{"br, gzip;q=0", false},
		{"br, gzip;q=0.5", true},
		{"br", false},
		{"identity", false},
		{"identity;q=1, gzip;q=0", false}, // explicit reject
		{"gzip; q=1", true},               // space after semicolon
	}
	for _, c := range cases {
		c := c
		t.Run(c.header, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "http://example/", nil)
			if c.header != "" {
				req.Header.Set("Accept-Encoding", c.header)
			}
			if got := acceptsGzip(req); got != c.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// TestStatusHTML_VaryAlwaysEmitted: cache-correctness regression
// guard. The response carries Vary: Accept-Encoding on BOTH the
// identity and gzipped branches so an intermediary cache cannot
// serve a stored identity response to a later gzip-accepting
// client (or vice versa). Codex-review iter-7 caught the
// gzip-only emission.
func TestStatusHTML_VaryAlwaysEmitted(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	tr := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: tr}
	for _, header := range []string{"gzip", "identity", ""} {
		t.Run("ae="+header, func(t *testing.T) {
			req, _ := http.NewRequest("GET", base+"/", nil)
			if header != "" {
				req.Header.Set("Accept-Encoding", header)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
				t.Errorf("Accept-Encoding=%q: Vary = %q, want substring 'Accept-Encoding'", header, got)
			}
		})
	}
}

// TestStatusHTML_GzipSuppressedByQZero exercises the standards-
// compliant suppression: client explicitly disables gzip even
// though it lists the coding. Server must emit identity.
func TestStatusHTML_GzipSuppressedByQZero(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	tr := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (gzip;q=0)", got)
	}
}

// TestStatusJSON_GzipsWhenAccepted pins that the JSON branch also
// honours Accept-Encoding: gzip, per docs/admin-ui-spec.md §12's
// gzip mechanism description. The encoded body decodes to the
// same payload TestJSONContractPreserved pins — Content-Encoding
// is orthogonal to the §0.1 byte-identical-JSON contract.
func TestStatusJSON_GzipsWhenAccepted(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	tr := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", base+"/?format=json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("JSON body not gzipped; first bytes: % x", body[:min(len(body), 8)])
	}
}

// TestStatusHTML_GzipsWhenAccepted pins the §12 "over the wire"
// budget: the admin status response is gzip-encoded when the client
// sends Accept-Encoding: gzip. Without server-side gzip, the
// representative healthy render is ~41KB raw vs ~10KB gzipped — the
// budget enforcement at TestRenderSizeBudget assumes the latter, so
// the production server must actually emit Content-Encoding: gzip
// for the test's gzipped-byte assertion to map to operator reality.
func TestStatusHTML_GzipsWhenAccepted(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	// Build a request that explicitly disables transparent
	// decompression so we can observe the wire bytes.
	tr := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: tr}
	req, err := http.NewRequest("GET", base+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want substring %q", got, "Accept-Encoding")
	}
	body, _ := io.ReadAll(resp.Body)
	// gzip member starts with 0x1f 0x8b.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("body does not look gzipped; first bytes: % x", body[:min(len(body), 8)])
	}
}

// TestStatusHTML_NoGzipWhenNotAccepted is the negative of the above:
// clients that do not advertise gzip get identity.
func TestStatusHTML_NoGzipWhenNotAccepted(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	tr := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", base+"/", nil)
	// No Accept-Encoding header sent.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (no Accept-Encoding sent)", got)
	}
}

// TestStatusHTML_TLSMITM_SectionRendersWhenEnabled checks the HTML
// status page renders the SPEC6 §10.4 TLS MITM block when MITM is
// enabled. The redesign (docs/admin-ui-spec.md) places the MITM
// content inside the Plumbing panel under an <h3> rather than the
// previous top-level <h2>; the data is still visible per its own
// labelled rows. CA fingerprint is now chunked via chunkHex (groups
// of 4 hex separated by space).
func TestStatusHTML_TLSMITM_SectionRendersWhenEnabled(t *testing.T) {
	issuedAt := time.Unix(1_730_000_000, 0)
	snap := TLSMITMSnapshot{
		Enabled:             true,
		CASource:            "generated",
		CAFingerprintSHA256: "deadbeefcafebabe",
		CANotAfterUnixTime:  1_900_000_000,
		EffectiveAllowlist:  `^archive\.ubuntu\.com$`,
		CertCacheSize:       3,
		CertCacheCapacity:   256,
		LastIssuedHost:      "archive.ubuntu.com",
		LastIssuedAt:        issuedAt,
		HitRate60sHits:      4,
		HitRate60sMisses:    1,
	}
	_, base, cleanup := startAdminServer(t, withTLSMITM(&stubTLSMITMProvider{snap: snap}))
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The redesigned layout exposes MITM under the Plumbing panel, with
	// an <h3>TLS MITM</h3> heading, not <h2>.
	if !strings.Contains(html, "TLS MITM") {
		t.Errorf("HTML missing TLS MITM heading; body:\n%s", html)
	}
	for _, want := range []string{
		"dead beef cafe babe", // CA fingerprint, now chunkHex'd into 4-hex groups
		"archive.ubuntu.com",  // last cert issued host
		"3 / 256",             // cert cache
		"^archive",            // effective allowlist (escaped to ^archive in HTML)
		"80.0% (5 lookups)",   // hit rate
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q; body:\n%s", want, html)
		}
	}
}

// TestStatusHTML_TLSMITM_SectionOmittedWhenDisabled pins the spec
// rule "When tls_mitm.enabled = false, the section is omitted".
func TestStatusHTML_TLSMITM_SectionOmittedWhenDisabled(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The Plumbing panel header always reads "Listeners, TLS MITM,
	// build" per mockup, so a bare "TLS MITM" substring isn't a clean
	// signal. The MITM detail block ships the "CA fingerprint
	// (SHA-256)" key only when Enabled — that's the unique tripwire
	// for whether the MITM kv table actually rendered.
	if strings.Contains(html, "CA fingerprint (SHA-256)") {
		t.Errorf("HTML rendered TLS MITM detail block even though disabled")
	}
}

// TestStatusHTML_TLSMITM_NoIssuanceShowsPlaceholder checks the
// "no cert issued yet" branch renders the muted placeholder
// rather than panicking on a nil last-issued.
func TestStatusHTML_TLSMITM_NoIssuanceShowsPlaceholder(t *testing.T) {
	snap := TLSMITMSnapshot{
		Enabled:             true,
		CASource:            "generated",
		CAFingerprintSHA256: "x",
		CANotAfterUnixTime:  1_900_000_000,
		CertCacheCapacity:   128,
	}
	_, base, cleanup := startAdminServer(t, withTLSMITM(&stubTLSMITMProvider{snap: snap}))
	defer cleanup()

	resp := mustGet(t, base+"/")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	if !strings.Contains(html, "(none yet)") {
		t.Errorf("HTML missing '(none yet)' placeholder; body:\n%s", html)
	}
	if !strings.Contains(html, "n/a (no lookups in window)") {
		t.Errorf("HTML missing 'n/a (no lookups in window)' for hit rate; body:\n%s", html)
	}
	if !strings.Contains(html, "(none — vacuously true)") {
		t.Errorf("HTML missing empty-allowlist placeholder; body:\n%s", html)
	}
}

func TestFormatHitRatePercent(t *testing.T) {
	cases := []struct {
		name     string
		pct      *float64
		observed int
		want     string
	}{
		{"nil_pct_no_lookups", nil, 0, "n/a (no lookups in window)"},
		{"zero_pct_with_misses", float64Ptr(0), 5, "0.0% (5 lookups)"},
		{"hundred_pct", float64Ptr(100), 7, "100.0% (7 lookups)"},
		{"fractional", float64Ptr(87.5), 8, "87.5% (8 lookups)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHitRatePercent(tc.pct, tc.observed); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func float64Ptr(v float64) *float64 { return &v }

// TestFormatUnixTimeTag pins the <time> element shape used by the
// inline browser-local-time rewrite. Zero → empty (no element);
// non-zero → a closed <time> tag carrying datetime, data-unix, and
// a title attribute, plus the UTC fallback text content.
func TestFormatUnixTimeTag(t *testing.T) {
	if got := formatUnixTimeTag(0); got != "" {
		t.Errorf("formatUnixTimeTag(0) = %q, want empty", got)
	}
	// 1715443200 = 2024-05-11 16:00:00 UTC.
	const unix = int64(1715443200)
	got := string(formatUnixTimeTag(unix))
	wants := []string{
		`<time `,
		`data-unix="1715443200"`,
		`datetime="2024-05-11T16:00:00Z"`,
		`title="2024-05-11 16:00:00 UTC"`,
		`>2024-05-11 16:00:00 UTC</time>`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("formatUnixTimeTag(%d) missing %q; got: %s", unix, w, got)
		}
	}
}

func TestFormatUnixTimePtrTag(t *testing.T) {
	if got := formatUnixTimePtrTag(nil); got != "-" {
		t.Errorf("formatUnixTimePtrTag(nil) = %q, want %q", got, "-")
	}
	u := int64(1715443200)
	got := string(formatUnixTimePtrTag(&u))
	if !strings.Contains(got, `data-unix="1715443200"`) {
		t.Errorf("formatUnixTimePtrTag(&u) missing data-unix attribute; got: %s", got)
	}
}
