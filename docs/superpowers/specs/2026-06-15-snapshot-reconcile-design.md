# Snapshot reconcile — in-place heal of degraded snapshots

Date: 2026-06-15
Status: design approved; ready for implementation plan.
Context: SPEC6_8 follow-up. The [[binary-all-arch-filter-trap]] fix prevents
NEW degraded snapshots (Layer-2 serve-contract guard), but already-published
degraded snapshots (e.g. dev/stg Microsoft suites missing `binary-all`) do not
self-heal — re-adoption fires only on a CHANGED InRelease, and a force-readopt
of the unchanged content collides with `idx_suite_snapshot_natural`
(`ErrSnapshotNaturalKeyAdopted`). This design heals such snapshots IN PLACE.

## 1. Goal & non-goals

Heal a current snapshot that is missing a *requestable* IndexTarget the
snapshot's own signed Release declared, by fetching and inserting the missing
members into the SAME snapshot — no new snapshot, no natural-key collision, no
serving flip/gap. Two triggers: automatic (every unchanged freshness tick) and
on-demand (`POST /reconcile`).

Non-goals: snapshot replacement; healing on a CHANGED InRelease (that is normal
re-adoption); reworking the natural-key/content-addressing model; healing
optional members (Contents/dep11/i18n) — those keep the existing integrity-skip
repair path. Reconcile is scoped to IndexTargets a configured client requests.

## 2. Trust model (unchanged from adoption)

The anchor is the snapshot's already-GPG-verified Release: `snapshot_member`
holds an `InRelease` (inline) or `Release` (detached) row → blob, hash-pinned
via `suite_snapshot.inrelease_hash`/`release_hash` and refcounted so GC never
reaps it. Reconcile re-parses THAT blob (no re-fetch of the signed metadata, no
re-verification needed — it was verified at adoption), takes the declared
SHA256/size as the per-member trust anchor, validates each fetched member's
bytes against it (exactly as adoption does), and inserts only inside a
transaction that re-checks the snapshot is still current. No new trust surface;
a tampered or stale member can never be inserted.

## 3. Components

### 3.1 `Adopter.ReconcileSnapshot(ctx, suite, snapshotID, force) (healed int, err error)`
New file `internal/freshness/reconcile.go`.

1. **Gate.** If `!force` and `snapshotID ∈ a.reconciledSnapshots` (a
   `sync.Map[int64]struct{}` on the Adopter) → return (0, nil). `force=true`
   (the endpoint) bypasses the gate and always re-checks.
2. **Load + parse anchor.** Read the `InRelease`/`Release` member blob; parse
   with the existing adoption Release parser → declared `[]ReleaseMember`
   (path, declared SHA256, size). On missing anchor / parse failure → WARN and
   return err (do not mark complete).
3. **Diff.** Load present `snapshot_member` paths; compute declared-but-absent
   requestable IndexTarget GROUPS via the Layer-2 predicate
   (`missingRequestableIndexGroups` against the present set + `a.architectureAllowlist`).
   Empty → add to `reconciledSnapshots`, return (0, nil).
4. **Heal.** For each declared base+compressed variant of a missing group:
   `adoptMember` (by-hash probe, validate vs the re-parsed declared hash); then
   for the index itself, parse it (Packages/Sources) and build `package_hash`
   rows, exactly as adoption — so arch:all `.deb`s are hash-validated, not just
   served trust-upstream. Insert `snapshot_member` + by-hash alias +
   `package_hash` via a new cache method (§3.2) in one tx. A per-member fetch
   failure is logged and left for the next tick (the snapshot stays uncompleted).
5. **Mark.** Add to `reconciledSnapshots` only when the diff is empty (fully
   healed), so a partial heal retries.

Concurrency: invoked in the same unchanged-tick goroutine as
`RepairSkippedMembers`, under the per-suite mutex, and acquires the global
`[adoption].max_concurrent` gate before doing fetch work (mirroring the repair
pass — healthy/gated snapshots never touch the gate).

### 3.2 `cache.InsertReconciledMembers(ctx, snapshotID, members, packageHashes) error`
Mirrors `RepairSkippedMember`'s transactional shape but does NOT consume a
`snapshot_skipped_member` row (these members were never recorded as skipped).
Re-checks the snapshot is still the suite's current snapshot inside the tx;
returns `ErrSnapshotNotCurrent` if displaced. Inserts are idempotent on the
PK `(snapshot_id, path)` for safe retry (byte-identical re-insert is a no-op;
a conflicting hash is a loud error — same invariant as the by-hash-alias fix).

