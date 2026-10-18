# Flat repositories — Phase C: Flip consistency — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close PR #8 limitation #2 — a client that fetched a suite's old `InRelease` must not then receive index members from the new snapshot — with a two-phase flip barrier bounded below apt's 30 s timeout and one hold at a time process-wide, by-hash members served from retained (displaced) snapshots, and a `Last-Modified` that never lets `If-Modified-Since` hide a flip; each proven by virtual-clock simulations with an explicit apt client model.

**Architecture:** A new leaf package `internal/flipgate` owns all barrier state: per-suite in-flight anchor serves, holds, and the FIFO hold token. The handler registers every `<suite>/InRelease` and `<suite>/Release` serve of a tracked suite with the gate before it reads the snapshot pointer (waiting there while that suite's flip holds anchors) and marks it done after the response is written. The adopter runs `CommitAdoption` inside `Gate.Flip`, after the member pipeline and after freeing its host slot; the barrier waits on the checker's lifetime context, never on the adoption deadline, and `main` closes the gate at the shutdown signal, abandoning every flip that has not started its commit. `CommitAdoption` keeps `adopted_at` strictly increasing per suite, the handler serves snapshot anchors with `max(pool mtime, adopted_at)`, and a by-hash member the current snapshot lacks is served from the newest of the suite's displaced, non-retired snapshots.

**Tech Stack:** Go 1.25 (toolchain 1.26), modernc SQLite, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 5; Task 11 records this plan's rulings and deviations as revision 6) — C8 in full (flipgate, handler side, adopter side phases A/B, hold token, skip rules, first-adoption `adopted_at`, guarantee and residuals, retained by-hash, monotonic `Last-Modified`, README claim); C11's `adoption.flip_quiet_period`, the C8 counters and the `adoption_flip_barrier` event; C4 "Adoption capacity" and "Admission bound" (interactions); §7 C8 tests; §9 phase C row. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — Global Constraints, the "C:" entries under "Later phases add" and "Carry-forwards (from phase A and B reviews)". Deferred minors: `2026-09-28-flat-repositories-A-deferred.md` and `…-B-deferred.md` (IDs cited per task).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- No schema change: v7 already has every column this phase reads or writes (`suite_snapshot.adopted_at`, `retired_at`; `url_path.last_fetched_at`). Never edit `migrations`.
- Production SQL never uses SQLite clock functions; persisted stamps come from `wallclock` (the cache's `nowUnix`); all barrier timing uses monotonic `time.Now`/`time.Since`, timers and contexts.
- `internal/flipgate` imports only the standard library and `internal/metrics` (a guard test pins it). It never waits while holding its mutex: every wait is a `select` on channels, timers and contexts. Every section that a `recover` further up can unwind (the adoption's, net/http's, the expiry timer's own) unlocks the mutex in a `defer` (design review M6), so a panic never leaves the gate locked.
- Constants (spec C11), not knobs: hold 20 s (`flipgate.HoldMax`); phase-A cap 60 s; hold-token wait 60 s; waiter cap 256 per suite; apt's timeout 30 s (an anchor response still being sent that long is no longer waited for; design note 2). New knob: `adoption.flip_quiet_period` (default `10s`; `0s` disables; otherwise between `1s` and `15s` — controller ruling, deviation 6).
- `flip_quiet_period = 0s`, and any configuration without an adopter, behaves as the phase-B tip: no gate state, no registration read on the request path, no wait, and `Gate.Close` does nothing. The one exception is Task 7's `Last-Modified` floor, which applies in every configuration: one extra `GetSuiteSnapshot` per snapshot-served `InRelease`/`Release`/`Release.gpg`, whose `Last-Modified` becomes `max(pool mtime, adopted_at)` (design review M4). Every existing test constructs handlers and adopters without a gate and stays unchanged (exceptions listed under Conventions).
- The barrier never holds a host slot (the host slot is freed at `membersDone`, before the barrier) and never cancels a commit that has started.
- Tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`.
- Simulations run inside `testing/synctest` bubbles, finish in under 5 s under `-race`, keep events off each other's virtual instants unless the asserted outcome is order-independent, and silence loggers with `simtest.DiscardLogger()` (or capture them). Gates, caches and contexts a simulation waits on are created inside its bubble.
- Every goroutine recovers panics (the new production goroutines are the hold's `time.AfterFunc` expiry callback and `main`'s shutdown-drain waiter in `waitLogged`, which both do, and `anchorStart`'s `context.AfterFunc`, which only calls a `context.CancelFunc`).
- `go test -race ./...` and `golangci-lint run ./...` are clean; `gofmt -l .` lists only the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task. Never run `gofmt -w` on those five.
- Commit trailer: the implementer's own harness attribution line (phase A ruling); otherwise `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's **Interfaces** block lists what it uses from other tasks and existing test files. Never redefine a helper: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing.
- A "verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed.
- Mutation checks are run one at a time, each reverted before the next; record each failing test line in the commit message.
- **Existing tests that change** — four groups only: the `runRequest` callers in `internal/freshness/capacity_test.go` (new signature, Task 5, plus deferred minor B-29's rewrite of two timing windows there), `TestPhase2Chaos_AdoptionFlipUnderConcurrency` and its detached twin `TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency` (their logged straddle counts become assertions and their AIDEV-NOTEs cite the barrier, Task 10), deferred minor B-6's nil-safe rewrite of `TestCommitAdoption_FlipDescribesAdoptedAnchor`'s check-state assertion (Task 4), and one stale comment in `internal/gc/integration_test.go` (Task 4). Any other existing test that fails means the change is wrong.

## Rulings and design notes

Controller rulings (binding):

- **R-C1 — the global adoption slot is held through the barrier**, as spec C4 ("global slot held through the commit") and the C8 residual say. It bounds the flips waiting in the barrier by the pool size (default 2, so at most one flip ever waits for the token), keeping token contention low. The host slot is still freed at `membersDone`, before the barrier. This resolves the overview's open question.
- **R-C2 — the barrier's waits ignore `adoption.max_duration` but end on shutdown.** Phase A, the token wait and phase B are self-bounded (≤ 60 + 60 + 20 s), and honouring the deadline would throw away finished member work. They wait on the checker's lifetime context; when it ends, nothing commits and nothing is recorded (phase B's rule). The commit keeps `context.WithoutCancel` + `commitTimeout`. Deferred minor B-65 is folded in: shutdown is read from the lifetime context itself, not from `ctx.Err()`, which stays `DeadlineExceeded` once the deadline fired (Task 5).
- **R-C3 — the admission bound's A** becomes `adoption.max_duration` + the barrier (≤ 140 s) + `commitTimeout`: spec C4's "Admission bound", the scheduler `Run` doc and `docs/configuration.md` are updated (Task 11).
- **R-C4 — a Checker-level test that `membersDone` frees only the host slot** (the global slot stays held until after the commit): `TestCheck_MembersDoneFreesOnlyTheHostSlot`, Task 5, the task that wires the barrier.

Design notes (choices the spec leaves open — the boring, bounded option, with the reason):

1. **Registration needs a row, and precedes the pointer read.** Spec C8 tracks only suites with rows. The handler reads the suite row, then registers (`AnchorStart`), then serves; the serve's own `GetSuiteFreshness` (in `trySnapshotHit`) is the authoritative pointer read. Registering after reading the pointer would let a flip commit in between and hand the client the old anchor after the flip. Cost: one extra indexed read per anchor request of a tracked suite; none when the gate is disabled.
2. **An anchor response still being sent 30 s after it started is no longer waited for** ("anchors in flight longer than 30 s, apt's own timeout, are ignored"), so one slow link cannot stall a flip or force a hold to expire. apt's `Acquire::http::Timeout` is an *inactivity* timeout, though: a client on a slow link may still finish receiving it, and may then straddle — the spec's residual, counted in `acu_flip_slow_anchor_reads_total` (design note 20). Its completion still counts while no hold of its suite is in progress (it can only delay phase A by the quiet period), but not during a hold, where it could push the hold into expiry (design review M1, accepted refinement).
3. **A held request whose own context ends is not registered.** Its client is gone (request context) or the handler is closing (by then the server has force-closed connections); counting it in flight would delay the hold for a response nobody reads. An HTTPS request through the MITM pipeline keeps its inner context when its client disconnects (`internal/proxy/connect.go`: once the connection is hijacked, net/http no longer cancels `r.Context()`), so such a waiter stays until the hold ends (at most 20 s) and counts against the waiter cap meanwhile (design review M13; no test: the MITM rig is not cheap to drive into a hold).
4. **One expiry counter.** `acu_flip_hold_expired_total` counts every hold that reached its 20 s bound, whether the commit had not started (it then starts) or was still running (it continues): both are the documented phase-B-expiry residual.
5. **The first-adoption skip compares the form's primary anchor** — `<suite>/InRelease` (inline) or `<suite>/Release` (detached) — with the url_path row's blob. Index consistency depends on the Release text a client holds, not on its detached signature. A lookup error runs the barrier (the safe direction).
6. **The first adoption's `adopted_at`** is `max(now, MAX(last_fetched_at) + 1)` over the suite's `InRelease`, `Release` and `Release.gpg` url_path rows, read inside `CommitAdoption` before Step 3c re-seeds those rows. A url_path serve's `Last-Modified` is its pool file's mtime, not `last_fetched_at`; `runFetch` finalizes the pool file just before it stamps the row, so the mtime is not later than `last_fetched_at` (HTTP dates truncate to seconds), and every anchor a client could hold was served with that `Last-Modified` or older — by that ordering, not by construction (phase G carry-forward, Task 11).
7. **The `Last-Modified` floor also applies on the snapshot-member recovery path** (anchor blob lost and re-fetched): it serves a snapshot anchor too.
8. **Idle entries are swept lazily** (on gate calls, at most once per quiet period): no background goroutine. An idle entry's last completion is older than the quiet period, so dropping it loses nothing.
9. **The adoption's heartbeat ticker runs until the adoption returns, not until its deadline**: the barrier may outlast `max_duration` (R-C2), and the candidate and its member blobs must stay protected until the commit.
10. **`adoption_flip_barrier`** (INFO; spec C11 names it but gives no fields): `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `hold_expired`, `hold_skipped`, `not_needed`, or `commit_failed` when the commit ran and failed — the snapshot did not switch, design review M7), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent on the first-adoption skip). Not logged when the barrier is disabled or when shutdown abandoned the wait (the adoption's own failure line covers that).
11. **Token hand-off:** the next queued flip's hold starts the instant the previous hold ends (pending is marked at the grant), so its 20 s bound runs from there.
12. **Test seams:** the gate copies its three bounds (phase A, token wait, hold) into unexported fields that only `internal/flipgate` unit tests shorten, and its unexported `onExpire` hook (nil in production) lets one unit test inject a panic into the hold-expiry timer; every simulation runs the real constants.
13. **Wiring:** `buildAdopter` builds the gate from `adoption.flip_quiet_period` and passes it in `AdoptionConfig.FlipGate`; `Adopter.FlipGate()` (nil-safe: no adopter, no gate) hands the same gate to the handler through `main`'s new `handlerConfig` helper, which makes the wiring testable without starting the daemon. The handler's `KeepDisplaced` is `gc.keep_displaced`, also when `gc.enabled = false` (displaced snapshots then accumulate; the search stays bounded).
14. **`Run`/`RunDetached` callers** (tests, no checker) have no lifetime context: a `ctx` cancelled before the barrier still abandons the adoption, as today; once inside the barrier, which waits under `context.WithoutCancel(ctx)`, they run to its bounds (design review M8).
15. **Retained by-hash searches the newest `keep_displaced + 1` displaced, non-retired snapshots** (what GC keeps, plus the one a flip displaced since GC last ran) and serves from the newest of them that holds the alias; it never searches further back. A database error answers the pre-C8 404 and logs a WARN.
16. **Deferred minor B-64:** the daemon's shutdown logs INFO `shutdown_waiting_for_adoptions` once if the drain has not finished after 2 s. The drain waits for adoption, repair and reconcile goroutines, including member pipelines still unwinding, and its `detail` says so.
17. **A hold that held no request starts no cross-suite quiet period** (design review I1, accepted; deviation 5). Phase A (b) and phase B (b) exist because a hold delays other suites' requests queued behind a held one on a client's shared connection; a hold with `heldTotal == 0` delayed nobody. `endHoldLocked` therefore updates `lastEnd`/`lastEndBy`/`prevEnd` only for holds that held at least one request (a held request whose client then left still counts — the safe side). Without this, every flip that is not skipped — including an idle suite's zero-length hold — would delay every other flip process-wide by up to the quiet period while keeping its global slot. A hold still in progress keeps blocking phase A (b): it may yet hold requests.
18. **`main` closes the gate at the shutdown signal** (design review M3; controller ruling: yes). `(*Gate).Close` ends the hold in progress (its waiters are answered from the snapshot current then), starts no further hold, and makes every `Flip` that has not started its commit — in phase A, waiting for the token, holding, or called later — return `ErrAborted` wrapping `ErrClosed`; a commit that has started runs to its end. `main` calls it right after the signal, before the listeners drain, so barrier waits end at once and no commit starts late in the stop window. The lifetime context still runs then, so `spawnAdoption` never records an error that matches `flipgate.ErrAborted` (shutdown is not a failure, spec C7). A disabled gate ignores `Close` (`0s` stays the phase-B behaviour). `anchorStart` keeps ending a wait on handler shutdown as defence in depth (design review M19's removal of that context was not applied: not trivial, it changes Task 6's tests).
19. **`timed_out` only for a deadline that fired before the member pipeline finished** (design review I2, accepted). Under R-C2 the deadline can pass while the adoption waits in the barrier; `spawnAdoption` then leaves the outcome to the commit's own error (`db_failed`, a refusal from phase E on, or `timed_out` through the chain when the commit hit `commitTimeout`). `membersDone` sets a flag on the adoption goroutine, which is what `spawnAdoption` tests.
20. **Every residual has its own unlabelled counter** (design review M2; controller ruling: count them): `acu_flip_hold_expired_total` and `acu_flip_hold_skipped_total` (spec C8), plus `acu_flip_waiters_over_cap_total` (a request served at once because 256 already wait in the hold) and `acu_flip_slow_anchor_reads_total` (an anchor response that took 30 s or longer to send). `acu_flip_gate_panics_total` counts panics recovered in the hold-expiry timer — a bug's trace that does not depend on the logger (`internal/flipgate` has none). All are listed in `internal/freshness/metrics.go`'s inventory note and in `docs/log-fields.md`.
21. **`Last-Modified` across a clock step during an adoption is documented, not coded** (design review M5). Folding the previous anchor blobs' mtimes into `adoptionStampTx` would close it, but one future-dated pool file (a restored backup, a `touch`) would then push every later `adopted_at` into the future, and it would change stamps under every test's stubbed clock. `docs/configuration.md` states the gap (`Last-Modified` stays monotonic across same-second flips and backward steps between flips; a step back during an adoption can leave the next flip's `Last-Modified` below the previous one for up to the step's length).
22. **The admission bound with the barrier is simulated** (design review M15): `TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound` (Task 5, 0.4 s under `-race`) runs the scheduler with a pool of two, two busy signed suites in the barrier and a third signed suite queued behind them.

## Deliberate deviations from the spec

1. **The barrier's waits are outside `adoption.max_duration`** (ruling R-C2). Spec C4 says every adoption runs under that deadline; the barrier's waits (≤ 140 s) do not, shutdown still ends them, and an adoption can therefore end up to 140 s after `max_duration`. Task 11 amends spec C4's deadline sentence and its admission bound (R-C3).
2. **"At most three hold" is replaced by what the rules guarantee** (spec §9 phase C row; the C8 residual "hold-token timeout (more than three suites flipping at once)"; the overview's "C:" line). A flip waits for the token only behind other holds, each over within 20 s. Three holds that end normally take less than 60 s, so a fourth simultaneous flip always gets the token; the token wait expires only from the fifth simultaneous busy flip (the fourth only if every hold runs exactly to its bound). The DoD simulation therefore asserts: holds never overlap and each lasts at most 20 s; with holds near that bound four hold and the fifth commits unheld exactly 60 s after asking for the token (within 60 s + 20 s of the first hold's start) and is counted; held suites never straddle. Under the default `max_concurrent_adoptions = 2` at most one flip ever waits for the token (R-C1), so it cannot time out at all. Task 11 corrects the spec and overview text.
3. **Retained by-hash serves SHA512 aliases from phase C** (spec §7: "SHA256 in C, SHA512 in E"). The lookup is algorithm-agnostic and the SHA512 rule ("rely on the verified `snapshot_member` binding") needs no new data; phase C tests it with fixture rows, and phase E adds the adoption step that writes SHA512 alias rows.
4. **`snapshot_displaced`** (spec C11 lists the field without defining it) is a presence-as-signal boolean on the `request` log line of a retained by-hash serve; `X-Cache-Snapshot` names the displaced snapshot.
5. **Phase A (b) and phase B (b) apply only to holds that held at least one request** (spec C8 says "no hold … ended within the same period"; design review I1, accepted; design note 17). Task 11 amends spec C8.
6. **`adoption.flip_quiet_period` is `0s` or between `1s` and `15s`** (spec C11: "`0s` disables; ≤ `15s`"; controller ruling): a sub-second period protects almost no one. Config validation and `flipgate.New` both enforce it; Task 11 amends spec C11.

## Final-review fix wave (2026-10-18)

The three final reviews (`.superpowers/sdd/2026-09-28-flat-repositories-C-flip/final-review-*.md`) led to these controller rulings, applied after Task 11; spec revision 6 was amended to match. Where they contradict the design notes above, they win:

- **The quiet wait is cut short at 15 s** (concurrency Important 1, option b). Answering an expired hold's waiters from the old snapshot right before its commit made nearly every held client straddle once the commit took real time (41–42 against 2 without the barrier, with one slow reader). Once phase B (b) holds, a hold waits for local quiet only until `HoldMax − CommitMargin` (new constant, 5 s), then commits inside the hold: outcome `quiet_cut_short`, counter `acu_flip_quiet_cut_short_total`; expiry stays the backstop. `MaxQuiet ≤ HoldMax − CommitMargin` is pinned. This refines design note 4 (expiry now means "the bound came before the commit finished") and design note 20 (one more residual counter); the five-suite DoD simulation (deviation 2) uses 200 ms commits, and its four holds are cut short at 15 s.
- **Snapshot anchors' `Last-Modified` is `adopted_at` alone** (spec I-1, option a). This supersedes design note 21 and the Global Constraint's `max(pool mtime, adopted_at)`: the pool mtime is used only when the `adopted_at` read fails (logged), so neither a backward step during an adoption nor a recovery rewrite can produce a 304 for a changed anchor.
- **A skipped first-adoption flip honours a closed gate** (`(*Gate).Closed()`); **a held client that leaves is a client abort** (`client_canceled`, no WARN); **a flip abandoned at shutdown is never counted or logged as a failure** (INFO `adoption_abandoned_at_shutdown`, no `acu_adoption_total`, no admin-ring entry; refines design note 10's "the adoption's own failure line covers that"); **the barrier is visible** (gauge `acu_flip_barrier_active`, request field `flip_held_ms`).
- Test-only: `(*Adopter).SetCommitDelayForTest` models a commit's latency in the handler simulations. The remaining can-wait minors are in `2026-09-28-flat-repositories-C-deferred.md`.

## Definitions: straddle and the client model

The simulations count straddles with `simtest.Ledger` (Task 2), whose doc comment carries these definitions:

- **Client model (apt's).** One connection, one request at a time: apt keeps one connection and queue per host, and beyond 10 hosts and in mirror mode every source shares it. An update queues the anchors of its sources, and possibly *fillers* (unrelated downloads that keep the connection busy). When an anchor response has fully arrived, that suite's index request joins the **end** of the queue. A response takes its transfer time to arrive; a slow link keeps the anchor serve in flight at the cache for that long.
- **Generation.** The `X-Cache-Snapshot` a response carried. In the model simulations (Task 2) it is the suite's generation counter at the moment the serve read it — after `AnchorStart` returned, as in the handler; in the end-to-end simulations (Task 10) it is the snapshot id the real handler served.
- **Straddle.** An index response for suite *S* whose generation differs from that of the anchor of *S* the same client received earlier in the same update, when the index request was sent less than the guarantee window (`flip_quiet_period`) after that anchor's arrival — **not counting time the client's queue spent on responses the cache held**. (A request sent exactly at the window's end races a commit made at that same instant — the gate counts a suite quiet once a full period has passed — so it is outside the guarantee.) A hold must not push a client out of its window: that is the cross-suite case the hold token and phase B's cross-suite quiet exist for. Index requests sent later are outside the guarantee and are not checked. In every simulation below the clients' own transfers are short, so their index requests fall inside the window and every mismatch counts.
- **Where the window starts.** The gate measures quiet from when the cache finished *sending* the anchor (`Token.Done`, after the handler returns); the ledger measures from when the client *received* it. In production, kernel send buffering makes the first earlier on a slow link (by at most the buffered bytes over the link's bandwidth), so the effective window is slightly shorter than `flip_quiet_period` (design review M12). In the simulations the two instants coincide.

## Review Focus (phase C share)

1. **Clients sharing one apt queue** (mirror mode, many sources on one host) while several suites flip must not straddle — Tasks 1, 2, 10.
2. **Very large suites and slow clients** (commits or anchor reads longer than the hold) must degrade to today's behaviour, never to client timeouts: no hold exceeds 20 s — Tasks 1, 2, 10.
3. **Shutdown and deadlines:** an adoption in the barrier must never commit after shutdown (the gate closes at the signal), never be recorded as a failure for it, never lose finished member work to `max_duration` nor be mislabelled `timed_out` for a deadline that passed in the barrier, and never keep its host slot — Tasks 1, 5, 9.
4. **No wedge:** every barrier exit (commit, commit failure, panic, expiry, abort, token timeout) releases the hold, its waiters and the token — Task 1.
5. **Clock steps and same-second flips** must never produce a 304 for a changed anchor — Tasks 4, 7.
6. **Off is free and on is bounded:** `0s` changes nothing; gate state exists only for tracked suites, expires when idle and is capped per suite — Tasks 1, 6.

## File map

| File | Change |
|---|---|
| `internal/flipgate/gate.go`, `metrics.go` (+ `gate_test.go`, `imports_test.go`, `sim_test.go`) | New: the barrier, its counters, unit tests, model simulations. |
| `internal/simtest/aptclient.go` (+ `aptclient_test.go`) | apt client model and straddle ledger. |
| `internal/config/config.go` (+ `config_test.go`), `packaging/config/config.toml.default` | `adoption.flip_quiet_period` (`0s`, or `1s` to `15s`). |
| `internal/cache/adoption.go` (+ `flip_state_test.go`, `displaced_members_test.go`) | Monotonic `adopted_at`; `DisplacedSnapshotMembers`; B-7, B-6. |
| `internal/gc/integration_test.go` | Stale comment only. |
| `internal/freshness/adoption.go`, `freshness.go`, `metrics.go` (+ `flip_barrier_test.go`, `capacity_test.go`) | Commit behind the barrier; `runHooks` (B-65); first-adoption skip; event (`commit_failed`); heartbeat; `timed_out` only before the barrier; no record for a flip abandoned at shutdown; R-C4 test; admission-bound simulation; B-15, B-26, B-29, B-68. |
| `internal/handler/handler.go`, `metrics.go` (+ `flip_stack_test.go`, `flip_hook_test.go`, `anchor_lastmod_test.go`, `wallclock_stamps_test.go`, `byhash_displaced_test.go`, `sim_flip_test.go`, `phase2_chaos_test.go`, `phase2_detached_chaos_test.go`) | Anchor hook; `Last-Modified` floor; retained by-hash; end-to-end simulations; A-3 (handler half), A-54. |
| `cmd/apt-cacher-ultra/main.go` (+ `flip_wiring_test.go`) | Wiring; `Gate.Close` at the shutdown signal; B-64; A-48. |
| `README.md`, `docs/configuration.md`, `docs/log-fields.md`, the spec, `internal/freshness/scheduler.go` (doc only), the overview, both deferred files | Documentation, amendments (R-C3, deviation 2, A-7, A-16, A-57), stamps. |

---

### Task 1: The flip barrier package (`internal/flipgate`)

Spec C8 "New package `internal/flipgate`", the gate half of "Handler side", "Adopter side" (phase A, the hold token, phase B, expiry), the skip for `flip_quiet_period = 0s`, "Map entries … expire when idle for twice the quiet period, and are purged on reap", and the C11 constants and counters. Design notes 2 (slow anchors), 17 (holds that held nothing), 18 (`Close`) and 20 (a counter per residual); deviations 5 and 6; design review M6 (deferred unlocks; the expiry timer's recovered panic is counted), M11 (`expire.Stop()` before the hold's end), M18 (`MaxQuiet < HoldMax` pinned) and M19 (the `changed` channel is made only when a flip waits, so an anchor completion nobody waits for allocates nothing). The handler's "suites with a row" rule is Task 6's; the first-adoption skip is Task 5's.

Expectations the review revision changed, and why: `TestFlip_IgnoresAnchorsInFlightPastAptsTimeout`'s second flip now waits the quiet period, because a completion after 30 s counts while no hold is in progress (design note 2, design review M1); a third part checks that one during a hold is still ignored. `TestNew_QuietBounds` rejects sub-second periods (deviation 6) and pins `MaxQuiet < HoldMax` (M18). `TestFlip_HoldsAreExclusiveFIFOAndQuietAfterEachOther` keeps its assertions; its comment now says why c still waits after b's hold, which holds nothing (design note 17). The executability review's P3 (`TestGate_SweepKeepsTheEntryOfAFlipInProgress`) gains a request held in a's hold: under design note 17 a hold that held nothing would not keep k in phase A until 30 s.

**Files:**
- Create: `internal/flipgate/gate.go`, `internal/flipgate/metrics.go`, `internal/flipgate/gate_test.go`, `internal/flipgate/imports_test.go`

**Interfaces:**
- Consumes: `internal/metrics` (`NewCounter`, `Default.Render`).
- Produces (package `flipgate`): constants `MinQuiet`, `MaxQuiet`, `PhaseAMax`, `TokenWaitMax`, `HoldMax`, `AnchorTimeout`, `MaxWaiters`; `ErrQuietOutOfRange`, `ErrAborted`, `ErrClosed`; `type Config struct{ Quiet time.Duration }`; `type Outcome string` with `OutcomeDisabled`, `OutcomeHeld`, `OutcomeHoldExpired`, `OutcomeHoldSkipped`, `OutcomeAborted`; `type Result struct{ Outcome Outcome; PhaseA, TokenWait time.Duration; HoldStart, HoldEnd time.Time; Waiters int; Committed bool }`; `type Stats struct{ Flips, Holds, HoldsExpired, HoldsSkipped, Aborted, HeldRequests int }`; `New(Config) (*Gate, error)`; `Key(scheme, host, suitePath string) string`; `(*Gate).Enabled() bool`, `.Quiet() time.Duration`, `.Stats() Stats`, `.Len() int`, `.Purge(key string) bool`, `.Close()`, `.AnchorStart(ctx, key) *Token`, `.Flip(ctx, key, commit func() error) (Result, error)`; `(*Token).Done()`; counters `acu_flip_hold_skipped_total`, `acu_flip_hold_expired_total`, `acu_flip_waiters_over_cap_total`, `acu_flip_slow_anchor_reads_total`, `acu_flip_gate_panics_total`; the unexported test seam `Gate.onExpire`. Test helpers in package `flipgate` (Task 2 uses them): `testQuiet`, `bg`, `newGate`, `newTestGate`, `noCommit`, `flipOut`, `flipAsync`, `busyReaders`, `counterValue`.

- [ ] **Step 1: Write the failing tests**

`internal/flipgate/imports_test.go`:

```go
package flipgate

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The gate is a leaf: the standard library and internal/metrics only
// (spec C8: the handler and the adopter both import it).
func TestImportsAreLeaf(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			stdlib := !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
			if !stdlib && p != "github.com/linsomniac/apt-cacher-ultra/internal/metrics" {
				t.Errorf("%s imports %q", name, p)
			}
		}
	}
}
```

`internal/flipgate/gate_test.go`:

```go
package flipgate

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// testQuiet is the default adoption.flip_quiet_period.
const testQuiet = 10 * time.Second

var bg = context.Background()

func newGate(t *testing.T, quiet time.Duration) *Gate {
	t.Helper()
	g, err := New(Config{Quiet: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newTestGate(t *testing.T) *Gate {
	t.Helper()
	return newGate(t, testQuiet)
}

func noCommit() error { return nil }

// flipOut is one Flip's result and when it returned.
type flipOut struct {
	res Result
	err error
	at  time.Time
}

// flipAsync runs Flip in its own goroutine.
func flipAsync(ctx context.Context, g *Gate, key string, commit func() error) <-chan flipOut {
	ch := make(chan flipOut, 1)
	go func() {
		res, err := g.Flip(ctx, key, commit)
		ch <- flipOut{res: res, err: err, at: time.Now()}
	}()
	return ch
}

// busyReaders starts an anchor read of key every period, from offset
// until until, each taking transfer.
func busyReaders(g *Gate, key string, offset, period, transfer time.Duration, until time.Time, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(offset)
		for time.Now().Before(until) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok := g.AnchorStart(bg, key)
				time.Sleep(transfer)
				tok.Done()
			}()
			time.Sleep(period)
		}
	}()
}

// counterValue reads an unlabelled counter from the default registry (0
// before its first increment).
func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}

// adoption.flip_quiet_period is 0s or within [1s, 15s] (controller
// ruling): a sub-second period gives almost no protection.
func TestNew_QuietBounds(t *testing.T) {
	for _, q := range []time.Duration{-time.Second, time.Nanosecond, MinQuiet - time.Nanosecond, MaxQuiet + time.Nanosecond} {
		if _, err := New(Config{Quiet: q}); !errors.Is(err, ErrQuietOutOfRange) {
			t.Errorf("Quiet %v: err = %v, want ErrQuietOutOfRange", q, err)
		}
	}
	for _, q := range []time.Duration{0, MinQuiet, MaxQuiet} {
		if _, err := New(Config{Quiet: q}); err != nil {
			t.Errorf("Quiet %v: %v", q, err)
		}
	}
	// Phase B (b) can always be satisfied inside a hold only because a
	// whole quiet period fits in one (design review M18).
	if MaxQuiet >= HoldMax {
		t.Errorf("MaxQuiet %v must stay below HoldMax %v", MaxQuiet, HoldMax)
	}
}

// The gate is keyed by the freshness checker's suite key, so the reaper
// (phase F) can purge an entry with the key it already has.
func TestKey_IsTheFreshnessSuiteKey(t *testing.T) {
	if got := Key("http", "h", "/r/dists/x"); got != "http|h|/r/dists/x" {
		t.Fatalf("Key = %q, want http|h|/r/dists/x", got)
	}
}

// flip_quiet_period = 0s is today's behaviour (spec C8): nothing waits,
// nothing is tracked. A nil gate behaves the same.
func TestDisabledGate_IsTodaysBehaviour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for name, g := range map[string]*Gate{"nil gate": nil, "quiet 0s": newGate(t, 0)} {
			if g.Enabled() {
				t.Fatalf("%s: enabled", name)
			}
			tok := g.AnchorStart(bg, "k")
			if tok != nil {
				t.Fatalf("%s: AnchorStart registered a serve", name)
			}
			tok.Done()
			start := time.Now()
			committed := false
			res, err := g.Flip(bg, "k", func() error { committed = true; return nil })
			if err != nil || !committed || res.Outcome != OutcomeDisabled || time.Since(start) != 0 {
				t.Fatalf("%s: Flip = %+v, %v, committed %v after %v; want an immediate commit", name, res, err, committed, time.Since(start))
			}
			if g.Len() != 0 {
				t.Fatalf("%s: %d entries, want 0", name, g.Len())
			}
		}
	})
}

// Spec §7 C8 "idle zero-wait": a suite nobody is reading flips at once.
func TestFlip_IdleSuiteCommitsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		start := time.Now()
		res, err := g.Flip(bg, "k", noCommit)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("idle flip took %v, want 0", d)
		}
		if res.Outcome != OutcomeHeld || !res.Committed || res.HoldStart.IsZero() || res.HoldEnd != res.HoldStart {
			t.Fatalf("result = %+v, want a zero-length hold that committed", res)
		}
	})
}

// Phase A (spec C8): the flip waits until no anchor of the suite is in
// flight and none completed within the quiet period.
func TestFlip_WaitsForQuietAfterTheLastAnchor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		tok := g.AnchorStart(bg, "k")
		start := time.Now()
		out := flipAsync(bg, g, "k", noCommit)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		select {
		case o := <-out:
			t.Fatalf("committed while an anchor was in flight: %+v", o.res)
		default:
		}
		tok.Done()
		o := <-out
		if o.err != nil {
			t.Fatal(o.err)
		}
		if got := o.at.Sub(start); got != 14*time.Second {
			t.Fatalf("flip returned after %v, want 14s (4s in flight + the 10s quiet period)", got)
		}
		if o.res.PhaseA != 14*time.Second {
			t.Fatalf("phase A took %v, want 14s", o.res.PhaseA)
		}
	})
}

// Design note 2: an anchor response still being sent 30 s after it
// started is treated as abandoned — the barrier stops waiting for it, so
// one slow link cannot stall a flip or force a hold to expire — and is
// counted (spec C8 residual). Its completion still counts while no hold of
// the suite is in progress, but not during a hold.
func TestFlip_IgnoresAnchorsInFlightPastAptsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		slowReads := counterValue(t, "acu_flip_slow_anchor_reads_total")
		slow := g.AnchorStart(bg, "k")
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != AnchorTimeout {
			t.Fatalf("flip returned after %v, want %v (the in-flight read is ignored then)", got, AnchorTimeout)
		}
		time.Sleep(15 * time.Second)
		slow.Done() // completes 45 s after it started, no hold in progress: it counts
		start = time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != testQuiet {
			t.Fatalf("a completion after apt's timeout, outside a hold, delayed the next flip by %v, want %v", got, testQuiet)
		}
		slow = g.AnchorStart(bg, "k") // 55 s
		slowStart := time.Now()
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // holds from 85 s, when slow is ignored
		time.Sleep(40 * time.Second)
		slow.Done()    // 95 s, during the hold: ignored
		close(release) // the hold ends at 95 s
		if o := <-out; o.err != nil || o.res.HoldStart.Sub(slowStart) != AnchorTimeout {
			t.Fatalf("flip = %+v, %v; want a hold from %v after the read started", o.res, o.err, AnchorTimeout)
		}
		start = time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 0 {
			t.Fatalf("a completion after apt's timeout, during a hold, delayed the next flip by %v", got)
		}
		if got := counterValue(t, "acu_flip_slow_anchor_reads_total") - slowReads; got != 2 {
			t.Fatalf("acu_flip_slow_anchor_reads_total rose by %v, want 2", got)
		}
	})
}

