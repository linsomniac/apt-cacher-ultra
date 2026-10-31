package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// seedStaleAnchorAt caches a stale client-created InRelease anchor
// (testHex64, request_count 1) and a suite row for suite on host, so any
// body upstream serves is "changed".
func seedStaleAnchorAt(t *testing.T, c *cache.Cache, host, suite string) {
	t.Helper()
	if err := seedStaleAnchorErr(context.Background(), c, host, suite); err != nil {
		t.Fatal(err)
	}
}

// seedStaleAnchorErr is seedStaleAnchorAt for a goroutine the test spawned,
// where t.Fatal may not run: it returns the first error.
func seedStaleAnchorErr(ctx context.Context, c *cache.Cache, host, suite string) error {
	stale := testHex64
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		return err
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &stale, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true, RequestCount: 1}); err != nil {
		return err
	}
	return c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite})
}

// Spec C4: with no free slot the check spawns nothing, reports
// first_adoption_queued (the rig suite has no current snapshot) and leaves
// last_success_at alone so the scheduler retries it on its next turn.
func TestCheck_NoSlotQueuesWithoutAdvancingSuccess(t *testing.T) {
	r := newRetryRig(t, 0)
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	r.up.set(r.release("v1", 0))
	r.check()
	if r.ver.calls() != 0 || lastResult(r) != "first_adoption_queued" {
		t.Fatalf("calls %d result %q, want 0 first_adoption_queued", r.ver.calls(), lastResult(r))
	}
	if row := r.row(); row.LastSuccessAt != nil || row.InReleaseChangeSeenAt == nil || row.LastCheckAt == nil {
		t.Fatalf("queued check state: success %v seen %v check %v", row.LastSuccessAt, row.InReleaseChangeSeenAt, row.LastCheckAt)
	}
	held.release()
	r.check()
	if r.ver.calls() != 1 || r.row().CurrentSnapshotID == nil {
		t.Fatalf("not adopted once a slot was free: calls %d", r.ver.calls())
	}
	if g, h := r.checker.admission.usage(); g != 0 || h != 0 {
		t.Fatalf("slots leaked: %d/%d", g, h)
	}
}

// The detached check queues the same way (both form checkers carry the
// no-slot block), and a queued check of an adopted suite restores the
// previous last_success_at rather than clearing it.
func TestCheck_NoSlotQueuesDetachedToo(t *testing.T) {
	r := newRetryRig(t, 0)
	seedDetachedAnchors(t, r.cache)
	r.up.setAt(rigReleaseURL, r.releaseText("det1", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("sig one"))
	r.check()
	adopted := r.row()
	if adopted.CurrentSnapshotID == nil || adopted.LastSuccessAt == nil {
		t.Fatal("setup: detached v1 not adopted")
	}
	r.clock.advance(time.Hour)
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	r.up.setAt(rigReleaseURL, r.releaseText("det2", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("sig two"))
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_queued" {
		t.Fatalf("calls %d result %q, want 1 changed_queued", r.ver.calls(), lastResult(r))
	}
	row := r.row()
	if i64Of(row.LastSuccessAt) != *adopted.LastSuccessAt || i64Of(row.LastCheckAt) != r.clock.now().Unix() {
		t.Fatalf("queued detached check: success %d (want %d), check %d", i64Of(row.LastSuccessAt), *adopted.LastSuccessAt, i64Of(row.LastCheckAt))
	}
	held.release()
	r.check()
	if r.ver.calls() != 2 || *r.row().CurrentSnapshotID == *adopted.CurrentSnapshotID {
		t.Fatalf("det2 not adopted once a slot was free: calls %d", r.ver.calls())
	}
}

// Replaces TestAdopter_ConcurrencyCap: max_concurrent_adoptions bounds
// adoptions across hosts, and a queued suite runs once the slot frees.
func TestCheck_GlobalSlotCapsConcurrentAdoptions(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pending := make(chan blockReq, 8)
	bf := &blockingFetcher{pending: pending}
	up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
	hosts := []string{"a.example", "b.example"}
	for i, host := range hosts {
		// Each host declares its own Packages body: an identical member
		// would be reused from the pool after A's adoption and B would
		// never reach the fetch this test waits for.
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat(string(rune('a'+i)), 64)})
		seedStaleAnchorAt(t, c, host, rigSuite)
		bf.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
		up.setAt("http://"+host+rigSuite+"/InRelease", signedRelease("cap", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs}))
	}
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: bf, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 1,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
		Adopter: ad, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	chk.Check(ctx, "http", hosts[0], rigSuite)
	var aReq blockReq
	select {
	case aReq = <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("A never reached its member fetch")
	}
	// B's refusal is read from its result and the slot count, not
	// inferred from a timing window (deferred minor B-29).
	if rep := chk.check(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: hosts[1], SuitePath: rigSuite}, checkOpts{}); rep.result != "first_adoption_queued" {
		t.Fatalf("B's check = %q, want first_adoption_queued while the only global slot is held", rep.result)
	}
	if g, _ := chk.admission.usage(); g != 1 {
		t.Fatalf("%d global slots in use, want 1 (A's)", g)
	}
	close(aReq.release)
	chk.WaitForAdoptions()
	chk.Check(ctx, "http", hosts[1], rigSuite)
	select {
	case bReq := <-pending:
		close(bReq.release)
	case <-time.After(2 * time.Second):
		t.Fatal("B never started after A released the slot")
	}
	chk.WaitForAdoptions()
	for _, host := range hosts {
		if sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite); err != nil || sf.CurrentSnapshotID == nil {
			t.Errorf("%s not adopted (err %v)", host, err)
		}
	}
}

