package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// logRecords returns the JSON log records whose level is at least level
// (and, when msg is not empty, whose msg is msg).
func logRecords(logs *syncLog, msg string, level slog.Level) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		var l slog.Level
		if s, _ := m["level"].(string); l.UnmarshalText([]byte(s)) != nil || l < level {
			continue
		}
		if msg == "" || m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// heldSuite adopts suite and starts a flip of it whose commit blocks until
// the returned release is called: the suite is idle, so its hold starts at
// once and holds its anchor requests until then. release waits for the
// flip to return.
func heldSuite(t *testing.T, s *flipStack, suite string) (release func()) {
	t.Helper()
	s.adopt(suite, 1)
	unblock, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.gate.Flip(context.Background(), flipgate.Key("http", flipSimHost, suite),
			func() error { <-unblock; return nil }); err != nil {
			t.Error(err)
		}
	}()
	synctest.Wait()
	return func() { close(unblock); <-done }
}

// Final review, spec M2: a held request whose client leaves is a client
// abort. It is not registered (the gate's rule), and the handler answers
// it as one — request line outcome client_canceled, no WARN — instead of
// failing its snapshot read (a WARN "suite_freshness lookup failed" and a
// 502 "hit" line).
func TestServeHTTP_HeldClientThatLeavesIsAClientAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/u/dists/s"
		release := heldSuite(t, s, suite)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel) // the client leaves a second into its wait
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/InRelease", nil).WithContext(ctx))
		release()
		if warns := logRecords(logs, "", slog.LevelWarn); len(warns) != 0 {
			t.Errorf("a client that left while held logged %d WARN or worse, first %v", len(warns), warns[0])
		}
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 1 || lines[0]["outcome"] != "client_canceled" || lines[0]["status"] != float64(http.StatusServiceUnavailable) {
			t.Fatalf("request lines %v, want one client_canceled (503)", lines)
		}
		if got := lines[0]["flip_held_ms"]; got != float64(1000) {
			t.Errorf("flip_held_ms = %v, want 1000 (held until the client left)", got)
		}
	})
}

// Final review, tests Minor 2 (operability): the request line of an anchor
// request that waited in a hold carries flip_held_ms, so a slow apt update
// is explained by the line itself; a request that did not wait has no such
// field.
func TestServeHTTP_HeldAnchorRequestLineCarriesFlipHeldMs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLog{}
		s := newFlipStackLogging(t, 10*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		const suite = "/u/dists/s"
		release := heldSuite(t, s, suite)
		time.AfterFunc(2500*time.Millisecond, release) // the commit returns 2.5 s into the hold
		if _, code := s.get(suite+"/InRelease", 0); code != http.StatusOK {
			t.Fatalf("held request: %d, want 200", code)
		}
		time.Sleep(time.Minute) // every quiet period is over
		if _, code := s.get(suite+"/InRelease", 0); code != http.StatusOK {
			t.Fatalf("unheld request: %d, want 200", code)
		}
		lines := logRecords(logs, "request", slog.LevelInfo)
		if len(lines) != 2 {
			t.Fatalf("request lines %v, want two", lines)
		}
		if got := lines[0]["flip_held_ms"]; got != float64(2500) {
			t.Errorf("the held request's flip_held_ms = %v, want 2500", got)
		}
		if got, ok := lines[1]["flip_held_ms"]; ok {
			t.Errorf("the unheld request carries flip_held_ms = %v, want no field", got)
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