// A busy suite never goes quiet: phase A gives up after 60 s; the hold
// then holds new anchor requests, commits once the suite is quiet, and
// answers the held requests from the new generation (spec C8).
func TestFlip_BusySuiteIsHeldAndAnsweredFromTheNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var gen atomic.Int64
		gen.Store(1)
		type served struct {
			start, done time.Time
			gen         int64
		}
		var mu sync.Mutex
		var reads []served
		var wg sync.WaitGroup
		until := time.Now().Add(100 * time.Second)
		wg.Add(1)
		go func() { // a client every 500 ms, each anchor read taking 100 ms
			defer wg.Done()
			for time.Now().Before(until) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					tok := g.AnchorStart(bg, "k")
					v := gen.Load()
					time.Sleep(100 * time.Millisecond)
					tok.Done()
					mu.Lock()
					reads = append(reads, served{start: start, done: time.Now(), gen: v})
					mu.Unlock()
				}()
				time.Sleep(500 * time.Millisecond)
			}
		}()
		time.Sleep(250 * time.Millisecond) // off the request grid
		flipStart := time.Now()
		var committedAt time.Time
		res, err := g.Flip(bg, "k", func() error { committedAt = time.Now(); gen.Store(2); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if res.PhaseA != PhaseAMax || res.Outcome != OutcomeHeld {
			t.Fatalf("result = %+v, want phase A to give up after %v and the hold to end normally", res, PhaseAMax)
		}
		// The last read before the hold started at 60.0 s and ended at
		// 60.1 s; the commit follows the 10 s quiet period.
		if got, want := committedAt.Sub(flipStart), PhaseAMax+9850*time.Millisecond; got != want {
			t.Fatalf("committed %v after the flip started, want %v", got, want)
		}
		wg.Wait()
		held := 0
		for _, s := range reads {
			switch {
			case s.start.Before(res.HoldStart):
				if s.gen != 1 {
					t.Errorf("a read that started before the hold saw generation %d", s.gen)
				}
			case s.start.Before(committedAt):
				held++
				if s.gen != 2 {
					t.Errorf("a held read saw generation %d, want 2 (the new snapshot)", s.gen)
				}
				if w := s.done.Sub(s.start) - 100*time.Millisecond; w > HoldMax {
					t.Errorf("a read was held %v, more than %v", w, HoldMax)
				}
			}
		}
		if held == 0 || res.Waiters != held {
			t.Fatalf("%d reads were held, Result.Waiters = %d", held, res.Waiters)
		}
	})
}

// Phase B is bounded by 20 s (spec §7 C8 "slow reader"): a read slower
// than the hold (but faster than apt's timeout) is in flight when the
// hold starts, so the suite never goes quiet; the hold expires at 20 s,
// the commit starts then, the held requests are answered, and the expiry
// is counted.
func TestFlip_SlowReaderExpiresTheHoldAt20s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		expired := counterValue(t, "acu_flip_hold_expired_total")
		var wg sync.WaitGroup
		for _, at := range []time.Duration{0, 20 * time.Second, 40 * time.Second, 59500 * time.Millisecond} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				tok := g.AnchorStart(bg, "k")
				time.Sleep(25 * time.Second)
				tok.Done()
			}()
		}
		waited := make(chan time.Duration, 1)
		go func() { // arrives during the hold
			time.Sleep(70 * time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		synctest.Wait() // the first slow read is registered
		start := time.Now()
		var committedAt time.Time
		res, err := g.Flip(bg, "k", func() error { committedAt = time.Now(); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeHoldExpired || !res.Committed || res.HoldEnd.Sub(res.HoldStart) != HoldMax {
			t.Fatalf("result = %+v, want a hold that expired after %v and then committed", res, HoldMax)
		}
		if got := committedAt.Sub(start); got != PhaseAMax+HoldMax {
			t.Fatalf("committed %v after the start, want %v", got, PhaseAMax+HoldMax)
		}
		if w := <-waited; w != 10*time.Second {
			t.Fatalf("the held request waited %v, want 10s (until the hold expired)", w)
		}
		if st := g.Stats(); st.HoldsExpired != 1 {
			t.Fatalf("stats = %+v, want one expired hold", st)
		}
		if got := counterValue(t, "acu_flip_hold_expired_total") - expired; got != 1 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 1", got)
		}
		wg.Wait()
	})
}

// A commit slower than the hold: the hold still ends at 20 s — pending is
// cleared and held requests are answered from the snapshot current then
// (the old one) — and the commit is never interrupted (spec C8 residual,
// F-2 on very large suites).
func TestFlip_SlowCommitEndsTheHoldAt20s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var gen atomic.Int64
		gen.Store(1)
		seen := make(chan int64, 1)
		go func() {
			time.Sleep(5 * time.Second)
			tok := g.AnchorStart(bg, "k") // waits in the hold
			seen <- gen.Load()
			tok.Done()
		}()
		start := time.Now()
		res, err := g.Flip(bg, "k", func() error {
			time.Sleep(25 * time.Second)
			gen.Store(2)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if v := <-seen; v != 1 {
			t.Fatalf("a request released when the hold expired saw generation %d, want 1 (the commit had not finished)", v)
		}
		if res.Outcome != OutcomeHoldExpired || res.HoldEnd.Sub(res.HoldStart) != HoldMax || time.Since(start) != 25*time.Second {
			t.Fatalf("result = %+v after %v, want the hold to end at %v and the commit to finish at 25s", res, time.Since(start), HoldMax)
		}
	})
}

// One hold at a time, granted in request order; the next hold starts
// when the previous one ends and commits no sooner than the quiet period
// after that end. b's own readers stop at 55 s, so when its hold starts
// (~70 s) only the cross-suite rule keeps it from committing at once:
// a's hold held requests, and clients released from it may still have
// b's requests queued behind them (spec C8 phase B (b)). b's hold holds
// nothing, so it starts no cross-suite quiet period (design note 17); c
// is kept a quiet period past b's end by its own reads, still in flight
// when its hold starts.
func TestFlip_HoldsAreExclusiveFIFOAndQuietAfterEachOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var wg sync.WaitGroup
		t0 := time.Now()
		keys := []string{"a", "b", "c"}
		for i, k := range keys {
			until := t0.Add(120 * time.Second)
			if k == "b" {
				until = t0.Add(55 * time.Second)
			}
			busyReaders(g, k, time.Duration(7*i)*time.Millisecond, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		}
		var outs []<-chan flipOut
		for _, k := range keys {
			time.Sleep(100 * time.Millisecond) // a, b, c ask for the token 0.1 s apart
			outs = append(outs, flipAsync(bg, g, k, noCommit))
		}
		var res []Result
		for i, ch := range outs {
			o := <-ch
			if o.err != nil || o.res.Outcome != OutcomeHeld {
				t.Fatalf("%s: %+v, %v", keys[i], o.res, o.err)
			}
			res = append(res, o.res)
		}
		for i := 1; i < len(res); i++ {
			if res[i].HoldStart != res[i-1].HoldEnd {
				t.Errorf("%s's hold started at %v, want %s's hold end %v (exclusive, in token order)",
					keys[i], res[i].HoldStart, keys[i-1], res[i-1].HoldEnd)
			}
			if d := res[i].HoldEnd.Sub(res[i-1].HoldEnd); d < testQuiet {
				t.Errorf("%s committed %v after %s's hold ended, want at least the quiet period", keys[i], d, keys[i-1])
			}
		}
		wg.Wait()
	})
}

// While another suite holds, a flip stays in phase A until that hold has
// ended and the quiet period has passed, so its own hold is short (spec
// C8 phase A (b)).
func TestFlip_PhaseAWaitsOutAnotherSuitesHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		var wg sync.WaitGroup
		busyReaders(g, "a", 0, 500*time.Millisecond, 50*time.Millisecond, time.Now().Add(90*time.Second), &wg)
		time.Sleep(250 * time.Millisecond)
		outA := flipAsync(bg, g, "a", noCommit) // holds from 60.25 s
		time.Sleep(65 * time.Second)
		resB, err := g.Flip(bg, "b", noCommit) // b is idle
		if err != nil {
			t.Fatal(err)
		}
		a := <-outA
		if resB.HoldStart.Before(a.res.HoldEnd.Add(testQuiet)) {
			t.Errorf("b's hold started %v after a's ended, want at least the quiet period",
				resB.HoldStart.Sub(a.res.HoldEnd))
		}
		if d := resB.HoldEnd.Sub(resB.HoldStart); d != 0 {
			t.Errorf("b held its anchors for %v; phase A should have waited out the quiet period instead", d)
		}
		wg.Wait()
	})
}

// A flip that cannot get the hold token within the wait commits without
// a hold and is counted (spec C8 residual). The production wait is 60 s;
// TestSimFlip_FiveBusySuites runs it.
func TestFlip_HoldTokenTimeoutCommitsUnheld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax, g.tokenWait = time.Second, 5*time.Second
		skipped := counterValue(t, "acu_flip_hold_skipped_total")
		var wg sync.WaitGroup
		until := time.Now().Add(30 * time.Second)
		busyReaders(g, "a", 0, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		busyReaders(g, "b", 7*time.Millisecond, 500*time.Millisecond, 50*time.Millisecond, until, &wg)
		wg.Add(1)
		go func() { // a read slower than the hold keeps a's hold going to its bound
			defer wg.Done()
			time.Sleep(900 * time.Millisecond)
			tok := g.AnchorStart(bg, "a")
			time.Sleep(25 * time.Second)
			tok.Done()
		}()
		synctest.Wait()                         // the first reads are registered
		outA := flipAsync(bg, g, "a", noCommit) // phase A gives up at 1 s; holds 1 s..21 s
		time.Sleep(10 * time.Millisecond)
		start := time.Now()
		res, err := g.Flip(bg, "b", noCommit) // phase A until 1.01 s, then 5 s for the token
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeHoldSkipped || !res.Committed || res.TokenWait != 5*time.Second || !res.HoldStart.IsZero() {
			t.Fatalf("result = %+v, want an unheld commit after the 5s token wait", res)
		}
		if d := time.Since(start); d != 6*time.Second {
			t.Fatalf("b committed after %v, want 6s", d)
		}
		if got := counterValue(t, "acu_flip_hold_skipped_total") - skipped; got != 1 {
			t.Fatalf("acu_flip_hold_skipped_total rose by %v, want 1", got)
		}
		if st := g.Stats(); st.HoldsSkipped != 1 {
			t.Fatalf("stats = %+v, want one skipped hold", st)
		}
		g.mu.Lock()
		queued := len(g.queue)
		g.mu.Unlock()
		if queued != 0 {
			t.Fatalf("%d flips still queued for the token after timing out", queued)
		}
		if a := <-outA; a.err != nil || a.res.Outcome != OutcomeHoldExpired {
			t.Fatalf("a: %+v, %v; want its hold to run to the bound", a.res, a.err)
		}
		wg.Wait()
	})
}

// At most MaxWaiters requests wait in one hold; the next one is served at
// once (spec C8 waiter cap). All waiters are answered when the hold ends.
func TestAnchorStart_WaiterCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		var returned atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < MaxWaiters; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.AnchorStart(bg, "k").Done()
				returned.Add(1)
			}()
		}
		synctest.Wait()
		if n := returned.Load(); n != 0 {
			t.Fatalf("%d of %d waiters returned during the hold", n, MaxWaiters)
		}
		overCap := counterValue(t, "acu_flip_waiters_over_cap_total")
		extra := g.AnchorStart(bg, "k") // beyond the cap: served at once
		extra.Done()
		if st := g.Stats(); st.HeldRequests != MaxWaiters {
			t.Fatalf("stats = %+v, want %d held requests", st, MaxWaiters)
		}
		if got := counterValue(t, "acu_flip_waiters_over_cap_total") - overCap; got != 1 {
			t.Fatalf("acu_flip_waiters_over_cap_total rose by %v, want 1", got)
		}
		close(release)
		wg.Wait()
		if n := returned.Load(); n != MaxWaiters {
			t.Fatalf("%d waiters answered after the hold, want %d", n, MaxWaiters)
		}
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

// A held request whose client goes away stops waiting at once and is not
// counted: its read never happens (spec §7 C8 "client cancel").
func TestAnchorStart_CancelledWaiterLeavesUncounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil })
		synctest.Wait()
		ctx, cancel := context.WithCancel(bg)
		got := make(chan *Token, 1)
		go func() { got <- g.AnchorStart(ctx, "k") }()
		synctest.Wait()
		cancel()
		tok := <-got
		tok.Done()
		g.mu.Lock()
		e := g.entries["k"]
		inflight, lastDone := len(e.inflight), e.lastDone
		g.mu.Unlock()
		if inflight != 0 || !lastDone.IsZero() {
			t.Fatalf("a cancelled waiter was counted: %d in flight, last completion %v", inflight, lastDone)
		}
		close(release)
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

func assertAborted(t *testing.T, res Result, err error) {
	t.Helper()
	if !errors.Is(err, ErrAborted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrAborted wrapping context.Canceled", err)
	}
	if res.Outcome != OutcomeAborted || res.Committed {
		t.Fatalf("result = %+v, want aborted and not committed", res)
	}
}

// Shutdown (the flip's context ending) abandons a flip in any phase:
// nothing commits, a started hold ends and its waiters are answered, and
// the token passes on (spec C8, ruling R-C2).
func TestFlip_ContextEndAbortsEveryPhase(t *testing.T) {
	mustNotCommit := func(t *testing.T) func() error {
		return func() error { t.Error("committed after shutdown"); return nil }
	}
	t.Run("before", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			ctx, cancel := context.WithCancel(bg)
			cancel()
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if st := g.Stats(); st.Holds != 0 || st.Aborted != 1 {
				t.Fatalf("stats = %+v", st)
			}
		})
	})
	t.Run("phase A", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			tok := g.AnchorStart(bg, "k")
			defer tok.Done()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(3*time.Second, cancel)
			start := time.Now()
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if d := time.Since(start); d != 3*time.Second || g.Stats().Holds != 0 {
				t.Fatalf("aborted after %v with %d holds, want 3s and none", d, g.Stats().Holds)
			}
		})
	})
	t.Run("token wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			g.phaseAMax = time.Second
			var wg sync.WaitGroup
			defer wg.Wait()
			slow := g.AnchorStart(bg, "a") // keeps a's hold going to its bound
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(25 * time.Second); slow.Done() }()
			outA := flipAsync(bg, g, "a", noCommit) // holds from 1 s
			time.Sleep(500 * time.Millisecond)
			reading := g.AnchorStart(bg, "b") // b is busy: its phase A gives up at 1.5 s
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(2 * time.Second); reading.Done() }()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(2500*time.Millisecond, cancel) // at 3 s, while b waits for the token
			res, err := g.Flip(ctx, "b", mustNotCommit(t))
			assertAborted(t, res, err)
			g.mu.Lock()
			queued := len(g.queue)
			g.mu.Unlock()
			if queued != 0 {
				t.Fatalf("an aborted flip stayed in the token queue (%d queued)", queued)
			}
			if a := <-outA; a.err != nil || a.res.Outcome != OutcomeHoldExpired {
				t.Fatalf("a: %+v, %v; want it unaffected", a.res, a.err)
			}
		})
	})
	t.Run("phase B", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := newTestGate(t)
			g.phaseAMax = time.Second
			var wg sync.WaitGroup
			defer wg.Wait()
			slow := g.AnchorStart(bg, "k") // in flight until 5 s: phase B waits for it
			wg.Add(1)
			go func() { defer wg.Done(); time.Sleep(5 * time.Second); slow.Done() }()
			waited := make(chan time.Duration, 1)
			go func() {
				time.Sleep(2 * time.Second)
				start := time.Now()
				g.AnchorStart(bg, "k").Done()
				waited <- time.Since(start)
			}()
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(3*time.Second, cancel)
			res, err := g.Flip(ctx, "k", mustNotCommit(t))
			assertAborted(t, res, err)
			if res.HoldStart.IsZero() {
				t.Fatal("the hold had not started; this case is meant to abort phase B")
			}
			if w := <-waited; w != time.Second {
				t.Fatalf("a held request waited %v, want 1s (released by the abort)", w)
			}
			g.mu.Lock()
			holder := g.holder
			g.mu.Unlock()
			if holder != nil {
				t.Fatal("the token is still held after the abort")
			}
		})
	})
}

// A failed commit still ends the hold: waiters are answered and the
// token is free again (spec §7 C8 "commit failure releases waiters").
func TestFlip_CommitFailureEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		boom := errors.New("commit failed")
		waited := make(chan time.Duration, 1)
		go func() {
			time.Sleep(time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		res, err := g.Flip(bg, "k", func() error { time.Sleep(3 * time.Second); return boom })
		if !errors.Is(err, boom) || res.Outcome != OutcomeHeld || !res.Committed {
			t.Fatalf("Flip = %+v, %v; want the commit's error from a held flip", res, err)
		}
		if w := <-waited; w != 2*time.Second {
			t.Fatalf("the held request waited %v, want 2s", w)
		}
		g.mu.Lock()
		holder := g.holder
		g.mu.Unlock()
		if holder != nil {
			t.Fatal("the token is still held after a failed commit")
		}
	})
}

// A panicking commit still ends the hold through Flip's own defer (spec
// C5), and the panic reaches the caller (the adoption's recover).
func TestFlip_CommitPanicEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		waited := make(chan time.Duration, 1)
		go func() {
			time.Sleep(time.Second)
			start := time.Now()
			g.AnchorStart(bg, "k").Done()
			waited <- time.Since(start)
		}()
		func() {
			defer func() {
				if r := recover(); r != "commit bug" {
					t.Fatalf("recovered %v, want the commit's panic", r)
				}
			}()
			_, _ = g.Flip(bg, "k", func() error { time.Sleep(2 * time.Second); panic("commit bug") })
		}()
		if w := <-waited; w != time.Second {
			t.Fatalf("the held request waited %v, want 1s", w)
		}
		g.mu.Lock()
		holder, flips := g.holder, g.entries["k"].flips
		g.mu.Unlock()
		if holder != nil || flips != 0 {
			t.Fatalf("after the panic: holder %v, %d flips in progress; want neither", holder, flips)
		}
	})
}

// Entries expire after twice the quiet period idle (spec C8); an entry
// with a serve in flight never does.
func TestGate_IdleEntriesExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.AnchorStart(bg, "a").Done()        // the first call sweeps (an empty map)
		reading := g.AnchorStart(bg, "busy") // in flight for the whole test
		defer reading.Done()
		time.Sleep(19 * time.Second)
		g.AnchorStart(bg, "b").Done() // sweeps: a has been idle 19 s, under 20 s
		if n := g.Len(); n != 3 {
			t.Fatalf("%d entries at 19 s, want 3", n)
		}
		time.Sleep(12 * time.Second)
		g.AnchorStart(bg, "c").Done() // sweeps: a idle 31 s goes, b idle 12 s stays
		g.mu.Lock()
		_, a := g.entries["a"]
		_, busy := g.entries["busy"]
		n := len(g.entries)
		g.mu.Unlock()
		if a || !busy || n != 3 {
			t.Fatalf("after the second sweep: a kept %v, busy kept %v, %d entries; want a gone, busy kept, 3", a, busy, n)
		}
	})
}

// Purge drops an idle entry (phase F's reaper calls it); a token of the
// dropped instance stays harmless, and a flip in progress keeps its entry.
func TestGate_PurgeKeepsOldTokensHarmless(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		old := g.AnchorStart(bg, "k")
		if !g.Purge("k") || g.Len() != 0 {
			t.Fatalf("Purge did not drop the entry (%d left)", g.Len())
		}
		old.Done()
		if g.Len() != 0 {
			t.Fatal("a purged entry's token re-created it")
		}
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil || time.Since(start) != 0 {
			t.Fatalf("a fresh entry was delayed %v by a purged entry's serve (err %v)", time.Since(start), err)
		}
		release := make(chan struct{})
		out := flipAsync(bg, g, "k2", func() error { <-release; return nil })
		synctest.Wait()
		if g.Purge("k2") {
			t.Fatal("purged an entry whose flip is in progress")
		}
		close(release)
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

func TestCounters_AreRegistered(t *testing.T) {
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, name := range []string{"acu_flip_hold_skipped_total", "acu_flip_hold_expired_total",
		"acu_flip_waiters_over_cap_total", "acu_flip_slow_anchor_reads_total", "acu_flip_gate_panics_total"} {
		if !strings.Contains(buf.String(), "# TYPE "+name+" counter") {
			t.Errorf("%s is not registered", name)
		}
	}
}

// Spec §7 C8 DoD "phase-B expiry (commit or in-flight reader > 20 s)
// counted": a commit slower than the hold is counted like a slow reader.
func TestFlip_SlowCommitExpiryIsCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		expired := counterValue(t, "acu_flip_hold_expired_total")
		res, err := g.Flip(bg, "k", func() error { time.Sleep(25 * time.Second); return nil })
		if err != nil || res.Outcome != OutcomeHoldExpired {
			t.Fatalf("Flip = %+v, %v; want a hold that expired during the commit", res, err)
		}
		if st := g.Stats(); st.HoldsExpired != 1 {
			t.Fatalf("stats = %+v, want one expired hold", st)
		}
		if got := counterValue(t, "acu_flip_hold_expired_total") - expired; got != 1 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 1", got)
		}
	})
}

// A grant that lands at the instant the token wait times out is taken (or
// passed on), never leaked: once both flips have returned the token is free
// and nothing is queued. The two events share one virtual instant, so the
// order is up to the scheduler; 200 rounds make the late-grant path all but
// certain.
func TestFlip_GrantAtTheTokenTimeoutNeverLeaksTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax, g.tokenWait = 0, 5*time.Second
		for i := 0; i < 200; i++ {
			outA := flipAsync(bg, g, "a", func() error { time.Sleep(5 * time.Second); return nil }) // holds 5 s
			synctest.Wait()
			outB := flipAsync(bg, g, "b", noCommit) // its 5 s token wait ends with a's hold
			if a := <-outA; a.err != nil {
				t.Fatal(a.err)
			}
			b := <-outB
			if b.err != nil {
				t.Fatal(b.err)
			}
			g.mu.Lock()
			holder, queued := g.holder, len(g.queue)
			g.mu.Unlock()
			if holder != nil || queued != 0 {
				t.Fatalf("round %d: b %s; holder %v and %d queued after both flips returned", i, b.res.Outcome, holder, queued)
			}
			time.Sleep(time.Minute) // every quiet period is over
		}
	})
}

// The idle sweep never drops the entry of a flip in progress. a's hold
// holds a request, so k's flip waits in phase A for that hold and the quiet
// period after it (until 30 s); a sweep at 25 s finds k's entry idle for
// 24 s. A client that starts reading k's anchor at 26 s must still hold
// k's commit back.
func TestGate_SweepKeepsTheEntryOfAFlipInProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		outA := flipAsync(bg, g, "a", func() error { <-release; return nil }) // holds from 0 s to its 20 s bound
		synctest.Wait()
		go func() { g.AnchorStart(bg, "a").Done() }() // held until a's hold ends
		time.Sleep(time.Second)
		outK := flipAsync(bg, g, "k", noCommit)
		time.Sleep(24 * time.Second)
		g.AnchorStart(bg, "x").Done() // 25 s: sweeps
		time.Sleep(time.Second)
		reading := g.AnchorStart(bg, "k") // 26 s
		time.Sleep(14 * time.Second)
		readDone := time.Now()
		reading.Done() // 40 s
		k := <-outK
		if k.err != nil {
			t.Fatal(k.err)
		}
		if k.at.Before(readDone.Add(testQuiet)) {
			t.Fatalf("k committed %v after the read it must wait for ended, want at least the quiet period", k.at.Sub(readDone))
		}
		close(release)
		if a := <-outA; a.err != nil {
			t.Fatal(a.err)
		}
	})
}

// A completion refreshes the entry's idle clock: an anchor read slower than
// twice the quiet period that has just finished is not swept, so a flip
// right after it still waits out the quiet period.
func TestGate_SweepKeepsAJustFinishedSlowRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		slow := g.AnchorStart(bg, "k") // 0 s (this call sweeps an empty map)
		time.Sleep(25 * time.Second)
		slow.Done() // 25 s: under apt's 30 s timeout, so it counts
		time.Sleep(time.Second)
		g.AnchorStart(bg, "x").Done() // 26 s: sweeps
		start := time.Now()
		if _, err := g.Flip(bg, "k", noCommit); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 9*time.Second {
			t.Fatalf("flip waited %v, want 9s (the quiet period after the 25 s completion)", got)
		}
	})
}

// The waiter cap counts the requests waiting now: a waiter whose client left
// frees its place, so a later request still waits in the hold.
func TestAnchorStart_WaiterCapCountsOnlyCurrentWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		release := make(chan struct{})
		out := flipAsync(bg, g, "k", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		ctx, cancel := context.WithCancel(bg)
		var wg sync.WaitGroup
		for i := 0; i < MaxWaiters; i++ {
			c := bg
			if i == 0 {
				c = ctx
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.AnchorStart(c, "k").Done()
			}()
		}
		synctest.Wait()
		cancel() // one waiting client leaves
		synctest.Wait()
		late := make(chan struct{})
		go func() {
			g.AnchorStart(bg, "k").Done()
			close(late)
		}()
		synctest.Wait()
		select {
		case <-late:
			t.Fatalf("a request was served during the hold with only %d others waiting", MaxWaiters-1)
		default:
		}
		close(release)
		wg.Wait()
		<-late
		if o := <-out; o.err != nil {
			t.Fatal(o.err)
		}
	})
}

// Design note 17 (design review I1): a hold that held no request delayed
// no client's queue, so it starts no cross-suite quiet period: an idle
// suite that flips a second after another idle suite's flip commits at
// once.
func TestFlip_HoldsThatHeldNothingDoNotDelayOtherFlips(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		if res, err := g.Flip(bg, "a", noCommit); err != nil || res.Waiters != 0 {
			t.Fatalf("a: %+v, %v; want a hold that held nothing", res, err)
		}
		time.Sleep(time.Second)
		start := time.Now()
		res, err := g.Flip(bg, "b", noCommit)
		if err != nil {
			t.Fatal(err)
		}
		if res.PhaseA != 0 || time.Since(start) != 0 {
			t.Fatalf("b: phase A %v, committed after %v; want both 0 (a's hold held nothing)", res.PhaseA, time.Since(start))
		}
	})
}

// Design note 18 (design review M3): main closes the gate at the shutdown
// signal. Every flip that has not started its commit — in phase A, waiting
// for the token, or called later — returns ErrAborted wrapping ErrClosed at
// once; the hold in progress ends and answers its waiters, and no hold
// starts again; a commit already running finishes.
func TestGate_CloseAbortsEveryFlipNotYetCommitting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.phaseAMax = time.Second
		release := make(chan struct{})
		committing := flipAsync(bg, g, "a", func() error { <-release; return nil }) // idle: holds at once
		synctest.Wait()
		held := make(chan time.Time, 1)
		go func() { g.AnchorStart(bg, "a").Done(); held <- time.Now() }() // waits in a's hold
		busyB := g.AnchorStart(bg, "b")
		defer busyB.Done()
		queued := flipAsync(bg, g, "b", noCommit) // phase A gives up at 1 s; then it queues for the token
		time.Sleep(1500 * time.Millisecond)
		busyC := g.AnchorStart(bg, "c")
		defer busyC.Done()
		inPhaseA := flipAsync(bg, g, "c", noCommit) // in phase A until 2.5 s
		time.Sleep(500 * time.Millisecond)
		closedAt := time.Now()
		g.Close()
		for name, ch := range map[string]<-chan flipOut{"queued for the token": queued, "in phase A": inPhaseA} {
			o := <-ch
			if !errors.Is(o.err, ErrAborted) || !errors.Is(o.err, ErrClosed) || o.res.Committed || !o.at.Equal(closedAt) {
				t.Errorf("%s: %+v, %v, %v after Close; want ErrAborted wrapping ErrClosed at once and no commit",
					name, o.res, o.err, o.at.Sub(closedAt))
			}
		}
		if at := <-held; !at.Equal(closedAt) {
			t.Errorf("the held request was answered %v after Close, want at once", at.Sub(closedAt))
		}
		if _, err := g.Flip(bg, "d", func() error { t.Error("committed after Close"); return nil }); !errors.Is(err, ErrClosed) {
			t.Errorf("a flip after Close: %v, want ErrAborted wrapping ErrClosed", err)
		}
		close(release)
		if o := <-committing; o.err != nil || o.res.Outcome != OutcomeHeld || !o.res.Committed {
			t.Fatalf("the commit in progress: %+v, %v; want it to finish", o.res, o.err)
		}
		if st := g.Stats(); st.Holds != 1 || st.Aborted != 3 {
			t.Fatalf("stats = %+v, want one hold (none started after Close) and three aborted flips", st)
		}
	})
}

// Design review M6 (exec review observation 4): a panic in the hold-expiry
// timer — a gate bug, injected here — is counted, and the timer's deferred
// unlock keeps the gate usable: the commit's return still ends the hold
// through the flip's own deferred end, and the next flip runs.
func TestFlip_PanicInTheExpiryTimerLeavesTheGateUsable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		g.onExpire = func() { panic("injected gate bug") }
		panics := counterValue(t, "acu_flip_gate_panics_total")
		out := flipAsync(bg, g, "k", func() error { time.Sleep(25 * time.Second); return nil })
		time.Sleep(21 * time.Second) // the expiry timer fired, and panicked, at 20 s
		if !g.mu.TryLock() {
			g.mu.Unlock() // let the flip finish
			t.Fatal("a panic in the expiry timer left the gate locked")
		}
		g.mu.Unlock()
		o := <-out
		if o.err != nil || !o.res.Committed || o.res.HoldEnd.Sub(o.res.HoldStart) != 25*time.Second {
			t.Fatalf("Flip = %+v, %v; want the commit's return at 25 s to end the hold", o.res, o.err)
		}
		if got := counterValue(t, "acu_flip_gate_panics_total") - panics; got != 1 {
			t.Fatalf("acu_flip_gate_panics_total rose by %v, want 1", got)
		}
		start := time.Now()
		if _, err := g.Flip(bg, "k2", noCommit); err != nil || time.Since(start) != 0 {
			t.Fatalf("the next flip: err %v after %v, want an immediate commit", err, time.Since(start))
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/flipgate/`
Expected: compile failure naming undefined `Gate`, `New`, `Config`, `Result`, `MaxQuiet` and more (Go stops at ten errors).

- [ ] **Step 3: The counters**

`internal/flipgate/metrics.go`:

```go
package flipgate

import "github.com/linsomniac/apt-cacher-ultra/internal/metrics"

// Spec C8 counters, unlabelled (spec C11: no suite labels): one per
// documented residual, plus the expiry timer's recovered panics. Listed in
// the flat-repository inventory note in internal/freshness/metrics.go.
var (
	holdSkippedTotal = metrics.NewCounter(
		"acu_flip_hold_skipped_total",
		"Flips that committed without a hold because the hold token was not free within 60 s (spec C8).",
	)
	holdExpiredTotal = metrics.NewCounter(
		"acu_flip_hold_expired_total",
		"Flip holds that reached their 20 s bound before the commit finished (spec C8).",
	)
	waitersOverCapTotal = metrics.NewCounter(
		"acu_flip_waiters_over_cap_total",
		"Anchor requests served at once during a flip hold because 256 requests already waited in it (spec C8).",
	)
	slowAnchorReadsTotal = metrics.NewCounter(
		"acu_flip_slow_anchor_reads_total",
		"InRelease/Release responses that took 30 s or longer to send; the flip barrier does not wait for them (spec C8).",
	)
	gatePanicsTotal = metrics.NewCounter(
		"acu_flip_gate_panics_total",
		"Panics recovered in the flip barrier's hold-expiry timer: a bug, the barrier stays usable (spec C8).",
	)
)
```

- [ ] **Step 4: The gate**

`internal/flipgate/gate.go`:

```go
// Package flipgate is the spec C8 flip barrier (flat-repositories design,
// limitation #2). An adoption's commit — the switch of a suite's current
// snapshot — waits until every client that received the suite's old
// anchor (InRelease or Release) has had the quiet period to fetch the
// matching indexes:
//
//   - Phase A waits, at most PhaseAMax, for natural quiet: no anchor of
//     the suite in flight, none completed within the quiet period, no hold
//     in progress anywhere, and none that held requests ended within the
//     quiet period.
//   - The flip then takes the process-wide hold token (FIFO, one holder at
//     a time, waiting at most TokenWaitMax; on timeout it commits unheld).
//   - Phase B holds the suite's new anchor requests until it is quiet and
//     no other suite's hold that held requests ended within the quiet
//     period, commits, and answers the held requests from the new
//     snapshot — all within HoldMax of the grant; at that bound the hold
//     ends regardless and a commit not yet started starts.
//
// Close (the daemon's shutdown signal) ends the hold in progress and
// abandons every flip that has not started its commit.
//
// AIDEV-NOTE: the gate never waits while holding its mutex: every wait is
// a select on channels, timers and contexts, so it runs inside
// testing/synctest bubbles. A Token references its entry instance, so an
// entry that was purged, or expired and re-created, is never confused by
// a late Done. Every release is idempotent and the hold's end is deferred:
// a commit that fails or panics still ends its hold, answers its waiters
// and passes the token on. Code that a recover further up can unwind (the
// adoption's, net/http's, the expiry timer's own) unlocks the mutex in a
// defer, so a panic never leaves the gate locked.
package flipgate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Spec C11 constants, not knobs.
//
// AIDEV-NOTE: MaxQuiet < HoldMax is load-bearing (TestNew_QuietBounds pins
// it): phase B (b) can always be satisfied inside a hold only because a
// whole quiet period fits in one, and the straddle guarantee relies on it.
const (
	// MinQuiet and MaxQuiet bound a non-zero adoption.flip_quiet_period;
	// zero disables the barrier.
	MinQuiet = time.Second
	MaxQuiet = 15 * time.Second
	// PhaseAMax bounds phase A, the wait for natural quiet without holds.
	PhaseAMax = 60 * time.Second
	// TokenWaitMax bounds the wait for the process-wide hold token; a flip
	// that times out commits without a hold (acu_flip_hold_skipped_total).
	TokenWaitMax = 60 * time.Second
	// HoldMax bounds a hold, from the token's grant, and so every held
	// anchor request; it stays below apt's 30 s Acquire::http::Timeout.
	HoldMax = 20 * time.Second
	// AnchorTimeout is apt's 30 s Acquire::http::Timeout. apt's is an
	// inactivity timeout, but the barrier treats an anchor response still
	// being sent this long after it started as abandoned and stops waiting
	// for it, so one slow link cannot stall a flip or force a hold to
	// expire. A client that does finish it may straddle (a documented
	// residual, counted in acu_flip_slow_anchor_reads_total); its
	// completion still counts while no hold of its suite is in progress.
	AnchorTimeout = 30 * time.Second
	// MaxWaiters caps the requests waiting in one hold; a request beyond
	// it is served at once, counts as in flight and is counted
	// (acu_flip_waiters_over_cap_total).
	MaxWaiters = 256
)

var (
	// ErrQuietOutOfRange rejects a quiet period that is neither zero nor
	// within [MinQuiet, MaxQuiet].
	ErrQuietOutOfRange = errors.New("flipgate: flip quiet period must be 0s or between 1s and 15s")
	// ErrAborted reports that the flip's context ended, or the gate was
	// closed, before its commit started, so nothing was committed. Flip
	// wraps the cause: the context's error, or ErrClosed.
	ErrAborted = errors.New("flipgate: flip abandoned before the commit")
	// ErrClosed is the cause of a flip abandoned because Close ran.
	ErrClosed = errors.New("flipgate: gate closed")
)

// Config configures a Gate.
type Config struct {
	// Quiet is adoption.flip_quiet_period; zero disables the barrier.
	Quiet time.Duration
}

// Outcome says how a Flip ended (the adoption_flip_barrier outcome).
type Outcome string

// Flip outcomes.
const (
	OutcomeDisabled    Outcome = "disabled"     // no barrier: committed at once
	OutcomeHeld        Outcome = "held"         // committed within its hold
	OutcomeHoldExpired Outcome = "hold_expired" // the hold reached HoldMax before the commit finished
	OutcomeHoldSkipped Outcome = "hold_skipped" // no token within TokenWaitMax: committed unheld
	OutcomeAborted     Outcome = "aborted"      // ctx ended or the gate closed before the commit started: nothing committed
)

// Result describes one Flip.
type Result struct {
	Outcome   Outcome
	PhaseA    time.Duration // waiting for natural quiet
	TokenWait time.Duration // waiting for the hold token
	HoldStart time.Time     // zero when no hold started
	HoldEnd   time.Time     // zero when no hold started
	Waiters   int           // anchor requests this flip's hold held
	Committed bool          // commit ran
}

// Stats counts what a gate did since New (tests, diagnostics).
type Stats struct {
	Flips        int // Flip calls that ran the barrier
	Holds        int // holds started
	HoldsExpired int // holds that reached HoldMax before their commit finished
	HoldsSkipped int // flips that committed unheld after waiting TokenWaitMax
	Aborted      int // flips whose context ended, or whose gate closed, before the commit started
	HeldRequests int // anchor requests that waited in a hold
}

// Gate is the flip barrier, keyed by Key. A nil *Gate is a disabled gate.
type Gate struct {
	quiet time.Duration
	// The spec C11 bounds, copied from the constants by New; unexported so
	// that only this package's unit tests shorten them.
	phaseAMax, tokenWait, holdMax time.Duration
	// onExpire is a test seam (nil in production): it runs, with the mutex
	// held, whenever a hold reaches its bound.
	onExpire func()
	// closedCh is closed by Close; flips waiting for the token watch it.
	closedCh chan struct{}

	mu        sync.Mutex
	entries   map[string]*entry
	changed   chan struct{} // closed on every change a flip may wait for; nil while no flip waits
	holder    *hold         // the hold in progress, nil if none
	queue     []*tokenWaiter
	lastEnd   time.Time // when the most recent hold that held requests ended (zero: none yet)
	lastEndBy *entry    // whose hold that was
	prevEnd   time.Time // the most recent such end by an entry other than lastEndBy
	lastSweep time.Time
	closed    bool
	stats     Stats
}

type entry struct {
	inflight map[*Token]struct{}
	lastDone time.Time // completion of the newest counted anchor serve
	hold     *hold     // this suite's hold, while phase B runs
	flips    int       // Flip calls in progress for this key
	lastUse  time.Time
}

type hold struct {
	e         *entry
	start     time.Time
	end       time.Time
	released  chan struct{} // closed when the hold ends
	waiting   int           // requests waiting now (MaxWaiters applies)
	heldTotal int           // requests that waited in this hold
	ended     bool
	expired   bool
}

type tokenWaiter struct {
	e       *entry
	granted chan struct{}
	hold    *hold // set under the gate's mutex before granted is closed
}

// Token is one anchor serve registered with the gate.
type Token struct {
	g     *Gate
	e     *entry
	start time.Time
	done  bool // guarded by g.mu
}

// New returns a gate. A zero Quiet returns a disabled gate: AnchorStart
// registers nothing and Flip commits at once (the pre-C8 behaviour).
func New(cfg Config) (*Gate, error) {
	if q := cfg.Quiet; q != 0 && (q < MinQuiet || q > MaxQuiet) {
		return nil, fmt.Errorf("%w: %v", ErrQuietOutOfRange, cfg.Quiet)
	}
	return &Gate{
		quiet:     cfg.Quiet,
		phaseAMax: PhaseAMax,
		tokenWait: TokenWaitMax,
		holdMax:   HoldMax,
		closedCh:  make(chan struct{}),
		entries:   map[string]*entry{},
	}, nil
}

// Key is the gate key of a suite: the freshness checker's suite key.
func Key(scheme, host, suitePath string) string {
	return scheme + "|" + host + "|" + suitePath
}

// Enabled reports whether the barrier runs.
func (g *Gate) Enabled() bool { return g != nil && g.quiet > 0 }

// Quiet returns the quiet period (zero for a nil or disabled gate).
func (g *Gate) Quiet() time.Duration {
	if g == nil {
		return 0
	}
	return g.quiet
}

// Stats returns the gate's counters.
func (g *Gate) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Len returns the number of suites the gate tracks.
func (g *Gate) Len() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}

