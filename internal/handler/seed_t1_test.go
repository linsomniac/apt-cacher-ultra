package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Spec C3 T1-on-seed (phase D plan, design note 2; overview "E:"
// carry-forward): a miss that creates its suite's row fires T1 once its
// response is done, keyed on the creation — a miss on a suite that already
// has a row fires none — and only with adoption enabled.
func TestServeHTTP_MissThatCreatesItsSuiteFiresT1(t *testing.T) {
	for _, adoption := range []bool{true, false} {
		r := newSuiteRig(t, adoption, 0)
		r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
		r.serve("/r/dists/s/Release", upFile{body: rowRelease})
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || r.suite("/r/dists/s") == nil {
			t.Fatalf("adoption=%v: the creating miss = %d", adoption, rec.Code)
		}
		if rec := r.get("/r/dists/s/Release"); rec.Code != http.StatusOK {
			t.Fatalf("adoption=%v: the second miss = %d", adoption, rec.Code)
		}
		r.h.activeWG.Wait()
		want := 0
		if adoption {
			want = 1
		}
		if calls := r.fresh.snapshot(); len(calls) != want || (want == 1 && calls[0].suitePath != "/r/dists/s") {
			t.Fatalf("adoption=%v: T1 calls %v, want %d for /r/dists/s", adoption, calls, want)
		}
	}
}

// Final fix wave FW-5 (c) (final spec review M2, its probe): T1-on-seed end
// to end through the real handler, the real checker and the real adopter.
// A first-contact miss of a new suite's InRelease creates its row — with
// the miss row's request count and the creation's interest stamp, which
// spec C4 Dispatch 3 requires — and fires T1, whose eager first adoption
// gives the suite a current snapshot with no further client request. The
// recording double of the other T1 tests cannot see a regression in that
// seam.
func TestServeHTTP_FirstContactMissAdoptsThroughTheRealChecker(t *testing.T) {
	ctx := context.Background()
	var logbuf syncLog
	logger := slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logbuf.String())
		}
	})
	c := simtest.OpenCache(t)
	const host, suite = "e2e.example", "/r/dists/s"
	pkgs := []byte("Package: p\nArchitecture: amd64\nVersion: 1\nFilename: pool/p_1_amd64.deb\nSize: 1\nSHA256: " +
		strings.Repeat("ab", 32) + "\n\n")
	sum := sha256.Sum256(pkgs)
	inRel := simtest.ClearsignedFixture(fmt.Sprintf("Label: e2e\nSHA256:\n %s %d main/binary-amd64/Packages\n",
		hex.EncodeToString(sum[:]), len(pkgs)))
	members := &memberStore{bodies: map[string][]byte{}}
	members.put("http://"+host+suite+"/main/binary-amd64/Packages", pkgs)
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: members, Verifier: plainVerifier{},
		HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	probes := &simtest.StaticConditional{Status: http.StatusOK, Body: inRel}
	ck, err := freshness.New(freshness.Config{Cache: c, Fetcher: probes, HostLimiter: hostsem.New(8),
		Logger: logger, Adopter: ad, LifetimeCtx: ctx})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: staticUpstream{"http://" + host + suite + "/InRelease": inRel},
		Logger: logger, HostLimiter: hostsem.New(8), Freshness: ck, AdoptionEnabled: true, SuiteGuard: ck})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+suite+"/InRelease", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first contact = %d", rec.Code)
	}
	h.activeWG.Wait() // the T1 goroutine (Close would cancel its context)
	ck.WaitForAdoptions()
	sf, err := c.GetSuiteFreshness(ctx, "http", host, suite)
	if err != nil {
		t.Fatal(err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatalf("the first-contact miss led to no eager first adoption (%d probes)", probes.Calls())
	}
	snap, err := c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
	if err != nil || snap.Form != cache.FormInline {
		t.Fatalf("current snapshot %+v (err %v), want the inline form", snap, err)
	}
}

