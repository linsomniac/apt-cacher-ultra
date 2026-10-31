package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Deferred minor E-21: the detached twin of
// TestCheck_UsesSnapshotHashForComparison. A detached suite's Release is
// compared with its current snapshot's release_hash, not the url_path
// anchor's blob: the same Release is unchanged, and its signature is never
// fetched.
func TestCheck_UsesDetachedSnapshotHashForComparison(t *testing.T) {
	fc := newFakeCache()
	body, sig := []byte("Origin: Test\nSuite: x\n"), armoredSig("the signature")
	rel, sigHash, stale := hashOf(body), hashOf(sig), testHex64
	id := int64(7)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x",
		Form: cache.FormDetached, ReleaseHash: &rel, ReleaseGPGHash: &sigHash})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	for path, h := range map[string]*string{"/r/dists/x/Release": &stale, "/r/dists/x/Release.gpg": &sigHash} {
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: h,
			UpstreamURL: "http://h" + path, IsMetadata: true})
	}
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/Release", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 200, Body: body}, nil
	})
	up.Set("http://h/r/dists/x/Release.gpg", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 200, Body: sig}, nil
	})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger(),
		now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}, checkOpts{})
	if rep.result != "unchanged" {
		t.Fatalf("result %q, want unchanged (the snapshot's Release, not the stale url_path blob)", rep.result)
	}
	if n := len(up.Calls("http://h/r/dists/x/Release.gpg")); n != 0 {
		t.Fatalf("%d Release.gpg fetches, want none for an unchanged Release", n)
	}
	if row, _ := fc.suite("http", "h", "/r/dists/x"); row.InReleaseChangeSeenAt != nil {
		t.Fatalf("inrelease_change_seen_at = %d, want nil", *row.InReleaseChangeSeenAt)
	}
}

// Deferred minor E-32: the spec C7 hold — an active record, not yet due,
// for this very content — has one spelling.
func TestRetryGate_Holds(t *testing.T) {
	const k = "key-of-the-failing-content"
	for _, tc := range []struct {
		name string
		gate retryGate
		key  string
		want bool
	}{
		{"active, not due, same content", retryGate{active: true, hash: k}, k, true},
		{"no record", retryGate{hash: k}, k, false},
		{"due", retryGate{active: true, due: true, hash: k}, k, false},
		{"other content", retryGate{active: true, hash: k}, "other", false},
	} {
		if got := tc.gate.holds(tc.key); got != tc.want {
			t.Errorf("%s: holds = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Deferred minor E-27 (survivor C3): a probe sends no validators, so a 304
// answering it is a protocol violation — unknown, never present.
func TestProbe_A304IsUnknown(t *testing.T) {
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/InRelease", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 304}, nil
	})
	c, err := New(Config{Cache: newFakeCache(), Fetcher: up, HostLimiter: hostsem.New(1), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	o := c.probe(context.Background(), suite, c.anchorURLsFor(suite, nil), "InRelease")
	if o.class != classUnknown || o.status != 304 || !errors.Is(o.err, errUnexpectedStatus) {
		t.Fatalf("probe = %v (status %d, err %v), want unknown: a 304 to a request without validators", o.class, o.status, o.err)
	}
}

// Deferred minor E-28 (survivors X2, X3; design review I-1): a cross-form
// request's failure record keeps no validators — they belong to another
// anchor file than the one the current form conditional-GETs — for the
// unsigned suite's upgrade probe and for the probe tree of an absent
// anchor (lateralInline's was pinned already). The rig upstream gives
// every 200 an ETag.
func TestCheck_CrossFormFailureRecordsKeepNoValidators(t *testing.T) {
	noValidators := func(t *testing.T, r *retryRig) {
		t.Helper()
		row := r.row()
		if strOf(row.AdoptionFailReason) != "gpg_failed" {
			t.Fatalf("setup: record %q/%d, want the failed attempt's", strOf(row.AdoptionFailReason), row.AdoptionFailCount)
		}
		if row.AdoptionFailETag != nil || row.AdoptionFailLastMod != nil {
			t.Fatalf("record validators %q / %q, want none", strOf(row.AdoptionFailETag), strOf(row.AdoptionFailLastMod))
		}
	}
	t.Run("the upgrade probe", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adoptUnsigned("u1")
		in := r.release("signed", 0)
		r.ver.reject(hashOf(in))
		r.up.setFail(rigAnchorURL, nil)
		r.up.set(in)
		r.check()
		noValidators(t, r)
	})
	t.Run("the absent anchor's probe tree", func(t *testing.T) {
		r := newRetryRig(t, 0)
		r.adopt()
		rel, sig := r.releaseText("v2", 0), armoredSig("v2 sig")
		r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig))
		r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
		r.up.setAt(rigReleaseURL, rel)
		r.up.setAt(rigReleaseGPGURL, sig)
		r.clock.advance(time.Minute)
		r.check()                        // the first observation
		r.clock.advance(6 * time.Minute) // due, and past the confirmation gap: the tree runs
		r.check()
		noValidators(t, r)
	})
}

// Deferred minor E-30: the stale WARN names the stale suites it leaves out
// (paced by their record), so its two counts add up to the gauge's
// adopted="true" value.
func TestTick_StaleWarnNamesThePacedSuitesLeftOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old, retry := now.Add(-5*time.Hour).Unix(), now.Add(time.Hour).Unix()
	absent, id := cache.ReasonAnchorAbsent, int64(3)
	fc := newFakeCache()
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/paced", LastSuccessAt: &old,
		CurrentSnapshotID: &id, AdoptionFailCount: 1, AdoptionFailReason: &absent, AdoptionFailHash: &anchorAbsentKey,
		AdoptionNextRetryAt: &retry})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/frozen", LastSuccessAt: &old,
		CurrentSnapshotID: &id})
	var logs lockedBuffer
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Refresh: time.Hour,
		Adopter: &Adopter{}, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.tick(context.Background(), newSchedQueues())
	warns := logRecords(t, &logs, "freshness: adopted suites stale beyond threshold")
	if len(warns) != 1 || warns[0]["adopted_stale"] != float64(1) || warns[0]["paced_stale"] != float64(1) {
		t.Fatalf("stale WARNs %v, want one with adopted_stale 1 and paced_stale 1", warns)
	}
	if got := renderedMetricLabels("acu_freshness_stale_suites", `adopted="true"`); got != "2" {
		t.Fatalf(`acu_freshness_stale_suites{adopted="true"} = %q, want 2`, got)
	}
}

