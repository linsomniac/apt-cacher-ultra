# Flat repositories plan review

Reviewed on **2026-10-05**, against `feature/flat-repos` at `525178a940b905c7be55d7281493de0565a7e9dc`:

- [Implementation overview](2026-09-28-flat-repositories-00-overview.md).
- [Phase A: Foundations](2026-09-28-flat-repositories-A-foundations.md).
- [Revision 3 design specification](../specs/2026-09-27-flat-repositories-design.md), as the governing context for later phases.

**No critical-severity finding was confirmed. Seven high-priority recommendations follow.** Findings 1–3 concern existing state surviving an upgrade; 4–7 identify incomplete remediation or contracts that need strengthening before later phases are implemented. They do not assert that future Phase C–G code already exists.

The overview makes the specification authoritative at line 11. Corrections to contracts shared with that specification must update both documents.

Resolve legacy trust cleanup and classification in Phase A, preserve signed history across every supported phase upgrade, then make strict inline decoding, atomic downgrade prevention, cross-suite flip coordination and scheduler fairness explicit prerequisites for the corresponding features. Keep the planned atomic migration rollback, exhaustive URL encoding tests and panic defer-order checks.

## 1. High: Repair already-materialized R-1 pollution

**Pre-existing vulnerability incompletely remediated.** Task 8 prevents future reconciliation from parsing unsigned armor content, but does not remove data already derived from it. [Phase A](2026-09-28-flat-repositories-A-foundations.md), lines 2381–2424, only changes the parser. Existing [reconciliation](../../../internal/freshness/reconcile.go), lines 48–55, returns successfully when all legitimate requested members are present; it does not reject extra members. [Cache reconciliation](../../../internal/cache/reconcile.go), lines 94–104 and 107–130, adds or reconciles rows rather than rebuilding the trusted set.

A temporary reproduction verified the legitimate signature cryptographically, then used the old reconciler to materialize one forged index and one `package_hash` row from declarations appended inside unsigned signature armor. The proposed plaintext parser subsequently returned only the legitimate declaration and zero missing members. Both forged rows remained stored.

**Impact:** upgrading can leave attacker-derived metadata servable and attacker-derived package hashes vouching for downloads. This preserves a proxy integrity bypass; it does not establish a bypass of apt's own signature verification.

**Plan change:** add a safe, resumable audit/rebuild or quarantine of legacy derived members, aliases and package hashes before trusting them. Reconcile reference counts and include displaced snapshots that could later become eligible for retained serving. Do not rely on another ordinary reconcile or an eventual upstream change.

**Acceptance test:** start with a polluted pre-fix v6 cache, upgrade, and assert that the malicious index cannot be served and its package hash cannot vouch, while legitimate metadata remains usable. Exercise current and retained snapshots and interruption/restart of cleanup.

## 2. High: Preserve the distinction between legacy plaintext and signed snapshots

**Confirmed migration classification defect.** [Phase A](2026-09-28-flat-repositories-A-foundations.md), lines 1697–1698, labels every legacy single-anchor snapshot `inline`; lines 1757–1759 mark every adopted snapshot signed-seen. Yet [VerifyInline](../../../internal/gpg/verifier.go), lines 244–252, accepts plain text when `require_signature=false`. Such an adoption can already contain package hashes. Phase A explicitly preserves this case at lines 2404–2413 and the verifier fallback at line 2544.

**Impact:** Phase E's filtering of explicit `unsigned` forms will miss these falsely classified snapshots. They can retain package vouching and strict-mode participation. The signed-current dispatch retains the old snapshot when signed anchors disappear, while the false signed-seen marker can block corrected unsigned adoption. See the [specification](../specs/2026-09-27-flat-repositories-design.md), lines 341–351 and 510–534.

**Plan change:** audit actual stored anchor envelopes and available provenance before assigning trust-bearing forms. Quarantine or safely convert legacy plaintext snapshots and rebuild their derived trust data before unsigned semantics become active. Merely changing the form to `unsigned` is insufficient: their historical anchor path was `InRelease`, whereas the new unsigned form uses `Release`. Preserve the separately documented `accept_any_signer` policy; intentional delegation to client signature verification is distinct from accepting plaintext as signed metadata.

**Acceptance test:** perform a real permissive plaintext adoption with pre-v7 behavior, upgrade through Phase E without an upstream change, and verify that its old hashes no longer vouch, it does not participate as a signed snapshot, and refresh reaches the correct supported form without a false downgrade refusal.