// slowUpstream answers like staticUpstream after delay, counting fetches.
type slowUpstream struct {
	staticUpstream
	delay   time.Duration
	fetches *atomic.Int32
}

func (u slowUpstream) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	u.fetches.Add(1)
	time.Sleep(u.delay)
	return u.staticUpstream.Fetch(ctx, target, dst)
}

// commitSnapshotWhenTracked holds a flip of suite until the suite has a row
// and commits a snapshot whose InRelease it returns.
func commitSnapshotWhenTracked(t *testing.T, s *flipStack, suite string) ([]byte, chan struct{}) {
	t.Helper()
	ctx := context.Background()
	body := simtest.ClearsignedFixture("Origin: snapshot\n")
	hash := writeBlob(t, s.h, body)
	cand, _, err := s.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: flipSimHost, SuitePath: suite, InReleaseHash: &hash})
	if err != nil {
		t.Fatal(err)
	}
	return body, s.holdUntilTracked(suite, 0, func() error {
		return s.c.CommitAdoption(ctx, cand, []cache.SnapshotMember{{SnapshotID: cand, Path: "InRelease",
			BlobHash: hash, DeclaredSHA256: hash}}, nil, nil, nil, false)
	})
}

// twoGets runs two concurrent GETs of path and returns their bodies.
func twoGets(s *flipStack, path string) [2][]byte {
	var bodies [2][]byte
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
			bodies[i] = rec.Body.Bytes()
		}()
	}
	wg.Wait()
	return bodies
}

// Phase D carry-forward (final review, tests Minor 3; t9_miss_waiter_noreg):
// a miss waiter that shares the creating leader's fetch registers its own
// serve with the flip barrier, so a flip of the just-created suite (an
// eager first adoption) holds it too, and it serves what the flip
// committed — reachable since phase E.
func TestServeHTTP_CoalescedMissWaiterRegistersItsOwnServe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		var fetches atomic.Int32
		s.h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://" + flipSimHost + suite + "/InRelease": rowInRelease},
			delay: time.Second, fetches: &fetches}
		snap, flipped := commitSnapshotWhenTracked(t, s, suite)
		bodies := twoGets(s, suite+"/InRelease")
		<-flipped
		if fetches.Load() != 1 {
			t.Fatalf("%d upstream fetches, want 1 (the waiter shares the leader's)", fetches.Load())
		}
		for i, b := range bodies {
			if !bytes.Equal(b, snap) {
				t.Fatalf("request %d served %q, want the snapshot the flip committed (its serve was not registered)", i, b)
			}
		}
	})
}

// Phase D carry-forward (t10_waiter_noreg): an onboarding hit that shares
// another hit's creation registers its own serve, as above.
func TestServeHTTP_OnboardingWaiterRegistersItsOwnServe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.cacheRow(suite+"/InRelease", rowInRelease, wallclock.Unix()) // a legacy anchor: the hits onboard
		var creations atomic.Int32
		s.h.cachedAnchorsHook = func() { // the first creation lasts a second: the other hit joins it
			if creations.Add(1) == 1 {
				time.Sleep(time.Second)
			}
		}
		snap, flipped := commitSnapshotWhenTracked(t, s, suite)
		bodies := twoGets(s, suite+"/InRelease")
		<-flipped
		if creations.Load() != 1 {
			t.Fatalf("%d onboardings ran, want 1 shared", creations.Load())
		}
		for i, b := range bodies {
			if !bytes.Equal(b, snap) {
				t.Fatalf("hit %d served %q, want the snapshot the flip committed (its serve was not registered)", i, b)
			}
		}
	})
}

