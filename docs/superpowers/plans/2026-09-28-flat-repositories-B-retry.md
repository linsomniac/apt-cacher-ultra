# Flat repositories — Phase B: Retry discipline and capacity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop deterministic adoption failures from retrying every cooldown (PR #8 limitation #1), apply apt's `Date` rules, bound adoption/repair/reconcile capacity without ever waiting while holding a suite mutex, and replace the sequential T2 loop with a fair per-host scheduler whose latency bound holds after restarts — each proven by virtual-clock simulations.

**Architecture:** The v7 columns phase A added carry a per-suite adoption failure record (content key, reason, streak, next retry, the failing content's validators) and an unknown-check streak. The checker writes the record from the adoption goroutine before the suite mutex is released and gates every later attempt on it; `CommitAdoption` clears it in the flip transaction, together with the pending-change stamp, and sets the suite validators to the adopted anchor's. A checker-owned admission object hands out one global and one per-host slot by `TryAcquire`. The scheduler keeps per-host FIFO queues in three persisted classes, served by four workers, one check per host at a time, round-robin.

**Tech Stack:** Go 1.25 (toolchain 1.26.7), modernc SQLite, `testing/synctest`, `math/rand/v2`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 4) — C7 (all), C5 "`Date` rules" and the rollback/natural-key rule, C4 "Scheduler fairness", "Latency bound", "Adoption capacity", "Validators", "`inrelease_change_seen_at`", the clock-step rule for `next_retry_at`, the new `freshness_check` results this phase owns (`changed_deferred`, `changed_queued`), C11 (`adoption.max_duration`, constants, events `adoption_deferred`, `adoption_failing_persistently`, `adoption_refused`), §9 phase B row. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — its Global Constraints, the phase-B entries under "Later phases add" and "Phase A carry-forwards", and the deferred minors in `2026-09-28-flat-repositories-A-deferred.md` (IDs cited per task).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 already holds every column this phase writes (`suite_freshness.adoption_fail_*`, `check_fail_count`, `last_requested_at`; `suite_snapshot.release_date`). Never edit `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock.Now()` (the cache's `nowUnix`, the checker's and adopter's `now`); durations and deadlines use monotonic `time.Now`/`time.Since` and contexts.
- New code blocks only on channels, timers and contexts, never on a `sync.Mutex` held across a wait. Nothing may wait for an adoption slot while holding a suite mutex.
- Constants (spec C11), not knobs: backoff 2 min → 1 h with ±25 % jitter (adoption failures and repeated *unknown* checks), check deadline 2 min, check workers 4. New knob: `adoption.max_duration` (default `30m`, ≥ `1m`).
- `adoption_next_retry_at` is a future-event stamp: it is clamped to now + 1 h when found further ahead than any legitimate value (75 min), and never routed through the "infinitely old" past-event rule.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`.
- Every simulation finishes in under 5 s under `-race` on the dev box; size intervals, never drop assertions. Simulation phase lengths are not exact multiples of the tick interval (same-instant goroutine order inside a bubble is not deterministic).
- `go test -race ./...`, `golangci-lint run ./...` clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five.
- Every goroutine started under `internal/freshness` recovers panics.
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists the helpers it uses from other tasks and existing test files. Never redefine one: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed.
- **Existing tests that encode replaced behaviour.** Some tests assert behaviour this phase deliberately changes (an adoption retried on the very next check; `inrelease_change_seen_at` moved to the latest sighting; `CommitAdoption` leaving validators untouched; the adopter's internal blocking cap; the sequential `tick`). When a task's full-suite run fails such a test, update it to the new behaviour and cite the spec section in a comment; for "retry on the next check" use the documented pattern — `cache.SetAdoptionNextRetry(ctx, scheme, host, suite, 0)` (or advance the test clock past `adoption_next_retry_at`) before the retrying check. Never weaken an assertion that the new behaviour still satisfies. Every such test is named in the task's commit message.

## Deliberate deviations from the spec

1. **Repair and reconcile admission happens after their cheap "anything to do?" checks.** Spec C4 says the check `TryAcquire`s before spawning the goroutine. Taking a slot for every unchanged check of every healthy suite would make healthy suites compete with real adoptions for slots. Here the check first asks `Adopter.repairPending` (one indexed read) and `Adopter.reconcilePending` (memoised; one Release re-parse per snapshot per process) and only `TryAcquire`s — still before spawning, still without waiting — when there is work.
2. **`upstream_older` is a deferral reason, not a separate `freshness_check` result.** A check cannot know the candidate's `Date` before adoption runs. The refusal is recorded by the adoption (`adoption_refused`, reason `upstream_older`); later checks of the same content log `result=changed_deferred` with `reason=upstream_older`.
3. **`flat` in admin `suites[]` is added in phase G** with `aptpath.IsFlatSuitePath`, which does not exist yet. Phase B adds `anchor_form`, `last_requested_unixtime`, the record fields and the status text.
4. **The queue bound `freshness.max_suites` (C4) arrives with phase D's cap.** Until then the queues are bounded by the `suite_freshness` table itself: a suite key is queued at most once.
5. **`timed_out` outcome.** The spec names no outcome for an adoption that hits `adoption.max_duration`. It is recorded as outcome and reason `timed_out` (a failure, retried on the normal schedule).

## Review Focus (phase B share)

1. **A deterministic adoption failure** (untrusted signer, broken member, future `Date`) must cost at most ~30 attempts per day, a burst of client requests right after it must cost zero extra attempts, and new upstream content must still be tried at once — Tasks 6, 7, 13.
2. **A flapping or rolled-back mirror** must never move a suite backwards, and must stop costing work once upstream serves the current content again — Tasks 4, 7, 13.
3. **Saturation** — slow-drip adoptions, stalling hosts, hundreds of failing suites on one host — must not starve healthy suites anywhere, must keep the degraded suites' checks going, and must keep goroutines bounded, including on the first tick after a restart — Tasks 9, 11, 12.
4. **Wall-clock steps** (backward and forward) must neither freeze retries nor make every suite due at once beyond the pool's bounds — Tasks 7, 11, 12, 13.
5. **Operators must be able to see why a suite is not updating** — failing since when, why, next retry, or refused, and a lag that actually fires — Tasks 6, 14, 15.

## File map

| File | Change |
|---|---|
| `internal/cache/types.go`, `queries.go` (+ `adoption_failure.go`, tests) | v7 suite columns in `SuiteFreshness` and every reader; `check_fail_count` written by `PutSuiteFreshness`; failure-record writers; refusal reasons. |
| `internal/cache/adoption.go` (+ test) | `ReleaseDate` on candidates; `NaturalKeyAdoptedError`; flip transaction sets validators, clears `inrelease_change_seen_at` and the record. |
| `internal/freshness/release_date.go` (+ test) | Port of apt's `RFC1123StrToTime`; `releaseDate`. |
| `internal/freshness/adoption.go`, `metrics.go` (+ tests) | `Date` rules, refusal sentinels, outcome/reason classification; `runRequest`; host-slot hook; commit under `WithoutCancel`; no internal blocking cap. |
| `internal/freshness/retry.go`, `admission.go` (+ tests) | Content key, backoff, jitter; admission slots with the reserved slot. |
| `internal/freshness/freshness.go` (+ tests) | Failure record, gate, `change_seen`, validators rule, queued result, check deadline, unknown-check streak, restart reset, repair/reconcile admission. |
| `internal/freshness/repair.go`, `reconcile.go` | Drop the blocking cap; `repairPending`, `reconcilePending`. |
| `internal/freshness/scheduler.go` (+ tests) | Fair per-host scheduler, failing-suites gauge. |
| `internal/simtest/simtest.go` (+ guard test) | Multi-host upstream with stall/latency behaviours; import guard. |
| `internal/freshness/sim_*_test.go` | DoD simulations. |
| `internal/config/config.go` (+ test), `cmd/apt-cacher-ultra/main.go` | `adoption.max_duration`. |
| `internal/admin/status.go`, `status_template.go` (+ tests) | Record, anchor form, interest stamp, lag from first sighting, failing count. |
| `docs/configuration.md`, `docs/log-fields.md`, `packaging/config/config.toml.default`, overview | Docs, stamps. |

---

### Task 1: Suite rows carry the v7 columns; failure-record writers

**Files:**
- Modify: `internal/cache/types.go` (`SuiteFreshness`, `SuiteWithAdoption`), `internal/cache/queries.go` (`GetSuiteFreshness`, `ListSuites`, `ListSuitesWithAdoption`, `PutSuiteFreshness`)
- Create: `internal/cache/adoption_failure.go`, `internal/cache/adoption_failure_test.go`

**Interfaces:**
- Consumes (existing test helpers in package `cache`): `openCache(t) *Cache`, `stubNow(t, ts int64) (restore func())`, `seedBlob(t, c, content string) string`.
- Produces: `SuiteFreshness` fields `LastRequestedAt *int64`, `CheckFailCount int64`, `AdoptionFailHash *string`, `AdoptionFailReason *string`, `AdoptionFailCount int64`, `AdoptionFailFirstAt *int64`, `AdoptionFailLastAt *int64`, `AdoptionNextRetryAt *int64`, `AdoptionFailETag *string`, `AdoptionFailLastMod *string`; `SuiteWithAdoption.CurrentAnchorForm string`; `type AdoptionFailure struct{ Hash, Reason string; Count, FirstAt, LastAt, NextRetryAt int64; ETag, LastMod *string }`; `(*Cache).RecordAdoptionFailure(ctx, scheme, host, suitePath string, f AdoptionFailure) error`; `(*Cache).ClearAdoptionFailure(ctx, scheme, host, suitePath string) error`; `(*Cache).SetAdoptionNextRetry(ctx, scheme, host, suitePath string, next int64) error`; `(*Cache).RescheduleFailingAdoptions(ctx, now int64, offset func() int64) (int, error)`; `IsRefusalReason(reason string) bool`; constants `ReasonUpstreamOlder`, `ReasonRollbackRefused`, `ReasonUnsignedDowngradeRefused`, `ReasonUnsignedUnconfirmed`, `ReasonUnsignedTooLarge`, `ReasonAnchorAbsent`, `ReasonPanic`; `ErrInvalidAdoptionFailure`; unexported `clearAdoptionFailureSet` (SQL fragment, used by Task 2).

- [ ] **Step 1: Write the failing tests**

`internal/cache/adoption_failure_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const afSuite = "/r/dists/x"

func putSuite(t *testing.T, c *Cache, s SuiteFreshness) {
	t.Helper()
	if s.CanonicalScheme == "" {
		s.CanonicalScheme, s.CanonicalHost, s.SuitePath = "http", "h", afSuite
	}
	if err := c.PutSuiteFreshness(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func getSuite(t *testing.T, c *Cache) SuiteFreshness {
	t.Helper()
	s, err := c.GetSuiteFreshness(context.Background(), "http", "h", afSuite)
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func sampleFailure() AdoptionFailure {
	etag, lm := "e1", "Mon, 02 Jan 2006 15:04:05 GMT"
	return AdoptionFailure{
		Hash: strings.Repeat("a", 64), Reason: "gpg_failed", Count: 2,
		FirstAt: 100, LastAt: 200, NextRetryAt: 300, ETag: &etag, LastMod: &lm,
	}
}

func assertRecord(t *testing.T, where string, s SuiteFreshness, f AdoptionFailure) {
	t.Helper()
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	i64 := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if str(s.AdoptionFailHash) != f.Hash || str(s.AdoptionFailReason) != f.Reason ||
		s.AdoptionFailCount != f.Count || i64(s.AdoptionFailFirstAt) != f.FirstAt ||
		i64(s.AdoptionFailLastAt) != f.LastAt || i64(s.AdoptionNextRetryAt) != f.NextRetryAt ||
		str(s.AdoptionFailETag) != str(f.ETag) || str(s.AdoptionFailLastMod) != str(f.LastMod) {
		t.Fatalf("%s: record = hash %q reason %q count %d first %d last %d next %d etag %q lm %q; want %+v",
			where, str(s.AdoptionFailHash), str(s.AdoptionFailReason), s.AdoptionFailCount,
			i64(s.AdoptionFailFirstAt), i64(s.AdoptionFailLastAt), i64(s.AdoptionNextRetryAt),
			str(s.AdoptionFailETag), str(s.AdoptionFailLastMod), f)
	}
}

func assertNoRecord(t *testing.T, where string, s SuiteFreshness) {
	t.Helper()
	if s.AdoptionFailHash != nil || s.AdoptionFailReason != nil || s.AdoptionFailCount != 0 ||
		s.AdoptionFailFirstAt != nil || s.AdoptionFailLastAt != nil || s.AdoptionNextRetryAt != nil ||
		s.AdoptionFailETag != nil || s.AdoptionFailLastMod != nil {
		t.Fatalf("%s: record not clear: %+v", where, s)
	}
}

// Every suite_freshness reader returns the v7 columns (spec §6, C4, C7).
func TestSuiteFreshnessReaders_ReturnV7Columns(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	restore := stubNow(t, 1_800_000_000)
	putSuite(t, c, SuiteFreshness{CheckFailCount: 3})
	restore()
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	check := func(where string, s SuiteFreshness) {
		t.Helper()
		if s.LastRequestedAt == nil || *s.LastRequestedAt != 1_800_000_000 {
			t.Fatalf("%s: last_requested_at = %v, want 1800000000", where, s.LastRequestedAt)
		}
		if s.CheckFailCount != 3 {
			t.Fatalf("%s: check_fail_count = %d, want 3", where, s.CheckFailCount)
		}
		assertRecord(t, where, s, f)
	}
	check("GetSuiteFreshness", getSuite(t, c))
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSuites = %v, %v", all, err)
	}
	check("ListSuites", all[0])
	wa, err := c.ListSuitesWithAdoption(ctx)
	if err != nil || len(wa) != 1 {
		t.Fatalf("ListSuitesWithAdoption = %v, %v", wa, err)
	}
	check("ListSuitesWithAdoption", wa[0].SuiteFreshness)
	if wa[0].CurrentAnchorForm != "" {
		t.Fatalf("unadopted suite anchor form = %q, want empty", wa[0].CurrentAnchorForm)
	}
}

func TestListSuitesWithAdoption_ReportsCurrentAnchorForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "inrelease for anchor-form listing")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	wa, err := c.ListSuitesWithAdoption(ctx)
	if err != nil || len(wa) != 1 {
		t.Fatalf("ListSuitesWithAdoption = %v, %v", wa, err)
	}
	if wa[0].CurrentAnchorForm != string(FormInline) {
		t.Fatalf("anchor form = %q, want inline", wa[0].CurrentAnchorForm)
	}
}

// PutSuiteFreshness owns check_fail_count but never the failure record
// (spec C7: the record has its own UPDATE-only writers).
func TestPutSuiteFreshness_WritesStreakNeverRecord(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{})
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	putSuite(t, c, SuiteFreshness{CheckFailCount: 5})
	s := getSuite(t, c)
	if s.CheckFailCount != 5 {
		t.Fatalf("check_fail_count = %d, want 5", s.CheckFailCount)
	}
	assertRecord(t, "after PutSuiteFreshness", s, f)
}

func TestRecordAdoptionFailure_IsUpdateOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatalf("unknown suite: err = %v, want nil (no-op)", err)
	}
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 0 {
		t.Fatalf("a record write created a row: %v, %v", all, err)
	}
}

func TestRecordAdoptionFailure_RejectsIncompleteRecord(t *testing.T) {
	c := openCache(t)
	putSuite(t, c, SuiteFreshness{})
	for name, mut := range map[string]func(*AdoptionFailure){
		"no hash":   func(f *AdoptionFailure) { f.Hash = "" },
		"no reason": func(f *AdoptionFailure) { f.Reason = "" },
		"count 0":   func(f *AdoptionFailure) { f.Count = 0 },
	} {
		f := sampleFailure()
		mut(&f)
		if err := c.RecordAdoptionFailure(context.Background(), "http", "h", afSuite, f); !errors.Is(err, ErrInvalidAdoptionFailure) {
			t.Errorf("%s: err = %v, want ErrInvalidAdoptionFailure", name, err)
		}
	}
	assertNoRecord(t, "after rejected writes", getSuite(t, c))
}

func TestClearAdoptionFailure_ResetsEveryColumn(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{CheckFailCount: 2})
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	if err := c.ClearAdoptionFailure(ctx, "http", "h", afSuite); err != nil {
		t.Fatal(err)
	}
	s := getSuite(t, c)
	assertNoRecord(t, "after clear", s)
	if s.CheckFailCount != 2 {
		t.Fatalf("clear touched check_fail_count: %d", s.CheckFailCount)
	}
}

func TestSetAdoptionNextRetry_OnlyMovesAnExistingRecord(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{})
	if err := c.SetAdoptionNextRetry(ctx, "http", "h", afSuite, 999); err != nil {
		t.Fatal(err)
	}
	assertNoRecord(t, "no record to move", getSuite(t, c))
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAdoptionNextRetry(ctx, "http", "h", afSuite, 999); err != nil {
		t.Fatal(err)
	}
	f.NextRetryAt = 999
	assertRecord(t, "moved", getSuite(t, c), f)
}

// Spec C7 restart rule: failing rows get one retry each, spread over the
// refresh interval; refusal states and panic records keep their schedule.
func TestRescheduleFailingAdoptions_SkipsRefusalsAndPanics(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	reasons := map[string]string{"/a": "gpg_failed", "/b": ReasonUpstreamOlder, "/c": ReasonPanic, "/d": ""}
	for suite, reason := range reasons {
		if err := c.PutSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}); err != nil {
			t.Fatal(err)
		}
		if reason == "" {
			continue
		}
		f := sampleFailure()
		f.Reason, f.NextRetryAt = reason, 9_999_999_999
		if err := c.RecordAdoptionFailure(ctx, "http", "h", suite, f); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.RescheduleFailingAdoptions(ctx, 5_000, func() int64 { return 42 })
	if err != nil || n != 1 {
		t.Fatalf("RescheduleFailingAdoptions = %d, %v; want 1, nil", n, err)
	}
	next := func(suite string) *int64 {
		s, err := c.GetSuiteFreshness(ctx, "http", "h", suite)
		if err != nil {
			t.Fatal(err)
		}
		return s.AdoptionNextRetryAt
	}
	if v := next("/a"); v == nil || *v != 5_042 {
		t.Fatalf("/a next = %v, want 5042", v)
	}
	for _, s := range []string{"/b", "/c"} {
		if v := next(s); v == nil || *v != 9_999_999_999 {
			t.Fatalf("%s next = %v, want untouched", s, v)
		}
	}
	if v := next("/d"); v != nil {
		t.Fatalf("/d (no record) next = %v, want nil", *v)
	}
}

func TestIsRefusalReason(t *testing.T) {
	for _, r := range []string{ReasonUpstreamOlder, ReasonRollbackRefused, ReasonUnsignedDowngradeRefused,
		ReasonUnsignedUnconfirmed, ReasonUnsignedTooLarge, ReasonAnchorAbsent} {
		if !IsRefusalReason(r) {
			t.Errorf("%q: want refusal", r)
		}
	}
	for _, r := range []string{ReasonPanic, "gpg_failed", "member_fetch_failed", "timed_out", ""} {
		if IsRefusalReason(r) {
			t.Errorf("%q: want failure, not refusal", r)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'TestSuiteFreshnessReaders|TestListSuitesWithAdoption_Reports|TestPutSuiteFreshness_WritesStreak|TestRecordAdoptionFailure|TestClearAdoptionFailure|TestSetAdoptionNextRetry|TestRescheduleFailingAdoptions|TestIsRefusalReason'`
Expected: compile failure naming `AdoptionFailure`, `RecordAdoptionFailure`, `CheckFailCount`, `CurrentAnchorForm`, `ErrInvalidAdoptionFailure`, `SetAdoptionNextRetry`, `RescheduleFailingAdoptions`, `ReasonUpstreamOlder`, `IsRefusalReason`.

- [ ] **Step 3: Add the fields**

In `internal/cache/types.go`, append to `SuiteFreshness` (after `CurrentSnapshotID`):

```go
	// v7 columns (spec §6). LastRequestedAt is the suite's client-interest
	// stamp; only phase D's TouchSuite and the row-creating writers set it.
	LastRequestedAt *int64
	// CheckFailCount is the streak of consecutive freshness checks that
	// ended unknown (spec C4). PutSuiteFreshness writes it; the scheduler
	// reads it to order and back off suites.
	CheckFailCount int64
	// Adoption failure record (spec C7): the content key and reason of the
	// latest failed or refused adoption, the streak since the last success,
	// the next retry time, and the failing content's validators.
	//
	// AIDEV-NOTE: only RecordAdoptionFailure, ClearAdoptionFailure,
	// SetAdoptionNextRetry, RescheduleFailingAdoptions and CommitAdoption's
	// flip write these columns. PutSuiteFreshness never does.
	AdoptionFailHash    *string
	AdoptionFailReason  *string
	AdoptionFailCount   int64
	AdoptionFailFirstAt *int64
	AdoptionFailLastAt  *int64
	AdoptionNextRetryAt *int64
	AdoptionFailETag    *string
	AdoptionFailLastMod *string
```

And to `SuiteWithAdoption`:

```go
	// CurrentAnchorForm is the current snapshot's anchor_form ("" when the
	// suite has no current snapshot).
	CurrentAnchorForm string
```

- [ ] **Step 4: Route every reader through one column list**

In `internal/cache/queries.go` (add `strings` to the imports if absent), add above `GetSuiteFreshness`:

```go
// suiteFreshnessColumns is the column list every suite_freshness reader
// scans, in (*SuiteFreshness).scanDest order.
//
// AIDEV-NOTE: keep in lockstep with scanDest. Every column is nullable or
// defaulted, so a column added to one list and not the other shifts the
// following fields silently instead of failing.
var suiteFreshnessColumns = []string{
	"canonical_scheme", "canonical_host", "suite_path",
	"last_check_at", "last_success_at",
	"inrelease_etag", "inrelease_lastmod", "inrelease_change_seen_at",
	"current_snapshot_id", "last_requested_at", "check_fail_count",
	"adoption_fail_hash", "adoption_fail_reason", "adoption_fail_count",
	"adoption_fail_first_at", "adoption_fail_last_at", "adoption_next_retry_at",
	"adoption_fail_etag", "adoption_fail_lastmod",
}

// suiteFreshnessSelect renders suiteFreshnessColumns for a SELECT list,
// qualified by alias when it is not empty.
func suiteFreshnessSelect(alias string) string {
	cols := make([]string, len(suiteFreshnessColumns))
	for i, c := range suiteFreshnessColumns {
		if alias != "" {
			c = alias + "." + c
		}
		cols[i] = c
	}
	return strings.Join(cols, ", ")
}

// scanDest returns Scan destinations matching suiteFreshnessColumns.
func (s *SuiteFreshness) scanDest() []any {
	return []any{
		&s.CanonicalScheme, &s.CanonicalHost, &s.SuitePath,
		&s.LastCheckAt, &s.LastSuccessAt,
		&s.InReleaseETag, &s.InReleaseLastMod, &s.InReleaseChangeSeenAt,
		&s.CurrentSnapshotID, &s.LastRequestedAt, &s.CheckFailCount,
		&s.AdoptionFailHash, &s.AdoptionFailReason, &s.AdoptionFailCount,
		&s.AdoptionFailFirstAt, &s.AdoptionFailLastAt, &s.AdoptionNextRetryAt,
		&s.AdoptionFailETag, &s.AdoptionFailLastMod,
	}
}
```

Replace the body of `GetSuiteFreshness` (keep its doc comment):

```go
func (c *Cache) GetSuiteFreshness(ctx context.Context, scheme, host, suitePath string) (*SuiteFreshness, error) {
	q := `SELECT ` + suiteFreshnessSelect("") + `
FROM suite_freshness
WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	var s SuiteFreshness
	err := c.db.QueryRowContext(ctx, q, scheme, host, suitePath).Scan(s.scanDest()...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("GetSuiteFreshness: %w", err)
	}
	return &s, nil
}
```

In `ListSuites`, replace the `const q` with `q := `SELECT ` + suiteFreshnessSelect("") + ` FROM suite_freshness`` and the `rows.Scan(...)` call with `rows.Scan(s.scanDest()...)`.

In `ListSuitesWithAdoption`, replace the query and the scan:

```go
	q := `SELECT ` + suiteFreshnessSelect("sf") + `, ss.adopted_at, ss.anchor_form
FROM suite_freshness sf
LEFT JOIN suite_snapshot ss
       ON ss.snapshot_id = sf.current_snapshot_id`
```

```go
		var s SuiteWithAdoption
		var form sql.NullString
		dest := append(s.scanDest(), &s.CurrentAdoptedAt, &form)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("ListSuitesWithAdoption scan: %w", err)
		}
		s.CurrentAnchorForm = form.String
		out = append(out, s)
```

- [ ] **Step 5: `PutSuiteFreshness` writes the unknown-check streak**

Replace the SQL and the Exec arguments of `PutSuiteFreshness`, and extend its AIDEV-NOTE:

```go
	// AIDEV-NOTE: last_requested_at (v7) is the suite's client-interest
	// stamp. A new row starts "requested now"; this writer never updates
	// it (phase D's TouchSuite owns updates), so a scheduler write cannot
	// keep an idle suite alive. check_fail_count (spec C4) is written on
	// every call: each check result sets or resets the streak in the same
	// statement that bumps last_check_at. The adoption failure record
	// (adoption_fail_*) is never written here (spec C7).
	const q = `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at,
   inrelease_etag, inrelease_lastmod, inrelease_change_seen_at,
   check_fail_count, last_requested_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  last_check_at            = excluded.last_check_at,
  last_success_at          = excluded.last_success_at,
  inrelease_etag           = excluded.inrelease_etag,
  inrelease_lastmod        = excluded.inrelease_lastmod,
  inrelease_change_seen_at = excluded.inrelease_change_seen_at,
  check_fail_count         = excluded.check_fail_count`
```

```go
		_, err := conn.ExecContext(ctx, q,
			s.CanonicalScheme, s.CanonicalHost, s.SuitePath,
			s.LastCheckAt, s.LastSuccessAt,
			s.InReleaseETag, s.InReleaseLastMod, s.InReleaseChangeSeenAt,
			s.CheckFailCount, now)
```

- [ ] **Step 6: The failure-record writers**

`internal/cache/adoption_failure.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Refusal reasons (spec C7): states that use the adoption failure record
// and its retry schedule but are not failures. They are excluded from the
// failing-suites gauge and from adoption_failing_persistently, and the
// admin page shows them as "refused: <reason>".
const (
	ReasonUpstreamOlder            = "upstream_older"
	ReasonRollbackRefused          = "rollback_refused"
	ReasonUnsignedDowngradeRefused = "unsigned_downgrade_refused"
	ReasonUnsignedUnconfirmed      = "unsigned_unconfirmed"
	ReasonUnsignedTooLarge         = "unsigned_too_large"
	ReasonAnchorAbsent             = "anchor_absent"

	// ReasonPanic marks the record of an adoption that panicked. It is a
	// failure, but the restart reset leaves it alone (spec C7): retrying
	// a deterministic panic at every start helps nobody.
	ReasonPanic = "panic"
)

var refusalReasons = []string{
	ReasonUpstreamOlder, ReasonRollbackRefused, ReasonUnsignedDowngradeRefused,
	ReasonUnsignedUnconfirmed, ReasonUnsignedTooLarge, ReasonAnchorAbsent,
}

// IsRefusalReason reports whether a persisted adoption_fail_reason is a
// refusal state rather than a failure.
func IsRefusalReason(reason string) bool { return slices.Contains(refusalReasons, reason) }

// ErrInvalidAdoptionFailure rejects a record without a content key, a
// reason or a positive streak count, before any SQL runs.
var ErrInvalidAdoptionFailure = errors.New("cache: invalid adoption failure record")

// AdoptionFailure is one write of the spec C7 failure record. Count and
// FirstAt describe the streak since the last success; NextRetryAt is when
// the next attempt of the same content may run; ETag/LastMod are the
// failing content's validators, which the checker sends until then.
type AdoptionFailure struct {
	Hash        string
	Reason      string
	Count       int64
	FirstAt     int64
	LastAt      int64
	NextRetryAt int64
	ETag        *string
	LastMod     *string
}

// clearAdoptionFailureSet resets the record. CommitAdoption's flip
// appends it to its ON CONFLICT clause, so a successful adoption clears
// the record in the same transaction that makes the snapshot current.
const clearAdoptionFailureSet = `adoption_fail_hash     = NULL,
  adoption_fail_reason   = NULL,
  adoption_fail_count    = 0,
  adoption_fail_first_at = NULL,
  adoption_fail_last_at  = NULL,
  adoption_next_retry_at = NULL,
  adoption_fail_etag     = NULL,
  adoption_fail_lastmod  = NULL`

// RecordAdoptionFailure writes the failure record of an existing suite
// row. It never creates a row: a suite reaped in the meantime records
// nothing (UPDATE-only, spec C3/C7).
func (c *Cache) RecordAdoptionFailure(ctx context.Context, scheme, host, suitePath string, f AdoptionFailure) error {
	if f.Hash == "" || f.Reason == "" || f.Count < 1 {
		return fmt.Errorf("RecordAdoptionFailure: %w: hash, reason and a positive count are required", ErrInvalidAdoptionFailure)
	}
	const q = `
UPDATE suite_freshness
   SET adoption_fail_hash     = ?,
       adoption_fail_reason   = ?,
       adoption_fail_count    = ?,
       adoption_fail_first_at = ?,
       adoption_fail_last_at  = ?,
       adoption_next_retry_at = ?,
       adoption_fail_etag     = ?,
       adoption_fail_lastmod  = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q,
			f.Hash, f.Reason, f.Count, f.FirstAt, f.LastAt, f.NextRetryAt, f.ETag, f.LastMod,
			scheme, host, suitePath); err != nil {
			return fmt.Errorf("RecordAdoptionFailure: %w", err)
		}
		return nil
	})
}

