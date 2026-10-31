package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// mintedFlatUpstream is mintedUpstream for flat repositories: a root
// Packages.gz answers gzip magic too, so every flat suite file passes its
// sniff.
type mintedFlatUpstream struct{ mintedUpstream }

func (u mintedFlatUpstream) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if !strings.HasSuffix(target.URL, "/Packages.gz") {
		return u.mintedUpstream.Fetch(ctx, target, dst)
	}
	body := append([]byte{0x1f, 0x8b, 0x08, 0x00}, target.URL...)
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// Spec §9's G row with phase D's minted-suites DoD (Review Focus 1) for
// flat repositories — a sibling simulation, so TestServeHTTP_MintedSuitesAtScale
// keeps its 4.5 s budget (ruling R-G5, deferred minor F-12): minted flat
// directories on many hosts — each a suite and its ./ twin, with their
// InRelease, Release.gpg, root Packages.gz and by-hash names, the cap
// splitting each pair — from an
// upstream that answers every one with sniff-passing bytes create rows
// only up to the caps (2 per host, 20 in all — freshness.max_suites 20,
// a tenth of it per host: ten hosts fill theirs, the eleventh is refused
// at the global cap), a marker exactly for the bases
// of tracked suites (a suite and its twin share one), and no lock-map,
// flip-gate or touch-map entry beyond the tracked suites; the rejection
// cache holds the refused suites only.
//
// The simulation captures the checker's, the adopter's and the handler's
// WARN and ERROR lines and must run pristine: no recovered panic, and no
// such line but the caps' suite_seed_rejected.
//
// The counts are exact, as phase D's sibling's are, only while two things
// hold: the bubble's time never advances, so the flip gate's idle sweep
// never runs; and every check answers 304 to a probe that carried no
// validators — an unknown answer, a failed check — so nothing adopts and
// no anchorless refresh runs (it needs both anchors answered absent). An
// edit that advances time or answers 404 changes the gate's count or
// starts anchorless refreshes.
func TestServeHTTP_MintedFlatSuitesAtScale(t *testing.T) {
	synctest.Test(t, testMintedFlatSuitesAtScale)
}

func testMintedFlatSuitesAtScale(t *testing.T) {
	c := simtest.OpenCache(t)
	ctx := context.Background()
	logs := &syncLog{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	gate, err := flipgate.New(flipgate.Config{Quiet: flipgate.MaxQuiet})
	if err != nil {
		t.Fatal(err)
	}
	up := &simtest.StaticConditional{Status: http.StatusNotModified}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: &memberStore{bodies: map[string][]byte{}},
		Verifier: plainVerifier{}, HostLimiter: hostsem.New(64), Logger: logger, FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	const hosts, dirs, perHostCap, maxSuites = 11, 2, 2, 20
	ck, err := freshness.New(freshness.Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(64),
		Logger: logger, MaxSuites: maxSuites, Adopter: ad})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: mintedFlatUpstream{}, Logger: logger,
		HostLimiter: hostsem.New(64), Freshness: ck, AdoptionEnabled: true, FlipGate: ad.FlipGate(),
		MaxSuites: maxSuites, SuiteGuard: ck})
	if err != nil {
		t.Fatal(err)
	}
	closeH := sync.OnceFunc(h.Close)
	t.Cleanup(closeH)
	byHash := "/by-hash/SHA256/" + strings.Repeat("ab", 32)
	// The first directory's twin comes before its suite, the second's
	// suite before its twin, so the per-host cap tracks one of each pair:
	// the twin's marker must land on the base it shares.
	order := []string{"/repo000/.", "/repo001", "/repo000", "/repo001/."}
	for i := range hosts {
		for _, suite := range order {
			base := fmt.Sprintf("http://f%02d.example%s", i, suite)
			for _, p := range []string{"/InRelease", "/Release.gpg", "/Packages.gz", byHash, "/InRelease"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+p, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s%s = %d", base, p, rec.Code)
				}
			}
		}
	}
	closeH() // drains the T1 checks
	all, err := c.ListSuites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	rowsOf := map[string]int{}
	for _, s := range all {
		if !aptpath.IsFlatSuitePath(s.SuitePath) {
			t.Fatalf("row %s%s is no flat suite", s.CanonicalHost, s.SuitePath)
		}
		tracked[s.CanonicalHost+s.SuitePath] = true
		rowsOf[s.CanonicalHost]++
	}
	if len(all) != maxSuites {
		t.Fatalf("rows = %d, want %d (the global cap)", len(all), maxSuites)
	}
	if !tracked["f00.example/repo000/."] || tracked["f00.example/repo000"] {
		t.Fatalf("f00: tracked %v; want the first directory's twin, not its suite", tracked)
	}
	for i := range hosts - 1 {
		if n := rowsOf[fmt.Sprintf("f%02d.example", i)]; n != perHostCap {
			t.Fatalf("f%02d: %d rows, want the per-host cap %d", i, n, perHostCap)
		}
	}
	if n := rowsOf[fmt.Sprintf("f%02d.example", hosts-1)]; n != 0 {
		t.Fatalf("f%02d: %d rows, want 0 (refused at the global cap)", hosts-1, n)
	}
	for i := range hosts {
		for j := range dirs {
			host, suite := fmt.Sprintf("f%02d.example", i), fmt.Sprintf("/repo%03d", j)
			_, ok, err := c.SignedSeen(ctx, "http", host, aptpath.MarkerBase(suite))
			if err != nil {
				t.Fatal(err)
			}
			if want := tracked[host+suite] || tracked[host+suite+"/."]; ok != want {
				t.Fatalf("%s%s: marker %v, want %v (a marker exactly for a tracked suite's base)", host, suite, ok, want)
			}
		}
	}
	if n := ck.LockCount(); n != len(all) {
		t.Errorf("lock map = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n := gate.Len(); n != len(all) {
		t.Errorf("flip gate = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n := c.TouchedSuites(); n != len(all) {
		t.Errorf("touch map = %d entries, want one per tracked suite (%d)", n, len(all))
	}
	if n, want := h.rejections.len(), (hosts-1)*(2*dirs-perHostCap)+2*dirs; n != want {
		t.Errorf("rejection cache = %d entries, want the %d refused suites", n, want)
	}
	// Pristine (the final review's tests lens M-3): no recovered panic —
	// freshness_check_panicked, adoption_panicked and
	// handler_goroutine_panicked are ERROR lines — and no WARN or ERROR line
	// but suite_seed_rejected, the caps' refusal this simulation makes by
	// design (rate-limited: the bubble's time never advances).
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] != "suite_seed_rejected" {
			t.Errorf("unexpected %v line %v: the simulation must run pristine", m["level"], m["msg"])
		}
	}
}
