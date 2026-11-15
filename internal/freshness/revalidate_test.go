package freshness

// The checker's side of request-time revalidation (freshness.revalidate_wait,
// the 2026-11-14 publication-delay review): CheckNow tells the handler how
// the check it ran ended, and AwaitSuite lets a request wait, within its
// bound, for the check or adoption in flight on its suite.

import (
	"context"
	"testing"
	"time"
)

// newRelease makes the rig's upstream publish a release whose index is new
// (so its adoption must fetch it) and returns a fetcher that holds that
// fetch until released.
func newRelease(t *testing.T, r *retryRig, label string) *holdFetcher {
	t.Helper()
	r.pkgs = fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
	r.members.put(rigPkgsURL, r.pkgs)
	hold := &holdFetcher{fakeFetcher: r.members, hold: rigPkgsURL,
		entered: make(chan struct{}, 1), release: make(chan struct{}), held: make(chan struct{})}
	r.adopter.fetcher = hold
	r.up.set(r.release(label, 0))
	return hold
}

func TestCheckNow_ReportsHowTheCheckEnded(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, time.Minute)
	r.adopt()
	r.clock.advance(time.Minute)
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != "not_modified" {
		t.Fatalf("an unchanged upstream: CheckNow = %q, want not_modified", got)
	}
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != resultSkipped {
		t.Fatalf("inside the cooldown: CheckNow = %q, want %q", got, resultSkipped)
	}
	r.clock.advance(time.Minute)
	r.up.set(r.release("v2", 0))
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != "changed" {
		t.Fatalf("a new release: CheckNow = %q, want changed", got)
	}
	r.checker.WaitForAdoptions()
	if got := r.checker.CheckNow(ctx, "http", "nowhere", "/dists/none"); got != resultSkipped {
		t.Fatalf("an untracked suite: CheckNow = %q, want %q", got, resultSkipped)
	}
}

// A request whose check found a new release waits for its adoption and is
// told it was published.
func TestAwaitSuite_WaitsForTheAdoptionTheCheckStarted(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.adopt()
	before := *r.row().CurrentSnapshotID
	hold := newRelease(t, r, "v2")

	start := time.Now()
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != "changed" {
		t.Fatalf("CheckNow = %q, want changed", got)
	}
	type result struct {
		check              string
		published, settled bool
	}
	done := make(chan result, 1)
	go func() {
		check, published, settled := r.checker.AwaitSuite(ctx, "http", rigHost, rigSuite, start, 10*time.Second)
		done <- result{check, published, settled}
	}()
	select {
	case res := <-done:
		t.Fatalf("AwaitSuite returned %+v while the adoption was still fetching", res)
	case <-time.After(100 * time.Millisecond):
	}
	close(hold.release)
	select {
	case res := <-done:
		if res != (result{"changed", true, true}) {
			t.Fatalf("AwaitSuite = %+v, want the check's result, published and settled", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitSuite did not return once the adoption had finished")
	}
	if now := r.row().CurrentSnapshotID; now == nil || *now == before {
		t.Fatal("AwaitSuite reported a publication, but the suite's snapshot did not change")
	}
}

// The wait is bounded: an adoption that outlasts it is not waited for.
func TestAwaitSuite_GivesUpAtItsBound(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.adopt()
	hold := newRelease(t, r, "v2")
	defer func() {
		close(hold.release)
		r.checker.WaitForAdoptions()
	}()

	start := time.Now()
	r.checker.CheckNow(ctx, "http", rigHost, rigSuite)
	check, published, settled := r.checker.AwaitSuite(ctx, "http", rigHost, rigSuite, start, 150*time.Millisecond)
	if settled || published {
		t.Fatalf("AwaitSuite = (%q, published %v, settled %v), want neither", check, published, settled)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > 5*time.Second {
		t.Fatalf("AwaitSuite returned after %s, want about its 150ms bound", took)
	}
}

// A request that arrives when an adoption has already run longer than the
// wait does not wait at all: the adoption has shown it is slow, and every
// later request would otherwise be delayed for nothing.
func TestAwaitSuite_AnAdoptionOlderThanTheWaitIsNotWaitedFor(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.adopt()
	hold := newRelease(t, r, "v2")
	defer func() {
		close(hold.release)
		r.checker.WaitForAdoptions()
	}()
	r.checker.CheckNow(ctx, "http", rigHost, rigSuite)
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	_, published, settled := r.checker.AwaitSuite(ctx, "http", rigHost, rigSuite, start, 150*time.Millisecond)
	if settled || published {
		t.Fatalf("AwaitSuite = (published %v, settled %v), want neither", published, settled)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("AwaitSuite waited %s for an adoption already older than the wait", took)
	}
}

// An adoption that fails publishes nothing, and the request is told so.
func TestAwaitSuite_AFailedAdoptionIsNotAPublication(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.staleMirror()
	r.up.set(r.release("v2", 0))

	start := time.Now()
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != "changed" {
		t.Fatalf("CheckNow = %q, want changed", got)
	}
	check, published, settled := r.checker.AwaitSuite(ctx, "http", rigHost, rigSuite, start, 10*time.Second)
	if check != "changed" || published {
		t.Fatalf("AwaitSuite = (%q, published %v), want changed and not published", check, published)
	}
	_ = settled // the adoption may have ended before the call: either way nothing was published
}

// With nothing in flight there is nothing to wait for.
func TestAwaitSuite_NothingInFlight(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	start := time.Now()
	check, published, settled := r.checker.AwaitSuite(context.Background(), "http", rigHost, rigSuite, start, 10*time.Second)
	if check != "" || published || !settled {
		t.Fatalf("AwaitSuite = (%q, published %v, settled %v), want nothing in flight", check, published, settled)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("AwaitSuite waited %s with nothing in flight", took)
	}
}

// A second request arrives while the first one's check is still asking
// upstream: its own check ends busy, and it waits for the first's, whose
// result it is told.
func TestAwaitSuite_ASecondRequestWaitsForTheCheckInFlight(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.adopt()
	// The first check waits in its upstream request until released.
	entered, release := make(chan struct{}), make(chan struct{})
	r.up.mu.Lock()
	r.up.before = func(string) {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
	}
	r.up.mu.Unlock()
	first := make(chan string, 1)
	go func() { first <- r.checker.CheckNow(ctx, "http", rigHost, rigSuite) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the first check never asked upstream")
	}

	start := time.Now()
	if got := r.checker.CheckNow(ctx, "http", rigHost, rigSuite); got != resultBusy {
		t.Fatalf("the second request's CheckNow = %q, want %q", got, resultBusy)
	}
	done := make(chan string, 1)
	go func() {
		check, _, _ := r.checker.AwaitSuite(ctx, "http", rigHost, rigSuite, start, 10*time.Second)
		done <- check
	}()
	select {
	case got := <-done:
		t.Fatalf("AwaitSuite returned %q while the first check was still asking upstream", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-first; got != "not_modified" {
		t.Fatalf("the first check = %q, want not_modified", got)
	}
	select {
	case got := <-done:
		if got != "not_modified" {
			t.Fatalf("AwaitSuite = %q, want the first check's not_modified", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitSuite did not return once the first check had ended")
	}
}