// ClearAdoptionFailure resets the record of an existing suite row: the
// checker calls it when upstream again serves the current snapshot's
// content (spec C7). UPDATE-only.
func (c *Cache) ClearAdoptionFailure(ctx context.Context, scheme, host, suitePath string) error {
	q := `UPDATE suite_freshness SET ` + clearAdoptionFailureSet + `
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q, scheme, host, suitePath); err != nil {
			return fmt.Errorf("ClearAdoptionFailure: %w", err)
		}
		return nil
	})
}

// SetAdoptionNextRetry moves the next retry time of an existing record:
// the checker clamps a next_retry_at left far in the future by a
// backward clock step (spec C7), and tests make a retry due at once.
// A row without a record is left alone.
func (c *Cache) SetAdoptionNextRetry(ctx context.Context, scheme, host, suitePath string, next int64) error {
	const q = `
UPDATE suite_freshness SET adoption_next_retry_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
   AND adoption_fail_count > 0`
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, q, next, scheme, host, suitePath); err != nil {
			return fmt.Errorf("SetAdoptionNextRetry: %w", err)
		}
		return nil
	})
}

// RescheduleFailingAdoptions gives every failing suite one prompt retry
// at checker start: next_retry_at = now + offset(), offset drawn per row
// (spec C7 "Restart"). Refusal states and panic records keep their
// schedule. Returns the number of rows rescheduled. One transaction.
func (c *Cache) RescheduleFailingAdoptions(ctx context.Context, now int64, offset func() int64) (int, error) {
	skip := append(slices.Clone(refusalReasons), ReasonPanic)
	args := make([]any, len(skip))
	for i, r := range skip {
		args[i] = r
	}
	sel := `SELECT canonical_scheme, canonical_host, suite_path FROM suite_freshness
 WHERE adoption_fail_count > 0
   AND adoption_fail_reason NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",") + `)`
	const upd = `UPDATE suite_freshness SET adoption_next_retry_at = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`
	n := 0
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, sel, args...)
		if err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: select: %w", err)
		}
		type key struct{ scheme, host, suite string }
		var keys []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.scheme, &k.host, &k.suite); err != nil {
				_ = rows.Close()
				return fmt.Errorf("RescheduleFailingAdoptions: scan: %w", err)
			}
			keys = append(keys, k)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: close: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: iterate: %w", err)
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, upd, now+offset(), k.scheme, k.host, k.suite); err != nil {
				return fmt.Errorf("RescheduleFailingAdoptions: update: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RescheduleFailingAdoptions: commit: %w", err)
		}
		n = len(keys)
		return nil
	})
	return n, err
}
```

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/cache/`
Expected: PASS. If an existing test compared a whole `SuiteFreshness` struct literal and now fails only because the new fields are populated, update its expectation (convention above).

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/cache/
git add internal/cache/types.go internal/cache/queries.go internal/cache/adoption_failure.go internal/cache/adoption_failure_test.go
git commit -m "Read the v7 suite columns and add the adoption failure record writers"
```

---

### Task 2: The flip describes the adopted anchor; candidates carry their `Date`

**Files:**
- Modify: `internal/cache/types.go` (`SnapshotCandidate`), `internal/cache/adoption.go` (`InsertCandidateSnapshot`, `CommitAdoption` and its doc comment), add `NaturalKeyAdoptedError` next to `ErrSnapshotNaturalKeyAdopted`
- Create: `internal/cache/flip_state_test.go`

**Interfaces:**
- Consumes: Task 1 (`clearAdoptionFailureSet`, `AdoptionFailure`, `RecordAdoptionFailure`, test helpers `putSuite`, `getSuite`, `sampleFailure`, `assertNoRecord`, constant `afSuite`); existing `openCache`, `seedBlob`.
- Produces: `SnapshotCandidate.ReleaseDate *int64`; `type NaturalKeyAdoptedError struct{ SnapshotID int64 }` (unwraps to `ErrSnapshotNaturalKeyAdopted`); the flip contract: `CommitAdoption` sets `suite_freshness.inrelease_etag/inrelease_lastmod` to the candidate's, clears `inrelease_change_seen_at` and the failure record, leaves `check_fail_count`, `last_check_at` and `last_success_at` alone.

- [ ] **Step 1: Write the failing tests**

`internal/cache/flip_state_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"testing"
)

func commitCandidate(t *testing.T, c *Cache, sc SnapshotCandidate, content string) int64 {
	t.Helper()
	ctx := context.Background()
	h := seedBlob(t, c, content)
	sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath, sc.InReleaseHash = "http", "h", afSuite, &h
	id, _, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

// Spec C4 "Validators" and "inrelease_change_seen_at", C7 "cleared in the
// flip transaction": after a flip the suite row describes the adopted
// anchor and carries no pending-change or failure state.
func TestCommitAdoption_FlipDescribesAdoptedAnchor(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	oldETag, oldLM, seen, check, success := "old-etag", "old-lm", int64(123), int64(456), int64(789)
	putSuite(t, c, SuiteFreshness{InReleaseETag: &oldETag, InReleaseLastMod: &oldLM,
		InReleaseChangeSeenAt: &seen, LastCheckAt: &check, LastSuccessAt: &success, CheckFailCount: 2})
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	newETag, newLM := "new-etag", "new-lm"
	commitCandidate(t, c, SnapshotCandidate{InReleaseETag: &newETag, InReleaseLastMod: &newLM}, "flip anchor")

	s := getSuite(t, c)
	if s.InReleaseETag == nil || *s.InReleaseETag != newETag || s.InReleaseLastMod == nil || *s.InReleaseLastMod != newLM {
		t.Fatalf("validators = %v/%v, want the adopted anchor's", s.InReleaseETag, s.InReleaseLastMod)
	}
	if s.InReleaseChangeSeenAt != nil {
		t.Fatalf("inrelease_change_seen_at = %d, want cleared", *s.InReleaseChangeSeenAt)
	}
	assertNoRecord(t, "after flip", s)
	if s.CheckFailCount != 2 || *s.LastCheckAt != check || *s.LastSuccessAt != success {
		t.Fatalf("flip touched check state: fail=%d check=%v success=%v", s.CheckFailCount, *s.LastCheckAt, *s.LastSuccessAt)
	}
}

// A candidate without validators must not leave the previous anchor's
// validators on the row (they would describe content no longer served).
func TestCommitAdoption_CandidateWithoutValidatorsClearsOldOnes(t *testing.T) {
	c := openCache(t)
	oldETag := "old-etag"
	putSuite(t, c, SuiteFreshness{InReleaseETag: &oldETag})
	commitCandidate(t, c, SnapshotCandidate{}, "anchor without validators")
	if s := getSuite(t, c); s.InReleaseETag != nil || s.InReleaseLastMod != nil {
		t.Fatalf("validators = %v/%v, want NULL", s.InReleaseETag, s.InReleaseLastMod)
	}
}

// Spec C5: the verified Date is stored as suite_snapshot.release_date; a
// reused candidate takes the retry's value.
func TestInsertCandidateSnapshot_StoresReleaseDate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "dated inrelease")
	d1, d2 := int64(1_700_000_000), int64(1_700_000_500)
	sc := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h, ReleaseDate: &d1}
	id, _, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != d1 {
		t.Fatalf("release_date = %v (err %v), want %d", snap.ReleaseDate, err, d1)
	}
	sc.ReleaseDate = &d2
	id2, reused, err := c.InsertCandidateSnapshot(ctx, sc)
	if err != nil || !reused || id2 != id {
		t.Fatalf("reuse = %d, %v, %v", id2, reused, err)
	}
	if snap, _ := c.GetSuiteSnapshot(ctx, id); snap.ReleaseDate == nil || *snap.ReleaseDate != d2 {
		t.Fatalf("reused release_date = %v, want %d", snap.ReleaseDate, d2)
	}
}

func TestInsertCandidateSnapshot_NaturalKeyErrorNamesTheSnapshot(t *testing.T) {
	c := openCache(t)
	id := commitCandidate(t, c, SnapshotCandidate{}, "adopted once")
	h := seedBlob(t, c, "adopted once")
	_, _, err := c.InsertCandidateSnapshot(context.Background(), SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h})
	var nk *NaturalKeyAdoptedError
	if !errors.As(err, &nk) || nk.SnapshotID != id || !errors.Is(err, ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want *NaturalKeyAdoptedError{%d} wrapping ErrSnapshotNaturalKeyAdopted", err, id)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'TestCommitAdoption_FlipDescribes|TestCommitAdoption_CandidateWithout|TestInsertCandidateSnapshot_StoresReleaseDate|TestInsertCandidateSnapshot_NaturalKeyError'`
Expected: compile failure naming `ReleaseDate` (field of `SnapshotCandidate`) and `NaturalKeyAdoptedError`.

- [ ] **Step 3: Candidate `Date` and the typed collision error**

In `internal/cache/types.go`, add to `SnapshotCandidate` (after `Form`):

```go
	// ReleaseDate is the verified Release Date in Unix seconds (spec C5;
	// 0 = missing or unparseable). nil leaves release_date NULL, which the
	// Date rules treat as "no rule" (snapshots adopted before v7).
	ReleaseDate *int64
```

In `internal/cache/adoption.go`, next to `ErrSnapshotNaturalKeyAdopted`'s declaration (find it with `grep -n 'ErrSnapshotNaturalKeyAdopted =' internal/cache/*.go`), add:

```go
// NaturalKeyAdoptedError names the adopted snapshot a candidate collided
// with, so the adopter can tell a rollback to a displaced snapshot (spec
// C5 rollback_refused) from a race with the current one.
type NaturalKeyAdoptedError struct{ SnapshotID int64 }

func (e *NaturalKeyAdoptedError) Error() string {
	return fmt.Sprintf("%v: snapshot_id=%d", ErrSnapshotNaturalKeyAdopted, e.SnapshotID)
}

func (e *NaturalKeyAdoptedError) Unwrap() error { return ErrSnapshotNaturalKeyAdopted }
```

In `InsertCandidateSnapshot`:
- `insertQ` gains `release_date` after `anchor_form`: column list `... release_hash, release_gpg_hash, anchor_form, release_date, created_at, adopted_at, package_coverage_complete, heartbeat_at)` and `VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`; the Exec arguments become `..., sc.ReleaseHash, sc.ReleaseGPGHash, string(form), sc.ReleaseDate, now, coverage, now`.
- `refreshQ` gains `release_date = ?,` before `heartbeat_at = ?`, with `sc.ReleaseDate` passed in that position.
- Replace `return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotNaturalKeyAdopted, existingID)` with `return &NaturalKeyAdoptedError{SnapshotID: existingID}`.

- [ ] **Step 4: The flip sets the anchor's validators and clears pending state**

In `CommitAdoption` Step 1, read the candidate's validators too:

```go
		var (
			scheme, host, suite, form string
			adoptedAt, retiredAt      sql.NullInt64
			candETag, candLastMod     sql.NullString
		)
		err = tx.QueryRowContext(ctx, `
SELECT canonical_scheme, canonical_host, suite_path, anchor_form, adopted_at, retired_at,
       inrelease_etag, inrelease_lastmod
FROM suite_snapshot
WHERE snapshot_id = ?`, snapshotID).Scan(&scheme, &host, &suite, &form, &adoptedAt, &retiredAt, &candETag, &candLastMod)
```

Replace Step 6 (comment and statement):

```go
		// Step 6: upsert the pointer and make the row describe the adopted
		// anchor (spec C4 "Validators"): its validators become the
		// candidate's, the pending-change stamp and the adoption failure
		// record are cleared (spec C4, C7). check and success stamps and
		// check_fail_count are freshness state and stay as they are. A suite
		// row created here starts with its v7 client-interest stamp
		// (last_requested_at = now, as in PutSuiteFreshness); an existing
		// row keeps its stamp.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path, current_snapshot_id, last_requested_at,
   inrelease_etag, inrelease_lastmod)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  current_snapshot_id      = excluded.current_snapshot_id,
  inrelease_etag           = excluded.inrelease_etag,
  inrelease_lastmod        = excluded.inrelease_lastmod,
  inrelease_change_seen_at = NULL,
  `+clearAdoptionFailureSet,
			scheme, host, suite, snapshotID, now, candETag, candLastMod); err != nil {
			return fmt.Errorf("CommitAdoption: flip pointer: %w", err)
		}
```

In the doc comment's numbered list, replace item 6 with: "6. Upsert suite_freshness.current_snapshot_id to snapshotID, set the row's validators to the candidate's, and clear inrelease_change_seen_at and the adoption failure record (spec C4, C7)."

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/`
Expected: PASS, except existing tests that asserted the old flip (validators left untouched, `inrelease_change_seen_at` surviving a flip): update them per the convention and name them in the commit message.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/cache/
git add internal/cache/
git commit -m "Make the adoption flip describe the adopted anchor and store the Release Date"
```

---

### Task 3: Port apt's `Date` parser

**Files:**
- Create: `internal/freshness/release_date.go`, `internal/freshness/release_date_test.go`

**Interfaces:**
- Consumes: `scanBufCap` (existing, `release_parse.go`).
- Produces: `parseAptDate(s string) (int64, bool)`, `releaseDate(text []byte) int64`, `const maxReleaseDateSkew = 10 * time.Second`.

- [ ] **Step 1: Write the failing test**

`internal/freshness/release_date_test.go`:

```go
package freshness

import (
	"testing"
	"time"
)

// rfcExample is RFC 7231's example instant, Sun, 06 Nov 1994 08:49:37 GMT.
const rfcExample = int64(784111777)

func utc(y int, mo time.Month, d, h, mi, s int) int64 {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC).Unix()
}

