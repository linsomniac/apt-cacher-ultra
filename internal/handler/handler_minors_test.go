package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Deferred minor D-32: readCachedAnchor is the one reading of a cached
// signed anchor that the creating transaction's marks and the backstop
// share. No row: nothing, no error; a row without a blob: unsigned; a
// clearsigned InRelease and an armored Release.gpg: signed; a plain-text
// InRelease: unsigned; a pool file that is gone: the row and an error.
func TestReadCachedAnchor(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	ctx := context.Background()
	key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/r/dists/s"}
	if row, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); row != nil || signed || err != nil {
		t.Fatalf("no row: %+v %v %v, want nil, false, nil", row, signed, err)
	}
	if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		Path: "/r/dists/s/InRelease", UpstreamURL: "http://127.0.0.1/r/dists/s/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if row, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); row == nil || signed || err != nil {
		t.Fatalf("a row without a blob: %+v %v %v, want the row, unsigned, nil", row, signed, err)
	}
	for _, tc := range []struct {
		path string
		body []byte
		want bool
	}{
		{"/r/dists/s/InRelease", rowInRelease, true},
		{"/r/dists/s/Release.gpg", rowGPG, true},
		{"/r/dists/p/InRelease", []byte("Origin: plain\n"), false},
	} {
		r.cacheAnchor(tc.path, tc.body, 1_700_000_000)
		k := key
		k.SuitePath = tc.path[:len(tc.path)-len("/InRelease")]
		if tc.path == "/r/dists/s/Release.gpg" {
			k.SuitePath = "/r/dists/s"
		}
		if row, signed, err := r.h.readCachedAnchor(ctx, k, tc.path); row == nil || signed != tc.want || err != nil {
			t.Fatalf("%s: %+v %v %v, want signed=%v", tc.path, row, signed, err, tc.want)
		}
	}
	row, _, _ := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease")
	if err := os.Remove(r.c.BlobPath(*row.BlobHash)); err != nil {
		t.Fatal(err)
	}
	if got, signed, err := r.h.readCachedAnchor(ctx, key, "/r/dists/s/InRelease"); got == nil || signed || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing pool file: %+v %v %v, want the row, unsigned, ErrNotExist", got, signed, err)
	}
}

// Deferred minor D-31: the rejection sweep's timer body recovers its own
// panic — reported through onPanic, the lock released (the deferred
// unlock runs first) — and New refuses a negative MaxSuites.
func TestSeedRejections_ExpireRecoversItsPanic(t *testing.T) {
	var got any
	s := &seedRejections{onPanic: func(v any) { got = v }, sweepHook: func() { panic("sweep boom") }}
	s.expire()
	if got != "sweep boom" {
		t.Fatalf("onPanic got %v, want the sweep's panic", got)
	}
	done := make(chan int, 1)
	go func() { done <- s.len() }() // the lock was released
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep's panic left the rejection cache locked")
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{Parser: parser, Cache: openTestCache(t), Fetch: noUpstream{}, HostLimiter: hostsem.New(1),
		Logger: silentLogger(), MaxSuites: -1})
	if err == nil || err.Error() != "handler: negative MaxSuites" {
		t.Fatalf("New(MaxSuites: -1) = %v, want handler: negative MaxSuites", err)
	}
}

// openTestCache opens a cache in a test directory.
func openTestCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Deferred minor E-65 (a): only the miss that created its suite's row
// fires the seed T1. Two first-contact misses of one suite on different
// files: the Release miss reads the suite untracked, then — while it
// prepares its creation — the InRelease miss creates the row and fires
// its seed T1; the Release miss's creation finds the row (SuiteExists)
// and fires none.
func TestServeHTTP_AMissThatFindsTheRowCreatedFiresNoSeedT1(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	r.serve("/r/dists/s/Release", upFile{body: rowRelease})
	r.h.cachedAnchorsHook = func() { // the Release miss's creation, before its writer operation
		r.h.cachedAnchorsHook = nil
		if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusOK || r.suite("/r/dists/s") == nil {
			t.Errorf("InRelease miss = %d; want 200 and the row created", rec.Code)
		}
	}
	if rec := r.get("/r/dists/s/Release"); rec.Code != http.StatusOK {
		t.Fatalf("Release miss = %d", rec.Code)
	}
	r.h.Close() // drains the T1 goroutines
	if calls := r.fresh.snapshot(); len(calls) != 1 {
		t.Fatalf("T1 calls %v, want exactly one (the creating InRelease miss's)", calls)
	}
}

// Deferred minor E-65 (b), design note 32: a miss whose request started
// longer ago than missBodyBudget gets a backstop deadline already past —
// the backstop fails at once — never a floor in the future.
func TestMissBackstopDeadline_APastBudgetIsPast(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	if d := missBackstopDeadline(start); !d.Equal(start.Add(missBodyBudget)) {
		t.Fatalf("deadline %v after start, want %v (the budget, already past)", d.Sub(start), missBodyBudget)
	}
	if d := missBackstopDeadline(time.Now()); time.Until(d) > backstopTimeout {
		t.Fatalf("a fresh miss's deadline is %v away, want at most %v", time.Until(d), backstopTimeout)
	}
}

// Deferred minor E-65 (c), D-34: onboarding's backstop runs under the
// handler's lifecycle, so a shutdown that begins while it runs cuts it
// short at once: it never waits out the deadline it shares with its
// creation, and the hit is served.
func TestServeHTTP_OnboardingBackstopStopsAtShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 0)
		const suite = "/n/dists/s"
		s.h.adoptionEnabled = true
		s.cacheRow(suite+"/Release", rowRelease, wallclock.Unix())
		s.signsAfterTheSniff(suite, wallclock.Unix()) // the creation is unproven: the backstop runs
		ran := false
		s.h.backstopHook = func(ctx context.Context) {
			ran = true
			s.h.lifecycleCancel() // shutdown begins during the backstop
			select {
			case <-ctx.Done():
			case <-time.After(time.Hour):
			}
		}
		start := time.Now()
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/Release", nil))
		if rec.Code != http.StatusOK || !ran || time.Since(start) != 0 {
			t.Fatalf("status %d after %v, backstop ran %v; want 200 at once, the backstop cut short by the shutdown",
				rec.Code, time.Since(start), ran)
		}
	})
}

// Deferred minor E-71: the hit path's strict-mode decision when the
// request's context ends between its two lookups — DeclaredHashesForPath
// found no row, then the host's coverage read fails on the dead context —
// is client_canceled (503), never the .deb (FW-5 (a)).
func TestServeHTTP_StrictCanceledBetweenTheLookups(t *testing.T) {
	var upstreamHits atomic.Int32
	h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, &upstreamHits)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)
	adoptCoverageComplete(t, h, scheme, canonHost, "/dists/noble", true)
	const debPath = "/pool/main/u/unvouched/unvouched_1_amd64.deb"
	blob := writeBlob(t, h, []byte("an unvouched .deb, cached"))
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: scheme, CanonicalHost: canonHost,
		Path: debPath, BlobHash: &blob, UpstreamURL: srv.URL + debPath}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.strictLookupHook = cancel
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, debPath).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "unvouched .deb, cached") {
		t.Fatalf("status %d body %q, want 503 and no .deb", rec.Code, rec.Body.String())
	}
	recs := logs.find("request")
	if len(recs) == 0 || recs[len(recs)-1]["outcome"] != "client_canceled" {
		t.Fatalf("request lines %v, want the last one client_canceled", recs)
	}
}
