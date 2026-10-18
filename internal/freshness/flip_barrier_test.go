package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

var (
	barrierSuite = SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}
	barrierKey   = flipgate.Key("http", rigHost, rigSuite)
)

// barrierRig is a real cache and an adopter that commits behind a flip
// barrier, for rigSuite on rigHost, inside a synctest bubble.
type barrierRig struct {
	c    *cache.Cache
	gate *flipgate.Gate
	ad   *Adopter
	logs *lockedBuffer
	pkgs []byte
}

func newBarrierRig(t *testing.T, quiet, heartbeat time.Duration) *barrierRig {
	t.Helper()
	c := simtest.OpenCache(t)
	gate, err := flipgate.New(flipgate.Config{Quiet: quiet})
	if err != nil {
		t.Fatal(err)
	}
	members := newFakeFetcher()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	members.put(rigPkgsURL, pkgs)
	logs := &lockedBuffer{}
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, HeartbeatInterval: heartbeat, FlipGate: gate,
		Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &barrierRig{c: c, gate: gate, ad: ad, logs: logs, pkgs: pkgs}
}

func (r *barrierRig) release(label string) []byte {
	return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
}

func (r *barrierRig) request(label string) *adoptionRequest {
	return &adoptionRequest{suite: barrierSuite, form: adoptionFormInline, bytes: r.release(label)}
}

// current returns the suite's current snapshot id, or 0 for none.
func (r *barrierRig) current(t *testing.T) int64 {
	t.Helper()
	sf, err := r.c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
	if errors.Is(err, cache.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	if sf.CurrentSnapshotID == nil {
		return 0
	}
	return *sf.CurrentSnapshotID
}

// barrierEvents returns the adopter's adoption_flip_barrier records.
func (r *barrierRig) barrierEvents() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "adoption_flip_barrier" {
			out = append(out, m)
		}
	}
	return out
}

// busyAnchor reads the suite's anchor for 500 ms once a second, n times.
func (r *barrierRig) busyAnchor(n int) {
	go func() {
		for i := 0; i < n; i++ {
			tok := r.gate.AnchorStart(context.Background(), barrierKey)
			time.Sleep(500 * time.Millisecond)
			tok.Done()
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

// The reaper (phase F) purges gate entries by the suite key it holds.
func TestSuiteKey_IsTheFlipGateKey(t *testing.T) {
	if got, want := suiteKey("http", "h", "/r/dists/x"), flipgate.Key("http", "h", "/r/dists/x"); got != want {
		t.Fatalf("suiteKey = %q, flipgate.Key = %q; want them equal", got, want)
	}
}

// Spec C8 adopter side: the commit waits while a client is still reading
// the suite's anchor, then the quiet period; the barrier is logged.
func TestAdopter_CommitsBehindTheFlipBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		tok := r.gate.AnchorStart(context.Background(), barrierKey) // a client reads the old anchor
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- r.ad.Run(context.Background(), barrierSuite, r.release("v1"), "", "") }()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if id := r.current(t); id != 0 {
			t.Fatalf("committed snapshot %d while an anchor was in flight", id)
		}
		tok.Done()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 15*time.Second {
			t.Fatalf("committed after %v, want 15s (5s in flight + the 10s quiet period)", d)
		}
		if r.current(t) == 0 {
			t.Fatal("no current snapshot after the flip")
		}
		evs := r.barrierEvents()
		if len(evs) != 1 || evs[0]["outcome"] != "held" || evs[0]["phase_a_seconds"] != 15.0 ||
			evs[0]["token_wait_seconds"] != 0.0 || evs[0]["hold_seconds"] != 0.0 || evs[0]["held_requests"] != 0.0 {
			t.Fatalf("adoption_flip_barrier events = %v, want one held flip after 15s of phase A", evs)
		}
	})
}

