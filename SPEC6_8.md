# SPEC6_8 — `binary-all` IndexTarget must never be arch-filtered; serve-contract guard

Status: implemented (branch `fix/binary-all-indextarget-filter`).
Incident: 2026-06-14, dev + stg. Third of the "snapshot published missing a
required index → authoritative 404 for the snapshot's whole lifetime" class
(see [[freshness-freeze-urlpath-gc-trap]], [[skipped-member-404-trap]]).

## 1. Incident

Nightly ansible `apt update` on `map1.dev` / `map1.stg` failed:

```
E:Failed to fetch https://packages.microsoft.com/ubuntu/24.04/prod/dists/noble/main/binary-all/Packages
  404  Not Found [IP: 10.13.13.99 3142]
```

The cacher served an **authoritative** "not in snapshot" 404 (request log:
`outcome:"hit" status:404 bytes_sent:0 duration_ms:0 architecture:"all"`) for
`dists/noble/main/binary-all/Packages` on the Microsoft repo (jammy 22.04 +
noble 24.04). Microsoft is a Debian-style third-party repo that publishes a
SEPARATE `binary-all` IndexTarget for arch-independent (`Architecture: all`)
packages; apt fetches it on EVERY host alongside `binary-<host-arch>`.

## 2. Root cause

`internal/freshness/adoption.go archFromFilteredPath` classified
`binary-all/Packages` as arch `"all"` and reported it `filtered=true`. With
`[adoption].architectures=["amd64"]` active, the SPEC6_5 §7.2 filter then
skipped it (`adoption_member_skipped reason="arch_not_in_allowlist"
architecture="all"`), and adoption "succeeded" with a snapshot that omits a
required index. The `"all"` exemption existed in the function's doc contract
and in the optional-member branch (Contents-all / Components-all, added
SPEC6_7 §7, commit `70a2225`) but was **never implemented in the binary
branch** (original SPEC6_5 filter, commit `bc5edc52`).

The bug was latent from SPEC6_5; enabling `architectures=["amd64"]` in dev/stg
(the SPEC6_7 follow-up that closed the previous incident) armed it. Ubuntu's
own archive is immune — it folds arch:all into each `binary-<arch>` and
publishes no `binary-all`, so testing on Ubuntu suites never surfaced it.

## 3. Fix (Layer 1)

`archFromFilteredPath`: exempt `"all"` in the binary branch, mirroring the
optional-member branch — the function now honors its own documented contract.
`binary-all/Packages*` is never filtered. Tests: `TestArchFromFilteredPath`
(`binary-all/Packages{,.gz,.xz,.diff/Index}`, d-i variant → exempt) +
`TestAdopter_Allowlist_KeepsBinaryAllIndexTarget` (Microsoft-shaped Release,
proven RED against pre-fix code).

## 4. Prevention — pre-publish serve-contract guard (Layer 2)

`missingRequestableIndexGroups(declared, fetched, allowlist)` + a guard before
the snapshot commit: refuse to publish (defer the adoption, keep the prior
coherent snapshot serving) when the Release declared an IndexTarget group for
a **requestable** arch — any allowlisted arch, plus the always-required `all`
— that the snapshot did not fetch. Returns `ErrAdoptionMemberFetchFailed` so
outcome classification / freshness retry / "keep prior snapshot" all behave
like the existing SPEC6_7 §6 `required_architectures` guard, which this
generalizes. Inert when the allowlist is unset (REC 1 already makes every
IndexTarget fatal-on-failure).

This also closes a SECOND path to the same trap that the Layer-1 fix does not:
a transient upstream 404 on `binary-all/Packages` is a `4xx_index_target`
skip, which would otherwise publish a degraded snapshot. Test:
`TestAdopter_Allowlist_BlocksPublishWhenBinaryAllUnavailable`.

## 5. Observability (Layer 3)

