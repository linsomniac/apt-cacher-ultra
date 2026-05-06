# apt-cacher-ultra — Future Review Items

Status: live document. Last updated 2026-05-06.

This file captures decisions explicitly deferred for **lack of observational
data**. Each item is *not* a question waiting on a maintainer's preference —
it's a decision that requires production evidence (typically from multiple
deployments and over weeks-to-months) before it can be answered well.

Revisit each item only when its listed criteria are met. Calendar reviews
without data attached are noise.

---

## 1. Streaming-while-fetching + per-byte upstream read timeouts

**Origin.** SPEC §6.2 named streaming-while-fetching as a candidate Phase 2
optimization. PHASE-2-SCOPING.md Q1 deferred both streaming and per-byte
read timeouts to "Phase 3+ polish, revisit if production measurement argues
for it." Phase 3 scoping (PHASE-3-SCOPING.md §1.2) deferred them further to
this file.

**What "needs observational data" means here.** The judgment call between
"the coalesce-and-serialize singleflight policy is fine" and "streaming
would meaningfully shorten p99 cache-miss for big debs" requires:

- Cache-miss latency p99 broken down by file-size bucket. Small debs are
  not worth optimizing — the signal lives above ~32 MB (NVIDIA drivers,
  Chrome, ML wheels, language packs).
- Singleflight join-time histograms — how late do waiters arrive on a
  big-deb fetch? If joins typically arrive near the leader's start, the
  coalesce wait isn't the limiting factor and streaming would buy little.
- Frequency of `bad_gateway` outcomes whose `duration_ms` clusters near
  `total_timeout` with no preceding retry attempts. That's the slow-trickle
  signal for per-byte timeouts: an upstream went into byte-trickle mode
  and `idle_read_timeout` (informational, not enforced) didn't catch it.

**Why one deployment isn't enough.** A single cache's traffic mix is too
narrow to characterize the signal. "Big debs" is workload-dependent and
varies wildly across user populations. To answer with confidence we need
data from multiple users running diverse workloads over weeks-to-months —
a single short bake will under-sample the distribution.

**Revisit when.** Either trigger condition is sufficient:

1. Multiple production deployments (≥3) have logged ≥30 days of traffic,
   AND the aggregated p99 miss latency by size bucket shows a clearly
   actionable curve (i.e. some size threshold above which the
   coalesce-wait visibly hurts client wallclock).
2. OR a single deployment has a documented incident traceable to a
   slow-trickle upstream that `idle_read_timeout` (informational) failed
   to catch and `total_timeout` reached.

When either fires, open the design as its own scoping doc.

---

## 2. Per-suite freshness cadence

**Origin.** PHASE-3-SCOPING.md Q4 — whether `freshness.periodic_refresh`
should vary per-suite (e.g. tighter for hot suites, slacker for dormant
ones) instead of remaining a global value.

**Resolution at scoping time.** No per-suite cadence in Phase 3.
Operators who need fast adoption set `periodic_refresh` aggressively
globally; the cost (one conditional GET per suite per cycle) is negligible
at any reasonable suite count.

**What "needs observational data" means here.** The case for adding
per-suite scheduling is only worth its complexity (per-suite timers /
earliest-next-check heap, plus the test surface that comes with it) if
real deployments are demonstrably hurt by uniform-global cadence:

- Total suite count per cache, with the dormant fraction quantified.
  Per-suite tightening is most valuable when the cache holds 200+ active
  suites with a heavy dormant tail.
- Upstream-load measurements (HTTP request rate, observed throttling /
  rate-limit responses) under aggressive global `periodic_refresh`.
- Operator latency requirements for the hot subset. If "I publish a new
  package and want it cached" tolerates ≥5 minutes globally, no need.
  If sub-minute is required for a small subset only, per-suite is
  meaningful.

**Why one deployment isn't enough.** Suite-count fan-out and dormant-tail
shape vary across user populations; a single small deployment with a
short suite list won't motivate the feature even if larger deployments
genuinely need it. Months-to-years of multi-user data is the right
input.

**Revisit when.** All three conditions hold simultaneously:

1. Multiple deployments report holding 200+ active suites with a heavy
   long-tail of dormant ones.
2. Real upstream-load measurements show running `periodic_refresh`
   globally aggressive on those deployments visibly costs the upstream
   (rate-limit responses, throttling, or observable load on the upstream
   operator's side).
3. The operator latency tolerance for the hot subset is genuinely
   sub-minute (otherwise a less-aggressive global setting still works).

Without all three, the complexity of per-suite scheduling doesn't pay
for itself.

---

## How to use this file

When you have data that *might* satisfy a revisit criterion:

1. Drop the data summary (or a link to it) into the relevant section.
2. Decide whether it's enough to act, or just enough to keep watching.
3. If acting: this item becomes a Phase N goal and graduates out of
   this file into a phase-scoping doc.

This file is not a wishlist. Items here are gated on evidence, not
preference.