// Replaces TestRepairSkippedMembers_RespectsConcurrencyCap: the repair
// pass takes the same global slot as adoptions (codex review finding),
// without waiting for it.
func TestCheck_RepairObeysTheGlobalSlot(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	setupFF := newFakeFetcher()
	setupAd, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: setupFF, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	const suitePath = "/ubuntu/dists/n"
	hosts := [2]string{"a.example", "b.example"}
	var goods [2][]byte
	for i, host := range hosts {
		goods[i] = []byte(strings.Repeat(string(rune('A'+i)), 100))
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
		releaseText, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs, "Contents-amd64.gz": goods[i]})
		base := "http://" + host + suitePath + "/"
		setupFF.put(base+"main/binary-amd64/Packages", pkgs)
		setupFF.put(base+"Contents-amd64.gz", []byte(strings.Repeat("z", 90))) // stale generation
		suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suitePath}
		trackSuite(t, c, suite)
		// The adoption stores an ETag, so the check's GET is conditional and
		// the upstream's 304 is not_modified: a 304 to a GET without
		// validators is a failed check (final fix wave FW-3 (c)).
		if err := setupAd.Run(ctx, suite, releaseText, `"e-`+host+`"`, ""); err != nil {
			t.Fatalf("setup Run(%s): %v", host, err)
		}
	}
	pending := make(chan blockReq, 8)
	bf := &blockingFetcher{pending: pending}
	for i, host := range hosts {
		bf.put("http://"+host+suitePath+"/by-hash/SHA256/"+shaOf(goods[i]), goods[i])
	}
	repAd, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: bf, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 1, RepairSkippedMembers: true,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	chk, err := New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: repAd, Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	chk.Check(ctx, "http", hosts[0], suitePath)
	var aReq blockReq
	select {
	case aReq = <-pending:
	case <-time.After(2 * time.Second):
		t.Fatal("repair A never reached its fetch")
	}
	// B's repair is deferred for want of a slot: read from its line and
	// the slot count, not a timing window (deferred minor B-29), and
	// counted (B-31).
	deferredBefore := hostCounter(t, "acu_repair_deferred_total", hosts[1])
	chk.Check(ctx, "http", hosts[1], suitePath)
	if got := hostCounter(t, "acu_repair_deferred_total", hosts[1]) - deferredBefore; got != 1 {
		t.Fatalf("acu_repair_deferred_total{host=b.example} rose by %v, want 1", got)
	}
	deferred := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"freshness_repair_deferred_no_adoption_slot"`) &&
			strings.Contains(line, `"canonical_host":"b.example"`) {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("repair B was not deferred while the only global slot was held")
	}
	if g, _ := chk.admission.usage(); g != 1 {
		t.Fatalf("%d global slots in use, want 1 (repair A's)", g)
	}
	close(aReq.release)
	chk.WaitForAdoptions()
	chk.Check(ctx, "http", hosts[1], suitePath)
	select {
	case bReq := <-pending:
		close(bReq.release)
	case <-time.After(2 * time.Second):
		t.Fatal("repair B never ran after A released the slot")
	}
	chk.WaitForAdoptions()
	for i, host := range hosts {
		sf, err := c.GetSuiteFreshness(ctx, "http", host, suitePath)
		if err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("host %d: %v", i, err)
		}
		if rows, err := c.ListRepairableSkippedMembers(ctx, *sf.CurrentSnapshotID); err != nil || len(rows) != 0 {
			t.Errorf("host %d: %d skip rows remain (err %v)", i, len(rows), err)
		}
	}
}

// A panic while deciding whether an unchanged suite needs healing runs
// inside the check's recover: the suite mutex and any admission slot are
// released (a wedged mutex would stop the suite for the process lifetime).
func TestCheck_RepairDecisionPanicReleasesTheSuite(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: not adopted")
	}
	r.adopter.repairSkippedMembers = true
	r.adopter.reconcilePendingHook = func() { panic("repair decision bug") }
	r.check() // 304: unchanged, so the repair decision runs and panics
	v, _ := r.checker.locks.Load(suiteKey("http", rigHost, rigSuite))
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		t.Fatal("suite mutex wedged after a panic in the repair decision")
	}
	mu.Unlock()
	if g, h := r.checker.admission.usage(); g != 0 || h != 0 {
		t.Fatalf("slots leaked: %d/%d", g, h)
	}
	r.adopter.reconcilePendingHook = nil
	r.check()
	if lastResult(r) != "not_modified" {
		t.Fatalf("the suite did not recover: last result %q", lastResult(r))
	}
}

// POST /reconcile answers 409 when no slot is free (spec C4): Reconcile
// returns false instead of waiting.
func TestReconcile_NoSlotReturnsFalse(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	r.check()
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	if r.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("reconcile triggered without a free slot")
	}
	held.release()
	if !r.checker.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("reconcile refused with a free slot")
	}
	r.checker.WaitForAdoptions()
}

// Spec C4 reserved slot, through the Checker: with one slot taken by a
// first adoption elsewhere, a suite with no signed current snapshot is
// queued and a suite with one is admitted.
func TestCheck_ReservedSlotGoesToSignedCurrent(t *testing.T) {
	r := newRetryRig(t, 0) // MaxConcurrent: 2
	other := mustAcquire(t, r.checker.admission, "other", false)
	r.up.set(r.release("v1", 0))
	r.check()
	if r.ver.calls() != 0 || lastResult(r) != "first_adoption_queued" {
		t.Fatalf("never-adopted suite: calls %d result %q, want 0 first_adoption_queued (the last slot is reserved)", r.ver.calls(), lastResult(r))
	}
	other.release()
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("v1 not adopted once both slots were free")
	}
	other = mustAcquire(t, r.checker.admission, "other", false)
	defer other.release()
	r.up.set(r.release("v2", 0))
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("a signed-current suite was not admitted to the reserved slot: calls %d", r.ver.calls())
	}
}

// A healthy unchanged suite (nothing to repair or reconcile) never asks
// for a slot, and its complete snapshot is memoised after one look.
func TestCheck_HealthySuiteAsksForNoRepairSlot(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.repairSkippedMembers = true
	// Clearsigned: the unchanged check reconciles, which parses only
	// clearsigned InRelease (phase D).
	r.up.set(r.release("v1", 0))
	r.check()
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	defer held.release()
	r.check() // 304: unchanged
	if strings.Contains(r.logs.String(), "freshness_repair_deferred_no_adoption_slot") {
		t.Fatal("a healthy suite asked for a repair slot")
	}
	if _, ok := r.adopter.reconciledSnapshots.Load(*r.row().CurrentSnapshotID); !ok {
		t.Fatal("a complete snapshot was not memoised after its first reconcile look")
	}
}

// panicOnMessage is a slog.Handler that panics on records whose message
// is msg.
type panicOnMessage struct{ msg string }

func (h panicOnMessage) Enabled(context.Context, slog.Level) bool { return true }
func (h panicOnMessage) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		panic("log handler bug")
	}
	return nil
}
func (h panicOnMessage) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h panicOnMessage) WithGroup(string) slog.Handler      { return h }

// A panic after the check took an adoption slot (here in the
// freshness_check line of a changed check) releases the slot and starts
// no adoption.
func TestCheck_PanicAfterAdmissionReleasesTheLease(t *testing.T) {
	r := newRetryRig(t, 0)
	r.up.set(r.release("v1", 0))
	chk, err := New(Config{Cache: r.cache, Fetcher: r.up, HostLimiter: hostsem.New(8), Adopter: r.adopter,
		Logger: slog.New(panicOnMessage{msg: "freshness_check"}), now: r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	chk.Check(context.Background(), "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	if g, h := chk.admission.usage(); g != 0 || h != 0 {
		t.Fatalf("lease leaked after a panic: %d/%d", g, h)
	}
	if r.ver.calls() != 0 {
		t.Fatal("an adoption ran after its check panicked")
	}
}

type stallFetcher struct{}

func (stallFetcher) Fetch(ctx context.Context, _ *fetch.Target, _ fetch.FetchDst) (*fetch.FetchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// adoption.max_duration bounds an adoption whose member never arrives:
// it fails as timed_out, is recorded, and releases its slots.
func TestAdoption_MaxDurationBoundsAStalledAdoption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		seedStaleAnchorAt(t, c, rigHost, rigSuite)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		rel := signedRelease("slow", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: stallFetcher{}, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 20 * time.Minute,
			Logger: discardLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		chk, err := New(Config{Cache: c, Fetcher: &changedUpstream{body: rel}, HostLimiter: hostsem.New(8),
			Adopter: ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		chk.WaitForAdoptions()
		if d := time.Since(start); d < 20*time.Minute || d >= 21*time.Minute {
			t.Fatalf("adoption ended after %v, want the configured 20m", d)
		}
		sf, err := c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
		if err != nil || strOf(sf.AdoptionFailReason) != "timed_out" {
			t.Fatalf("record reason = %q (err %v), want timed_out", strOf(sf.AdoptionFailReason), err)
		}
		if g, h := chk.admission.usage(); g != 0 || h != 0 {
			t.Fatalf("slots leaked: %d/%d", g, h)
		}
	})
}

// stallOnFetcher serves the canned bodies of fakeFetcher, except that a
// fetch of stall waits for its context to end (an upstream slower than
// adoption.max_duration); like the real client, it fails any fetch under a
// context that has ended.
type stallOnFetcher struct {
	*fakeFetcher
	stall string
}

func (f *stallOnFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if target.URL == f.stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.fakeFetcher.Fetch(ctx, target, dst)
}

// trailingOptionalRelease is a signed rig Release whose IndexTarget
// (main/binary-amd64/Packages) comes first and its optional members
// (i18n Translations, never IndexTargets) after it, in that order, and a
// fetcher that serves them all but stalls on the first optional one.
func trailingOptionalRelease(t *testing.T, optional int) ([]byte, *stallOnFetcher) {
	t.Helper()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	f := newFakeFetcher()
	f.put(rigPkgsURL, pkgs)
	var sb strings.Builder
	sb.WriteString("Origin: Test\nSuite: x\nSHA256:\n")
	fmt.Fprintf(&sb, " %s %d main/binary-amd64/Packages\n", shaOf(pkgs), len(pkgs))
	for i := range optional {
		body := []byte(fmt.Sprintf("Package: foo\nDescription-l%d: foo\n", i))
		p := fmt.Sprintf("main/i18n/Translation-l%d", i)
		fmt.Fprintf(&sb, " %s %d %s\n", shaOf(body), len(body), p)
		f.put("http://"+rigHost+rigSuite+"/"+p, body)
	}
	return simtest.ClearsignedFixture(sb.String()),
		&stallOnFetcher{fakeFetcher: f, stall: "http://" + rigHost + rigSuite + "/main/i18n/Translation-l0"}
}

// The final review's R2 I-2: adoption.max_duration passing while the member
// pipeline fetches the optional members after the last IndexTarget ends the
// attempt timed_out. Before, each remaining optional member was skipped as
// an integrity failure (WARN adoption_member_skipped, a repair record) and
// the snapshot committed as a success: an operator alerting on timed_out
// never learnt max_duration was too short.
func TestAdoption_DeadlineInTrailingOptionalMembersIsTimedOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		seedStaleAnchorAt(t, c, rigHost, rigSuite)
		rel, members := trailingOptionalRelease(t, 1)
		logs := &lockedBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxDuration: 20 * time.Minute,
			TolerateOptionalMemberFailures: true, RepairSkippedMembers: true, Logger: logger,
		})
		if err != nil {
			t.Fatal(err)
		}
		chk, err := New(Config{Cache: c, Fetcher: &changedUpstream{body: rel}, HostLimiter: hostsem.New(8),
			Adopter: ad, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		chk.WaitForAdoptions()
		sf, err := c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
		if err != nil {
			t.Fatal(err)
		}
		if sf.CurrentSnapshotID != nil || strOf(sf.AdoptionFailReason) != "timed_out" {
			t.Fatalf("committed %v, record reason %q; want no commit and timed_out", sf.CurrentSnapshotID != nil,
				strOf(sf.AdoptionFailReason))
		}
		if recs := logRecords(t, logs, "adoption_member_skipped"); len(recs) != 0 {
			t.Fatalf("adoption_member_skipped lines %v, want none", recs)
		}
	})
}

// The final review's R2 I-2, at shutdown: the lifetime context ending while
// the member pipeline fetches optional members abandons the attempt (INFO
// adoption_abandoned_at_shutdown) without a WARN adoption_member_skipped
// for each remaining member (an Ubuntu suite has hundreds; design note 18:
// a shutdown pages nobody), and commits nothing.
func TestAdoption_ShutdownInOptionalMembersSkipsNoMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		seedStaleAnchorAt(t, c, rigHost, rigSuite)
		rel, members := trailingOptionalRelease(t, 5)
		logs := &lockedBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxDuration: 20 * time.Minute,
			TolerateOptionalMemberFailures: true, RepairSkippedMembers: true, Logger: logger,
		})
		if err != nil {
			t.Fatal(err)
		}
		life, stop := context.WithCancel(context.Background())
		defer stop()
		chk, err := New(Config{Cache: c, Fetcher: &changedUpstream{body: rel}, HostLimiter: hostsem.New(8),
			Adopter: ad, LifetimeCtx: life, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		chk.Check(context.Background(), "http", rigHost, rigSuite)
		synctest.Wait() // the adoption waits in the first optional member's fetch
		stop()
		chk.WaitForAdoptions()
		for _, level := range []string{"WARN", "ERROR"} {
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, `"level":"`+level+`"`) {
					t.Errorf("logged at %s during a shutdown: %s", level, line)
				}
			}
		}
		if recs := logRecords(t, logs, "adoption_abandoned_at_shutdown"); len(recs) != 1 {
			t.Errorf("adoption_abandoned_at_shutdown lines %v, want one", recs)
		}
		sf, err := c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
		if err != nil {
			t.Fatal(err)
		}
		if sf.CurrentSnapshotID != nil || sf.AdoptionFailCount != 0 {
			t.Fatalf("current %v, failure count %d after a shutdown; want no commit and no record",
				sf.CurrentSnapshotID, sf.AdoptionFailCount)
		}
	})
}

// cancelAfterFetcher answers as fakeFetcher does and cancels once it has
// answered after: a shutdown arriving as that member's fetch ends.
type cancelAfterFetcher struct {
	*fakeFetcher
	after  string
	cancel context.CancelFunc
}

func (f *cancelAfterFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	res, err := f.fakeFetcher.Fetch(ctx, target, dst)
	if target.URL == f.after {
		f.cancel()
	}
	return res, err
}

// The final review's R2 I-2: the member loop itself stops once its context
// has ended. Here the shutdown arrives as a member's fetch ends in a 404,
// a skip that writes nothing and so returns no error: no later member is
// filtered (WARN adoption_member_skipped, arch_not_in_allowlist), fetched
// or skipped, and nothing commits.
func TestAdoption_EndedContextStopsTheMemberLoop(t *testing.T) {
	env := newAdoptionTestEnv(t)
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.fail404(base + "main/i18n/Translation-de")
	tr, arm := []byte("Package: foo\nDescription-de: foo\n"), []byte("Package: bar\n")
	var sb strings.Builder
	sb.WriteString("Origin: Test\nSuite: noble\nSHA256:\n")
	fmt.Fprintf(&sb, " %s %d main/binary-amd64/Packages\n", shaOf(pkgs), len(pkgs))
	fmt.Fprintf(&sb, " %s %d main/i18n/Translation-de\n", shaOf(tr), len(tr))
	fmt.Fprintf(&sb, " %s %d main/binary-arm64/Packages\n", shaOf(arm), len(arm))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedBuffer{}
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8),
		Fetcher:       &cancelAfterFetcher{fakeFetcher: env.fetcher, after: base + "main/i18n/Translation-de", cancel: cancel},
		Architectures: []string{"amd64"}, TolerateOptionalMemberFailures: true,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ad.runShared(ctx, env.suite, &adoptionPayload{form: adoptionFormInline, inlineBytes: []byte(sb.String()), stop: ctx}, "", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runShared = %v, want the shutdown (context.Canceled)", err)
	}
	for _, rec := range logRecords(t, logs, "adoption_member_skipped") {
		if rec["path"] != "main/i18n/Translation-de" {
			t.Errorf("adoption_member_skipped %v after the context ended, want only the 404 that ended with it", rec)
		}
	}
	sf, err := env.cache.GetSuiteFreshness(context.Background(), "http", env.suite.CanonicalHost, env.suite.SuitePath)
	if err == nil && sf.CurrentSnapshotID != nil {
		t.Fatalf("current snapshot %d after the context ended, want none", *sf.CurrentSnapshotID)
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		t.Fatal(err)
	}
}

// Spec C4: CommitAdoption runs under context.WithoutCancel, so the
// adoption deadline (adoption.max_duration) cannot abort the commit: an
// adoption whose deadline passes after its member pipeline still commits.
func TestAdoption_CommitSurvivesTheAdoptionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		seedStaleAnchorAt(t, c, rigHost, rigSuite)
		members := newFakeFetcher()
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members.put(rigPkgsURL, pkgs)
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		req := &adoptionRequest{suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite},
			form: adoptionFormInline, bytes: datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})}
		// membersDone runs right before the commit: waiting there for the
		// deadline models it expiring at the worst moment.
		if err := ad.runRequest(ctx, req, runHooks{membersDone: func() { <-ctx.Done() }, stop: context.Background()}); err != nil {
			t.Fatalf("commit aborted by the adoption deadline: %v", err)
		}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("setup: the deadline had not passed before the commit (ctx err %v)", ctx.Err())
		}
		if sf, err := c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite); err != nil || sf.CurrentSnapshotID == nil {
			t.Fatalf("no current snapshot after the commit (err %v)", err)
		}
	})
}

// Final review C M-1: a shutdown (the lifetime context cancelled) before
// the commit starts commits nothing, so the stop is not held for up to
// commitTimeout (the shipped unit's TimeoutStopSec is 35 s). The candidate
// stays uncommitted (GC reaps it after its stale grace).
func TestAdoption_ShutdownBeforeTheCommitCommitsNothing(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &adoptionRequest{suite: env.suite, form: adoptionFormInline,
		bytes: datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})}
	// membersDone runs right before the commit: cancelling there models a
	// shutdown at the worst moment.
	err := env.adopter.runRequest(ctx, req, runHooks{membersDone: cancel, stop: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runRequest = %v, want a shutdown error (context.Canceled)", err)
	}
	sf, err := env.cache.GetSuiteFreshness(context.Background(), "http", env.suite.CanonicalHost, env.suite.SuitePath)
	if err == nil && sf.CurrentSnapshotID != nil {
		t.Fatalf("current snapshot %d after a shutdown before the commit, want none", *sf.CurrentSnapshotID)
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		t.Fatal(err)
	}
}

// Spec C4: the host slot is released when the member pipeline ends,
// before the commit.
func TestAdoption_HostSlotFreedBeforeCommit(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	called := false
	membersDone := func() {
		called = true
		sf, err := env.cache.GetSuiteFreshness(context.Background(), "http", "archive.ubuntu.com", "/ubuntu/dists/noble")
		if err == nil && sf.CurrentSnapshotID != nil {
			t.Error("the commit ran before the host slot was released")
		}
	}
	req := &adoptionRequest{suite: env.suite, form: adoptionFormInline,
		bytes: datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})}
	if err := env.adopter.runRequest(context.Background(), req, runHooks{membersDone: membersDone}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("membersDone never ran")
	}
}