// Spec C8: a first adoption whose anchor is byte-for-byte the url_path
// anchor clients have been served changes nothing they hold, so it skips
// the barrier; any other first adoption waits.
func TestAdopter_FirstAdoptionOfTheServedAnchorSkipsTheBarrier(t *testing.T) {
	for _, tc := range []struct {
		name     string
		served   func(rel []byte) []byte
		wantWait time.Duration
		outcome  string
	}{
		{"same bytes", func(rel []byte) []byte { return rel }, 0, "not_needed"},
		{"other bytes", func([]byte) []byte { return []byte("an older InRelease") }, 15 * time.Second, "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				r := newBarrierRig(t, 10*time.Second, 0)
				rel := r.release("v1")
				h, err := r.ad.writeBlobBytes(ctx, tc.served(rel))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
					Path: rigSuite + "/InRelease", BlobHash: &h, UpstreamURL: rigAnchorURL, IsMetadata: true}); err != nil {
					t.Fatal(err)
				}
				tok := r.gate.AnchorStart(ctx, barrierKey)
				go func() { time.Sleep(5 * time.Second); tok.Done() }()
				start := time.Now()
				if err := r.ad.Run(ctx, barrierSuite, rel, "", ""); err != nil {
					t.Fatal(err)
				}
				if d := time.Since(start); d != tc.wantWait {
					t.Fatalf("committed after %v, want %v", d, tc.wantWait)
				}
				if evs := r.barrierEvents(); len(evs) != 1 || evs[0]["outcome"] != tc.outcome {
					t.Fatalf("adoption_flip_barrier events = %v, want one with outcome %s", evs, tc.outcome)
				}
				time.Sleep(10 * time.Second) // the reader is done
			})
		})
	}
}

// Design note 5: the detached form compares Release — index consistency
// depends on the Release text a client holds, not on its signature.
func TestAdopter_DetachedFirstAdoptionComparesTheRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newBarrierRig(t, 10*time.Second, 0)
		rel := r.release("detached v1")
		h, err := r.ad.writeBlobBytes(ctx, rel)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
			Path: rigSuite + "/Release", BlobHash: &h, UpstreamURL: "http://h/r/dists/x/Release", IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
		tok := r.gate.AnchorStart(ctx, barrierKey)
		defer tok.Done()
		start := time.Now()
		if err := r.ad.RunDetached(ctx, barrierSuite, rel, []byte("a detached signature"), "", ""); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("waited %v; the clients already hold this Release", d)
		}
		if evs := r.barrierEvents(); len(evs) != 1 || evs[0]["outcome"] != "not_needed" {
			t.Fatalf("adoption_flip_barrier events = %v, want one not_needed", evs)
		}
	})
}

// Ruling R-C2: the barrier does not honour adoption.max_duration — an
// adoption whose deadline passes while it waits for quiet still commits.
func TestAdopter_BarrierOutlivesTheAdoptionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		r.busyAnchor(45) // the last read ends at 44.5 s
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		if err := r.ad.runRequest(ctx, r.request("v1"), runHooks{stop: context.Background()}); err != nil {
			t.Fatalf("an adoption whose deadline passed in the barrier failed: %v", err)
		}
		if d := time.Since(start); d != 54500*time.Millisecond {
			t.Fatalf("committed after %v, want 54.5s (the last read ends at 44.5s, then the quiet period)", d)
		}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || r.current(t) == 0 {
			t.Fatalf("ctx err %v, current %d; want the deadline passed and a commit", ctx.Err(), r.current(t))
		}
	})
}

// Ruling R-C2: shutdown while the adoption waits in the barrier commits
// nothing; the adoption returns a shutdown error and logs no outcome.
func TestAdopter_ShutdownInTheBarrierCommitsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		tok := r.gate.AnchorStart(context.Background(), barrierKey)
		defer tok.Done()
		stop, stopNow := context.WithCancel(context.Background())
		time.AfterFunc(5*time.Second, stopNow)
		err := r.ad.runRequest(context.Background(), r.request("v1"), runHooks{stop: stop})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, flipgate.ErrAborted) {
			t.Fatalf("runRequest = %v, want a shutdown in the flip barrier", err)
		}
		if id := r.current(t); id != 0 || r.gate.Stats().Aborted != 1 || len(r.barrierEvents()) != 0 {
			t.Fatalf("current %d, stats %+v, events %v; want no commit, one abort, no barrier event",
				id, r.gate.Stats(), r.barrierEvents())
		}
	})
}

