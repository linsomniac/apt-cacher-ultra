package main

// SPEC6 §15 #16 claim 3 — graceful shutdown during a streaming
// cache-miss fetch must not leave an orphan blob in pool/.
//
// The §15 #16 line bundles three claims:
//
//   1. In-flight CONNECT tunnels drain on graceful shutdown.
//   2. No leaked goroutines after shutdown completes.
//   3. No orphan `pool/` blobs from cancelled inner GETs.
//
// Claims (1) and (2) are pinned by connect_shutdown_test.go's
// TestServe_GracefulShutdown_DrainsInflightCONNECT. The pool/ walk
// in that test is a regression guard, not a non-vacuous pin —
// nothing reaches the fetch pipeline (no TLS ClientHello, so no
// inner GET) and the comments at connect_shutdown_test.go:198-209
// explicitly call that out.
//
// Claim 3 — "no orphan blob from a cancelled inner GET" — depends on
// the SPEC2 atomic-finalize contract: a fetch cancelled mid-body
// must not promote its temp blob to pool/. This test pins THAT
// contract end-to-end via the cache's HTTP fetch path, which uses
// the same singleflight + temp/ → pool/ promotion pipeline as the
// MITM-tunneled inner-GET case (handler.serveCacheMiss → runFetch
// → fetch.Fetch → cache.PutBlob). The MITM-specific trigger
// (CONNECT + inner GET against an HTTPS upstream) requires
// privileged port-443 binding to exercise end-to-end; the underlying
// cancellation invariant is path-independent, so an HTTP-trigger
// test pins the same property at lower cost.
//
// Sequence:
//
//   1. Slow-body httptest.NewServer that flushes response headers
//      + a first byte chunk, then blocks until inbound r.Context
//      is cancelled. The cache's fetch reaches headers, starts
//      writing to a temp blob, and is then mid-body when shutdown
//      fires.
//   2. Cache booted with a Mirror rule pointing at the slow upstream.
//   3. Test client sends GET, reads the 200 status line (proves
//      bytes are flowing through the cache → temp blob path).
//   4. Test waits for upstream to confirm it's blocked on
//      r.Context().Done() (mid-stream window open).
//   5. Daemon ctx cancelled. h.lifecycleCtx fires; fetch's HTTP
//      transport closes the outbound conn; upstream's r.Context
//      returns from Done, handler returns, conn closes; cache's
//      fetch unwinds with a context error; temp-blob defer cleans
//      up the partial file before any rename to pool/ runs.
//   6. After serveListeners returns, walk pool/ and temp/.
//      Both must be empty.
//
// The corresponding connect_shutdown_test.go pool/ walk asserts the
// CONNECT pipeline by itself (cert generation, hijack accounting)
// never touches pool/. Together the two tests pin the orphan-blob
// invariant under both shutdown shapes the daemon can encounter.
//
// Mutates the package-level shutdownTimeout var, so NOT t.Parallel.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

