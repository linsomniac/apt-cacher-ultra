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

## 6. Recovery of the live degraded snapshots — DEFERRED (follow-up)

A degraded snapshot published under the old binary does NOT self-heal until
upstream republishes InRelease (freshness 304s never re-attempt skipped
members; the next freshness tick re-adopts only on an InRelease *change*).

A `POST /readopt` "force re-adopt" endpoint was prototyped and **pulled**: it
cannot work as a refetch-and-readopt. Snapshots are content-addressed —
`idx_suite_snapshot_natural` is UNIQUE on
`(scheme, host, suite, COALESCE(inrelease_hash, release_hash))`, and
`InsertCandidateSnapshot` returns `ErrSnapshotNaturalKeyAdopted` when an
adopted snapshot already occupies that key. Re-fetching the SAME (unchanged)
InRelease therefore collides with the degraded snapshot and the adoption
fails — exactly the recovery case. Forcing the freshness "changed" decision
is necessary but NOT sufficient; recovery must also REPLACE the adopted
snapshot for that natural key (delete-then-rebuild, with a brief
Phase-1-fallback serving window and failure-atomicity to handle), or heal it
IN PLACE by extending the SPEC6_7 §3 repair pass to fetch declared-but-absent
requestable IndexTargets into the existing snapshot. Either is a real design
and is tracked as the SPEC6_8 follow-up; not shipped here.

Interim recovery for dev/stg (pick one):

1. **Wait for upstream.** Microsoft republishes the `prod` InRelease
   periodically; the fixed binary adopts binary-all correctly on the next
   change. Lowest effort; unbounded latency.
2. **Force a content change to retrigger adoption.** Remove the degraded
   snapshot's natural-key occupancy so the next tick re-adopts: unset
   `suite_freshness.current_snapshot_id`, delete the `suite_snapshot` row for
   the affected Microsoft suites, AND clear the InRelease `url_path` baseline
   (otherwise the unchanged-hash check still short-circuits). DB surgery — do
   it with care, or wait for the follow-up tool.

The Layer-2 guard means that whenever re-adoption DOES happen (naturally or
via the follow-up), it can no longer publish a binary-all-less snapshot.

## 7. Alert on

- `acu_serve_snapshot_index_target_404_total` > 0 (page — apt is broken now).
- `acu_adoption_serve_target_missing_total` > 0 (a snapshot is being held back).
- `adoption_member_skipped reason="4xx_index_target"` sustained (degraded risk).

## 8. Deployment

No schema change. Deploy the binary; recover the already-degraded Microsoft
snapshots per §6 (the fix prevents NEW degraded snapshots but does not heal
existing ones). `architectures=["amd64"]` stays correct — the bug was the
missing `all` exemption, not the allowlist.