// The cases pin apt's RFC1123StrToTime (apt-pkg/contrib/strutl.cc),
// including its leniencies, so the cache refuses exactly what apt refuses.
func TestParseAptDate(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"Sun, 06 Nov 1994 08:49:37 GMT", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 UTC", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 Z", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 +0000", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 -0000", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 0", rfcExample, true},
		{"Sun Nov  6 08:49:37 1994", rfcExample, true},     // asctime
		{"Sunday, 06-Nov-94 08:49:37 GMT", rfcExample, true}, // RFC 850
		{"sun, 06 nov 1994 08:49:37 GMT", rfcExample, true},  // weekday and month case-insensitive
		{"Mon, 06 Nov 1994 08:49:37 GMT", rfcExample, true},  // weekday never checked against the date
		{"Sun, 6 Nov 1994 8:49:37 GMT", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37.5 GMT", rfcExample, true}, // strptime ignores text after %S
		{"Sat, 17 Oct 2026 10:00:00 UTC", utc(2026, time.October, 17, 10, 0, 0), true},
		{"Sun, 06 Xyz 1994 08:49:37 GMT", utc(1994, time.January, 6, 8, 49, 37), true}, // unknown month: January
		{"Sun, 31 Feb 2026 00:00:00 UTC", utc(2026, time.March, 3, 0, 0, 0), true},     // timegm rolls over
		{"Sunday, 06-Nov-26 08:49:37 GMT", utc(1926, time.November, 6, 8, 49, 37), true},
		{"Sun, 06 Nov 1994 08:49:37 +0100", 0, false}, // only UTC
		{"Sun, 06 Nov 1994 08:49:37 +00:00", 0, false},
		{"Sun, 06 Nov 1994 08:49:37 gmt", 0, false}, // zone is case-sensitive
		{"Sun, 06 Nov 1994 08:49:37 CET", 0, false},
		{"Sun, 06 Nov 1994 08:49:37 GMT extra", 0, false},
		{"Sun, 06 Nov 1994 08:49:37", 0, false},
		{"Sun, 06 Nov 1994 24:00:00 GMT", 0, false},
		{"Sun, 06 Nov 1994 08:60:00 GMT", 0, false},
		{"Sun, 00 Nov 1994 08:49:37 GMT", 0, false},
		{"Sun, 32 Nov 1994 08:49:37 GMT", 0, false},
		{"Sun 06 Nov 1994 08:49:37 GMT", 0, false}, // three-letter weekday means asctime order
		{"Sunday 06-Nov-94 08:49:37 GMT", 0, false},
		{"Xyz, 06 Nov 1994 08:49:37 GMT", 0, false},
		{"Su, 06 Nov 1994 08:49:37 GMT", 0, false},
		{"Sunday, 06-Nov 08:49:37 GMT", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseAptDate(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseAptDate(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReleaseDate(t *testing.T) {
	cases := map[string]int64{
		"Origin: X\nDate: Sun, 06 Nov 1994 08:49:37 GMT\nSHA256:\n x 1 a\n": rfcExample,
		"Origin: X\r\ndate:   Sun, 06 Nov 1994 08:49:37 UTC  \r\n":         rfcExample, // key case, padding, CRLF
		"Origin: X\nSHA256:\n x 1 a\n":                                      0,           // missing
		"Date: yesterday\n":                                                 0,           // unparseable
		"Origin: X\n Date: Sun, 06 Nov 1994 08:49:37 GMT\n":                 0,           // continuation line, not a field
	}
	for text, want := range cases {
		if got := releaseDate([]byte(text)); got != want {
			t.Errorf("releaseDate(%q) = %d, want %d", text, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestParseAptDate|TestReleaseDate'`
Expected: compile failure naming `parseAptDate`, `releaseDate`.

- [ ] **Step 3: Implement**

`internal/freshness/release_date.go`:

```go
package freshness

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// maxReleaseDateSkew is apt's Acquire::Max-FutureTime default: a Release
// dated further ahead than this is "not valid yet" (spec C5).
const maxReleaseDateSkew = 10 * time.Second

// releaseDate returns the Date field of verified Release text in Unix
// seconds, or 0 when the field is missing or apt would not parse it (apt
// treats both as 0: debmetaindex.cc).
func releaseDate(text []byte) int64 {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Date") {
			continue
		}
		if t, ok := parseAptDate(strings.TrimSpace(val)); ok {
			return t
		}
		return 0
	}
	return 0
}

// parseAptDate ports apt's RFC1123StrToTime (apt-pkg/contrib/strutl.cc,
// apt 2.x/3.x). It accepts, in UTC only:
//
//	Sun, 06 Nov 1994 08:49:37 GMT   RFC 1123; zone GMT, UTC, Z or a zero numeric offset
//	Sun Nov  6 08:49:37 1994        asctime
//	Sunday, 06-Nov-94 08:49:37 GMT  RFC 850; the two-digit year is 19yy
//
// AIDEV-NOTE: a port, quirks included, so the cache refuses exactly what
// apt refuses: the weekday is checked only by its first three letters and
// never against the date; an unknown month name means January (MonthConv);
// text after the seconds is ignored (strptime); a day past the month's end
// rolls over (timegm). One deliberate divergence: apt reads numeric tokens
// with istream >>, which stops at the first non-digit, so a token such as
// "6," parses there; here numeric tokens must be all digits, and an RFC 850
// day above 65535 is rejected instead of wrapping. Real Release files never
// reach either difference. TestParseAptDate pins the behaviour.
func parseAptDate(s string) (int64, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}
	wd := strings.ToLower(f[0])
	if len(wd) < 3 || !isAptWeekday(wd[:3]) {
		return 0, false
	}
	var (
		year, month, day int
		timespec, zone   string
		ok               bool
	)
	switch {
	case len(wd) == 3: // asctime: Sun Nov  6 08:49:37 1994
		if len(f) != 5 {
			return 0, false
		}
		month = aptMonth(f[1])
		if day, ok = aptDigits(f[2]); !ok {
			return 0, false
		}
		timespec = f[3]
		if year, ok = aptSignedInt(f[4]); !ok {
			return 0, false
		}
		zone = "UTC"
	case len(wd) == 4: // RFC 1123: Sun, 06 Nov 1994 08:49:37 GMT
		if wd[3] != ',' || len(f) != 6 {
			return 0, false
		}
		if day, ok = aptDigits(f[1]); !ok {
			return 0, false
		}
		month = aptMonth(f[2])
		if year, ok = aptSignedInt(f[3]); !ok {
			return 0, false
		}
		timespec, zone = f[4], f[5]
	default: // RFC 850: Sunday, 06-Nov-94 08:49:37 GMT
		if wd[len(wd)-1] != ',' || len(f) != 4 {
			return 0, false
		}
		parts := strings.Split(f[1], "-")
		if len(parts) != 3 {
			return 0, false
		}
		if day, ok = aptStoi(parts[0]); !ok || day < 0 || day > 65535 {
			return 0, false
		}
		yy, ok := aptStoi(parts[2])
		if !ok {
			return 0, false
		}
		year = 1900 + yy
		month = aptMonth(parts[1])
		timespec, zone = f[2], f[3]
	}
	if !aptUTCZone(zone) {
		return 0, false
	}
	// strptime("%Y-%m-%d %H:%M:%S") over "%.4d-%.2d-%.2d <timespec>":
	// %Y takes 0..9999 and %d 1..31.
	if year < 0 || year > 9999 || day < 1 || day > 31 {
		return 0, false
	}
	h, mi, sec, ok := aptClock(timespec)
	if !ok {
		return 0, false
	}
	return time.Date(year, time.Month(month), day, h, mi, sec, 0, time.UTC).Unix(), true
}

func isAptWeekday(s string) bool {
	switch s {
	case "sun", "mon", "tue", "wed", "thu", "fri", "sat":
		return true
	}
	return false
}

// aptMonth is apt's MonthConv plus one: it looks at up to three letters
// and defaults to January.
func aptMonth(m string) int {
	at := func(i int) byte {
		if i >= len(m) {
			return 0
		}
		c := m[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		return c
	}
	switch at(0) {
	case 'a':
		if at(1) == 'p' {
			return 4
		}
		return 8
	case 'd':
		return 12
	case 'f':
		return 2
	case 'j':
		if at(1) == 'a' {
			return 1
		}
		if at(2) == 'n' {
			return 6
		}
		return 7
	case 'm':
		if at(2) == 'r' {
			return 3
		}
		return 5
	case 'n':
		return 11
	case 'o':
		return 10
	case 's':
		return 9
	}
	return 1
}

// aptDigits reads an unsigned short token: digits only, at most 65535.
func aptDigits(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v > 65535 {
		return 0, false
	}
	return v, true
}

// aptSignedInt reads a signed int token (optional sign, digits).
func aptSignedInt(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// aptStoi is std::stoi with "the whole string was consumed": optional
// sign, at least one digit, nothing else.
func aptStoi(s string) (int, bool) {
	return aptSignedInt(s)
}

// aptUTCZone accepts GMT, UTC, Z, or any numeric zone equal to zero.
func aptUTCZone(z string) bool {
	switch z {
	case "GMT", "UTC", "Z":
		return true
	}
	v, ok := aptStoi(z)
	return ok && v == 0
}

// aptClock parses strptime's "%H:%M:%S": one or two digits each, hour
// 0..23, minute 0..59, second 0..61; anything after the seconds is
// ignored, as strptime ignores it.
func aptClock(ts string) (h, m, s int, ok bool) {
	i := 0
	num := func(max int) (int, bool) {
		start, v := i, 0
		for i < len(ts) && i-start < 2 && ts[i] >= '0' && ts[i] <= '9' {
			v = v*10 + int(ts[i]-'0')
			i++
		}
		return v, i > start && v <= max
	}
	colon := func() bool {
		if i < len(ts) && ts[i] == ':' {
			i++
			return true
		}
		return false
	}
	var good bool
	if h, good = num(23); !good || !colon() {
		return 0, 0, 0, false
	}
	if m, good = num(59); !good || !colon() {
		return 0, 0, 0, false
	}
	if s, good = num(61); !good {
		return 0, 0, 0, false
	}
	return h, m, s, true
}
```

Note: `strconv.Atoi` accepts a leading `+` or `-` and nothing else besides digits, which is exactly `std::stoi` with the full-consumption check apt adds.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ -run 'TestParseAptDate|TestReleaseDate'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/release_date.go internal/freshness/release_date_test.go
git add internal/freshness/release_date.go internal/freshness/release_date_test.go
git commit -m "Port apt's Release Date parser"
```

---
### Task 4: The adopter applies apt's `Date` rules and refuses rollbacks

**Files:**
- Modify: `internal/freshness/adoption.go` (sentinels; `priorAdoptionForm` → `priorSnapshot`; `runShared` Steps 0a, 3a, 4), `internal/freshness/metrics.go` (`classifyAdoptionOutcome`, `classifyAdoptionReason`, outcome inventory note)
- Create: `internal/freshness/adoption_date_test.go`

**Interfaces:**
- Consumes: Task 2 (`SnapshotCandidate.ReleaseDate`, `cache.NaturalKeyAdoptedError`), Task 3 (`releaseDate`, `maxReleaseDateSkew`), Task 1 (`cache.ReasonUpstreamOlder`, `cache.ReasonRollbackRefused`, `cache.ReasonPanic`); existing test helpers `newAdoptionTestEnv`, `fakePackagesStanzas`, `makeRelease`, `currentSnapshotID`, `passThroughVerifier`, `shaOf`, `testHex64`.
- Produces: `ErrAdoptionUpstreamOlder`, `ErrAdoptionRollbackRefused`, `ErrAdoptionReleaseDateInFuture`; outcomes `refused`, `release_date_in_future`, `panicked`, `timed_out`; `(*Adopter).priorSnapshot(ctx, suite) *cache.SuiteSnapshot`; `formOfSnapshot(*cache.SuiteSnapshot) (adoptionForm, bool)`; test helpers `datedRelease(label string, date int64, members map[string][]byte) []byte` (used by Tasks 6, 7, 13) and `dateNow`.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/adoption_date_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// datedRelease is makeRelease's text headed by a Label (so otherwise equal
// bodies differ) and, when date != 0, a Date field in apt's format.
func datedRelease(label string, date int64, members map[string][]byte) []byte {
	rel, _ := makeRelease(members)
	hdr := "Label: " + label + "\n"
	if date != 0 {
		hdr += "Date: " + time.Unix(date, 0).UTC().Format(http.TimeFormat) + "\n"
	}
	return append([]byte(hdr), rel...)
}

// adoptDated runs one adoption of env's suite whose Release is labelled
// label and dated date (0 = no Date field).
func adoptDated(t *testing.T, env *adoptionTestEnv, ad *Adopter, label string, date int64) error {
	t.Helper()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	rel := datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": pkgs})
	return ad.Run(context.Background(), env.suite, rel, "", "")
}

func fixedNowAdopter(t *testing.T, env *adoptionTestEnv, now time.Time) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

var dateNow = time.Unix(1_800_000_000, 0)

func TestAdopter_StoresReleaseDate(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "v1", d); err != nil {
		t.Fatal(err)
	}
	snap, err := env.cache.GetSuiteSnapshot(context.Background(), currentSnapshotID(t, env))
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != d {
		t.Fatalf("release_date = %v (err %v), want %d", snap.ReleaseDate, err, d)
	}
}

// Spec C5: an older Date is refused (upstream_older) after one
// verification and before any member download.
func TestAdopter_OlderDateRefusedBeforeMemberFetch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d2 := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "v2", d2); err != nil {
		t.Fatal(err)
	}
	cur := currentSnapshotID(t, env)
	calls := env.fetcher.calls.Load()
	err := adoptDated(t, env, ad, "v1", d2-3600)
	if !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
	if got := env.fetcher.calls.Load(); got != calls {
		t.Fatalf("member fetches during a refused adoption = %d, want 0", got-calls)
	}
	if got := currentSnapshotID(t, env); got != cur {
		t.Fatalf("current snapshot moved from %d to %d", cur, got)
	}
}

// A missing or invalid Date is 0, which is older than any dated snapshot
// (apt's rule).
func TestAdopter_MissingDateAfterDatedSnapshotIsOlder(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "v2", dateNow.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "v3", 0); !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
}

// Snapshots adopted before v7 have release_date NULL: no Date rule
// applies to their successor (spec §6 notes).
func TestAdopter_UndatedLegacySnapshotImposesNoRule(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	h := shaOf([]byte("legacy anchor"))
	if err := env.cache.PutBlob(ctx, h, 13); err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "old-dated", 1_000_000_000); err != nil {
		t.Fatalf("adoption after a legacy (undated) snapshot: %v", err)
	}
}

// apt rejects a Release dated more than 10 s ahead (Max-FutureTime).
func TestAdopter_FutureDateBoundary(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "ahead11", dateNow.Unix()+11); !errors.Is(err, ErrAdoptionReleaseDateInFuture) {
		t.Fatalf("Date now+11s: err = %v, want ErrAdoptionReleaseDateInFuture", err)
	}
	if err := adoptDated(t, env, ad, "ahead10", dateNow.Unix()+10); err != nil {
		t.Fatalf("Date now+10s: %v", err)
	}
}

// Spec C5 rollback_refused: equal-Date content equal to an adopted,
// displaced snapshot is refused, so a flapping mirror cannot flip back.
func TestAdopter_RollbackToDisplacedSnapshotRefused(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "A", d); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "B", d); err != nil {
		t.Fatal(err)
	}
	cur := currentSnapshotID(t, env)
	err := adoptDated(t, env, ad, "A", d)
	if !errors.Is(err, ErrAdoptionRollbackRefused) || !errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want ErrAdoptionRollbackRefused wrapping the natural-key collision", err)
	}
	if got := currentSnapshotID(t, env); got != cur {
		t.Fatalf("rolled back: current %d -> %d", cur, got)
	}
}

// The Date rule runs before the candidate insert, so an older rollback
// is upstream_older, not rollback_refused.
func TestAdopter_OlderRollbackIsUpstreamOlder(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d1 := dateNow.Add(-2 * time.Hour).Unix()
	if err := adoptDated(t, env, ad, "A", d1); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "B", d1+3600); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "A", d1); !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
}

func TestClassify_RefusalsAndNewOutcomes(t *testing.T) {
	cases := []struct {
		err             error
		outcome, reason string
	}{
		{fmt.Errorf("%w: x", ErrAdoptionUpstreamOlder), "refused", cache.ReasonUpstreamOlder},
		{fmt.Errorf("%w: %w", ErrAdoptionRollbackRefused, cache.ErrSnapshotNaturalKeyAdopted), "refused", cache.ReasonRollbackRefused},
		{fmt.Errorf("%w: x", ErrAdoptionReleaseDateInFuture), "release_date_in_future", "release_date_in_future"},
		{ErrAdoptionPanicked, "panicked", cache.ReasonPanic},
		{fmt.Errorf("fetch: %w", context.DeadlineExceeded), "timed_out", "timed_out"},
	}
	for _, tc := range cases {
		out := classifyAdoptionOutcome(tc.err)
		if out != tc.outcome {
			t.Errorf("classifyAdoptionOutcome(%v) = %q, want %q", tc.err, out, tc.outcome)
		}
		if r := classifyAdoptionReason(tc.err, out, nil); r != tc.reason {
			t.Errorf("classifyAdoptionReason(%v) = %q, want %q", tc.err, r, tc.reason)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestAdopter_StoresReleaseDate|TestAdopter_OlderDate|TestAdopter_MissingDate|TestAdopter_UndatedLegacy|TestAdopter_FutureDate|TestAdopter_Rollback|TestAdopter_OlderRollback|TestClassify_Refusals'`
Expected: compile failure naming `ErrAdoptionUpstreamOlder`, `ErrAdoptionRollbackRefused`, `ErrAdoptionReleaseDateInFuture`.

- [ ] **Step 3: Sentinels**

In the sentinel `var (...)` block of `internal/freshness/adoption.go`, after `ErrAdoptionUnpinnedSuite`:

```go
	// ErrAdoptionUpstreamOlder is the spec C5 refusal for a candidate
	// whose verified Release Date is older than the current snapshot's
	// (apt ignores such a Release). A refusal state, not a failure (C7).
	ErrAdoptionUpstreamOlder = errors.New("adoption_upstream_older")

	// ErrAdoptionRollbackRefused is the spec C5 refusal for content equal
	// to an adopted snapshot of the suite that is no longer current: a
	// flapping mirror must not flip the cache backwards.
	ErrAdoptionRollbackRefused = errors.New("adoption_rollback_refused")

	// ErrAdoptionReleaseDateInFuture fails a candidate dated more than
	// maxReleaseDateSkew ahead of the cache's clock (apt: "not valid
	// yet"). A failure: the normal retry schedule applies.
	ErrAdoptionReleaseDateInFuture = errors.New("adoption_release_date_in_future")
```

- [ ] **Step 4: Read the prior snapshot once**

Replace `priorAdoptionForm` (and its stale doc comment, deferred minor A-28) with:

```go
// priorSnapshot returns the suite's current snapshot, or nil when there
// is none or it cannot be read. runShared reads it before any mutation:
// after CommitAdoption the prior snapshot is no longer current.
func (a *Adopter) priorSnapshot(ctx context.Context, suite SuiteRef) *cache.SuiteSnapshot {
	fresh, err := a.cache.GetSuiteFreshness(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil || fresh == nil || fresh.CurrentSnapshotID == nil {
		return nil
	}
	snap, err := a.cache.GetSuiteSnapshot(ctx, *fresh.CurrentSnapshotID)
	if err != nil {
		return nil
	}
	return snap
}

// formOfSnapshot maps a snapshot's stored anchor form to the adoption
// form; (0, false) for nil or a form adoption cannot drift from.
func formOfSnapshot(snap *cache.SuiteSnapshot) (adoptionForm, bool) {
	if snap == nil {
		return 0, false
	}
	switch snap.Form {
	case cache.FormInline:
		return adoptionFormInline, true
	case cache.FormDetached:
		return adoptionFormDetached, true
	}
	return 0, false
}

// priorAdoptionForm returns the form of the suite's current snapshot,
// read from its stored anchor_form; (0, false) when there is none.
func (a *Adopter) priorAdoptionForm(ctx context.Context, suite SuiteRef) (adoptionForm, bool) {
	return formOfSnapshot(a.priorSnapshot(ctx, suite))
}
```

If nothing but `runShared` calls `priorAdoptionForm` (`grep -rn priorAdoptionForm internal/`), delete the wrapper instead of keeping it. In `runShared` Step 0a, replace `priorForm, hadPrior := a.priorAdoptionForm(ctx, suite)` with:

```go
	prior := a.priorSnapshot(ctx, suite)
	priorForm, hadPrior := formOfSnapshot(prior)
```

- [ ] **Step 5: Step 3a, the `Date` rules**

In `runShared`, directly after the `ParseRelease` error block (end of Step 3), insert:

```go
	// Step 3a (spec C5): apt's Date rules, checked before the candidate
	// insert so a refusal costs one verification and no member download.
	// A missing or unparseable Date is 0 (apt). Snapshots adopted before
	// v7 have no stored date (NULL): no rule applies to their successor.
	relDate := releaseDate(releaseText)
	if now := a.now(); relDate > now.Add(maxReleaseDateSkew).Unix() {
		return fmt.Errorf("%w: Date is %ds ahead of the cache's clock",
			ErrAdoptionReleaseDateInFuture, relDate-now.Unix())
	}
	if prior != nil && prior.ReleaseDate != nil && relDate < *prior.ReleaseDate {
		return fmt.Errorf("%w: Date %d is older than the current snapshot's %d",
			ErrAdoptionUpstreamOlder, relDate, *prior.ReleaseDate)
	}
```

In Step 4, after the `switch p.form` that fills `cand`, add `cand.ReleaseDate = &relDate`.

Replace the `InsertCandidateSnapshot` call's error block with:

```go
	snapshotID, reused, err := a.cache.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		// AIDEV-NOTE: content equal to an adopted snapshot that is no
		// longer current is a rollback (spec C5 rollback_refused). Equal to
		// the CURRENT snapshot is a race with a check that already saw the
		// flip; the next check reports "unchanged", so it stays a plain
		// DB-failure wrap.
		var nk *cache.NaturalKeyAdoptedError
		if errors.As(err, &nk) && (prior == nil || nk.SnapshotID != prior.SnapshotID) {
			return fmt.Errorf("%w: %w", ErrAdoptionRollbackRefused, err)
		}
		if errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
			a.logger.Warn("adoption: natural key already adopted",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
		} else {
			a.logger.Warn("adoption: insert candidate failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
		}
		return fmt.Errorf("%w: insert candidate: %w", ErrAdoptionDBFailed, err)
	}
```

- [ ] **Step 6: Classification**

In `internal/freshness/metrics.go` add the `"context"` and `cache` imports. At the top of `classifyAdoptionOutcome`'s switch (before `ErrAdoptionUnpinnedSuite`), add:

```go
	case errors.Is(err, ErrAdoptionUpstreamOlder), errors.Is(err, ErrAdoptionRollbackRefused):
		return "refused"
	case errors.Is(err, ErrAdoptionReleaseDateInFuture):
		return "release_date_in_future"
	case errors.Is(err, ErrAdoptionPanicked):
		return "panicked"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
```

In `classifyAdoptionReason`, after the `err == nil` return, add:

```go
	switch outcome {
	case "refused":
		if errors.Is(err, ErrAdoptionUpstreamOlder) {
			return cache.ReasonUpstreamOlder
		}
		return cache.ReasonRollbackRefused
	case "panicked":
		return cache.ReasonPanic
	}
```

Update the outcome inventory in the metrics AIDEV-NOTE to: "success, refused (spec C5 refusal states; the reason says which), release_date_in_future, timed_out (adoption.max_duration), unpinned_suite, gpg_failed, parse_failed, member_mismatch, member_fetch_failed, db_failed, run_failed, and panicked (Checker.recoverAdoption)".

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/freshness/`
Expected: PASS (`TestRun_NaturalKeyCollisionIsWrapped` keeps passing: its collision is with the current snapshot).

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/freshness/
git add internal/freshness/adoption.go internal/freshness/metrics.go internal/freshness/adoption_date_test.go
git commit -m "Apply apt's Release Date rules and refuse rollbacks before member downloads"
```

---

### Task 5: Content key, backoff and jitter

**Files:**
- Create: `internal/freshness/retry.go`, `internal/freshness/retry_test.go`

**Interfaces:**
- Produces: constants `retryBaseDelay = 2 * time.Minute`, `retryMaxDelay = time.Hour`, `retryJitterFrac = 0.25`, `retryHorizon = retryMaxDelay + retryMaxDelay/4`; `contentKey(form adoptionForm, inline, release, sig []byte) string`; `backoffDelay(count int64) time.Duration`; `jitterDelay(d time.Duration, u float64) time.Duration`.

- [ ] **Step 1: Write the failing test**

`internal/freshness/retry_test.go`:

```go
package freshness

import (
	"testing"
	"time"
)

// Spec C7 content key: inline sha256(InRelease); detached
// sha256(sha256(Release) || sha256(Release.gpg)).
func TestContentKey(t *testing.T) {
	rel, sig1, sig2 := []byte("Release body"), []byte("sig one"), []byte("sig two")
	if got := contentKey(adoptionFormInline, rel, nil, nil); got != hashOf(rel) {
		t.Fatalf("inline key = %s, want sha256 of the body", got)
	}
	k1 := contentKey(adoptionFormDetached, nil, rel, sig1)
	if k1 != contentKey(adoptionFormDetached, nil, rel, sig1) {
		t.Fatal("detached key not deterministic")
	}
	if k1 == contentKey(adoptionFormDetached, nil, rel, sig2) {
		t.Fatal("a re-signature must change the detached key")
	}
	if k1 == hashOf(rel) || len(k1) != 64 {
		t.Fatalf("detached key %q must be a distinct sha256 hex", k1)
	}
}

func TestBackoffDelay(t *testing.T) {
	want := map[int64]time.Duration{
		0: 0, 1: 2 * time.Minute, 2: 4 * time.Minute, 3: 8 * time.Minute, 4: 16 * time.Minute,
		5: 32 * time.Minute, 6: time.Hour, 7: time.Hour, 1 << 40: time.Hour,
	}
	for n, w := range want {
		if got := backoffDelay(n); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", n, got, w)
		}
	}
}

func TestJitterDelay(t *testing.T) {
	d := 40 * time.Minute
	if got := jitterDelay(d, 0.5); got != d {
		t.Fatalf("u=0.5: %v, want %v", got, d)
	}
	if got := jitterDelay(d, 0); got != 30*time.Minute {
		t.Fatalf("u=0: %v, want 30m (-25%%)", got)
	}
	if got := jitterDelay(d, 0.999999); got >= 50*time.Minute || got < 49*time.Minute {
		t.Fatalf("u->1: %v, want just under 50m (+25%%)", got)
	}
	if retryHorizon != 75*time.Minute {
		t.Fatalf("retryHorizon = %v, want 75m (1h + 25%% jitter)", retryHorizon)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestContentKey|TestBackoffDelay|TestJitterDelay'`
Expected: compile failure naming `contentKey`, `backoffDelay`, `jitterDelay`, `retryHorizon`.

- [ ] **Step 3: Implement**

`internal/freshness/retry.go`:

```go
package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Spec C7/C4 retry schedule (C11: constants, not knobs). One schedule
// serves adoption failures and the scheduler's unknown-check backoff.
const (
	retryBaseDelay  = 2 * time.Minute
	retryMaxDelay   = time.Hour
	retryJitterFrac = 0.25

	// retryHorizon is the furthest ahead a next_retry_at can legitimately
	// be written (the 1 h cap plus 25 % jitter). A stored value further
	// ahead comes from a backward clock step and is clamped to now + 1 h.
	retryHorizon = retryMaxDelay + retryMaxDelay/4
)

// contentKey identifies the content an adoption attempted (spec C7).
// Any byte change is new content, attempted at once. Inline:
// sha256(InRelease). Detached: sha256(sha256(Release) || sha256(Release.gpg)),
// so a re-signature alone is new content too. (Phase E adds unsigned:
// sha256(Release).)
func contentKey(form adoptionForm, inline, release, sig []byte) string {
	if form == adoptionFormDetached {
		r, g := sha256.Sum256(release), sha256.Sum256(sig)
		h := sha256.New()
		h.Write(r[:])
		h.Write(g[:])
		return hex.EncodeToString(h.Sum(nil))
	}
	s := sha256.Sum256(inline)
	return hex.EncodeToString(s[:])
}

// backoffDelay is min(2 min × 2^(count−1), 1 h) for a streak of count
// failures; 0 for count < 1.
func backoffDelay(count int64) time.Duration {
	if count < 1 {
		return 0
	}
	if count > 6 { // 2 min << 5 already exceeds the cap; avoid shift overflow
		return retryMaxDelay
	}
	return min(retryBaseDelay<<(count-1), retryMaxDelay)
}

// jitterDelay spreads d by ±25 %: u is uniform in [0, 1); u = 0.5 is d.
func jitterDelay(d time.Duration, u float64) time.Duration {
	return d + time.Duration((u*2-1)*retryJitterFrac*float64(d))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ -run 'TestContentKey|TestBackoffDelay|TestJitterDelay'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/retry.go internal/freshness/retry_test.go
git add internal/freshness/retry.go internal/freshness/retry_test.go
git commit -m "Add the retry schedule primitives"
```

---
### Task 6: Record failed and refused adoptions

**Files:**
- Modify: `internal/freshness/freshness.go` (`Cache` interface, `Config.rand`, `Checker.rand`, `New`, `adoptionRequest.key`, the adoption goroutine in `Check`, `recoverGoroutine`, the request literals in `checkLockedInline`/`checkLockedDetached`), `internal/freshness/metrics.go` (inventory note), `internal/freshness/freshness_test.go` (`fakeCache` gains the four record methods)
- Create: `internal/freshness/adoption_failure.go`, `internal/freshness/retry_rig_test.go`, `internal/freshness/adoption_failure_test.go`

**Interfaces:**
- Consumes: Task 1 (cache record methods, reason constants), Task 4 (`datedRelease`, outcome/reason classification), Task 5 (`backoffDelay`, `jitterDelay`, `contentKey`); existing `seedStaleAnchor`, `lockedBuffer`, `fakeFetcher`/`newFakeFetcher`, `fakePackagesStanzas`, `hashOf`, `testHex64`, `suiteKey`.
- Produces: freshness `Cache` interface methods `RecordAdoptionFailure`, `ClearAdoptionFailure`, `SetAdoptionNextRetry`, `RescheduleFailingAdoptions` (signatures as in the cache); `Config.rand func() float64` (unexported seam) and `Checker.rand`; `(*Checker).recordAdoptionFailure(req *adoptionRequest, reason string)`; `(*Checker).recoverAdoption(req *adoptionRequest, start time.Time)`; `isPersistentMilestone(n int64) bool`; `nonEmpty(string) *string`; `adoptionRequest.key string`; the rig in `retry_rig_test.go`: `newRetryRig(t, cooldown time.Duration) *retryRig` with fields `cache, adopter, checker, members, up, ver, clock, ring, logs, pkgs, cancelLifetime` and methods `release(label string, date int64) []byte`, `check()`, `row() cache.SuiteFreshness`, `events(msg string) []map[string]any`; `rigClock` (`now`, `advance`); `rigUpstream` (`set`, `setAt`, `setIgnoreValidators`, `setStall`, `etagsSent`); `rigVerifier` (`reject`, `panicOn`, `blockNext`, `calls`, field `entered`); `rigETag(body []byte) string`; constants `rigHost`, `rigSuite`, `rigAnchorURL`, `rigPkgsURL`; `strOf(*string) string`, `i64Of(*int64) int64`.

- [ ] **Step 1: The test rig**

`internal/freshness/retry_rig_test.go`:

```go
package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

const (
	rigHost      = "h"
	rigSuite     = "/r/dists/x"
	rigAnchorURL = "http://h/r/dists/x/InRelease"
	rigPkgsURL   = "http://h/r/dists/x/main/binary-amd64/Packages"
)

// rigClock is the settable wall clock shared by the rig's checker and
// adopter.
type rigClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *rigClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *rigClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// rigETag is the ETag the rig upstream gives a body.
func rigETag(body []byte) string { return "e-" + hashOf(body)[:12] }

// rigUpstream answers the checker's conditional GETs by URL: 200 with the
// body and its ETag, or 304 when If-None-Match equals that ETag (unless
// validators are ignored). A URL without a body answers 404. While
// stalled, every request blocks until its context ends.
type rigUpstream struct {
	mu               sync.Mutex
	bodies           map[string][]byte
	ignoreValidators bool
	stall            bool
	sent             map[string][]string // If-None-Match per URL, in order
}

func (u *rigUpstream) set(body []byte) { u.setAt(rigAnchorURL, body) }

func (u *rigUpstream) setAt(url string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies[url] = body
}

func (u *rigUpstream) setIgnoreValidators(v bool) { u.mu.Lock(); u.ignoreValidators = v; u.mu.Unlock() }

func (u *rigUpstream) setStall(v bool) { u.mu.Lock(); u.stall = v; u.mu.Unlock() }

func (u *rigUpstream) etagsSent(url string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.sent[url]...)
}

func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, etag, _ string, _ int64) (*fetch.ConditionalResult, error) {
	u.mu.Lock()
	u.sent[target.URL] = append(u.sent[target.URL], etag)
	body, ok := u.bodies[target.URL]
	stall, ignore := u.stall, u.ignoreValidators
	u.mu.Unlock()
	if stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	tag := rigETag(body)
	if !ignore && etag != "" && etag == tag {
		return &fetch.ConditionalResult{Status: 304, ETag: tag}, nil
	}
	return &fetch.ConditionalResult{Status: 200, Body: body, ETag: tag}, nil
}

var errRigUntrusted = errors.New("rig: untrusted signer")

// rigVerifier passes every body through as its own plaintext, except
// content keys marked rejected (error) or panicking. blockNext makes the
// next call signal entered and wait for its context to end.
type rigVerifier struct {
	mu       sync.Mutex
	rejected map[string]bool
	panics   map[string]bool
	block    bool
	n        int
	entered  chan struct{}
}

func (v *rigVerifier) reject(key string)  { v.mu.Lock(); v.rejected[key] = true; v.mu.Unlock() }
func (v *rigVerifier) panicOn(key string) { v.mu.Lock(); v.panics[key] = true; v.mu.Unlock() }
func (v *rigVerifier) blockNext()         { v.mu.Lock(); v.block = true; v.mu.Unlock() }
func (v *rigVerifier) calls() int         { v.mu.Lock(); defer v.mu.Unlock(); return v.n }

func (v *rigVerifier) verify(ctx context.Context, key string, plain []byte) ([]byte, error) {
	v.mu.Lock()
	v.n++
	rej, pan, blk := v.rejected[key], v.panics[key], v.block
	v.block = false
	v.mu.Unlock()
	if blk {
		v.entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if pan {
		panic("rig verifier panic")
	}
	if rej {
		return nil, errRigUntrusted
	}
	return plain, nil
}

func (v *rigVerifier) VerifyInline(ctx context.Context, _ SuiteRef, b []byte) ([]byte, error) {
	return v.verify(ctx, contentKey(adoptionFormInline, b, nil, nil), b)
}

func (v *rigVerifier) VerifyDetached(ctx context.Context, _ SuiteRef, rel, sig []byte) ([]byte, error) {
	return v.verify(ctx, contentKey(adoptionFormDetached, nil, rel, sig), rel)
}

// retryRig wires a real cache, Adopter and Checker for suite /r/dists/x
// on host h, whose cached InRelease anchor is stale (testHex64), so any
// body the upstream serves is "changed". The clock starts at
// 1_800_000_000 and moves only when the test advances it; jitter is off
// (rand = 0.5).
type retryRig struct {
	t              *testing.T
	cache          *cache.Cache
	adopter        *Adopter
	checker        *Checker
	members        *fakeFetcher
	up             *rigUpstream
	ver            *rigVerifier
	clock          *rigClock
	ring           *observability.Ring
	logs           *lockedBuffer
	pkgs           []byte
	cancelLifetime context.CancelFunc
}

func newRetryRig(t *testing.T, cooldown time.Duration) *retryRig {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	seedStaleAnchor(t, c)
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite}); err != nil {
		t.Fatal(err)
	}
	r := &retryRig{
		t: t, cache: c, members: newFakeFetcher(),
		up:    &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}},
		ver:   &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		clock: &rigClock{t: time.Unix(1_800_000_000, 0)},
		ring:  observability.NewRing(50), logs: &lockedBuffer{},
		pkgs:  fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64}),
	}
	r.members.put(rigPkgsURL, r.pkgs)
	r.adopter, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: r.ver,
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lifeCtx, cancel := context.WithCancel(ctx)
	r.cancelLifetime = cancel
	r.checker, err = New(Config{Cache: c, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Cooldown: cooldown, Refresh: 15 * time.Minute, Adopter: r.adopter, LifetimeCtx: lifeCtx,
		AdoptionRing: r.ring,
		Logger:       slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:          r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.checker.WaitForAdoptions() })
	return r
}

// release is a Release body the rig can adopt: label makes it distinct;
// date != 0 adds a Date field.
func (r *retryRig) release(label string, date int64) []byte {
	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
}

// check runs one freshness check and waits for any adoption it started.
func (r *retryRig) check() {
	r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	r.checker.WaitForAdoptions()
}

func (r *retryRig) row() cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
	if err != nil {
		r.t.Fatal(err)
	}
	return *s
}

// events returns the checker's JSON log records whose msg is msg.
func (r *retryRig) events(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func i64Of(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}
```

- [ ] **Step 2: Write the failing tests**

`internal/freshness/adoption_failure_test.go`:

```go
package freshness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Spec C7: a failed adoption records its content key, reason, streak,
// next retry and the failing content's validators.
func TestAdoptionFailure_RecordsKeyReasonScheduleAndValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()

	row := r.row()
	now := r.clock.now().Unix()
	if row.AdoptionFailCount != 1 || strOf(row.AdoptionFailReason) != "gpg_failed" || strOf(row.AdoptionFailHash) != hashOf(bad) {
		t.Fatalf("record = count %d reason %q hash %q", row.AdoptionFailCount, strOf(row.AdoptionFailReason), strOf(row.AdoptionFailHash))
	}
	if strOf(row.AdoptionFailETag) != rigETag(bad) {
		t.Fatalf("failing validators = %q, want %q", strOf(row.AdoptionFailETag), rigETag(bad))
	}
	if i64Of(row.AdoptionNextRetryAt) != now+int64(2*time.Minute/time.Second) {
		t.Fatalf("next retry = %d, want now+120 (no jitter at rand 0.5)", i64Of(row.AdoptionNextRetryAt))
	}
	if i64Of(row.AdoptionFailFirstAt) != now || i64Of(row.AdoptionFailLastAt) != now {
		t.Fatalf("first/last = %d/%d, want %d", i64Of(row.AdoptionFailFirstAt), i64Of(row.AdoptionFailLastAt), now)
	}
	ev := r.events("adoption_deferred")
	if len(ev) != 1 || ev[0]["reason"] != "gpg_failed" || ev[0]["fail_count"] != float64(1) || ev[0]["refusal"] != false {
		t.Fatalf("adoption_deferred = %v", ev)
	}
}

// The streak and its start survive new failures; the persistent warning
// fires at 3, 6, 12, ... only.
func TestAdoptionFailure_StreakAndPersistentWarnings(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	first := r.clock.now().Unix()
	for i := 0; i < 6; i++ {
		r.check()
		r.clock.advance(2 * time.Hour) // past any next_retry_at
	}
	row := r.row()
	if row.AdoptionFailCount != 6 || i64Of(row.AdoptionFailFirstAt) != first {
		t.Fatalf("count %d first %d, want 6 and %d", row.AdoptionFailCount, i64Of(row.AdoptionFailFirstAt), first)
	}
	var counts []float64
	for _, e := range r.events("adoption_failing_persistently") {
		counts = append(counts, e["fail_count"].(float64))
	}
	if len(counts) != 2 || counts[0] != 3 || counts[1] != 6 {
		t.Fatalf("persistent warnings at %v, want [3 6]", counts)
	}
}

func TestIsPersistentMilestone(t *testing.T) {
	for n := int64(0); n <= 100; n++ {
		want := n == 3 || n == 6 || n == 12 || n == 24 || n == 48 || n == 96
		if got := isPersistentMilestone(n); got != want {
			t.Errorf("isPersistentMilestone(%d) = %v, want %v", n, got, want)
		}
	}
}

// Spec C7: a shutdown context cancellation is not recorded.
func TestAdoptionFailure_ShutdownIsNotRecorded(t *testing.T) {
	r := newRetryRig(t, 0)
	r.ver.blockNext()
	r.up.set(r.release("slow", 0))
	go r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	<-r.ver.entered
	r.cancelLifetime()
	r.checker.WaitForAdoptions()
	if row := r.row(); row.AdoptionFailCount != 0 {
		t.Fatalf("shutdown recorded a failure: %+v", row)
	}
}

// Spec C5 I-1 + C7: a panicking adoption is a recorded failure with
// reason panic, shown in the ring, and the suite mutex is released.
func TestAdoptionFailure_PanicIsRecorded(t *testing.T) {
	r := newRetryRig(t, 0)
	boom := r.release("boom", 0)
	r.ver.panicOn(hashOf(boom))
	r.up.set(boom)
	r.check()
	if row := r.row(); row.AdoptionFailCount != 1 || strOf(row.AdoptionFailReason) != cache.ReasonPanic {
		t.Fatalf("record = %d %q, want 1 %q", row.AdoptionFailCount, strOf(row.AdoptionFailReason), cache.ReasonPanic)
	}
	evs := r.ring.Snapshot()
	if len(evs) == 0 || evs[len(evs)-1].Outcome != "panicked" || evs[len(evs)-1].Reason != cache.ReasonPanic {
		t.Fatalf("ring = %+v, want a panicked event", evs)
	}
	v, _ := r.checker.locks.Load(suiteKey("http", rigHost, rigSuite))
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		t.Fatal("suite mutex still held after a panic")
	}
	mu.Unlock()
}

// Refusals use the record and schedule but are not failures: logged as
// adoption_refused and never as adoption_failing_persistently.
func TestAdoptionFailure_RefusalIsNotAFailure(t *testing.T) {
	r := newRetryRig(t, 0)
	d2 := r.clock.now().Add(-time.Hour).Unix()
	r.up.set(r.release("v2", d2))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: v2 not adopted")
	}
	r.up.set(r.release("v1", d2-3600))
	for i := 0; i < 3; i++ {
		r.check()
		r.clock.advance(2 * time.Hour)
	}
	row := r.row()
	if row.AdoptionFailCount != 3 || strOf(row.AdoptionFailReason) != cache.ReasonUpstreamOlder {
		t.Fatalf("record = %d %q, want 3 upstream_older", row.AdoptionFailCount, strOf(row.AdoptionFailReason))
	}
	if n := len(r.events("adoption_refused")); n != 3 {
		t.Fatalf("adoption_refused lines = %d, want 3", n)
	}
	if n := len(r.events("adoption_failing_persistently")); n != 0 {
		t.Fatalf("a refusal raised %d persistent-failure warnings", n)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestAdoptionFailure|TestIsPersistentMilestone'`
Expected: compile failure naming `rand` (unknown field in `Config`) and `isPersistentMilestone`.

- [ ] **Step 4: Interface, seam and request key**

In `internal/freshness/freshness.go`, add to the `Cache` interface:

```go
	// Adoption failure record (spec C7). UPDATE-only on existing rows.
	RecordAdoptionFailure(ctx context.Context, scheme, host, suitePath string, f cache.AdoptionFailure) error
	ClearAdoptionFailure(ctx context.Context, scheme, host, suitePath string) error
	SetAdoptionNextRetry(ctx context.Context, scheme, host, suitePath string, next int64) error
	RescheduleFailingAdoptions(ctx context.Context, now int64, offset func() int64) (int, error)
```

Add to `Config`, after `now`:

```go
	// rand returns a uniform value in [0, 1) for retry jitter and the
	// restart spread (spec C7). Test seam; production uses math/rand/v2.
	rand func() float64
```

Add `rand func() float64` to `Checker`. In `New` (import `mrand "math/rand/v2"`):

```go
	rnd := cfg.rand
	if rnd == nil {
		rnd = mrand.Float64
	}
```

and set `rand: rnd` in the returned struct.

Add to `adoptionRequest`:

```go
	// key is the spec C7 content key of the bytes this request carries;
	// a failure is recorded under it.
	key string
```

In `checkLockedInline`'s `changed` branch, add `key: newHash,` to the `adoptionRequest` literal. In `checkLockedDetached`'s request literal, add `key: contentKey(adoptionFormDetached, nil, res.Body, gpgRes.Body),`.

In `internal/freshness/freshness_test.go`, give `fakeCache` the four methods:

```go
func (f *fakeCache) RecordAdoptionFailure(_ context.Context, scheme, host, suitePath string, r cache.AdoptionFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	s, ok := f.suites[k]
	if !ok {
		return nil
	}
	hash, reason, first, last, next := r.Hash, r.Reason, r.FirstAt, r.LastAt, r.NextRetryAt
	s.AdoptionFailHash, s.AdoptionFailReason, s.AdoptionFailCount = &hash, &reason, r.Count
	s.AdoptionFailFirstAt, s.AdoptionFailLastAt, s.AdoptionNextRetryAt = &first, &last, &next
	s.AdoptionFailETag, s.AdoptionFailLastMod = r.ETag, r.LastMod
	f.suites[k] = s
	return nil
}

func (f *fakeCache) ClearAdoptionFailure(_ context.Context, scheme, host, suitePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if s, ok := f.suites[k]; ok {
		s.AdoptionFailHash, s.AdoptionFailReason, s.AdoptionFailCount = nil, nil, 0
		s.AdoptionFailFirstAt, s.AdoptionFailLastAt, s.AdoptionNextRetryAt = nil, nil, nil
		s.AdoptionFailETag, s.AdoptionFailLastMod = nil, nil
		f.suites[k] = s
	}
	return nil
}

func (f *fakeCache) SetAdoptionNextRetry(_ context.Context, scheme, host, suitePath string, next int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := keyOf(scheme, host, suitePath)
	if s, ok := f.suites[k]; ok && s.AdoptionFailCount > 0 {
		s.AdoptionNextRetryAt = &next
		f.suites[k] = s
	}
	return nil
}

func (f *fakeCache) RescheduleFailingAdoptions(_ context.Context, now int64, offset func() int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, s := range f.suites {
		if s.AdoptionFailCount < 1 || s.AdoptionFailReason == nil ||
			cache.IsRefusalReason(*s.AdoptionFailReason) || *s.AdoptionFailReason == cache.ReasonPanic {
			continue
		}
		next := now + offset()
		s.AdoptionNextRetryAt = &next
		f.suites[k] = s
		n++
	}
	return n, nil
}
```

- [ ] **Step 5: The record writer and the panic boundary**

`internal/freshness/adoption_failure.go`:

```go
package freshness

import (
	"context"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// failureRecordTimeout bounds the record write after an adoption ends
// (the adoption's own deadline may already have passed).
const failureRecordTimeout = 30 * time.Second

// recordAdoptionFailure writes the spec C7 failure record for req's
// content and logs the transition. It runs in the adoption goroutine
// before the suite mutex is released, so the next check already sees the
// record. Shutdown is not a failure: nothing is recorded once the
// lifetime context is done.
func (c *Checker) recordAdoptionFailure(req *adoptionRequest, reason string) {
	s := req.suite
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("adoption_panicked", "record", s, r)
		}
	}()
	if c.lifetimeCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.lifetimeCtx, failureRecordTimeout)
	defer cancel()
	cur, err := c.cache.GetSuiteFreshness(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if err != nil {
		// cache.ErrNotFound: the suite row is gone and nothing is gated.
		c.logger.Warn("adoption_failure_record_failed",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
		return
	}
	now := c.now()
	count := cur.AdoptionFailCount + 1
	first := now.Unix()
	if cur.AdoptionFailCount > 0 && cur.AdoptionFailFirstAt != nil {
		first = *cur.AdoptionFailFirstAt
	}
	delay := jitterDelay(backoffDelay(count), c.rand())
	next := now.Add(delay).Unix()
	f := cache.AdoptionFailure{
		Hash: req.key, Reason: reason, Count: count, FirstAt: first, LastAt: now.Unix(),
		NextRetryAt: next, ETag: nonEmpty(req.etag), LastMod: nonEmpty(req.lastmod),
	}
	if err := c.cache.RecordAdoptionFailure(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath, f); err != nil {
		c.logger.Warn("adoption_failure_record_failed",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath, "err", err)
		return
	}
	refusal := cache.IsRefusalReason(reason)
	attrs := []any{
		"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
		"reason", reason, "refusal", refusal, "fail_count", count,
		"retry_in_seconds", int64(delay / time.Second), "next_retry_unixtime", next,
	}
	if refusal {
		c.logger.Info("adoption_refused", attrs...)
	}
	c.logger.Info("adoption_deferred", attrs...)
	if !refusal && isPersistentMilestone(count) {
		c.logger.Warn("adoption_failing_persistently",
			"canonical_host", s.CanonicalHost, "suite_path", s.SuitePath,
			"reason", reason, "fail_count", count, "failing_since_unixtime", first)
	}
}

// isPersistentMilestone reports streak counts 3, 6, 12, 24, …, the
// adoption_failing_persistently thresholds (spec C7).
func isPersistentMilestone(n int64) bool {
	if n < 3 || n%3 != 0 {
		return false
	}
	q := n / 3
	return q&(q-1) == 0
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// recoverAdoption is the adoption goroutine's panic boundary (I-1). It
// must be deferred directly — recover() returns nil anywhere else — and
// it runs before the suite mutex is released. A panic is a failed
// attempt: counted, timed, shown in the admin ring, and recorded with
// reason panic so the retry schedule applies (spec C5, C7).
func (c *Checker) recoverAdoption(req *adoptionRequest, start time.Time) {
	r := recover()
	if r == nil {
		return
	}
	c.reportPanic("adoption_panicked", "adoption", req.suite, r)
	d := time.Since(start)
	host := req.suite.CanonicalHost
	adoptionTotal.Inc("panicked", host)
	adoptionDurationSeconds.Observe(d.Seconds(), "panicked", host)
	if c.adoptionRing != nil {
		c.adoptionRing.Record(observability.AdoptionEvent{
			Host: host, SuitePath: req.suite.SuitePath, Outcome: "panicked", Reason: cache.ReasonPanic,
			CompletedUnixSec: c.now().Unix(), DurationSeconds: d.Seconds(),
		})
	}
	c.recordAdoptionFailure(req, cache.ReasonPanic)
}
```

- [ ] **Step 6: Wire them into the adoption goroutine**

In `Check`, replace the adoption goroutine's opening (its AIDEV-NOTE and defers) with:

```go
	// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock,
	// recoverAdoption (first). The failure record is written before the
	// mutex is released on both the error path and the panic path, so the
	// next check (T1 or T2) already sees it (spec C7).
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		start := time.Now()
		defer c.recoverAdoption(req, start)
```

and delete the old `start := time.Now()` line further down (keep its AIDEV-NOTE about monotonic time next to the new line). Inside the existing `if err != nil {` block, after the `adoption_run_failed` Warn, add `c.recordAdoptionFailure(req, reason)`.

In `recoverGoroutine`, delete the `if where == "adoption" { adoptionTotal.Inc(...) }` block and start its doc comment with "recoverGoroutine is deferred by the repair and on-demand reconcile goroutines as their LAST defer, …". In the metrics AIDEV-NOTE, the `panicked` entry names `Checker.recoverAdoption`.

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./cmd/...`
Expected: PASS. `TestCheck_AdoptionPanicIsRecovered` keeps passing (same log line and counter).

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/freshness/
git add internal/freshness/
git commit -m "Record failed and refused adoptions before releasing the suite mutex"
```

---

### Task 7: Gate attempts on the failure record

**Files:**
- Modify: `internal/freshness/freshness.go` (`checkLockedInline`, `checkLockedDetached`; new `retryGate`, `retryGateFor`, `validators`, `applyClamp`, `setChangeSeen`; the `clockStep` AIDEV-NOTE)
- Create: `internal/freshness/retry_gate_test.go`

**Interfaces:**
- Consumes: Task 6 (rig, record), Task 5 (`retryHorizon`, `retryMaxDelay`, `contentKey`), Task 1 (`ClearAdoptionFailure`, `SetAdoptionNextRetry`); existing `EvictURLPath`, `testHex64`.
- Produces: `type retryGate struct{ active, due bool; hash, etag, lastmod, reason string; clampTo int64 }`; `(*Checker).retryGateFor(cur *cache.SuiteFreshness, now time.Time) retryGate`; `(retryGate).validators(cur *cache.SuiteFreshness) (etag, lastmod string)`; `(*Checker).applyClamp(ctx, cur, g retryGate, now time.Time)`; `setChangeSeen(cur *cache.SuiteFreshness, now int64)`; result `changed_deferred`; test helpers `failOnce(t, r) []byte`, `lastResult(r) string`, `seedDetachedAnchors(t, c)`.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/retry_gate_test.go`:

```go
package freshness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// failOnce leaves the rig with one recorded failure of an untrusted body.
func failOnce(t *testing.T, r *retryRig) []byte {
	t.Helper()
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.ver.calls() != 1 || r.row().AdoptionFailCount != 1 {
		t.Fatalf("setup: calls %d count %d", r.ver.calls(), r.row().AdoptionFailCount)
	}
	return bad
}

func lastResult(r *retryRig) string {
	ev := r.events("freshness_check")
	if len(ev) == 0 {
		return ""
	}
	res, _ := ev[len(ev)-1]["result"].(string)
	return res
}

// Before next_retry_at the check sends the failing content's validators;
// a 304 defers without an attempt and keeps the first-sighting stamp.
func TestGate_DeferredSendsFailingValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	bad := failOnce(t, r)
	seen := r.row().InReleaseChangeSeenAt
	r.clock.advance(time.Minute)
	r.check()
	sent := r.up.etagsSent(rigAnchorURL)
	if got := sent[len(sent)-1]; got != rigETag(bad) {
		t.Fatalf("validator sent = %q, want the failing content's %q", got, rigETag(bad))
	}
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("calls %d result %q, want 1 changed_deferred", r.ver.calls(), lastResult(r))
	}
	row := r.row()
	if row.AdoptionFailCount != 1 || i64Of(row.InReleaseChangeSeenAt) != i64Of(seen) {
		t.Fatalf("deferral changed state: count %d seen %d (was %d)", row.AdoptionFailCount, i64Of(row.InReleaseChangeSeenAt), i64Of(seen))
	}
}

// An upstream that ignores validators and re-serves the failing bytes is
// deferred the same way.
func TestGate_SameContent200IsDeferred(t *testing.T) {
	r := newRetryRig(t, 0)
	failOnce(t, r)
	r.up.setIgnoreValidators(true)
	r.clock.advance(time.Minute)
	r.check()
	if r.ver.calls() != 1 || lastResult(r) != "changed_deferred" {
		t.Fatalf("calls %d result %q", r.ver.calls(), lastResult(r))
	}
}

// New content is attempted at once, whatever the schedule says.
func TestGate_NewContentIsAttemptedAtOnce(t *testing.T) {
	r := newRetryRig(t, 0)
	failOnce(t, r)
	r.up.set(r.release("fixed", 0))
	r.clock.advance(time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("verifier calls = %d, want 2 (new content tried before next_retry_at)", r.ver.calls())
	}
	if row := r.row(); row.CurrentSnapshotID == nil || row.AdoptionFailCount != 0 {
		t.Fatalf("new content not adopted or record not cleared: %+v", row)
	}
}

// At next_retry_at the check sends no validators and retries.
func TestGate_DueRetryHasNoValidators(t *testing.T) {
	r := newRetryRig(t, 0)
	failOnce(t, r)
	r.clock.advance(2 * time.Minute)
	r.check()
	sent := r.up.etagsSent(rigAnchorURL)
	if got := sent[len(sent)-1]; got != "" {
		t.Fatalf("due retry sent validator %q, want none", got)
	}
	if r.ver.calls() != 2 || r.row().AdoptionFailCount != 2 {
		t.Fatalf("calls %d count %d, want 2 and 2", r.ver.calls(), r.row().AdoptionFailCount)
	}
}

// When upstream again serves the current snapshot's content, the record
// is cleared (spec C7).
func TestGate_CurrentContentClearsTheRecord(t *testing.T) {
	r := newRetryRig(t, 0)
	good := r.release("good", 0)
	r.up.set(good)
	r.check()
	if r.row().CurrentSnapshotID == nil {
		t.Fatal("setup: good content not adopted")
	}
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	r.check()
	if r.row().AdoptionFailCount != 1 {
		t.Fatal("setup: no failure recorded")
	}
	r.up.set(good)
	r.clock.advance(time.Minute)
	r.check()
	if row := r.row(); row.AdoptionFailCount != 0 || row.InReleaseChangeSeenAt != nil || lastResult(r) != "unchanged" {
		t.Fatalf("record %d seen %v result %q, want cleared, nil, unchanged", row.AdoptionFailCount, row.InReleaseChangeSeenAt, lastResult(r))
	}
}

// A next_retry_at far in the future (backward clock step) is clamped to
// now + 1 h: a retry within the hour, not immediately (spec C7).
func TestGate_FutureNextRetryClampedToOneHour(t *testing.T) {
	r := newRetryRig(t, 0)
	failOnce(t, r)
	ctx := context.Background()
	if err := r.cache.SetAdoptionNextRetry(ctx, "http", rigHost, rigSuite, r.clock.now().Add(10*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	r.check()
	if want := r.clock.now().Add(time.Hour).Unix(); i64Of(r.row().AdoptionNextRetryAt) != want {
		t.Fatalf("next retry = %d, want clamped to %d", i64Of(r.row().AdoptionNextRetryAt), want)
	}
	r.clock.advance(59 * time.Minute)
	r.check()
	if r.ver.calls() != 1 {
		t.Fatalf("retried before the clamped hour: calls %d", r.ver.calls())
	}
	r.clock.advance(2 * time.Minute)
	r.check()
	if r.ver.calls() != 2 {
		t.Fatalf("no retry after the clamped hour: calls %d", r.ver.calls())
	}
}

// Spec C7 DoD: a burst of client-triggered checks right after a failure
// costs zero extra attempts.
func TestGate_T1StormAfterFailureCostsNothing(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	failOnce(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.checker.Check(context.Background(), "http", rigHost, rigSuite) }()
	}
	wg.Wait()
	r.checker.WaitForAdoptions()
	r.clock.advance(61 * time.Second)
	r.check()
	if r.ver.calls() != 1 {
		t.Fatalf("attempts after a T1 storm = %d, want 1", r.ver.calls())
	}
}

// PR #8 limitation #1: an untrusted body checked every cooldown (60 s)
// for 12 h costs ~16 attempts, not 720.
func TestGate_DeterministicFailureIsBounded(t *testing.T) {
	r := newRetryRig(t, time.Minute)
	bad := r.release("bad", 0)
	r.ver.reject(hashOf(bad))
	r.up.set(bad)
	for elapsed := time.Duration(0); elapsed < 12*time.Hour; elapsed += time.Minute {
		r.check()
		r.clock.advance(time.Minute)
	}
	// Attempts at 0, 2, 6, 14, 30, 62 min, then hourly to 662: 5 + 11 = 16.
	if got := r.ver.calls(); got < 14 || got > 18 {
		t.Fatalf("attempts in 12 h = %d, want ~16", got)
	}
}

// inrelease_change_seen_at keeps the first sighting while a change is
// pending (spec C4), and validators are never taken from a changed body.
func TestChangeSeenAndValidatorsRules(t *testing.T) {
	r := newRetryRig(t, 0)
	before := r.row()
	bad := failOnce(t, r)
	first := r.row().InReleaseChangeSeenAt
	if first == nil || *first != r.clock.now().Unix() {
		t.Fatalf("first sighting = %v", first)
	}
	r.clock.advance(3 * time.Hour)
	bad2 := r.release("bad2", 0)
	r.ver.reject(hashOf(bad2))
	r.up.set(bad2)
	r.check()
	row := r.row()
	if i64Of(row.InReleaseChangeSeenAt) != *first {
		t.Fatalf("change_seen moved to %d, want first sighting %d", i64Of(row.InReleaseChangeSeenAt), *first)
	}
	if strOf(row.InReleaseETag) != strOf(before.InReleaseETag) || strOf(row.InReleaseETag) == rigETag(bad) {
		t.Fatalf("validators taken from a changed body: %q", strOf(row.InReleaseETag))
	}
}

// Detached suites: a re-signature behind a 304 waits for the retry; a
// re-signature delivered with a 200 is new content, tried at once.
func TestGate_DetachedResignature(t *testing.T) {
	for _, via200 := range []bool{false, true} {
		r := newRetryRig(t, 0)
		seedDetachedAnchors(t, r.cache)
		relURL, sigURL := "http://h/r/dists/x/Release", "http://h/r/dists/x/Release.gpg"
		rel, sig1, sig2 := r.release("det", 0), []byte("sig one"), []byte("sig two")
		r.ver.reject(contentKey(adoptionFormDetached, nil, rel, sig1))
		r.up.setAt(relURL, rel)
		r.up.setAt(sigURL, sig1)
		r.check()
		if r.row().AdoptionFailCount != 1 {
			t.Fatalf("via200=%v: setup: no detached failure", via200)
		}
		r.up.setAt(sigURL, sig2)
		r.up.setIgnoreValidators(via200)
		r.clock.advance(time.Minute)
		r.check()
		wantCalls := 1
		if via200 {
			wantCalls = 2
		}
		if r.ver.calls() != wantCalls {
			t.Fatalf("via200=%v: verifier calls = %d, want %d", via200, r.ver.calls(), wantCalls)
		}
		if !via200 {
			r.clock.advance(2 * time.Minute)
			r.check()
			if r.ver.calls() != 2 || r.row().CurrentSnapshotID == nil {
				t.Fatalf("re-signature not adopted at the retry: calls %d", r.ver.calls())
			}
		}
	}
}

// seedDetachedAnchors removes the rig's InRelease anchor and caches stale
// Release + Release.gpg anchors, so checks take the detached path.
func seedDetachedAnchors(t *testing.T, c *cache.Cache) {
	t.Helper()
	ctx := context.Background()
	if err := c.EvictURLPath(ctx, "http", rigHost, rigSuite+"/InRelease"); err != nil {
		t.Fatal(err)
	}
	other := hashOf([]byte("stale Release.gpg"))
	if err := c.PutBlob(ctx, other, 1); err != nil {
		t.Fatal(err)
	}
	for path, h := range map[string]string{rigSuite + "/Release": testHex64, rigSuite + "/Release.gpg": other} {
		h := h
		if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: rigHost, Path: path,
			BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true}); err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestGate_|TestChangeSeenAndValidatorsRules'`
Expected: FAIL — without the gate, deferred checks re-attempt (`calls 2 ... want 1`, result `changed`), the 12 h test makes ~720 attempts, and `change_seen` moves.

- [ ] **Step 3: The gate**

In `internal/freshness/freshness.go`, after `repairIfUnchanged`, add:

```go
// retryGate is a check's view of the suite's adoption failure record
// (spec C7 "Gate").
type retryGate struct {
	active  bool   // a failure or refusal record exists
	due     bool   // now >= next_retry_at (after clamping)
	hash    string // content key of the failing content
	etag    string // the failing content's validators
	lastmod string
	reason  string
	// clampTo is non-zero when the stored next_retry_at lay beyond
	// retryHorizon (a backward clock step); applyClamp persists it.
	clampTo int64
}

// retryGateFor reads the gate from the suite row.
//
// AIDEV-NOTE: next_retry_at is a FUTURE-event stamp. It is never
// "infinitely old" (that would disable backoff after a backward step);
// a value beyond the legitimate horizon is clamped to now + 1 h instead.
func (c *Checker) retryGateFor(cur *cache.SuiteFreshness, now time.Time) retryGate {
	if cur.AdoptionFailCount < 1 || cur.AdoptionFailHash == nil {
		return retryGate{}
	}
	g := retryGate{active: true, due: true, hash: *cur.AdoptionFailHash}
	if cur.AdoptionFailETag != nil {
		g.etag = *cur.AdoptionFailETag
	}
	if cur.AdoptionFailLastMod != nil {
		g.lastmod = *cur.AdoptionFailLastMod
	}
	if cur.AdoptionFailReason != nil {
		g.reason = *cur.AdoptionFailReason
	}
	if cur.AdoptionNextRetryAt != nil {
		next := time.Unix(*cur.AdoptionNextRetryAt, 0)
		if next.After(now.Add(retryHorizon)) {
			next = now.Add(retryMaxDelay)
			g.clampTo = next.Unix()
		}
		g.due = !now.Before(next)
	}
	return g
}

// validators returns what the conditional GET sends: the current
// anchor's validators with no record, the failing content's before
// next_retry_at, none at or after it.
func (g retryGate) validators(cur *cache.SuiteFreshness) (etag, lastmod string) {
	switch {
	case g.active && g.due:
		return "", ""
	case g.active:
		return g.etag, g.lastmod
	}
	if cur.InReleaseETag != nil {
		etag = *cur.InReleaseETag
	}
	if cur.InReleaseLastMod != nil {
		lastmod = *cur.InReleaseLastMod
	}
	return etag, lastmod
}

// applyClamp persists a clamped next_retry_at (backward clock step).
func (c *Checker) applyClamp(ctx context.Context, cur *cache.SuiteFreshness, g retryGate, now time.Time) {
	if g.clampTo == 0 {
		return
	}
	c.clockStep("adoption_next_retry_at", cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, *cur.AdoptionNextRetryAt, now)
	if err := c.cache.SetAdoptionNextRetry(ctx, cur.CanonicalScheme, cur.CanonicalHost, cur.SuitePath, g.clampTo); err != nil {
		c.logger.Warn("freshness: clamp adoption retry failed",
			"err", err, "canonical_host", cur.CanonicalHost, "suite_path", cur.SuitePath)
	}
}

// setChangeSeen stamps the first sighting of a pending upstream change;
// later sightings keep it, so "lagging" measures from first sight
// (spec C4).
func setChangeSeen(cur *cache.SuiteFreshness, now int64) {
	if cur.InReleaseChangeSeenAt == nil {
		v := now
		cur.InReleaseChangeSeenAt = &v
	}
}
```

Change the last sentence of the `clockStep` AIDEV-NOTE to: "adoption_next_retry_at is a FUTURE-event stamp: a detection is logged through this function, but the stamp is clamped to now+1h (retryGateFor), never treated as infinitely old."

- [ ] **Step 4: Use the gate in the inline check**

In `checkLockedInline`, replace the block from `target := &fetch.Target{` through the `InReleaseLastMod` validator assignments with:

```go
	target := &fetch.Target{
		CanonicalHost: host,
		URL:           urlRow.UpstreamURL,
	}
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)
```

Replace the result `switch res.Status { … }` and everything after it up to and including the final `return` with:

```go
	var (
		result      string
		req         *adoptionRequest
		clearRecord bool
	)
	switch res.Status {
	case 304:
		if gate.active {
			// The failing content's validators matched: upstream still
			// serves what failed (spec C7 changed_deferred).
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		} else {
			// Upstream serves the cached anchor again; clear the pending
			// diagnostic (e.g. after an upstream rollback).
			cur.InReleaseChangeSeenAt = nil
			result = "not_modified"
		}
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		switch {
		case newHash == cachedHash:
			// Same bytes as the current anchor: refresh validators (the
			// only response they are ever taken from, spec C4), clear the
			// pending diagnostic and any failure record (spec C7).
			if res.ETag != "" {
				v := res.ETag
				cur.InReleaseETag = &v
			}
			if res.LastModified != "" {
				v := res.LastModified
				cur.InReleaseLastMod = &v
			}
			cur.InReleaseChangeSeenAt = nil
			clearRecord = gate.active
			result = "unchanged"
		case gate.active && !gate.due && newHash == gate.hash:
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		default:
			setChangeSeen(cur, nowUnix)
			c.logger.Info("InRelease changed at upstream",
				"canonical_host", host,
				"suite_path", suitePath,
				"cached_hash", cachedHash,
				"upstream_hash", newHash,
			)
			result = "changed"
			req = &adoptionRequest{
				suite:   suite,
				form:    adoptionFormInline,
				key:     newHash,
				bytes:   res.Body,
				etag:    res.ETag,
				lastmod: res.LastModified,
			}
		}
	default:
		c.logger.Error("freshness: unexpected success status",
			"status", res.Status,
			"canonical_host", host,
			"suite_path", suitePath,
		)
		cur.LastSuccessAt = nil // undo the optimistic success bump
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed", "err", perr)
		}
		return nil, nil, false
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, scheme, host, suitePath); err != nil {
			c.logger.Warn("freshness: clear adoption failure failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}

	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "inline",
		"result", result,
		"upstream_status", res.Status,
	}
	if result == "changed_deferred" {
		logAttrs = append(logAttrs, "reason", gate.reason)
	}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(result, host)
	return req, repairIfUnchanged(result, suite, cur), false
```

- [ ] **Step 5: Use the gate in the detached check**

In `checkLockedDetached`, replace the `var etag, lastmod string …` block after `target := …` with:

```go
	gate := c.retryGateFor(cur, now)
	c.applyClamp(ctx, cur, gate, now)
	etag, lastmod := gate.validators(cur)
```

Replace its `switch res.Status { … }` and everything after it up to and including the final `return` with:

```go
	var (
		result      string
		req         *adoptionRequest
		clearRecord bool
	)
	switch res.Status {
	case 304:
		if gate.active {
			setChangeSeen(cur, nowUnix)
			result = "changed_deferred"
		} else {
			cur.InReleaseChangeSeenAt = nil
			result = "not_modified"
		}
	case 200:
		sum := sha256.Sum256(res.Body)
		newHash := hex.EncodeToString(sum[:])
		if newHash == cachedHash {
			if res.ETag != "" {
				v := res.ETag
				cur.InReleaseETag = &v
			}
			if res.LastModified != "" {
				v := res.LastModified
				cur.InReleaseLastMod = &v
			}
			cur.InReleaseChangeSeenAt = nil
			clearRecord = gate.active
			result = "unchanged"
			break
		}
		setChangeSeen(cur, nowUnix)
		// The content key covers the signature, so Release.gpg is fetched
		// before the gate can compare (no validators: the signature is
		// bound to this Release).
		gpgTarget := &fetch.Target{
			CanonicalHost: host,
			URL:           releaseGPGURL.UpstreamURL,
		}
		gpgRes, gerr := c.fetcher.Conditional(ctx, gpgTarget, "", "", defaultMaxReleaseGPGBytes)
		if gerr != nil || gpgRes.Status != 200 {
			// As before: record the change and retry on a later check;
			// last_success_at stays bumped because Release answered.
			if gerr != nil {
				c.logger.Warn("freshness: Release.gpg fetch failed; deferring adoption",
					"err", gerr, "canonical_host", host, "suite_path", suitePath)
			} else {
				c.logger.Warn("freshness: Release.gpg fetch returned non-200; deferring adoption",
					"status", gpgRes.Status, "canonical_host", host, "suite_path", suitePath)
			}
			result = "changed"
			break
		}
		key := contentKey(adoptionFormDetached, nil, res.Body, gpgRes.Body)
		if gate.active && !gate.due && key == gate.hash {
			result = "changed_deferred"
			break
		}
		c.logger.Info("Release changed at upstream",
			"canonical_host", host,
			"suite_path", suitePath,
			"cached_hash", cachedHash,
			"upstream_hash", newHash,
		)
		result = "changed"
		req = &adoptionRequest{
			suite:        suite,
			form:         adoptionFormDetached,
			key:          key,
			releaseBytes: res.Body,
			sigBytes:     gpgRes.Body,
			etag:         res.ETag,
			lastmod:      res.LastModified,
		}
	default:
		c.logger.Error("freshness: unexpected success status",
			"status", res.Status,
			"canonical_host", host,
			"suite_path", suitePath,
			"form", "detached",
		)
		cur.LastSuccessAt = nil
		if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
			c.logger.Warn("freshness: persist anomaly failed", "err", perr)
		}
		return nil, nil
	}

	if perr := c.cache.PutSuiteFreshness(ctx, *cur); perr != nil {
		c.logger.Warn("freshness: persist success failed",
			"err", perr,
			"canonical_host", host,
			"suite_path", suitePath,
		)
	}
	if clearRecord {
		if err := c.cache.ClearAdoptionFailure(ctx, scheme, host, suitePath); err != nil {
			c.logger.Warn("freshness: clear adoption failure failed",
				"err", err, "canonical_host", host, "suite_path", suitePath)
		}
	}

	logAttrs := []any{
		"canonical_host", host,
		"suite_path", suitePath,
		"form", "detached",
		"result", result,
		"upstream_status", res.Status,
	}
	if result == "changed_deferred" {
		logAttrs = append(logAttrs, "reason", gate.reason)
	}
	c.logger.Info("freshness_check", logAttrs...)
	freshnessCheckTotal.Inc(result, host)
	return req, repairIfUnchanged(result, suite, cur)
```

- [ ] **Step 6: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/ ./cmd/...`
Expected: the new tests PASS. Existing tests that re-attempt an adoption on the next check without time passing now see `changed_deferred`: update them with the convention's pattern (`SetAdoptionNextRetry(..., 0)` or advancing the test clock) and name them in the commit message.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/
git add internal/freshness/ internal/handler/ cmd/
git commit -m "Gate adoption attempts on the failure record"
```

---

### Task 8: One prompt retry per failing suite at start

**Files:**
- Modify: `internal/freshness/scheduler.go` (`Run`), `internal/freshness/freshness.go` (new `rescheduleFailingAdoptions`)
- Create: `internal/freshness/restart_reset_test.go`

**Interfaces:**
- Consumes: Task 1 (`RescheduleFailingAdoptions`), Task 6 (`Config.rand`, rig).
- Produces: `(*Checker).rescheduleFailingAdoptions(ctx context.Context)`; `Run` calls it first, before its `refresh <= 0` return.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/restart_reset_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestRescheduleFailingAdoptions_Spreads|TestRun_ReschedulesBeforeAnything'`
Expected: compile failure naming `rescheduleFailingAdoptions`.

- [ ] **Step 3: Implement**

In `internal/freshness/freshness.go`, add:

```go
// rescheduleFailingAdoptions gives every failing suite one retry spread
// uniformly over the next periodic_refresh (spec C7 "Restart"): the
// recovery path after an operator installs a key or changes pins,
// without a thundering herd. Refusal states and panic records keep
// their schedule.
func (c *Checker) rescheduleFailingAdoptions(ctx context.Context) {
	if c.adopter == nil {
		return
	}
	spread := int64(c.refresh / time.Second)
	n, err := c.cache.RescheduleFailingAdoptions(ctx, c.now().Unix(), func() int64 {
		if spread <= 0 {
			return 0
		}
		return int64(c.rand() * float64(spread))
	})
	if err != nil {
		c.logger.Warn("adoption_retries_reschedule_failed", "err", err)
		return
	}
	if n > 0 {
		c.logger.Info("adoption_retries_rescheduled", "suites", n, "spread_seconds", spread)
	}
}
```

In `internal/freshness/scheduler.go`, make it the first statement of `Run`, and add to `Run`'s doc comment: "Before anything else it reschedules failing adoptions (spec C7 restart rule)."

```go
func (c *Checker) Run(ctx context.Context) {
	c.rescheduleFailingAdoptions(ctx)
	if c.refresh <= 0 {
		return
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/
git add internal/freshness/
git commit -m "Give each failing suite one prompt retry at checker start"
```

---
### Task 9: Admission: one global and one per-host slot, taken without waiting

**Files:**
- Create: `internal/freshness/admission.go`, `internal/freshness/admission_test.go`, `internal/freshness/capacity_test.go`
- Modify: `internal/freshness/freshness.go` (`Checker` fields `admission`, `maxAdoptionDuration`; `New`; `Check` split into `checkLockedSafe`/`spawnAdoption`/`maybeSpawnRepair`; `checkLocked`, `checkLockedInline`, `checkLockedDetached` take `out *checkOutcome`; `Reconcile`), `internal/freshness/adoption.go` (remove `concurrencySem`; fields `maxConcurrent`, `maxDuration`; `AdoptionConfig.MaxDuration`; `adoptionPayload.membersDone`; `runRequest`; host-slot hook and `WithoutCancel` commit in `runShared`), `internal/freshness/repair.go` (drop the blocking cap; `repairPending`), `internal/freshness/reconcile.go` (drop the blocking cap; `reconcilePending`), `internal/config/config.go` (+ `config_test.go`), `cmd/apt-cacher-ultra/main.go`
- Delete: `TestAdopter_ConcurrencyCap` (`internal/freshness/adoption_test.go`) and `TestRepairSkippedMembers_RespectsConcurrencyCap` (`internal/freshness/phase6_7_skip_repair_test.go`) — replaced by checker-level tests in `capacity_test.go`

**Interfaces:**
- Consumes: Task 6 rig and `recordAdoptionFailure`; Task 7 `lastResult`; Task 4 `datedRelease`; existing `blockingFetcher`/`blockReq`, `changedUpstream`, `passThroughVerifier`, `newAdoptionTestEnv`, `currentSnapshotID`, `fakePackagesStanzas`, `makeRelease`, `shaOf`, `simtest.OpenCache`, `simtest.StaticConditional`, `simtest.DiscardLogger`.
- Produces: `newAdmission(global int) *admission`; `(*admission).tryAcquire(host string, signedCurrent bool) (*lease, bool)`; `(*admission).usage() (global, hosts int)`; `(*lease).releaseHost()`, `(*lease).release()` (both idempotent and nil-safe); `type checkOutcome struct{ req *adoptionRequest; repair *repairRequest; lease *lease }`; result `changed_queued`; `(*Adopter).runRequest(ctx, req *adoptionRequest, membersDone func()) error`; `(*Adopter).repairPending(ctx, suite SuiteRef, snapshotID int64) bool`; `(*Adopter).reconcilePending(ctx, snapshotID int64) (bool, error)`; `AdoptionConfig.MaxDuration`; constants `commitTimeout = 5 * time.Minute`, `defaultMaxAdoptionDuration = 30 * time.Minute`; config key `adoption.max_duration`; test helper `seedStaleAnchorAt(t, c, host, suite string)`.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/admission_test.go`:

```go
package freshness

import "testing"

func mustAcquire(t *testing.T, a *admission, host string, signed bool) *lease {
	t.Helper()
	l, ok := a.tryAcquire(host, signed)
	if !ok {
		t.Fatalf("tryAcquire(%q, signed=%v) refused", host, signed)
	}
	return l
}

func TestAdmission_PerHostAndGlobal(t *testing.T) {
	a := newAdmission(3)
	l1 := mustAcquire(t, a, "a", true)
	if _, ok := a.tryAcquire("a", true); ok {
		t.Fatal("a second slot on the same host")
	}
	mustAcquire(t, a, "b", true)
	mustAcquire(t, a, "c", true)
	if _, ok := a.tryAcquire("d", true); ok {
		t.Fatal("the global cap was exceeded")
	}
	l1.release()
	mustAcquire(t, a, "d", true)
	if g, h := a.usage(); g != 3 || h != 3 {
		t.Fatalf("usage = %d/%d, want 3/3", g, h)
	}
}

// Spec C4: with two or more global slots, the last free one is reserved
// for suites whose current snapshot is signed.
func TestAdmission_ReservedSlotForSignedSuites(t *testing.T) {
	a := newAdmission(2)
	u := mustAcquire(t, a, "a", false)
	if _, ok := a.tryAcquire("b", false); ok {
		t.Fatal("an unsigned-current suite took the reserved slot")
	}
	s := mustAcquire(t, a, "b", true)
	u.release()
	if _, ok := a.tryAcquire("c", false); ok {
		t.Fatal("an unsigned-current suite took the last free slot")
	}
	s.release()
	mustAcquire(t, a, "c", false)
}

func TestAdmission_NoReservationBelowTwoSlots(t *testing.T) {
	mustAcquire(t, newAdmission(1), "a", false)
}

func TestAdmission_UnlimitedGlobalKeepsPerHost(t *testing.T) {
	a := newAdmission(0)
	for i := 0; i < 100; i++ {
		mustAcquire(t, a, string(rune('A'+i%26))+string(rune('a'+i/26)), false)
	}
	if _, ok := a.tryAcquire("Aa", false); ok {
		t.Fatal("unlimited global slots must still allow one per host")
	}
}

func TestLease_ReleaseHostThenRelease(t *testing.T) {
	a := newAdmission(2)
	l := mustAcquire(t, a, "a", true)
	l.releaseHost()
	if g, h := a.usage(); g != 1 || h != 0 {
		t.Fatalf("after releaseHost usage = %d/%d, want 1/0", g, h)
	}
	other := mustAcquire(t, a, "a", true) // the host is free again; the global slot is not
	l.release()
	l.release()
	other.release()
	var nilLease *lease
	nilLease.release()
	nilLease.releaseHost()
	if g, h := a.usage(); g != 0 || h != 0 {
		t.Fatalf("usage = %d/%d, want 0/0", g, h)
	}
}
```

`internal/freshness/capacity_test.go`:

```go
package freshness

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// seedStaleAnchorAt caches a stale InRelease anchor (testHex64) and a
// suite row for suite on host, so any body upstream serves is "changed".
func seedStaleAnchorAt(t *testing.T, c *cache.Cache, host, suite string) {
	t.Helper()
	ctx := context.Background()
	stale := testHex64
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &stale, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite}); err != nil {
		t.Fatal(err)
	}
}

// Spec C4: with no free slot the check spawns nothing, reports
// changed_queued and leaves last_success_at alone so the scheduler
// retries it on its next turn.
func TestCheck_NoSlotQueuesWithoutAdvancingSuccess(t *testing.T) {
	r := newRetryRig(t, 0)
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	r.up.set(r.release("v1", 0))
	r.check()
	if r.ver.calls() != 0 || lastResult(r) != "changed_queued" {
		t.Fatalf("calls %d result %q, want 0 changed_queued", r.ver.calls(), lastResult(r))
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

// Replaces TestAdopter_ConcurrencyCap: max_concurrent_adoptions bounds
// adoptions across hosts, and a queued suite runs once the slot frees.
func TestCheck_GlobalSlotCapsConcurrentAdoptions(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	rel := datedRelease("cap", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
	pending := make(chan blockReq, 8)
	bf := &blockingFetcher{pending: pending}
	hosts := []string{"a.example", "b.example"}
	for _, host := range hosts {
		seedStaleAnchorAt(t, c, host, rigSuite)
		bf.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
	}
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: bf, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := New(Config{Cache: c, Fetcher: &changedUpstream{body: rel}, HostLimiter: hostsem.New(8),
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
	chk.Check(ctx, "http", hosts[1], rigSuite)
	select {
	case <-pending:
		t.Fatal("B started while the only global slot was held")
	case <-time.After(150 * time.Millisecond):
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
	c, err := cache.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	setupFF := newFakeFetcher()
	setupAd, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: setupFF, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true})
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
		if err := setupAd.Run(ctx, suite, releaseText, "", ""); err != nil {
			t.Fatalf("setup Run(%s): %v", host, err)
		}
	}
	pending := make(chan blockReq, 8)
	bf := &blockingFetcher{pending: pending}
	for i, host := range hosts {
		bf.put("http://"+host+suitePath+"/by-hash/SHA256/"+shaOf(goods[i]), goods[i])
	}
	repAd, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: bf, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 1, RepairSkippedMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: repAd, Logger: discardLogger()})
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
	chk.Check(ctx, "http", hosts[1], suitePath)
	select {
	case <-pending:
		t.Fatal("repair B ran while the only global slot was held")
	case <-time.After(150 * time.Millisecond):
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
		rel := datedRelease("slow", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: stallFetcher{}, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute})
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
		if d := time.Since(start); d < 30*time.Minute || d >= 31*time.Minute {
			t.Fatalf("adoption ended after %v, want 30m", d)
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

// Spec C4: CommitAdoption runs under context.WithoutCancel, so the end
// of the adoption's context cannot abort a commit in progress.
func TestAdoption_CommitSurvivesTheAdoptionDeadline(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &adoptionRequest{suite: env.suite, form: adoptionFormInline,
		bytes: datedRelease("v1", 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})}
	// membersDone runs right before the commit: cancelling there models
	// the deadline expiring at the worst moment.
	if err := env.adopter.runRequest(ctx, req, cancel); err != nil {
		t.Fatalf("commit aborted by the adoption context: %v", err)
	}
	if currentSnapshotID(t, env) == 0 {
		t.Fatal("no current snapshot after the commit")
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
	if err := env.adopter.runRequest(context.Background(), req, membersDone); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("membersDone never ran")
	}
}
```

In `internal/config/config_test.go` add:

```go
// adoption.max_duration (spec C11): default 30m, at least 1m.
func TestAdoptionMaxDuration_DefaultAndBounds(t *testing.T) {
	load := func(extra string) (*Config, error) {
		dir := t.TempDir()
		return Load(writeTOML(t, dir, "config.toml", "[cache]\ndir = \""+dir+"\"\n"+extra))
	}
	cfg, err := load("")
	if err != nil || cfg.Adoption.MaxDuration.Duration != 30*time.Minute {
		t.Fatalf("default = %v (err %v), want 30m", cfg.Adoption.MaxDuration.Duration, err)
	}
	if cfg, err = load("[adoption]\nmax_duration = \"45m\"\n"); err != nil || cfg.Adoption.MaxDuration.Duration != 45*time.Minute {
		t.Fatalf("explicit 45m = %v (err %v)", cfg.Adoption.MaxDuration.Duration, err)
	}
	for _, bad := range []string{"30s", "0s"} {
		if _, err := load("[adoption]\nmax_duration = \"" + bad + "\"\n"); err == nil {
			t.Errorf("max_duration = %s accepted, want a validation error", bad)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ ./internal/config/ -run 'TestAdmission|TestLease|TestCheck_NoSlot|TestCheck_GlobalSlot|TestCheck_RepairObeys|TestReconcile_NoSlot|TestAdoption_MaxDuration|TestAdoption_CommitSurvives|TestAdoption_HostSlotFreed|TestAdoptionMaxDuration'`
Expected: compile failure naming `newAdmission`, `admission` (field), `runRequest`, `MaxDuration`.

- [ ] **Step 3: The admission type**

`internal/freshness/admission.go`:

```go
package freshness

import "sync"

// admission is the one capacity discipline for adoption, repair and
// reconcile (spec C4): a global slot pool (max_concurrent_adoptions,
// 0 = no global limit) and one slot per canonical host. Callers
// TryAcquire before spawning and never wait for a slot, so no suite
// mutex is ever held while queued.
//
// AIDEV-NOTE: when the pool has two or more slots, the last free one is
// reserved for suites whose current snapshot is signed: security updates
// are never starved by first adoptions (or, from phase E, unsigned ones).
type admission struct {
	mu     sync.Mutex
	global int // 0 = unlimited
	inUse  int
	hosts  map[string]bool
}

// lease holds a global and a host slot until released.
type lease struct {
	a          *admission
	host       string
	hostHeld   bool
	globalHeld bool
}

func newAdmission(global int) *admission {
	return &admission{global: global, hosts: map[string]bool{}}
}

// tryAcquire takes a global and a host slot, or nothing.
func (a *admission) tryAcquire(host string, signedCurrent bool) (*lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hosts[host] {
		return nil, false
	}
	if a.global > 0 {
		free := a.global - a.inUse
		if free <= 0 || (a.global >= 2 && free == 1 && !signedCurrent) {
			return nil, false
		}
	}
	a.inUse++
	a.hosts[host] = true
	return &lease{a: a, host: host, hostHeld: true, globalHeld: true}, true
}

// usage reports the global slots and hosts in use.
func (a *admission) usage() (global, hosts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inUse, len(a.hosts)
}

// releaseHost frees the host slot when the member pipeline ends; the
// global slot stays held through the commit. Idempotent, nil-safe.
func (l *lease) releaseHost() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
	}
}

// release frees whatever the lease still holds. Idempotent, nil-safe.
func (l *lease) release() {
	if l == nil {
		return
	}
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.hostHeld {
		delete(l.a.hosts, l.host)
		l.hostHeld = false
	}
	if l.globalHeld {
		l.a.inUse--
		l.globalHeld = false
	}
}
```

- [ ] **Step 4: The adopter loses its blocking cap**

In `internal/freshness/adoption.go`:
- Add constants near `maxDecompressedPackagesBytes`:

```go
// commitTimeout bounds CommitAdoption, which runs detached from the
// adoption's context (spec C4).
const commitTimeout = 5 * time.Minute

// defaultMaxAdoptionDuration is adoption.max_duration's default.
const defaultMaxAdoptionDuration = 30 * time.Minute
```

- In `AdoptionConfig`, reword `MaxConcurrent`'s comment to "MaxConcurrent is max_concurrent_adoptions: the Checker's global admission slots for adoption, repair and reconcile (spec C4). Zero means unlimited." and add:

```go
	// MaxDuration bounds every adoption, repair and reconcile run
	// (adoption.max_duration, spec C4). Zero means 30m.
	MaxDuration time.Duration
```

- In `Adopter`, replace the `concurrencySem` field and its comment with:

```go
	// maxConcurrent and maxDuration configure the Checker's admission and
	// run deadline (spec C4); the Adopter itself never waits for a slot.
	maxConcurrent int
	maxDuration   time.Duration
```

- In `NewAdopter`: add `if cfg.MaxDuration < 0 { return nil, fmt.Errorf("freshness: adoption.max_duration must be >= 0, got %s", cfg.MaxDuration) }`; delete the `sem` construction; set `maxConcurrent: cfg.MaxConcurrent` and `maxDuration: maxDur` where

```go
	maxDur := cfg.MaxDuration
	if maxDur == 0 {
		maxDur = defaultMaxAdoptionDuration
	}
```

- In `adoptionPayload`, add:

```go
	// membersDone, when set, runs once the member pipeline (members,
	// by-hash, hot prefetch) is over and before the commit: the Checker
	// frees the host slot there (spec C4).
	membersDone func()
```

- Add, after `RunDetached`:

```go
// runRequest runs the adoption a check requested. membersDone (may be
// nil) runs before the commit; see adoptionPayload.membersDone.
func (a *Adopter) runRequest(ctx context.Context, req *adoptionRequest, membersDone func()) error {
	p := &adoptionPayload{form: req.form, membersDone: membersDone}
	switch req.form {
	case adoptionFormInline:
		p.inlineBytes = req.bytes
	case adoptionFormDetached:
		p.releaseBytes, p.sigBytes = req.releaseBytes, req.sigBytes
	}
	return a.runShared(ctx, req.suite, p, req.etag, req.lastmod)
}
```

- In `runShared`, delete Step 0 (the `concurrencySem` select). In `Run`'s doc comment replace "the global concurrency cap held inside Run bounds total parallel adoptions" with "admission (global and per-host slots) is the Checker's job".
- Replace the Step 11 block's opening (comment and `CommitAdoption` call) with:

```go
	// Spec C4: the member pipeline is over; free the host slot so the
	// next suite on this host can start. The global slot stays held
	// through the commit.
	if p.membersDone != nil {
		p.membersDone()
	}

	// Step 11: atomic flip transaction, under its own bound (spec C4): the
	// adoption deadline (adoption.max_duration) or a shutdown must not
	// abort a commit already in progress. coverageComplete is the SPEC3
	// §7.5.4 per-snapshot proof for strict mode.
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancelCommit()
	if err := a.cache.CommitAdoption(commitCtx, snapshotID, memberRows, integritySkipped, packageHashes,
		prefetchedURLPaths, pkgHashRes.coverageComplete); err != nil {
```

In `internal/freshness/repair.go`, delete the "Global concurrency cap" block, change the doc comment's admission sentence to "Admission (a global and a host slot) is taken by the Checker before it spawns the repair; this method never waits for one.", and add:

```go
// repairPending reports whether the snapshot has integrity-class skip
// rows a repair pass could heal: one indexed read, so the Checker asks
// before it takes an admission slot.
func (a *Adopter) repairPending(ctx context.Context, suite SuiteRef, snapshotID int64) bool {
	rows, err := a.cache.ListRepairableSkippedMembers(ctx, snapshotID)
	if err != nil {
		a.logger.Warn("adoption_repair_list_failed",
			"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
			"snapshot_id", snapshotID, "err", err)
		return false
	}
	return len(rows) > 0
}
```

In `internal/freshness/reconcile.go`, delete the `concurrencySem` block in `ReconcileSnapshot` and add:

```go
// reconcilePending reports whether ReconcileSnapshot(force=false) would
// fetch anything. A memoised snapshot costs nothing; otherwise the
// trust-anchored Release is re-parsed once, and a complete snapshot is
// memoised so later checks skip it. The Checker asks before it takes an
// admission slot.
func (a *Adopter) reconcilePending(ctx context.Context, snapshotID int64) (bool, error) {
	if _, done := a.reconciledSnapshots.Load(snapshotID); done {
		return false, nil
	}
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		return false, err
	}
	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return false, fmt.Errorf("reconcile: list members: %w", err)
	}
	if len(missingRequestableMembers(declared, present, a.architectureAllowlist)) == 0 {
		a.reconciledSnapshots.Store(snapshotID, struct{}{})
		return false, nil
	}
	return true, nil
}
```

- [ ] **Step 5: The checker takes slots and spawns**

In `internal/freshness/freshness.go`:

Add to `Checker`:

```go
	// admission hands out the global and per-host slots for adoption,
	// repair and reconcile (spec C4); maxAdoptionDuration bounds each run.
	admission           *admission
	maxAdoptionDuration time.Duration
```

In `New`, before the return:

```go
	maxConc, maxDur := 0, defaultMaxAdoptionDuration
	if cfg.Adopter != nil {
		maxConc, maxDur = cfg.Adopter.maxConcurrent, cfg.Adopter.maxDuration
	}
```

and set `admission: newAdmission(maxConc), maxAdoptionDuration: maxDur` in the struct literal.

Add after `repairRequest`:

```go
// checkOutcome is what one check produces. lease is the admission slot
// taken for req; on a panic checkLockedSafe releases it.
type checkOutcome struct {
	req    *adoptionRequest
	repair *repairRequest
	lease  *lease
}
```

Replace `Check` (keep its doc comment, adding a sentence: "Before spawning anything it takes an admission slot without waiting (spec C4).") with:

```go
func (c *Checker) Check(ctx context.Context, scheme, host, suitePath string) {
	key := suiteKey(scheme, host, suitePath)
	muVal, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		// Another goroutine is on it; SPEC §7.3 says skip.
		return
	}
	out, ok := c.checkLockedSafe(ctx, scheme, host, suitePath)
	switch {
	case !ok:
		mu.Unlock()
	case out.req != nil && out.lease != nil:
		c.spawnAdoption(mu, out.req, out.lease)
	case out.repair != nil:
		c.maybeSpawnRepair(ctx, mu, out.repair)
	default:
		mu.Unlock()
	}
}

// spawnAdoption hands the suite mutex and the admission lease to the
// adoption goroutine, which runs against the lifetime context bounded by
// adoption.max_duration (a request closing must not abort it).
//
// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
// release, recoverAdoption (first). The failure record is written before
// the mutex is released on both the error and the panic path (spec C7).
// The host slot is freed earlier, when the member pipeline ends.
func (c *Checker) spawnAdoption(mu *sync.Mutex, req *adoptionRequest, l *lease) {
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		// AIDEV-NOTE: monotonic time.Now/time.Since, never c.now(): a
		// wall-clock step mid-adoption must not produce a negative duration.
		start := time.Now()
		defer c.recoverAdoption(req, start)
		actx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		err := c.adopter.runRequest(actx, req, l.releaseHost)
		duration := time.Since(start)
		outcome := classifyAdoptionOutcome(err)
		if err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && c.lifetimeCtx.Err() == nil {
			outcome = "timed_out" // a member fetch reports the deadline as its own failure
		}
		reason := classifyAdoptionReason(err, outcome, c.gpgReasonClassifier)
		adoptionTotal.Inc(outcome, req.suite.CanonicalHost)
		adoptionDurationSeconds.Observe(duration.Seconds(), outcome, req.suite.CanonicalHost)
		if c.adoptionRing != nil {
			memberPath, detail := memberErrorFields(err)
			c.adoptionRing.Record(observability.AdoptionEvent{
				Host:             req.suite.CanonicalHost,
				SuitePath:        req.suite.SuitePath,
				Outcome:          outcome,
				Reason:           reason,
				CompletedUnixSec: c.now().Unix(),
				DurationSeconds:  duration.Seconds(),
				MemberPath:       memberPath,
				Detail:           detail,
			})
		}
		if err != nil {
			// Several Adopter paths log their own categorized line first;
			// this backstop makes every failure grep-able.
			c.logger.Warn("adoption_run_failed",
				"canonical_host", req.suite.CanonicalHost,
				"suite_path", req.suite.SuitePath,
				"form", formName(req.form),
				"err", err,
			)
			c.recordAdoptionFailure(req, reason)
		}
	}()
}

// maybeSpawnRepair runs the unchanged-suite heal (SPEC6_7 §3 repair and
// SPEC6_8 §6 reconcile) when there is something to heal and an admission
// slot is free; otherwise it releases the mutex and returns at once.
//
// AIDEV-NOTE: deliberate deviation from spec C4's "TryAcquire before
// spawning": the cheap "anything to do?" checks run first, so healthy
// suites (the steady state) never compete with adoptions for slots. The
// slot is still taken without waiting and before spawning.
func (c *Checker) maybeSpawnRepair(ctx context.Context, mu *sync.Mutex, rp *repairRequest) {
	if c.adopter == nil || !c.adopter.repairSkippedMembers {
		mu.Unlock()
		return
	}
	needRepair := c.adopter.repairPending(ctx, rp.suite, rp.snapshotID)
	needReconcile, err := c.adopter.reconcilePending(ctx, rp.snapshotID)
	if err != nil {
		// A fail-closed anchor (R-1 decode error) must be visible, not a
		// silent non-heal.
		c.logger.Warn("adoption_reconcile_failed",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
			"snapshot_id", rp.snapshotID, "err", err)
	}
	if !needRepair && !needReconcile {
		mu.Unlock()
		return
	}
	l, ok := c.admission.tryAcquire(rp.suite.CanonicalHost, true)
	if !ok {
		c.logger.Debug("freshness: repair deferred, no adoption slot",
			"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath)
		mu.Unlock()
		return
	}
	// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
	// release, recover (first). See recoverGoroutine.
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		defer c.recoverGoroutine("repair", rp.suite)
		rctx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		if needRepair {
			c.adopter.RepairSkippedMembers(rctx, rp.suite, rp.snapshotID)
		}
		if needReconcile {
			if _, err := c.adopter.ReconcileSnapshot(rctx, rp.suite, rp.snapshotID, false); err != nil {
				c.logger.Warn("adoption_reconcile_failed",
					"canonical_host", rp.suite.CanonicalHost, "suite_path", rp.suite.SuitePath,
					"snapshot_id", rp.snapshotID, "err", err)
			}
		}
	}()
}
```

Replace `checkLockedSafe` with:

```go
func (c *Checker) checkLockedSafe(ctx context.Context, scheme, host, suitePath string) (out *checkOutcome, ok bool) {
	out = &checkOutcome{}
	defer func() {
		if r := recover(); r != nil {
			out.lease.release()
			c.reportPanic("freshness_check_panicked", "check",
				SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, r)
			c.bumpFailedCheck(ctx, scheme, host, suitePath)
			freshnessCheckTotal.Inc("failed", host)
			out, ok = nil, false
		}
	}()
	c.checkLocked(ctx, out, scheme, host, suitePath)
	return out, true
}
```

Change `checkLocked` to `func (c *Checker) checkLocked(ctx context.Context, out *checkOutcome, scheme, host, suitePath string)`: every `return nil, nil` becomes `return`; the form dispatch at the end becomes:

```go
	form := c.detectForm(ctx, cur)
	if form == adoptionFormDetached {
		out.req, out.repair = c.checkLockedDetached(ctx, out, cur, suite, now, nowUnix)
		return
	}
	req, repair, fellBack := c.checkLockedInline(ctx, out, cur, suite, now, nowUnix)
	if !fellBack {
		out.req, out.repair = req, repair
		return
	}
	out.req, out.repair = c.checkLockedDetached(ctx, out, cur, suite, now, nowUnix)
```

Add the `out *checkOutcome` parameter (after `ctx`) to `checkLockedInline` and `checkLockedDetached`. In both, remember whether the current snapshot is signed and the previous success stamp. In `checkLockedInline` replace the cached-hash block with:

```go
	var cachedHash string
	signedCurrent := false
	if cur.CurrentSnapshotID != nil {
		snap, serr := c.cache.GetSuiteSnapshot(ctx, *cur.CurrentSnapshotID)
		if serr != nil {
			c.logger.Warn("freshness: snapshot lookup failed; falling back to url_path",
				"err", serr,
				"snapshot_id", *cur.CurrentSnapshotID,
				"canonical_host", host,
				"suite_path", suitePath,
			)
		} else {
			signedCurrent = snap.Form == cache.FormInline || snap.Form == cache.FormDetached
			if snap.Form == cache.FormInline && snap.SingleAnchorHash != nil {
				cachedHash = *snap.SingleAnchorHash
			}
		}
	}
```

and in `checkLockedDetached` the same shape with `if snap.ReleaseHash != nil { cachedHash = *snap.ReleaseHash }` in place of the inline condition. In both functions, put `prevSuccess := cur.LastSuccessAt` immediately before the `cur.LastCheckAt = &nowUnix` / `cur.LastSuccessAt = &nowUnix` pair that follows a successful `Conditional`. In both `changed` branches, right after the `req = &adoptionRequest{…}` literal, add:

```go
			if c.adopter != nil {
				l, ok := c.admission.tryAcquire(host, signedCurrent)
				if !ok {
					// Spec C4: no slot — spawn nothing and keep
					// last_success_at, so the scheduler retries this suite on
					// its next turn (admission rotates through queued suites).
					result, req = "changed_queued", nil
					cur.LastSuccessAt = prevSuccess
				} else {
					out.lease = l
				}
			}
```

(In the detached function the `changed` branch is the code after `key := contentKey(…)`; insert the block after its `req = &adoptionRequest{…}` literal.)

Replace the `Reconcile` body after `mu.TryLock()` with:

```go
	if !mu.TryLock() {
		return false
	}
	l, ok := c.admission.tryAcquire(host, true)
	if !ok {
		mu.Unlock()
		return false // POST /reconcile answers 409 (spec C4)
	}
	// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, lease
	// release, recover (first). See recoverGoroutine.
	c.adoptionWg.Add(1)
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer l.release()
		defer c.recoverGoroutine("reconcile", suite)
		rctx, cancel := context.WithTimeout(c.lifetimeCtx, c.maxAdoptionDuration)
		defer cancel()
		if _, err := c.adopter.ReconcileSnapshot(rctx, suite, snapID, true); err != nil {
			c.logger.Warn("adoption_reconcile_failed",
				"canonical_host", host, "suite_path", suitePath,
				"snapshot_id", snapID, "err", err)
		}
	}()
	return true
```

and extend `Reconcile`'s doc comment: "It also returns false when no admission slot is free (spec C4: POST /reconcile then answers 409)."

- [ ] **Step 6: Configuration**

In `internal/config/config.go`, add to the adoption config struct (after `HotPrefetchBudget`):

```go
	// MaxDuration bounds every adoption, repair and reconcile run (spec
	// C4, C11). Default 30m; at least 1m.
	MaxDuration Duration `toml:"max_duration"`
```

In `Load`, next to the `hot_prefetch_budget` default: `if !md.IsDefined("adoption", "max_duration") { cfg.Adoption.MaxDuration.Duration = 30 * time.Minute }`. In `Validate`, next to the `HotPrefetchBudget` check: `if c.Adoption.MaxDuration.Duration < time.Minute { errs = append(errs, errors.New("adoption.max_duration must be at least 1m")) }`. (`Validate` runs only from `Load`; programmatic configs leave it 0 and `NewAdopter` applies 30m.)

In `cmd/apt-cacher-ultra/main.go`, pass `MaxDuration: cfg.Adoption.MaxDuration.Duration,` in the `freshness.AdoptionConfig` literal and add `"max_duration", cfg.Adoption.MaxDuration.Duration,` to the `phase2 adoption enabled` log line.

- [ ] **Step 7: Remove the replaced tests**

Delete `TestAdopter_ConcurrencyCap` from `internal/freshness/adoption_test.go` and `TestRepairSkippedMembers_RespectsConcurrencyCap` from `internal/freshness/phase6_7_skip_repair_test.go` (keep `blockingFetcher` and `blockReq`; the new tests use them). Remove imports that become unused.

- [ ] **Step 8: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/config/ ./internal/handler/ ./internal/admin/ ./cmd/...`
Expected: PASS. `TestCheck_UnchangedTick_LogsReconcileFailure` still finds its `adoption_reconcile_failed` WARN (now emitted by `maybeSpawnRepair`).

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/freshness/ internal/config/ cmd/apt-cacher-ultra/
git add internal/freshness/ internal/config/ cmd/apt-cacher-ultra/main.go
git commit -m "Admit adoption, repair and reconcile through non-blocking global and host slots"
```

---

### Task 10: Check deadline and the unknown-check streak

**Files:**
- Modify: `internal/freshness/freshness.go` (`Check`, the failure/success paths of `checkLockedInline` and `checkLockedDetached`, `bumpFailedCheck`)
- Create: `internal/freshness/check_streak_test.go`

**Interfaces:**
- Consumes: Task 1 (`CheckFailCount`, written by `PutSuiteFreshness`), Task 6 (`rigUpstream`), existing `fakeCache`, `seedFakeAnchor`, `changedUpstream`, `discardLogger`.
- Produces: `const checkDeadline = 2 * time.Minute`; `CheckFailCount` maintenance by every check (T1 and T2): +1 on an *unknown* end (transport error, deadline, non-2xx/304 status, panic), 0 on any answered check; a cancelled caller records nothing.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/check_streak_test.go`:

```go
package freshness

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func streakChecker(t *testing.T, f Fetcher) (*Checker, *fakeCache) {
	t.Helper()
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})
	c, err := New(Config{Cache: fc, Fetcher: f, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return c, fc
}

func streakOf(t *testing.T, fc *fakeCache) cache.SuiteFreshness {
	t.Helper()
	s, ok := fc.suite("http", "h", "/r/dists/x")
	if !ok {
		t.Fatal("suite row missing")
	}
	return s
}

// Spec C4: check_fail_count is the streak of checks that ended unknown;
// any answered check resets it.
func TestCheck_UnknownStreakCountsAndResets(t *testing.T) {
	up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}} // 404: unknown in phase B
	c, fc := streakChecker(t, up)
	for i := int64(1); i <= 3; i++ {
		c.Check(context.Background(), "http", "h", "/r/dists/x")
		if got := streakOf(t, fc).CheckFailCount; got != i {
			t.Fatalf("after failure %d: streak = %d", i, got)
		}
	}
	up.set([]byte("new"))
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := streakOf(t, fc).CheckFailCount; got != 0 {
		t.Fatalf("after an answered check: streak = %d, want 0", got)
	}
}

type oddStatusUpstream struct{}

func (oddStatusUpstream) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	return &fetch.ConditionalResult{Status: 206}, nil
}

func TestCheck_UnexpectedStatusExtendsTheStreak(t *testing.T) {
	c, fc := streakChecker(t, oddStatusUpstream{})
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := streakOf(t, fc).CheckFailCount; got != 1 {
		t.Fatalf("streak = %d, want 1", got)
	}
}

func TestCheck_PanicExtendsTheStreak(t *testing.T) {
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	c, fc := streakChecker(t, up)
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if s := streakOf(t, fc); s.CheckFailCount != 1 || s.LastCheckAt == nil {
		t.Fatalf("streak = %d, last_check_at = %v; want 1 and set", s.CheckFailCount, s.LastCheckAt)
	}
}

// Spec C4: every check runs under a 2-minute deadline (the D of the
// scheduler's latency bound); hitting it is an unknown end.
func TestCheck_DeadlineBoundsAStalledUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		up.setStall(true)
		c, fc := streakChecker(t, up)
		start := time.Now()
		c.Check(context.Background(), "http", "h", "/r/dists/x")
		if d := time.Since(start); d != checkDeadline {
			t.Fatalf("check took %v, want %v", d, checkDeadline)
		}
		if s := streakOf(t, fc); s.CheckFailCount != 1 || s.LastCheckAt == nil {
			t.Fatalf("streak %d last_check_at %v", s.CheckFailCount, s.LastCheckAt)
		}
	})
}

// A cancelled caller (shutdown) says nothing about upstream: no failure.
func TestCheck_CancelledCallerRecordsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		up.setStall(true)
		c, fc := streakChecker(t, up)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel)
		c.Check(ctx, "http", "h", "/r/dists/x")
		if s := streakOf(t, fc); s.CheckFailCount != 0 || s.LastCheckAt != nil {
			t.Fatalf("cancelled check recorded streak %d, last_check_at %v", s.CheckFailCount, s.LastCheckAt)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestCheck_UnknownStreak|TestCheck_UnexpectedStatusExtends|TestCheck_PanicExtends|TestCheck_DeadlineBounds|TestCheck_CancelledCaller'`
Expected: compile failure naming `checkDeadline`; once that constant exists, FAIL on the streak assertions.

- [ ] **Step 3: Deadline and streak**

In `internal/freshness/freshness.go`:

Add near the other constants:

```go
// checkDeadline bounds one whole freshness check (spec C4, a constant):
// it is the D of the scheduler's latency bound. Adoption, repair and
// reconcile goroutines run under their own contexts.
const checkDeadline = 2 * time.Minute
```

In `Check`, right after the successful `TryLock`:

```go
	ctx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()
```

In `checkLockedInline`'s `Conditional` error path, before the detached-fallback test, add:

```go
		if errors.Is(err, context.Canceled) {
			// A cancelled caller (shutdown) is not evidence about upstream;
			// the check deadline (DeadlineExceeded) is, and counts below.
			c.logger.Debug("freshness: check cancelled", "canonical_host", host, "suite_path", suitePath)
			return nil, nil, false
		}
```

and, where that path bumps `cur.LastCheckAt = &nowUnix`, add `cur.CheckFailCount++` (spec C4: the UPDATE that bumps last_check_at after an unknown check extends the streak). After the success pair `cur.LastCheckAt = &nowUnix; cur.LastSuccessAt = &nowUnix`, add `cur.CheckFailCount = 0`. In the `default:` (unexpected status) branch, before its `PutSuiteFreshness`, add `cur.CheckFailCount++`. Make the same three edits in `checkLockedDetached` (its `Release` error path returns `nil, nil` on cancellation).

Replace `bumpFailedCheck` with:

```go
// bumpFailedCheck mirrors checkLocked's unknown-check bookkeeping after a
// panic: last_check_at = now and one more on the streak (spec C4);
// last_success_at untouched. Best effort; it never panics its caller.
// AIDEV-NOTE: phase D turns the Put into the UPDATE-only writer.
func (c *Checker) bumpFailedCheck(ctx context.Context, scheme, host, suitePath string) {
	defer func() { _ = recover() }()
	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		cur = &cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	case err != nil:
		c.logger.Warn("freshness: read suite row after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
		return
	}
	now := c.now().Unix()
	cur.LastCheckAt = &now
	cur.CheckFailCount++
	if err := c.cache.PutSuiteFreshness(ctx, *cur); err != nil {
		c.logger.Warn("freshness: persist failure bump after a panic failed",
			"err", err, "canonical_host", host, "suite_path", suitePath)
	}
}
```

(This closes deferred minor A-36.)

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/
git add internal/freshness/
git commit -m "Bound every check by a deadline and keep the unknown-check streak"
```

---

### Task 11: The fair per-host scheduler

**Files:**
- Modify: `internal/freshness/scheduler.go` (rewrite `Run`, `tick`, `checkRecovered`; add the queue types, `suiteDue`, `classOf`, `checkWorker`, `safeTick`, `failingAdoptionCount`), `internal/freshness/metrics.go` (gauge `acu_adoption_failing_suites` + inventory note), `internal/freshness/freshness_test.go` (`fakeCache.listHook`; `TestTick_FilersByLastSuccessAt` and `TestStaleSuiteCounts`), `internal/freshness/clockstep_test.go` (`TestTick_FutureLastSuccessAtIsDue`), `internal/freshness/sim_smoke_test.go` (exact count), `internal/simtest/simtest.go`
- Create: `internal/freshness/scheduler_test.go`, `internal/simtest/guard_test.go`

**Interfaces:**
- Consumes: Task 10 (`checkDeadline`, `CheckFailCount`), Task 5 (`backoffDelay`), Task 1 (`AdoptionNextRetryAt`, `IsRefusalReason`).
- Produces: `const checkWorkers = 4`; `type schedClass int` with `classHealthy`, `classNeverChecked`, `classFailing`, `numSchedClasses`; `classOf(cache.SuiteFreshness) schedClass`; `type schedItem struct{ ref SuiteRef; key string }`; `newSchedQueues() *schedQueues` with `enqueue(cache.SuiteFreshness) bool`, `pop() (schedItem, bool)`, `done(schedItem)`, `len() int`; `(*Checker).suiteDue(s cache.SuiteFreshness, now time.Time) bool`; `(*Checker).tick(ctx, q *schedQueues)`; `(*Checker).checkRecovered(ctx, ref SuiteRef)`; `failingAdoptionCount([]cache.SuiteFreshness) int`; test helpers `drainTick(ctx, c *Checker)` and `seedFakeSuite(fc, host, suite string, lastSuccess *int64)`; `simtest.Behavior`, `simtest.NotModified()`, `simtest.Serve(body)`, `simtest.Stall()`, `simtest.After(d, next)`, `simtest.ETag(body)`, `simtest.NewUpstream() *simtest.Upstream` with `Set`, `Conditional`, `Calls`, `MaxInFlight`, `ResetMax`.

- [ ] **Step 1: The simulation upstream**

Append to `internal/simtest/simtest.go` (add imports `crypto/sha256`, `encoding/hex`, `sync`, `time`), and fix the `StaticConditional` doc comment (deferred minor A-45) to "answering every conditional GET with Status and Body, counting calls":

```go
// Behavior answers one conditional GET for an Upstream route.
type Behavior func(ctx context.Context, etag string) (*fetch.ConditionalResult, error)

// NotModified always answers 304.
func NotModified() Behavior {
	return func(context.Context, string) (*fetch.ConditionalResult, error) {
		return &fetch.ConditionalResult{Status: 304}, nil
	}
}

// Serve answers 200 with body and its ETag, or 304 when the request's
// validator equals that ETag.
func Serve(body []byte) Behavior {
	tag := ETag(body)
	return func(_ context.Context, etag string) (*fetch.ConditionalResult, error) {
		if etag == tag {
			return &fetch.ConditionalResult{Status: 304, ETag: tag}, nil
		}
		return &fetch.ConditionalResult{Status: 200, Body: body, ETag: tag}, nil
	}
}

// Stall blocks until the request context ends: a host that accepts the
// connection and never answers.
func Stall() Behavior {
	return func(ctx context.Context, _ string) (*fetch.ConditionalResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// After waits d, or until the request context ends, then answers with
// next.
func After(d time.Duration, next Behavior) Behavior {
	return func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
		return next(ctx, etag)
	}
}

// ETag is the validator Serve gives body.
func ETag(body []byte) string {
	s := sha256.Sum256(body)
	return "e-" + hex.EncodeToString(s[:6])
}

// Upstream is a freshness.Fetcher that routes by URL to a Behavior
// (unknown URLs answer 404), records the time of every request, and
// tracks how many requests are in flight, overall and per canonical host.
type Upstream struct {
	mu         sync.Mutex
	routes     map[string]Behavior
	calls      map[string][]time.Time
	inFlight   map[string]int
	total      int
	maxTotal   int
	maxPerHost int
}

// NewUpstream returns an Upstream with no routes.
func NewUpstream() *Upstream {
	return &Upstream{routes: map[string]Behavior{}, calls: map[string][]time.Time{}, inFlight: map[string]int{}}
}

// Set routes url to b, replacing any earlier route.
func (u *Upstream) Set(url string, b Behavior) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.routes[url] = b
}

// Conditional implements freshness.Fetcher.
func (u *Upstream) Conditional(ctx context.Context, target *fetch.Target, etag, _ string, _ int64) (*fetch.ConditionalResult, error) {
	host := target.CanonicalHost
	u.mu.Lock()
	b, ok := u.routes[target.URL]
	u.calls[target.URL] = append(u.calls[target.URL], time.Now())
	u.total++
	u.inFlight[host]++
	u.maxTotal = max(u.maxTotal, u.total)
	u.maxPerHost = max(u.maxPerHost, u.inFlight[host])
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.total--
		u.inFlight[host]--
		u.mu.Unlock()
	}()
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	return b(ctx, etag)
}

// Calls returns the times url was requested, in order.
func (u *Upstream) Calls(url string) []time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]time.Time(nil), u.calls[url]...)
}

// MaxInFlight returns the most requests ever in flight at once, overall
// and on any one host, since creation or the last ResetMax.
func (u *Upstream) MaxInFlight() (total, perHost int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.maxTotal, u.maxPerHost
}

// ResetMax restarts the maxima from the requests in flight now.
func (u *Upstream) ResetMax() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.maxTotal, u.maxPerHost = u.total, 0
	for _, n := range u.inFlight {
		u.maxPerHost = max(u.maxPerHost, n)
	}
}
```

`internal/simtest/guard_test.go` (deferred minor A-46):

```go
package simtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// simtest imports "testing", so production code must never import it.
func TestNoProductionCodeImportsSimtest(t *testing.T) {
	const root = "../.."
	const imp = `"github.com/linsomniac/apt-cacher-ultra/internal/simtest"`
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.HasPrefix(filepath.ToSlash(p), root+"/internal/simtest/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), imp) {
			t.Errorf("%s imports internal/simtest", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Write the failing scheduler tests**

In `internal/freshness/freshness_test.go`, add a `listHook func()` field to `fakeCache` and call it at the top of `ListSuites` (before taking the mutex): `if h := f.listHook; h != nil { h() }`.

`internal/freshness/scheduler_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// drainTick runs one scheduler tick and then every queued check
// synchronously, in the order the worker pool would start them.
func drainTick(ctx context.Context, c *Checker) {
	q := newSchedQueues()
	c.tick(ctx, q)
	for {
		item, ok := q.pop()
		if !ok {
			return
		}
		c.checkRecovered(ctx, item.ref)
		q.done(item)
	}
}

// seedFakeSuite gives fc a suite with a cached InRelease anchor at
// http://<host><suite>/InRelease.
func seedFakeSuite(fc *fakeCache, host, suite string, lastSuccess *int64) {
	bh := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastSuccessAt: lastSuccess, LastCheckAt: lastSuccess})
}

func schedRow(host, suite string, lastCheck *int64, fails int64) cache.SuiteFreshness {
	return cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastCheckAt: lastCheck, CheckFailCount: fails}
}

func popAll(q *schedQueues) []string {
	var out []string
	for {
		it, ok := q.pop()
		if !ok {
			return out
		}
		out = append(out, it.ref.CanonicalHost+it.ref.SuitePath)
		q.done(it)
	}
}

// Spec C4: within a host, healthy before never-checked before failing;
// first in, first out inside a class.
func TestSchedQueues_ClassesThenFIFO(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/f1", &one, 2))
	q.enqueue(schedRow("a", "/n1", nil, 0))
	q.enqueue(schedRow("a", "/h1", &one, 0))
	q.enqueue(schedRow("a", "/h2", &one, 0))
	got := popAll(q)
	want := []string{"a/h1", "a/h2", "a/n1", "a/f1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Spec C4: one check per host turn, round-robin, and never two suites of
// one host in flight.
func TestSchedQueues_RoundRobinOneInFlightPerHost(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	for _, s := range [][2]string{{"a", "/1"}, {"a", "/2"}, {"b", "/1"}, {"c", "/1"}, {"c", "/2"}} {
		q.enqueue(schedRow(s[0], s[1], &one, 0))
	}
	i1, _ := q.pop()
	i2, _ := q.pop()
	i3, _ := q.pop()
	if got := i1.ref.CanonicalHost + i2.ref.CanonicalHost + i3.ref.CanonicalHost; got != "abc" {
		t.Fatalf("first turn = %s, want abc", got)
	}
	if it, ok := q.pop(); ok {
		t.Fatalf("popped %v while every host had a check in flight", it.ref)
	}
	q.done(i1)
	if it, _ := q.pop(); it.ref.CanonicalHost+it.ref.SuitePath != "a/2" {
		t.Fatalf("after a finished: got %v, want a/2", it.ref)
	}
	q.done(i3)
	if it, _ := q.pop(); it.ref.CanonicalHost+it.ref.SuitePath != "c/2" {
		t.Fatalf("after c finished: got %v, want c/2", it.ref)
	}
}

func TestSchedQueues_NoDuplicatesWhileQueuedOrInFlight(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	s := schedRow("a", "/1", &one, 0)
	if !q.enqueue(s) || q.enqueue(s) {
		t.Fatal("a queued suite was queued twice")
	}
	it, _ := q.pop()
	if q.enqueue(s) {
		t.Fatal("a suite was queued while its check was in flight")
	}
	q.done(it)
	if !q.enqueue(s) {
		t.Fatal("a suite could not be queued again after its check")
	}
}

func TestSchedQueues_PrunesIdleHosts(t *testing.T) {
	q := newSchedQueues()
	one := int64(1)
	q.enqueue(schedRow("a", "/1", &one, 0))
	q.enqueue(schedRow("b", "/1", &one, 0))
	it, _ := q.pop()
	q.done(it)
	if len(q.hosts) != 1 || len(q.order) != 1 || q.len() != 1 {
		t.Fatalf("hosts %d order %d queued %d, want 1/1/1", len(q.hosts), len(q.order), q.len())
	}
}

func TestSuiteDue(t *testing.T) {
	c, err := New(Config{Cache: newFakeCache(), Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(1),
		Refresh: time.Hour, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) *int64 { v := now.Add(d).Unix(); return &v }
	cases := []struct {
		name string
		s    cache.SuiteFreshness
		want bool
	}{
		{"never succeeded", cache.SuiteFreshness{}, true},
		{"fresh", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute)}, false},
		{"stale", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour)}, true},
		{"exactly one refresh old", cache.SuiteFreshness{LastSuccessAt: at(-time.Hour)}, true},
		{"success in the future (clock step)", cache.SuiteFreshness{LastSuccessAt: at(time.Hour)}, true},
		{"success 2 s ahead is within tolerance", cache.SuiteFreshness{LastSuccessAt: at(2 * time.Second)}, false},
		{"failing inside backoff", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-time.Minute), CheckFailCount: 1}, false},
		{"failing past backoff", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-3 * time.Minute), CheckFailCount: 1}, true},
		{"long streak waits the hour", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(-59 * time.Minute), CheckFailCount: 9}, false},
		{"failing check stamp in the future", cache.SuiteFreshness{LastSuccessAt: at(-2 * time.Hour), LastCheckAt: at(time.Hour), CheckFailCount: 9}, true},
		{"adoption retry due", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(-time.Second)}, true},
		{"adoption retry not yet due", cache.SuiteFreshness{LastSuccessAt: at(-time.Minute), AdoptionFailCount: 1, AdoptionNextRetryAt: at(time.Minute)}, false},
	}
	for _, tc := range cases {
		if got := c.suiteDue(tc.s, now); got != tc.want {
			t.Errorf("%s: suiteDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Spec C4: four workers, at most one check in flight per host.
func TestRun_FourWorkersOneCheckPerHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		up := simtest.NewUpstream()
		var urls []string
		for h := 0; h < 10; h++ {
			for s := 0; s < 3; s++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/x", s)
				seedFakeSuite(fc, host, suite, nil)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.After(time.Minute, simtest.NotModified()))
				urls = append(urls, url)
			}
		}
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(time.Hour)
		cancel()
		<-done
		if total, perHost := up.MaxInFlight(); total != checkWorkers || perHost != 1 {
			t.Fatalf("max in flight = %d overall, %d per host; want %d and 1", total, perHost, checkWorkers)
		}
		for _, u := range urls {
			if len(up.Calls(u)) == 0 {
				t.Errorf("%s never checked", u)
			}
		}
	})
}

// The carry-forward from phase A: a panic in a tick (before any check)
// must not stop the scheduler.
func TestRun_SurvivesATickPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		var panicked atomic.Bool
		fc.listHook = func() {
			if panicked.CompareAndSwap(false, true) {
				panic("list bug")
			}
		}
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", simtest.NotModified())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(10 * time.Minute)
		cancel()
		<-done
		if !panicked.Load() || len(up.Calls("http://h/r/dists/x/InRelease")) == 0 {
			t.Fatalf("panicked=%v calls=%d: the scheduler stopped after a tick panic", panicked.Load(), len(up.Calls("http://h/r/dists/x/InRelease")))
		}
	})
}

// Shutdown cancels in-flight checks instead of waiting out their deadline.
func TestRun_ReturnsPromptlyOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedFakeSuite(fc, "h", "/r/dists/x", nil)
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/InRelease", simtest.Stall())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		time.Sleep(4*time.Minute + 30*time.Second) // the first tick (3m45s) started a stalled check
		start := time.Now()
		cancel()
		<-done
		if d := time.Since(start); d != 0 {
			t.Fatalf("Run took %v after cancel, want 0 (no wait for the check deadline)", d)
		}
	})
}
```

Update existing tests:
- `internal/freshness/freshness_test.go` `TestTick_FilersByLastSuccessAt`: replace `c.tick(context.Background())` with `drainTick(context.Background(), c)`.
- `TestStaleSuiteCounts` (deferred minor A-41): add `mk(i(3002), i(7)), // 2 s ahead: the tolerance edge -> fresh` and `mk(i(3003), i(7)), // 3 s ahead -> infinitely old: count adopted` to the slice and change the expectation to `adopted != 4` / "want 4".
- `internal/freshness/clockstep_test.go` `TestTick_FutureLastSuccessAtIsDue` (deferred minor A-39): give the checker `Logger: slog.New(slog.NewJSONHandler(logs, nil))` with `logs := &lockedBuffer{}`, replace `c.tick(context.Background())` with `drainTick(context.Background(), c)`, and add after the call-count assertion:

```go
	if !strings.Contains(logs.String(), `"stamp":"last_success_at"`) {
		t.Fatalf("the scheduler path did not report the clock step; log:\n%s", logs.String())
	}
```

- `internal/freshness/sim_smoke_test.go` (deferred minor A-44): replace the `358..362` range with an exact check: `if got := up.Calls(); got != 360 { t.Fatalf("checks in 30 virtual days = %d, want exactly 360", got) }` and drop the slack sentence from the comment. `startSimChecker` registers `t.Cleanup(stop)` and its callers keep calling `stop()` (calling it twice must be safe: guard with a `sync.Once`).

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/freshness/ ./internal/simtest/ -run 'TestSchedQueues|TestSuiteDue|TestRun_|TestNoProductionCodeImportsSimtest|TestTick_|TestStaleSuiteCounts'`
Expected: compile failure naming `newSchedQueues`, `schedQueues`, `suiteDue`, `checkWorkers`, and `tick` called with two arguments.

- [ ] **Step 4: Rewrite the scheduler**

Replace `internal/freshness/scheduler.go` below `staleSuiteCounts` (keep `minFastTickInterval`, `staleSuiteFactor`, `staleSuiteCounts` and their comments) with:

```go
// failingAdoptionCount counts suites whose adoption failure record is a
// failure rather than a refusal (spec C7 gauge).
func failingAdoptionCount(suites []cache.SuiteFreshness) int {
	n := 0
	for _, s := range suites {
		if s.AdoptionFailCount > 0 && s.AdoptionFailReason != nil && !cache.IsRefusalReason(*s.AdoptionFailReason) {
			n++
		}
	}
	return n
}

// checkWorkers is the fixed size of the T2 check pool (spec C4, a
// constant).
const checkWorkers = 4

// schedClass orders the suites queued on one host (spec C4), from
// persisted columns so the order survives restarts.
type schedClass int

const (
	classHealthy      schedClass = iota // checked before, check_fail_count = 0
	classNeverChecked                   // last_check_at IS NULL
	classFailing                        // check_fail_count >= 1
	numSchedClasses
)

func classOf(s cache.SuiteFreshness) schedClass {
	switch {
	case s.CheckFailCount > 0:
		return classFailing
	case s.LastCheckAt == nil:
		return classNeverChecked
	}
	return classHealthy
}

type schedItem struct {
	ref SuiteRef
	key string
}

type hostQueue struct {
	classes  [numSchedClasses][]schedItem
	inFlight bool
}

func (h *hostQueue) empty() bool {
	for _, q := range h.classes {
		if len(q) > 0 {
			return false
		}
	}
	return true
}

// schedQueues holds the scheduler's per-host queues of due suites. The
// Run goroutine owns it exclusively, so it takes no locks.
//
// AIDEV-NOTE: invariants: a suite key is in `queued` iff the suite sits
// in a class queue or its check is in flight; a host is in `order` iff
// it has a queued suite or a check in flight; pop never returns a second
// suite of a host whose check is in flight. Memory is bounded by the
// suite table (each key at most once).
type schedQueues struct {
	hosts  map[string]*hostQueue
	order  []string // round-robin ring of hosts
	next   int      // index in order of the host whose turn is next
	queued map[string]struct{}
}

func newSchedQueues() *schedQueues {
	return &schedQueues{hosts: map[string]*hostQueue{}, queued: map[string]struct{}{}}
}

// enqueue appends s to the back of its class on its host, unless the
// suite is already queued or in flight. It reports whether s was added.
func (q *schedQueues) enqueue(s cache.SuiteFreshness) bool {
	key := suiteKey(s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if _, dup := q.queued[key]; dup {
		return false
	}
	h, ok := q.hosts[s.CanonicalHost]
	if !ok {
		h = &hostQueue{}
		q.hosts[s.CanonicalHost] = h
		q.order = append(q.order, s.CanonicalHost)
	}
	cls := classOf(s)
	h.classes[cls] = append(h.classes[cls], schedItem{
		ref: SuiteRef{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath},
		key: key,
	})
	q.queued[key] = struct{}{}
	return true
}

// pop returns the next suite to check: hosts take turns round-robin, one
// check per turn, skipping hosts with a check in flight; within a host,
// the first suite of the best class. It marks the host in flight.
func (q *schedQueues) pop() (schedItem, bool) {
	n := len(q.order)
	for i := 0; i < n; i++ {
		idx := (q.next + i) % n
		h := q.hosts[q.order[idx]]
		if h.inFlight {
			continue
		}
		for c := range h.classes {
			if len(h.classes[c]) == 0 {
				continue
			}
			item := h.classes[c][0]
			h.classes[c][0] = schedItem{}
			h.classes[c] = h.classes[c][1:]
			if len(h.classes[c]) == 0 {
				h.classes[c] = nil // let the backing array go
			}
			h.inFlight = true
			q.next = (idx + 1) % n
			return item, true
		}
	}
	return schedItem{}, false
}

// done records that item's check finished: its host may run its next
// suite and the suite may be queued again.
func (q *schedQueues) done(item schedItem) {
	delete(q.queued, item.key)
	if h, ok := q.hosts[item.ref.CanonicalHost]; ok {
		h.inFlight = false
	}
	q.prune()
}

// prune drops hosts with nothing queued and nothing in flight, keeping
// the round-robin position on the same host where it survives.
func (q *schedQueues) prune() {
	if len(q.order) == 0 {
		return
	}
	nextHost := q.order[q.next%len(q.order)]
	kept := q.order[:0]
	for _, host := range q.order {
		h := q.hosts[host]
		if h.inFlight || !h.empty() {
			kept = append(kept, host)
			continue
		}
		delete(q.hosts, host)
	}
	q.order = kept
	q.next = 0
	for i, host := range q.order {
		if host == nextHost {
			q.next = i
			break
		}
	}
}

// len is the number of suites queued or in flight.
func (q *schedQueues) len() int { return len(q.queued) }

// suiteDue reports whether the scheduler should queue s now (spec C4):
// its last success is older than the refresh interval or its adoption
// retry time has come, and it is not inside the backoff its unknown-check
// streak earns (2 min × 2^(k−1), at most 1 h). Clock-step rule: a
// last_success_at or last_check_at in the future is infinitely old.
func (c *Checker) suiteDue(s cache.SuiteFreshness, now time.Time) bool {
	nowU := now.Unix()
	future := nowU + int64(clockStepTolerance/time.Second)
	stale := s.LastSuccessAt == nil || *s.LastSuccessAt <= now.Add(-c.refresh).Unix()
	if s.LastSuccessAt != nil && *s.LastSuccessAt > future {
		c.clockStep("last_success_at", s.CanonicalScheme, s.CanonicalHost, s.SuitePath, *s.LastSuccessAt, now)
		stale = true
	}
	retryDue := s.AdoptionFailCount > 0 && s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt <= nowU
	if !stale && !retryDue {
		return false
	}
	if s.CheckFailCount > 0 && s.LastCheckAt != nil && *s.LastCheckAt <= future {
		if nowU < *s.LastCheckAt+int64(backoffDelay(s.CheckFailCount)/time.Second) {
			return false
		}
	}
	return true
}

// Run starts the SPEC §7.4 periodic refresh loop (T2): a fair scheduler
// (spec C4) with per-host queues of due suites, refreshed every tick and
// served by checkWorkers workers, one check per host at a time, hosts
// taking turns. It returns when ctx is cancelled, after its workers.
// Before anything else it reschedules failing adoptions (spec C7 restart
// rule). Refresh = 0 disables periodic refresh.
//
// Latency bound (spec C4, simulation-tested): a due suite S is checked
// within one tick interval + (n + 1) × ⌈H/4⌉ × checkDeadline, where H is
// the number of hosts with queued suites and n the suites ahead of S in
// its host's queue (healthy ones only when S is healthy).
func (c *Checker) Run(ctx context.Context) {
	c.rescheduleFailingAdoptions(ctx)
	if c.refresh <= 0 {
		return
	}
	interval := c.refresh / 4
	if interval < minFastTickInterval {
		interval = minFastTickInterval
	}
	work := make(chan schedItem)
	done := make(chan schedItem, checkWorkers)
	var wg sync.WaitGroup
	for i := 0; i < checkWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.checkWorker(ctx, work, done)
		}()
	}
	defer wg.Wait()
	t := time.NewTicker(interval)
	defer t.Stop()
	q := newSchedQueues()
	idle := checkWorkers
	for {
		for idle > 0 {
			item, ok := q.pop()
			if !ok {
				break
			}
			select {
			case work <- item:
				idle--
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.safeTick(ctx, q)
		case item := <-done:
			q.done(item)
			idle++
		}
	}
}

// checkWorker runs queued checks until ctx ends. The done channel is
// buffered for every worker, so a send never blocks.
func (c *Checker) checkWorker(ctx context.Context, work <-chan schedItem, done chan<- schedItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-work:
			c.checkRecovered(ctx, item.ref)
			done <- item
		}
	}
}

// safeTick keeps a panic in a tick (ListSuites, gauges) from ending the
// scheduler; the next tick tries again.
func (c *Checker) safeTick(ctx context.Context, q *schedQueues) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", SuiteRef{}, r)
		}
	}()
	c.tick(ctx, q)
}

