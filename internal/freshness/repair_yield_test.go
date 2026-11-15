package freshness

// A repair gives way (the 2026-11-14 publication-delay review): the heal
// of an optional file in a snapshot already serving must never keep a new
// release — its own suite's, or another suite's on the same host — from
// being noticed or adopted.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

const rigContentsURL = "http://h/r/dists/x/Contents-amd64.gz"

// holdFetcher answers as its fakeFetcher does, except that the first fetch
// of hold waits — after signalling entered — until its context ends or
// release is closed.
type holdFetcher struct {
	*fakeFetcher
	hold    string
	entered chan struct{}
	release chan struct{}
	held    chan struct{} // closed once the held fetch has returned
}

func (f *holdFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if target.URL == f.hold {
		select {
		case f.entered <- struct{}{}:
			defer close(f.held)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-f.release:
			}
		default: // a later fetch of the same URL passes
		}
	}
	return f.fakeFetcher.Fetch(ctx, target, dst)
}

// degradedRig is a retry rig whose suite serves a snapshot that lacks an
// optional member — Contents-amd64.gz, skipped at adoption because the
// mirror served another generation's bytes — with repair on. Upstream now
// serves the right bytes, through the returned fetcher, which holds the
// first fetch of that member: the next unchanged check starts a repair
// that waits there.
func degradedRig(t *testing.T, cooldown time.Duration) (*retryRig, *holdFetcher, []byte) {
	t.Helper()
	r := newRetryRig(t, cooldown)
	r.adopter.tolerateOptionalMembers = true
	r.adopter.repairSkippedMembers = true
	contents := []byte(strings.Repeat("C", 100))
	rel := signedRelease("v1", 0, map[string][]byte{
		"main/binary-amd64/Packages": r.pkgs,
		"Contents-amd64.gz":          contents,
	})
	r.members.put(rigContentsURL, []byte("the previous generation's bytes"))
	r.up.set(rel)
	r.check()
	snap := r.row().CurrentSnapshotID
	if snap == nil {
		t.Fatal("setup: not adopted")
	}
	if rows, err := r.cache.ListRepairableSkippedMembers(context.Background(), *snap); err != nil || len(rows) != 1 {
		t.Fatalf("setup: %d repairable skip rows (err %v), want 1", len(rows), err)
	}
	r.members.put(rigContentsURL, contents)
	hold := &holdFetcher{fakeFetcher: r.members, hold: rigContentsURL,
		entered: make(chan struct{}, 1), release: make(chan struct{}), held: make(chan struct{})}
	r.adopter.fetcher = hold
	r.clock.advance(cooldown) // the next check is past the cooldown
	return r, hold, rel
}

// startRepair runs the unchanged check that starts the repair and waits
// until the repair is in its held fetch.
func startRepair(t *testing.T, r *retryRig, hold *holdFetcher) {
	t.Helper()
	r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	select {
	case <-hold.held:
		t.Fatal("setup: the repair's fetch returned before it was held")
	case <-time.After(5 * time.Second):
		t.Fatal("setup: no repair started")
	case <-waitEntered(hold):
	}
}

// waitEntered returns a channel that closes once the held fetch began.
func waitEntered(hold *holdFetcher) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for len(hold.entered) == 0 {
			time.Sleep(time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

// settle waits for the rig's adoptions and repairs to end, as
// WaitForAdoptions does, but gives up after ten seconds: it then lets the
// held fetch go, so that the goroutines unwind, and fails the test. A
// repair that does not give way would otherwise hang the test.
func settle(t *testing.T, r *retryRig, hold *holdFetcher) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		r.checker.WaitForAdoptions()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(hold.release)
		<-done
		t.Fatal("the repair was still running after 10s: it did not give way")
	}
}

// warnings returns the rig's log lines at WARN or ERROR.
func warnings(r *retryRig) []string {
	var out []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) || strings.Contains(line, `"level":"ERROR"`) {
			out = append(out, line)
		}
	}
	return out
}

// A suite on the same host has a new release while the repair runs: the
// repair stops, and the release is adopted by the very check that found
// it, instead of waiting for the repair to finish (up to
// adoption.max_duration) and then for its next check.
func TestRepair_GivesWayToAnAdoptionOnItsHost(t *testing.T) {
	ctx := context.Background()
	r, hold, _ := degradedRig(t, 0)
	startRepair(t, r, hold)

	const other = "/r/dists/y"
	seedStaleAnchorAt(t, r.cache, rigHost, other)
	interest := int64(rigStart)
	if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost,
		SuitePath: other, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r.up.setAt("http://"+rigHost+other+"/InRelease", signedRelease("y1", 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs}))

	r.checker.Check(ctx, "http", rigHost, other)
	settle(t, r, hold)

	row, err := r.cache.GetSuiteFreshness(ctx, "http", rigHost, other)
	if err != nil {
		t.Fatal(err)
	}
	if row.CurrentSnapshotID == nil {
		t.Fatalf("the other suite's release was not adopted while a repair held its host; last result %q", lastResult(r))
	}
	ev := r.events("repair_yielded")
	if len(ev) != 1 || ev[0]["to"] != "adoption" || ev[0]["suite_path"] != rigSuite {
		t.Fatalf("repair_yielded = %v, want one line for %s, to an adoption", ev, rigSuite)
	}
	if w := warnings(r); len(w) != 0 {
		t.Errorf("a repair that gave way logged at WARN or above: %v", w)
	}
	// Giving way proves nothing about upstream: the member's failed
	// attempts are not counted, and it is repaired on a later check.
	rows, err := r.cache.ListRepairableSkippedMembers(ctx, *r.row().CurrentSnapshotID)
	if err != nil || len(rows) != 1 || rows[0].RetryCount != 0 {
		t.Errorf("skip rows = %+v (err %v), want the one row, attempts uncounted", rows, err)
	}
	if g, h := r.checker.admission.usage(); g != 0 || h != 0 {
		t.Errorf("slots still held: %d global, %d host", g, h)
	}
}