### 3.3 Checker auto-heal hook
The unchanged-tick handoff in `Checker.check` (today: run `RepairSkippedMembers`
when `repair_skipped_members` and a repairable row exists) also runs
`ReconcileSnapshot(..., force=false)` for the current snapshot, in the same
goroutine, gated by the same `repair_skipped_members` config flag (reconcile is
the same "heal degraded snapshots on unchanged ticks" intent). Both run; order:
repair (recorded integrity skips) then reconcile (declared-vs-present).

### 3.4 `Checker.Reconcile(ctx, scheme, host, suitePath) bool`
On-demand entry. Looks up the suite's current snapshot via `GetSuiteFreshness`;
if the suite is unknown or has no current snapshot → return false WITHOUT
allocating a per-suite lock (bounds the lock map to real suites — the codex
Finding-3 lock-growth concern). Else `TryLock` the suite mutex (false if busy),
hand off a goroutine that runs `ReconcileSnapshot(..., force=true)` and unlocks.
Returns whether a reconcile was triggered.

### 3.5 Admin `POST /reconcile`
Thin trigger over `Checker.Reconcile` via a `Reconciler` interface on
`admin.Config` (nil → 501; keeps admin from importing freshness; cmd wires the
Checker). POST-only branch in the dispatcher (like the other action endpoints
would be), behind the same auth gate as the listener. Body bounded with
`http.MaxBytesReader`. Params (form/query): `host` (required), `suite`
(required suite_path), `scheme` (optional, default `https`). Responses: 202
triggered (async — watch `adoption_snapshot_reconciled` /
`acu_adoption_reconciled_total`), 409 not triggered (busy / no current snapshot
/ unknown suite), 400 missing params, 413 oversized body, 501 not wired. The
admin listener stays protected at the network layer + htpasswd; reconcile is
strictly additive (it can only ADD declared, hash-validated members to a
current snapshot — it cannot degrade serving), so the blast radius of an
unintended call is bounded to extra upstream fetches.

## 4. Observability

- `adoption_snapshot_reconciled` INFO: host, suite, snapshot_id, healed group
  count, remaining.
- `acu_adoption_reconciled_total{architecture}` (bounded arch label, reusing
  the Layer-3 `boundedArchLabel` helper).
- Fetch/insert failures reuse the repair failure log + metric family.
- The Layer-3 `snapshot_index_target_404` WARN / `acu_serve_snapshot_index_target_404_total`
  naturally fall to zero once a suite is healed — the end-to-end success signal.

## 5. Config

The AUTOMATIC (per-tick) reconcile is gated by `[adoption].repair_skipped_members`
— reconcile is the same "heal degraded snapshots on unchanged ticks" intent, so
it shares the flag rather than adding a new one. The `POST /reconcile` endpoint
is INDEPENDENT of the flag: it is an explicit operator action and is available
whenever a Reconciler is wired (so an operator can heal a suite on a deployment
where the auto pass is disabled). No schema change.

## 6. Edge cases

- Anchor blob missing/unparseable → WARN, no mark, retry next tick.
- Snapshot displaced mid-reconcile (new adoption) → insert returns
  `ErrSnapshotNotCurrent` → abandon; the new snapshot is complete by
  construction (Layer-2 guard) or reconciled on its own first tick.
- A genuinely-unserved requestable IndexTarget (upstream 404 — e.g. an arch
  added to the allowlist that the repo never serves) → fetch fails every tick,
  never marked complete, `snapshot_index_target_404` keeps alerting. Correct:
  that snapshot genuinely cannot satisfy the configured clients; the alert is
  the signal to fix the allowlist or the upstream.
- Restart drops `reconciledSnapshots` → each current snapshot is re-checked
  once after restart (cheap; also a robustness win — a snapshot that went
  degraded by any path gets re-examined).

## 7. Testing (TDD)

- Unit: declared-vs-present diff returns the missing requestable groups
  (reusing the Layer-2 `missingRequestableIndexGroups` predicate against the
  snapshot's present-member set).
- Integration (the test the reverted feature could NOT pass): adopt into a
  REAL cache a snapshot missing `binary-all` (same suite, same InRelease hash),
  run `ReconcileSnapshot`, assert `binary-all/Packages` member + by-hash alias
  + arch:all `package_hash` rows now present AND `CurrentSnapshotID` is the SAME
  id (in-place, no flip).
- Gate: a complete snapshot is a no-op and is marked; `force=true` re-checks a
  marked snapshot.
- Displacement: snapshot replaced mid-reconcile → `ErrSnapshotNotCurrent`,
  abandons cleanly.
- Endpoint: 202 triggered / 409 no-current-snapshot / 400 / 413 / 501; unknown
  suite allocates no lock.
- Auto-heal: an unchanged tick on a degraded suite triggers reconcile and heals.