// tick refreshes the queues with every due suite and updates the gauges.
func (c *Checker) tick(ctx context.Context, q *schedQueues) {
	suites, err := c.cache.ListSuites(ctx)
	if err != nil {
		c.logger.Warn("freshness: list suites failed", "err", err)
		return
	}
	nowT := c.now()
	// SPEC §7.4 observability: an adopted suite stale past
	// staleSuiteFactor*refresh is the freeze signature.
	staleBefore := nowT.Add(-staleSuiteFactor * c.refresh).Unix()
	adoptedStale, unadoptedStale := staleSuiteCounts(suites, staleBefore, nowT.Unix())
	freshnessStaleSuites.Set(float64(adoptedStale), "true")
	freshnessStaleSuites.Set(float64(unadoptedStale), "false")
	if adoptedStale > 0 {
		c.logger.Warn("freshness: adopted suites stale beyond threshold",
			"adopted_stale", adoptedStale,
			"stale_factor", staleSuiteFactor,
			"refresh", c.refresh.String(),
		)
	}
	adoptionFailingSuites.Set(float64(failingAdoptionCount(suites)))
	var due []cache.SuiteFreshness
	for _, s := range suites {
		if c.suiteDue(s, nowT) {
			due = append(due, s)
		}
	}
	// Spec C4: FIFO by queue time. Suites that become due on the same
	// tick are queued in key order, so a suite's position is fixed.
	sort.Slice(due, func(i, j int) bool {
		return suiteKey(due[i].CanonicalScheme, due[i].CanonicalHost, due[i].SuitePath) <
			suiteKey(due[j].CanonicalScheme, due[j].CanonicalHost, due[j].SuitePath)
	})
	for _, s := range due {
		q.enqueue(s)
	}
}