## 3. High: Backfill signed history for unadopted suites and staged upgrades

**Confirmed backfill omission, plus a staged-upgrade gap.** [Phase A](2026-09-28-flat-repositories-A-foundations.md), lines 1756–1759, derives markers only from adopted snapshots. Its fixture includes a client-requested, unadopted `InRelease` at lines 1209–1216, while the marker assertion at lines 1329–1331 does not cover previously served signed anchors. A temporary SQLite execution of the planned migration preserved a recently requested unadopted suite's interest timestamp but produced no marker.

This is a normal legacy state: unchanged anchors do not trigger adoption in [freshness.go](../../../internal/freshness/freshness.go), lines 690–719. After upgrade, eager adoption can observe missing signatures twice and publish unsigned content before another client request restores the marker, although clients were previously served signed metadata.

There is also an A–C upgrade window. [Overview](2026-09-28-flat-repositories-00-overview.md), line 83, defers `MarkSignedSeen` to D. Signed snapshots created after the v7 backfill therefore need a catch-up operation; v7 does not run again when D–F arrive. Reaping can otherwise erase their remaining signed history.

**Plan change:** backfill sniff-passing cached `InRelease` and `Release.gpg` anchors for tracked suites, using the marker's normalized base and accounting for finding 2. Either start maintaining signed history in A or require an explicit, idempotent activation catch-up before unsigned adoption or reaping. Follow the [marker contract](../specs/2026-09-27-flat-repositories-design.md), lines 311–318, rather than equating “signed seen” with “adopted.”

**Acceptance test:** cover a never-adopted signed v6 suite before any post-upgrade client serve, plus an A-created signed snapshot followed by D/E/F activation, reap and return. Neither history may silently permit unsigned adoption.

## 4. High: Reject malformed signed envelopes even when signatures are optional

**Pre-existing fallback conflicts with the future unsigned contract.** This path requires the non-default `require_signature=false` setting. [Phase A](2026-09-28-flat-repositories-A-foundations.md), lines 2524–2544, catches decoder panics but explicitly leaves the `block == nil` fallback unchanged. [Verifier code](../../../internal/gpg/verifier.go), lines 244–252, then returns raw bytes; lines 458–462 also allow a missing signature end marker to reach that decoder.

The proposed [first-line sniff and verification ordering](../specs/2026-09-27-flat-repositories-design.md), lines 259 and 434–436, do not close this path. A body can begin with the required signed-message marker, fail normal structural decoding without panicking, and still reach `ParseRelease` as raw text. Phase A lines 1896–1899 assign the inline form to that path.

**Impact:** malformed apparent `InRelease` input can avoid Phase E's explicit unsigned confirmation, marker checks and no-vouch policy. This is source-confirmed control flow and a later-phase contract gap, not a claimed new regression introduced by A or an executed end-to-end reproduction.

**Plan change:** make structural inline decoding failure an error regardless of the legacy flag. Route unsigned metadata exclusively through the explicit `Release` eligibility path. Keep deliberate `accept_any_signer` behavior, including its structural signature requirements, distinct from accepting an undecodable envelope.

**Acceptance test:** with `require_signature=false`, submit marker-present bodies with malformed or absent signature frames. They must create no inline snapshot, package hashes or signed strict-mode participant. Distinguish malformed signature errors from genuine signature-file absence.

## 5. High: Recheck signed-seen atomically at unsigned commit

**Later-phase transaction contract gap.** The [specification](../specs/2026-09-27-flat-repositories-design.md), lines 526–530, checks signed-seen eligibility but describes the commit safeguard only as rejecting a signed prior current snapshot. [Overview](2026-09-28-flat-repositories-00-overview.md), lines 83–84, separates marker writes in D from unsigned adoption in E without specifying their transaction ordering.

A never-adopted suite can pass both unsigned observations and start downloading members. Meanwhile, a handler serves a newly available signed anchor and commits its marker. The adoption still holds the freshness suite mutex, so the subsequent T1 check skips; see [freshness.go](../../../internal/freshness/freshness.go), lines 291–306 and 351–367. At unsigned commit, the prior current snapshot is still absent. The specified prior-snapshot guard therefore permits publication despite the new marker.

**Impact:** signed-seen protection can fail during an ordinary concurrent serve, publishing unsigned metadata for a suite already observed signed. Unsigned content still does not vouch packages, and verifying clients retain their signature checks.