// Deferred minor E-31: one predicate for a signed current snapshot; an
// unknown or empty form is not signed.
func TestSignedForm(t *testing.T) {
	for form, want := range map[cache.AnchorForm]bool{cache.FormInline: true, cache.FormDetached: true, cache.FormUnsigned: false, "": false} {
		if got := signedForm(form); got != want {
			t.Errorf("signedForm(%q) = %v, want %v", form, got, want)
		}
	}
}

// Deferred minor E-31: Reconcile counts a current snapshot it cannot read
// as unsigned — it never takes the reserved slot — and says so at WARN.
func TestReconcile_AnUnreadableSnapshotIsLoggedAndCountsUnsigned(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	fc := &snapReadFailCache{Cache: r.cache}
	fc.fail.Store(true)
	var logs lockedBuffer
	chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	held := mustAcquire(t, chk.admission, "other", true)
	defer held.release()
	if chk.Reconcile(context.Background(), "http", rigHost, rigSuite) {
		t.Fatal("a reconcile whose snapshot could not be read took the reserved slot")
	}
	warns := logRecords(t, &logs, "reconcile_snapshot_read_failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["snapshot_id"] != float64(*r.row().CurrentSnapshotID) ||
		warns[0]["canonical_scheme"] != "http" || warns[0]["suite_path"] != rigSuite ||
		!strings.Contains(fmt.Sprint(warns[0]["err"]), "injected snapshot read failure") {
		t.Fatalf("WARN records %v, want one naming the snapshot, the suite and the error", warns)
	}
}

// The H1 final review's prod minor 1: Reconcile logs its snapshot read
// failure while it holds the suite mutex, so the log runs under
// unloggedPanic. A broken log handler is counted once and Reconcile
// returns; without the boundary the panic reached net/http with the mutex
// held, and every later check of the suite answered busy for the life of
// the process.
func TestReconcile_ABrokenLoggerLeavesTheSuiteFree(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	fc := &snapReadFailCache{Cache: r.cache}
	fc.fail.Store(true)
	logger := slog.New(panicLogHandler{panicOn: func(rec slog.Record) bool { return rec.Message == "reconcile_snapshot_read_failed" }})
	chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8), Adopter: r.adopter, Logger: logger,
		now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	held := mustAcquire(t, chk.admission, "other", true) // the reserved slot is left: Reconcile answers false
	defer held.release()
	before := unlogged()
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Errorf("Reconcile panicked: %v", v)
			}
		}()
		if chk.Reconcile(context.Background(), "http", rigHost, rigSuite) {
			t.Fatal("a reconcile whose snapshot could not be read took the reserved slot")
		}
	}()
	if got := unlogged() - before; got != 1 {
		t.Errorf("unlogged panics rose by %v, want 1 (the read failure's line)", got)
	}
	release, ok := chk.HoldSuite(cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite})
	if !ok {
		t.Fatal("the suite is still held after Reconcile returned")
	}
	release(false)
}