// Purge drops key's entry unless a flip of it is in progress, and reports
// whether it did. Spec C8: entries are purged when their suite is reaped
// (phase F). Tokens of a dropped entry stay valid and touch only it.
func (g *Gate) Purge(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entries[key]
	if e == nil || e.flips > 0 {
		return false
	}
	delete(g.entries, key)
	return true
}

// Close ends the barrier at shutdown; main calls it at the signal, before
// the listeners drain (design note 18). The hold in progress ends — its
// waiters are answered from the snapshot current then — no hold starts
// again, and every Flip that has not started its commit, waiting now or
// called later, returns ErrAborted wrapping ErrClosed. A commit that has
// started runs to its end. Idempotent. A nil or disabled gate ignores it:
// with flip_quiet_period = 0s Flip commits at once, as before.
func (g *Gate) Close() {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.closedCh)
	if g.holder != nil {
		g.endHoldLocked(g.holder, false)
	}
	g.broadcastLocked()
}

// AnchorStart registers an anchor serve of key (spec C8, handler side) and
// returns its token; Done must run once the response is written. It
// returns at once unless a flip of key is holding anchors: then it waits
// until that hold ends — at most HoldMax, and while ctx lasts — unless
// MaxWaiters requests already wait. A request whose ctx ends while it
// waits (its client left, or the handler is closing) is not registered:
// its token's Done does nothing. A disabled gate returns nil; Done on a
// nil Token is a no-op.
func (g *Gate) AnchorStart(ctx context.Context, key string) *Token {
	if !g.Enabled() {
		return nil
	}
	t, e, h := g.join(key)
	if t != nil {
		return t
	}
	timer := time.NewTimer(g.holdMax)
	defer timer.Stop()
	left := false
	select {
	case <-h.released:
	case <-timer.C:
	case <-ctx.Done():
		left = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	h.waiting--
	if left {
		return &Token{}
	}
	return g.registerLocked(e, time.Now())
}

// join registers an anchor serve of key and returns its token or, while a
// flip of key holds anchors and fewer than MaxWaiters requests wait in that
// hold, joins the hold instead: a nil token, with the entry and the hold.
// A request beyond the cap is served at once and counted.
func (g *Gate) join(key string) (*Token, *entry, *hold) {
	g.mu.Lock()
	defer g.mu.Unlock() // net/http recovers a panicking request: never leave the gate locked
	now := time.Now()
	e := g.entryLocked(key, now)
	h := e.hold
	if h == nil || h.waiting >= MaxWaiters {
		if h != nil {
			waitersOverCapTotal.Inc() // spec C8 residual: served at once
		}
		return g.registerLocked(e, now), nil, nil
	}
	h.waiting++
	h.heldTotal++
	g.stats.HeldRequests++
	return nil, e, h
}

// Done records the serve's completion and drops it from its suite's
// in-flight set. A serve that ran for AnchorTimeout or longer is counted
// (acu_flip_slow_anchor_reads_total) and records its completion only while
// no hold of its suite is in progress (design note 2). Safe on a nil or
// unregistered Token, and idempotent.
func (t *Token) Done() {
	if t == nil || t.g == nil {
		return
	}
	g := t.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	now := time.Now()
	delete(t.e.inflight, t)
	t.e.lastUse = now
	late := now.Sub(t.start) >= AnchorTimeout
	if late {
		slowAnchorReadsTotal.Inc()
	}
	// A late completion can only delay phase A by the quiet period; during
	// a hold it could push the hold into expiry, so it is ignored there.
	if (!late || t.e.hold == nil) && now.After(t.e.lastDone) {
		t.e.lastDone = now
	}
	g.broadcastLocked()
}

// Flip runs commit behind the barrier for key and reports how it went.
// ctx is the shutdown signal (the freshness checker's lifetime context),
// never the adoption's deadline (ruling R-C2): when it ends, or the gate
// is closed, before the commit starts, Flip returns ErrAborted and commits
// nothing; a commit that has started always runs to its end. commit's
// error is returned as is.
func (g *Gate) Flip(ctx context.Context, key string, commit func() error) (Result, error) {
	var res Result
	if err := ctx.Err(); err != nil {
		err = g.abort(&res, err)
		return res, err
	}
	if !g.Enabled() {
		res.Outcome, res.Committed = OutcomeDisabled, true
		return res, commit()
	}
	start := time.Now()
	g.mu.Lock()
	e := g.entryLocked(key, start)
	e.flips++
	g.stats.Flips++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		e.flips--
		e.lastUse = time.Now()
	}()

	// Phase A: natural quiet, no holds.
	limit := start.Add(g.phaseAMax)
	for {
		g.mu.Lock()
		now := time.Now()
		closed := g.closed
		okLocal, atLocal := g.localQuietLocked(e, now)
		okHolds, atHolds := g.holdsQuietLocked(now)
		changed := g.changedLocked()
		g.mu.Unlock()
		if closed {
			res.PhaseA = time.Since(start)
			err := g.abort(&res, ErrClosed)
			return res, err
		}
		if (okLocal && okHolds) || !now.Before(limit) {
			break
		}
		if !waitFor(ctx, changed, earliest(atLocal, atHolds), limit) {
			res.PhaseA = time.Since(start)
			err := g.abort(&res, ctx.Err())
			return res, err
		}
	}
	res.PhaseA = time.Since(start)

	// The hold token.
	asked := time.Now()
	h, err := g.acquire(ctx, e)
	res.TokenWait = time.Since(asked)
	if err != nil {
		err = g.abort(&res, err)
		return res, err
	}
	if h == nil {
		// Spec C8 residual: no token within the wait, so commit unheld.
		if err := g.stopErr(ctx); err != nil {
			err = g.abort(&res, err)
			return res, err
		}
		g.mu.Lock()
		g.stats.HoldsSkipped++
		g.mu.Unlock()
		holdSkippedTotal.Inc()
		res.Outcome, res.Committed = OutcomeHoldSkipped, true
		return res, commit()
	}
	res.HoldStart = h.start
	err = g.runHold(ctx, e, h, commit, &res)
	return res, err
}

// acquire takes the hold token for e in FIFO order, waiting at most the
// token wait; the hold starts at the grant. It returns (nil, nil) when the
// wait timed out and (nil, err) when ctx ended or the gate closed first.
func (g *Gate) acquire(ctx context.Context, e *entry) (*hold, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	if g.holder == nil && len(g.queue) == 0 {
		h := g.startHoldLocked(e, time.Now())
		g.mu.Unlock()
		return h, nil
	}
	w := &tokenWaiter{e: e, granted: make(chan struct{})}
	g.queue = append(g.queue, w)
	g.mu.Unlock()

	timer := time.NewTimer(g.tokenWait)
	defer timer.Stop()
	var cause error
	select {
	case <-w.granted:
		return w.hold, nil
	case <-timer.C:
	case <-ctx.Done():
		cause = ctx.Err()
	case <-g.closedCh:
		cause = ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.hold != nil {
		// Granted as it gave up: take it, or pass it straight on.
		if cause == nil {
			return w.hold, nil
		}
		g.endHoldLocked(w.hold, false)
		return nil, cause
	}
	for i, q := range g.queue {
		if q == w {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			break
		}
	}
	return nil, cause
}

// runHold is phase B for hold h of entry e. Its deferred end releases the
// hold, its waiters and the token whatever happens, a panicking commit
// included (spec C5: the barrier releases its pending state through its
// own defer).
func (g *Gate) runHold(ctx context.Context, e *entry, h *hold, commit func() error, res *Result) error {
	expiry := h.start.Add(g.holdMax)
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock() // a panicking commit unwinds to the adoption's recover
		g.endHoldLocked(h, false)
		res.HoldEnd, res.Waiters = h.end, h.heldTotal
		if h.expired && res.Outcome == OutcomeHeld {
			res.Outcome = OutcomeHoldExpired
		}
	}()
	for {
		g.mu.Lock()
		now := time.Now()
		closed := g.closed
		okLocal, atLocal := g.localQuietLocked(e, now)
		okOthers, atOthers := g.otherHoldsQuietLocked(e, now)
		changed := g.changedLocked()
		g.mu.Unlock()
		if closed {
			return g.abort(res, ErrClosed)
		}
		if !now.Before(expiry) {
			// Spec C8: at the bound pending is cleared, the waiters are
			// answered from the current snapshot, and a commit not yet
			// started starts at once (counted).
			if g.expireHold(h) {
				holdExpiredTotal.Inc()
			}
			if err := g.stopErr(ctx); err != nil {
				return g.abort(res, err)
			}
			res.Outcome, res.Committed = OutcomeHoldExpired, true
			return commit()
		}
		if okLocal && okOthers {
			break
		}
		if !waitFor(ctx, changed, earliest(atLocal, atOthers), expiry) {
			return g.abort(res, ctx.Err())
		}
	}
	if err := g.stopErr(ctx); err != nil {
		return g.abort(res, err)
	}
	// Commit within the hold. A commit that outlasts the bound is never
	// interrupted, but the hold still ends at the bound.
	expire := time.AfterFunc(time.Until(expiry), func() {
		defer func() {
			if recover() != nil {
				gatePanicsTotal.Inc() // a gate bug: counted; the deferred unlock kept the gate usable
			}
		}()
		if g.expireHold(h) {
			holdExpiredTotal.Inc()
		}
	})
	defer expire.Stop()
	res.Outcome, res.Committed = OutcomeHeld, true // the deferred end makes it hold_expired if the bound came first
	err := commit()
	expire.Stop() // design review M11: an expiry the timer has not fired by now did not happen
	return err
}

// expireHold ends h at its bound (runHold's loop and its timer) and
// reports whether this call ended it.
func (g *Gate) expireHold(h *hold) bool {
	g.mu.Lock()
	defer g.mu.Unlock() // the timer recovers a panic: never leave the gate locked
	if g.onExpire != nil {
		g.onExpire()
	}
	return g.endHoldLocked(h, true)
}

// stopErr reports why a flip must not start its commit: ErrClosed once the
// gate is closed, else ctx's error (nil while both last).
func (g *Gate) stopErr(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrClosed
	}
	return ctx.Err()
}

// abort records a flip abandoned before its commit.
func (g *Gate) abort(res *Result, cause error) error {
	if g != nil {
		g.mu.Lock()
		g.stats.Aborted++
		g.mu.Unlock()
	}
	res.Outcome, res.Committed = OutcomeAborted, false
	return fmt.Errorf("%w: %w", ErrAborted, cause)
}

func (g *Gate) entryLocked(key string, now time.Time) *entry {
	g.sweepLocked(now)
	e := g.entries[key]
	if e == nil {
		e = &entry{inflight: map[*Token]struct{}{}}
		g.entries[key] = e
	}
	e.lastUse = now
	return e
}

// sweepLocked drops entries idle for twice the quiet period (spec C8), at
// most once per quiet period. An idle entry's last completion is older
// than the quiet period, so dropping it loses nothing a flip waits for.
func (g *Gate) sweepLocked(now time.Time) {
	if now.Sub(g.lastSweep) < g.quiet {
		return
	}
	g.lastSweep = now
	for k, e := range g.entries {
		if len(e.inflight) == 0 && e.hold == nil && e.flips == 0 && now.Sub(e.lastUse) >= 2*g.quiet {
			delete(g.entries, k)
		}
	}
}

func (g *Gate) registerLocked(e *entry, now time.Time) *Token {
	t := &Token{g: g, e: e, start: now}
	e.inflight[t] = struct{}{}
	e.lastUse = now
	return t
}

func (g *Gate) startHoldLocked(e *entry, now time.Time) *hold {
	h := &hold{e: e, start: now, released: make(chan struct{})}
	g.holder, e.hold = h, h
	g.stats.Holds++
	return h
}

// endHoldLocked ends h once and reports whether this call did: the
// suite's anchors are no longer held, the waiters proceed (reading the
// snapshot current then), and the token passes to the next waiting flip,
// whose hold starts now — unless the gate is closed.
//
// AIDEV-NOTE: lastEnd/lastEndBy/prevEnd answer "the most recent hold end
// by another suite" in O(1): holds never overlap, so the newest end by an
// entry other than x is lastEnd when lastEndBy != x, and prevEnd if not.
// Only a hold that held at least one request sets them: a hold that held
// no request delayed no client queue, so it never starts a cross-suite
// quiet period (design note 17).
func (g *Gate) endHoldLocked(h *hold, expired bool) bool {
	if h.ended {
		return false
	}
	now := time.Now()
	h.ended, h.expired, h.end = true, expired, now
	if expired {
		g.stats.HoldsExpired++
	}
	if h.e.hold == h {
		h.e.hold = nil
	}
	h.e.lastUse = now
	close(h.released)
	if h.heldTotal > 0 {
		if g.lastEndBy != h.e {
			g.prevEnd = g.lastEnd
		}
		g.lastEnd, g.lastEndBy = now, h.e
	}
	if g.holder == h {
		g.holder = nil
		if len(g.queue) > 0 && !g.closed {
			w := g.queue[0]
			g.queue = g.queue[1:]
			w.hold = g.startHoldLocked(w.e, now)
			close(w.granted)
		}
	}
	g.broadcastLocked()
	return true
}

// changedLocked returns the channel the next change closes, creating it on
// demand: only a flip about to wait asks for it, so a change no flip waits
// for (most anchor completions) allocates nothing.
func (g *Gate) changedLocked() <-chan struct{} {
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

func (g *Gate) broadcastLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// localQuietLocked: no anchor of e in flight (ignoring any in flight for
// AnchorTimeout or longer) and none completed within the quiet period.
// If not, it also returns when to look again (zero: at the next change).
func (g *Gate) localQuietLocked(e *entry, now time.Time) (bool, time.Time) {
	var youngest time.Time
	for t := range e.inflight {
		if now.Sub(t.start) < AnchorTimeout && t.start.After(youngest) {
			youngest = t.start
		}
	}
	if !youngest.IsZero() {
		return false, youngest.Add(AnchorTimeout)
	}
	if !e.lastDone.IsZero() && now.Sub(e.lastDone) < g.quiet {
		return false, e.lastDone.Add(g.quiet)
	}
	return true, time.Time{}
}

// holdsQuietLocked is phase A (b): no hold in progress anywhere and none
// that held requests ended within the quiet period — such a hold delayed
// other suites' requests on a client's shared queue.
func (g *Gate) holdsQuietLocked(now time.Time) (bool, time.Time) {
	if g.holder != nil {
		return false, time.Time{}
	}
	if !g.lastEnd.IsZero() && now.Sub(g.lastEnd) < g.quiet {
		return false, g.lastEnd.Add(g.quiet)
	}
	return true, time.Time{}
}

// otherHoldsQuietLocked is phase B (b): no other suite's hold that held
// requests ended within the quiet period — clients released from it may
// still have this suite's requests queued behind the held one.
func (g *Gate) otherHoldsQuietLocked(e *entry, now time.Time) (bool, time.Time) {
	end := g.lastEnd
	if g.lastEndBy == e {
		end = g.prevEnd
	}
	if !end.IsZero() && now.Sub(end) < g.quiet {
		return false, end.Add(g.quiet)
	}
	return true, time.Time{}
}

// earliest returns the earlier of a and b, ignoring a zero time.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero(), a.Before(b):
		return a
	}
	return b
}

