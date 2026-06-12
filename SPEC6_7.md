# apt-cacher-ultra — Phase 6.7 Specification

This document specifies the contract for Phase 6.7: **surviving the
mirror-sync race that degrades adoptions, and healing the snapshots it
already degraded.** It is a delta over [SPEC6_5.md](SPEC6_5.md) (REC 1
optional-member tolerance, REC 2 Acquire-By-Hash, §7.2 architecture
filter) and SPEC2 §7.5 (adoption). Everything else carries forward
unchanged.

## Motivation — the 2026-06-09 incident

Adoption fires the moment freshness observes a changed InRelease, which
by construction races the upstream mirror pool's own sync. On
2026-06-09 at 21:42/21:46 UTC-6, two suites (resolute-security,
resolute-updates) adopted against a round-robin `us.archive.ubuntu.com`
backend that was mid-sync:

- the **by-hash** URL for `Contents-amd64.gz` returned 404 (the new
  blob had not synced yet), and
- the **canonical** path served the *previous publication generation*
  (`served 4360619 vs declared 4406247` — caught by the Content-Length
  gate),

so REC 1 tolerance skipped the member. A skipped member has no
`snapshot_member` row, so the adopted suite served authoritative `404
"not in snapshot"` for every form of the file (canonical, uncompressed
fallback, by-hash). Re-adoption fires only on a **changed** InRelease,
and devel suites republish slowly — the 404s persisted ~17 hours and
failed that night's fleet-wide `apt update` runs (apt fetches Contents
for its index targets; the "apt doesn't need these" assumption behind
treating them as skippable is not true for fleets using apt-file-style
index targets).

Phase 6.7 attacks the window at three layers: retry inside the
adoption (§1), record what was skipped anyway (§2), and repair it on
ordinary freshness ticks (§3). Two adjacent hazards confirmed by the
same journals are closed alongside: suite-root members were excluded
from by-hash entirely (§4), and the 404-skip path silently applies to
IndexTargets too (§6).

## 1. In-adoption member retry

`adoptMember` is wrapped in a retry loop (`adoptMemberWithRetry`):

- **Retried:** failures wrapping `ErrAdoptionMemberFetchFailed` or
  `ErrAdoptionMemberMismatch` — the integrity/availability class whose
  dominant real-world cause is the stale-mirror race, which heals
  within the retry window once the lagging backend syncs.
- **Never retried:** 404/410 member skips (`errAdoptionMemberSkipped`
  — permanent publication artifacts; retrying would burn the delay
  budget on guaranteed failures), `ErrAdoptionDBFailed` (local fault),
  and anything after ctx cancellation (shutdown).
- Each fresh attempt re-runs the **full** member sequence: pool-reuse
  check, by-hash probe, canonical fetch. The by-hash probe is the
  expected heal path — its URL embeds the declared sha256 and cannot
  serve the wrong generation.
- The inter-attempt sleep holds **no host-semaphore slot** (the
  semaphore is acquired per attempt inside `adoptMember`), so a member
  in backoff never starves other fetches to the same host. The
  per-adoption heartbeat ticker (SPEC4 §7.5.2 site 6) keeps the
  candidate row alive across the sleeps.
- Applies to IndexTargets too (they benefit most — a deferred adoption
  costs a whole tick), but exhaustion preserves the pre-existing
  fatality rules exactly: tolerant skip for optional members, fatal
  for IndexTargets.

Config: `[adoption].member_retry_count` (default **2**) and
`member_retry_delay` (default **"30s"**); see §5.

## 2. Recording skips — schema v5 `snapshot_skipped_member`

Schema v5 (forward-only, pure additive DDL) adds:

```sql
CREATE TABLE snapshot_skipped_member (
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  path             TEXT NOT NULL,
  declared_sha256  TEXT NOT NULL CHECK (…64 lowercase hex…),
  size             INTEGER NOT NULL,
  reason           TEXT NOT NULL,
  detail           TEXT,
  skipped_at       INTEGER NOT NULL,
  retry_count      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (snapshot_id, path)
);
```