**Plan change:** read the normalized-base marker inside the same writer transaction that commits an unsigned candidate, alongside the prior-snapshot guard. Define marker-write ordering relative to serving signed bytes. The suite mutex alone is insufficient because handler serving and the `/.` twin can establish signed history independently.

**Acceptance test:** pause an eligible unsigned adoption, serve a signed anchor and complete `MarkSignedSeen`, then resume commit. Assert refusal and no snapshot-pointer change. Repeat through the normalized `/.` twin; tests that merely preload a signed current snapshot miss this race.

## 6. High: Coordinate cross-suite holds during Phase B

**Later-phase flip contract gap.** [Overview](2026-09-28-flat-repositories-00-overview.md), line 45, requires shared-queue clients not to straddle generations. The [specification](../specs/2026-09-27-flat-repositories-design.md), lines 658–671, checks global hold activity only in Phase A; Phase B checks local anchor activity.

With the default 10-second quiet period, this permitted execution remains inconsistent:

- At −0.1 seconds a client receives A's old anchor. Busy A and B exhaust their 60-second Phase A budgets and enter holds at 0 and 0.1 seconds.
- At 0.2 seconds the client requests B's anchor; that hold delays its queued A index fetch. Another existing B anchor response completes at 6 seconds.
- A commits at 10 seconds after satisfying local quiet. B commits at 16.1 seconds after satisfying its local quiet. The released client requests A's index at 16.3 seconds and receives A's new generation.

Both holds finish under 20 seconds; no waiter limit, long commit or reader exceeding 30 seconds is needed. Same-host overlap is possible because the host adoption slot is released before the barrier, as specified at lines 395–396.

**Impact:** the proxy's own hold can cause the plain-path metadata mismatch Phase C intends to eliminate. The narrow guarantee at lines 679–682 covers requests arriving within the quiet interval, but this hold pushes them outside it. That does not satisfy the broader cross-suite requirement or zero-straddle test at lines 939–944.

**Plan change and acceptance test:** define coordination across Phase B holds and recently released queues while preserving bounded waits and counting deliberate degradation. Add this overlapping-hold trace to a realistic shared-client-queue simulation. Retained by-hash behavior alone does not repair plain-path requests.

## 7. High: Bound waiting time across suites, not just each check

**Pre-existing scheduler weakness insufficiently addressed by later capacity rules.** [Scheduler code](../../../internal/freshness/scheduler.go), lines 98–112, checks suites synchronously. [ListSuites](../../../internal/cache/queries.go), lines 705–720, supplies no fair ordering. The [specification](../specs/2026-09-27-flat-repositories-design.md), lines 322–325, retains the sequential scheduler and adds a two-minute check deadline.

**Impact:** that deadline does not bound another suite's delay. At the planned 500-suite per-host default cap, a permitted scan placing 500 two-minute failing anchor checks ahead of a healthy suite delays its periodic check by **16 hours 40 minutes**. This is arithmetic for an allowed execution, not a measured benchmark or a claim about guaranteed SQL ordering. Client-triggered T1 checks may bypass the sequential T2 loop; the example does not imply every client update freezes. Reserved adoption capacity is irrelevant until the healthy suite reaches admission. Repeated admission in scan order can also favor earlier repairs or changing candidates without a fair retry discipline.

**Plan change:** before eager/unsigned adoption, require bounded concurrent checks with fairness across hosts and rotating or oldest-pending adoption admission. State a maximum healthy-suite latency under defined saturation, including repair/reconcile contention. Keep concurrency and queue memory bounded; per-operation deadlines and `TryAcquire` alone do not establish liveness.

**Acceptance test:** use slow or timing-out anchor probes ahead of healthy suites on another host, not merely slow member downloads after adoption starts. Add persistent repair/changing-candidate contention and assert the declared latency bound, continuing checks and bounded goroutines. The existing [capacity test](../specs/2026-09-27-flat-repositories-design.md), lines 918–921, should exercise both stages.

## Validation scope

Validation included source and plan inspection, a temporary Go overlay with cryptographic verification demonstrating persistent R-1-derived rows, temporary SQLite migration probes, and explicit concurrency/state traces. The R-1 reproduction was independently rerun. The complete Phase A implementation and full repository test suite were not executed. Future-phase findings identify contract gaps requiring implementation tests; they are not reports against completed future code.