// checkRecovered keeps one suite's panic from ending a worker. Check
// already converts a checkLocked panic into a failed check; this catches
// anything outside it.
func (c *Checker) checkRecovered(ctx context.Context, ref SuiteRef) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler", ref, r)
		}
	}()
	c.Check(ctx, ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath)
}
```

Set the file's imports to `context`, `sort`, `sync`, `time` and the `cache` package.

In `internal/freshness/metrics.go`, add to the `var (...)` block:

```go
	// adoptionFailingSuites is the number of suites whose latest adoption
	// attempt failed (refusal states excluded, spec C7), refreshed every
	// scheduler tick.
	adoptionFailingSuites = metrics.NewGauge(
		"acu_adoption_failing_suites",
		"Suites whose latest adoption attempt failed, refusal states excluded; refreshed every scheduler tick (spec C7).",
	)
```

and add `acu_adoption_failing_suites (spec C7, unlabelled gauge)` to the AIDEV-NOTE listing metrics added by the flat-repository work.

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/freshness/ ./internal/simtest/`
Expected: PASS, including `TestSimSmoke_30DaysPeriodicRefresh` (exactly 360) and `TestSimClockSteps_KeepChecking`.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/freshness/ internal/simtest/
git add internal/freshness/ internal/simtest/
git commit -m "Replace the sequential T2 loop with a fair per-host scheduler"
```

---
### Task 12: Scheduler and capacity simulations (phase B DoD)

**Files:**
- Create: `internal/freshness/sim_sched_test.go`

**Interfaces:**
- Consumes: Task 11 (`simtest.Upstream` and behaviours, `checkWorkers`), Task 10 (`checkDeadline`), Task 9 (`seedStaleAnchorAt`, `AdoptionConfig.MaxDuration`), Task 4 (`datedRelease`); existing `simtest.OpenCache`, `simtest.DiscardLogger`, `fakePackagesStanzas`, `newFakeFetcher`, `passThroughVerifier`, `wallclock.SetOffsetForTest`, `testHex64`.
- Produces: test helpers `startSched(t, cfg Config) (*Checker, func())` and `seedSimSuite(t, c *cache.Cache, host, suite string, lastAt, failCount int64)` (used by Task 13).

- [ ] **Step 1: Write the simulations**

`internal/freshness/sim_sched_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// startSched runs a checker's scheduler until the returned stop is called
// (also registered as a cleanup; calling it twice is safe).
func startSched(t *testing.T, cfg Config) (*Checker, func()) {
	t.Helper()
	chk, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { chk.Run(ctx); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			chk.WaitForAdoptions()
		})
	}
	t.Cleanup(stop)
	return chk, stop
}