// Deferred minor B-65: a shutdown that arrives after adoption.max_duration
// fired, but before the commit, still abandons it. ctx.Err() stays
// DeadlineExceeded then, so shutdown is read from the lifetime context.
func TestAdoption_ShutdownAfterTheDeadlineStillAbandonsTheCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 0, 0) // no barrier: the check before the commit decides
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		stop, stopNow := context.WithCancel(context.Background())
		defer stopNow()
		err := r.ad.runRequest(ctx, r.request("v1"), runHooks{stop: stop, membersDone: func() {
			<-ctx.Done() // the deadline fires
			stopNow()    // then the daemon stops
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runRequest = %v, want a shutdown error", err)
		}
		if id := r.current(t); id != 0 {
			t.Fatalf("committed snapshot %d after a shutdown", id)
		}
	})
}

// Design note 9: the barrier may outlast adoption.max_duration (ruling
// R-C2), so the heartbeat ticker runs until the adoption returns — the
// candidate and its member blobs stay protected until the commit.
func TestAdoption_HeartbeatRunsUntilTheCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 10*time.Second)
		r.busyAnchor(45)
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := wallclock.Unix()
		if err := r.ad.runRequest(ctx, r.request("v1"), runHooks{stop: context.Background()}); err != nil {
			t.Fatal(err)
		}
		hb := readHeartbeatAtDirect(t, filepath.Join(r.c.Dir(), "cache.db"), r.current(t))
		if hb < start+40 {
			t.Fatalf("last heartbeat %ds after the start; the ticker stopped at the 20s deadline", hb-start)
		}
	})
}

// Ruling R-C4 (phase C carry-forward): the end of the member pipeline frees
// the host slot only; the global slot stays held through the barrier and
// the commit (spec C4), which the C8 residual and R-C1 rely on.
func TestCheck_MembersDoneFreesOnlyTheHostSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		seedStaleAnchorAt(t, r.c, rigHost, rigSuite)
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{body: r.release("v1")}, HostLimiter: hostsem.New(8),
			Adopter: r.ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		tok := r.gate.AnchorStart(context.Background(), barrierKey) // keeps the adoption in the barrier
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		time.Sleep(time.Second)
		synctest.Wait()
		if g, h := chk.admission.usage(); g != 1 || h != 0 {
			t.Fatalf("in the barrier: %d global and %d host slots held, want 1 and 0", g, h)
		}
		if id := r.current(t); id != 0 {
			t.Fatalf("committed snapshot %d before the barrier let it", id)
		}
		tok.Done()
		chk.WaitForAdoptions()
		if g, h := chk.admission.usage(); g != 0 || h != 0 {
			t.Fatalf("after the commit: %d global and %d host slots held, want none", g, h)
		}
		if r.current(t) == 0 {
			t.Fatal("not adopted")
		}
	})
}

// Ruling R-C2 through the Checker: a shutdown while the adoption waits in
// the barrier commits nothing and records nothing (spec C7: shutdown is
// not a failure), and every slot is released.
func TestCheck_ShutdownInTheBarrierRecordsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		seedStaleAnchorAt(t, r.c, rigHost, rigSuite)
		life, stop := context.WithCancel(context.Background())
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{body: r.release("v1")}, HostLimiter: hostsem.New(8),
			Adopter: r.ad, LifetimeCtx: life, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		tok := r.gate.AnchorStart(context.Background(), barrierKey)
		defer tok.Done()
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		time.Sleep(time.Second)
		synctest.Wait()
		stop()
		chk.WaitForAdoptions()
		sf, err := r.c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
		if err != nil {
			t.Fatal(err)
		}
		if sf.CurrentSnapshotID != nil || sf.AdoptionFailCount != 0 || sf.AdoptionFailReason != nil {
			t.Fatalf("after a shutdown in the barrier: current set %v, failure count %d, reason set %v; want neither",
				sf.CurrentSnapshotID != nil, sf.AdoptionFailCount, sf.AdoptionFailReason != nil)
		}
		if g, h := chk.admission.usage(); g != 0 || h != 0 {
			t.Fatalf("slots leaked: %d/%d", g, h)
		}
		if st := r.gate.Stats(); st.Aborted != 1 {
			t.Fatalf("gate stats = %+v, want one aborted flip", st)
		}
	})
}

