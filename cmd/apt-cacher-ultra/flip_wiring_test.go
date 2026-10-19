package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Spec C8, C11: buildAdopter builds the flip barrier from
// adoption.flip_quiet_period (0s: disabled) and reports it; an
// out-of-range value (config validation normally stops it) is an error.
// Mutates keyringDirs, so NOT t.Parallel.
func TestBuildAdopter_BuildsTheFlipBarrier(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	for _, quiet := range []time.Duration{10 * time.Second, 0} {
		cfg := newAdoptionEnabledCfg(t, false)
		cfg.Adoption.FlipQuietPeriod.Duration = quiet
		c, fetcher, hosts := newAdoptionWiringDeps(t)
		logs := &syncLogBuffer{}
		a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewJSONHandler(logs, nil)))
		if err != nil {
			t.Fatalf("quiet %v: buildAdopter: %v", quiet, err)
		}
		if g := a.FlipGate(); g == nil || g.Quiet() != quiet || g.Enabled() != (quiet > 0) {
			t.Fatalf("quiet %v: gate quiet %v, enabled %v", quiet, g.Quiet(), g.Enabled())
		}
		if want := `"flip_quiet_period":` + strconv.FormatInt(int64(quiet), 10); !strings.Contains(logs.String(), want) {
			t.Errorf("quiet %v: the phase2 adoption enabled line lacks %s:\n%s", quiet, want, logs.String())
		}
	}
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 16 * time.Second
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	if _, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger()); err == nil {
		t.Fatal("buildAdopter accepted a 16s quiet period")
	}
}

// Spec C8: the handler shares the adopter's gate — anchor serves and
// commits meet at one barrier — and retained by-hash searches what GC
// keeps. With adoption disabled there is no gate.
func TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	cfg.GC.KeepDisplaced = 3
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := handlerConfig(cfg, parser, c, fetcher, hosts, silentBuildLogger(), nil, a)
	if hc.FlipGate == nil || hc.FlipGate != a.FlipGate() {
		t.Fatal("the handler does not share the adopter's flip barrier")
	}
	if hc.KeepDisplaced != 3 {
		t.Fatalf("KeepDisplaced = %d, want gc.keep_displaced (3)", hc.KeepDisplaced)
	}
	if hc := handlerConfig(cfg, parser, c, fetcher, hosts, silentBuildLogger(), nil, nil); hc.FlipGate != nil {
		t.Fatal("adoption disabled, yet the handler has a flip barrier")
	}
}

// Deferred minor B-64: a shutdown that waits on a running adoption commit
// says so once the wait outlasts the notice delay, and still returns
// exactly when the drain ends.
func TestWaitLogged_ExplainsASlowAdoptionDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLogBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		start := time.Now()
		waitLogged(func() { time.Sleep(time.Second) }, adoptionDrainNotice, logger)
		if strings.Contains(logs.String(), "shutdown_waiting_for_adoptions") {
			t.Fatalf("a 1s drain was logged:\n%s", logs.String())
		}
		waitLogged(func() { time.Sleep(time.Minute) }, adoptionDrainNotice, logger)
		if n := strings.Count(logs.String(), `"msg":"shutdown_waiting_for_adoptions"`); n != 1 {
			t.Fatalf("a 1m drain logged %d notices, want 1:\n%s", n, logs.String())
		}
		if d := time.Since(start); d != time.Second+time.Minute {
			t.Fatalf("waitLogged returned after %v in all, want when the drains ended (1m1s)", d)
		}
	})
}

// Deferred minor A-48: a failed legacy trust audit refuses the start with
// one "legacy trust audit:" prefix, not two.
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