// seedSimSuite caches an anchor and a suite row last checked and last
// succeeded at lastAt, with the given unknown-check streak.
func seedSimSuite(t *testing.T, c *cache.Cache, host, suite string, lastAt, failCount int64) {
	t.Helper()
	ctx := context.Background()
	bh := testHex64
	if err := c.PutBlob(ctx, bh, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: suite + "/InRelease",
		BlobHash: &bh, UpstreamURL: "http://" + host + suite + "/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite,
		LastCheckAt: &lastAt, LastSuccessAt: &lastAt, CheckFailCount: failCount}); err != nil {
		t.Fatal(err)
	}
}

// Spec C4 DoD (1): H = 8 hosts, several stalling; a healthy suite S with
// n = 3 healthy (slow) suites ahead of it is checked within
// one tick + (n + 1) × ⌈H/4⌉ × D.
func TestSimScheduler_LatencyBoundEightHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		t0 := time.Now()
		stale := t0.Add(-2 * time.Hour).Unix()
		for _, s := range []string{"/a1/dists/s", "/a2/dists/s", "/a3/dists/s"} {
			seedSimSuite(t, c, "x", s, stale, 0)
			up.Set("http://x"+s+"/InRelease", simtest.After(checkDeadline-time.Second, simtest.NotModified()))
		}
		const sPath = "/zz/dists/s"
		seedSimSuite(t, c, "x", sPath, stale, 0)
		up.Set("http://x"+sPath+"/InRelease", simtest.NotModified())
		for h := 1; h <= 7; h++ {
			for k := 0; k < 2; k++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/s", k)
				seedSimSuite(t, c, host, suite, stale, 0)
				up.Set("http://"+host+suite+"/InRelease", simtest.Stall())
			}
		}
		const refresh = time.Hour
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh, Logger: simtest.DiscardLogger()})
		time.Sleep(45*time.Minute + 7*time.Second)
		stop()

		sCalls := up.Calls("http://x" + sPath + "/InRelease")
		if len(sCalls) == 0 {
			t.Fatal("S was never checked")
		}
		bound := refresh/4 + 4*2*checkDeadline // tick + (3+1) × ⌈8/4⌉ × D = 31 min
		if got := sCalls[0].Sub(t0); got > bound {
			t.Fatalf("S first checked after %v, bound %v", got, bound)
		}
		if a3 := up.Calls("http://x/a3/dists/s/InRelease"); len(a3) == 0 || !sCalls[0].After(a3[0]) {
			t.Fatal("S overtook a healthy suite queued ahead of it")
		}
	})
}

