package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const disabledAdoptionSuite = "/r/dists/stable"

// Seed the persisted state of a successful adoption. The test exercises the
// configuration transition and HTTP serving; signature verification has its
// own tests. Also leave legacy URL rows, which must not resurrect stale bytes.
func seedDisabledAdoptionSnapshot(t *testing.T, dir, upstream string, anchor, index []byte) int64 {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, dir, silentBuildLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var members []cache.SnapshotMember
	var anchorHash string
	for path, body := range map[string][]byte{"InRelease": anchor, "main/binary-amd64/Packages": index} {
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		hash, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.PutBlob(ctx, hash, int64(len(body))); err != nil {
			t.Fatal(err)
		}
		if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
			Path: disabledAdoptionSuite + "/" + path, UpstreamURL: upstream + disabledAdoptionSuite + "/" + path,
			BlobHash: &hash, IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
		members = append(members, cache.SnapshotMember{Path: path, BlobHash: hash, DeclaredSHA256: hash})
		if path == "InRelease" {
			anchorHash = hash
		}
	}
	id, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: "127.0.0.1", SuitePath: disabledAdoptionSuite, InReleaseHash: &anchorHash})
	if err != nil {
		t.Fatal(err)
	}
	for i := range members {
		members[i].SnapshotID = id
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestServe_DisablingAdoptionForgetsSnapshotsBeforeServing(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	var revision atomic.Int32
	revision.Store(1)
	index := func(v int32) []byte {
		return []byte(fmt.Sprintf("Package: example\nVersion: %d\nArchitecture: amd64\n\n", v))
	}
	const sources = "Package: example\nDirectory: pool/main/e/example\n\n"
	anchor := func(v int32) []byte {
		validUntil := time.Now().Add(7 * 24 * time.Hour)
		if v == 1 {
			validUntil = time.Now().Add(-24 * time.Hour)
		}
		return simtest.ClearsignedFixture(fmt.Sprintf("Origin: example\nDate: Mon, 01 Jan 2024 00:00:00 UTC\nValid-Until: %s\nDescription: revision %d\nSHA256:\n %x %d main/binary-amd64/Packages\n %x %d main/source/Sources\n",
			validUntil.UTC().Format(time.RFC1123), v, sha256.Sum256(index(v)), len(index(v)), sha256.Sum256([]byte(sources)), len(sources)))
	}
	// Keep each revision byte-identical across requests, including its dates.
	anchors := map[int32][]byte{1: anchor(1), 2: anchor(2), 3: anchor(3)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case disabledAdoptionSuite + "/InRelease":
			_, _ = w.Write(anchors[revision.Load()])
		case disabledAdoptionSuite + "/main/binary-amd64/Packages":
			_, _ = w.Write(index(revision.Load()))
		case disabledAdoptionSuite + "/main/source/Sources":
			_, _ = io.WriteString(w, sources)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	id := seedDisabledAdoptionSnapshot(t, dir, upstream.URL, anchors[1], index(1))
	cfg := minimalCfg(dir, []config.MirrorRule{{Prefix: "/repo", Upstream: upstream.URL + "/"}})
	cfg.Adoption.Architectures = []string{"amd64"}
	cfg.Adoption.RequireSignature = true
	cfg.GC.Enabled = false // The transition must work without either GC or reaping.
	cfg.GC.SuiteIdleTTL.Duration = 0
	cfg.Freshness.PeriodicRefresh.Duration = 0
	cfg.Freshness.Cooldown.Duration = 10 * time.Millisecond
	oldETag := fmt.Sprintf(`"sha256-%x"`, sha256.Sum256(anchors[1]))
	for run := range 3 {
		cfg.Adoption.Enabled = run == 0
		if run == 1 {
			revision.Store(2)
		}
		baseline := metrics.Default.SnapshotNamesForTest()
		logs := &syncLogBuffer{}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {
			return serveListeners(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), ln, nil, nil, nil)
		})
		waitUsageReady(t, ln.Addr().String())
		base := "http://" + ln.Addr().String() + "/repo" + disabledAdoptionSuite
		client := &http.Client{Timeout: 5 * time.Second}
		get := func(path string, conditional bool) (*http.Response, string) {
			t.Helper()
			req, err := http.NewRequest(http.MethodGet, base+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if conditional {
				req.Header.Set("If-None-Match", oldETag)
				req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			return resp, string(body)
		}
		resp, body := get("/InRelease", run != 0)
		if resp.StatusCode != 200 || body != string(anchors[revision.Load()]) {
			t.Fatalf("run %d anchor: %d %q", run, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Cache-Snapshot"); (got != "") != cfg.Adoption.Enabled {
			t.Fatalf("run %d snapshot header %q", run, got)
		}
		if run == 2 && resp.Header.Get("X-Cache") != "HIT" {
			t.Fatal("another disabled restart discarded the now-unadopted cache")
		}
		resp, body = get("/main/source/Sources", false)
		if run == 0 {
			if resp.StatusCode != 404 {
				t.Fatalf("adopted snapshot's excluded source index: %d", resp.StatusCode)
			}
		} else {
			if resp.StatusCode != 200 || body != sources {
				t.Fatalf("disabled mode retained the architecture exclusion: %d %q", resp.StatusCode, body)
			}
			resp, body = get("/main/binary-amd64/Packages", false)
			if resp.StatusCode != 200 || body != string(index(revision.Load())) {
				t.Fatalf("stale Packages survived forget: %d %q", resp.StatusCode, body)
			}
		}
		if run == 1 {
			// After the one-time conversion, ordinary client-triggered checks
			// must continue to refresh a repository with adoption disabled.
			revision.Store(3)
			deadline := time.Now().Add(5 * time.Second)
			for {
				resp, body = get("/InRelease", true)
				if resp.StatusCode == 200 && body == string(anchors[3]) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("disabled suite stopped refreshing: %d %q\n%s", resp.StatusCode, body, logs.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			resp, body = get("/main/binary-amd64/Packages", false)
			if resp.StatusCode != 200 || body != string(index(3)) {
				t.Fatalf("changed Packages did not refresh: %d %q", resp.StatusCode, body)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("shutdown timeout")
		}
		metrics.Default.UnregisterAddedSinceForTest(baseline)
		if got := strings.Contains(logs.String(), `"msg":"adoption_disabled_suites_forgotten","suites":1`); got != (run == 1) {
			t.Fatalf("run %d unexpected forget summary:\n%s", run, logs.String())
		}
		c, err := cache.Open(context.Background(), dir, silentBuildLogger())
		if err != nil {
			t.Fatal(err)
		}
		s, readErr := c.GetSuiteSnapshot(context.Background(), id)
		_ = c.Close()
		if readErr != nil || (s.RetiredAt != nil) != (run > 0) {
			t.Fatalf("run %d snapshot retirement: %+v %v", run, s, readErr)
		}
	}
}

func TestServe_DisabledAdoptionForgetFailureStopsStartup(t *testing.T) {
	dir := t.TempDir()
	seedDisabledAdoptionSnapshot(t, dir, "http://127.0.0.1", simtest.ClearsignedFixture("Origin: example\nSHA256:\n"), []byte("old index"))
	db, err := sql.Open("sqlite", filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_startup_forget BEFORE DELETE ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected forget failure'); END`); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = serveListeners(ctx, minimalCfg(dir, nil), silentBuildLogger(), ln, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "forget adopted suites with adoption disabled") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup did not return the forget error: %v", err)
	}
}
