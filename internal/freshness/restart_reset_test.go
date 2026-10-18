package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func seedRecords(t *testing.T, r *retryRig, next int64, recs map[string]string) {
	t.Helper()
	ctx := context.Background()
	for path, reason := range recs {
		if err := r.cache.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: path}); err != nil {
			t.Fatal(err)
		}
		if err := r.cache.RecordAdoptionFailure(ctx, "http", rigHost, path, cache.AdoptionFailure{
			Hash: testHex64, Reason: reason, Count: 9, FirstAt: 1, LastAt: 2, NextRetryAt: next}); err != nil {
			t.Fatal(err)
		}
	}
}

func nextRetryOf(t *testing.T, r *retryRig, path string) int64 {
	t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", rigHost, path)
	if err != nil {
		t.Fatal(err)
	}
	return i64Of(s.AdoptionNextRetryAt)
}

// Spec C7 "Restart": every failing suite gets one retry within
// [now, now + periodic_refresh); refusal states and panic records keep
// their schedule.
func TestRescheduleFailingAdoptions_SpreadsFailingSuitesOnly(t *testing.T) {
	r := newRetryRig(t, 0)
	far := r.clock.now().Add(50 * time.Minute).Unix()
	seedRecords(t, r, far, map[string]string{
		"/fail/dists/x": "gpg_failed", "/older/dists/x": cache.ReasonUpstreamOlder, "/boom/dists/x": cache.ReasonPanic,
	})
	r.checker.rescheduleFailingAdoptions(context.Background())
	// rand = 0.5 over the rig's 15 min refresh: now + 450 s.
	if got, want := nextRetryOf(t, r, "/fail/dists/x"), r.clock.now().Unix()+450; got != want {
		t.Fatalf("failing suite next retry = %d, want %d", got, want)
	}
	for _, p := range []string{"/older/dists/x", "/boom/dists/x"} {
		if got := nextRetryOf(t, r, p); got != far {
			t.Fatalf("%s next retry = %d, want untouched %d", p, got, far)
		}
	}
	if ev := r.events("adoption_retries_rescheduled"); len(ev) != 1 || ev[0]["suites"] != float64(1) {
		t.Fatalf("adoption_retries_rescheduled = %v", ev)
	}
}

// Run reschedules before anything else, even with periodic refresh off.
func TestRun_ReschedulesBeforeAnything(t *testing.T) {
	r := newRetryRig(t, 0)
	seedRecords(t, r, r.clock.now().Add(50*time.Minute).Unix(), map[string]string{"/fail/dists/x": "gpg_failed"})
	chk, err := New(Config{Cache: r.cache, Fetcher: r.up, HostLimiter: hostsem.New(8), Adopter: r.adopter,
		Logger: discardLogger(), now: r.clock.now, rand: func() float64 { return 0.5 }}) // Refresh 0
	if err != nil {
		t.Fatal(err)
	}
	chk.Run(context.Background()) // returns at once: periodic refresh is disabled
	if got, want := nextRetryOf(t, r, "/fail/dists/x"), r.clock.now().Unix(); got != want {
		t.Fatalf("next retry = %d, want now (no spread with refresh 0)", got)
	}
}