// Spec C4 DoD (2): 500 stalling suites and one healthy suite on one host.
// After each stalling suite has failed once, the healthy one is checked
// within one tick + ⌈1/4⌉ × D of becoming due — and again on the first
// tick after a restart, because the class and streak are persisted.
// Mutations that must fail this test: ordering never-checked suites
// first, keeping the streak in memory, or keying the order on
// last_check_at.
func TestSimScheduler_FiveHundredStallingOneHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		stale := time.Now().Add(-2 * time.Hour).Unix()
		for i := 0; i < 500; i++ {
			suite := fmt.Sprintf("/s%03d/dists/x", i)
			seedSimSuite(t, c, "a", suite, stale, 0)
			up.Set("http://a"+suite+"/InRelease", simtest.Stall())
		}
		const sPath = "/zzz/dists/x"
		sURL := "http://a" + sPath + "/InRelease"
		seedSimSuite(t, c, "a", sPath, stale, 0)
		up.Set(sURL, simtest.NotModified())
		const refresh = time.Hour
		tick := refresh / 4
		cfg := Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh, Logger: simtest.DiscardLogger()}
		_, stop := startSched(t, cfg)

		// Phase 1: every stalling suite fails once while still in the
		// healthy class (spec C4: a first failure is paid once):
		// 500 × 2 min ≈ 16 h 40 min.
		time.Sleep(17*time.Hour + 37*time.Minute)
		all, err := c.ListSuites(ctx)
		if err != nil {
			t.Fatal(err)
		}
		failing := 0
		for _, s := range all {
			if s.CheckFailCount > 0 {
				failing++
			}
		}
		if failing != 500 {
			t.Fatalf("after phase 1, %d suites are failing, want 500", failing)
		}

		// Phase 2: S is checked within one tick + D of every due time.
		before := len(up.Calls(sURL))
		if before == 0 {
			t.Fatal("S never checked in phase 1")
		}
		time.Sleep(3*time.Hour + 11*time.Minute)
		calls := up.Calls(sURL)[before-1:]
		if len(calls) < 3 {
			t.Fatalf("S checked %d times in phase 2, want at least 3", len(calls)-1)
		}
		for i := 1; i < len(calls); i++ {
			if gap := calls[i].Sub(calls[i-1]); gap > refresh+tick+checkDeadline {
				t.Fatalf("S waited %v between checks, bound %v", gap, refresh+tick+checkDeadline)
			}
		}

		// Phase 3: an hour of downtime, then a fresh checker on the same
		// cache. S (healthy) goes before the 500 (failing) on the first tick.
		stop()
		time.Sleep(time.Hour + 3*time.Minute)
		restart := time.Now()
		_, stop2 := startSched(t, cfg)
		time.Sleep(tick + checkDeadline + time.Minute)
		stop2()
		after := up.Calls(sURL)
		last := after[len(after)-1]
		if last.Before(restart) || last.Sub(restart) > tick+checkDeadline {
			t.Fatalf("after the restart S was checked at %v past the restart, bound %v", last.Sub(restart), tick+checkDeadline)
		}
	})
}

// simMembers is an AdoptionFetcher serving canned members, except that
// every fetch for a stalled host blocks until its context ends. It
// tracks how many stalled fetches are in flight.
type simMembers struct {
	*fakeFetcher
	mu       sync.Mutex
	stalled  map[string]bool
	inFlight int
	maxStall int
}

func (m *simMembers) stall(host string) { m.mu.Lock(); m.stalled[host] = true; m.mu.Unlock() }

func (m *simMembers) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	m.mu.Lock()
	stalled := m.stalled[target.CanonicalHost]
	if stalled {
		m.inFlight++
		m.maxStall = max(m.maxStall, m.inFlight)
	}
	m.mu.Unlock()
	if !stalled {
		return m.fakeFetcher.Fetch(ctx, target, dst)
	}
	defer func() { m.mu.Lock(); m.inFlight--; m.mu.Unlock() }()
	<-ctx.Done()
	return nil, ctx.Err()
}

// Spec C4 DoD capacity simulation: two slow-drip first adoptions, twenty
// normal suites with signed snapshots, one degraded suite. Every normal
// suite adopts within max_duration + cooldown (here: well inside it,
// because the reserved slot serves signed suites while a first adoption
// holds the general one); the degraded suite keeps being checked; at
// most one first adoption runs at a time; goroutines stay bounded.
func TestSimCapacity_SlowDripAndDegraded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members := &simMembers{fakeFetcher: newFakeFetcher(), stalled: map[string]bool{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: 30 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		rel := func(label string) []byte {
			return datedRelease(label, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
		}
		up := simtest.NewUpstream()
		var normal []string
		for i := 0; i < 20; i++ {
			host := fmt.Sprintf("n%02d", i)
			members.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
			if err := ad.Run(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: rigSuite}, rel("v1-"+host), "", ""); err != nil {
				t.Fatal(err)
			}
			up.Set("http://"+host+rigSuite+"/InRelease", simtest.Serve(rel("v2-"+host)))
			normal = append(normal, host)
		}
		for _, host := range []string{"drip1", "drip2"} {
			seedStaleAnchorAt(t, c, host, rigSuite)
			members.put("http://"+host+rigSuite+"/main/binary-amd64/Packages", pkgs)
			members.stall(host)
			up.Set("http://"+host+rigSuite+"/InRelease", simtest.Serve(rel("drip-"+host)))
		}
		seedStaleAnchorAt(t, c, "deg", rigSuite)
		up.Set("http://deg"+rigSuite+"/InRelease", simtest.Stall())

		base := runtime.NumGoroutine()
		maxG := base
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
			Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger()})
		for i := 0; i < 15; i++ {
			time.Sleep(time.Minute)
			maxG = max(maxG, runtime.NumGoroutine())
		}
		for _, host := range normal {
			sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite)
			if err != nil || sf.CurrentSnapshotID == nil {
				t.Fatalf("%s: no current snapshot (err %v)", host, err)
			}
			snap, err := c.GetSuiteSnapshot(ctx, *sf.CurrentSnapshotID)
			if err != nil || snap.SingleAnchorHash == nil || *snap.SingleAnchorHash != hashOf(rel("v2-"+host)) {
				t.Fatalf("%s did not adopt v2 within 15 min while first adoptions held a slot", host)
			}
		}
		for i := 0; i < 105; i++ {
			time.Sleep(time.Minute)
			maxG = max(maxG, runtime.NumGoroutine())
		}
		stop()
		if n := len(up.Calls("http://deg" + rigSuite + "/InRelease")); n < 4 {
			t.Fatalf("degraded suite checked %d times in 2 h, want its checks to continue (>= 4)", n)
		}
		members.mu.Lock()
		maxStall := members.maxStall
		members.mu.Unlock()
		if maxStall != 1 {
			t.Fatalf("%d first adoptions ran at once, want 1 (the last free slot is reserved for signed suites)", maxStall)
		}
		for _, host := range []string{"drip1", "drip2"} {
			sf, err := c.GetSuiteFreshness(ctx, "http", host, rigSuite)
			if err != nil || strOf(sf.AdoptionFailReason) != "timed_out" {
				t.Fatalf("%s record reason = %q (err %v), want timed_out", host, strOf(sf.AdoptionFailReason), err)
			}
		}
		// Workers + Run + two adoptions + slack for transient goroutines.
		if limit := base + checkWorkers + 1 + 2 + 8; maxG > limit {
			t.Fatalf("goroutines peaked at %d, limit %d", maxG, limit)
		}
	})
}

// Phase A carry-forward: the burst after a forward or backward clock
// step (every suite due at once) stays within the pool's bounds and
// every suite is still checked.
func TestSimScheduler_PacingAfterClockSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		now := time.Now().Unix()
		var urls []string
		for h := 0; h < 10; h++ {
			for k := 0; k < 4; k++ {
				host, suite := fmt.Sprintf("h%d", h), fmt.Sprintf("/s%d/dists/x", k)
				seedSimSuite(t, c, host, suite, now, 0)
				url := "http://" + host + suite + "/InRelease"
				up.Set(url, simtest.After(30*time.Second, simtest.NotModified()))
				urls = append(urls, url)
			}
		}
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute, Logger: simtest.DiscardLogger()})
		defer stop()
		phase := func(name string, offset time.Duration) {
			restore := wallclock.SetOffsetForTest(offset)
			defer restore()
			up.ResetMax()
			counts := make([]int, len(urls))
			for i, u := range urls {
				counts[i] = len(up.Calls(u))
			}
			time.Sleep(2*time.Hour + 7*time.Minute)
			if total, perHost := up.MaxInFlight(); total > checkWorkers || perHost > 1 {
				t.Errorf("%s: %d checks in flight overall, %d on one host; want <= %d and <= 1", name, total, perHost, checkWorkers)
			}
			for i, u := range urls {
				if len(up.Calls(u)) == counts[i] {
					t.Errorf("%s: %s was not checked", name, u)
				}
			}
		}
		phase("baseline", 0)
		phase("forward +30d", 30*24*time.Hour)
		phase("back to 0 (-30d step)", 0)
		phase("backward -2h", -2*time.Hour)
	})
}
```

- [ ] **Step 2: Run them**

Run: `go test -race -run 'TestSimScheduler|TestSimCapacity' -v ./internal/freshness/ 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL)'`
Expected: PASS, each simulation under 5 s. If one is slower, coarsen its intervals (never drop assertions) and note the change in the commit message. If the 500-suite simulation fails on the healthy suite's wait, the scheduler is wrong, not the bound: fix the scheduler (classes, FIFO, persisted streak).

- [ ] **Step 3: Mutation check (do not commit)**

Temporarily change `classOf` to return `classHealthy` for every suite (as if the streak lived in memory), run `go test -run TestSimScheduler_FiveHundredStallingOneHealthy ./internal/freshness/`, and confirm it FAILS; revert. Record the observed failure line in the commit message.

- [ ] **Step 4: Commit**

```bash
gofmt -l internal/freshness/sim_sched_test.go
git add internal/freshness/sim_sched_test.go
git commit -m "Prove the scheduler's latency bound and the capacity limits by simulation"
```

---

### Task 13: Retry and `Date` simulations (phase B DoD)

**Files:**
- Create: `internal/freshness/sim_retry_test.go`

**Interfaces:**
- Consumes: Task 12 (`startSched`), Task 11 (`simtest.Upstream`, `simtest.Serve`), Task 9 (`seedStaleAnchorAt`), Task 6 (`rigVerifier`, `rigAnchorURL`, `rigPkgsURL`, `rigHost`, `rigSuite`, `strOf`, `i64Of`), Task 4 (`datedRelease`); `wallclock`.
- Produces: nothing for later tasks.

- [ ] **Step 1: Write the simulations**

`internal/freshness/sim_retry_test.go`:

