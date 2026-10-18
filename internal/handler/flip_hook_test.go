package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
)

// Spec C8 handler side: InRelease and Release serves of a tracked suite
// register with the barrier (a flip right after one waits out the quiet
// period); Release.gpg and index serves never do.
func TestServeHTTP_AnchorServesRegisterWithTheBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/u/dists/s"
		s.adopt(suite, 1)
		key := flipgate.Key("http", flipSimHost, suite)
		for _, tc := range []struct {
			file      string
			registers bool
		}{
			{"/InRelease", true},
			{"/Release", true}, // 404 from this inline snapshot: still an anchor serve
			{"/Release.gpg", false},
			{"/main/binary-amd64/Packages", false},
		} {
			time.Sleep(time.Minute) // every earlier quiet period is over
			s.get(suite+tc.file, 0)
			start := time.Now()
			if _, err := s.gate.Flip(context.Background(), key, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			want := time.Duration(0)
			if tc.registers {
				want = 10 * time.Second
			}
			if got := time.Since(start); got != want {
				t.Errorf("%s: a flip right after the serve waited %v, want %v", tc.file, got, want)
			}
		}
	})
}

// Spec C8: gate state exists only for suites with a row — and not at all
// when the barrier is off (flip_quiet_period = 0s, today's behaviour).
func TestServeHTTP_OnlyTrackedSuitesCreateGateState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		s.get("/untracked/dists/s/InRelease", 0) // no suite row: a plain miss (404 upstream)
		if n := s.gate.Len(); n != 0 {
			t.Fatalf("an untracked suite created %d gate entries", n)
		}
		if err := s.c.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{CanonicalScheme: "http",
			CanonicalHost: flipSimHost, SuitePath: "/tracked/dists/s"}); err != nil {
			t.Fatal(err)
		}
		s.get("/tracked/dists/s/InRelease", 0)
		if n := s.gate.Len(); n != 1 {
			t.Fatalf("a tracked suite's anchor serve left %d gate entries, want 1", n)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 0)
		s.adopt("/u/dists/s", 1)
		s.get("/u/dists/s/InRelease", 0)
		if n := s.gate.Len(); n != 0 {
			t.Fatalf("a disabled barrier holds %d entries", n)
		}
	})
}

// Spec C8: a request that arrives while its suite's flip holds anchors
// waits — it registers before the snapshot pointer is read — and is
// answered from the new snapshot the moment the commit is done.
func TestServeHTTP_HeldAnchorIsAnsweredFromTheNewSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/u/dists/s"
		s.adopt(suite, 1)
		var wg sync.WaitGroup
		t0 := time.Now()
		wg.Add(1)
		go func() { // a client every 500 ms, each reading the anchor over 1 s
			defer wg.Done()
			for time.Since(t0) < 75*time.Second {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.get(suite+"/InRelease", time.Second)
				}()
				time.Sleep(500 * time.Millisecond)
			}
		}()
		var committedAt time.Time
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(250 * time.Millisecond)
			s.adopt(suite, 2) // phase A gives up at 60.25 s; the hold commits once quiet
			committedAt = time.Now()
		}()
		time.Sleep(65 * time.Second) // the hold is in progress
		sent := time.Now()
		gen, code := s.get(suite+"/InRelease", 0)
		answered := time.Now()
		wg.Wait()
		if code != http.StatusOK || gen != s.current(suite) {
			t.Fatalf("held request: status %d, snapshot %q; want 200 from the new snapshot %q", code, gen, s.current(suite))
		}
		if !answered.Equal(committedAt) || answered.Sub(sent) > flipgate.HoldMax {
			t.Fatalf("answered %v after it was sent (at %v); the commit was at %v",
				answered.Sub(sent), answered.Sub(t0), committedAt.Sub(t0))
		}
	})
}

// Spec C8: the wait honours handler shutdown — Close releases a held
// request at once. Defence in depth: the daemon closes the gate at the
// shutdown signal first (design note 18), so no hold is left by then.
func TestHandler_CloseReleasesHeldAnchorRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/u/dists/s"
		s.adopt(suite, 1)
		var wg sync.WaitGroup
		t0 := time.Now()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Since(t0) < 65*time.Second {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.get(suite+"/InRelease", time.Second)
				}()
				time.Sleep(500 * time.Millisecond)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(250 * time.Millisecond)
			s.adopt(suite, 2) // holds from 60.25 s, commits at 71 s
		}()
		time.Sleep(66 * time.Second)
		answered := make(chan time.Time, 1)
		go func() {
			s.get(suite+"/InRelease", 0) // held
			answered <- time.Now()
		}()
		time.Sleep(time.Second)
		closeAt := time.Now()
		s.h.Close()
		if at := <-answered; !at.Equal(closeAt) {
			t.Fatalf("the held request was answered %v after Close, want at once", at.Sub(closeAt))
		}
		// Close waits for the other released requests' 1 s transfers, not
		// for the commit.
		if d := time.Since(closeAt); d > time.Second {
			t.Fatalf("Close took %v, want at most the released reads' 1s transfer", d)
		}
		wg.Wait()
	})
}
