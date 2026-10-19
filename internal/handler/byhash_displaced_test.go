package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const dispSuite = "/ubuntu/dists/noble"

// syncLog is a log sink safe for the handler's concurrent writers.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// countingUpstream answers 404 and counts the fetches it is asked for.
type countingUpstream struct {
	mu sync.Mutex
	n  int
}

func (*countingUpstream) HostAllowed(string) bool { return true }

func (u *countingUpstream) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	return nil, &fetch.StatusError{Code: 404}
}

func (u *countingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// newDisplacedHandler builds a handler whose retained by-hash searches
// keep+1 displaced snapshots, logging to logs when it is not nil.
func newDisplacedHandler(t *testing.T, keep int, logs *syncLog) (*Handler, *countingUpstream) {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	logger := silentLogger()
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	up := &countingUpstream{}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: up, Logger: logger, HostLimiter: hostsem.New(4), KeepDisplaced: keep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, up
}

// adoptWithMembers adopts an inline snapshot of dispSuite whose members
// are its InRelease plus extra (path → blob content).
func adoptWithMembers(t *testing.T, h *Handler, label string, extra map[string]string) int64 {
	t.Helper()
	inrel := writeBlob(t, h, simtest.ClearsignedFixture("InRelease "+label))
	members := []cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}
	for p, content := range extra {
		b := writeBlob(t, h, []byte(content))
		members = append(members, cache.SnapshotMember{Path: p, BlobHash: b, DeclaredSHA256: b})
	}
	return commitInlineSnapshot(t, h, "http", "127.0.0.1", dispSuite, inrel, members, nil)
}

// aliasOf is content's SHA256 by-hash path under binary-amd64.
func aliasOf(content string) string {
	return "main/binary-amd64/by-hash/SHA256/" + sha256Hex([]byte(content))
}

func getMember(t *testing.T, h *Handler, member string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newProxyReqHostPort("GET", "http", "127.0.0.1", dispSuite+"/"+member))
	return rec
}

// Spec C8 retained by-hash: a by-hash member the current snapshot lacks
// is served from the displaced snapshot that declared it, with that
// snapshot's X-Cache-Snapshot, counted, logged as snapshot_displaced, and
// without asking upstream.
func TestServeHTTP_RetainedByHashServesTheDisplacedSnapshot(t *testing.T) {
	logs := &syncLog{}
	h, up := newDisplacedHandler(t, 3, logs)
	id1 := adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{aliasOf("Packages two"): "Packages two"})
	before := sumMetricLines(t, []string{"acu_serve_byhash_displaced_total"})["acu_serve_byhash_displaced_total"]
	rec := getMember(t, h, aliasOf("Packages one"))
	if rec.Code != http.StatusOK || rec.Body.String() != "Packages one" || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d %q from snapshot %q; want 200 \"Packages one\" from %d",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	after := sumMetricLines(t, []string{"acu_serve_byhash_displaced_total"})["acu_serve_byhash_displaced_total"]
	if after-before != 1 {
		t.Errorf("acu_serve_byhash_displaced_total rose by %v, want 1", after-before)
	}
	if !strings.Contains(logs.String(), `"snapshot_displaced":true`) {
		t.Errorf("no request line carries snapshot_displaced=true:\n%s", logs.String())
	}
	if n := up.count(); n != 0 {
		t.Errorf("upstream was asked %d times, want 0", n)
	}
}

// A by-hash alias directly under the suite root — the dists root's
// Contents-* aliases today, every flat suite's indexes from phase G — is
// matched by byHashMemberRE's ^ alternative and retained like any other
// (final review, spec M7).
func TestServeHTTP_RetainedByHashServesASuiteRootAlias(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	alias := "by-hash/SHA256/" + sha256Hex([]byte("Contents-amd64 one"))
	id1 := adoptWithMembers(t, h, "one", map[string]string{alias: "Contents-amd64 one"})
	adoptWithMembers(t, h, "two", nil)
	rec := getMember(t, h, alias)
	if rec.Code != http.StatusOK || rec.Body.String() != "Contents-amd64 one" || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d %q from snapshot %q; want 200 from %d", rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
}