// Deferred minor D-35: onboarding's creation and its backstop share one
// deadline, so a hold of the just-created suite between them (reachable
// since eager first adoption) cannot stretch the wait before the body past
// onboardTimeout + flipgate.HoldMax.
func TestServeHTTP_OnboardingSharesOneDeadlineWithItsBackstop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/n/dists/s"
		s.h.adoptionEnabled = true
		s.cacheRow(suite+"/Release", rowRelease, wallclock.Unix())
		s.signsAfterTheSniff(suite, wallclock.Unix())
		s.h.backstopHook = func(ctx context.Context) { <-ctx.Done() } // a writer that never answers
		flipped := s.holdUntilTracked(suite, 3*time.Second, func() error { return nil })
		start := time.Now()
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
		<-flipped
		if elapsed := time.Since(start); rec.Code != http.StatusOK || elapsed != onboardTimeout {
			t.Fatalf("status %d after %v, want 200 after the shared %v (not the 3 s hold + a fresh %v)",
				rec.Code, elapsed, onboardTimeout, backstopTimeout)
		}
	})
}

// Deferred minor D-34: onboarding runs under the handler's lifecycle, so a
// graceful shutdown cuts its creation short; the hit is served untracked.
func TestServeHTTP_OnboardingStopsAtShutdown(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.cacheAnchor("/r/dists/s/InRelease", rowInRelease, 1_700_000_000)
	r.h.cachedAnchorsHook = func() { r.h.lifecycleCancel() } // shutdown begins during the creation
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("hit = %d, want the cached anchor served", rec.Code)
	}
	if r.suite("/r/dists/s") != nil {
		t.Fatal("the onboarding outlived the shutdown")
	}
}

// Design review M-15: the miss path's backstop deadline counts from the
// request's start, so a slow fetch (or a flip's hold) leaves the backstop
// only what is left of the 25 s budget (missBodyBudget): the body still
// starts inside apt's 30 s, not after the fetch plus a fresh
// backstopTimeout.
func TestServeHTTP_MissBackstopSharesTheRequestsBudget(t *testing.T) {
	const budget = 25 * time.Second // missBodyBudget
	for _, fetchTime := range []time.Duration{2 * time.Second, 22 * time.Second} {
		t.Run(fetchTime.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newFlipStack(t, 0)
				const suite = "/n/dists/s"
				s.h.adoptionEnabled = true // a Release creates the row; it marks nothing itself
				var fetches atomic.Int32
				s.h.fetch = slowUpstream{staticUpstream: staticUpstream{"http://" + flipSimHost + suite + "/Release": rowRelease},
					delay: fetchTime, fetches: &fetches}
				s.signsAfterTheSniff(suite, wallclock.Unix())                 // the creation is unproven: the backstop runs
				s.h.backstopHook = func(ctx context.Context) { <-ctx.Done() } // a writer that never answers
				start := time.Now()
				rec := httptest.NewRecorder()
				s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
				want := min(fetchTime+backstopTimeout, budget)
				if elapsed := time.Since(start); rec.Code != http.StatusOK || elapsed != want {
					t.Fatalf("status %d after %v, want 200 after %v (the %v fetch, then the backstop within the %v budget)",
						rec.Code, elapsed, want, fetchTime, budget)
				}
			})
		})
	}
}

// Spec C3 T1-on-seed: only the creating request fires T1 — a coalesced
// miss waiter shares the leader's result, creation included, and fires
// nothing. (Proposed by the executability review; kills X27.) The leader
// waits at its host slot until the second miss has joined it, so the two
// coalesce without a wall-clock window (deferred minor E-66).
func TestServeHTTP_CoalescedCreatingMissFiresT1Once(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	var fetches atomic.Int32
	r.h.fetch = slowUpstream{staticUpstream: staticUpstream{r.srv.URL + "/r/dists/s/InRelease": rowInRelease},
		fetches: &fetches}
	_, host, port := splitURL(t, r.srv.URL)
	holdHostSlotsUntilJoined(t, r.h, hostKey(host, port), 0)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK {
				t.Errorf("miss = %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	r.h.activeWG.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("%d upstream fetches, want 1 (setup: the two misses must coalesce)", fetches.Load())
	}
	if calls := r.fresh.snapshot(); len(calls) != 1 {
		t.Fatalf("T1 calls %v, want exactly one (the creating leader's)", calls)
	}
}