// A shutdown in the barrier is not a commit failure: no DB-failure
// sentinel (acu_adoption_total{outcome="db_failed"}) and no WARN
// "adoption: commit failed".
func TestAdopter_ShutdownInTheBarrierIsNotACommitFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		tok := r.gate.AnchorStart(context.Background(), barrierKey)
		defer tok.Done()
		stop, stopNow := context.WithCancel(context.Background())
		time.AfterFunc(5*time.Second, stopNow)
		err := r.ad.runRequest(context.Background(), r.request("v1"), runHooks{stop: stop})
		if errors.Is(err, ErrAdoptionDBFailed) || classifyAdoptionOutcome(err) == "db_failed" {
			t.Fatalf("a shutdown in the barrier is reported as a DB failure: %v", err)
		}
		if strings.Contains(r.logs.String(), "adoption: commit failed") {
			t.Fatalf("a shutdown in the barrier logged a commit failure:\n%s", r.logs.String())
		}
	})
}

// The first-adoption skip still commits (inline and detached).
func TestAdopter_FirstAdoptionSkipStillCommits(t *testing.T) {
	for _, detached := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx := context.Background()
			r := newBarrierRig(t, 10*time.Second, 0)
			rel := r.release("skip still commits")
			h, err := r.ad.writeBlobBytes(ctx, rel)
			if err != nil {
				t.Fatal(err)
			}
			path := rigSuite + "/InRelease"
			if detached {
				path = rigSuite + "/Release"
			}
			if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost,
				Path: path, BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
				t.Fatal(err)
			}
			if detached {
				err = r.ad.RunDetached(ctx, barrierSuite, rel, []byte("a detached signature"), "", "")
			} else {
				err = r.ad.Run(ctx, barrierSuite, rel, "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if evs := r.barrierEvents(); len(evs) != 1 || evs[0]["outcome"] != "not_needed" {
				t.Fatalf("detached %v: events %v, want one not_needed", detached, evs)
			}
			if r.current(t) == 0 {
				t.Fatalf("detached %v: the barrier was skipped but nothing was committed", detached)
			}
		})
	}
}

