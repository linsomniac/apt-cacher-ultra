package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

type syncLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Spec §6.1: the legacy trust audit runs at startup before the GC startup
// pass and before anything serves, with adoption disabled too.
// Mutates package-level shutdownTimeout, so NOT t.Parallel.
func TestServeListeners_LegacyAuditRunsBeforeGCAndServing(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })
	preMetrics := metrics.Default.SnapshotNamesForTest()

	ctx := context.Background()
	cacheDir := t.TempDir()
	// A pre-upgrade-shaped state built with the cache API: one current
	// inline snapshot whose stored InRelease is plain text, queued. Plain
	// text on purpose (spec §7 "Fixtures", ruling PD-2): it models a
	// legacy snapshot the audit must quarantine.
	c, err := cache.Open(ctx, cacheDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("Origin: x\nSHA256:\n " + strings.Repeat("a", 64) + " 1 main/binary-amd64/Packages\n")
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Dir(c.BlobPath(h)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.BlobPath(h), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.PutBlob(ctx, h, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	id, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// CommitAdoption marked the inline commit, as a v7 binary would. A
	// pre-v7 cache has no markers: start from none.
	if err := c.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := minimalCfg(cacheDir, nil)
	cfg.Adoption.Enabled = false
	cfg.GC.Enabled = true
	// config.Defaults() leaves the [gc] tunables zero (Load() fills them),
	// and gc.New rejects zero tunables when GC is enabled: mirror Load().
	cfg.GC.Interval.Duration = 24 * time.Hour
	cfg.GC.BatchSize = 100
	cfg.GC.SnapshotBatchSize = 10
	cfg.GC.MaxTickDuration.Duration = 5 * time.Minute
	cfg.GC.BlobGrace.Duration = 5 * time.Minute
	cfg.GC.PoolScanWorkers = 4
	logs := &syncLogBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveListeners(serveCtx, cfg, logger, cacheLn, nil, nil, nil) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("serveListeners: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners did not return")
		}
		metrics.Default.UnregisterAddedSinceForTest(preMetrics)
	}
	t.Cleanup(stop)
	if err := waitForDaemonReady(t, cacheLn.Addr().String(), 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}
	out := logs.String()
	audit := strings.Index(out, `"msg":"legacy_trust_audit_completed"`)
	gcRun := strings.Index(out, `"msg":"gc_run_complete"`)
	switch {
	case audit < 0:
		t.Fatalf("no legacy_trust_audit_completed line:\n%s", out)
	case !strings.Contains(out[audit:], `"quarantined":1`):
		t.Fatalf("audit did not quarantine the plain-text snapshot:\n%s", out[audit:])
	case gcRun < 0:
		t.Fatalf("no gc_run_complete line from the GC startup pass:\n%s", out)
	case gcRun < audit:
		t.Fatalf("the GC startup pass ran before the legacy trust audit")
	}
	stop()
	c2, err := cache.Open(ctx, cacheDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if sf, err := c2.GetSuiteFreshness(ctx, "http", "h", "/r/dists/x"); err != nil || sf.CurrentSnapshotID != nil {
		t.Fatalf("suite still has a current snapshot after startup: %+v (%v)", sf, err)
	}
}

// Deferred minor A-48: a failed legacy trust audit refuses the start with
// one "legacy trust audit:" prefix, not two. (Moved from
// flip_wiring_test.go: deferred minor C-34.)
func TestServeListeners_LegacyAuditErrorHasOnePrefix(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })
	ctx := context.Background()
	cacheDir := t.TempDir()
	c, err := cache.Open(ctx, cacheDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A queued inline snapshot whose anchor is not in the (empty) pool:
	// the audit's pre-flight refuses the start.
	h := strings.Repeat("ab", 32)
	if err := c.PutBlob(ctx, h, 1); err != nil {
		t.Fatal(err)
	}
	id, _, err := c.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = serveListeners(ctx, minimalCfg(cacheDir, nil), silentBuildLogger(), ln, nil, nil, nil)
	if err == nil {
		t.Fatal("serveListeners started with the pool's anchors missing")
	}
	if n := strings.Count(err.Error(), "legacy trust audit:"); n != 1 {
		t.Fatalf("the error carries the prefix %d times: %v", n, err)
	}
}