// Deferred minor E-37: a future last_success_at is reported by a check
// that runs, not by a client check the cooldown turns away.
func TestCheck_FutureLastSuccessIsReportedBelowTheCooldown(t *testing.T) {
	var clock atomic.Int64
	clock.Store(1_800_000_000)
	future, recent := clock.Load()+3600, clock.Load()-10
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x",
		LastSuccessAt: &future, LastCheckAt: &recent})
	c, err := New(Config{Cache: fc, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return time.Unix(clock.Load(), 0) }})
	if err != nil {
		t.Fatal(err)
	}
	steps := func() float64 { return renderedFloat(t, renderedMetric("acu_clock_steps_detected_total")) }
	before := steps()
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := steps() - before; got != 0 {
		t.Fatalf("a check inside the cooldown counted %v clock steps, want 0", got)
	}
	clock.Add(120) // past the cooldown
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := steps() - before; got != 1 {
		t.Fatalf("the check past the cooldown counted %v clock steps, want 1", got)
	}
}

// releaseGPGFailCache fails LookupURL for a Release.gpg row while fail is
// set, calling onFail first.
type releaseGPGFailCache struct {
	Cache
	fail   atomic.Bool
	onFail func()
}

func (c *releaseGPGFailCache) LookupURL(ctx context.Context, scheme, host, path string) (*cache.URLPath, error) {
	if c.fail.Load() && strings.HasSuffix(path, "/Release.gpg") {
		if c.onFail != nil {
			c.onFail()
		}
		return nil, errors.New("injected Release.gpg row read failure")
	}
	return c.Cache.LookupURL(ctx, scheme, host, path)
}

// Deferred minor GD-6: when an adopted suite's absent anchor sends it down
// the probe tree and its cached anchor rows cannot be read, the WARN names
// the scheme (an http suite and its https namesake differ), and a read a
// shutdown cut short is DEBUG, as checkFirst's is (ruling PG-35).
func TestAnchorAbsent_AnchorRowReadFailure(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			r := newRetryRig(t, 0)
			r.adopt()
			fc := &releaseGPGFailCache{Cache: r.cache}
			var logs lockedBuffer
			chk, err := New(Config{Cache: fc, Fetcher: r.up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
				Adopter: r.adopter, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
				now: r.clock.now, rand: func() float64 { return 0.5 }})
			if err != nil {
				t.Fatal(err)
			}
			r.up.setFail(rigAnchorURL, &fetch.StatusError{Code: 404})
			r.clock.advance(time.Minute)
			chk.Check(context.Background(), "http", rigHost, rigSuite) // the first observation
			chk.WaitForAdoptions()
			r.clock.advance(6 * time.Minute) // due, past the confirmation gap: the tree reads the rows
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if shutdown {
				fc.onFail = cancel
			}
			fc.fail.Store(true)
			chk.Check(ctx, "http", rigHost, rigSuite)
			chk.WaitForAdoptions()
			warns := logRecords(t, &logs, "freshness_read_anchor_rows_failed")
			ended := logRecords(t, &logs, "freshness_check_ended_by_shutdown")
			if shutdown {
				if len(warns) != 0 || len(ended) != 1 || ended[0]["step"] != "read the anchor rows" || ended[0]["canonical_scheme"] != "http" {
					t.Fatalf("WARNs %v, shutdown lines %v; want no WARN and one DEBUG for the read", warns, ended)
				}
				return
			}
			if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["canonical_scheme"] != "http" ||
				warns[0]["suite_path"] != rigSuite || !strings.Contains(fmt.Sprint(warns[0]["err"]), "injected Release.gpg row read failure") {
				t.Fatalf("WARN records %v, want one naming the scheme, the suite and the error", warns)
			}
		})
	}
}

// snapReadCancelCache cancels the caller's context at GetSuiteSnapshot and
// fails the read with its error: an admin client that hung up mid-request.
type snapReadCancelCache struct {
	Cache
	cancel func()
}

func (c *snapReadCancelCache) GetSuiteSnapshot(ctx context.Context, id int64) (*cache.SuiteSnapshot, error) {
	c.cancel()
	return nil, fmt.Errorf("injected snapshot read failure (id %d): %w", id, ctx.Err())
}

// Task 8 review M-2: Reconcile reads the snapshot under the admin
// request's context, so a read a client hang-up cut short is DEBUG, not
// WARN (ruling PG-35, the E-13 class), and still counts unsigned.
func TestReconcile_ASnapshotReadCutShortByTheRequestIsDebug(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs lockedBuffer
	chk, err := New(Config{Cache: &snapReadCancelCache{Cache: r.cache, cancel: cancel}, Fetcher: r.up,
		HostLimiter: hostsem.New(8), Adopter: r.adopter,
		Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	held := mustAcquire(t, chk.admission, "other", true)
	defer held.release()
	if chk.Reconcile(ctx, "http", rigHost, rigSuite) {
		t.Fatal("a reconcile whose snapshot read was cut short took the reserved slot")
	}
	recs := logRecords(t, &logs, "reconcile_snapshot_read_failed")
	if len(recs) != 1 || recs[0]["level"] != "DEBUG" || recs[0]["canonical_scheme"] != "http" || recs[0]["suite_path"] != rigSuite {
		t.Fatalf("records %v, want one DEBUG naming the suite and no WARN", recs)
	}
}