// Design review I2: a deadline that passes in the flip barrier (ruling
// R-C2) does not make the attempt timed_out; the commit's own error
// classifies it. Here the commit fails — its candidate was retired while
// the adoption waited — after adoption.max_duration (20 s) has passed: the
// outcome and the recorded reason are db_failed, and adoption_flip_barrier
// says commit_failed (design review M7).
func TestCheck_BarrierPastTheDeadlineKeepsTheCommitErrorsOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newBarrierRig(t, 10*time.Second, 0)
		members := newFakeFetcher()
		members.put(rigPkgsURL, r.pkgs)
		ad, err := NewAdopter(AdoptionConfig{Cache: r.c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 20 * time.Second, FlipGate: r.gate,
			Logger: slog.New(slog.NewJSONHandler(r.logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		seedStaleAnchorAt(t, r.c, rigHost, rigSuite)
		ring := observability.NewRing(10)
		rel := r.release("v1")
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{body: rel}, HostLimiter: hostsem.New(8),
			Adopter: ad, AdoptionRing: ring, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		tok := r.gate.AnchorStart(ctx, barrierKey) // a client reads the old anchor
		chk.Check(ctx, "http", rigHost, rigSuite)
		time.Sleep(25 * time.Second) // the adoption waits in the barrier; its deadline passed at 20 s
		hash := hashOf(rel)
		id, reused, err := r.c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
			CanonicalHost: rigHost, SuitePath: rigSuite, InReleaseHash: &hash})
		if err != nil || !reused {
			t.Fatalf("the adoption's candidate: id %d, reused %v, err %v", id, reused, err)
		}
		if _, err := r.c.ApplyLegacyAuditDecisions(ctx, []cache.LegacyAuditDecision{{SnapshotID: id, Outcome: cache.LegacyAuditQuarantine}}); err != nil {
			t.Fatal(err)
		}
		tok.Done() // the commit runs at 35 s, and fails: the candidate is retired
		chk.WaitForAdoptions()
		sf, err := r.c.GetSuiteFreshness(ctx, "http", rigHost, rigSuite)
		if err != nil {
			t.Fatal(err)
		}
		if sf.CurrentSnapshotID != nil || strOf(sf.AdoptionFailReason) != "db_failed" {
			t.Fatalf("current set %v, recorded reason %q; want no commit and db_failed",
				sf.CurrentSnapshotID != nil, strOf(sf.AdoptionFailReason))
		}
		if evs := ring.Snapshot(); len(evs) != 1 || evs[0].Outcome != "db_failed" {
			t.Fatalf("adoption ring %+v, want one db_failed attempt", evs)
		}
		if evs := r.barrierEvents(); len(evs) != 1 || evs[0]["outcome"] != "commit_failed" || evs[0]["held_requests"] != 0.0 {
			t.Fatalf("adoption_flip_barrier events = %v, want one commit_failed with the barrier's fields", evs)
		}
	})
}

// Design note 18 (design review M3): main closes the gate at the shutdown
// signal, while the lifetime context still runs. An adoption waiting in
// the barrier abandons its commit at once and records nothing: an
// abandoned flip is shutdown, never a failure (spec C7).
func TestCheck_GateCloseInTheBarrierRecordsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBarrierRig(t, 10*time.Second, 0)
		seedStaleAnchorAt(t, r.c, rigHost, rigSuite)
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{body: r.release("v1")}, HostLimiter: hostsem.New(8),
			Adopter: r.ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		tok := r.gate.AnchorStart(context.Background(), barrierKey)
		defer tok.Done()
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		time.Sleep(time.Second)
		synctest.Wait()
		closedAt := time.Now()
		r.gate.Close()
		chk.WaitForAdoptions()
		if d := time.Since(closedAt); d != 0 {
			t.Fatalf("the adoption ended %v after Close, want at once", d)
		}
		sf, err := r.c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
		if err != nil {
			t.Fatal(err)
		}
		if sf.CurrentSnapshotID != nil || sf.AdoptionFailCount != 0 || sf.AdoptionFailReason != nil {
			t.Fatalf("after Close: current set %v, failure count %d, reason set %v; want neither",
				sf.CurrentSnapshotID != nil, sf.AdoptionFailCount, sf.AdoptionFailReason != nil)
		}
		if g, h := chk.admission.usage(); g != 0 || h != 0 {
			t.Fatalf("slots leaked: %d/%d", g, h)
		}
		if st := r.gate.Stats(); st.Aborted != 1 {
			t.Fatalf("gate stats = %+v, want one aborted flip", st)
		}
	})
}