// A SHA256 alias is served only when its blob is the hash the URL names,
// and a pool blob that no displaced snapshot declares is never served for
// an adopted suite (stray-blob negatives).
func TestServeHTTP_RetainedByHashSHA256MustMatchTheBlob(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	forged := "main/binary-amd64/by-hash/SHA256/" + strings.Repeat("ab", 32)
	adoptWithMembers(t, h, "one", map[string]string{forged: "bytes that hash to something else"})
	adoptWithMembers(t, h, "two", nil)
	if rec := getMember(t, h, forged); rec.Code != http.StatusNotFound {
		t.Errorf("an alias bound to another blob: %d, want 404", rec.Code)
	}
	stray := writeBlob(t, h, []byte("a stray blob"))
	if rec := getMember(t, h, "main/binary-amd64/by-hash/SHA256/"+stray); rec.Code != http.StatusNotFound {
		t.Errorf("a stray pool blob: %d, want 404", rec.Code)
	}
}

// A SHA512 alias relies on the snapshot_member binding verified at
// adoption (plan deviation 3: phase E's adoptions write these rows).
func TestServeHTTP_RetainedByHashSHA512UsesTheVerifiedBinding(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	alias := "main/binary-amd64/by-hash/SHA512/" + strings.Repeat("cd", 64)
	id1 := adoptWithMembers(t, h, "one", map[string]string{alias: "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	rec := getMember(t, h, alias)
	if rec.Code != http.StatusOK || rec.Body.String() != "Packages one" || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d %q from snapshot %q; want 200 from %d", rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
}

func TestServeHTTP_RetainedByHashPrefersTheNewestDisplacedSnapshot(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	shared := map[string]string{aliasOf("shared Packages"): "shared Packages"}
	adoptWithMembers(t, h, "one", shared)
	id2 := adoptWithMembers(t, h, "two", shared)
	adoptWithMembers(t, h, "three", nil)
	if got := getMember(t, h, aliasOf("shared Packages")).Header().Get(hdrXCacheSnapshot); got != itoa(id2) {
		t.Fatalf("served from snapshot %q, want the newest displaced one, %d", got, id2)
	}
}

// The search covers the newest keep_displaced + 1 displaced snapshots:
// what GC keeps, plus the one a flip displaced since GC last ran.
func TestServeHTTP_RetainedByHashSearchesKeepDisplacedPlusOne(t *testing.T) {
	h, _ := newDisplacedHandler(t, 1, nil) // searches two
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{aliasOf("Packages two"): "Packages two"})
	adoptWithMembers(t, h, "three", nil)
	adoptWithMembers(t, h, "four", nil) // displaced, newest first: three, two, one
	if rec := getMember(t, h, aliasOf("Packages two")); rec.Code != http.StatusOK {
		t.Errorf("the second-newest displaced snapshot's alias: %d, want 200", rec.Code)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Errorf("the third-newest displaced snapshot's alias: %d, want 404", rec.Code)
	}
}

func TestServeHTTP_RetainedByHashIsOffWhenKeepDisplacedIsZero(t *testing.T) {
	h, _ := newDisplacedHandler(t, 0, nil)
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (gc.keep_displaced = 0 disables retained by-hash)", rec.Code)
	}
}

func TestServeHTTP_RetainedByHashNeverServesRetiredSnapshots(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	id1 := adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if _, err := h.cache.ApplyLegacyAuditDecisions(context.Background(),
		[]cache.LegacyAuditDecision{{SnapshotID: id1, Outcome: cache.LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d from a retired snapshot, want 404", rec.Code)
	}
}

// No recovery fetch for displaced snapshots: a lost blob is a 404.
func TestServeHTTP_RetainedByHashLostBlobIs404WithoutAFetch(t *testing.T) {
	h, up := newDisplacedHandler(t, 3, nil)
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if err := os.Remove(h.cache.BlobPath(sha256Hex([]byte("Packages one")))); err != nil {
		t.Fatal(err)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("upstream was asked %d times, want 0", n)
	}
}

func TestNew_RejectsNegativeKeepDisplaced(t *testing.T) {
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := New(Config{Parser: parser, Cache: c, Fetch: &countingUpstream{}, Logger: silentLogger(),
		HostLimiter: hostsem.New(4), KeepDisplaced: -1}); err == nil {
		t.Fatal("New accepted KeepDisplaced = -1")
	}
}

// A newer displaced snapshot whose SHA256 alias row is bound to other bytes
// must not hide an older displaced snapshot that holds the real bytes.
func TestServeHTTP_RetainedByHashSkipsAMismatchedNewerRow(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	alias := aliasOf("Packages one")
	id1 := adoptWithMembers(t, h, "one", map[string]string{alias: "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{alias: "bytes that hash to something else"})
	adoptWithMembers(t, h, "three", nil)
	rec := getMember(t, h, alias)
	if rec.Code != http.StatusOK || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d from snapshot %q, want 200 from %d", rec.Code, rec.Header().Get(hdrXCacheSnapshot), id1)
	}
}