- **Served-404-on-IndexTarget alert (symptom, cause-agnostic).** The handler
  emits `WARN snapshot_index_target_404 {architecture, path, snapshot_id}` and
  increments `acu_serve_snapshot_index_target_404_total{architecture}` when it
  serves an authoritative "not in snapshot" 404 for an apt IndexTarget
  (`binary-<arch>/Packages*` / `source/Sources*`; by-hash / pdiff / optional
  members excluded — apt has fallbacks). This is the single highest-signal
  indicator that a client's `apt update` is broken, whatever the cause —
  it fires on this incident, the Contents incident, and the freeze trap alike.
- **Cause-side alert.** `acu_adoption_serve_target_missing_total{arch}` +
  `ERROR adoption_blocked_serve_target_missing` from the Layer-2 guard.
- **adoption_success** now records the effective `architectures` allowlist, so
  an operator can confirm a skip was intended without reverse-engineering it
  from per-member WARN lines.

## 6. Recovery of degraded snapshots — implemented (SPEC6_8 in-place reconcile)

Recovery is fully implemented. Two paths heal a snapshot that was published
missing a required IndexTarget:

**(a) Automatic in-place heal on unchanged freshness ticks** (`repair_skipped_members`
flag). When the freshness checker fetches an upstream InRelease that is
byte-identical to the current snapshot's (unchanged tick), it triggers
`Adopter.ReconcileSnapshot` on the current snapshot. The reconciler re-parses
the snapshot's GPG-verified Release (the trust anchor — the original blob,
unchanged in the pool), diffs declared-vs-present requestable IndexTarget
groups, fetches each missing member from upstream, validates it against the
re-parsed declared hash, and inserts it into the **same** snapshot via a
transaction that re-checks the snapshot is still current. A once-complete
snapshot is memoized so the per-tick cost is a single DB read.

Why "in place" rather than re-adopt: snapshots are content-addressed —
`idx_suite_snapshot_natural` is UNIQUE on
`(scheme, host, suite, COALESCE(inrelease_hash, release_hash))`. Re-fetching
the same unchanged InRelease collides with the already-adopted snapshot and
fails with `ErrSnapshotNaturalKeyAdopted`. Healing in place sidesteps the
collision, requires no serving flip, and cannot degrade serving (inserts are
additive; the transaction re-validates the snapshot is current before commit).

**(b) On-demand via `POST /reconcile`** (SPEC6_8 recovery tool). Forces an
immediate in-place reconcile of one suite's current snapshot without waiting
for the next tick. Auth + method enforcement is handled by the admin
dispatcher. Params (form-encoded):

| param  | required | notes |
|--------|----------|-------|
| `host` | yes | canonical upstream host, e.g. `packages.microsoft.com` |
| `suite` | yes | suite path, e.g. `/ubuntu/24.04/prod/dists/noble` |
| `scheme` | no | default `https` |

Status codes: `202 Accepted` (reconcile triggered async) / `409 Conflict`
(busy, unknown suite, or no current snapshot) / `400` (missing params) /
`413` (oversized body) / `501` (not wired).

```bash
curl -s -X POST http://admin-host:9143/reconcile \
  -d 'host=packages.microsoft.com' \
  -d 'suite=/ubuntu/24.04/prod/dists/noble'
```

**Observability.** `acu_adoption_reconciled_total{architecture}` counts healed
members. It pairs inversely with `acu_serve_snapshot_index_target_404_total` —
once reconcile runs successfully, the 404 counter stops climbing.

## 7. Alert on

- `acu_serve_snapshot_index_target_404_total` > 0 (page — apt is broken now).
- `acu_adoption_serve_target_missing_total` > 0 (a snapshot is being held back).
- `adoption_member_skipped reason="4xx_index_target"` sustained (degraded risk).

## 8. Deployment

No schema change. Deploy the binary; recover the already-degraded Microsoft
snapshots per §6 (the fix prevents NEW degraded snapshots but does not heal
existing ones). `architectures=["amd64"]` stays correct — the bug was the
missing `all` exemption, not the allowlist.