func TestServe_GracefulShutdown_FetchMidStream_NoOrphanBlob(t *testing.T) {
	oldTimeout := shutdownTimeout
	shutdownTimeout = 500 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = oldTimeout })

	// First chunk size is small but non-zero so the cache's fetch
	// definitely enters its body-copy loop (and therefore opens
	// the temp blob) before the upstream blocks.
	firstChunk := make([]byte, 1024)
	for i := range firstChunk {
		firstChunk[i] = 'A'
	}

	var serverEnteredOnce sync.Once
	serverEntered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare a length the cache cannot satisfy with the
		// flushed bytes alone, so the fetch stays in the
		// streaming-body state when shutdown fires. (A short
		// declared length whose bytes we send fully would let
		// the fetch finalize successfully and the test would
		// race the cancellation against atomic-finalize.)
		w.Header().Set("Content-Length", "1048576")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(firstChunk); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		serverEnteredOnce.Do(func() { close(serverEntered) })
		// Block until the cache's outbound conn closes (which
		// fires when h.lifecycleCtx cancels and fetch's
		// transport tears down the conn).
		<-r.Context().Done()
	}))
	defer upstream.Close()

	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, []config.MirrorRule{
		{Prefix: "/test", Upstream: upstream.URL + "/"},
	})
	cfg.Upstream.ConnectTimeout.Duration = 2 * time.Second
	cfg.Upstream.TotalTimeout.Duration = 30 * time.Second
	cfg.Upstream.MaxRetries = 0

	cacheLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cacheAddr := cacheLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil)
	}()

	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}

	// Send the GET on a goroutine; the test main path waits for
	// the upstream to reach its blocked state, then cancels. The
	// client's Read will error out when shutdown closes the
	// underlying conn — we don't care about its exact body or
	// error, only that the fetch reached the mid-stream state.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get("http://" + cacheAddr + "/test/slow.bin")
		if err != nil {
			return
		}
		// Read at least the status line + headers + a few body
		// bytes via a bufio.Reader; do NOT call ReadAll since
		// the body never completes. We discard everything.
		buf := make([]byte, 4096)
		_, _ = bufio.NewReader(resp.Body).Read(buf)
		resp.Body.Close()
	}()

	// Wait for upstream to confirm it's blocked. Once this fires,
	// the cache has dialed, sent the GET, received the headers,
	// written the first chunk into its temp blob, and is now
	// blocked waiting for more body bytes.
	select {
	case <-serverEntered:
	case <-time.After(10 * time.Second):
		t.Fatalf("upstream handler never observed the GET; fetch never made it across (Mirror rule? gate config?)")
	}

	// --- Trigger graceful shutdown mid-stream ---
	shutdownStart := time.Now()
	cancel()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("serveListeners: %v", err)
		}
		// Drain budget 500ms + lifecycle-ctx cancel propagation
		// + http transport teardown. 5s is the regression
		// ceiling — TotalTimeout (30s) firing instead means the
		// lifecycle-ctx chain regressed and fetch outlived
		// shutdown.
		if dur := time.Since(shutdownStart); dur > 5*time.Second {
			t.Errorf("serveListeners returned in %v; expected sub-second after lifecycleCtx cancel", dur)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveListeners did not return after shutdown — leader fetch may have outlived lifecycle cancel")
	}

	// Wait for the client goroutine to finish so its conn close
	// races aren't mistaken for goroutine leaks elsewhere.
	<-clientDone

	// --- §15 #16 claim 3 assertion ---
	//
	// pool/ must be empty: atomic-finalize is the ONLY path that
	// can place a file under pool/, and it requires a successful
	// fetch (status 200, declared length matched). A cancelled
	// fetch never finalizes, so any blob present here proves the
	// rollback contract is broken.
	poolDir := filepath.Join(cacheDir, "pool")
	var poolFiles []string
	if err := filepath.Walk(poolDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		poolFiles = append(poolFiles, path)
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk pool/: %v", err)
	}
	if len(poolFiles) > 0 {
		t.Errorf("§15 #16 claim 3 violated: pool/ has %d file(s) after cancelled mid-stream fetch; want 0\nfiles: %v",
			len(poolFiles), poolFiles)
	}

	// temp/ must also be empty: fetch's deferred cleanup removes
	// the partial temp file when the body copy returns an error.
	// A leaked temp/ file would not be an "orphan" in the pool/
	// sense, but it is a related resource leak and would
	// foreshadow a regression where the partial temp accidentally
	// got renamed.
	tempDir := filepath.Join(cacheDir, "temp")
	var tempFiles []string
	if err := filepath.Walk(tempDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		tempFiles = append(tempFiles, path)
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Errorf("walk temp/: %v", err)
	}
	if len(tempFiles) > 0 {
		t.Errorf("temp/ has %d leaked file(s) after cancelled fetch; want 0\nfiles: %v",
			len(tempFiles), tempFiles)
	}
}