```go
package freshness

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// retrySim runs the real scheduler, checker, adopter and cache for suite
// /r/dists/x on host h inside a synctest bubble.
type retrySim struct {
	c    *cache.Cache
	up   *simtest.Upstream
	ver  *rigVerifier
	pkgs []byte
	stop func()
}

func startRetrySim(t *testing.T, refresh time.Duration) *retrySim {
	t.Helper()
	c := simtest.OpenCache(t)
	seedStaleAnchorAt(t, c, rigHost, rigSuite)
	s := &retrySim{c: c, up: simtest.NewUpstream(),
		ver:  &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		pkgs: fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})}
	members := newFakeFetcher()
	members.put(rigPkgsURL, s.pkgs)
	ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: s.ver, HostLimiter: hostsem.New(8), MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, s.stop = startSched(t, Config{Cache: c, Fetcher: s.up, HostLimiter: hostsem.New(8), Refresh: refresh,
		Cooldown: time.Minute, Adopter: ad, Logger: simtest.DiscardLogger(), rand: func() float64 { return 0.5 }})
	return s
}

func (s *retrySim) release(label string, date int64) []byte {
	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": s.pkgs})
}

func (s *retrySim) serve(body []byte) { s.up.Set(rigAnchorURL, simtest.Serve(body)) }

func (s *retrySim) row(t *testing.T) cache.SuiteFreshness {
	t.Helper()
	r, err := s.c.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
	if err != nil {
		t.Fatal(err)
	}
	return *r
}

// current returns the hash of the current snapshot's anchor, or "".
func (s *retrySim) current(t *testing.T) string {
	t.Helper()
	r := s.row(t)
	if r.CurrentSnapshotID == nil {
		return ""
	}
	snap, err := s.c.GetSuiteSnapshot(context.Background(), *r.CurrentSnapshotID)
	if err != nil || snap.SingleAnchorHash == nil {
		t.Fatalf("current snapshot unreadable: %v", err)
	}
	return *snap.SingleAnchorHash
}

// PR #8 limitation #1 reproduced under the real scheduler: an untrusted
// InRelease costs about 30 attempts a day (main: about 1,440 at the
// default 60 s cooldown), while checks keep running every refresh.
func TestSimLim1_DeterministicFailureAttemptsPerDay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startRetrySim(t, 15*time.Minute)
		bad := s.release("untrusted", 0)
		s.ver.reject(hashOf(bad))
		s.serve(bad)
		time.Sleep(24*time.Hour + 7*time.Minute)
		s.stop()
		if a := s.ver.calls(); a < 18 || a > 30 {
			t.Fatalf("attempts in a day = %d, want 18..30", a)
		}
		if n := len(s.up.Calls(rigAnchorURL)); n < 90 {
			t.Fatalf("checks in a day = %d, want >= 90 (liveness)", n)
		}
	})
}

// Spec C7/C5 30-day simulation: an untrusted body (bounded attempts), new
// content (adopted within a refresh interval), an upstream rollback to
// older-dated content (never adopted, bounded refusals, record cleared
// when upstream recovers), and backward and forward clock steps (new
// content still adopted, checks continue).
func TestSim30Days_RetryDateRulesAndClockSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const refresh = 2 * time.Hour
		const day = 24 * time.Hour
		window := refresh + refresh/4 + 7*time.Minute // one refresh + one tick, off the tick grid
		s := startRetrySim(t, refresh)
		t0 := time.Now()

		bad := s.release("untrusted", t0.Unix())
		s.ver.reject(hashOf(bad))
		s.serve(bad)
		time.Sleep(10*day + 7*time.Minute)
		if a := s.ver.calls(); a > 10*30 {
			t.Fatalf("untrusted body: %d attempts in 10 days, want <= 300", a)
		}
		if s.current(t) != "" {
			t.Fatal("untrusted content was adopted")
		}

		v2 := s.release("v2", time.Now().Unix())
		s.serve(v2)
		time.Sleep(window)
		if s.current(t) != hashOf(v2) {
			t.Fatal("new content not adopted within one refresh interval")
		}

		older := s.release("v1-older", t0.Add(day).Unix())
		s.serve(older)
		before := s.ver.calls()
		time.Sleep(8*day + 11*time.Minute)
		if s.current(t) != hashOf(v2) {
			t.Fatal("the suite moved back to older-dated content")
		}
		if a := s.ver.calls() - before; a < 3 || a > 8*30 {
			t.Fatalf("older content cost %d verifications in 8 days, want 3..240", a)
		}
		if r := strOf(s.row(t).AdoptionFailReason); r != cache.ReasonUpstreamOlder {
			t.Fatalf("record reason = %q, want upstream_older", r)
		}
		s.serve(v2)
		time.Sleep(window)
		if r := s.row(t); r.AdoptionFailCount != 0 {
			t.Fatalf("record not cleared once upstream served the current content again: %+v", r)
		}

		restore := wallclock.SetOffsetForTest(-2 * time.Hour)
		time.Sleep(4*day + 13*time.Minute)
		v3 := s.release("v3", wallclock.Now().Unix())
		s.serve(v3)
		time.Sleep(window)
		if s.current(t) != hashOf(v3) {
			t.Fatal("after a backward clock step, new content was not adopted")
		}
		restore()

		restore = wallclock.SetOffsetForTest(30 * day)
		time.Sleep(4*day + 17*time.Minute)
		v4 := s.release("v4", wallclock.Now().Unix())
		s.serve(v4)
		time.Sleep(window)
		if s.current(t) != hashOf(v4) {
			t.Fatal("after a forward clock step, new content was not adopted")
		}
		restore()
		s.stop()
		if n := len(s.up.Calls(rigAnchorURL)); n < 28*10 {
			t.Fatalf("only %d checks over ~28 days, want >= 280 (liveness)", n)
		}
	})
}

// Spec C5: a flapping mirror alternating old and new content never moves
// the suite backwards.
func TestSimFlappingMirror_NeverFlipsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startRetrySim(t, 15*time.Minute)
		now := time.Now()
		v1 := s.release("v1", now.Add(-2*time.Hour).Unix())
		v2 := s.release("v2", now.Add(-time.Hour).Unix())
		var n atomic.Int64
		s.up.Set(rigAnchorURL, func(ctx context.Context, etag string) (*fetch.ConditionalResult, error) {
			if n.Add(1)%2 == 1 {
				return simtest.Serve(v1)(ctx, etag)
			}
			return simtest.Serve(v2)(ctx, etag)
		})
		seenV2 := false
		for i := 0; i < 3*24*6; i++ { // sample every 10 min for 3 days
			time.Sleep(10 * time.Minute)
			switch s.current(t) {
			case hashOf(v2):
				seenV2 = true
			case hashOf(v1):
				if seenV2 {
					t.Fatalf("flipped back to v1 after %v", time.Duration(i+1)*10*time.Minute)
				}
			}
		}
		s.stop()
		if !seenV2 || s.current(t) != hashOf(v2) {
			t.Fatal("never settled on v2")
		}
	})
}

// Spec C7 "Restart": failing suites retry spread over the refresh
// interval after a restart, not all on one tick.
func TestSimRestartSpread(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		up := simtest.NewUpstream()
		ver := &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)}
		pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
		members := newFakeFetcher()
		now := time.Now().Unix()
		var urls []string
		for i := 0; i < 50; i++ {
			host := fmt.Sprintf("h%02d", i)
			seedSimSuite(t, c, host, rigSuite, now, 0) // fresh: only the retry makes it due
			bad := datedRelease("bad-"+host, 0, map[string][]byte{"main/binary-amd64/Packages": pkgs})
			ver.reject(hashOf(bad))
			url := "http://" + host + rigSuite + "/InRelease"
			up.Set(url, simtest.Serve(bad))
			urls = append(urls, url)
			if err := c.RecordAdoptionFailure(ctx, "http", host, rigSuite, cache.AdoptionFailure{
				Hash: hashOf(bad), Reason: "gpg_failed", Count: 5, FirstAt: now - 3600, LastAt: now - 60,
				NextRetryAt: now + 50*60}); err != nil {
				t.Fatal(err)
			}
		}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: ver, HostLimiter: hostsem.New(8), MaxConcurrent: 2})
		if err != nil {
			t.Fatal(err)
		}
		rnd := mrand.New(mrand.NewPCG(1, 2))
		var mu sync.Mutex
		const refresh = time.Hour
		start := time.Now()
		_, stop := startSched(t, Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: refresh,
			Adopter: ad, Logger: simtest.DiscardLogger(),
			rand: func() float64 { mu.Lock(); defer mu.Unlock(); return rnd.Float64() }})
		time.Sleep(refresh + 7*time.Minute)
		stop()
		buckets := map[int64]int{}
		retried := 0
		for _, u := range urls {
			calls := up.Calls(u)
			if len(calls) == 0 {
				continue
			}
			retried++
			buckets[int64(calls[0].Sub(start)/(refresh/4))]++
		}
		if retried != 50 {
			t.Fatalf("%d of 50 failing suites retried within the refresh interval after the restart", retried)
		}
		for b, n := range buckets {
			if n > 25 {
				t.Fatalf("%d retries landed in tick window %d, want them spread (<= 25 per window)", n, b)
			}
		}
	})
}
```

- [ ] **Step 2: Run them**

Run: `go test -race -run 'TestSimLim1|TestSim30Days|TestSimFlapping|TestSimRestartSpread' -v ./internal/freshness/ 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL)'`
Expected: PASS, each under 5 s. Coarsen intervals (not assertions) if slower.

- [ ] **Step 3: Mutation check (do not commit)**

Temporarily make `retryGateFor` always return `retryGate{}` and confirm `TestSimLim1_DeterministicFailureAttemptsPerDay` FAILS (hundreds of attempts); then temporarily remove the `rescheduleFailingAdoptions` call from `Run` and confirm `TestSimRestartSpread` FAILS. Revert both; record the failure lines in the commit message.

- [ ] **Step 4: Commit**

```bash
gofmt -l internal/freshness/sim_retry_test.go
git add internal/freshness/sim_retry_test.go
git commit -m "Prove retry discipline, Date rules and restart spread by simulation"
```

---

### Task 14: Admin visibility: the record, the lag that fires, the failing count

**Files:**
- Modify: `internal/admin/status.go` (`suiteEntry`, `buildSuiteEntries`, `laggingAnnotation`, new `adoptionStatus`, the `suites` panel state, `buildStatusModel`), `internal/admin/status_template.go`, `internal/admin/admin_test.go` (`TestLaggingAnnotation` and the lagging HTML test), `internal/admin/testdata/json_contract*_golden.json` (regenerated)
- Create: `internal/admin/suites_status_test.go`

**Interfaces:**
- Consumes: Task 1 (`SuiteFreshness` record fields, `SuiteWithAdoption.CurrentAnchorForm`, `cache.IsRefusalReason`); existing `durationOf`, `wallclock`.
- Produces: `suiteEntry` JSON fields `anchor_form`, `last_requested_unixtime`, `check_fail_count`, `adoption_fail_reason`, `adoption_fail_count`, `adoption_fail_first_unixtime`, `adoption_next_retry_unixtime`, `adoption_refused`; HTML-only `AdoptionFailing`, `AdoptionStatus`, `LagSeconds`; `buildSuiteEntries(rows []cache.SuiteWithAdoption, now int64) []suiteEntry`; `laggingAnnotation(seenAt *int64, now int64) (string, int64)`; `adoptionStatus(s cache.SuiteFreshness, refused bool, now int64) string`.

- [ ] **Step 1: Write the failing tests**

`internal/admin/suites_status_test.go`:

```go
package admin

import (
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func p64(v int64) *int64    { return &v }
func pstr(s string) *string { return &s }

// Spec C7 "Visibility": failing and refused records render as text; the
// lag is measured from the first sighting against now.
func TestBuildSuiteEntries_RecordAndLag(t *testing.T) {
	const now = int64(1_800_000_000)
	failing := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{
		CanonicalHost: "h", SuitePath: "/a", CheckFailCount: 2, LastRequestedAt: p64(now - 60),
		AdoptionFailReason: pstr("untrusted_signer"), AdoptionFailCount: 4,
		AdoptionFailFirstAt: p64(now - (3*86400 + 2*3600)), AdoptionNextRetryAt: p64(now + 41*60),
		InReleaseChangeSeenAt: p64(now - 5400)}, CurrentAnchorForm: "inline"}
	refused := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{
		CanonicalHost: "h", SuitePath: "/b", AdoptionFailReason: pstr(cache.ReasonUpstreamOlder),
		AdoptionFailCount: 1, AdoptionFailFirstAt: p64(now - 60), AdoptionNextRetryAt: p64(now - 1)}}
	clean := cache.SuiteWithAdoption{SuiteFreshness: cache.SuiteFreshness{CanonicalHost: "h", SuitePath: "/c"}}

	got := buildSuiteEntries([]cache.SuiteWithAdoption{failing, refused, clean}, now)
	f, r, c := got[0], got[1], got[2]
	if !f.AdoptionFailing || f.AdoptionRefused || f.AdoptionStatus != "adoption failing 3d 2h · untrusted_signer · retry in 41m" {
		t.Errorf("failing entry = %+v", f)
	}
	if f.AnchorForm != "inline" || f.CheckFailCount != 2 || f.LastRequestedUnixTime == nil || f.AdoptionFailCount != 4 {
		t.Errorf("failing entry fields = %+v", f)
	}
	if f.Lagging != "(lagging 1h 30m)" || f.LagSeconds != 5400 {
		t.Errorf("lag = %q / %d, want (lagging 1h 30m) / 5400", f.Lagging, f.LagSeconds)
	}
	if r.AdoptionFailing || !r.AdoptionRefused || r.AdoptionStatus != "refused: upstream_older · retry due" {
		t.Errorf("refused entry = %+v", r)
	}
	if c.AdoptionFailing || c.AdoptionRefused || c.AdoptionStatus != "" || c.Lagging != "" {
		t.Errorf("clean entry = %+v", c)
	}
}

func TestLaggingAnnotation_FromFirstSighting(t *testing.T) {
	const now = int64(100_000)
	cases := []struct {
		seen *int64
		want string
		lag  int64
	}{
		{nil, "", 0},
		{p64(now), "(lagging 0m)", 0},
		{p64(now - 1800), "(lagging 30m)", 1800},
		{p64(now - 90000), "(lagging 1d 1h)", 90000},
		{p64(now + 60), "", 0}, // a future stamp (clock step) shows no lag
	}
	for _, tc := range cases {
		got, lag := laggingAnnotation(tc.seen, now)
		if got != tc.want || lag != tc.lag {
			t.Errorf("laggingAnnotation(%v) = %q, %d; want %q, %d", tc.seen, got, lag, tc.want, tc.lag)
		}
	}
}
```

Replace `TestLaggingAnnotation` in `internal/admin/admin_test.go` with a pointer comment (`// Lag rendering is covered by TestLaggingAnnotation_FromFirstSighting in suites_status_test.go.`) and delete the old function. In the lagging HTML test just above it, set the fixture's `InReleaseChangeSeenAt` to `wallclock.Now().Unix() - 5400` (import `wallclock`) so it still expects `(lagging 1h 30m)`; any `LastSuccessAt`-relative setup that existed only to create the lag can stay.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/admin/ -run 'TestBuildSuiteEntries_RecordAndLag|TestLaggingAnnotation_FromFirstSighting'`
Expected: compile failure naming `AdoptionFailing`, `AdoptionStatus`, `LagSeconds`, and `buildSuiteEntries`/`laggingAnnotation` argument mismatches.

- [ ] **Step 3: Implement**

In `internal/admin/status.go`, extend `suiteEntry` (after `InReleaseChangeSeenAtUnixTime`; keep `Lagging`):

```go
	// Spec C7 "Visibility" (phase B). AnchorForm is "" for a suite with no
	// current snapshot. The adoption_fail_* fields mirror the failure
	// record; adoption_refused marks a refusal state (not a failure).
	AnchorForm                string  `json:"anchor_form"`
	LastRequestedUnixTime     *int64  `json:"last_requested_unixtime"`
	CheckFailCount            int64   `json:"check_fail_count"`
	AdoptionFailReason        *string `json:"adoption_fail_reason"`
	AdoptionFailCount         int64   `json:"adoption_fail_count"`
	AdoptionFailFirstUnixTime *int64  `json:"adoption_fail_first_unixtime"`
	AdoptionNextRetryUnixTime *int64  `json:"adoption_next_retry_unixtime"`
	AdoptionRefused           bool    `json:"adoption_refused"`

	// HTML-only: AdoptionFailing marks a failing (not refused) record;
	// AdoptionStatus is its one-line summary; LagSeconds feeds the panel
	// state. JSON consumers compute these from the fields above.
	AdoptionFailing bool   `json:"-"`
	AdoptionStatus  string `json:"-"`
	LagSeconds      int64  `json:"-"`
```

Replace `buildSuiteEntries` and `laggingAnnotation`:

```go
func buildSuiteEntries(rows []cache.SuiteWithAdoption, now int64) []suiteEntry {
	out := make([]suiteEntry, 0, len(rows))
	for _, r := range rows {
		e := suiteEntry{
			Host:                             r.CanonicalHost,
			SuitePath:                        r.SuitePath,
			LastCheckUnixTime:                r.LastCheckAt,
			LastSuccessUnixTime:              r.LastSuccessAt,
			CurrentSnapshotID:                r.CurrentSnapshotID,
			CurrentSnapshotAdoptedAtUnixTime: r.CurrentAdoptedAt,
			InReleaseChangeSeenAtUnixTime:    r.InReleaseChangeSeenAt,
			AnchorForm:                       r.CurrentAnchorForm,
			LastRequestedUnixTime:            r.LastRequestedAt,
			CheckFailCount:                   r.CheckFailCount,
			AdoptionFailReason:               r.AdoptionFailReason,
			AdoptionFailCount:                r.AdoptionFailCount,
			AdoptionFailFirstUnixTime:        r.AdoptionFailFirstAt,
			AdoptionNextRetryUnixTime:        r.AdoptionNextRetryAt,
		}
		e.Lagging, e.LagSeconds = laggingAnnotation(r.InReleaseChangeSeenAt, now)
		if r.AdoptionFailCount > 0 && r.AdoptionFailReason != nil {
			e.AdoptionRefused = cache.IsRefusalReason(*r.AdoptionFailReason)
			e.AdoptionFailing = !e.AdoptionRefused
			e.AdoptionStatus = adoptionStatus(r.SuiteFreshness, e.AdoptionRefused, now)
		}
		out = append(out, e)
	}
	return out
}

// laggingAnnotation renders "(lagging …)" while an upstream change first
// seen at seenAt has not been adopted, and returns the lag in seconds.
// The lag runs from the first sighting to now (spec C7): the old rule
// compared it with last_success_at, which every check bumps, so it could
// never fire. A stamp in the future (clock step) shows no lag.
func laggingAnnotation(seenAt *int64, now int64) (string, int64) {
	if seenAt == nil || *seenAt > now {
		return "", 0
	}
	lag := now - *seenAt
	return "(lagging " + durationOf(lag) + ")", lag
}

// adoptionStatus summarises the spec C7 record for the suites table:
// "adoption failing 3d 2h · untrusted_signer · retry in 41m", or
// "refused: upstream_older · retry due".
func adoptionStatus(s cache.SuiteFreshness, refused bool, now int64) string {
	retry := "retry due"
	if s.AdoptionNextRetryAt != nil && *s.AdoptionNextRetryAt > now {
		retry = "retry in " + durationOf(*s.AdoptionNextRetryAt-now)
	}
	if refused {
		return "refused: " + *s.AdoptionFailReason + " · " + retry
	}
	var since int64
	if s.AdoptionFailFirstAt != nil && *s.AdoptionFailFirstAt < now {
		since = now - *s.AdoptionFailFirstAt
	}
	return "adoption failing " + durationOf(since) + " · " + *s.AdoptionFailReason + " · " + retry
}
```

In `buildStatusModel`, call `buildSuiteEntries(suitesRaw.([]cache.SuiteWithAdoption), wallclock.Now().Unix())` (import `internal/wallclock`).

Replace the `case "suites":` body of the panel-state function with:

```go
	case "suites":
		if len(m.Suites) == 0 {
			return "stale"
		}
		var worstLag int64
		anyFailing := false
		for _, s := range m.Suites {
			worstLag = max(worstLag, s.LagSeconds)
			anyFailing = anyFailing || s.AdoptionFailing
		}
		switch {
		case worstLag > 24*3600:
			return "crit"
		case worstLag > 3600 || anyFailing:
			// An adoption normally completes within minutes; a lag beyond
			// the 1 h retry cap means at least one attempt failed.
			return "warn"
		}
		return "ok"
```

In `internal/admin/status_template.go`:
- In the Suites vital (`vital__sub`), count failing suites and print the count:

```
        {{$lagCount := 0}}{{$failCount := 0}}{{range .Suites}}{{if .Lagging}}{{$lagCount = (add1 $lagCount)}}{{end}}{{if .AdoptionFailing}}{{$failCount = (add1 $failCount)}}{{end}}{{end}}
        <span>{{if eq $lagCount 0}}<b>0 LAGGING</b>{{else}}<b class="accent-warn">{{$lagCount}} LAGGING</b>{{end}} &middot; {{if eq $failCount 0}}0 FAILING{{else}}<b class="accent-warn">{{$failCount}} FAILING</b>{{end}}</span>
```

- In the Suites panel eyebrow, after the lagging count: `{{$fc := 0}}{{range .Suites}}{{if .AdoptionFailing}}{{$fc = (add1 $fc)}}{{end}}{{end}}{{if gt $fc 0}}<span class="sep">&middot;</span><span class="count-warn">{{$fc}} FAILING</span>{{end}}`.
- Panel description: append " The Adoption column shows a failing or refused adoption and when it is retried."
- Table: add `<th>Adoption</th>` after `<th>InRelease changed</th>` and, in each row after the InRelease-changed cell, `<td data-label="Adoption" class="mono">{{.AdoptionStatus}}</td>`; change the row attribute to `<tr{{if or .Lagging .AdoptionFailing}} data-state="warn"{{end}}>`.

- [ ] **Step 4: Run the tests; regenerate the JSON contract**

Run: `go test ./internal/admin/ -run TestJSONContractPreserved -update.jsoncontract`, inspect `git diff internal/admin/testdata/` (only the new `suites[]` keys may appear), then `go test -race ./internal/admin/`.
Expected: PASS. Golden HTML fixtures that pin the suites table (if any fail) are updated for the new column, and named in the commit message.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/admin/
git add internal/admin/
git commit -m "Show adoption failures and refusals, and measure lag from the first sighting"
```

---

### Task 15: Documentation, phase A minors in touched files, stamps

**Files:**
- Modify: `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, `internal/freshness/adoption.go` (A-31), `internal/freshness/freshness.go` → new `internal/freshness/clockstep.go` (A-43), `internal/freshness/clockstep_test.go` (A-40), `docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`

**Interfaces:**
- Consumes: every earlier task.
- Produces: operator documentation of phase B behaviour.

- [ ] **Step 1: Configuration reference**

In `docs/configuration.md`'s `[adoption]` table, after `hot_prefetch_budget`, add:

```
| `max_duration` | duration | `"30m"` | Longest an adoption, repair or reconcile may run before it is abandoned and retried on the normal schedule. The final commit is never cut short. At least `"1m"`. |
```

Add a section after the `[adoption]` table:

```markdown
### How failed adoptions are retried

When an adoption fails (an untrusted or malformed signature, a member that will not download intact, a `Date` in the future, a timeout), the cache records the failing content and backs off: the same content is retried after 2, 4, 8, 16, 32 minutes and then hourly (each ±25 %). Until then each check asks upstream with the failing content's validators, so an unchanged upstream costs one small request and no download. **Any new content is tried at once.** A deterministic failure therefore costs about 30 attempts a day instead of one per `cooldown`.

Two outcomes are refusals, not failures, and are shown as "refused: …" on the status page:

- `upstream_older`: upstream serves a Release whose `Date` is older than the snapshot being served. apt would ignore it too; the cache keeps serving the newer snapshot.
- `rollback_refused`: upstream serves content identical to an older snapshot the cache already replaced (a flapping mirror). Only the last `gc.keep_displaced` snapshots are remembered for this.

A Release dated more than 10 seconds ahead of the cache's clock is a failure (apt rejects it as "not valid yet"); check the cache host's clock if this persists.

At start, every failing suite gets one retry spread over the next `periodic_refresh`, so installing a missing key or fixing pins takes effect without waiting an hour. Refusals keep their schedule.

### Capacity and fairness

`freshness.max_concurrent_adoptions` slots are shared by adoptions, repairs and reconciles, plus one slot per upstream host. Nothing waits for a slot: a suite that finds none is retried on the scheduler's next turn. When there are at least two slots, the last free one is reserved for suites that already serve a signed snapshot, so security updates are not delayed by first adoptions. `POST /reconcile` answers 409 when no slot is free.

Periodic checks run on four workers, at most one check per upstream host at a time, hosts taking turns. On each host, healthy suites go first, then never-checked suites, then suites whose checks keep failing; a failing suite waits 2 minutes, doubling to an hour, between checks. Each check is limited to 2 minutes. A host that stalls therefore delays healthy suites on other hosts by at most one check, and healthy suites on its own host by about one check per tick once its stalling suites are known to fail — also right after a restart.
```

In the `[freshness]` section's `max_concurrent_adoptions` row, append: "Shared by adoptions, repairs and reconciles; see *Capacity and fairness*." Where the docs describe the status page's suites table or "lagging", replace any wording that ties lag to `last_success_at` with: "Lagging counts from the first time the cache saw upstream's new metadata; a suite stays lagging until it adopts it." Next to the clock-step text, add (deferred minor A-42): "`acu_clock_steps_detected_total` counts detections — one per stored timestamp found in the future — not distinct clock steps."

- [ ] **Step 2: Log fields**

In `docs/log-fields.md`'s "Operational events" table, add rows:

```
| `adoption_deferred` | INFO | `canonical_host`, `suite_path`, `reason`, `refusal`, `fail_count`, `retry_in_seconds`, `next_retry_unixtime` | An adoption failed or was refused; the same content is retried at `next_retry_unixtime` (new content is tried at once). |
| `adoption_refused` | INFO | as `adoption_deferred` | The attempt ended in a refusal state (`upstream_older`, `rollback_refused`), not a failure. |
| `adoption_failing_persistently` | WARN | `canonical_host`, `suite_path`, `reason`, `fail_count`, `failing_since_unixtime` | The suite's adoption has failed 3, 6, 12, 24, … times in a row. |
| `adoption_failure_record_failed` | WARN | `canonical_host`, `suite_path`, `err` | The failure record could not be written; the next check may retry sooner than scheduled. |
| `adoption_retries_rescheduled` | INFO | `suites`, `spread_seconds` | At start, failing suites were given one retry spread over the refresh interval. |
| `adoption_retries_reschedule_failed` | WARN | `err` | The start-time retry spread could not be written; failing suites keep their schedule. |
```

Extend the `adoption_panicked` row's `where` set with `record`. Document the `freshness_check` result values `changed_deferred` (with field `reason`: why the content is held back) and `changed_queued` (no adoption slot was free; the suite is retried on its next turn), wherever that line's results are listed (add a short paragraph under "Operational events" if they are not listed in this file). Add the adoption outcome values `refused`, `release_date_in_future` and `timed_out` and the gauge `acu_adoption_failing_suites` wherever outcomes and metrics are inventoried in the docs.

- [ ] **Step 3: Admin UI spec and default config**

In `docs/admin-ui-spec.md`, add the new `suites[]` fields (`anchor_form`, `last_requested_unixtime`, `check_fail_count`, `adoption_fail_reason`, `adoption_fail_count`, `adoption_fail_first_unixtime`, `adoption_next_retry_unixtime`, `adoption_refused`) with one-line meanings next to the existing suites fields, the Adoption column, the FAILING count and the new lag rule. In `packaging/config/config.toml.default`'s `[adoption]` block, after `hot_prefetch_budget`, add:

```toml
# Longest an adoption, repair or reconcile may run before it is abandoned
# and retried on the normal schedule (the final commit is never cut short).
max_duration          = "30m"
```

- [ ] **Step 4: Phase A minors in files this phase touched**

- A-31: in `internal/freshness/adoption.go`, prefix the sentinel comment that encodes a load-bearing invariant (`adoption.go`, around the `ErrAdoptionGuardFailed` declaration) with `AIDEV-NOTE:`.
- A-43: move `clockStepTolerance`, `clockStepLogEvery`, `processStart` and `clockStep` (with their comments) from `freshness.go` into a new `internal/freshness/clockstep.go`; no behaviour change.
- A-40: in `TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals` (`clockstep_test.go`), assert that after the healing check `LastSuccessAt` equals the test's `now` (when the check succeeds) as well as `LastCheckAt`.
- Delete the entries A-28, A-31, A-36, A-39, A-40, A-41, A-42, A-43, A-44, A-45 and A-46 from `2026-09-28-flat-repositories-A-deferred.md` (fixed in this phase).

- [ ] **Step 5: Overview**

In `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`: set phase B's status to `done (commit <sha of this task's commit>)` (re-stamp after the final review's fix wave), delete the "**B:**" block under "Phase A carry-forwards" (all items are done: worker/run-loop recovery in Task 11, panicked adoptions in the ring in Task 6, pacing in Task 12, the clock-step rule for the retry stamp in Task 7, simulation hygiene in Tasks 11–12), and add any carry-forwards the phase B reviews produce for later phases.

- [ ] **Step 6: Full verification**

Run: `go vet ./... && golangci-lint run ./... && gofmt -l . && go test -race -count=1 ./...`
Expected: vet and lint clean; `gofmt -l .` lists only the five drift files; every package passes.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/freshness/
git add docs/ packaging/config/config.toml.default internal/freshness/
git commit -m "Document retry discipline, capacity and the fair scheduler; mark phase B done"
```