- Adoption records **only integrity-class skips**
  (`reason = 'optional_member_integrity'`) — each row carries the
  member's *signed declaration* (sha256 + size), which is the trust
  anchor any later repair fetch validates against. 4xx skips are
  deliberately NOT recorded: of ~164 skips per Ubuntu suite observed
  in production, all but 2–3 are permanent publication artifacts
  (foreign-arch members served only by ports.ubuntu.com, uncompressed
  decoys, empty dep11 icon tars), and re-attempting them would be
  guaranteed-failure upstream traffic on every tick.
- Rows are inserted by `CommitAdoption` **atomically with the flip**
  (same rationale as the §7.5.4 coverage bit: no "snapshot is current
  but its skip record is not yet visible" mid-state).
- Rows die with their snapshot: the SPEC4 §9.6.3 snapshot-GC cascade
  DELETE now includes `snapshot_skipped_member`.
- `adoption_success` gains a `skipped_integrity_count` field beside
  `skipped_count`, so "this snapshot went live degraded" is one grep.

## 3. Freshness-tick repair pass

`checkLockedInline` / `checkLockedDetached` emit a *repair request*
when the check **confirms upstream unchanged** (304, or 200 with
byte-identical body) and the suite has a current snapshot. `Check`
hands the per-suite mutex to a repair goroutine exactly as it does for
adoption (same `adoptionWg`, so graceful shutdown drains both), which
runs `Adopter.RepairSkippedMembers`:

1. List the snapshot's `reason = 'optional_member_integrity'` rows
   (no-op for every healthy suite — the steady state).
2. With work to do, acquire the same global
   `[adoption].max_concurrent` gate as adoptions — the per-suite
   mutex and per-host limiter bound neither the cross-suite fan-out
   of repair fetches nor their cache-write pressure.
3. Per row, run the full `adoptMember` sequence (by-hash probe first)
   against the recorded declaration.
4. On success, promote the member via `cache.RepairSkippedMember`: one
   writer transaction that (a) re-verifies the snapshot is **still
   current** (repairing a displaced snapshot would leak refcounts —
   `ErrSnapshotNotCurrent` otherwise), (b) loads the skip row and
   **binds** every inserted row to its persisted signed declaration —
   blob hash and declared hash must both equal the recorded
   `declared_sha256`, rows must reference this snapshot, and the
   canonical path must be present — then consumes it, (c) bumps
   `blob.refcount` once iff the snapshot does not already pin the
   blob (preserving the "one bump per distinct blob per snapshot"
   invariant the displacement decrement relies on), and (d) inserts
   the canonical member row plus its by-hash alias, idempotently
   accepting an already-present **byte-identical** row (identical
   same-directory members share one alias path, which the original
   adoption or an earlier repair may have committed) and failing the
   whole transaction on any same-path mismatch.
5. On failure — fetch or promotion — bump `retry_count` and leave the
   row for the next tick. Attempts recur for as long as the snapshot
   stays current (indefinitely, for a suite whose upstream never
   republishes) at 1–2 upstream requests per degraded member per
   fresh tick, with tick cadence already cooldown-limited; there is
   deliberately no cap or backoff, and a persistently climbing
   `retry_count` is the operator signal that a member needs
   investigation. "changed" ticks route to adoption instead, whose
   new snapshot supersedes the old skip records.

Net effect: recovery from a stale-mirror adoption is bounded by
mirror-sync time (minutes) instead of the next InRelease publication
(hours), while the degraded-but-mostly-fresh snapshot keeps serving
throughout.

Config: `[adoption].repair_skipped_members` (default **true**); §5.

## 4. Suite-root by-hash aliases

`byHashAliasPath` previously returned `""` for members at the suite
root (no directory component), on the assumption that apt never
fetches root-level files via by-hash. The incident request logs
disproved that: apt requests `dists/<suite>/by-hash/SHA256/<h>` for
root-level `Contents-*` and Ubuntu serves it. The `""` special case
therefore denied root members both the adoption-time by-hash probe
(the race-immune URL — exactly the file class the incident hit) and
the served alias row.

Phase 6.7: a root-level member aliases to `by-hash/SHA256/<sha256>`
(the suite root's own by-hash directory). Component-level behavior is
unchanged. Empty declared paths still return `""`.

## 5. Config summary (delta over SPEC6_5 §5.1)

| key | default | semantics |
|-----|---------|-----------|
| `adoption.member_retry_count` | `2` | extra attempts per failing member (§1). `0` = single attempt (pre-6.7). Presence-sensitive; must be ≥ 0. |
| `adoption.member_retry_delay` | `"30s"` | wait between attempts. Presence-sensitive; must be ≥ 0. |
| `adoption.repair_skipped_members` | `true` | the §3 tick repair pass. Bool pre-populate (explicit `false` survives Load). |
| `adoption.required_architectures` | `[]` | the §6 served-index guard. Entries follow the SPEC6_5 §5.2 shape rules (incl. cap 32); must be a subset of `adoption.architectures` when that allowlist is non-empty (a required-but-filtered arch would fail every adoption by construction — rejected at Load). |

## 6. IndexTarget 404s: distinct reason + opt-in served-index guard

The production journals confirmed the SPEC2 §7.5.2 4xx-skip applies to
IndexTargets (`main/binary-armhf/Packages.gz` skipped `reason="4xx"`).
That is *correct* for foreign arches the main archive declares but
only ports.ubuntu.com serves — but the same skip on an arch clients
actually use would commit a snapshot that hard-fails `apt update`
until the next InRelease publication. Two measures:

- **Distinct reason value.** A 404/410 skip on an IndexTarget now
  logs/counts `reason = "4xx_index_target"` instead of `"4xx"`.
  Behavior is unchanged; the signal is separated so operators can
  alert on it without drowning in the ~160 ordinary artifact skips
  per Ubuntu suite. (The `acu_adoption_members_skipped_total{reason}`
  enum grows to: `4xx`, `4xx_index_target`,
  `optional_member_integrity`, `arch_not_in_allowlist`.)
- **`required_architectures` guard (opt-in).** Members are grouped by
  *index-target group*: all compression variants of one logical
  `binary-<arch>/Packages` / `source/Sources` collapse to one key
  (codec suffix stripped; `.diff/Index` pdiff manifests are NOT group
  members — a present patch manifest must not count as the index
  being served). For each group whose arch is listed in
  `required_architectures` ("source" = the Sources pseudo-arch): if
  the Release declares ≥ 1 variant and adoption fetched **zero**, the
  adoption FAILS (`ErrAdoptionMemberFetchFailed`, member path = the
  group key, plus an `adoption_required_index_target_missing` WARN).
  The previous coherent snapshot keeps serving and the next freshness
  tick re-attempts (upstream still differs → "changed" → re-adopt).
  One served variant satisfies the group, so uncompressed decoy
  declarations stay harmless. The guard is opt-in because upstreams
  legitimately declare arches they never serve; only the operator
  knows which arches the fleet needs.

## 7. Architecture-filter extension (delta over SPEC6_5 §7.2)

The §7.2 filter previously scoped only `Packages*` / `Sources*`
shapes; SPEC6_5 explicitly left `Contents-*` outside it. Phase 6.7
extends `archFromFilteredPath` to the per-arch *optional* member
shapes:

- `Contents-<arch>` and `Contents-udeb-<arch>` (component-level or
  suite-root, plain or `.gz`/`.xz`/`.bz2`),
- `cnf/Commands-<arch>` (plain or compressed),
- `dep11/Components-<arch>.yml` (plain or compressed).

The pseudo-arch **`all` is exempt by construction**: arch:all content
(Debian's `Contents-all`, `Components-all.yml`) serves clients of
every architecture, so no allowlist may filter it. Arch-independent
optional members (dep11 icons, i18n Translations, per-component-arch
`Release` files) remain unfiltered, as before.

With `architectures = ["amd64"]` on an Ubuntu suite this removes
roughly 90 additional guaranteed-404 fetch attempts per adoption and
shrinks the residual skip list to the genuine artifacts (uncompressed
decoys, empty icon tars) — making `adoption_member_skipped` and its
metric high-signal.

## 8. Backward compatibility

- **Schema v5** is forward-only, pure additive DDL (one new table); a
  v5 database refuses to open under a pre-6.7 binary per the existing
  version gate.
- **`CommitAdoption`** gains a `skipped []SkippedMember` parameter
  (inserted after `members`). Callers passing `nil` get exactly the
  pre-6.7 behavior.
- **Defaults change behavior modestly:** member retries (2 × 30s) add
  up to ~60s to an adoption whose members are failing — bounded per
  member, with heartbeats covering candidate liveness — and the
  repair pass adds one cheap indexed SELECT per fresh tick per
  adopted suite in the steady state (no repairable rows). A degraded
  suite additionally re-fetches its repairable members each fresh
  tick, gated by the same `[adoption].max_concurrent` cap as
  adoptions. Both features have config kill switches.
- **Metric/label deltas:** new counters
  `acu_adoption_member_retries_total{outcome ∈ success|exhausted}`
  and `acu_adoption_member_repairs_total{outcome ∈ success|failure}`;
  `acu_adoption_members_skipped_total` gains the `4xx_index_target`
  reason (some skips previously labeled `4xx` move there).
  `adoption_success` gains `skipped_integrity_count`.
- **Suite-root by-hash aliases (§4)** add snapshot_member alias rows
  for root-level members; apt's root by-hash requests now HIT instead
  of falling through to the canonical name.

## 9. Test plan (implemented)

`internal/cache`: v4→v5 migration; CommitAdoption records/validates
skip rows; ListRepairableSkippedMembers filters by reason;
RepairSkippedMember (promotion + alias, single refcount bump,
shared-blob no-double-bump with displacement-to-zero proof,
not-current refusal, shared-alias idempotence with
displacement-to-zero proof, conflicting-row rejection,
declaration-binding refusals); BumpSkippedMemberRetry; snapshot-GC
cascade.

`internal/freshness`: integrity skip recorded with declaration +
detail; `skipped_integrity_count` in adoption_success; the incident
replay (by-hash 404 → canonical stale → retry heals via by-hash);
retry exhaustion still skips + records; 404 never retried; IndexTarget
retried then fatal; repair promotes member + alias and consumes the
row; repair fetch AND promotion failures bump retry_count; repair
respects the global max_concurrent gate; repair kill switch; end-to-end
Checker tick → repair (httptest upstream, unchanged 200);
indexTargetGroup classification; required-arch guard (all-missing
defers, one-variant suffices, foreign groups ignored, opt-in);
4xx_index_target vs 4xx log reasons; extended arch-filter shapes
(incl. udeb, root-level, "all" exemption) unit + adoption-level.

`internal/config`: retry-knob defaults / explicit zeros / negative
rejection; repair switch default-true / explicit-false;
required_architectures defaults, shape, subset rule.

## 10. Observability inventory (delta over SPEC6_5 §10.3)

New log events: `adoption_member_retry` (Info),
`adoption_member_repaired` (Info), `adoption_member_repair_failed`
(Warn), `adoption_repair_pass` (Info, per-pass aggregate),
`adoption_repair_list_failed` (Warn),
`adoption_required_index_target_missing` (Warn),
`adoption_repair_snapshot_displaced` (Info).

New/changed metrics: `acu_adoption_member_retries_total{outcome}`,
`acu_adoption_member_repairs_total{outcome}`,
`acu_adoption_members_skipped_total{reason}` enum + `4xx_index_target`.

Alerting guidance:
`acu_adoption_members_skipped_total{reason="optional_member_integrity"}`
> 0 means "a degraded snapshot went live" (it should now self-heal —
pair with `acu_adoption_member_repairs_total{outcome="success"}`);
any `4xx_index_target` increase deserves a look (and consider
`required_architectures`); plain `4xx` is baseline noise on Ubuntu
archives unless it spikes.
