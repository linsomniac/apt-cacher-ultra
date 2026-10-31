package main

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
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
	if _, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger()); !errors.Is(err, flipgate.ErrQuietOutOfRange) {
		t.Fatalf("buildAdopter(16s) = %v, want flipgate.ErrQuietOutOfRange (deferred minor C-31)", err)
	}
}

// Spec C8: the handler shares the adopter's gate — anchor serves and
// commits meet at one barrier — and retained by-hash searches what GC
// keeps. With no adopter (main builds none when adoption is disabled)
// there is no gate. gc.keep_displaced is wired whether or not GC runs
// (minimalCfg leaves gc.enabled false); 7 is not its default (3), so the
// value seen is the configured one (deferred minor C-32).
func TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	cfg.GC.KeepDisplaced = 7
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
	if hc.KeepDisplaced != 7 {
		t.Fatalf("KeepDisplaced = %d, want gc.keep_displaced (7)", hc.KeepDisplaced)
	}
	if hc := handlerConfig(cfg, parser, c, fetcher, hosts, silentBuildLogger(), nil, nil); hc.FlipGate != nil {
		t.Fatal("no adopter, yet the handler has a flip barrier")
	}
}