// Spec C4 admission bound with the C8 barrier (ruling R-C3, design review
// M15). A pool of two; two signed suites whose clients keep reading their
// anchors, so their adoptions sit in the barrier; a third signed suite that
// changes at the same time and queues for a slot. The busy adoptions keep
// their global slots through the barrier (R-C1) and hold them for at most
// the barrier's 140 s bound after their member pipelines; the queued suite
// is re-offered the slot the moment the first of them commits, so it is
// admitted within one tick + 140 s + D — inside one tick + (A + D), whose A
// adds the barrier to adoption.max_duration and the commit bound, which
// this simulation never reaches.
func TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		gate, err := flipgate.New(flipgate.Config{Quiet: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}, delay: map[string]time.Duration{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, FlipGate: gate, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		pkgsFor := func(label string) []byte {
			return fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_" + label + ".deb": testHex64})
		}
		rel := func(label string, pkgs []byte) []byte {
			return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		}
		pkgsURL := func(host string) string { return "http://" + host + rigSuite + "/main/binary-amd64/Packages" }
		up := simtest.NewUpstream()
		hosts := []string{"busy1", "busy2", "queued"}
		for _, host := range hosts {
			members.put(pkgsURL(host), pkgsFor("1-"+host))
			ref := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: rigSuite}
			if err := ad.Run(ctx, ref, rel("v1-"+host, pkgsFor("1-"+host)), "", ""); err != nil {
				t.Fatal(err)
			}
			members.put(pkgsURL(host), pkgsFor("2-"+host))
			answer := simtest.Serve(rel("v2-"+host, pkgsFor("2-"+host)))
			if host == "queued" {
				answer = simtest.After(time.Second, answer) // the busy suites take both slots first
			}
			up.Set("http://"+host+rigSuite+"/InRelease", answer)
		}
		members.started = map[string]time.Time{}
		ring := observability.NewRing(10)
		t0 := time.Now()
		var wg sync.WaitGroup
		for _, host := range hosts[:2] {
			key := flipgate.Key("http", host, rigSuite)
			wg.Add(1)
			go func() { // a client reads the anchor every 500 ms for ten minutes
				defer wg.Done()
				for time.Since(t0) < 10*time.Minute {
					tok := gate.AnchorStart(ctx, key)
					time.Sleep(50 * time.Millisecond)
					tok.Done()
					time.Sleep(450 * time.Millisecond)
				}
			}()
		}
		const refresh = 15 * time.Minute
		tick := refresh / 4
		chk, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Cooldown: time.Minute, Adopter: ad, AdoptionRing: ring, Logger: simtest.DiscardLogger(),
			rand: func() float64 { return 0.5 }})
		// 30 s after the first tick both busy adoptions are in the barrier:
		// two global slots held, no host slot, the third suite not admitted.
		time.Sleep(tick + 30*time.Second + 7*time.Millisecond)
		members.mu.Lock()
		_, admitted := members.started["queued"]
		members.mu.Unlock()
		if g, h := chk.admission.usage(); g != 2 || h != 0 || admitted {
			t.Fatalf("in the barrier: %d global and %d host slots held, queued suite admitted %v; want 2, 0, false", g, h, admitted)
		}
		time.Sleep(10*time.Minute - time.Since(t0))
		stop()
		wg.Wait()

		members.mu.Lock()
		started := maps.Clone(members.started)
		members.mu.Unlock()
		ends := map[string]time.Time{}
		for _, ev := range ring.Snapshot() {
			if ev.Outcome != "success" {
				t.Fatalf("adoption %s: %s (%s)", ev.Host, ev.Outcome, ev.Reason)
			}
			ends[ev.Host] = started[ev.Host].Add(time.Duration(ev.DurationSeconds * float64(time.Second)))
		}
		if len(ends) != 3 {
			t.Fatalf("adoptions that ended: %v, want all three", ends)
		}
		barrierMax := flipgate.PhaseAMax + flipgate.TokenWaitMax + flipgate.HoldMax
		first := ends["busy1"]
		for _, host := range hosts[:2] {
			if d := ends[host].Sub(started[host]); d > barrierMax {
				t.Errorf("%s held its slot for %v, more than the barrier's %v", host, d, barrierMax)
			}
			if ends[host].Before(first) {
				first = ends[host]
			}
		}
		if q := started["queued"]; q.Before(first) || q.Sub(first) > 2*time.Second {
			t.Errorf("the queued suite was admitted %v after the first busy commit, want within its 1 s anchor answer", q.Sub(first))
		}
		if bound := tick + barrierMax + checkDeadline; started["queued"].Sub(t0) > bound {
			t.Errorf("the queued suite was admitted %v after the start, bound one tick + 140 s + D = %v", started["queued"].Sub(t0), bound)
		}
	})
}