// The repair's own suite gets a new release: once the repair has run for
// the cooldown — when a check would ask upstream again — the check stops
// it and runs, and the release is adopted.
func TestRepair_GivesWayToItsSuitesCheck(t *testing.T) {
	r, hold, _ := degradedRig(t, 0)
	startRepair(t, r, hold)
	before := *r.row().CurrentSnapshotID

	r.up.set(signedRelease("v2", 0, map[string][]byte{
		"main/binary-amd64/Packages": r.pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("C", 100)),
	}))
	r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	settle(t, r, hold)

	row := r.row()
	if row.CurrentSnapshotID == nil || *row.CurrentSnapshotID == before {
		t.Fatalf("the new release was not adopted while the repair ran: snapshot %v, last result %q",
			row.CurrentSnapshotID, lastResult(r))
	}
	ev := r.events("repair_yielded")
	if len(ev) != 1 || ev[0]["to"] != "check" {
		t.Fatalf("repair_yielded = %v, want one line, to a check", ev)
	}
	if w := warnings(r); len(w) != 0 {
		t.Errorf("a repair that gave way logged at WARN or above: %v", w)
	}
}

// Inside the cooldown a check would ask upstream nothing, so the repair is
// left to finish: the check ends busy, as before, and the member heals.
func TestRepair_YoungerThanTheCooldownIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	r, hold, _ := degradedRig(t, time.Hour)
	startRepair(t, r, hold)
	snap := *r.row().CurrentSnapshotID

	r.checker.Check(ctx, "http", rigHost, rigSuite)
	if ev := r.events("repair_yielded"); len(ev) != 0 {
		t.Fatalf("repair_yielded = %v, want none inside the cooldown", ev)
	}
	select {
	case <-hold.held:
		t.Fatal("the repair's fetch was cancelled inside the cooldown")
	default:
	}
	close(hold.release)
	r.checker.WaitForAdoptions()
	if _, err := r.cache.GetSnapshotMember(ctx, snap, "Contents-amd64.gz"); err != nil {
		t.Fatalf("the member was not repaired: %v", err)
	}
}

// Admission tells an adoption which repairs stand in its way, and only
// those: a repair on its host, and — when the pool is short — a repair
// elsewhere holding a global slot. An adoption in the way is never asked.
func TestAdmission_Yielders(t *testing.T) {
	yielding := func(l *lease) *lease {
		l.setYield(func(string) {})
		return l
	}
	t.Run("a repair on the host", func(t *testing.T) {
		a := newAdmission(0)
		rep := yielding(mustAcquire(t, a, "h", true))
		if ys := a.yielders("h", true); len(ys) != 1 || ys[0] != rep {
			t.Fatalf("yielders = %v, want the host's repair", ys)
		}
		if ys := a.yielders("other", true); len(ys) != 0 {
			t.Fatalf("yielders for a free host = %v, want none", ys)
		}
	})
	t.Run("an adoption on the host", func(t *testing.T) {
		a := newAdmission(0)
		mustAcquire(t, a, "h", true)
		if ys := a.yielders("h", true); len(ys) != 0 {
			t.Fatalf("yielders = %v, want none: an adoption does not give way", ys)
		}
	})
	t.Run("a repair elsewhere holds the last global slot", func(t *testing.T) {
		a := newAdmission(2)
		mustAcquire(t, a, "a", true)
		rep := yielding(mustAcquire(t, a, "b", true))
		if ys := a.yielders("h", true); len(ys) != 1 || ys[0] != rep {
			t.Fatalf("yielders = %v, want the repair on b", ys)
		}
	})
	t.Run("the reserved slot still binds", func(t *testing.T) {
		a := newAdmission(2)
		mustAcquire(t, a, "a", true) // an adoption: stays
		yielding(mustAcquire(t, a, "b", true))
		// A suite without a signed snapshot needs two free slots; one
		// repair gone leaves one. Nothing is disturbed for nothing.
		if ys := a.yielders("h", false); len(ys) != 0 {
			t.Fatalf("yielders = %v, want none: the repair's slot would not be enough", ys)
		}
	})
	t.Run("freeFor counts a repair's slots as room", func(t *testing.T) {
		a := newAdmission(2)
		yielding(mustAcquire(t, a, "h", true))
		if n := a.freeFor("h", true); n != 2 {
			t.Fatalf("freeFor = %d, want 2: the repair gives way", n)
		}
		mustAcquire(t, a, "a", true)
		if n := a.freeFor("h", true); n != 1 {
			t.Fatalf("freeFor = %d, want 1", n)
		}
	})
	t.Run("a released lease closes released", func(t *testing.T) {
		a := newAdmission(2)
		l := mustAcquire(t, a, "h", true)
		l.releaseHost()
		select {
		case <-l.released:
			t.Fatal("released closed while the global slot is still held")
		default:
		}
		l.release()
		select {
		case <-l.released:
		default:
			t.Fatal("released not closed after the full release")
		}
	})
}