// waitFor blocks until changed closes, the earlier of at (when set) and
// limit comes, or ctx ends; it reports false only when ctx ended.
func waitFor(ctx context.Context, changed <-chan struct{}, at, limit time.Time) bool {
	timer := time.NewTimer(time.Until(earliest(at, limit)))
	defer timer.Stop()
	select {
	case <-changed:
		return true
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/flipgate/`
Expected: `ok`. Then `go vet ./internal/flipgate/ && golangci-lint run ./internal/flipgate/` — clean.

- [ ] **Step 6: Mutation check (do not commit)**

One at a time, reverting each (each was verified against this plan's code). Several kill their test with `t.Fatal` while bubble goroutines still wait, so the run also reports `panic: deadlock: main bubble goroutine has exited but blocked goroutines remain` — still a failure.

(a) in `Done`, record every completion: change `if (!late || t.e.hold == nil) && now.After(t.e.lastDone) {` to `if now.After(t.e.lastDone) {` → `TestFlip_IgnoresAnchorsInFlightPastAptsTimeout` FAILS ("a completion after apt's timeout, during a hold, delayed the next flip by 10s"); (b) in `localQuietLocked`, delete the `if !youngest.IsZero() { … }` block → `TestFlip_WaitsForQuietAfterTheLastAnchor` FAILS (commits while the anchor is in flight); (c) in `acquire`, change `if g.holder == nil && len(g.queue) == 0 {` to `if true {` (a non-exclusive token) → `TestFlip_HoldsAreExclusiveFIFOAndQuietAfterEachOther` FAILS; (d) make `otherHoldsQuietLocked` return `true, time.Time{}` first → the same test FAILS ("b committed 0s after a's hold ended"); (e) make `holdsQuietLocked` return `true, time.Time{}` first → `TestFlip_PhaseAWaitsOutAnotherSuitesHold` FAILS (b holds its anchors 10 s); (f) in `runHold`, arm the expiry timer with `time.Hour` instead of `time.Until(expiry)` → `TestFlip_SlowCommitEndsTheHoldAt20s` FAILS (the hold ends only with the commit, at 25 s: the released request sees generation 2 or, when its own 20 s wait ends at that same instant, the result shows a hold that ended at 25 s); (g) in `endHoldLocked`, grant the newest waiter (LIFO): `w := g.queue[len(g.queue)-1]` and `g.queue = g.queue[:len(g.queue)-1]` instead of `g.queue[0]` / `g.queue[1:]` → `TestFlip_HoldsAreExclusiveFIFOAndQuietAfterEachOther` FAILS ("b's hold started at …, want a's hold end …"; changing only the index hangs the test until `-timeout`); (h) in `join`, drop `|| h.waiting >= MaxWaiters` → `TestAnchorStart_WaiterCap` FAILS (257 held requests); (i) in `AnchorStart`, return `g.registerLocked(e, time.Now())` when `left` → `TestAnchorStart_CancelledWaiterLeavesUncounted` FAILS; (j) delete `g.endHoldLocked(h, false)` from `runHold`'s deferred function → `TestFlip_CommitPanicEndsTheHold` and the `phase B` case of `TestFlip_ContextEndAbortsEveryPhase` FAIL (the held request waits 20 s); (k) in `sweepLocked`, use `>= g.quiet` instead of `>= 2*g.quiet` → `TestGate_IdleEntriesExpire` FAILS; (l) in `runHold`'s expiry branch, return `nil` instead of `commit()` → `TestFlip_SlowReaderExpiresTheHoldAt20s` FAILS.

Added by the review revision: (m) in `Done`, never record a late completion: `if !late && now.After(t.e.lastDone) {` → `TestFlip_IgnoresAnchorsInFlightPastAptsTimeout` FAILS ("… outside a hold, delayed the next flip by 0s, want 10s"); (n) in `acquire`, delete the `if w.hold != nil { … }` block ("Granted as it gave up") → `TestFlip_GrantAtTheTokenTimeoutNeverLeaksTheHold` FAILS ("round N: b hold_skipped; holder … after both flips returned", within the first few rounds); (o) in the expiry timer's callback in `runHold`, replace `if g.expireHold(h) { holdExpiredTotal.Inc() }` with `_ = g.expireHold(h)` → `TestFlip_SlowCommitExpiryIsCounted` FAILS ("acu_flip_hold_expired_total rose by 0, want 1"); (p) in `sweepLocked`, drop `e.flips == 0 &&` → `TestGate_SweepKeepsTheEntryOfAFlipInProgress` FAILS ("k committed -10s after the read it must wait for ended"); (q) in `Done`, delete `t.e.lastUse = now` → `TestGate_SweepKeepsAJustFinishedSlowRead` FAILS ("flip waited 0s, want 9s"); (r) in `AnchorStart`, delete `h.waiting--` → `TestAnchorStart_WaiterCapCountsOnlyCurrentWaiters` FAILS ("a request was served during the hold with only 255 others waiting"); (s) in `join`, delete `waitersOverCapTotal.Inc()` → `TestAnchorStart_WaiterCap` FAILS ("acu_flip_waiters_over_cap_total rose by 0, want 1"); (t) in `Done`, delete the `if late { slowAnchorReadsTotal.Inc() }` block → `TestFlip_IgnoresAnchorsInFlightPastAptsTimeout` FAILS ("acu_flip_slow_anchor_reads_total rose by 0, want 2"); (u) in `endHoldLocked`, drop the `if h.heldTotal > 0 {` guard (update `prevEnd`, `lastEnd` and `lastEndBy` after every hold) → `TestFlip_HoldsThatHeldNothingDoNotDelayOtherFlips` FAILS ("b: phase A 9s, committed after 9s"); (v) in `Close`, delete the `if g.holder != nil { … }` block → `TestGate_CloseAbortsEveryFlipNotYetCommitting` FAILS ("the held request was answered 18s after Close"); (w) in `acquire`, delete the `case <-g.closedCh:` arm → the same test FAILS ("queued for the token: … 59s after Close"); (x) in `Flip`'s phase A loop, change `if closed {` to `if false && closed {` → the same test FAILS ("in phase A: … 500ms after Close"); (y) in `endHoldLocked`, drop `&& !g.closed` → the same test FAILS (`Holds:2`); (z) in `expireHold`, unlock without the defer (`ended := g.endHoldLocked(h, true)`, `g.mu.Unlock()`, `return ended`) → `TestFlip_PanicInTheExpiryTimerLeavesTheGateUsable` FAILS ("a panic in the expiry timer left the gate locked"; the test unlocks the mutex itself, so it does not hang); (aa) in the expiry timer's callback, replace the `if recover() != nil { gatePanicsTotal.Inc() … }` block with `_ = recover()` → the same test FAILS ("acu_flip_gate_panics_total rose by 0, want 1"); (bb) set `MaxQuiet = 20 * time.Second` → `TestNew_QuietBounds` FAILS ("MaxQuiet 20s must stay below HoldMax 20s"); (cc) in `New`, accept any quiet period up to `MaxQuiet` (`if q := cfg.Quiet; q < 0 || q > MaxQuiet {`) → `TestNew_QuietBounds` FAILS ("Quiet 1ns: err = <nil>, want ErrQuietOutOfRange"); (dd) make `changedLocked` return a fresh `make(chan struct{})` without storing it → `TestFlip_WaitsForQuietAfterTheLastAnchor` FAILS ("flip returned after 30s, want 14s").

Not tested (equivalent or unreachable, from the executability review): the `ctx` checks at a same-instant shutdown and token timeout, the per-waiter `time.NewTimer(g.holdMax)` (defence in depth while every hold ends), M11's `expire.Stop()` before the hold's end (a same-instant race), and the absence of panics in `endHoldLocked` (it cannot panic: `h.ended` guards `close(h.released)`, and each waiter is popped before its `granted` channel is closed).

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/flipgate/
git add internal/flipgate/
git commit -m "Add the flip barrier: phase A, a FIFO hold token, and a hold bounded at 20 s"
```

---

### Task 2: The straddle ledger and barrier simulations (phase C DoD, model)

Spec §7 C8 ("the overlapping-hold trace … zero straddles"; "hold-token timeout"), the overview's "C:" DoD simulations, plan deviation 2 and the straddle definition above. These simulations drive the gate with a model of the cache (generation counters) so their timing is exact; Task 10 repeats the shared-queue trace end to end through the real handler and adopter.

**Files:**
- Create: `internal/simtest/aptclient.go`, `internal/simtest/aptclient_test.go`, `internal/flipgate/sim_test.go`

**Interfaces:**
- Consumes: Task 1 (`Gate`, `New`, `Result`, `Outcome*`, `PhaseAMax`, `TokenWaitMax`, `HoldMax`, and the test helpers `testQuiet`, `bg`, `newTestGate`, `flipOut`, `flipAsync`, `counterValue`).
- Produces: `simtest.Request{Suite string; Anchor bool; Filler time.Duration}`, `simtest.ServeFunc` (`func(Request) (gen string, held time.Duration)`), `simtest.Straddle{Client int; Suite, AnchorGen, IndexGen string; Delay time.Duration}`, `simtest.NewLedger(window) *Ledger`, `(*Ledger).RunUpdate(client int, queue []Request, serve ServeFunc)`, `.Straddles() []Straddle`, `.Checked() int`, `.AnchorGens(suite string) []string` (Task 10 uses all of them).

- [ ] **Step 1: Write the failing tests**

`internal/simtest/aptclient_test.go`:

```go
package simtest

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// A straddle is judged net of the time the client's queue spent on held
// responses: a's index request goes out 15 s after a's anchor arrived,
// all of it behind b's held anchor, so it is still inside the window.
func TestLedger_CountsStraddlesInsideTheWindowNetOfHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gens := map[string]string{"a": "1", "b": "1"}
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor && rq.Suite == "b" {
				time.Sleep(15 * time.Second) // held while a flips
				gens["a"] = "2"
				return gens["b"], 15 * time.Second
			}
			return gens[rq.Suite], 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(7, []Request{{Suite: "a", Anchor: true}, {Suite: "b", Anchor: true}}, serve)
		s := l.Straddles()
		want := Straddle{Client: 7, Suite: "a", AnchorGen: "1", IndexGen: "2", Delay: 0}
		if len(s) != 1 || s[0] != want {
			t.Fatalf("straddles = %+v, want [%+v]", s, want)
		}
		if l.Checked() != 2 || !slices.Equal(l.AnchorGens("a"), []string{"1"}) {
			t.Fatalf("checked %d, a's anchor generations %v; want 2 and [1]", l.Checked(), l.AnchorGens("a"))
		}
	})
}

// An index request sent after the window (here behind the client's own
// 11 s download) is outside the guarantee and is not checked; so is one
// sent exactly at its end, which races a commit made at that instant.
func TestLedger_IgnoresIndexesOutsideTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor {
				return "1", 0
			}
			return "2", 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(1, []Request{{Suite: "a", Anchor: true}, {Filler: 11 * time.Second}}, serve)
		l.RunUpdate(3, []Request{{Suite: "a", Anchor: true}, {Filler: 10 * time.Second}}, serve)
		if l.Checked() != 0 || len(l.Straddles()) != 0 {
			t.Fatalf("checked %d, straddles %v; want neither", l.Checked(), l.Straddles())
		}
		l.RunUpdate(2, []Request{{Suite: "a", Anchor: true}, {Filler: 9 * time.Second}}, serve)
		if l.Checked() != 1 || len(l.Straddles()) != 1 {
			t.Fatalf("checked %d, straddles %v; want one of each", l.Checked(), l.Straddles())
		}
	})
}
```

`internal/flipgate/sim_test.go`:

```go
package flipgate

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// gateModel is the cache as the barrier sees it (spec C8): suites whose
// generation a commit bumps; anchor serves that register with the gate
// before reading the generation (the handler's order) and stay in flight
// for their transfer; index serves that never wait.
type gateModel struct {
	g    *Gate
	gens map[string]*atomic.Int64
}

func newGateModel(g *Gate, keys ...string) *gateModel {
	m := &gateModel{g: g, gens: map[string]*atomic.Int64{}}
	for _, k := range keys {
		m.gens[k] = &atomic.Int64{}
		m.gens[k].Store(1)
	}
	return m
}

// serve answers ledger requests; an anchor of key takes anchor[key] to
// send, an index 10 ms.
func (m *gateModel) serve(anchor map[string]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen := m.gens[rq.Suite]
		if !rq.Anchor {
			v := gen.Load()
			time.Sleep(10 * time.Millisecond)
			return strconv.FormatInt(v, 10), 0
		}
		start := time.Now()
		tok := m.g.AnchorStart(bg, rq.Suite)
		held := time.Since(start)
		v := gen.Load()
		time.Sleep(anchor[rq.Suite])
		tok.Done()
		return strconv.FormatInt(v, 10), held
	}
}

// commit is a flip's commit: the next generation becomes current.
func (m *gateModel) commit(key string) func() error {
	return func() error { m.gens[key].Add(1); return nil }
}

// clients starts, every period from offset until until, a client that
// runs one update of queue.
func clients(l *simtest.Ledger, serve simtest.ServeFunc, queue []simtest.Request, offset, period time.Duration, until time.Time, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(offset)
		for i := 0; time.Now().Before(until); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.RunUpdate(i, queue, serve)
			}()
			time.Sleep(period)
		}
	}()
}

// recordHeld wraps serve and records how long each anchor was held.
func recordHeld(serve simtest.ServeFunc, held *[]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen, h := serve(rq)
		if rq.Anchor {
			*held = append(*held, h)
		}
		return gen, h
	}
}

// Spec C8 DoD, the review's overlapping-hold trace (plan review
// 2026-10-05, finding 6) in a shared-queue simulation. Busy suites A and
// B leave phase A at its cap 0.1 s apart. An apt client whose single
// queue serves both (mirror mode) receives A's old anchor just before
// A's hold starts, spends 0.3 s on an unrelated download, then asks for
// B's anchor with A's index queued behind it. Under concurrent holds B
// would hold that request until B committed — after A — and the client
// would fetch A's new index for A's old anchor ("another existing B
// anchor response" on a slow link keeps B from committing early). With
// one hold at a time B is not holding yet, so neither of the client's
// anchors waits and its A index arrives in time. Many more shared-queue
// clients run across both flips.
func TestSimFlip_OverlappingHoldTrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		m := newGateModel(g, "A", "B")
		fast := m.serve(map[string]time.Duration{"A": 50 * time.Millisecond, "B": 50 * time.Millisecond})
		l := simtest.NewLedger(testQuiet)
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(111 * time.Second)
		clients(l, fast, []simtest.Request{{Suite: "A", Anchor: true}}, 11*time.Millisecond, 500*time.Millisecond, until, &wg)
		clients(l, fast, []simtest.Request{{Suite: "B", Anchor: true}}, 261*time.Millisecond, 500*time.Millisecond, until, &wg)
		wg.Add(1)
		go func() { // another B reader, on a slow link, still reading when B's hold could start
			defer wg.Done()
			time.Sleep(60950 * time.Millisecond)
			l.RunUpdate(1000, []simtest.Request{{Suite: "B", Anchor: true}}, m.serve(map[string]time.Duration{"B": 6 * time.Second}))
		}()
		var traceHeld []time.Duration
		wg.Add(1)
		go func() { // the trace client
			defer wg.Done()
			time.Sleep(60853 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: "A", Anchor: true}, {Filler: 300 * time.Millisecond}, {Suite: "B", Anchor: true}},
				recordHeld(fast, &traceHeld))
		}()
		for i := 0; i < 150; i++ { // shared-queue clients, both orders
			q := []simtest.Request{{Suite: "A", Anchor: true}, {Suite: "B", Anchor: true}}
			if i%2 == 1 {
				q[0], q[1] = q[1], q[0]
			}
			at := 51*time.Second + 7*time.Millisecond + time.Duration(i)*300*time.Millisecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				l.RunUpdate(3000+i, q, fast)
			}()
		}
		time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
		outA := flipAsync(bg, g, "A", m.commit("A"))
		time.Sleep(100 * time.Millisecond)
		outB := flipAsync(bg, g, "B", m.commit("B"))
		a, b := <-outA, <-outB
		wg.Wait()

		if a.err != nil || b.err != nil || a.res.Outcome != OutcomeHeld || b.res.Outcome != OutcomeHeld {
			t.Fatalf("A: %+v, %v; B: %+v, %v; want both held", a.res, a.err, b.res, b.err)
		}
		if a.res.PhaseA != PhaseAMax || b.res.PhaseA != PhaseAMax {
			t.Fatalf("phase A took %v and %v, want both to give up at %v (busy suites)", a.res.PhaseA, b.res.PhaseA, PhaseAMax)
		}
		if b.res.HoldStart != a.res.HoldEnd || b.at.Sub(a.res.HoldEnd) < testQuiet {
			t.Fatalf("B held from %v and committed %v after A's hold ended (%v); want B's hold to start then and commit a quiet period later",
				b.res.HoldStart, b.at.Sub(a.res.HoldEnd), a.res.HoldEnd)
		}
		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] != 0 {
			t.Fatalf("the trace client's anchors were held %v, want neither", traceHeld)
		}
		if s := l.Straddles(); len(s) != 0 {
			t.Fatalf("%d straddles, first %+v", len(s), s[0])
		}
		for _, k := range []string{"A", "B"} {
			if gens := l.AnchorGens(k); !slices.Equal(gens, []string{"1", "2"}) {
				t.Errorf("%s: clients saw anchor generations %v, want [1 2] (they crossed the flip)", k, gens)
			}
		}
		if n := l.Checked(); n < 400 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}

// Spec C8 phase B (b): a client released from another suite's hold may
// still have this suite's index queued behind the held request. A's hold
// lasts ~15 s (its readers are on 6 s links); B leaves phase A at its cap
// while A holds, so it waits for the token. The trace client gets B's old
// anchor, then waits in A's hold for A's anchor, then asks for B's index
// — 18.5 s after B's anchor arrived, 12.5 s of that in A's hold. B is
// quiet by itself when A's hold ends; only the cross-suite quiet period
// keeps it from committing before that index arrives.
func TestSimFlip_ClientReleasedFromAnotherHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		m := newGateModel(g, "A", "B")
		serve := m.serve(map[string]time.Duration{"A": 6 * time.Second, "B": 50 * time.Millisecond})
		l := simtest.NewLedger(testQuiet)
		var wg sync.WaitGroup
		t0 := time.Now()
		clients(l, serve, []simtest.Request{{Suite: "A", Anchor: true}}, 11*time.Millisecond, 500*time.Millisecond, t0.Add(100*time.Second), &wg)
		clients(l, serve, []simtest.Request{{Suite: "B", Anchor: true}}, 261*time.Millisecond, 500*time.Millisecond, t0.Add(61*time.Second), &wg)
		var traceHeld []time.Duration
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(64003 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: "B", Anchor: true}, {Suite: "A", Anchor: true}}, recordHeld(serve, &traceHeld))
		}()
		time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
		outA := flipAsync(bg, g, "A", m.commit("A")) // holds from 61.003 s to 76.511 s
		time.Sleep(2 * time.Second)
		outB := flipAsync(bg, g, "B", m.commit("B")) // phase A gives up at 63.003 s, while A holds
		a, b := <-outA, <-outB
		wg.Wait()

		if a.err != nil || b.err != nil {
			t.Fatalf("A: %v; B: %v", a.err, b.err)
		}
		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] == 0 {
			t.Fatalf("the trace client's anchors were held %v, want B's not held and A's held", traceHeld)
		}
		if b.res.HoldStart != a.res.HoldEnd || b.at.Sub(a.res.HoldEnd) != testQuiet {
			t.Fatalf("B held from %v and committed %v after A's hold ended; want its hold to start at A's end and commit the quiet period later",
				b.res.HoldStart, b.at.Sub(a.res.HoldEnd))
		}
		if s := l.Straddles(); len(s) != 0 {
			t.Fatalf("%d straddles, first %+v", len(s), s[0])
		}
	})
}

// Spec C8 DoD "five busy suites flipping within 0.1 s" (plan deviation
// 2). Each suite is read by a client every 500 ms over a slow link: an
// anchor takes 9.02 s to send, so a hold waits ~9 s for the reads in
// flight plus the 10 s quiet period (~19 s, near the 20 s bound). No
// suite is ever quiet, so all five leave phase A at its 60 s cap, 20 ms
// apart, and queue for the hold token in that order. Holds never overlap
// and end normally; four fit inside the fifth's 60 s token wait (three
// holds of under 20 s cannot exhaust a fourth's wait); the fifth commits
// unheld at its deadline and is counted; held suites never straddle.
func TestSimFlip_FiveBusySuites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newTestGate(t)
		keys := []string{"s1", "s2", "s3", "s4", "s5"}
		m := newGateModel(g, keys...)
		slow := map[string]time.Duration{}
		for _, k := range keys {
			slow[k] = 9020 * time.Millisecond
		}
		serve := m.serve(slow)
		l := simtest.NewLedger(testQuiet)
		skipped := counterValue(t, "acu_flip_hold_skipped_total")
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(150 * time.Second)
		for i, k := range keys {
			clients(l, serve, []simtest.Request{{Suite: k, Anchor: true}}, time.Duration(13*(i+1))*time.Millisecond, 500*time.Millisecond, until, &wg)
		}
		outs := make([]<-chan flipOut, len(keys))
		starts := make([]time.Time, len(keys))
		for i, k := range keys {
			time.Sleep(time.Until(t0.Add(time.Second + time.Duration(5+20*i)*time.Millisecond))) // the suites are busy by now
			starts[i] = time.Now()
			outs[i] = flipAsync(bg, g, k, m.commit(k))
		}
		res := make([]Result, len(keys))
		ends := make([]time.Time, len(keys))
		for i, ch := range outs {
			o := <-ch
			if o.err != nil {
				t.Fatalf("%s: %v", keys[i], o.err)
			}
			res[i], ends[i] = o.res, o.at
		}
		wg.Wait()

		for i := 0; i < 4; i++ {
			if res[i].Outcome != OutcomeHeld {
				t.Errorf("%s: outcome %s, want held", keys[i], res[i].Outcome)
			}
			if d := res[i].HoldEnd.Sub(res[i].HoldStart); d <= 0 || d > HoldMax {
				t.Errorf("%s held %v, want (0, %v]", keys[i], d, HoldMax)
			}
			if i > 0 && res[i].HoldStart.Before(res[i-1].HoldEnd) {
				t.Errorf("holds overlap: %s from %v, %s until %v", keys[i], res[i].HoldStart, keys[i-1], res[i-1].HoldEnd)
			}
		}
		r5 := res[4]
		if r5.Outcome != OutcomeHoldSkipped || r5.TokenWait != TokenWaitMax || !r5.HoldStart.IsZero() {
			t.Fatalf("s5: %+v, want an unheld commit after the %v token wait", r5, TokenWaitMax)
		}
		if got := ends[4].Sub(starts[4]); got != PhaseAMax+TokenWaitMax {
			t.Errorf("s5 committed %v after its flip started, want %v", got, PhaseAMax+TokenWaitMax)
		}
		if got := ends[4].Sub(res[0].HoldStart); got > TokenWaitMax+HoldMax {
			t.Errorf("s5 committed %v after the first hold started, want at most %v", got, TokenWaitMax+HoldMax)
		}
		for i, k := range keys {
			if d := ends[i].Sub(starts[i]); d > PhaseAMax+TokenWaitMax+HoldMax {
				t.Errorf("%s's flip took %v, more than %v", k, d, PhaseAMax+TokenWaitMax+HoldMax)
			}
		}
		if st := g.Stats(); st.Holds != 4 || st.HoldsSkipped != 1 || st.HoldsExpired != 0 {
			t.Errorf("stats = %+v, want 4 holds, 1 skipped, 0 expired", st)
		}
		if got := counterValue(t, "acu_flip_hold_skipped_total") - skipped; got != 1 {
			t.Errorf("acu_flip_hold_skipped_total rose by %v, want 1", got)
		}
		for _, s := range l.Straddles() {
			if s.Suite != "s5" {
				t.Errorf("held suite %s straddled: %+v", s.Suite, s)
			}
		}
		for _, k := range keys[:4] {
			if gens := l.AnchorGens(k); !slices.Equal(gens, []string{"1", "2"}) {
				t.Errorf("%s: clients saw anchor generations %v, want [1 2]", k, gens)
			}
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/simtest/ ./internal/flipgate/`
Expected: compile failure naming undefined `NewLedger`, `Request`, `Straddle` (package `simtest`) and `simtest.ServeFunc`, `simtest.Request`, `simtest.NewLedger` (package `flipgate`).

- [ ] **Step 3: The ledger**

`internal/simtest/aptclient.go`:

```go
package simtest

import (
	"slices"
	"sync"
	"time"
)

// Request is one request in an apt client's queue: the anchor (InRelease)
// or the index of Suite, or — with Suite empty — a filler: an unrelated
// download that keeps the client's connection busy for Filler.
type Request struct {
	Suite  string
	Anchor bool
	Filler time.Duration
}

// ServeFunc answers one request once its response has fully arrived. It
// returns the response's snapshot generation (X-Cache-Snapshot) and how
// long the cache held the request before answering it (zero unless it
// waited in a flip hold).
type ServeFunc func(Request) (gen string, held time.Duration)

// Straddle is one index response whose generation differs from that of
// the anchor its client holds (spec C8).
type Straddle struct {
	Client    int
	Suite     string
	AnchorGen string
	IndexGen  string
	Delay     time.Duration // request sent this long after the anchor arrived, holds excluded
}

// Ledger runs apt client updates and counts straddles (spec C8).
//
// AIDEV-NOTE: the definitions every flip-consistency simulation relies on
// (phase C plan, "Definitions"); keep them in step with it:
//   - Client model (apt's): one connection, one request at a time (apt
//     keeps one connection and queue per host; beyond 10 hosts and in
//     mirror mode every source shares it). An update queues its anchors
//     and fillers; when an anchor response arrives, that suite's index
//     request joins the END of the queue.
//   - Straddle: an index response for suite S whose generation differs
//     from that of the anchor of S the same client received earlier in
//     the same update, when the index request was sent less than the
//     window (flip_quiet_period) after that anchor's arrival, not counting
//     time the client's queue spent on responses the cache held: a hold
//     must not push a client out of its guarantee. Later requests are not
//     checked; one sent exactly at the window's end races a commit made at
//     that instant (the gate is quiet once a full period has passed).
//   - Where the window starts: the gate measures quiet from when the cache
//     finished sending the anchor, this ledger from when the client
//     received it. In production kernel send buffering makes the first
//     earlier on a slow link (by at most the buffered bytes over the link's
//     bandwidth), so the effective window is slightly shorter than
//     flip_quiet_period; in the simulations the two instants coincide.
type Ledger struct {
	window time.Duration

	mu        sync.Mutex
	checked   int
	straddles []Straddle
	gens      map[string]map[string]bool // suite → anchor generations of checked pairs
}

// NewLedger returns a ledger whose guarantee window is window.
func NewLedger(window time.Duration) *Ledger {
	return &Ledger{window: window, gens: map[string]map[string]bool{}}
}

// RunUpdate runs one update for client, starting from queue.
func (l *Ledger) RunUpdate(client int, queue []Request, serve ServeFunc) {
	type arrival struct {
		gen  string
		at   time.Time
		held time.Duration // the update's held total when the anchor arrived
	}
	queue = slices.Clone(queue)
	anchors := map[string]arrival{}
	var held time.Duration
	for len(queue) > 0 {
		rq := queue[0]
		queue = queue[1:]
		if rq.Suite == "" {
			time.Sleep(rq.Filler)
			continue
		}
		sent, heldBefore := time.Now(), held
		gen, h := serve(rq)
		held += h
		if rq.Anchor {
			anchors[rq.Suite] = arrival{gen: gen, at: time.Now(), held: held}
			queue = append(queue, Request{Suite: rq.Suite})
			continue
		}
		if a, ok := anchors[rq.Suite]; ok {
			l.record(client, rq.Suite, a.gen, gen, sent.Sub(a.at)-(heldBefore-a.held))
		}
	}
}

func (l *Ledger) record(client int, suite, anchorGen, indexGen string, delay time.Duration) {
	if delay >= l.window {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checked++
	if l.gens[suite] == nil {
		l.gens[suite] = map[string]bool{}
	}
	l.gens[suite][anchorGen] = true
	if anchorGen != indexGen {
		l.straddles = append(l.straddles, Straddle{Client: client, Suite: suite, AnchorGen: anchorGen, IndexGen: indexGen, Delay: delay})
	}
}

// Straddles returns the straddles counted so far.
func (l *Ledger) Straddles() []Straddle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.straddles)
}

// Checked returns how many index responses fell inside the window.
func (l *Ledger) Checked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checked
}

// AnchorGens returns, sorted, the anchor generations of suite's checked
// pairs: two or more prove its clients crossed a flip.
func (l *Ledger) AnchorGens(suite string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for g := range l.gens[suite] {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}
```

- [ ] **Step 4: Run them**

Run: `go test -race -count=1 -run 'TestLedger|TestSimFlip' -v ./internal/simtest/ ./internal/flipgate/ 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: PASS, each simulation well under 5 s; then `go test -race -count=1 ./internal/simtest/ ./internal/flipgate/` — `ok` twice (`TestNoProductionCodeImportsSimtest` still passes: only test files import `simtest`).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `internal/flipgate/gate.go` `acquire`, change `if g.holder == nil && len(g.queue) == 0 {` to `if true {` (concurrent holds) → `TestSimFlip_OverlappingHoldTrace` FAILS (B's hold starts 0.1 s after A's instead of at its end; with that assertion commented out it fails on the trace client's B anchor being held ~20 s (`[0s 19.804s]`); with the `traceHeld` assertion commented out too, it reports the straddle `{Client:2000 Suite:A AnchorGen:1 IndexGen:2 Delay:350ms}` — the review's trace); (b) make `otherHoldsQuietLocked` return `true, time.Time{}` first → `TestSimFlip_ClientReleasedFromAnotherHold` FAILS (B commits 0 s after A's hold ends); (c) in `Flip`'s `h == nil` branch, insert `time.Sleep(time.Second)` before `return res, commit()` → `TestSimFlip_FiveBusySuites` FAILS ("s5 committed 2m1s after its flip started, want 2m0s"); (d) in `RunUpdate`, replace `sent.Sub(a.at)-(heldBefore-a.held)` with `sent.Sub(a.at)-0*(heldBefore-a.held)` → `TestLedger_CountsStraddlesInsideTheWindowNetOfHolds` FAILS (no straddle counted); (e) in `record`, make the check `delay > l.window` → `TestLedger_IgnoresIndexesOutsideTheWindow` FAILS (the exactly-10 s request is checked).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/simtest/ internal/flipgate/
git add internal/simtest/aptclient.go internal/simtest/aptclient_test.go internal/flipgate/sim_test.go
git commit -m "Simulate overlapping holds, cross-suite release and five busy suites with an apt client model"
```

---

### Task 3: `adoption.flip_quiet_period`

Spec C11: `adoption.flip_quiet_period`, default `"10s"`, `0s` disables the barrier, at most `15s` — and, by controller ruling (deviation 6), at least `1s` unless it is `0s`: a sub-second period protects almost no one. `0s` is meaningful, so the default is presence-sensitive (applied only when the key is absent), like `hot_prefetch_budget`. Task 9 wires it.

**Files:**
- Modify: `internal/config/config.go` (`AdoptionConfig`, `Load`, `Validate`), `internal/config/config_test.go`, `packaging/config/config.toml.default`

**Interfaces:**
- Consumes: existing test helper `writeTOML(t, dir, name, body string) string` (package `config`).
- Produces: `config.AdoptionConfig.FlipQuietPeriod Duration` (TOML `flip_quiet_period`).

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go` (after `TestAdoptionMaxDuration_DefaultAndBounds`):

```go
// adoption.flip_quiet_period (spec C11): default 10s, 0s disables the
// flip barrier and survives Load; otherwise 1s to 15s (controller ruling:
// a sub-second period gives almost no protection).
func TestAdoptionFlipQuietPeriod_DefaultAndBounds(t *testing.T) {
	load := func(extra string) (*Config, error) {
		dir := t.TempDir()
		return Load(writeTOML(t, dir, "config.toml", "[cache]\ndir = \""+dir+"\"\n"+extra))
	}
	cfg, err := load("")
	if err != nil || cfg.Adoption.FlipQuietPeriod.Duration != 10*time.Second {
		t.Fatalf("default = %v (err %v), want 10s", cfg.Adoption.FlipQuietPeriod.Duration, err)
	}
	for _, ok := range []struct {
		value string
		want  time.Duration
	}{{"0s", 0}, {"1s", time.Second}, {"15s", 15 * time.Second}, {"2500ms", 2500 * time.Millisecond}} {
		cfg, err := load("[adoption]\nflip_quiet_period = \"" + ok.value + "\"\n")
		if err != nil || cfg.Adoption.FlipQuietPeriod.Duration != ok.want {
			t.Errorf("flip_quiet_period = %s: got %v (err %v), want %v", ok.value, cfg.Adoption.FlipQuietPeriod.Duration, err, ok.want)
		}
	}
	for _, bad := range []string{"16s", "-1s", "500ms", "999ms"} {
		if _, err := load("[adoption]\nflip_quiet_period = \"" + bad + "\"\n"); err == nil {
			t.Errorf("flip_quiet_period = %s accepted, want a validation error", bad)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run TestAdoptionFlipQuietPeriod`
Expected: compile failure naming `cfg.Adoption.FlipQuietPeriod undefined (type AdoptionConfig has no field or method FlipQuietPeriod)`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to `AdoptionConfig` right after the `MaxDuration` field:

```go

	// FlipQuietPeriod is adoption.flip_quiet_period (spec C8, C11): how
	// long after the last client finished receiving a suite's anchor the
	// cache keeps the suite's snapshot before switching to a new one, so
	// that client's index requests still match the anchor it holds.
	// Default 10s; 0s disables the flip barrier (the previous behaviour);
	// otherwise between 1s and 15s.
	FlipQuietPeriod Duration `toml:"flip_quiet_period"`
```

In `Load`, right after the `max_duration` default (`if !md.IsDefined("adoption", "max_duration") { … }`):

```go
	// adoption.flip_quiet_period = 0s disables the flip barrier: a
	// meaningful value that must survive Load.
	if !md.IsDefined("adoption", "flip_quiet_period") {
		cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	}
```

In `Validate`, right after the `adoption.max_duration must be at least 1m` check:

```go
	// A sub-second quiet period gives almost no protection: 0s (off) or
	// 1s..15s (controller ruling, phase C plan).
	if q := c.Adoption.FlipQuietPeriod.Duration; q != 0 && (q < time.Second || q > 15*time.Second) {
		errs = append(errs, errors.New("adoption.flip_quiet_period must be 0s or between 1s and 15s"))
	}
```

(`Defaults()` leaves the field alone: programmatic configs — every `cmd` test's `minimalCfg` — get `0s`, the barrier off.)

In `packaging/config/config.toml.default`, in the `[adoption]` block right after the `max_duration` line (`TestDefaultConfigFileDocumentsEveryKey` requires every key there):

```toml
# How long the cache keeps a suite's snapshot after a client last finished
# receiving its InRelease/Release before switching to a new snapshot, so
# that client's index requests still match. A busy suite's new
# InRelease/Release requests are held instead, one suite at a time, for at
# most 20 s (below apt's 30 s timeout). 0s switches at once; otherwise
# between 1s and 15s.
flip_quiet_period     = "10s"
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/config/`
Expected: PASS (including `TestDefaultConfigFileDocumentsEveryKey`).

- [ ] **Step 5: Mutation check (do not commit)**

(a) In `Validate`, accept any period up to 15 s (`if q := c.Adoption.FlipQuietPeriod.Duration; q < 0 || q > 15*time.Second {`) → `TestAdoptionFlipQuietPeriod_DefaultAndBounds` FAILS ("flip_quiet_period = 500ms accepted, want a validation error").

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/config/
git add internal/config/config.go internal/config/config_test.go packaging/config/config.toml.default
git commit -m "Add adoption.flip_quiet_period (default 10s; 0s disables; otherwise 1s to 15s)"
```

---

### Task 4: Monotonic `adopted_at` and the displaced-member lookup

Spec C8 "Monotonic `Last-Modified`" (`adopted_at = max(now, prior.adopted_at + 1)`; a first adoption `max(now, url_path anchor last_fetched_at + 1)`) and "Retained by-hash" (the cache half: the suite's other adopted, non-retired snapshots, newest first, at most `gc.keep_displaced + 1`). Deferred minors B-7 (`internal/cache/adoption.go`) and B-6 (`internal/cache/flip_state_test.go`), both in files this task touches. The executability review's P6 (each served anchor file counts on its own).

**Files:**
- Modify: `internal/cache/adoption.go` (`CommitAdoption`; new `adoptionStampTx`, `DisplacedSnapshotMembers`; B-7), `internal/cache/flip_state_test.go` (new tests; B-6), `internal/gc/integration_test.go` (comment only)
- Create: `internal/cache/displaced_members_test.go`

**Interfaces:**
- Consumes (existing test helpers in package `cache`): `openCache(t)`, `stubNow(t, ts int64) (restore func())`, `seedBlob(t, c, content string) string`, `commitCandidate(t, c, sc SnapshotCandidate, content string) int64` (`flip_state_test.go`; suite `afSuite` = `/r/dists/x` on host `h`), `afSuite`.
- Produces: `(*Cache).DisplacedSnapshotMembers(ctx, scheme, host, suitePath, path string, currentID int64, limit int) ([]SnapshotMember, error)`; unexported `adoptionStampTx(ctx, tx, scheme, host, suite string, now int64) (int64, error)`; `CommitAdoption` stamps the monotonic `adopted_at`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cache/flip_state_test.go`:

```go
func adoptedAtOf(t *testing.T, c *Cache, id int64) int64 {
	t.Helper()
	snap, err := c.GetSuiteSnapshot(context.Background(), id)
	if err != nil || snap.AdoptedAt == nil {
		t.Fatalf("snapshot %d: adopted_at %v (err %v)", id, snap, err)
	}
	return *snap.AdoptedAt
}

// Spec C8 monotonic Last-Modified: adopted_at = max(now, prior + 1), so
// two flips in one second, or one after a backward clock step, still move
// it forward; a forward step is taken as is.
func TestCommitAdoption_AdoptedAtIsMonotonic(t *testing.T) {
	c := openCache(t)
	stamp := func(now int64, content string) int64 {
		restore := stubNow(t, now)
		defer restore()
		return adoptedAtOf(t, c, commitCandidate(t, c, SnapshotCandidate{}, content))
	}
	got := []int64{
		stamp(1_000_000, "monotonic one"),
		stamp(1_000_000, "monotonic two"),  // same second
		stamp(500_000, "monotonic three"),  // the clock stepped back
		stamp(2_000_000, "monotonic four"), // and forward
	}
	if want := []int64{1_000_000, 1_000_001, 1_000_002, 2_000_000}; !slices.Equal(got, want) {
		t.Fatalf("adopted_at = %v, want %v", got, want)
	}
}

// A first adoption must pass the anchors clients were served from the
// url_path cache: max(now, the newest anchor last_fetched_at + 1), read
// before CommitAdoption Step 3c re-seeds those rows with now.
func TestCommitAdoption_FirstAdoptionPassesTheServedAnchors(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putAnchor := func(file string, fetched int64) {
		h := seedBlob(t, c, "served "+file)
		if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: afSuite + file,
			BlobHash: &h, UpstreamURL: "http://h" + afSuite + file, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
			t.Fatal(err)
		}
	}
	putAnchor("/InRelease", 1_400_000)
	putAnchor("/Release", 1_500_000)
	putAnchor("/Release.gpg", 1_600_000)
	restore := stubNow(t, 1_000_000) // the clock stepped back after those fetches
	id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption")
	restore()
	if got := adoptedAtOf(t, c, id); got != 1_600_001 {
		t.Fatalf("adopted_at = %d, want 1600001 (newest anchor fetch + 1)", got)
	}
}

func TestCommitAdoption_FirstAdoptionKeepsNowPastOlderAnchors(t *testing.T) {
	c := openCache(t)
	h := seedBlob(t, c, "served InRelease")
	fetched := int64(900_000)
	if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: afSuite + "/InRelease",
		BlobHash: &h, UpstreamURL: "http://h" + afSuite + "/InRelease", IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	restore := stubNow(t, 1_000_000)
	id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption, newer clock")
	restore()
	if got := adoptedAtOf(t, c, id); got != 1_000_000 {
		t.Fatalf("adopted_at = %d, want now (1000000)", got)
	}
}

// Each anchor file a client may have been served counts on its own: a
// detached suite's clients hold Release (and Release.gpg), not InRelease.
func TestCommitAdoption_FirstAdoptionPassesEachServedAnchorFile(t *testing.T) {
	for _, file := range []string{"/InRelease", "/Release", "/Release.gpg"} {
		t.Run(file, func(t *testing.T) {
			c := openCache(t)
			h := seedBlob(t, c, "served "+file)
			fetched := int64(1_600_000)
			if err := c.PutURLPath(context.Background(), URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: afSuite + file,
				BlobHash: &h, UpstreamURL: "http://h" + afSuite + file, IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
				t.Fatal(err)
			}
			restore := stubNow(t, 1_000_000)
			id := commitCandidate(t, c, SnapshotCandidate{}, "first adoption after "+file)
			restore()
			if got := adoptedAtOf(t, c, id); got != 1_600_001 {
				t.Fatalf("adopted_at = %d, want 1600001 (the served %s's fetch + 1)", got, file)
			}
		})
	}
}
```

Add `"slices"` to the file's imports, between `"errors"` and `"testing"`. In the same file (deferred minor B-6), make `TestCommitAdoption_FlipDescribesAdoptedAnchor` nil-safe: replace

```go
	if s.CheckFailCount != 2 || *s.LastCheckAt != check || *s.LastSuccessAt != success {
		t.Fatalf("flip touched check state: fail=%d check=%v success=%v", s.CheckFailCount, *s.LastCheckAt, *s.LastSuccessAt)
	}
```

with

```go
	orNeg := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if s.CheckFailCount != 2 || orNeg(s.LastCheckAt) != check || orNeg(s.LastSuccessAt) != success {
		t.Fatalf("flip touched check state: fail=%d check=%d success=%d", s.CheckFailCount, orNeg(s.LastCheckAt), orNeg(s.LastSuccessAt))
	}
```

`internal/cache/displaced_members_test.go`:

```go
package cache

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// commitWithMembers adopts an inline snapshot of suite on host h whose
// members are its InRelease plus extra (path → blob content).
func commitWithMembers(t *testing.T, c *Cache, suite, label string, extra map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	inrel := seedBlob(t, c, "InRelease "+label)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, InReleaseHash: &inrel})
	if err != nil {
		t.Fatal(err)
	}
	members := []SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}
	for p, content := range extra {
		b := seedBlob(t, c, content)
		members = append(members, SnapshotMember{SnapshotID: id, Path: p, BlobHash: b, DeclaredSHA256: b})
	}
	if err := c.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}

func snapshotIDs(ms []SnapshotMember) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.SnapshotID)
	}
	return out
}

// Spec C8 retained by-hash, the cache half: the lookup searches the
// suite's newest limit adopted, non-retired snapshots other than the
// current one and returns the member from those that hold it, newest
// first. limit bounds the snapshots searched, not the rows returned.
func TestDisplacedSnapshotMembers(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const suite = "/r/dists/x"
	alias := "main/binary-amd64/by-hash/SHA256/" + strings.Repeat("a", 64)
	s1 := commitWithMembers(t, c, suite, "one", map[string]string{alias: "content one"})
	s2 := commitWithMembers(t, c, suite, "two", map[string]string{alias: "content two"})
	commitWithMembers(t, c, suite, "three", nil)                                           // newest displaced, no alias
	s4 := commitWithMembers(t, c, suite, "four", map[string]string{alias: "content four"}) // current
	commitWithMembers(t, c, "/r/dists/y", "other suite", map[string]string{alias: "other suite"})

	lookup := func(limit int) []int64 {
		t.Helper()
		got, err := c.DisplacedSnapshotMembers(ctx, "http", "h", suite, alias, s4, limit)
		if err != nil {
			t.Fatal(err)
		}
		return snapshotIDs(got)
	}
	if got := lookup(10); !slices.Equal(got, []int64{s2, s1}) {
		t.Fatalf("limit 10: snapshots %v, want [%d %d] (newest first; current and other suites excluded)", got, s2, s1)
	}
	if got := lookup(1); len(got) != 0 {
		t.Fatalf("limit 1: snapshots %v, want none (the newest displaced snapshot lacks the member)", got)
	}
	if got := lookup(2); !slices.Equal(got, []int64{s2}) {
		t.Fatalf("limit 2: snapshots %v, want [%d]", got, s2)
	}
	if got := lookup(0); len(got) != 0 {
		t.Fatalf("limit 0: snapshots %v, want none", got)
	}
	if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: s2, Outcome: LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.DisplacedSnapshotMembers(ctx, "http", "h", suite, alias, s4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snapshotIDs(got), []int64{s1}) {
		t.Fatalf("after retiring %d: snapshots %v, want [%d] (retired snapshots never serve)", s2, snapshotIDs(got), s1)
	}
	if got[0].Path != alias || got[0].BlobHash != got[0].DeclaredSHA256 {
		t.Fatalf("member = %+v, want the alias row with its binding", got[0])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'TestCommitAdoption_AdoptedAtIsMonotonic|TestCommitAdoption_FirstAdoption|TestDisplacedSnapshotMembers'`
Expected: compile failure naming `c.DisplacedSnapshotMembers undefined`. (With Step 4 applied before Step 3, the stamp tests' own red state shows: `TestCommitAdoption_AdoptedAtIsMonotonic` fails with `adopted_at = [1000000 1000000 500000 2000000]`, `…FirstAdoptionPassesTheServedAnchors` and every case of `…FirstAdoptionPassesEachServedAnchorFile` with `adopted_at = 1000000, want 1600001`; `…FirstAdoptionKeepsNowPastOlderAnchors` already passes — it pins existing behaviour, and mutant (a) is its killer.)

- [ ] **Step 3: The adoption stamp**

In `internal/cache/adoption.go`, inside `CommitAdoption`, right after the line `now := nowUnix()` (the one Step 3a's url_path inserts use, just below the comment ending `// hot-set computation.`) insert:

```go
		// Spec C8 (monotonic Last-Modified): the adoption stamp, taken
		// before Step 3c re-seeds the url_path anchor rows with now.
		stamp, err := adoptionStampTx(ctx, tx, scheme, host, suite, now)
		if err != nil {
			return fmt.Errorf("CommitAdoption: %w", err)
		}
```

Replace Step 7:

```go
		// Step 7: mark the new snapshot adopted. Reuses `now` captured
		// at the top of the transaction for step 3a's last_fetched_at
		// — both writes belong to the same logical commit moment, so a
		// shared timestamp is more honest than two separate
		// nowUnix() calls.
		if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`,
			now, snapshotID); err != nil {
```

with:

```go
		// Step 7: mark the new snapshot adopted with the spec C8 stamp:
		// `now` (the commit moment Step 3a also uses) unless the previous
		// anchor's stamp is not older, which keeps a client's
		// If-Modified-Since from hiding the flip.
		if _, err := tx.ExecContext(ctx, `
UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`,
			stamp, snapshotID); err != nil {
```

Update the doc list item `//  7. Stamp suite_snapshot.adopted_at = now.` to `//  7. Stamp suite_snapshot.adopted_at (adoptionStampTx: max(now, prior + 1)).`

Add after `decrementSnapshotRefcountsTx`:

```go
// adoptionStampTx returns the adopted_at a commit at now stamps (spec C8,
// monotonic Last-Modified): max(now, prior.adopted_at + 1) when the suite
// has a current snapshot; for a first adoption, max(now, the newest
// last_fetched_at of the suite's InRelease/Release/Release.gpg url_path
// rows + 1) — those rows are what clients were served, with that
// Last-Modified or older. The handler serves snapshot anchors with
// max(pool mtime, adopted_at), so a client's If-Modified-Since from the
// previous anchor always gets a 200 after a flip, even across a backward
// clock step.
//
// AIDEV-NOTE: call it before CommitAdoption Step 3c, which re-seeds the
// anchor rows' last_fetched_at with now. adopted_at's other readers (GC's
// keep_displaced ranking, the admin page) only need its order, which this
// keeps equal to the commit order per suite.
func adoptionStampTx(ctx context.Context, tx *sql.Tx, scheme, host, suite string, now int64) (int64, error) {
	var prior sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT s.adopted_at
  FROM suite_freshness f
  JOIN suite_snapshot s ON s.snapshot_id = f.current_snapshot_id
 WHERE f.canonical_scheme = ? AND f.canonical_host = ? AND f.suite_path = ?`,
		scheme, host, suite).Scan(&prior)
	switch {
	case err == nil && prior.Valid:
		return max(now, prior.Int64+1), nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("read prior adopted_at: %w", err)
	}
	var fetched sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT MAX(last_fetched_at) FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path IN (?, ?, ?)`,
		scheme, host, suite+"/InRelease", suite+"/Release", suite+"/Release.gpg").Scan(&fetched); err != nil {
		return 0, fmt.Errorf("read anchor last_fetched_at: %w", err)
	}
	if fetched.Valid {
		return max(now, fetched.Int64+1), nil
	}
	return now, nil
}
```

- [ ] **Step 4: The displaced-member lookup**

Add after `GetSnapshotMember` in `internal/cache/adoption.go`:

```go
// DisplacedSnapshotMembers returns the snapshot_member rows at path in the
// suite's newest limit adopted, non-retired snapshots other than
// currentID — its displaced snapshots — newest first (spec C8 retained
// by-hash). limit bounds the snapshots searched, not the rows returned: a
// member only an older displaced snapshot holds is not found. limit <= 0
// returns nothing.
//
// AIDEV-NOTE: retired snapshots (quarantined at upgrade, or reaped from
// phase F) never serve, so the retired_at filter is load-bearing.
func (c *Cache) DisplacedSnapshotMembers(ctx context.Context, scheme, host, suitePath, path string, currentID int64, limit int) ([]SnapshotMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT m.snapshot_id, m.path, m.blob_hash, m.declared_sha256
  FROM (SELECT snapshot_id, adopted_at
          FROM suite_snapshot
         WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
           AND adopted_at IS NOT NULL AND retired_at IS NULL AND snapshot_id <> ?
         ORDER BY adopted_at DESC, snapshot_id DESC
         LIMIT ?) s
  JOIN snapshot_member m ON m.snapshot_id = s.snapshot_id AND m.path = ?
 ORDER BY s.adopted_at DESC, s.snapshot_id DESC`,
		scheme, host, suitePath, currentID, limit, path)
	if err != nil {
		return nil, fmt.Errorf("DisplacedSnapshotMembers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotMember
	for rows.Next() {
		var m SnapshotMember
		if err := rows.Scan(&m.SnapshotID, &m.Path, &m.BlobHash, &m.DeclaredSHA256); err != nil {
			return nil, fmt.Errorf("DisplacedSnapshotMembers: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DisplacedSnapshotMembers: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 5: Deferred minor B-7 and a stale comment**

In `internal/cache/adoption.go`, extend the `ErrSnapshotNaturalKeyAdopted` doc comment with a last line: `// It arrives wrapped in a *NaturalKeyAdoptedError (errors.Is matches it; errors.As recovers the snapshot id).`

In `internal/gc/integration_test.go`, `TestForensicRetention_KeepDisplacedThree`'s doc comment, replace the paragraph

```go
// Adoptions all happen within the same wall-clock second on a fast
// host, so adopted_at ties are resolved by snapshot_id descending —
// which is monotonically increasing, i.e. ordering by adoption order.
```

with

```go
// CommitAdoption keeps adopted_at strictly increasing per suite (spec
// C8), so the (adopted_at DESC, snapshot_id DESC) ranking is adoption
// order even when all five commits land in one wall-clock second.
```

- [ ] **Step 6: Run the tests**

Run: `go test -race ./internal/cache/ ./internal/gc/`
Expected: PASS. (`TestForensicRetention_KeepDisplacedThree` and the tie-break test still pass: the tie-break test stamps `adopted_at` directly.)

- [ ] **Step 7: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `CommitAdoption`, move the `adoptionStampTx` call (with its comment and error check) to just before Step 7, after Step 3c → `TestCommitAdoption_FirstAdoptionKeepsNowPastOlderAnchors` FAILS (`adopted_at = 1000001`: Step 3c's re-seeded InRelease row is read as a served anchor); (b) in `adoptionStampTx`, return `max(now, prior.Int64)` → `TestCommitAdoption_AdoptedAtIsMonotonic` FAILS (`[1000000 1000000 1000000 2000000]`); (c) stamp `now` in Step 7 (write `now+0*stamp` to keep `stamp` used) → the same test FAILS (`[1000000 1000000 500000 2000000]`); (d) in `DisplacedSnapshotMembers`, move `LIMIT ?` from the subquery to the end of the outer query and pass `path, limit` in that order → `TestDisplacedSnapshotMembers` FAILS at limit 1; (e) drop `AND retired_at IS NULL` → `TestDisplacedSnapshotMembers` FAILS after the retirement; (f) in `adoptionStampTx`, replace `suite+"/Release"` in the `IN` list with `suite+"/Release.none"` → `TestCommitAdoption_FirstAdoptionPassesEachServedAnchorFile` FAILS (its `/Release` case: `adopted_at = 1000000, want 1600001`; the other test's values leave `Release.gpg` as the maximum either way).

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/cache/adoption.go internal/cache/flip_state_test.go internal/cache/displaced_members_test.go internal/gc/integration_test.go
git add internal/cache/adoption.go internal/cache/flip_state_test.go internal/cache/displaced_members_test.go internal/gc/integration_test.go
git commit -m "Keep adopted_at strictly increasing per suite and look up members of displaced snapshots"
```

---

### Task 5: The adoption commits behind the barrier

Spec C8 "Adopter side" and the skip rule for a first adoption whose anchor equals the served url_path anchor; C4 "the host slot is released when the member pipeline ends (before the C8 barrier); the global slot is held through the commit"; C11 `adoption_flip_barrier`; rulings R-C1, R-C2 (with deferred minor B-65 folded in) and R-C4; design notes 5, 9, 10, 14, 18 (no failure record for a flip abandoned at shutdown), 19 (`timed_out` only before the barrier) and 22 (the admission-bound simulation); design review I2, M3, M7 (`commit_failed`), M15, and the executability review's P9 and P10. The phase-C carry-forwards in the overview ("`membersDone` must stay before the barrier"; "the commit keeps `WithoutCancel` + `commitTimeout`, and a shutdown before the commit starts skips it"; "decide whether `Flip`'s waits honour `max_duration`"; "a Checker-level test that `membersDone` frees only the host slot") are all closed here. Deferred minors in files this task touches: B-15 (`metrics.go`), B-26 and B-68 (`freshness.go`), B-29 (`capacity_test.go`).

**Files:**
- Modify: `internal/freshness/adoption.go` (`AdoptionConfig.FlipGate`, `Adopter.flipGate`, `FlipGate()`, `adoptionPayload.stop` with `stopErr`/`barrierCtx`, `runHooks`, `runRequest`, the heartbeat ticker's context, Step 11 in `runShared`, new `flip` and `servedAnchorUnchanged`), `internal/freshness/freshness.go` (the `flipgate` import; `spawnAdoption`; B-26, B-68), `internal/freshness/metrics.go` (inventory note; B-15), `internal/freshness/capacity_test.go` (three `runRequest` callers; B-29)
- Create: `internal/freshness/flip_barrier_test.go`

**Interfaces:**
- Consumes: Task 1 (`flipgate.New`, `Config`, `Key`, `Gate.AnchorStart`, `Gate.Flip`, `Gate.Close`, `Gate.Stats`, `Result`, `Outcome*`, `ErrAborted`, `PhaseAMax`, `TokenWaitMax`, `HoldMax`). Existing test helpers (package `freshness`): `rigHost`, `rigSuite`, `rigAnchorURL`, `rigPkgsURL`, `strOf` (`retry_rig_test.go`), `newFakeFetcher`, `fakePackagesStanzas`, `passThroughVerifier`, `testHex64` (`adoption_test.go`, `adoption_url_test.go`), `datedRelease` (`adoption_date_test.go`), `lockedBuffer` (`panic_recovery_test.go`), `changedUpstream{body}` (`panic_recovery_test.go`), `seedStaleAnchorAt` (`capacity_test.go`), `readHeartbeatAtDirect` (`adoption_test.go`), `hashOf` (`freshness_test.go`), `simMembers` (`sim_sched_test.go`), `startSched` (`reoffer_test.go`), `simtest.OpenCache`, `simtest.DiscardLogger`, `simtest.NewUpstream`, `simtest.Serve`, `simtest.After`, `observability.NewRing`, `cache.ApplyLegacyAuditDecisions`.
- Produces: `freshness.AdoptionConfig.FlipGate *flipgate.Gate`; `(*Adopter).FlipGate() *flipgate.Gate` (nil-safe: a nil adopter, adoption disabled, has no gate; Tasks 6 and 9); unexported `runHooks{membersDone func(); stop context.Context}`, `runRequest(ctx, req, hooks runHooks)` and `barrierOutcome`; the INFO event `adoption_flip_barrier` (outcomes `held`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`). Test helpers (package `freshness`): `barrierSuite`, `barrierKey`, `barrierRig`, `newBarrierRig`.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/flip_barrier_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'TestSuiteKey_IsTheFlipGateKey|TestAdopter_|TestAdoption_|TestCheck_MembersDone|TestCheck_ShutdownInTheBarrier'`
Expected: compile failure naming `unknown field FlipGate in struct literal of type AdoptionConfig` and undefined `runHooks`.

- [ ] **Step 3: The gate in the adopter's configuration**

In `internal/freshness/adoption.go`, add the import `"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"`. Add to `AdoptionConfig`, right after the `MaxDuration` field:

```go

	// FlipGate is the spec C8 flip barrier the commit waits behind
	// (adoption.flip_quiet_period). nil, or a gate with a zero quiet
	// period, commits at once (the pre-C8 behaviour). The handler must
	// register anchor serves with the same gate (Adopter.FlipGate).
	FlipGate *flipgate.Gate
```

Add to the `Adopter` struct, right after `maxDuration time.Duration`:

```go

	// flipGate is the spec C8 barrier the commit waits behind (nil: none).
	flipGate *flipgate.Gate
```

In `NewAdopter`'s struct literal, after `maxDuration: maxDur,` add `flipGate: cfg.FlipGate,` (gofmt aligns it). After `NewAdopter` add:

```go
// FlipGate returns the spec C8 flip barrier the adopter commits behind:
// nil when none is wired, or for a nil adopter (adoption disabled). The
// handler registers anchor serves with the same gate.
func (a *Adopter) FlipGate() *flipgate.Gate {
	if a == nil {
		return nil
	}
	return a.flipGate
}
```

- [ ] **Step 4: The shutdown signal reaches the adoption (B-65)**

In `adoptionPayload`, replace

```go
	// membersDone, when set, runs once the member pipeline (members,
	// by-hash, hot prefetch) is over and before the commit: the Checker
	// frees the host slot there (spec C4).
	membersDone func()
}
```

with

```go
	// membersDone, when set, runs once the member pipeline (members,
	// by-hash, hot prefetch) is over and before the flip barrier: the
	// Checker frees the host slot there (spec C4).
	membersDone func()

	// stop is the shutdown signal: the Checker's lifetime context (nil for
	// Run/RunDetached, which have none). See stopErr and barrierCtx.
	stop context.Context
}

// stopErr reports a shutdown before the commit: the lifetime context
// ended or, for Run/RunDetached callers, ctx was cancelled. ctx's deadline
// (adoption.max_duration) is not a shutdown.
//
// AIDEV-NOTE: read the lifetime context itself, never only ctx: once the
// deadline fired, ctx.Err() stays DeadlineExceeded and a later shutdown
// would go unseen (deferred minor B-65).
func (p *adoptionPayload) stopErr(ctx context.Context) error {
	if p.stop != nil {
		return p.stop.Err()
	}
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// barrierCtx is what the flip barrier waits under: the shutdown signal,
// never the adoption deadline (ruling R-C2). Run/RunDetached callers have
// no shutdown signal: their barrier runs to its own bounds.
func (p *adoptionPayload) barrierCtx(ctx context.Context) context.Context {
	if p.stop != nil {
		return p.stop
	}
	return context.WithoutCancel(ctx)
}
```

Replace `runRequest`'s doc comment, signature and first line:

```go
// runRequest runs the adoption a check requested. membersDone (may be
// nil) runs before the commit; see adoptionPayload.membersDone.
func (a *Adopter) runRequest(ctx context.Context, req *adoptionRequest, membersDone func()) error {
	p := &adoptionPayload{form: req.form, membersDone: membersDone}
```

with

```go
// runHooks is what the Checker passes into an adoption it spawned.
type runHooks struct {
	// membersDone runs once the member pipeline is over, before the flip
	// barrier; the Checker frees the host slot there (spec C4).
	membersDone func()
	// stop is the Checker's lifetime context: its end abandons the
	// adoption before the commit and ends the barrier's waits.
	stop context.Context
}

// runRequest runs the adoption a check requested; see runHooks.
func (a *Adopter) runRequest(ctx context.Context, req *adoptionRequest, hooks runHooks) error {
	p := &adoptionPayload{form: req.form, membersDone: hooks.membersDone, stop: hooks.stop}
```

In `runShared`, replace the heartbeat ticker's context line `tickerCtx, tickerCancel := context.WithCancel(ctx)` with

```go
	// AIDEV-NOTE: the ticker runs until runShared returns, not until ctx's
	// deadline: the flip barrier may outlast adoption.max_duration (ruling
	// R-C2), and the candidate and its member blobs must stay protected
	// until the commit. Shutdown still ends it, through runShared's return.
	tickerCtx, tickerCancel := context.WithCancel(context.WithoutCancel(ctx))
```

- [ ] **Step 5: Step 11 behind the barrier**

In `runShared`, replace everything from the comment `// Spec C4: the member pipeline is over; free the host slot so the` through the closing `}` of the `if err := a.cache.CommitAdoption(...); err != nil { … return fmt.Errorf("%w: commit: %w", ErrAdoptionDBFailed, err) }` block with:

```go
	// Spec C4: the member pipeline is over; free the host slot so the
	// next suite on this host can start. The global slot stays held
	// through the flip barrier and the commit (ruling R-C1).
	//
	// AIDEV-NOTE: membersDone must stay BEFORE the barrier: the barrier
	// may wait up to 140 s, and holding the host slot that long would stall
	// the host's next adoption. Only the host slot is freed here
	// (TestCheck_MembersDoneFreesOnlyTheHostSlot).
	if p.membersDone != nil {
		p.membersDone()
	}

	// A shutdown before the commit starts abandons the adoption, as SPEC2
	// §9.5 step 5 says: starting a commit now would hold the stop for up
	// to commitTimeout, far beyond the shipped unit's TimeoutStopSec.
	// Nothing is recorded (the lifetime context is done); the abandoned
	// candidate is reaped by GC after its stale grace, like any
	// interrupted adoption, and the next start's checks see the change
	// again. See stopErr (B-65).
	if err := p.stopErr(ctx); err != nil {
		return fmt.Errorf("adoption: shutdown before the commit: %w", err)
	}

	// Step 11: the atomic flip transaction, behind the spec C8 flip
	// barrier. The barrier's waits do not honour adoption.max_duration
	// (they are bounded by 140 s, and giving up would throw away finished
	// member work) but end on shutdown, when nothing commits (ruling
	// R-C2). The commit runs under its own bound: the deadline must not
	// abort it, and a shutdown does not abort one already in progress —
	// the stop waits for it, up to commitTimeout. coverageComplete is the
	// SPEC3 §7.5.4 per-snapshot proof for strict mode.
	//
	// AIDEV-NOTE: a commit that has started is never cancelled; a
	// systemctl stop during a long commit is bounded by commitTimeout
	// (5 min), and the unit's TimeoutStopSec (35 s) may SIGKILL first —
	// SQLite rolls the transaction back, so nothing is lost.
	commit := func() error {
		commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		defer cancelCommit()
		return a.cache.CommitAdoption(commitCtx, snapshotID, memberRows, integritySkipped, packageHashes,
			prefetchedURLPaths, pkgHashRes.coverageComplete)
	}
	if err := a.flip(ctx, suite, p, prior == nil, snapshotID, commit); err != nil {
		if errors.Is(err, flipgate.ErrAborted) {
			return fmt.Errorf("adoption: shutdown in the flip barrier: %w", err)
		}
		a.logger.Warn("adoption: commit failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return fmt.Errorf("%w: commit: %w", ErrAdoptionDBFailed, err)
	}
```

Add after `priorSnapshot`:

```go
// flip runs commit behind the spec C8 flip barrier and logs
// adoption_flip_barrier (design note 10). Without an enabled gate it
// commits at once (the pre-C8 behaviour). A first adoption whose anchor is
// the url_path anchor clients have been served skips the barrier: the
// switch changes nothing they hold.
func (a *Adopter) flip(ctx context.Context, suite SuiteRef, p *adoptionPayload, first bool, snapshotID int64, commit func() error) error {
	if !a.flipGate.Enabled() {
		return commit()
	}
	attrs := []any{"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "snapshot_id", snapshotID}
	// first is defensive: Step 3c keeps the url_path anchor at the previous
	// snapshot's bytes, so a later adoption never matches it today; it keeps
	// the skip correct should a later phase write url_path anchors for
	// adopted suites.
	if first && a.servedAnchorUnchanged(ctx, suite, p) {
		err := commit()
		a.logger.Info("adoption_flip_barrier", append(attrs, "outcome", barrierOutcome("not_needed", err))...)
		return err
	}
	res, err := a.flipGate.Flip(p.barrierCtx(ctx), flipgate.Key(suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath), commit)
	if res.Outcome != flipgate.OutcomeAborted {
		a.logger.Info("adoption_flip_barrier", append(attrs,
			"outcome", barrierOutcome(string(res.Outcome), err),
			"phase_a_seconds", res.PhaseA.Seconds(),
			"token_wait_seconds", res.TokenWait.Seconds(),
			"hold_seconds", res.HoldEnd.Sub(res.HoldStart).Seconds(),
			"held_requests", res.Waiters)...)
	}
	return err
}

// barrierOutcome is adoption_flip_barrier's outcome: the barrier's own, or
// commit_failed when the commit ran and failed (the snapshot did not
// switch; the timing fields still describe the barrier).
func barrierOutcome(outcome string, commitErr error) string {
	if commitErr != nil {
		return "commit_failed"
	}
	return outcome
}

// servedAnchorUnchanged reports whether the anchor this adoption commits
// is byte-for-byte the url_path anchor clients have been served: the
// primary anchor of its form (InRelease inline, Release detached — index
// consistency depends on the Release text, not on its signature). A read
// error answers false, so the barrier runs (the safe direction).
//
// AIDEV-TODO(phase E): the unsigned form's anchor is <suite>/Release too.
func (a *Adopter) servedAnchorUnchanged(ctx context.Context, suite SuiteRef, p *adoptionPayload) bool {
	path, want := suite.SuitePath+inReleaseFilename, p.inlineHash
	if p.form == adoptionFormDetached {
		path, want = suite.SuitePath+releaseFilename, p.releaseHash
	}
	row, err := a.cache.LookupURL(context.WithoutCancel(ctx), suite.CanonicalScheme, suite.CanonicalHost, path)
	return err == nil && row.BlobHash != nil && *row.BlobHash == want
}
```

- [ ] **Step 6: The Checker passes its hooks; deferred minors B-15, B-26, B-68**

In `internal/freshness/freshness.go`, add the import `"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"` (after the `fetch` import). In `spawnAdoption`, replace

```go
		err := c.adopter.runRequest(actx, req, l.releaseHost)
		duration := time.Since(start)
		outcome := classifyAdoptionOutcome(err)
		if err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && c.lifetimeCtx.Err() == nil {
			outcome = "timed_out" // a member fetch reports the deadline as its own failure
		}
```

with (design note 19)

```go
		pipelineDone := false // set by membersDone, which runs on this goroutine (runShared Step 11)
		err := c.adopter.runRequest(actx, req, runHooks{
			membersDone: func() { pipelineDone = true; l.releaseHost() },
			stop:        c.lifetimeCtx,
		})
		duration := time.Since(start)
		outcome := classifyAdoptionOutcome(err)
		// A deadline that fires in the member pipeline makes the attempt
		// timed_out (a member fetch reports it as its own failure). One that
		// passes in the flip barrier (ruling R-C2) does not: what failed then
		// is the commit, and its own error classifies it (a commit that hit
		// commitTimeout is still timed_out, through its chain).
		if err != nil && !pipelineDone && errors.Is(actx.Err(), context.DeadlineExceeded) && c.lifetimeCtx.Err() == nil {
			outcome = "timed_out"
		}
```

and replace its last statement in the error branch, `c.recordAdoptionFailure(req, reason)`, with (design note 18)

```go
			// A flip abandoned in the barrier means shutdown: main closes
			// the gate at the signal, while the lifetime context still
			// runs. Shutdown is never a failure (spec C7, design note 18).
			if !errors.Is(err, flipgate.ErrAborted) {
				c.recordAdoptionFailure(req, reason)
			}
```

In its AIDEV-NOTE, replace the last sentence `The host slot is freed earlier, when the member pipeline ends.` with

```go
// The host slot is freed earlier, when the member pipeline ends; the global slot only here, after the flip barrier and the commit (ruling R-C1).
// A deadline that passes after the member pipeline, in the flip barrier
// (ruling R-C2), leaves the outcome to the commit's own error, and a flip
// abandoned in the barrier (shutdown, design note 18) is never recorded as
// a failure.
```

B-26: in `rescheduleFailingAdoptions`, put this comment directly above `if c.adopter == nil {`:

```go
	// Without an adopter nothing adopts: records left by a run with
	// adoption enabled are inert (Phase 1 parity), so rescheduling them
	// would only cost writes.
```

B-68: replace the `ErrAdoptionPanicked` doc comment (`// ErrAdoptionPanicked is recorded (phase B) when an adoption, repair or` / `// reconcile goroutine panics (security finding I-1).`) with

```go
// ErrAdoptionPanicked marks a panicked adoption (security finding I-1):
// the adoption goroutine records it as a failure with reason panic. Repair
// and reconcile panics are logged, not recorded (phase B plan, deviation 7).
```

In `internal/freshness/metrics.go`, append to the inventory AIDEV-NOTE that begins `// AIDEV-NOTE: metrics added by the flat-repository work` (its last line is `// unlabelled gauge).`) a sentence, so that the note ends:

```go
// (spec C7, unlabelled gauge); acu_freshness_waiting_for_slot (spec C4,
// unlabelled gauge). The flip barrier's acu_flip_hold_skipped_total,
// acu_flip_hold_expired_total, acu_flip_waiters_over_cap_total,
// acu_flip_slow_anchor_reads_total and acu_flip_gate_panics_total (spec
// C8, unlabelled counters) are declared in internal/flipgate.
```

B-15: in `classifyAdoptionOutcome`, directly above `case errors.Is(err, context.DeadlineExceeded):` add

```go
	// timed_out stays ahead of the member and DB sentinels: a deadline that
	// surfaces through a %w chain is timed_out — including a commit that
	// hit commitTimeout. Member-fetch errors flatten their cause with %v,
	// so they never match here; spawnAdoption reclassifies as timed_out an
	// attempt whose adoption.max_duration passed in the member pipeline,
	// never one whose deadline passed in the flip barrier (ruling R-C2).
```

- [ ] **Step 7: The existing `runRequest` callers and deferred minor B-29**

In `internal/freshness/capacity_test.go`:

- `TestAdoption_CommitSurvivesTheAdoptionDeadline`: `ad.runRequest(ctx, req, func() { <-ctx.Done() })` becomes `ad.runRequest(ctx, req, runHooks{membersDone: func() { <-ctx.Done() }, stop: context.Background()})`.
- `TestAdoption_ShutdownBeforeTheCommitCommitsNothing`: `env.adopter.runRequest(ctx, req, cancel)` becomes `env.adopter.runRequest(ctx, req, runHooks{membersDone: cancel, stop: ctx})` (the cancellable `ctx` plays the lifetime context).
- `TestAdoption_HostSlotFreedBeforeCommit`: `env.adopter.runRequest(context.Background(), req, membersDone)` becomes `env.adopter.runRequest(context.Background(), req, runHooks{membersDone: membersDone})`.

B-29, `TestCheck_GlobalSlotCapsConcurrentAdoptions`: replace

```go
	chk.Check(ctx, "http", hosts[1], rigSuite)
	select {
	case <-pending:
		t.Fatal("B started while the only global slot was held")
	case <-time.After(150 * time.Millisecond):
	}
```

with

```go
	// B's refusal is read from its result and the slot count, not
	// inferred from a timing window (deferred minor B-29).
	if rep := chk.check(ctx, SuiteRef{CanonicalScheme: "http", CanonicalHost: hosts[1], SuitePath: rigSuite}, checkOpts{}); rep.result != "changed_queued" {
		t.Fatalf("B's check = %q, want changed_queued while the only global slot is held", rep.result)
	}
	if g, _ := chk.admission.usage(); g != 1 {
		t.Fatalf("%d global slots in use, want 1 (A's)", g)
	}
```

B-29, `TestCheck_RepairObeysTheGlobalSlot`: build its checker with a capturing logger — replace

```go
	chk, err := New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: repAd, Logger: discardLogger()})
```

with

```go
	logs := &lockedBuffer{}
	chk, err := New(Config{Cache: c, Fetcher: &simtest.StaticConditional{Status: 304}, HostLimiter: hostsem.New(8),
		Adopter: repAd, Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
```

and replace

```go
	chk.Check(ctx, "http", hosts[1], suitePath)
	select {
	case <-pending:
		t.Fatal("repair B ran while the only global slot was held")
	case <-time.After(150 * time.Millisecond):
	}
```

with

```go
	// B's repair is deferred for want of a slot: read from its line and
	// the slot count, not a timing window (deferred minor B-29).
	chk.Check(ctx, "http", hosts[1], suitePath)
	deferred := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"freshness: repair deferred, no adoption slot"`) &&
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
```

Check: `grep -n 'runRequest(' internal/freshness/*.go` lists exactly `freshness.go` (one call), `adoption.go` (the definition) and the three `capacity_test.go` calls plus the new `flip_barrier_test.go` calls.

- [ ] **Step 8: Run the tests**

Run: `go test -race -count=1 ./internal/freshness/`
Expected: PASS. (Every existing adopter is built without `FlipGate`, so `flip` commits at once and no other test changes.)

- [ ] **Step 9: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `spawnAdoption`'s `membersDone`, call `l.release()` instead of `l.releaseHost()` → `TestCheck_MembersDoneFreesOnlyTheHostSlot` FAILS ("in the barrier: 0 global and 0 host slots held, want 1 and 0"), and so does `TestSimCapacity_TheBarrierHoldsTheGlobalSlotWithinItsBound` when run alone ("in the barrier: 0 global and 0 host slots held, queued suite admitted true; want 2, 0, false"); (b) in `stopErr`, make the `p.stop != nil` branch `return nil` → `TestAdoption_ShutdownAfterTheDeadlineStillAbandonsTheCommit` FAILS (it commits); (c) make `barrierCtx` return `ctx` → `TestAdopter_BarrierOutlivesTheAdoptionDeadline` FAILS ("flip abandoned before the commit: context deadline exceeded"); (d) make `barrierCtx` always return `context.WithoutCancel(ctx)` → `TestAdopter_ShutdownInTheBarrierCommitsNothing` and `TestCheck_ShutdownInTheBarrierRecordsNothing` FAIL (both commit); (e) in `flip`, change the condition to `if false && first && a.servedAnchorUnchanged(ctx, suite, p) {` → `TestAdopter_FirstAdoptionOfTheServedAnchorSkipsTheBarrier/same_bytes` ("committed after 15s, want 0s") and `TestAdopter_DetachedFirstAdoptionComparesTheRelease` FAIL; (f) in `servedAnchorUnchanged`, compare `p.inlineHash` for the detached form too → `TestAdopter_DetachedFirstAdoptionComparesTheRelease` FAILS; (g) restore `tickerCtx, tickerCancel := context.WithCancel(ctx)` → `TestAdoption_HeartbeatRunsUntilTheCommit` FAILS ("last heartbeat 10s after the start"); (h) in `runShared`, delete the `if errors.Is(err, flipgate.ErrAborted) { … }` branch → `TestAdopter_ShutdownInTheBarrierIsNotACommitFailure` FAILS ("a shutdown in the barrier is reported as a DB failure"); (i) in `flip`'s skip path, replace `err := commit()` with `var err error` → `TestAdopter_FirstAdoptionSkipStillCommits` FAILS ("the barrier was skipped but nothing was committed"); (j) in `spawnAdoption`, replace `!pipelineDone &&` with `(!pipelineDone || true) &&` (dropping it leaves `pipelineDone` unused) → `TestCheck_BarrierPastTheDeadlineKeepsTheCommitErrorsOutcome` FAILS (`recorded reason "timed_out"; want no commit and db_failed`); (k) make `barrierOutcome` return `outcome` always → the same test FAILS ("want one commit_failed"); (l) in `spawnAdoption`, change `if !errors.Is(err, flipgate.ErrAborted) {` to `if true || !errors.Is(err, flipgate.ErrAborted) {` → `TestCheck_GateCloseInTheBarrierRecordsNothing` FAILS ("after Close: current set false, failure count 1, reason set true").

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/freshness/adoption.go internal/freshness/freshness.go internal/freshness/metrics.go internal/freshness/capacity_test.go internal/freshness/flip_barrier_test.go
git add internal/freshness/adoption.go internal/freshness/freshness.go internal/freshness/metrics.go internal/freshness/capacity_test.go internal/freshness/flip_barrier_test.go
git commit -m "Commit adoptions behind the flip barrier, keeping the global slot and honouring only shutdown"
```

---

### Task 6: The handler registers anchor serves with the barrier

Spec C8 "Handler side": before serving `<suite>/InRelease` or `<suite>/Release` of a suite with a row, `AnchorStart` (waiting while that suite's flip holds anchors, at most 20 s, honouring the request context, handler shutdown and the waiter cap), before host-slot acquisition and singleflight; `Done` after the (re-read) response is written; `Release.gpg` never held; entries only for suites with rows. Design notes 1, 3 (including the MITM caveat, design review M13) and 18 (the handler-shutdown wait is defence in depth once `main` closes the gate at the signal). Deferred minor A-54 (`handler.go`, a file this task touches). This task also adds the end-to-end harness Task 10 builds on.

**Files:**
- Modify: `internal/handler/handler.go` (`Config.FlipGate`, `Handler.flipGate`, `New`, `ServeHTTP`, new `anchorStart` and `releaseSuffix`; A-54)
- Create: `internal/handler/flip_stack_test.go` (harness), `internal/handler/flip_hook_test.go`

**Interfaces:**
- Consumes: Task 1 (`flipgate.New`, `Config`, `Key`, `Gate.AnchorStart`, `Token.Done`, `Gate.Flip`, `Gate.Len`, `HoldMax`), Task 5 (`freshness.AdoptionConfig.FlipGate`). Existing: `freshness.NewAdopter`, `freshness.SuiteRef`, `simtest.OpenCache`, `simtest.DiscardLogger`, the handler's `hdrXCacheSnapshot`.
- Produces: `handler.Config.FlipGate *flipgate.Gate` (Task 9 wires it). Test helpers in package `handler` (Task 10 uses them): `flipSimHost`, `flipStack`, `newFlipStack(t, quiet)`, `(*flipStack).publish(suite, gen) (freshness.SuiteRef, []byte)`, `.adopt(suite, gen)`, `.current(suite) string`, `.get(path, transfer) (gen string, code int)`, `memberStore`, `plainVerifier`, `noUpstream`, `slowWriter`. (The ledger adapters `serve` and `flipReaders` arrive with their first user, Task 10.)

- [ ] **Step 1: The harness**

`internal/handler/flip_stack_test.go`:

```go
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// flipSimHost is the one upstream host of the flip simulations (mirror
// mode: every suite on one host, one shared client queue).
const flipSimHost = "mirror.example"

// flipStack is a real cache, adopter, flip barrier and handler for
// virtual-clock simulations of spec C8. Create it inside the bubble.
type flipStack struct {
	t       *testing.T
	c       *cache.Cache
	h       *Handler
	gate    *flipgate.Gate
	ad      *freshness.Adopter
	members *memberStore
}

// memberStore is the adopter's upstream: canned member bodies by URL.
type memberStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
}

func (m *memberStore) put(url string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies[url] = body
}

func (m *memberStore) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	m.mu.Lock()
	body, ok := m.bodies[target.URL]
	m.mu.Unlock()
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// plainVerifier passes every body through as its own verified text.
type plainVerifier struct{}

func (plainVerifier) VerifyInline(_ context.Context, _ freshness.SuiteRef, b []byte) ([]byte, error) {
	return b, nil
}

func (plainVerifier) VerifyDetached(_ context.Context, _ freshness.SuiteRef, rel, _ []byte) ([]byte, error) {
	return rel, nil
}

// noUpstream answers every handler fetch 404: in these simulations every
// file a client gets comes from a snapshot.
type noUpstream struct{}

func (noUpstream) HostAllowed(string) bool { return true }

func (noUpstream) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	return nil, &fetch.StatusError{Code: 404}
}

func newFlipStack(t *testing.T, quiet time.Duration) *flipStack {
	t.Helper()
	c := simtest.OpenCache(t)
	gate, err := flipgate.New(flipgate.Config{Quiet: quiet})
	if err != nil {
		t.Fatal(err)
	}
	members := &memberStore{bodies: map[string][]byte{}}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: members, Verifier: plainVerifier{},
		HostLimiter: hostsem.New(64), Logger: simtest.DiscardLogger(), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: simtest.DiscardLogger(),
		HostLimiter: hostsem.New(64), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close) // registered after the cache's cleanup, so it runs first
	return &flipStack{t: t, c: c, h: h, gate: gate, ad: ad, members: members}
}

// publish puts generation gen of suite's Packages upstream and returns
// the suite and its Release text.
func (s *flipStack) publish(suite string, gen int) (freshness.SuiteRef, []byte) {
	pkgs := []byte(fmt.Sprintf("Package: p\nArchitecture: amd64\nVersion: %d\nFilename: pool/p_%d_amd64.deb\nSize: 1\nSHA256: %064x\n\n",
		gen, gen, gen))
	s.members.put("http://"+flipSimHost+suite+"/main/binary-amd64/Packages", pkgs)
	sum := sha256.Sum256(pkgs)
	rel := []byte(fmt.Sprintf("Label: %s generation %d\nSHA256:\n %s %d main/binary-amd64/Packages\n",
		suite, gen, hex.EncodeToString(sum[:]), len(pkgs)))
	return freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}, rel
}

// adopt publishes generation gen of suite upstream and adopts it (inline
// form) through the barrier; it returns once the commit is done.
func (s *flipStack) adopt(suite string, gen int) {
	ref, rel := s.publish(suite, gen)
	if err := s.ad.Run(context.Background(), ref, rel, "", ""); err != nil {
		s.t.Errorf("adopt %s generation %d: %v", suite, gen, err)
	}
}

// current returns suite's current snapshot id as X-Cache-Snapshot shows it.
func (s *flipStack) current(suite string) string {
	sf, err := s.c.GetSuiteFreshness(context.Background(), "http", flipSimHost, suite)
	if err != nil || sf.CurrentSnapshotID == nil {
		return ""
	}
	return strconv.FormatInt(*sf.CurrentSnapshotID, 10)
}

// get serves one GET of path. The response takes transfer to reach the
// client, and the handler is still writing it — in flight — meanwhile.
// It returns the response's X-Cache-Snapshot and status.
func (s *flipStack) get(path string, transfer time.Duration) (gen string, code int) {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(&slowWriter{ResponseRecorder: rec, delay: transfer},
		httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
	return rec.Header().Get(hdrXCacheSnapshot), rec.Code
}

// slowWriter models a client's link: the first body write takes delay.
type slowWriter struct {
	*httptest.ResponseRecorder
	delay time.Duration
	slow  bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.slow {
		w.slow = true
		time.Sleep(w.delay)
	}
	return w.ResponseRecorder.Write(p)
}
```

- [ ] **Step 2: Write the failing tests**

`internal/handler/flip_hook_test.go`:

```go
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
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_AnchorServesRegister|TestServeHTTP_OnlyTrackedSuites|TestServeHTTP_HeldAnchor|TestHandler_CloseReleasesHeld'`
Expected: compile failure naming `unknown field FlipGate in struct literal of type Config`.

- [ ] **Step 4: Implement**

In `internal/handler/handler.go`, add the import `"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"`. Add to `Config`, after `AdoptionEnabled`:

```go

	// FlipGate is the spec C8 flip barrier, shared with the adopter
	// (freshness.Adopter.FlipGate). nil, or a gate with a zero quiet
	// period, never holds or tracks anything (the pre-C8 behaviour).
	FlipGate *flipgate.Gate
```

Add to `Handler`, after `adoptionEnabled bool`:

```go

	// flipGate is the spec C8 barrier (see anchorStart); nil when off.
	flipGate *flipgate.Gate
```

In `New`'s struct literal add `flipGate: cfg.FlipGate,` (after `adoptionEnabled`). Deferred minor A-54: directly above `if cfg.Fetch == nil {` in `New` add:

```go
	// AIDEV-NOTE: Fetch is an interface: a typed-nil *fetch.Client passes
	// this check and panics on first use. Callers pass a constructed
	// client (main.go) or a test fake.
```

In `ServeHTTP`, between the `h.parser.Parse` error block and the `// Fast path: SPEC §6.1.` comment, insert:

```go
	// Spec C8: register an anchor serve with the flip barrier before
	// anything reads the snapshot pointer (it may wait here while the
	// suite's flip holds anchors) and before host-slot acquisition and
	// singleflight; Done runs once the response has been written.
	if t := h.anchorStart(r, req); t != nil {
		defer t.Done()
	}

```

After the `inReleaseSuffix` constant add:

```go

// releaseSuffix is the detached form's anchor under a suite path; spec C8
// holds it like InRelease (and never Release.gpg).
const releaseSuffix = "/Release"
```

After `maybeFireFreshness` (and its `t1PanicStackLogged` var) add:

```go
// anchorStart is spec C8's handler side. For <suite>/InRelease and
// <suite>/Release of a suite with a suite_freshness row it registers the
// serve with the flip barrier and returns its token; while a flip of the
// suite holds anchors it first waits — at most flipgate.HoldMax, and no
// longer than the request or the handler lives. It returns nil otherwise:
// Release.gpg is never held, and untracked suites create no gate state.
//
// AIDEV-NOTE: the registration must precede every read of the snapshot
// pointer (trySnapshotHit re-reads it), or a flip could commit between the
// read and the registration and hand the client the old anchor after the
// flip. The row check before it therefore costs one extra read per anchor
// request of a tracked suite (none when the barrier is off). The wait's
// end on h.lifecycleCtx is defence in depth: the daemon closes the gate at
// the shutdown signal (design note 18), so no hold is left when the
// handler closes. An HTTPS request through the MITM pipeline keeps its
// inner context when its client disconnects (internal/proxy/connect.go),
// so such a waiter stays until the hold ends (at most HoldMax) and counts
// against the waiter cap meanwhile (design note 3).
func (h *Handler) anchorStart(r *http.Request, req *proxy.Request) *flipgate.Token {
	if !h.flipGate.Enabled() || !req.IsMetadata || req.SuitePath == "" {
		return nil
	}
	if req.Path != req.SuitePath+inReleaseSuffix && req.Path != req.SuitePath+releaseSuffix {
		return nil
	}
	if _, err := h.cache.GetSuiteFreshness(r.Context(), req.CanonicalScheme, req.CanonicalHost, req.SuitePath); err != nil {
		// No row, or the read failed (trySnapshotHit answers for that):
		// nothing to register.
		return nil
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(h.lifecycleCtx, cancel)
	defer stop()
	return h.flipGate.AnchorStart(ctx, flipgate.Key(req.CanonicalScheme, req.CanonicalHost, req.SuitePath))
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/handler/`
Expected: PASS (every existing handler is built without a gate).

- [ ] **Step 6: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `anchorStart`, drop `&& req.Path != req.SuitePath+releaseSuffix` → `TestServeHTTP_AnchorServesRegisterWithTheBarrier` FAILS ("/Release: a flip right after the serve waited 0s, want 10s"); (b) also register `Release.gpg` (append `&& req.Path != req.SuitePath+"/Release.gpg"` to that condition) → the same test FAILS (the `/Release.gpg` case waits 10 s); (c) make the row check `false && err != nil` → `TestServeHTTP_OnlyTrackedSuitesCreateGateState` FAILS ("an untracked suite created 1 gate entries"); (d) pass `r.Context()` to `AnchorStart` instead of the merged `ctx` (add `_ = ctx`) → `TestHandler_CloseReleasesHeldAnchorRequests` FAILS ("answered 4s after Close": at the commit); (e) move the `anchorStart` block in `ServeHTTP` to just before `h.serveCacheMiss(w, r, req, start)` → `TestServeHTTP_HeldAnchorIsAnsweredFromTheNewSnapshot` FAILS (snapshot hits no longer register: the flip sees no anchors and commits at once, 250 ms in — the setup flip's hold held nothing, design note 17 — so the request sent at 65 s is not held: "answered 0s after it was sent (at 1m5s); the commit was at 250ms").

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/handler/
git add internal/handler/handler.go internal/handler/flip_stack_test.go internal/handler/flip_hook_test.go
git commit -m "Register InRelease and Release serves of tracked suites with the flip barrier"
```

---

### Task 7: Snapshot anchors carry a monotonic `Last-Modified`

Spec C8 "Monotonic `Last-Modified`": snapshot-served anchors (`InRelease`, `Release`, `Release.gpg`) use `max(pool mtime, adopted_at)` as the `ServeContent` modtime; with Task 4's stamp, a client's `If-Modified-Since` from the previous anchor always gets a 200 after a flip, across same-second flips, a first adoption, and backward clock steps between flips (a step back during an adoption is documented, design note 21). Design note 7 (the recovery path too); the executability review's P7 (the pool-mtime side of the max) and observation 1 (the AIDEV-NOTE's monotonicity condition). Deferred minor A-3, handler half (the handler's wall-clock stamps were never told apart from `time.Now` by a test): `handler.go` is touched here.

**Files:**
- Modify: `internal/handler/handler.go` (`snapshotMeta.minModTime`, `serveBlobWithHeaders`, `trySnapshotHit`, `serveSnapshotMemberMiss`; new `laterOf`, `isAnchorMember`, `anchorModFloor`)
- Create: `internal/handler/anchor_lastmod_test.go`, `internal/handler/wallclock_stamps_test.go`

**Interfaces:**
- Consumes: Task 4 (`CommitAdoption`'s monotonic `adopted_at`, first-adoption stamp). Existing helpers (package `handler`): `newTestHandler`, `writeBlob`, `commitInlineSnapshot`, `newProxyReqHostPort`, `splitURL`, `hostKey`, `proxyReq`, `newSeamHandler`, `recFetcher` (`fetch_seam_test.go`); `wallclock.SetOffsetForTest`, `wallclock.Unix`.
- Produces: unexported `snapshotMeta.minModTime`, `laterOf`, `isAnchorMember`, `anchorModFloor`; `serveSnapshotMemberMiss` gains a `minMod time.Time` parameter. Test helpers `tenYears`, `httpTime` (Task 8 does not need them).

- [ ] **Step 1: Write the failing tests**

`internal/handler/anchor_lastmod_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// tenYears moves the wall clock — and so every adopted_at — far past any
// pool file's real mtime, so the Last-Modified floor shows.
const tenYears = 10 * 365 * 24 * time.Hour

const lmSuite = "/ubuntu/dists/noble"

// lastModifiedGet GETs path from 127.0.0.1 with an optional
// If-Modified-Since and returns the status and Last-Modified.
func lastModifiedGet(t *testing.T, h *Handler, path, ims string) (int, string) {
	t.Helper()
	r := newProxyReqHostPort("GET", "http", "127.0.0.1", path)
	if ims != "" {
		r.Header.Set("If-Modified-Since", ims)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Header().Get("Last-Modified")
}

func httpTime(unix int64) string { return time.Unix(unix, 0).UTC().Format(http.TimeFormat) }

// commitAnchor adopts an inline snapshot of lmSuite whose InRelease is body.
func commitAnchor(t *testing.T, h *Handler, body string) int64 {
	t.Helper()
	hash := writeBlob(t, h, []byte(body))
	return commitInlineSnapshot(t, h, "http", "127.0.0.1", lmSuite, hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
}

func adoptedAt(t *testing.T, h *Handler, id int64) int64 {
	t.Helper()
	snap, err := h.cache.GetSuiteSnapshot(context.Background(), id)
	if err != nil || snap.AdoptedAt == nil {
		t.Fatalf("snapshot %d: %v (err %v)", id, snap, err)
	}
	return *snap.AdoptedAt
}

// Spec C8: a snapshot anchor carries max(pool mtime, adopted_at), and
// adopted_at grows by at least 1 s per flip — so after two flips in one
// second the previous anchor's If-Modified-Since still gets a 200, while
// the current anchor's gets a 304.
func TestServeHTTP_SnapshotAnchorLastModifiedIsMonotonic(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	id1 := commitAnchor(t, h, "anchor one")
	code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	if code != http.StatusOK || lm1 != httpTime(adoptedAt(t, h, id1)) {
		t.Fatalf("first anchor: %d, Last-Modified %q; want 200 and adopted_at %q", code, lm1, httpTime(adoptedAt(t, h, id1)))
	}
	id2 := commitAnchor(t, h, "anchor two")
	code, lm2 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm1)
	if code != http.StatusOK || lm2 != httpTime(adoptedAt(t, h, id2)) {
		t.Fatalf("after the flip, If-Modified-Since %q got %d (Last-Modified %q); want 200 with %q",
			lm1, code, lm2, httpTime(adoptedAt(t, h, id2)))
	}
	if code, _ := lastModifiedGet(t, h, lmSuite+"/InRelease", lm2); code != http.StatusNotModified {
		t.Fatalf("an unchanged anchor got %d, want 304", code)
	}
}

// The same across a backward clock step: adopted_at = prior + 1.
func TestServeHTTP_SnapshotAnchorLastModifiedSurvivesABackwardClockStep(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears + time.Hour)
	defer restore()
	h := newTestHandler(t, nil, nil)
	id1 := commitAnchor(t, h, "anchor before the step")
	_, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	restoreStep := wallclock.SetOffsetForTest(tenYears) // the clock steps back an hour
	defer restoreStep()
	id2 := commitAnchor(t, h, "anchor after the step")
	if got, want := adoptedAt(t, h, id2), adoptedAt(t, h, id1)+1; got != want {
		t.Fatalf("adopted_at after the step = %d, want %d", got, want)
	}
	if code, lm2 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm1); code != http.StatusOK || lm2 != httpTime(adoptedAt(t, h, id2)) {
		t.Fatalf("If-Modified-Since %q got %d (Last-Modified %q), want 200", lm1, code, lm2)
	}
}

// A first adoption passes the anchor clients were served from the
// url_path cache, even when the clock stepped back since: adopted_at =
// max(now, url_path anchor last_fetched_at + 1) (spec C8).
func TestServeHTTP_FirstAdoptionLastModifiedPassesTheServedAnchor(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	served := writeBlob(t, h, []byte("anchor served before adoption"))
	fetched := wallclock.Unix() + 3600 // fetched an hour "later": the clock has since stepped back
	mt := time.Unix(fetched, 0)
	if err := os.Chtimes(h.cache.BlobPath(served), mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: lmSuite + "/InRelease",
		BlobHash: &served, UpstreamURL: "http://127.0.0.1" + lmSuite + "/InRelease", IsMetadata: true, LastFetchedAt: &fetched}); err != nil {
		t.Fatal(err)
	}
	code, lm0 := lastModifiedGet(t, h, lmSuite+"/InRelease", "")
	if code != http.StatusOK || lm0 != httpTime(fetched) {
		t.Fatalf("url_path serve: %d, Last-Modified %q; want 200 and %q", code, lm0, httpTime(fetched))
	}
	commitAnchor(t, h, "anchor adopted first")
	if code, lm1 := lastModifiedGet(t, h, lmSuite+"/InRelease", lm0); code != http.StatusOK || lm1 != httpTime(fetched+1) {
		t.Fatalf("after the first adoption, If-Modified-Since %q got %d (Last-Modified %q); want 200 with %q",
			lm0, code, lm1, httpTime(fetched+1))
	}
}

// Release and Release.gpg of a detached snapshot get the floor too;
// index members keep their pool mtime.
func TestServeHTTP_DetachedAnchorsGetTheFloorIndexesDoNot(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	rel := writeBlob(t, h, []byte("Release body"))
	sig := writeBlob(t, h, []byte("Release.gpg body"))
	pkgs := writeBlob(t, h, []byte("Packages body"))
	if err := h.cache.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: lmSuite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "127.0.0.1",
		SuitePath: lmSuite, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel},
		{SnapshotID: id, Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: pkgs, DeclaredSHA256: pkgs},
	}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"/Release", "/Release.gpg"} {
		if code, lm := lastModifiedGet(t, h, lmSuite+file, ""); code != http.StatusOK || lm != httpTime(adoptedAt(t, h, id)) {
			t.Errorf("%s: %d, Last-Modified %q; want adopted_at %q", file, code, lm, httpTime(adoptedAt(t, h, id)))
		}
	}
	st, err := os.Stat(h.cache.BlobPath(pkgs))
	if err != nil {
		t.Fatal(err)
	}
	if code, lm := lastModifiedGet(t, h, lmSuite+"/main/binary-amd64/Packages", ""); code != http.StatusOK || lm != st.ModTime().UTC().Format(http.TimeFormat) {
		t.Errorf("Packages: %d, Last-Modified %q; want its pool mtime", code, lm)
	}
}

// Design note 7: a snapshot anchor recovered from upstream (its pool file
// was lost) is served with the same floor.
func TestServeHTTP_RecoveredAnchorGetsTheFloor(t *testing.T) {
	restore := wallclock.SetOffsetForTest(tenYears)
	defer restore()
	body := []byte("InRelease recovered from upstream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	h := newTestHandler(t, nil, nil)
	scheme, host, port := splitURL(t, srv.URL)
	hash := writeBlob(t, h, body)
	id := commitInlineSnapshot(t, h, scheme, hostKey(host, port), "/dists/noble", hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
	if err := os.Remove(h.cache.BlobPath(hash)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/dists/noble/InRelease"))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("recovery: %d, X-Cache %q; want 200 MISS", rec.Code, rec.Header().Get("X-Cache"))
	}
	if lm := rec.Header().Get("Last-Modified"); lm != httpTime(adoptedAt(t, h, id)) {
		t.Fatalf("recovered anchor Last-Modified %q, want adopted_at %q", lm, httpTime(adoptedAt(t, h, id)))
	}
}

// max(pool mtime, adopted_at) (spec C8): a pool file newer than adopted_at
// keeps its own mtime as Last-Modified.
func TestServeHTTP_SnapshotAnchorKeepsANewerPoolMtime(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	hash := writeBlob(t, h, []byte("anchor with a newer pool mtime"))
	id := commitInlineSnapshot(t, h, "http", "127.0.0.1", lmSuite, hash,
		[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
	mt := time.Unix(adoptedAt(t, h, id)+3600, 0)
	if err := os.Chtimes(h.cache.BlobPath(hash), mt, mt); err != nil {
		t.Fatal(err)
	}
	if code, lm := lastModifiedGet(t, h, lmSuite+"/InRelease", ""); code != http.StatusOK || lm != httpTime(mt.Unix()) {
		t.Fatalf("%d, Last-Modified %q; want 200 with the newer pool mtime %q", code, lm, httpTime(mt.Unix()))
	}
}
```

`internal/handler/wallclock_stamps_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Deferred minor A-3 (handler half): every stamp the handler persists or
// reports comes from wallclock, so a test offset moves it; time.Now would
// not. (The GC's stamps stay listed in the deferred file.)
func TestHandler_StampsFollowTheWallClock(t *testing.T) {
	restore := wallclock.SetOffsetForTest(48 * time.Hour)
	defer restore()
	ctx := context.Background()
	h := newSeamHandler(t, &recFetcher{body: []byte("deb bytes")})
	defer h.Close()
	near := func(what string, got *int64) {
		t.Helper()
		now := wallclock.Unix()
		if got == nil {
			t.Errorf("%s is unset, want the wall clock %d", what, now)
			return
		}
		if *got < now-5 || *got > now+5 {
			t.Errorf("%s = %d, want the wall clock %d (time.Now is %d)", what, *got, now, time.Now().Unix())
		}
	}
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	// A miss stores its url_path row with wall-clock stamps.
	if rec := get("http://up.example/pool/x.deb"); rec.Code != http.StatusOK {
		t.Fatalf("miss: %d", rec.Code)
	}
	row, err := h.cache.LookupURL(ctx, "http", "up.example", "/pool/x.deb")
	if err != nil {
		t.Fatal(err)
	}
	near("miss last_fetched_at", row.LastFetchedAt)
	near("miss last_requested_at", row.LastRequestedAt)
	// A hit reports X-Cache-Age against the wall clock.
	fetched := wallclock.Unix() - 100
	row.LastFetchedAt = &fetched
	if err := h.cache.PutURLPath(ctx, *row); err != nil {
		t.Fatal(err)
	}
	if age, _ := strconv.Atoi(get("http://up.example/pool/x.deb").Header().Get("X-Cache-Age")); age < 100 || age > 105 {
		t.Errorf("hit X-Cache-Age = %d, want about 100", age)
	}
	// A by-hash content-addressed serve stores its row with wall-clock stamps.
	bh, err := h.parser.Parse("http://up.example/u/dists/s/by-hash/SHA256/"+*row.BlobHash, "up.example")
	if err != nil {
		t.Fatal(err)
	}
	h.recordByHashURLPath(bh, *row.BlobHash)
	byHash, err := h.cache.LookupURL(ctx, "http", "up.example", bh.Path)
	if err != nil {
		t.Fatal(err)
	}
	near("by-hash last_fetched_at", byHash.LastFetchedAt)
	near("by-hash last_requested_at", byHash.LastRequestedAt)
	// A stale serve reports X-Cache-Age against the wall clock.
	if rec := get("http://up.example/u/dists/s/InRelease"); rec.Code != http.StatusOK {
		t.Fatalf("metadata miss: %d", rec.Code)
	}
	md, err := h.cache.LookupURL(ctx, "http", "up.example", "/u/dists/s/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	staleAt := wallclock.Unix() - 200
	md.LastFetchedAt = &staleAt
	if err := h.cache.PutURLPath(ctx, *md); err != nil {
		t.Fatal(err)
	}
	h.serve.ServeStaleWhenUpstreamDown = true
	mdReq, err := h.parser.Parse("http://up.example/u/dists/s/InRelease", "up.example")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if !h.tryServeStale(rec, httptest.NewRequest(http.MethodGet, "http://up.example/u/dists/s/InRelease", nil), mdReq, 0, time.Now()) {
		t.Fatal("stale serve refused")
	}
	if age, _ := strconv.Atoi(rec.Header().Get("X-Cache-Age")); age < 200 || age > 205 {
		t.Errorf("stale X-Cache-Age = %d, want about 200", age)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_SnapshotAnchorLastModified|TestServeHTTP_FirstAdoptionLastModified|TestServeHTTP_DetachedAnchorsGetTheFloor|TestServeHTTP_RecoveredAnchorGetsTheFloor|TestHandler_StampsFollowTheWallClock|TestServeHTTP_SnapshotAnchorKeepsANewerPoolMtime'`
Expected: FAIL — `TestServeHTTP_SnapshotAnchorLastModifiedIsMonotonic`, `…SurvivesABackwardClockStep`, `TestServeHTTP_FirstAdoptionLastModifiedPassesTheServedAnchor`, `TestServeHTTP_DetachedAnchorsGetTheFloorIndexesDoNot` and `TestServeHTTP_RecoveredAnchorGetsTheFloor` report the pool mtime as `Last-Modified`; `TestHandler_StampsFollowTheWallClock` and `TestServeHTTP_SnapshotAnchorKeepsANewerPoolMtime` already PASS (they pin today's behaviour and the pool-mtime side of `max(pool mtime, adopted_at)`; their mutation checks are in Step 5).

- [ ] **Step 3: Implement**

In `internal/handler/handler.go`, replace the `snapshotMeta` type and its doc comment with:

```go
// snapshotMeta carries the metadata used by serveBlobWithHeaders to set
// the response headers. snapshotID = 0 marks a Phase 1 / non-snapshot
// serve (X-Cache-Snapshot suppressed); a positive value emits the
// header. lastFetchedAt is the source for X-Cache-Age and may be nil.
// minModTime, when later than the pool file's mtime, is the response's
// Last-Modified instead (spec C8: a snapshot anchor's adopted_at).
type snapshotMeta struct {
	snapshotID    int64
	lastFetchedAt *int64
	minModTime    time.Time
}
```

In `serveBlobWithHeaders`, change the `http.ServeContent` line that follows `cw := &countingWriter{ResponseWriter: w}` to:

```go
	http.ServeContent(cw, r, req.Path, laterOf(st.ModTime(), meta.minModTime), f)
```

In `trySnapshotHit`, replace

```go
	served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, mem.BlobHash, snapshotMeta{
		snapshotID:    snapshotID,
		lastFetchedAt: nil, // snapshot rows don't carry per-blob last-fetched
	})
```

with

```go
	// Spec C8: a snapshot anchor's Last-Modified is never older than its
	// snapshot's adopted_at, which grows by at least 1 s per flip.
	var floor time.Time
	if isAnchorMember(memberPath) {
		floor = h.anchorModFloor(r.Context(), snapshotID)
	}
	served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, mem.BlobHash, snapshotMeta{
		snapshotID:    snapshotID,
		lastFetchedAt: nil, // snapshot rows don't carry per-blob last-fetched
		minModTime:    floor,
	})
```

and its recovery call `h.serveSnapshotMemberMiss(w, r, req, mem, snapshotID, start)` with `h.serveSnapshotMemberMiss(w, r, req, mem, snapshotID, floor, start)`.

Replace the end of `serveSnapshotMemberMiss`'s doc comment and its signature

```go
// is emitted upstream by ServeHTTP under the existing tryCacheHit
// path.
func (h *Handler) serveSnapshotMemberMiss(
	w http.ResponseWriter, r *http.Request, req *proxy.Request,
	mem *cache.SnapshotMember, snapshotID int64, start time.Time,
) (served bool, status int, bytesWritten int64) {
```

with

```go
// is emitted upstream by ServeHTTP under the existing tryCacheHit
// path. minMod is the spec C8 Last-Modified floor (zero for
// non-anchors).
func (h *Handler) serveSnapshotMemberMiss(
	w http.ResponseWriter, r *http.Request, req *proxy.Request,
	mem *cache.SnapshotMember, snapshotID int64, minMod, start time.Time,
) (served bool, status int, bytesWritten int64) {
```

and change its `http.ServeContent` line (the one after the `hdrXUpstreamStat` block) to:

```go
	http.ServeContent(cw, r, req.Path, laterOf(st.ModTime(), minMod), f)
```

After `suiteRelativePath` add:

```go
// isAnchorMember reports whether a suite-relative member path is a
// snapshot anchor file: InRelease, Release or Release.gpg (spec C8).
func isAnchorMember(p string) bool {
	return p == "InRelease" || p == "Release" || p == "Release.gpg"
}

// anchorModFloor is the spec C8 Last-Modified floor of a snapshot-served
// anchor: the snapshot's adopted_at. CommitAdoption keeps adopted_at
// strictly increasing per suite, so a client's If-Modified-Since from the
// previous anchor always gets a 200 after a flip, even across a backward
// clock step. A read failure leaves the pool mtime (the pre-C8 behaviour).
//
// AIDEV-NOTE: monotonic unless the previous anchor's pool file was
// (re)written at a wall time later than the next commit's stamp: the clock
// stepped back between that blob's write and its commit, or after a
// snapshot-member recovery rewrote it (docs/configuration.md states the
// gap; design review M5).
func (h *Handler) anchorModFloor(ctx context.Context, snapshotID int64) time.Time {
	snap, err := h.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil || snap.AdoptedAt == nil {
		return time.Time{}
	}
	return time.Unix(*snap.AdoptedAt, 0)
}

// laterOf returns the later of a pool file's mtime and a Last-Modified
// floor (a zero floor leaves the mtime).
func laterOf(mtime, floor time.Time) time.Time {
	if floor.After(mtime) {
		return floor
	}
	return mtime
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `serveBlobWithHeaders`, pass `st.ModTime()` again → `TestServeHTTP_SnapshotAnchorLastModifiedIsMonotonic` FAILS ("first anchor: … want 200 and adopted_at …"); (b) in `serveSnapshotMemberMiss`, pass `st.ModTime()` again (keep `_ = minMod`) → `TestServeHTTP_RecoveredAnchorGetsTheFloor` FAILS; (c) make `isAnchorMember` return `true` → `TestServeHTTP_DetachedAnchorsGetTheFloorIndexesDoNot` FAILS ("Packages: … want its pool mtime"); (d) in `runFetch`, replace `now := wallclock.Unix()` with `now := time.Now().Unix()` → `TestHandler_StampsFollowTheWallClock` FAILS (miss stamps); (e) in `serveBlobWithHeaders`, compute the age from `time.Now().Unix()` → the same test FAILS ("hit X-Cache-Age = 0"); (f) the same in `recordByHashURLPath` → FAILS (by-hash stamps); (g) the same for `tryServeStale`'s age → FAILS ("stale X-Cache-Age = 0"); (h) in `laterOf`, always take a set floor (`if !floor.IsZero() {`) → `TestServeHTTP_SnapshotAnchorKeepsANewerPoolMtime` FAILS ("want 200 with the newer pool mtime").

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/
git add internal/handler/handler.go internal/handler/anchor_lastmod_test.go internal/handler/wallclock_stamps_test.go
git commit -m "Serve snapshot anchors with Last-Modified no older than their adopted_at"
```

---

### Task 8: Retained by-hash

Spec C8 "Retained by-hash": when `trySnapshotHit` misses the current snapshot for a `by-hash/SHA256|SHA512/<hex>` member, look it up in the suite's other adopted, non-retired snapshots (newest first; at most `gc.keep_displaced + 1`), serve it with that snapshot's `X-Cache-Snapshot`; SHA256 aliases must match the served blob hash, SHA512 aliases rely on the verified `snapshot_member` binding; no recovery fetch for displaced snapshots; counter `acu_serve_byhash_displaced_total{host}`; `gc.keep_displaced = 0` disables it. Plan deviations 3 and 4, design note 15; the executability review's P8 (a mismatched newer row must not hide an older valid one) and observation 3 (the regex's leading anchor is defensive, kept).

**Files:**
- Modify: `internal/handler/handler.go` (`Config.KeepDisplaced`, `Handler.keepDisplaced`, `New`, `trySnapshotHit`, `tryCacheHit`, `packageValidation`, `logRequestWithValidation`; new `byHashMemberRE`, `tryDisplacedByHash`), `internal/handler/metrics.go`
- Create: `internal/handler/byhash_displaced_test.go`

**Interfaces:**
- Consumes: Task 4 (`cache.DisplacedSnapshotMembers`), Task 7 (`trySnapshotHit` with its `floor`). Existing helpers (package `handler`): `writeBlob`, `commitInlineSnapshot`, `newProxyReqHostPort`, `sha256Hex`, `itoa` (`phase2_handler_test.go`), `sumMetricLines` (`disabled_mode_parity_test.go`), `silentLogger`; `cache.ApplyLegacyAuditDecisions`, `cache.LegacyAuditQuarantine`.
- Produces: `handler.Config.KeepDisplaced int` (Task 9 wires it); counter `acu_serve_byhash_displaced_total{host}`; the `request` log field `snapshot_displaced`.

- [ ] **Step 1: Write the failing tests**

`internal/handler/byhash_displaced_test.go`:

```go
package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

const dispSuite = "/ubuntu/dists/noble"

// syncLog is a log sink safe for the handler's concurrent writers.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// countingUpstream answers 404 and counts the fetches it is asked for.
type countingUpstream struct {
	mu sync.Mutex
	n  int
}

func (*countingUpstream) HostAllowed(string) bool { return true }

func (u *countingUpstream) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.n++
	return nil, &fetch.StatusError{Code: 404}
}

func (u *countingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// newDisplacedHandler builds a handler whose retained by-hash searches
// keep+1 displaced snapshots, logging to logs when it is not nil.
func newDisplacedHandler(t *testing.T, keep int, logs *syncLog) (*Handler, *countingUpstream) {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	logger := silentLogger()
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	up := &countingUpstream{}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: up, Logger: logger, HostLimiter: hostsem.New(4), KeepDisplaced: keep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, up
}

// adoptWithMembers adopts an inline snapshot of dispSuite whose members
// are its InRelease plus extra (path → blob content).
func adoptWithMembers(t *testing.T, h *Handler, label string, extra map[string]string) int64 {
	t.Helper()
	inrel := writeBlob(t, h, []byte("InRelease "+label))
	members := []cache.SnapshotMember{{Path: "InRelease", BlobHash: inrel, DeclaredSHA256: inrel}}
	for p, content := range extra {
		b := writeBlob(t, h, []byte(content))
		members = append(members, cache.SnapshotMember{Path: p, BlobHash: b, DeclaredSHA256: b})
	}
	return commitInlineSnapshot(t, h, "http", "127.0.0.1", dispSuite, inrel, members, nil)
}

// aliasOf is content's SHA256 by-hash path under binary-amd64.
func aliasOf(content string) string {
	return "main/binary-amd64/by-hash/SHA256/" + sha256Hex([]byte(content))
}

func getMember(t *testing.T, h *Handler, member string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newProxyReqHostPort("GET", "http", "127.0.0.1", dispSuite+"/"+member))
	return rec
}

// Spec C8 retained by-hash: a by-hash member the current snapshot lacks
// is served from the displaced snapshot that declared it, with that
// snapshot's X-Cache-Snapshot, counted, logged as snapshot_displaced, and
// without asking upstream.
func TestServeHTTP_RetainedByHashServesTheDisplacedSnapshot(t *testing.T) {
	logs := &syncLog{}
	h, up := newDisplacedHandler(t, 3, logs)
	id1 := adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{aliasOf("Packages two"): "Packages two"})
	before := sumMetricLines(t, []string{"acu_serve_byhash_displaced_total"})["acu_serve_byhash_displaced_total"]
	rec := getMember(t, h, aliasOf("Packages one"))
	if rec.Code != http.StatusOK || rec.Body.String() != "Packages one" || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d %q from snapshot %q; want 200 \"Packages one\" from %d",
			rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
	after := sumMetricLines(t, []string{"acu_serve_byhash_displaced_total"})["acu_serve_byhash_displaced_total"]
	if after-before != 1 {
		t.Errorf("acu_serve_byhash_displaced_total rose by %v, want 1", after-before)
	}
	if !strings.Contains(logs.String(), `"snapshot_displaced":true`) {
		t.Errorf("no request line carries snapshot_displaced=true:\n%s", logs.String())
	}
	if n := up.count(); n != 0 {
		t.Errorf("upstream was asked %d times, want 0", n)
	}
}

// A SHA256 alias is served only when its blob is the hash the URL names,
// and a pool blob that no displaced snapshot declares is never served for
// an adopted suite (stray-blob negatives).
func TestServeHTTP_RetainedByHashSHA256MustMatchTheBlob(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	forged := "main/binary-amd64/by-hash/SHA256/" + strings.Repeat("ab", 32)
	adoptWithMembers(t, h, "one", map[string]string{forged: "bytes that hash to something else"})
	adoptWithMembers(t, h, "two", nil)
	if rec := getMember(t, h, forged); rec.Code != http.StatusNotFound {
		t.Errorf("an alias bound to another blob: %d, want 404", rec.Code)
	}
	stray := writeBlob(t, h, []byte("a stray blob"))
	if rec := getMember(t, h, "main/binary-amd64/by-hash/SHA256/"+stray); rec.Code != http.StatusNotFound {
		t.Errorf("a stray pool blob: %d, want 404", rec.Code)
	}
}

// A SHA512 alias relies on the snapshot_member binding verified at
// adoption (plan deviation 3: phase E's adoptions write these rows).
func TestServeHTTP_RetainedByHashSHA512UsesTheVerifiedBinding(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	alias := "main/binary-amd64/by-hash/SHA512/" + strings.Repeat("cd", 64)
	id1 := adoptWithMembers(t, h, "one", map[string]string{alias: "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	rec := getMember(t, h, alias)
	if rec.Code != http.StatusOK || rec.Body.String() != "Packages one" || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d %q from snapshot %q; want 200 from %d", rec.Code, rec.Body.String(), rec.Header().Get(hdrXCacheSnapshot), id1)
	}
}

func TestServeHTTP_RetainedByHashPrefersTheNewestDisplacedSnapshot(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	shared := map[string]string{aliasOf("shared Packages"): "shared Packages"}
	adoptWithMembers(t, h, "one", shared)
	id2 := adoptWithMembers(t, h, "two", shared)
	adoptWithMembers(t, h, "three", nil)
	if got := getMember(t, h, aliasOf("shared Packages")).Header().Get(hdrXCacheSnapshot); got != itoa(id2) {
		t.Fatalf("served from snapshot %q, want the newest displaced one, %d", got, id2)
	}
}

// The search covers the newest keep_displaced + 1 displaced snapshots:
// what GC keeps, plus the one a flip displaced since GC last ran.
func TestServeHTTP_RetainedByHashSearchesKeepDisplacedPlusOne(t *testing.T) {
	h, _ := newDisplacedHandler(t, 1, nil) // searches two
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{aliasOf("Packages two"): "Packages two"})
	adoptWithMembers(t, h, "three", nil)
	adoptWithMembers(t, h, "four", nil) // displaced, newest first: three, two, one
	if rec := getMember(t, h, aliasOf("Packages two")); rec.Code != http.StatusOK {
		t.Errorf("the second-newest displaced snapshot's alias: %d, want 200", rec.Code)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Errorf("the third-newest displaced snapshot's alias: %d, want 404", rec.Code)
	}
}

func TestServeHTTP_RetainedByHashIsOffWhenKeepDisplacedIsZero(t *testing.T) {
	h, _ := newDisplacedHandler(t, 0, nil)
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (gc.keep_displaced = 0 disables retained by-hash)", rec.Code)
	}
}

func TestServeHTTP_RetainedByHashNeverServesRetiredSnapshots(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	id1 := adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if _, err := h.cache.ApplyLegacyAuditDecisions(context.Background(),
		[]cache.LegacyAuditDecision{{SnapshotID: id1, Outcome: cache.LegacyAuditQuarantine}}); err != nil {
		t.Fatal(err)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d from a retired snapshot, want 404", rec.Code)
	}
}

// No recovery fetch for displaced snapshots: a lost blob is a 404.
func TestServeHTTP_RetainedByHashLostBlobIs404WithoutAFetch(t *testing.T) {
	h, up := newDisplacedHandler(t, 3, nil)
	adoptWithMembers(t, h, "one", map[string]string{aliasOf("Packages one"): "Packages one"})
	adoptWithMembers(t, h, "two", nil)
	if err := os.Remove(h.cache.BlobPath(sha256Hex([]byte("Packages one")))); err != nil {
		t.Fatal(err)
	}
	if rec := getMember(t, h, aliasOf("Packages one")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("upstream was asked %d times, want 0", n)
	}
}

func TestNew_RejectsNegativeKeepDisplaced(t *testing.T) {
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := New(Config{Parser: parser, Cache: c, Fetch: &countingUpstream{}, Logger: silentLogger(),
		HostLimiter: hostsem.New(4), KeepDisplaced: -1}); err == nil {
		t.Fatal("New accepted KeepDisplaced = -1")
	}
}

// A newer displaced snapshot whose SHA256 alias row is bound to other bytes
// must not hide an older displaced snapshot that holds the real bytes.
func TestServeHTTP_RetainedByHashSkipsAMismatchedNewerRow(t *testing.T) {
	h, _ := newDisplacedHandler(t, 3, nil)
	alias := aliasOf("Packages one")
	id1 := adoptWithMembers(t, h, "one", map[string]string{alias: "Packages one"})
	adoptWithMembers(t, h, "two", map[string]string{alias: "bytes that hash to something else"})
	adoptWithMembers(t, h, "three", nil)
	rec := getMember(t, h, alias)
	if rec.Code != http.StatusOK || rec.Header().Get(hdrXCacheSnapshot) != itoa(id1) {
		t.Fatalf("got %d from snapshot %q, want 200 from %d", rec.Code, rec.Header().Get(hdrXCacheSnapshot), id1)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run 'TestServeHTTP_RetainedByHash|TestNew_RejectsNegativeKeepDisplaced'`
Expected: compile failure naming `unknown field KeepDisplaced in struct literal of type Config`.

- [ ] **Step 3: Implement**

In `internal/handler/metrics.go`, add to the `var (` block (after `serveSnapshotIndexTarget404Total`):

```go

	// serveByHashDisplacedTotal counts spec C8 retained by-hash serves: a
	// by-hash member the current snapshot lacks, served from one of the
	// suite's displaced snapshots (to a client that fetched an earlier
	// anchor). Host-labelled, no suite labels (spec C11).
	serveByHashDisplacedTotal = metrics.NewCounterWithCap(
		"acu_serve_byhash_displaced_total",
		"by-hash requests served from a displaced (earlier) snapshot of their suite, by upstream host (spec C8).",
		metrics.DefaultMaxSeries,
		"host",
	)
```

and after the line `// counter is emitted explicitly at logUnvouchedPassthrough.` add:

```go
//
// AIDEV-NOTE: added by the flat-repository work, outside the SPEC5
// §10.4.1 inventory: acu_serve_byhash_displaced_total{host} (spec C8
// retained by-hash).
```

In `internal/handler/handler.go`, add to `Config` (after `FlipGate`):

```go

	// KeepDisplaced is gc.keep_displaced. Spec C8 retained by-hash serves
	// a by-hash request the current snapshot lacks from the suite's newest
	// KeepDisplaced+1 displaced, non-retired snapshots; 0 disables it.
	KeepDisplaced int
```

to `Handler` (after `flipGate`):

```go

	// keepDisplaced bounds retained by-hash (see tryDisplacedByHash).
	keepDisplaced int
```

In `New`, after the `HostLimiter` nil check:

```go
	if cfg.KeepDisplaced < 0 {
		return nil, errors.New("handler: negative KeepDisplaced")
	}
```

and `keepDisplaced: cfg.KeepDisplaced,` in the struct literal.

`trySnapshotHit` now also reports whether it served from a displaced snapshot. Change its signature to

```go
func (h *Handler) trySnapshotHit(w http.ResponseWriter, r *http.Request, req *proxy.Request, start time.Time) (served bool, status int, bytesWritten int64, handled, displaced bool) {
```

append `, false` to each of its seven existing `return` statements (`return false, 0, 0, false, false` twice; `return h.writeFailClosed(w, "suite lookup failed"), http.StatusBadGateway, 0, true, false`; `return true, http.StatusNotFound, 0, true, false`; `return h.writeFailClosed(w, "snapshot lookup failed"), http.StatusBadGateway, 0, true, false`; `return served, status, bytesWritten, true, false`; `return true, status, bytesWritten, true, false`), and make the first statements of its `case errors.Is(err, cache.ErrNotFound):` branch (before the `// SPEC2 §6.1: snapshot is the contract.` comment):

```go
		// Spec C8 retained by-hash: a by-hash member the current snapshot
		// lacks may be one a displaced snapshot declared — a client that
		// fetched an earlier anchor asks for exactly those bytes.
		if ok, st, n := h.tryDisplacedByHash(w, r, req, snapshotID, memberPath); ok {
			go h.touchAsync(req)
			h.maybeFireFreshness(req)
			return true, st, n, true, true
		}
```

In `tryCacheHit`, replace

```go
		if served, status, body, handled := h.trySnapshotHit(w, r, req, start); handled {
			return served, "", status, body, packageValidation{}
		}
```

with

```go
		if served, status, body, handled, displaced := h.trySnapshotHit(w, r, req, start); handled {
			return served, "", status, body, packageValidation{SnapshotDisplaced: displaced}
		}
```

and in its doc comment extend regime 1's sentence `A miss in snapshot_member returns 404 — once a suite is adopted, the snapshot is the contract.`: replace

```go
//     url_path fallback. A miss in snapshot_member returns 404 — once
//     a suite is adopted, the snapshot is the contract.
```

with

```go
//     url_path fallback. A miss in snapshot_member returns 404 — once
//     a suite is adopted, the snapshot is the contract. The one
//     exception is a by-hash member one of the suite's displaced
//     snapshots declared (spec C8 retained by-hash).
```

Add to `packageValidation` (after `PackageName`):

```go

	// SnapshotDisplaced marks a spec C8 retained by-hash serve (from a
	// displaced snapshot); logged as snapshot_displaced=true.
	SnapshotDisplaced bool
```

In `logRequestWithValidation`, after the `if validation.Validated { … }` block:

```go
	// Spec C8: snapshot_displaced marks a retained by-hash serve (from a
	// displaced snapshot, which X-Cache-Snapshot names); emitted only when
	// true.
	if validation.SnapshotDisplaced {
		attrs = append(attrs, "snapshot_displaced", true)
	}
```

After `tryByHashContentAddressed` (and before `recordByHashURLPath`) add:

```go
// byHashMemberRE matches a suite-relative by-hash member path, SHA256 or
// SHA512, capturing the hex when the algorithm is SHA256. The leading
// (?:^|/) is defensive: IsMetadata routes only /by-hash/ paths here.
var byHashMemberRE = regexp.MustCompile(`(?:^|/)by-hash/(?:SHA256/([0-9a-f]{64})|SHA512/[0-9a-f]{128})$`)

// tryDisplacedByHash is spec C8's retained by-hash: a by-hash member the
// current snapshot lacks is served from one of the suite's displaced,
// non-retired snapshots — the newest of the newest keep_displaced+1 that
// holds it — with that snapshot's X-Cache-Snapshot. A SHA256 alias is
// served only when its blob is the hash the URL names; a SHA512 alias
// relies on the snapshot_member binding verified at adoption. There is no
// recovery fetch: a displaced member whose blob is gone stays a 404.
// keep_displaced = 0 disables it.
//
// AIDEV-NOTE: the one place a non-current snapshot serves. It is limited
// to content-addressed paths (a client holding an earlier anchor asks for
// exactly those bytes) and to the snapshots GC still keeps. A lookup
// failure answers the pre-C8 404 (logged), never the unverified miss path.
func (h *Handler) tryDisplacedByHash(w http.ResponseWriter, r *http.Request, req *proxy.Request, currentID int64, memberPath string) (served bool, status int, bytesWritten int64) {
	if h.keepDisplaced <= 0 {
		return false, 0, 0
	}
	m := byHashMemberRE.FindStringSubmatch(memberPath)
	if m == nil {
		return false, 0, 0
	}
	rows, err := h.cache.DisplacedSnapshotMembers(r.Context(), req.CanonicalScheme, req.CanonicalHost,
		req.SuitePath, memberPath, currentID, h.keepDisplaced+1)
	if err != nil {
		h.logger.Warn("displaced snapshot lookup failed",
			"err", err,
			"canonical_host", req.CanonicalHost,
			"path", req.Path,
		)
		return false, 0, 0
	}
	for _, row := range rows {
		if m[1] != "" && row.BlobHash != m[1] {
			continue // a SHA256 alias must be the bytes its URL names
		}
		if served, status, bytesWritten = h.serveBlobWithHeaders(w, r, req, row.BlobHash, snapshotMeta{snapshotID: row.SnapshotID}); served {
			serveByHashDisplacedTotal.Inc(req.CanonicalHost)
			return true, status, bytesWritten
		}
	}
	return false, 0, 0
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) make the alias check `if false && m[1] != "" && row.BlobHash != m[1]` → `TestServeHTTP_RetainedByHashSHA256MustMatchTheBlob` FAILS ("an alias bound to another blob: 200, want 404"); (b) pass `h.keepDisplaced` instead of `h.keepDisplaced+1` → `TestServeHTTP_RetainedByHashSearchesKeepDisplacedPlusOne` FAILS; (c) make the early return `if false && h.keepDisplaced <= 0` → `TestServeHTTP_RetainedByHashIsOffWhenKeepDisplacedIsZero` FAILS (served with limit 1); (d) narrow `byHashMemberRE` to `(?:^|/)by-hash/(?:SHA256/([0-9a-f]{64}))$` → `TestServeHTTP_RetainedByHashSHA512UsesTheVerifiedBinding` FAILS; (e) return `packageValidation{}` from `tryCacheHit`'s snapshot branch again (`_ = displaced`) → `TestServeHTTP_RetainedByHashServesTheDisplacedSnapshot` FAILS (no `snapshot_displaced`); (f) replace the `serveByHashDisplacedTotal.Inc(req.CanonicalHost)` line with `_ = serveByHashDisplacedTotal` → the same test FAILS ("rose by 0, want 1"); (g) cross-check of Task 4: drop `AND retired_at IS NULL` from `DisplacedSnapshotMembers` → `TestServeHTTP_RetainedByHashNeverServesRetiredSnapshots` FAILS; (h) in `tryDisplacedByHash`, replace `continue // a SHA256 alias must be the bytes its URL names` with `return false, 0, 0` (stop at the first mismatching row) → `TestServeHTTP_RetainedByHashSkipsAMismatchedNewerRow` FAILS ("got 404 from snapshot \"\", want 200 from …").

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/
git add internal/handler/handler.go internal/handler/metrics.go internal/handler/byhash_displaced_test.go
git commit -m "Serve by-hash members of displaced snapshots to clients holding an earlier anchor"
```

---

### Task 9: Wire the barrier and retained by-hash in `main`

Spec C8, C11: `buildAdopter` builds the gate from `adoption.flip_quiet_period` and hands it to the adopter; the handler gets the same gate (`Adopter.FlipGate()`, nil with adoption disabled) and `gc.keep_displaced`; both startup lines report the knob; the shutdown closes the gate at the signal (design note 18; Tasks 1 and 5 test what `Close` does and that nothing is recorded — the call itself in `serveListeners` has no test: no daemon-level test can reach a flip in the barrier cheaply). Design note 13. Deferred minors in `main.go`: B-64 (explain a slow drain at shutdown; design note 16) and A-48 (the doubled `legacy trust audit:` prefix).

**Files:**
- Modify: `cmd/apt-cacher-ultra/main.go` (import `flipgate`; `buildAdopter`; new `handlerConfig`, `adoptionDrainNotice`, `waitLogged`; the `handler.New` call; the startup line; `Gate.Close` at the signal; the shutdown drain; A-48)
- Create: `cmd/apt-cacher-ultra/flip_wiring_test.go`

**Interfaces:**
- Consumes: Task 1 (`flipgate.New`, `Gate.Quiet`, `Gate.Enabled`, `Gate.Close`), Task 3 (`config.AdoptionConfig.FlipQuietPeriod`), Task 5 (`freshness.AdoptionConfig.FlipGate`, nil-safe `(*Adopter).FlipGate()`), Tasks 6 and 8 (`handler.Config.FlipGate`, `handler.Config.KeepDisplaced`). Existing test helpers (package `main`): `withKeyringDirs`, `newAdoptionEnabledCfg`, `newAdoptionWiringDeps`, `silentBuildLogger` (`adoption_wiring_test.go`), `minimalCfg` (`main_test.go`), `syncLogBuffer` (`legacy_audit_wiring_test.go`); `metrics.Default.SnapshotNamesForTest` / `UnregisterAddedSinceForTest`.
- Produces: unexported `handlerConfig(cfg, parser, c, fetchClient, hostLimiter, logger, freshChecker, adopter) handler.Config`, `adoptionDrainNotice`, `waitLogged(wait func(), notice time.Duration, logger *slog.Logger)`; the INFO event `shutdown_waiting_for_adoptions`; the startup fields `adoption_flip_quiet_period` and `flip_quiet_period`.

- [ ] **Step 1: Write the failing tests**

`cmd/apt-cacher-ultra/flip_wiring_test.go`:

```go
package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Spec C8, C11: buildAdopter builds the flip barrier from
// adoption.flip_quiet_period (0s: disabled) and reports it; an
// out-of-range value (config validation normally stops it) is an error.
// Mutates keyringDirs, so NOT t.Parallel.
func TestBuildAdopter_BuildsTheFlipBarrier(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	for _, quiet := range []time.Duration{10 * time.Second, 0} {
		cfg := newAdoptionEnabledCfg(t, false)
		cfg.Adoption.FlipQuietPeriod.Duration = quiet
		c, fetcher, hosts := newAdoptionWiringDeps(t)
		logs := &syncLogBuffer{}
		a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewJSONHandler(logs, nil)))
		if err != nil {
			t.Fatalf("quiet %v: buildAdopter: %v", quiet, err)
		}
		if g := a.FlipGate(); g == nil || g.Quiet() != quiet || g.Enabled() != (quiet > 0) {
			t.Fatalf("quiet %v: gate quiet %v, enabled %v", quiet, g.Quiet(), g.Enabled())
		}
		if want := `"flip_quiet_period":` + strconv.FormatInt(int64(quiet), 10); !strings.Contains(logs.String(), want) {
			t.Errorf("quiet %v: the phase2 adoption enabled line lacks %s:\n%s", quiet, want, logs.String())
		}
	}
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 16 * time.Second
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	if _, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger()); err == nil {
		t.Fatal("buildAdopter accepted a 16s quiet period")
	}
}

// Spec C8: the handler shares the adopter's gate — anchor serves and
// commits meet at one barrier — and retained by-hash searches what GC
// keeps. With adoption disabled there is no gate.
func TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
	cfg.GC.KeepDisplaced = 3
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := handlerConfig(cfg, parser, c, fetcher, hosts, silentBuildLogger(), nil, a)
	if hc.FlipGate == nil || hc.FlipGate != a.FlipGate() {
		t.Fatal("the handler does not share the adopter's flip barrier")
	}
	if hc.KeepDisplaced != 3 {
		t.Fatalf("KeepDisplaced = %d, want gc.keep_displaced (3)", hc.KeepDisplaced)
	}
	if hc := handlerConfig(cfg, parser, c, fetcher, hosts, silentBuildLogger(), nil, nil); hc.FlipGate != nil {
		t.Fatal("adoption disabled, yet the handler has a flip barrier")
	}
}

// Deferred minor B-64: a shutdown that waits on a running adoption commit
// says so once the wait outlasts the notice delay, and still returns
// exactly when the drain ends.
func TestWaitLogged_ExplainsASlowAdoptionDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncLogBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		start := time.Now()
		waitLogged(func() { time.Sleep(time.Second) }, adoptionDrainNotice, logger)
		if strings.Contains(logs.String(), "shutdown_waiting_for_adoptions") {
			t.Fatalf("a 1s drain was logged:\n%s", logs.String())
		}
		waitLogged(func() { time.Sleep(time.Minute) }, adoptionDrainNotice, logger)
		if n := strings.Count(logs.String(), `"msg":"shutdown_waiting_for_adoptions"`); n != 1 {
			t.Fatalf("a 1m drain logged %d notices, want 1:\n%s", n, logs.String())
		}
		if d := time.Since(start); d != time.Second+time.Minute {
			t.Fatalf("waitLogged returned after %v in all, want when the drains ended (1m1s)", d)
		}
	})
}

// Deferred minor A-48: a failed legacy trust audit refuses the start with
// one "legacy trust audit:" prefix, not two.
func TestServeListeners_LegacyAuditErrorHasOnePrefix(t *testing.T) {
	preMetrics := metrics.Default.SnapshotNamesForTest()
	t.Cleanup(func() { metrics.Default.UnregisterAddedSinceForTest(preMetrics) })
	ctx := context.Background()
	cacheDir := t.TempDir()
	c, err := cache.Open(ctx, cacheDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A queued inline snapshot whose anchor is not in the (empty) pool:
	// the audit's pre-flight refuses the start.
	h := strings.Repeat("ab", 32)
	if err := c.PutBlob(ctx, h, 1); err != nil {
		t.Fatal(err)
	}
	id, _, err := c.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = serveListeners(ctx, minimalCfg(cacheDir, nil), silentBuildLogger(), ln, nil, nil, nil)
	if err == nil {
		t.Fatal("serveListeners started with the pool's anchors missing")
	}
	if n := strings.Count(err.Error(), "legacy trust audit:"); n != 1 {
		t.Fatalf("the error carries the prefix %d times: %v", n, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/apt-cacher-ultra/ -run 'TestBuildAdopter_BuildsTheFlipBarrier|TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced|TestWaitLogged_ExplainsASlowAdoptionDrain|TestServeListeners_LegacyAuditErrorHasOnePrefix'`
Expected: compile failure naming `undefined: handlerConfig`.

- [ ] **Step 3: Implement**

In `cmd/apt-cacher-ultra/main.go`, add the import `"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"` (after the `fetch` import).

In `buildAdopter`, directly before `adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{`, insert:

```go
	// Spec C8: the flip barrier. The handler registers anchor serves with
	// the same gate (Adopter.FlipGate); 0s disables it.
	flipGate, err := flipgate.New(flipgate.Config{Quiet: cfg.Adoption.FlipQuietPeriod.Duration})
	if err != nil {
		return nil, nil, fmt.Errorf("build flip barrier: %w", err)
	}

```

In the `freshness.AdoptionConfig` literal, after `MaxDuration: cfg.Adoption.MaxDuration.Duration,` add `FlipGate: flipGate,` (gofmt aligns it). In the `"phase2 adoption enabled"` log call, after `"max_duration", cfg.Adoption.MaxDuration.Duration,` add:

```go
		"flip_quiet_period", cfg.Adoption.FlipQuietPeriod.Duration,
```

In the `"apt-cacher-ultra starting"` log call, after `"adoption_hot_prefetch_budget", cfg.Adoption.HotPrefetchBudget.Duration,` add:

```go
		"adoption_flip_quiet_period", cfg.Adoption.FlipQuietPeriod.Duration,
```

Replace the `h, err := handler.New(handler.Config{ … })` statement (through its closing `})`) with:

```go
	h, err := handler.New(handlerConfig(cfg, parser, c, fetchClient, hostLimiter, logger, freshChecker, adopter))
```

Deferred minor A-48: replace

```go
	if _, err := freshness.RunLegacyTrustAudit(ctx, c, logger); err != nil {
		return fmt.Errorf("legacy trust audit: %w", err)
	}
```

with

```go
	if _, err := freshness.RunLegacyTrustAudit(ctx, c, logger); err != nil {
		return err // already prefixed "legacy trust audit:"
	}
```

Deferred minor B-64: replace

```go
	// rolls the transaction back). This Wait makes the shutdown
	// deterministic before the cache is closed by the deferred
	// c.Close().
	freshChecker.WaitForAdoptions()
```

with

```go
	// rolls the transaction back). This Wait makes the shutdown
	// deterministic before the cache is closed by the deferred
	// c.Close(); past adoptionDrainNotice it is explained in the journal.
	// No adoption is in the flip barrier any more: the gate was closed at
	// the signal.
	waitLogged(freshChecker.WaitForAdoptions, adoptionDrainNotice, logger)
```

Design note 18: directly after the `select` that waits for the shutdown signal (the one that logs `shutdown signal received`), before the `// SPEC §9.5 step 2: stop accepting` comment, insert:

```go

	// Spec C8 (phase C plan, design note 18): close the flip barrier
	// before anything drains. An adoption waiting in it abandons its commit
	// at once (and records nothing), held InRelease/Release requests are
	// answered, and no commit starts late in the stop window. A commit
	// already running is waited for below (waitLogged).
	adopter.FlipGate().Close()
```

After `buildAdopter` (before the `keyringProvider` comment) add:

```go
// handlerConfig assembles the request handler's configuration. The flip
// barrier is the adopter's — nil with adoption disabled — so anchor serves
// and commits meet at one gate (spec C8); retained by-hash searches the
// displaced snapshots GC keeps (gc.keep_displaced).
func handlerConfig(
	cfg *config.Config,
	parser *proxy.Parser,
	c *cache.Cache,
	fetchClient *fetch.Client,
	hostLimiter *hostsem.Sem,
	logger *slog.Logger,
	freshChecker *freshness.Checker,
	adopter *freshness.Adopter,
) handler.Config {
	return handler.Config{
		Parser:              parser,
		Cache:               c,
		Fetch:               fetchClient,
		HostLimiter:         hostLimiter,
		Logger:              logger,
		Freshness:           freshChecker,
		Serve:               cfg.Serve,
		RefuseUnvouchedDebs: cfg.Integrity.RefuseUnvouchedDebs,
		AdoptionEnabled:     cfg.Adoption.Enabled,
		FlipGate:            adopter.FlipGate(),
		KeepDisplaced:       cfg.GC.KeepDisplaced,
	}
}

// adoptionDrainNotice is how long the shutdown waits on in-flight
// adoptions before it explains the wait (deferred minor B-64).
const adoptionDrainNotice = 2 * time.Second

// waitLogged runs wait and, once it has run for notice, logs
// shutdown_waiting_for_adoptions at INFO (once), so a slow stop is
// explained in the journal: the drain waits for adoption, repair and
// reconcile goroutines, and a commit already running is never cancelled
// and may take up to the 5-minute commit timeout. It returns when wait
// does.
func waitLogged(wait func(), notice time.Duration, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logger.Error("shutdown wait panicked", "panic", r)
			}
		}()
		wait()
	}()
	timer := time.NewTimer(notice)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		logger.Info("shutdown_waiting_for_adoptions",
			"waited", notice,
			"detail", "an adoption, repair or reconcile is still finishing: a commit already running is never cancelled (spec C4) and takes at most the 5-minute commit timeout; other work stops at its next cancellation point")
	}
	<-done
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./cmd/apt-cacher-ultra/`
Expected: PASS (`minimalCfg` uses `Defaults()`, which leaves `FlipQuietPeriod` at 0, so the existing wiring tests run without the barrier).

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `buildAdopter`, add `flipGate = nil` right after the `build flip barrier` error check → `TestBuildAdopter_BuildsTheFlipBarrier` FAILS ("quiet 10s: gate quiet 0s, enabled false"); (b) in `handlerConfig`, drop `FlipGate: adopter.FlipGate(),` → `TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced` FAILS; (c) drop `KeepDisplaced: cfg.GC.KeepDisplaced,` → the same test FAILS ("KeepDisplaced = 0"); (d) in `waitLogged`, make the timer case `<-done` and return without logging → `TestWaitLogged_ExplainsASlowAdoptionDrain` FAILS ("a 1m drain logged 0 notices"); (e) restore `return fmt.Errorf("legacy trust audit: %w", err)` → `TestServeListeners_LegacyAuditErrorHasOnePrefix` FAILS ("the error carries the prefix 2 times"); (f) remove the `a == nil` check from Task 5's `(*Adopter).FlipGate` → `TestHandlerConfig_SharesTheFlipGateAndKeepDisplaced` FAILS (nil pointer dereference).

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/flip_wiring_test.go
git add cmd/apt-cacher-ultra/main.go cmd/apt-cacher-ultra/flip_wiring_test.go
git commit -m "Wire the flip barrier and retained by-hash into the daemon"
```

---

### Task 10: End-to-end flip simulations

Spec C8 tests, end to end: the real cache, adopter, barrier and handler in `testing/synctest` bubbles, driven by Task 2's apt client model (`simtest.Ledger`; plan "Definitions"). They prove the barrier's purpose (a busy suite straddles without it and never with it, inline and detached — design review M16: a detached client's `Release.gpg` must come from its `Release` snapshot), the review's shared-queue trace through the real stack, the hold bound, and zero straddles under seeded chaos. The phase-2 chaos tests' logged straddle counts become assertions, with their AIDEV-NOTEs updated (see Conventions); neither burst crosses the flip, so those assertions pin the burst structure and are not chaos coverage of the barrier (design review M17) — `TestSimFlip_SeededChaos` is.

**Files:**
- Create: `internal/handler/sim_flip_test.go`
- Modify: `internal/handler/phase2_chaos_test.go`, `internal/handler/phase2_detached_chaos_test.go` (AIDEV-NOTE and the straddle-count lines only)

**Interfaces:**
- Consumes: Task 2 (`simtest.NewLedger`, `Ledger.RunUpdate`, `Straddles`, `Checked`, `AnchorGens`, `simtest.Request`, `simtest.ServeFunc`), Task 1 (`flipgate.PhaseAMax`, `HoldMax`, `Gate.Stats`), Task 6's harness (`flipStack`, `newFlipStack`, `publish`, `adopt`, `get`, `flipSimHost`). Existing: `sumMetricLines` (`disabled_mode_parity_test.go`), `chaos2CountMetadataStraddles` (`phase2_chaos_test.go`).
- Produces: the ledger adapters `(*flipStack).serve`, `(*flipStack).serveDetached` and `flipReaders`, `(*flipStack).adoptDetached`, `busySuiteSim` and `recordAnchorHeld` (test helpers, package `handler`).

Every simulation creates its stack inside the bubble, sets the suites up (each first adoption takes a zero-length hold that holds nothing, so a second suite's first adoption does not wait for it — design note 17), and only then takes `t0` and a `Stats` baseline. Flips start at least 1 s after the readers, off the readers' 250/500 ms grids.

- [ ] **Step 1: Write the simulations**

`internal/handler/sim_flip_test.go`:

```go
package handler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// serve answers ledger requests through the handler (Request.Suite is the
// suite path): anchors are InRelease, indexes the suite's Packages. The
// cache held a request for whatever the serve took beyond its transfer.
func (s *flipStack) serve(anchorTransfer, indexTransfer time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		path, transfer := rq.Suite+"/main/binary-amd64/Packages", indexTransfer
		if rq.Anchor {
			path, transfer = rq.Suite+"/InRelease", anchorTransfer
		}
		start := time.Now()
		gen, code := s.get(path, transfer)
		if code != http.StatusOK {
			s.t.Errorf("GET %s = %d", path, code)
		}
		return gen, max(time.Since(start)-transfer, 0)
	}
}

// adoptDetached is adopt for the detached form: the snapshot's anchors are
// Release and Release.gpg.
func (s *flipStack) adoptDetached(suite string, gen int) {
	ref, rel := s.publish(suite, gen)
	sig := []byte(fmt.Sprintf("signature of %s generation %d", suite, gen))
	if err := s.ad.RunDetached(context.Background(), ref, rel, sig, "", ""); err != nil {
		s.t.Errorf("adopt %s generation %d (detached): %v", suite, gen, err)
	}
}

// serveDetached is serve for a detached suite: an anchor is Release and
// then Release.gpg, fetched as apt does on the client's one connection,
// and the anchor has arrived once both have. A Release.gpg from another
// snapshot than the Release before it is counted in sigMismatch.
func (s *flipStack) serveDetached(anchorTransfer, indexTransfer time.Duration, sigMismatch *atomic.Int64) simtest.ServeFunc {
	index := s.serve(anchorTransfer, indexTransfer)
	return func(rq simtest.Request) (string, time.Duration) {
		if !rq.Anchor {
			return index(rq)
		}
		start := time.Now()
		gen, code := s.get(rq.Suite+"/Release", anchorTransfer)
		held := max(time.Since(start)-anchorTransfer, 0)
		sigGen, sigCode := s.get(rq.Suite+"/Release.gpg", indexTransfer)
		if code != http.StatusOK || sigCode != http.StatusOK {
			s.t.Errorf("GET %s/Release = %d, Release.gpg = %d", rq.Suite, code, sigCode)
		}
		if sigGen != gen {
			sigMismatch.Add(1)
		}
		return gen, held
	}
}

// flipReaders starts, every period from offset until until, a client
// that runs one update of queue through serve.
func flipReaders(l *simtest.Ledger, serve simtest.ServeFunc, queue []simtest.Request, offset, period time.Duration, until time.Time, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(offset)
		for i := 0; time.Now().Before(until); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.RunUpdate(i, queue, serve)
			}()
			time.Sleep(period)
		}
	}()
}

// recordAnchorHeld wraps serve and records how long each anchor was held.
func recordAnchorHeld(serve simtest.ServeFunc, held *[]time.Duration) simtest.ServeFunc {
	return func(rq simtest.Request) (string, time.Duration) {
		gen, h := serve(rq)
		if rq.Anchor {
			*held = append(*held, h)
		}
		return gen, h
	}
}

// Spec C8's purpose, end to end (§7: a busy suite, an anchor every 500 ms
// for 120 s across a flip): clients read the anchor over 1 s links and ask
// for its index right after. Without the barrier
// (flip_quiet_period = 0s, today's behaviour) the flip lands while two
// anchors are in flight, and those clients get the new index for the old
// anchor. With it (10s) phase A gives up at its cap, the hold holds new
// anchor requests until the in-flight ones are done and the suite has
// been quiet for 10 s, and no client straddles.
func TestSimFlip_BusySuiteStraddlesOnlyWithoutTheBarrier(t *testing.T) {
	busySuiteSim(t, false)
}

// The same for a detached suite (design review M16): each client fetches
// Release, then Release.gpg — never held — then the index. Release.gpg
// must come from its client's Release snapshot: apt asks for it right after
// Release, within the quiet period.
func TestSimFlip_DetachedBusySuiteStraddlesOnlyWithoutTheBarrier(t *testing.T) {
	busySuiteSim(t, true)
}

// busySuiteSim runs the busy-suite simulation at 0s and 10s, for the
// inline or the detached form.
func busySuiteSim(t *testing.T, detached bool) {
	for _, quiet := range []time.Duration{0, 10 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			s := newFlipStack(t, quiet)
			const suite = "/u/dists/busy"
			adopt := s.adopt
			var sigMismatch atomic.Int64
			serve := s.serve(time.Second, 100*time.Millisecond)
			if detached {
				adopt = s.adoptDetached
				serve = s.serveDetached(time.Second, 100*time.Millisecond, &sigMismatch)
			}
			adopt(suite, 1)
			before := s.gate.Stats()
			l := simtest.NewLedger(10 * time.Second)
			var wg sync.WaitGroup
			t0 := time.Now()
			flipReaders(l, serve, []simtest.Request{{Suite: suite, Anchor: true}},
				11*time.Millisecond, 500*time.Millisecond, t0.Add(120*time.Second), &wg)
			flipAt := 30*time.Second + 7*time.Millisecond
			time.Sleep(flipAt)
			adopt(suite, 2)
			committed := time.Since(t0)
			wg.Wait()

			straddles := l.Straddles()
			if quiet == 0 {
				if committed != flipAt {
					t.Fatalf("without the barrier the commit came %v after the flip started, want at once", committed-flipAt)
				}
				if len(straddles) != 2 {
					t.Fatalf("without the barrier: %d straddles, want the 2 clients whose anchors were in flight: %+v", len(straddles), straddles)
				}
				if n := sigMismatch.Load(); detached && n != 2 {
					t.Fatalf("without the barrier: %d Release.gpg responses from another snapshot than their Release, want those 2 clients'", n)
				}
				return
			}
			if len(straddles) != 0 {
				t.Fatalf("with the barrier: %d straddles, first %+v", len(straddles), straddles[0])
			}
			if n := sigMismatch.Load(); n != 0 {
				t.Fatalf("with the barrier: %d Release.gpg responses from another snapshot than their Release", n)
			}
			holdStart := flipAt + flipgate.PhaseAMax // the suite is never naturally quiet
			if committed < holdStart+quiet || committed > holdStart+flipgate.HoldMax {
				t.Fatalf("committed at %v, want within the hold from %v, after the quiet period", committed, holdStart)
			}
			if st := s.gate.Stats(); st.Holds-before.Holds != 1 || st.HoldsExpired != before.HoldsExpired || st.HeldRequests == before.HeldRequests {
				t.Fatalf("stats %+v (before %+v): want one hold, ended normally, that held requests", st, before)
			}
			if gens := l.AnchorGens(suite); len(gens) != 2 {
				t.Fatalf("clients saw anchor generations %v, want two (they crossed the flip)", gens)
			}
		})
	}
}

// Spec C8 DoD, the review's overlapping-hold trace (plan review
// 2026-10-05, finding 6) through the real handler, adopter and cache, as
// in internal/flipgate's model simulation: busy suites a and b on one
// host leave phase A at its cap 0.1 s apart; the trace client gets a's
// old anchor just before a's hold, spends 0.3 s on an unrelated download,
// then asks for b's anchor with a's index queued behind it. b is not
// holding yet (one hold at a time), so neither anchor waits and the a
// index arrives before a commits. 100 shared-queue clients cross both
// flips; b commits at least a quiet period after a.
func TestSimFlip_SharedQueueTraceEndToEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const a, b = "/mirror/dists/a", "/mirror/dists/b"
		s.adopt(a, 1)
		s.adopt(b, 1) // at once: a's setup hold held nothing (design note 17)
		before := s.gate.Stats()
		fast := s.serve(50*time.Millisecond, 10*time.Millisecond)
		l := simtest.NewLedger(10 * time.Second)
		var wg sync.WaitGroup
		t0 := time.Now()
		until := t0.Add(100 * time.Second)
		flipReaders(l, fast, []simtest.Request{{Suite: a, Anchor: true}}, 11*time.Millisecond, 2*time.Second, until, &wg)
		flipReaders(l, fast, []simtest.Request{{Suite: b, Anchor: true}}, 1011*time.Millisecond, 2*time.Second, until, &wg)
		wg.Add(1)
		go func() { // another b reader, on a slow link, still reading when b's hold could start
			defer wg.Done()
			time.Sleep(60950 * time.Millisecond)
			l.RunUpdate(1000, []simtest.Request{{Suite: b, Anchor: true}}, s.serve(6*time.Second, 10*time.Millisecond))
		}()
		var traceHeld []time.Duration
		wg.Add(1)
		go func() { // the trace client
			defer wg.Done()
			time.Sleep(60853 * time.Millisecond)
			l.RunUpdate(2000, []simtest.Request{{Suite: a, Anchor: true}, {Filler: 300 * time.Millisecond}, {Suite: b, Anchor: true}},
				recordAnchorHeld(fast, &traceHeld))
		}()
		for i := 0; i < 100; i++ { // shared-queue clients, both orders
			q := []simtest.Request{{Suite: a, Anchor: true}, {Suite: b, Anchor: true}}
			if i%2 == 1 {
				q[0], q[1] = q[1], q[0]
			}
			at := 51*time.Second + 7*time.Millisecond + time.Duration(i)*450*time.Millisecond
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(at)
				l.RunUpdate(3000+i, q, fast)
			}()
		}
		var aAt, bAt time.Time
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Second + 3*time.Millisecond) // both suites are busy by now
			s.adopt(a, 2)
			aAt = time.Now()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Second + 103*time.Millisecond)
			s.adopt(b, 2)
			bAt = time.Now()
		}()
		wg.Wait()

		if len(traceHeld) != 2 || traceHeld[0] != 0 || traceHeld[1] != 0 {
			t.Fatalf("the trace client's anchors were held %v, want neither", traceHeld)
		}
		if st := l.Straddles(); len(st) != 0 {
			t.Fatalf("%d straddles, first %+v", len(st), st[0])
		}
		if bAt.Sub(aAt) < 10*time.Second {
			t.Fatalf("b committed %v after a, want at least the quiet period (one hold at a time, then cross-suite quiet)", bAt.Sub(aAt))
		}
		if st := s.gate.Stats(); st.Holds-before.Holds != 2 || st.HoldsExpired != before.HoldsExpired || st.HoldsSkipped != before.HoldsSkipped {
			t.Fatalf("stats %+v (before %+v): want two holds, none expired or skipped", st, before)
		}
		for _, k := range []string{a, b} {
			if gens := l.AnchorGens(k); len(gens) != 2 {
				t.Errorf("%s: clients saw anchor generations %v, want two (they crossed the flip)", k, gens)
			}
		}
		if n := l.Checked(); n < 250 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}

// Spec C8: a reader on a 25 s link keeps the suite from ever going quiet
// during the hold, so the hold ends at its 20 s bound, the commit starts
// at once, and the expiry is counted.
func TestSimFlip_SlowReaderEndsTheHoldAtHoldMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		const suite = "/u/dists/slow"
		s.adopt(suite, 1)
		before := s.gate.Stats()
		expired := sumMetricLines(t, []string{"acu_flip_hold_expired_total"})["acu_flip_hold_expired_total"]
		l := simtest.NewLedger(10 * time.Second)
		var wg sync.WaitGroup
		t0 := time.Now()
		flipReaders(l, s.serve(time.Second, 10*time.Millisecond), []simtest.Request{{Suite: suite, Anchor: true}},
			11*time.Millisecond, 500*time.Millisecond, t0.Add(100*time.Second), &wg)
		wg.Add(1)
		go func() { // starts half a second before the hold, on a 25 s link
			defer wg.Done()
			time.Sleep(60503 * time.Millisecond)
			l.RunUpdate(1000, []simtest.Request{{Suite: suite, Anchor: true}}, s.serve(25*time.Second, 10*time.Millisecond))
		}()
		flipAt := time.Second + 3*time.Millisecond
		time.Sleep(flipAt)
		s.adopt(suite, 2)
		committed := time.Since(t0)
		wg.Wait()

		if want := flipAt + flipgate.PhaseAMax + flipgate.HoldMax; committed != want {
			t.Fatalf("committed at %v, want at the hold's bound, %v", committed, want)
		}
		if st := s.gate.Stats(); st.Holds-before.Holds != 1 || st.HoldsExpired-before.HoldsExpired != 1 {
			t.Fatalf("stats %+v (before %+v): want one hold, expired", st, before)
		}
		if got := sumMetricLines(t, []string{"acu_flip_hold_expired_total"})["acu_flip_hold_expired_total"] - expired; got != 1 {
			t.Fatalf("acu_flip_hold_expired_total rose by %v, want 1", got)
		}
	})
}

// Spec C8 under seeded chaos: three suites on one host; 160 clients over
// four minutes with random queues (a random set of the suites' anchors in
// random order, sometimes behind an unrelated download), random link
// speeds (up to 3 s) and start times; two flips per suite at random times,
// the second 30-90 s after the first was due. All random values are drawn
// before anything runs, so the run is reproducible. Every hold ends
// normally and no flip skips its hold, so no client may straddle; clients
// cross all three generations of every suite.
func TestSimFlip_SeededChaos(t *testing.T) {
	type clientPlan struct {
		at       time.Duration
		transfer time.Duration // its anchors' transfer time
		queue    []simtest.Request
	}
	rng := rand.New(rand.NewPCG(1, 2))
	suites := []string{"/m/dists/a", "/m/dists/b", "/m/dists/c"}
	plans := make([]clientPlan, 160)
	for i := range plans {
		p := clientPlan{
			at:       time.Duration(rng.Int64N(int64(240 * time.Second))),
			transfer: 10*time.Millisecond + time.Duration(rng.Int64N(int64(3*time.Second))),
		}
		for _, j := range rng.Perm(len(suites))[:1+rng.IntN(len(suites))] {
			if rng.IntN(3) == 0 {
				p.queue = append(p.queue, simtest.Request{Filler: time.Duration(rng.Int64N(int64(2 * time.Second)))})
			}
			p.queue = append(p.queue, simtest.Request{Suite: suites[j], Anchor: true})
		}
		plans[i] = p
	}
	flips := make([][2]time.Duration, len(suites))
	for i := range flips {
		first := 20*time.Second + time.Duration(rng.Int64N(int64(60*time.Second)))
		flips[i] = [2]time.Duration{first, first + 30*time.Second + time.Duration(rng.Int64N(int64(60*time.Second)))}
	}

	synctest.Test(t, func(t *testing.T) {
		s := newFlipStack(t, 10*time.Second)
		for _, su := range suites {
			s.adopt(su, 1)
		}
		before := s.gate.Stats()
		l := simtest.NewLedger(10 * time.Second)
		var wg sync.WaitGroup
		t0 := time.Now()
		for i, p := range plans {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(p.at)
				l.RunUpdate(i, p.queue, s.serve(p.transfer, 10*time.Millisecond))
			}()
		}
		for i, su := range suites {
			wg.Add(1)
			go func() { // a suite's flips run one after the other
				defer wg.Done()
				for n, at := range flips[i] {
					time.Sleep(time.Until(t0.Add(at)))
					s.adopt(su, n+2)
				}
			}()
		}
		wg.Wait()

		st := s.gate.Stats()
		if st.Holds-before.Holds != 6 || st.HoldsExpired != before.HoldsExpired || st.HoldsSkipped != before.HoldsSkipped {
			t.Fatalf("stats %+v (before %+v): want six holds, none expired or skipped", st, before)
		}
		if got := l.Straddles(); len(got) != 0 {
			t.Fatalf("%d straddles, first %+v", len(got), got[0])
		}
		for _, su := range suites {
			if gens := l.AnchorGens(su); len(gens) != 3 {
				t.Errorf("%s: clients saw anchor generations %v, want three (they crossed both flips)", su, gens)
			}
		}
		if n := l.Checked(); n < 200 {
			t.Errorf("only %d index responses checked", n)
		}
	})
}
```

- [ ] **Step 2: Run the simulations**

Run: `go test -race -count=1 -v ./internal/handler/ -run 'TestSimFlip_'`
Expected: five PASS lines, each under 5 s of wall time (the detached busy-suite simulation is the slowest, about 3.3 s). (They pass at once: Tasks 1–9 built what they exercise.)

- [ ] **Step 3: The phase-2 chaos tests assert their straddle counts**

In `internal/handler/phase2_chaos_test.go`, replace the AIDEV-NOTE paragraph on `TestPhase2Chaos_AdoptionFlipUnderConcurrency` (from `// AIDEV-NOTE: SPEC2 §12.3 calls for` through `// that is outside the scope of this test.`) with:

```go
// AIDEV-NOTE: SPEC2 §12.3 calls for "no client receives A's InRelease
// with B's Packages or vice versa." Every request reads
// current_snapshot_id independently; per-client consistency across a flip
// comes from the spec C8 flip barrier (internal/flipgate): the commit
// waits until the suite's anchor serves have been quiet for
// adoption.flip_quiet_period, so a client that asks for its indexes
// within that period of its anchor gets the same snapshot. The flip
// simulations (internal/handler/sim_flip_test.go, internal/flipgate's
// sim_test.go) prove it with the apt client model; TestSimFlip_SeededChaos
// is the barrier's chaos test. This test runs without a barrier, and
// neither burst crosses the flip (the pre-flip burst runs while adoption
// is blocked, the post-flip one after the commit), so its zero-straddle
// assertions pin the burst structure rather than exercise the barrier,
// next to the per-response coherence it asserts throughout.
```

and replace

```go
	// Per-client metadata coherence: log the count of clients whose
	// InRelease and Packages.gz responses came from different
	// snapshots. Architecturally this can be non-zero (see the
	// AIDEV-NOTE on the test function), but inflating it is a
	// regression signal.
	preStraddles := chaos2CountMetadataStraddles(pre)
	postStraddles := chaos2CountMetadataStraddles(post)
	t.Logf("metadata straddle counts: pre-flip=%d post-flip=%d (architecturally allowed; inflation = regression)",
		preStraddles, postStraddles)
```

with

```go
	// Per-client metadata coherence: no client's InRelease and
	// Packages.gz responses came from different snapshots (neither
	// burst crosses the flip; see the AIDEV-NOTE on the test function).
	if preN, postN := chaos2CountMetadataStraddles(pre), chaos2CountMetadataStraddles(post); preN != 0 || postN != 0 {
		t.Errorf("metadata straddles: pre-flip=%d post-flip=%d, want 0", preN, postN)
	}
```

In `internal/handler/phase2_detached_chaos_test.go`, replace

```go
// AIDEV-NOTE: SPEC2 §12.3's per-client coherence claim is documented
// elsewhere as architecturally permitting straddle (see
// TestPhase2Chaos_AdoptionFlipUnderConcurrency); the same caveat
// applies here. We log straddle counts for regression tracking and
// strictly assert per-request coherence.
```

with

```go
// AIDEV-NOTE: SPEC2 §12.3's per-client coherence comes from the spec C8
// flip barrier (see TestPhase2Chaos_AdoptionFlipUnderConcurrency); as
// there, neither burst crosses the flip, so its zero-straddle assertions
// pin the burst structure (they do not exercise the barrier), and it
// strictly asserts per-request coherence.
```

and replace

```go
	preStraddles := chaos2CountMetadataStraddles(pre)
	postStraddles := chaos2CountMetadataStraddles(post)
	t.Logf("detached metadata straddle counts: pre-flip=%d post-flip=%d (architecturally allowed; inflation = regression)",
		preStraddles, postStraddles)
```

with

```go
	if preN, postN := chaos2CountMetadataStraddles(pre), chaos2CountMetadataStraddles(post); preN != 0 || postN != 0 {
		t.Errorf("detached metadata straddles: pre-flip=%d post-flip=%d, want 0", preN, postN)
	}
```

- [ ] **Step 4: Run the handler package**

Run: `go test -race -count=1 ./internal/handler/`
Expected: PASS.

- [ ] **Step 5: Mutation check (do not commit)**

One at a time, reverting each (verified against this plan's code): (a) in `internal/handler/handler.go` `anchorStart`, make the first condition `if true || !h.flipGate.Enabled() || …` (nothing registers) → `TestSimFlip_BusySuiteStraddlesOnlyWithoutTheBarrier` FAILS ("with the barrier: 2 straddles"); (b) in `internal/flipgate/gate.go` `acquire`, change `if g.holder == nil && len(g.queue) == 0 {` to `if true {` (concurrent holds) → `TestSimFlip_SharedQueueTraceEndToEnd` FAILS ("the trace client's anchors were held [0s 19.754s]"); (c) make `otherHoldsQuietLocked` return `true, time.Time{}` first → the same test FAILS ("b committed 9.45s after a"); (d) in `runHold`, make the bound `expiry := h.start.Add(2 * g.holdMax)` → `TestSimFlip_SlowReaderEndsTheHoldAtHoldMax` FAILS ("committed at 1m41.003s, want … 1m21.003s"; do not disable the expiry check instead — the phase-B loop then spins without advancing virtual time and the run hangs until the test timeout); (e) make `localQuietLocked` return `true, time.Time{}` first → `TestSimFlip_SeededChaos` FAILS (14 straddles); (f) in `internal/handler/handler.go` `anchorStart`, drop `&& req.Path != req.SuitePath+releaseSuffix` (Task 6's mutant (a)) → `TestSimFlip_DetachedBusySuiteStraddlesOnlyWithoutTheBarrier` FAILS ("with the barrier: 2 straddles": a detached suite's `Release` serves no longer register).

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/handler/
git add internal/handler/sim_flip_test.go internal/handler/phase2_chaos_test.go internal/handler/phase2_detached_chaos_test.go
git commit -m "Prove the flip barrier end to end in virtual-clock simulations"
```

---

### Task 11: Documentation, spec rulings, deferred minors, final verification

Spec §8 (docs for C8 and C11), the rulings' spec and doc updates (R-C1 to R-C3), plan deviations 2, 3, 5 and 6 written into the spec, the review revision's documentation items (design review M1, M2, M3, M5, M12, M14, M18 and the B-64 notice), the E and G carry-forwards, the overview's phase C bookkeeping and carry-forwards, deferred minors A-7 and A-16 (spec wording), A-57 (verified already correct), and deletion of every deferred-minor entry this plan resolved. Then the full verification.

**Files:**
- Modify: `README.md`, `docs/configuration.md`, `docs/log-fields.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, `internal/freshness/scheduler.go` (the `Run` doc only), `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md`

**Interfaces:**
- Consumes: the behaviour of Tasks 1–10 as built (event fields from Task 5's `flip`, counters from Tasks 1 and 8, the request field from Task 8, `shutdown_waiting_for_adoptions` and `Gate.Close` at the signal from Task 9).
- Produces: documentation only.

Every replacement below names text that occurs exactly once in its file; if one does not match, stop and report it instead of guessing.

- [ ] **Step 1: README**

In `README.md`, replace

```markdown
- Atomic snapshot adoption — per-suite InRelease + all referenced Packages/by-hash blobs are staged, GPG-verified, and
  flipped in a single SQLite transaction so clients always see a coherent metadata set.
```

with

```markdown
- Atomic snapshot adoption — per-suite InRelease + all referenced Packages/by-hash blobs are staged, GPG-verified, and
  flipped in a single SQLite transaction. The flip waits (bounded) until clients that received the old InRelease have
  had `adoption.flip_quiet_period` to fetch its indexes, so the index requests an apt client sends within that period
  of its InRelease match it (by-hash requests are served from the displaced snapshot even later); the residuals (a
  suite that never goes quiet within the bounds, responses slower than apt's own timeout, more than 256 held requests)
  each have a counter and are documented in [Flip consistency](docs/configuration.md#flip-consistency).
```

- [ ] **Step 2: `docs/configuration.md`**

(a) In the `[adoption]` table, replace the `max_duration` row

```markdown
| `max_duration` | duration | `"30m"` | Longest an adoption, repair or reconcile may run. An abandoned adoption is retried on the normal schedule; a repair or reconcile runs again on the next unchanged check. The deadline never cuts the final commit short: the commit runs under its own 5-minute bound. On shutdown an adoption that has not reached its commit is abandoned at once, but a commit already running is waited for, up to those 5 minutes; the packaged systemd unit's `TimeoutStopSec=35` may kill the process first, in which case SQLite rolls the commit back and nothing is lost. At least `"1m"`. |
```

with

```markdown
| `max_duration` | duration | `"30m"` | Longest an adoption, repair or reconcile may run. An abandoned adoption is retried on the normal schedule; a repair or reconcile runs again on the next unchanged check. The deadline never cuts the final commit short: the commit runs under its own 5-minute bound. Nor does it cut the flip barrier short (at most 140 s, see *Flip consistency*), which only a shutdown ends early. On shutdown an adoption that has not reached its commit is abandoned at once, but a commit already running is waited for, up to those 5 minutes; the packaged systemd unit's `TimeoutStopSec=35` may kill the process first, in which case SQLite rolls the commit back and nothing is lost. At least `"1m"`. |
| `flip_quiet_period` | duration | `"10s"` | How long a suite's `InRelease`/`Release` serves must have been quiet before a new snapshot replaces the current one, so a client that just received the old one fetches matching indexes (see *Flip consistency*). `"0s"` disables the barrier: adoptions commit at once, as before. Otherwise between `"1s"` and `"15s"`: a shorter period protects almost no one, and a longer one leaves less of a hold's 20 s for the responses already being sent when it starts, so more holds reach their bound (`acu_flip_hold_expired_total`). |
```

(b) In *Capacity and fairness*, replace

```markdown
`POST /reconcile` answers 409 when no slot is free. The gauge `acu_freshness_waiting_for_slot` counts suites waiting for a slot.
```

with

```markdown
`POST /reconcile` answers 409 when no slot is free. The gauge `acu_freshness_waiting_for_slot` counts suites waiting for a slot. An adoption gives its host's slot back when its downloads are done, but keeps its global slot through the flip barrier and the commit, so a slot is held for at most `max_duration` + 140 s (the barrier, see *Flip consistency*; `acu_adoption_duration_seconds` includes that time) + the 5-minute commit bound: N suites waiting for k slots they may use are all admitted within one tick + ⌈N/k⌉ × (that sum + 2 minutes).
```

(c) Directly before the line `### Trust and signer pins`, insert:

```markdown
### Flip consistency

apt fetches a suite's `InRelease` (or `Release`) first and its indexes after, one request at a time on one connection. If a new snapshot replaced the current one in between, the client would get indexes that do not match its `InRelease`, and `apt update` would fail with hash-sum mismatches. Before switching a suite to a new snapshot, an adoption therefore waits:

1. up to 60 s for the suite to go quiet by itself: none of its `InRelease`/`Release` responses being sent, none finished within `flip_quiet_period`, no other suite's switch holding requests, and none that held requests ended within that period;
2. up to 60 s for its turn to hold (one suite holds at a time);
3. then it holds: new `InRelease`/`Release` requests of the suite wait (`Release.gpg` never does) until the responses already being sent are done and the suite has been quiet for `flip_quiet_period`, and no other suite's hold that held requests ended within that period. Then the new snapshot is committed and the waiting requests are answered from it. A hold lasts at most 20 s, below apt's 30 s timeout.

A client that received the old `InRelease` therefore either finished receiving it at least `flip_quiet_period` before the switch, or waited and got the new one, so the index requests it sends within that period of its `InRelease` match it — also when many suites share its connection (mirror mode, or more than ten hosts). The period runs from when the cache finished sending, so on a slow link it is slightly shorter for the client. A client that fetches many indexes over a slow link may send some later than that; with apt's default `Acquire-By-Hash` those are still served from the snapshot they name (below). A switch takes at most 60 s + 60 s + 20 s plus its commit; `adoption.max_duration` does not cut it short. A shutdown abandons it at once: at the signal the daemon answers the waiting requests and abandons every switch that has not started its commit (nothing is committed or recorded, and the change is adopted after the restart). The first adoption of a suite whose `InRelease` is exactly the one clients were already being served skips the wait. Each switch logs `adoption_flip_barrier`.

Residuals, each with a counter: a hold that reaches 20 s (a very slow commit, or a client still receiving the old `InRelease`) answers its waiters from the snapshot current then and commits at once (`acu_flip_hold_expired_total`); a switch that waits 60 s for its turn commits without holding (`acu_flip_hold_skipped_total`; it takes three or four long holds ahead of it, so at least four suites switching at once, which needs `freshness.max_concurrent_adoptions` of 4 or more, or `0`); beyond 256 requests waiting in one hold, the next are answered at once (`acu_flip_waiters_over_cap_total`); and `InRelease`/`Release` responses that take longer than 30 s to send (very slow links) are not waited for (`acu_flip_slow_anchor_reads_total`). apt clients configured with an `Acquire::http::Timeout` below 20 s can time out while held; set `flip_quiet_period = "0s"` if you run such clients. A client still holding an older `InRelease` that asks for indexes by hash (apt's default `Acquire-By-Hash`) is still served: a by-hash file the current snapshot lacks is served from the newest `gc.keep_displaced` + 1 displaced snapshots of its suite (`acu_serve_byhash_displaced_total`; the request line carries `snapshot_displaced`). Snapshot `InRelease`, `Release` and `Release.gpg` responses carry a `Last-Modified` that grows with every switch — also for two switches in the same second and across a backward clock step between switches — so `If-Modified-Since` does not hide a new snapshot; a clock that steps back while an adoption is running can leave the next switch's `Last-Modified` below the one served before it, for up to the length of the step.

```

(d) In the `[gc]` table, replace the `keep_displaced` row

```markdown
| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. Does not by itself retain superseded package binaries. |
```

with

```markdown
| `keep_displaced` | integer | `3` | Recent displaced snapshots kept per suite for inspection. `0` retains none; must be nonnegative. Does not by itself retain superseded package binaries. Also bounds retained by-hash: a by-hash request the current snapshot lacks is served from the newest `keep_displaced` + 1 displaced snapshots of its suite, and `0` turns that off (see *Flip consistency*). |
```

(e) Deferred minor A-57: run `grep -c 'unreadable or empty, or when two or more inline snapshots are queued and' docs/configuration.md`; expected `1`. That sentence of *Upgrading an existing cache* (the daemon refuses to start when `pool/` is missing, unreadable or empty, or when two or more inline snapshots are queued and none of their pool files exists) already covers what A-57 asked for. Change nothing there; Step 7 deletes A-57.

- [ ] **Step 3: `docs/log-fields.md`**

(a) In *Field-presence-as-signal pattern*, replace

```markdown
- `upstream_status` — emitted only when a fetch was attempted; value 0
  means "fetch attempted, no response arrived" (timeout, dial denied).
```

with

```markdown
- `upstream_status` — emitted only when a fetch was attempted; value 0
  means "fetch attempted, no response arrived" (timeout, dial denied).
- `snapshot_displaced` — emitted as `true` only when a by-hash request
  was served from one of its suite's displaced snapshots (spec C8
  retained by-hash, for a client holding an earlier `InRelease`);
  `X-Cache-Snapshot` names that snapshot.
```

(b) Replace

```markdown
gauge `acu_freshness_waiting_for_slot` (suites waiting for an adoption
slot).
```

with

```markdown
gauge `acu_freshness_waiting_for_slot` (suites waiting for an adoption
slot). The flip barrier (spec C8) adds one unlabelled counter per
residual — `acu_flip_hold_skipped_total` (a flip that waited 60 s for the
hold token and committed without a hold), `acu_flip_hold_expired_total`
(a hold that reached its 20 s bound before its commit finished),
`acu_flip_waiters_over_cap_total` (an anchor request answered at once
because 256 already waited in a hold) and
`acu_flip_slow_anchor_reads_total` (an `InRelease`/`Release` response
that took 30 s or longer to send) — and `acu_flip_gate_panics_total` (a
panic recovered in the barrier's hold-expiry timer: a bug, please report
it). Retained by-hash adds `acu_serve_byhash_displaced_total{host}`
(by-hash requests served from a displaced snapshot).
`acu_adoption_duration_seconds` includes the barrier's wait (at most
140 s).
```

(c) Replace

```markdown
detection, and the startup legacy trust audit (logged by
`internal/freshness` and `internal/handler`).
```

with

```markdown
detection, the startup legacy trust audit, the flip barrier and a slow
shutdown drain (logged by `internal/freshness`, `internal/handler` and
the daemon's `main`).
```

(d) After the table's last row (the one starting `| \`legacy_trust_audit_completed\` |`), append:

```markdown
| `adoption_flip_barrier` | INFO | `canonical_host`, `suite_path`, `snapshot_id`, `outcome` (`held`, `hold_expired`, `hold_skipped`, `not_needed`, `commit_failed`), `phase_a_seconds`, `token_wait_seconds`, `hold_seconds`, `held_requests` (the last four absent when the barrier was skipped for a first adoption) | An adoption committed behind the spec C8 flip barrier: `held` within its hold; `hold_expired` when the hold reached 20 s first (`acu_flip_hold_expired_total`); `hold_skipped` after 60 s without the hold token (`acu_flip_hold_skipped_total`); `not_needed` for a first adoption whose anchor clients were already being served; `commit_failed` when the commit ran and failed — the snapshot did not switch, the timing fields still describe the barrier, and `adoption_run_failed` gives the error. Not logged when `adoption.flip_quiet_period = 0s`, nor for a flip abandoned at shutdown (`adoption_run_failed` says so). |
| `displaced snapshot lookup failed` | WARN | `err`, `canonical_host`, `path` | Retained by-hash could not read the suite's displaced snapshots; the request is answered 404, as before spec C8. |
| `shutdown_waiting_for_adoptions` | INFO | `waited`, `detail` | Shutdown has waited 2 s for the adoption, repair and reconcile goroutines: a commit already running is never cancelled and may take up to its 5-minute bound; other work stops at its next cancellation point (adoptions in the flip barrier were abandoned at the signal). Logged once per shutdown. |
```

- [ ] **Step 4: The spec**

In `docs/superpowers/specs/2026-09-27-flat-repositories-design.md`:

(a) Header: replace `- **Date:** 2026-09-27 (revision 5, 2026-10-17:` with

```markdown
- **Date:** 2026-09-27 (revision 6, 2026-10-18: phase C plan rulings — the C8 barrier keeps the adoption's global slot and does not honour `adoption.max_duration` but ends on shutdown (the daemon closes it at the signal), so the admission bound's *A* includes it; a hold that held no request starts no cross-suite quiet period; `flip_quiet_period` is `0s` or `1s` to `15s`; every C8 residual has its own counter; the hold-token residual and the five-suite DoD corrected (four hold, the fifth commits unheld); retained by-hash serves SHA512 aliases from phase C; exact C2 predicate and C3 `Release.gpg` sniff wording; revision 5, 2026-10-17:
```

(b) A-7, C2: replace

```markdown
non-empty and not `.` or `..` (one flat exception below), and the decoded path
contains none of `#`, `?`, `%`, `\`.
```

with

```markdown
non-empty and not `.` or `..` (one flat exception below), and the decoded path
contains none of `#`, `?`, `%`, `\` and no control byte (0x00–0x1F, 0x7F).
```

(c) A-16, C3 sniff table: replace

```markdown
| `Release.gpg` | armor header `-----BEGIN PGP SIGNATURE-----`, or a binary OpenPGP signature packet (old-format tag bytes 0x88–0x8B, new-format 0xC2) |
```

with

```markdown
| `Release.gpg` | armor header `-----BEGIN PGP SIGNATURE-----` as the first non-whitespace text (leading whitespace is accepted; leading non-header lines, which gpg tolerates, are rejected), or a binary OpenPGP signature packet (old-format tag bytes 0x88–0x8B, new-format 0xC2) |
```

(d) R-C1/R-C2, C4 capacity: replace

```markdown
host slot is released when the member pipeline ends (before the C8 barrier);
the global slot is held through the commit. Every adoption, repair and
reconcile runs under a context deadline `adoption.max_duration` (default
`30m`); `CommitAdoption` itself runs under `context.WithoutCancel` with its
own bounded timeout, so the deadline cannot abort a commit already in
progress. `POST /reconcile` answers 409 when no slot is free.
```

with

```markdown
host slot is released when the member pipeline ends (before the C8 barrier);
the global slot is held through the C8 barrier and the commit. Every
adoption, repair and reconcile runs under a context deadline
`adoption.max_duration` (default `30m`); `CommitAdoption` itself runs under
`context.WithoutCancel` with its own bounded timeout, so the deadline cannot
abort a commit already in progress. The C8 barrier's waits do not honour the
deadline either (they are bounded by themselves, ≤ 140 s, and giving up would
discard finished member work), but they end at once on shutdown, and then
nothing is committed. `POST /reconcile` answers 409 when no slot is free.
```

(e) R-C3, C4 admission bound: replace

```markdown
⌈*N*/*k*⌉ × (*A* + *D*), where *A* — `adoption.max_duration` plus the commit
bound — is the longest a slot is held. With the default pool of two and a
```

with

```markdown
⌈*N*/*k*⌉ × (*A* + *D*), where *A* — `adoption.max_duration` plus the C8
barrier (≤ 140 s) plus the commit bound — is the longest a slot is held.
With the default pool of two and a
```

(f) Deviation 2 and design note 20 (with design review M18's wording), C8 residuals: replace

```markdown
Residuals (documented, counted): phase-B expiry when the commit or an
in-flight reader exceeds 20 s (F-2 on very large suites), hold-token timeout
(more than three suites flipping at once), waiters beyond the cap, and anchor
reads slower than apt's own timeout. A flip waiting for the token keeps its
global adoption slot, so with several suites flipping at once other
adoptions can queue for up to about 140 s (bounded).
```

with

```markdown
Residuals (documented, each with its own unlabelled counter): phase-B expiry
when the commit or an in-flight reader exceeds 20 s (F-2 on very large
suites; `acu_flip_hold_expired_total`), hold-token timeout (a flip times out
only behind 60 s of holds: at least three holds at their 20 s bound, in
practice four or more, since a queued flip's suite is usually busy and its
hold then lasts at least the quiet period — the five busy suites of §9 give
four holds and one skip — so it takes at least four suites flipping at once,
which needs `max_concurrent_adoptions` of 4 or more, or 0 for unlimited,
because a flip keeps its global slot through the barrier;
`acu_flip_hold_skipped_total`), waiters beyond the cap
(`acu_flip_waiters_over_cap_total`), and anchor reads slower than apt's own
timeout (`acu_flip_slow_anchor_reads_total`). A flip waiting for the token
keeps its global adoption slot, so while two or more suites flip other
adoptions can queue for up to about 140 s (bounded).
```

(g) Deviation 3, §7 C8 bullet: replace

```markdown
  the chaos test's straddle count asserted zero; retained by-hash (SHA256 in
  C, SHA512 in E) with stray-blob and retired-snapshot negatives;
```

with

```markdown
  the chaos test's straddle count asserted zero; retained by-hash (SHA256 and
  SHA512 in C; E's SHA512 adoption writes the SHA512 alias rows) with
  stray-blob and retired-snapshot negatives;
```

(h) §9 phase C row: replace

```markdown
| **C. Flip consistency** | C8 flipgate with the hold token, handler anchor hook, retained by-hash (SHA256, non-retired only), monotonic `Last-Modified`. | Straddle test fails on the B tip, passes here; overlapping-hold trace in a shared-queue simulation; five busy suites flipping within 0.1 s (holds never overlap, at most three hold, the rest commit unheld within 60 s + 20 s and are counted, zero straddles for held suites); busy-suite, cross-suite and slow-reader tests; hold ≤ 20 s; chaos straddles = 0; `flip_quiet_period = 0s` keeps existing tests unchanged. |
```

with

```markdown
| **C. Flip consistency** | C8 flipgate with the hold token, handler anchor hook, retained by-hash (SHA256 and SHA512, non-retired only), monotonic `Last-Modified`. | Straddle test fails on the B tip (the busy-suite simulation at `flip_quiet_period = 0s`), passes here; overlapping-hold trace in a shared-queue simulation; five busy suites flipping within 0.1 s (holds never overlap, four hold, the fifth commits unheld within 60 s + 20 s of the first hold and is counted, zero straddles for held suites); busy-suite, cross-suite and slow-reader tests; hold ≤ 20 s; chaos straddles = 0; `flip_quiet_period = 0s` keeps existing tests unchanged. |
```

(i) §9 phase E row: replace `SHA512 merge, shared verify helper, aliases, retained by-hash SHA512;` with `SHA512 merge, shared verify helper, aliases (served from displaced snapshots by phase C's retained by-hash);`.

(j) Deviation 5, C8 phase A: replace

```markdown
  at most `15s`), and (b) **no hold is in progress anywhere** and none ended
  within the same period (a hold delays other suites on the client's shared
  queue). Bounded by 60 s; on timeout proceed to phase B.
```

with

```markdown
  `1s` to `15s`), and (b) **no hold is in progress anywhere** and none
  that held at least one request ended within the same period (such a hold
  delayed other suites on the client's shared queue; one that held nothing
  delayed no one). Bounded by 60 s; on timeout proceed to phase B.
```

(k) Deviation 5, C8 phase B: replace

```markdown
  *other* suite's hold ended within `flip_quiet_period` (clients released
  from that hold may still have this suite's index requests queued behind
  it). Then run `commit`, clear pending, wake waiters onto the new snapshot
  and release the token. The whole of phase B, including both waits, is
```

with

```markdown
  *other* suite's hold that held at least one request ended within
  `flip_quiet_period` (clients released from that hold may still have this
  suite's index requests queued behind it). Then run `commit`, clear
  pending, wake waiters onto the new snapshot and release the token. The
  whole of phase B, including both waits, is
```

(l) Deviation 6, C11: replace `` | `adoption.flip_quiet_period` | `"10s"` | `0s` disables the barrier; ≤ `15s` | `` with `` | `adoption.flip_quiet_period` | `"10s"` | `0s` disables the barrier; otherwise `1s` to `15s` | ``.

- [ ] **Step 5: The scheduler's `Run` doc (R-C3)**

In `internal/freshness/scheduler.go`, replace

```go
// queued behind k usable slots are admitted within one tick + ⌈N/k⌉ ×
// (A + D), A = adoption.max_duration + commitTimeout. An offered re-check
```

with

```go
// queued behind k usable slots are admitted within one tick + ⌈N/k⌉ ×
// (A + D), A = adoption.max_duration + the spec C8 flip barrier (≤ 140 s:
// flipgate.PhaseAMax + TokenWaitMax + HoldMax; an adoption keeps its
// global slot through it) + commitTimeout. An offered re-check
```

- [ ] **Step 6: The overview**

In `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`:

(a) Replace `` `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 4). `` with `` `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 6). ``

(b) Replace the phases-table row

```markdown
| C. Flip consistency | `…-C-flip.md` | C8 incl. the hold token | B | to write after B |
```

with

```markdown
| C. Flip consistency | `…-C-flip.md` | C8 incl. the hold token | B | in progress (plan written 2026-10-18) |
```

(c) Replace the whole `- C: \`flipgate.Gate\` with …` bullet under *Cross-phase interfaces* (it ends `Retained by-hash serves only non-retired snapshots.`) with:

```markdown
- C: package `flipgate` — `Gate` (`New(Config{Quiet})`, `Enabled`, `Quiet`, `Stats`, `Len`, `Purge(key)`, `Close()` (shutdown; `main` calls it at the signal), `AnchorStart(ctx, key) *Token`, `Flip(ctx, key, commit) (Result, error)`), `Key(scheme, host, suitePath)` (the freshness suite key), `Token.Done`, outcomes `held`/`hold_expired`/`hold_skipped`/`aborted`/`disabled`, `ErrAborted`, `ErrClosed`, the spec C11 constants (`PhaseAMax`, `TokenWaitMax`, `HoldMax`, `AnchorTimeout`, `MaxWaiters`, `MinQuiet`, `MaxQuiet`) and the counters `acu_flip_hold_skipped_total`, `acu_flip_hold_expired_total`, `acu_flip_waiters_over_cap_total`, `acu_flip_slow_anchor_reads_total`, `acu_flip_gate_panics_total`. `freshness.AdoptionConfig.FlipGate` and the nil-safe `(*Adopter).FlipGate()` (the handler shares the adopter's gate: `handler.Config.FlipGate`); `handler.Config.KeepDisplaced`; `cache.DisplacedSnapshotMembers`; `config.AdoptionConfig.FlipQuietPeriod`; the event `adoption_flip_barrier` (outcome `commit_failed` when the commit fails); the request field `snapshot_displaced`; `acu_serve_byhash_displaced_total{host}`. Rulings: an adoption keeps its global slot through the barrier and frees its host slot before it (R-C1); the barrier's waits ignore `adoption.max_duration` and end on shutdown, committing nothing (R-C2); the admission bound's *A* includes the barrier's ≤ 140 s (R-C3). Five busy suites flipping at once: four hold, the fifth commits unheld (phase C plan, deviation 2).
```

(d) Under *Carry-forwards*, delete the `**C:**` heading, its four bullets and the empty line after them (from the line `**C:**` through the bullet starting `- *(B)* Add a Checker-level test that \`membersDone\` frees only the host slot.` and the blank line that follows it): phase C resolved all four (Tasks 5 and 11; rulings R-C1 to R-C4).

(e) Append to the `**D:**` list:

```markdown
- *(C)* The flip gate registers anchor serves only for suites that already have a row, so the serve that creates a suite's row is not registered (today: `anchorStart` runs before the miss path). A first adoption racing that client's index fetches could straddle it — narrow, because the first-adoption skip covers an unchanged upstream. When D creates the row before the body, register that serve with the gate right after `CreateSuite` succeeds. D's minted-suite scale test also asserts that `Adopter.FlipGate().Len()` stays within the suite caps (gate entries exist only for suites with rows).
```

(f) Append to the `**E:**` list:

```markdown
- *(C)* `servedAnchorUnchanged` (`internal/freshness/adoption.go`, `AIDEV-TODO(phase E)`) must compare `<suite>/Release` for the unsigned form. Retained by-hash already serves SHA512 aliases from displaced snapshots (phase C plan, deviation 3): E's SHA512 adoption only writes the alias rows, and its SHA512 by-hash test should include a displaced snapshot.
- *(C)* An in-commit refusal after a barrier that outlived `adoption.max_duration` must still be recorded as the refusal. `spawnAdoption` reclassifies an attempt as `timed_out` only when the deadline fired before the member pipeline finished (phase C plan, design note 19), so E's never-downgrade refusal must surface from `CommitAdoption` as its refusal sentinel through `classifyAdoptionOutcome`; E's refusal test should include a barrier that outlives the deadline.
```

(g) Append to the `**F:**` list:

```markdown
- *(C)* Spec C8: flip-gate entries are purged on reap. After deleting a suite row the reaper calls `Adopter.FlipGate().Purge(flipgate.Key(scheme, host, suitePath))` (nil-safe; a no-op while a flip of the suite runs; idle entries also expire on their own after twice the quiet period).
```

(h) Append to the `**G:**` list:

```markdown
- *(C)* If G serves url_path metadata with `Last-Modified = max(pool mtime, last_fetched_at)` (spec C4, anchorless refresh), `adoptionStampTx`'s first-adoption floor (`MAX(last_fetched_at) + 1`, `internal/cache/adoption.go`) stays above it only while the pool file's mtime is not later than `last_fetched_at` — true today because `runFetch` finalizes the pool file before it stamps the row (`internal/handler/handler.go`). Keep that order, or fold the anchor blobs' mtimes into the floor.
```

(i) Replace `unless marked *(B)* (the phase B reviews and its final review, 2026-10-18).` with `unless marked *(B)* (the phase B reviews and its final review, 2026-10-18) or *(C)* (the phase C plan, 2026-10-18).`

- [ ] **Step 7: Deferred-minor files**

In `docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md`:

- Replace the A-3 bullet with `- **A-3** GC wallclock stamps (\`gc.go:276,362\`) are not distinguished from \`time.Now\` by any test (the handler's are, since phase C: \`TestHandler_StampsFollowTheWallClock\`).`
- Delete the bullets **A-7**, **A-16**, **A-48**, **A-54** and **A-57**, and the `## Docs` heading left empty by A-57.

In `docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md`:

- Delete the bullets **B-6**, **B-7**, **B-15**, **B-26**, **B-29**, **B-64**, **B-65** and **B-68**, and the `## cmd/apt-cacher-ultra` heading left empty by B-64.

Then run: `grep -nE '\*\*(A-(7|16|48|54|57)|B-(6|7|15|26|29|64|65|68))\*\*' docs/superpowers/plans/2026-09-28-flat-repositories-[AB]-deferred.md`
Expected: no output.

- [ ] **Step 8: Full verification**

Run: `go vet ./... && golangci-lint run ./... && gofmt -l . && go test -race -count=1 ./...`
Expected: vet and lint print nothing (lint: `0 issues.`); `gofmt -l .` prints exactly the five known drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`); every package `ok`. (No phase C test needs apt tooling or Docker.)

Run: `grep -rn 'AIDEV-TODO(phase C)' --include=*.go . ; grep -rn 'flip_quiet_period' docs/configuration.md packaging/config/config.toml.default | head`
Expected: no `AIDEV-TODO(phase C)` left; `flip_quiet_period` appears in both files.

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/freshness/scheduler.go
git add README.md docs/configuration.md docs/log-fields.md docs/superpowers/specs/2026-09-27-flat-repositories-design.md internal/freshness/scheduler.go docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md docs/superpowers/plans/2026-09-28-flat-repositories-A-deferred.md docs/superpowers/plans/2026-09-28-flat-repositories-B-deferred.md
git commit -m "Document the flip barrier and record phase C's rulings in the spec"
```
