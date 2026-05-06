# apt-cacher-ultra — Phase 2 Scoping

Status: **scoping draft — not locked**. Last updated 2026-05-05 (Q1/Q2/Q3/Q4/Q5/Q10 resolved; Q6/Q7/Q8/Q9 open).

This document gathers what Phase 2 is, what hooks Phase 1 left for it, and the
open design questions that must be settled before this becomes a locked SPEC2.md
(parallel to SPEC.md). Treat every "Open" callout as a decision point requiring
the user's input before implementation.

---

## 1. Goals

Phase 1 made the cache-hit path bulletproof against upstream failure. Phase 2
closes the *integrity* and *freshness* loops that Phase 1 deliberately left
open:

1. **Atomic metadata flip.** When upstream publishes a new `InRelease`, the
   cache must adopt it together with every index it references in a single
   transactional swap, so no client ever sees a hash-mismatch window between
   "old InRelease, new Packages" or vice versa.
2. **GPG signature verification of `InRelease`.** Before adopting any new
   metadata snapshot, the cache verifies the inline GPG signature against a
   trusted keyring. A MITM-or-compromised upstream cannot silently poison the
   cache.
3. **Hash validation against `InRelease`/`Packages`.** Once adoption is
   atomic and verified, the cache enforces declared `SHA256` digests on every
   metadata file (against `InRelease`) and every `.deb` (against `Packages`)
   it serves. SPEC §11 row 16: *"P1: stored as-is. Phase 2 will reject."*
4. **`by-hash/SHA256/<hex>` dedup.** Indices fetched via the by-hash variant
   already share content; route them through the existing content-addressed
   pool so disk and bandwidth dedup naturally across suites.

The four are not independent: atomic flip is the keystone, GPG gates adoption,
by-hash gives the flip a clean enumeration of what to fetch, and hash
validation is the runtime enforcement that proves the flip held.

### 1.1 Non-goals (deferred to later phases)

- Hot-package proactive refresh (Phase 3).
- Garbage collection of orphaned blobs from displaced snapshots (Phase 4 —
  the snapshot model in §3 produces orphans by design and waits for GC).
- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Streaming-while-fetching as a singleflight optimization (SPEC §6.2 Phase 2
  candidate). **Resolved (Q1): deferred** — revisit only if production
  measurement on the cache-miss singleflight path shows the coalesce-and-
  serialize policy hurts a real workload.
- Per-byte read timeouts on upstream (currently `idle_read_timeout` is
  informational). **Resolved (Q1): deferred** alongside the streaming
  question; both are Phase 3+ polish unless a specific incident motivates
  one sooner.

---

## 2. What Phase 1 already left in place

Walking the existing code, Phase 1 deliberately seeded these hooks:

| Phase 1 hook | Phase 2 use |
|---|---|
| `<cache_dir>/staging/` directory created on `Open` | Holds candidate snapshot blobs during prefetch, before atomic adoption |
| `suite_freshness.inrelease_change_seen_at` column | Already records the "upstream has newer" observation; Phase 2 turns this into the trigger for an adoption transaction |
| `handler.tryServeStale` | Becomes the centerpiece of "serve from frozen consistent set during adoption" — comment in `handler.go:625` already says so |
| `blob.refcount` column | Phase 2 starts incrementing it (per snapshot membership); Phase 4 GC reads it |
| `proxy.IsMetadata` already classifies `by-hash/*` as metadata | Phase 2 routes by-hash fetches through the same freshness state machine |
| Schema version constant + migration framework | First non-trivial migration: v1 → v2 |

Phase 2 is therefore mostly *additive* — new tables, new state, new validation.
Phase 1's wire contracts (§2), URL canonicalization (§3), per-host concurrency
(§9.3), and graceful shutdown (§9.5) all carry forward unchanged.

---

## 3. Architectural sketch

### 3.1 Snapshot model

The natural shape: one `(canonical_scheme, canonical_host, suite_path)` suite
has *N* snapshots, exactly one of which is `current`. A snapshot is the tuple
`(InRelease blob_hash, set of (path → blob_hash) for every referenced index)`.

```sql
-- Phase 2 schema delta (sketch).

CREATE TABLE suite_snapshot (
  snapshot_id      INTEGER PRIMARY KEY AUTOINCREMENT,
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  suite_path       TEXT NOT NULL,
  inrelease_hash   TEXT NOT NULL REFERENCES blob(hash),
  inrelease_etag   TEXT,
  inrelease_lastmod TEXT,
  created_at       INTEGER NOT NULL,
  adopted_at       INTEGER,                -- NULL = candidate not yet flipped
  -- One row per (suite, inrelease_hash) — same hash never makes two snapshots.
  UNIQUE (canonical_scheme, canonical_host, suite_path, inrelease_hash)
);

CREATE TABLE snapshot_member (
  snapshot_id  INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  path         TEXT NOT NULL,             -- e.g. /ubuntu/dists/noble/main/binary-amd64/Packages
  blob_hash    TEXT NOT NULL REFERENCES blob(hash),
  declared_sha256 TEXT NOT NULL,           -- as listed in InRelease; equal to blob_hash for sha256
  PRIMARY KEY (snapshot_id, path)
);

-- Per-snapshot .deb declared-hash index. Populated during adoption by
-- parsing every Packages member; the .deb fetch path looks up its
-- declared SHA256 here instead of re-parsing 30 MB of Packages on each
-- miss. (See Q10.)
CREATE TABLE package_hash (
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  path             TEXT NOT NULL,         -- the .deb path: /ubuntu/pool/main/...
  declared_sha256  TEXT NOT NULL,
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id)
);
CREATE INDEX idx_package_hash_snapshot ON package_hash(snapshot_id);

ALTER TABLE suite_freshness
  ADD COLUMN current_snapshot_id INTEGER REFERENCES suite_snapshot(snapshot_id);
```

`.deb` declared-hash lookup at fetch time:

```sql
SELECT declared_sha256
FROM package_hash p
JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
WHERE p.canonical_scheme = ? AND p.canonical_host = ? AND p.path = ?
LIMIT 1;
```

Empty result = no current snapshot vouches for this `.deb`. Phase 2 falls
back to Phase 1 trust-upstream behavior in that case (covers pre-Phase-2
deployments and `.deb` paths under suites that haven't adopted yet — see
Q9). Phase 3 may tighten this to "refuse" once every actively-used suite
on a host has at least one snapshot.

**Atomic flip = single SQLite transaction:**

1. INSERT into `suite_snapshot` (candidate; `adopted_at` NULL).
2. INSERT all `snapshot_member` rows.
3. Bump `blob.refcount` for every member's hash.
4. UPDATE `suite_freshness.current_snapshot_id` to the new snapshot.
5. UPDATE `suite_snapshot.adopted_at` = now.
6. Decrement refcounts of the *previous* snapshot's members. (Don't delete
   blobs here — Phase 4 GC reaps when refcount hits 0.)

The flip is one tx; either every client sees the new snapshot or every client
keeps seeing the old. No window.

**Resolved (Q2): snapshot-scoped lookup.** Metadata cache hits consult
`suite_freshness.current_snapshot_id` → `snapshot_member.path = ?` →
`blob_hash`. Two consequences:

- The Phase 1 `url_path` table stays authoritative for `.deb` (and any
  non-suite path), and continues to track `last_requested_at` /
  `request_count` for *all* paths including metadata. It is no longer the
  source of truth for which blob a metadata path resolves to.
- Adoption + read sit on the same key (`current_snapshot_id`). A request
  arriving mid-flip sees either the prior `current_snapshot_id` or the new
  one — never a partial mix — because the flip is one SQLite transaction.

**.deb hash validation derived from this model.** Snapshot members include
the `Packages` index, which lists every `.deb` filename with a declared
`SHA256`. At `.deb` cache-miss, the fetch path consults the current
snapshots covering the canonical host to recover the declared hash for the
target path; mismatch on the downloaded blob → 502 + discard. The lookup is
"join `snapshot_member` rows whose blob is a `Packages` file, parse the
`Packages` text, find this `.deb`'s line." Materializing that into a
queryable form (a `package_hash` table populated during adoption) is a
performance question — see Open Q10.

### 3.2 Adoption flow

```
On freshness check returning 200 with new InRelease bytes (today: log "awaiting
Phase 2 atomic flip" and return):

  1. GPG-verify the new InRelease. If verification fails: log loudly, do NOT
     create a candidate snapshot, leave inrelease_change_seen_at set so the
     diagnostic surfaces. (This is the only path that turns a successful HTTP
     response into a *rejected* update — apt-cacher-ng would have happily
     served the forgery.)
  2. Parse InRelease, extract the SHA256 → relative-path index.
  3. For each member path: if blob_hash already in pool/, skip; else fetch
     into staging/<snapshot_id>/<path>, hash-validate against the declared
     sha256, then promote into pool/.
  4. Once every member is in pool/: run the atomic-flip transaction (§3.1).
  5. Log adoption with snapshot_id, prior snapshot_id, and member count.

Failure handling at any step: candidate snapshot is abandoned, staging files
swept (extend the SPEC §4.2 sweeper to staging/), suite continues serving
the prior snapshot. inrelease_change_seen_at remains set so the next
periodic_refresh tries again.
```

**Resolved (Q3): shared `hostsem` + global adoption cap.**

Hybrid of options (a) and (c). Concretely:

- Adoption keeps using the same per-host semaphore the request-path miss
  handler and Phase 1's freshness checker already share. Per-host fan-out
  is bounded by the existing `upstream.max_concurrent_per_host` (default 8).
- Add a *cache-wide* `freshness.max_concurrent_adoptions` semaphore that
  caps how many adoptions can run simultaneously across the whole cache.
  Acquired once at adoption start, released after the atomic flip (or
  abort). Default 2; `0` disables the cap.
- Members within an adoption are fetched **sequentially**. One adoption
  in flight = at most one `hostsem` slot consumed by adoption traffic at
  any moment per host. With `max_concurrent_adoptions = 2`, request-path
  loses at most 2 of N host slots even during a restart storm.

**Why this combination over the alternatives:**

- **Option (b) — reserved fraction for request-path** — would mean nested
  semaphore acquisition (small adoption-budget sem inside the larger
  hostsem). That introduces ordering rules and a deadlock surface for a
  benefit that the global cap already provides without those hazards.
  Robustness loses to complexity; not worth it.
- **Option (a) alone** — the Phase 1 status quo — is fine in the steady
  state but degrades badly under the realistic worst case: a cache
  restart causes every suite to re-check, every check that finds new
  metadata starts an adoption, and 10+ simultaneous adoptions to the
  same host can saturate `hostsem` for minutes while request-path
  callers wait or 502. The global cap eliminates that scenario.
- **Option (c) alone** — global cap without sequential members — could
  still let one adoption parallelize member fetches and consume every
  hostsem slot for a host. Sequencing members inside an adoption is the
  cheap fix that keeps reasoning trivial: in-flight adoption load on any
  single host is bounded by the cap, full stop.

**Robustness properties this gives us:**

- Cache-hit path is untouched (Phase 1's bulletproofing carries forward).
- Cache-miss path on host H sees adoption consume at most
  `max_concurrent_adoptions` of H's slots, so a host with `N` slots and
  cap `2` always has `N-2` slots available for request-path traffic.
- Adoption itself is bounded: the cap prevents pathological queue depth;
  a 100-suite restart storm processes adoptions `cap` at a time but each
  adoption still completes in normal time.
- One semaphore, one acquisition site (top of the adoption goroutine),
  no nested ordering — the testable surface is small.

A future Phase 2 polish (under measurement) might parallelize member
fetches within an adoption for faster end-to-end adoption time. That's a
separate change with its own concurrency bound to design; sequential is
the conservative starting point.

### 3.3 GPG verification

**Resolved (Q4): hybrid keyring.**

- **Default trust set:** the host's apt keyring — `/etc/apt/trusted.gpg.d/*.gpg`
  plus `/etc/apt/keyrings/*` — loaded at startup. Mirrors what apt on the same
  host would accept, so an operator already curating apt's trust on that
  machine doesn't have to maintain a parallel set for the cache.
- **Optional per-suite pinning:** a new `[[trusted_signer]]` config block
  binds a `match_canonical_host` regex to a list of allowed GPG fingerprints
  (long form, no whitespace). When such a block matches a suite's host,
  *only* those fingerprints will satisfy verification, narrowing the trust
  set below the host keyring's default. Operators who want strict pinning
  configure it; everyone else gets sane apt-equivalent behavior for free.

```toml
# Strict pinning example.
[[trusted_signer]]
match_canonical_host = '^archive\.ubuntu\.com$'
fingerprints         = ['F6ECB3762474EDA9D21B7022871920D1991BC93C']
```

Implementation note: the cache's trust set per suite is the intersection of
"keys present in the host keyring" with "fingerprints whitelisted by a
matching `[[trusted_signer]]` block, if any." Empty pinning list = "no key
acceptable" = always reject; an explicit operator footgun the validator
should warn on at startup.

**Resolved (Q5): GPG library = `github.com/ProtonMail/go-crypto/openpgp`.**
The maintained successor to `x/crypto/openpgp` (which is deprecated).
Distributed as a tagged module, no cgo, used by the broader Go ecosystem
including ProtonMail and Hashicorp Vault — proven against real-world
keyring corpora.

**Failure semantics:** signature invalid → reject the InRelease (no candidate
snapshot created); signature missing → reject (treat unsigned upstream as
hostile in Phase 2); upstream returned a `Release.gpg` detached signature
instead of `InRelease` → support both forms (apt does), verify accordingly.
*Open Q6.*

### 3.4 by-hash dedup

The Debian by-hash protocol: every index file is also published at
`<dists>/<suite>/<component>/<arch>/by-hash/SHA256/<hex>` where `<hex>` is
the file's SHA256. Phase 1 already classifies these paths as metadata.

Phase 2's dedup move:

- When a by-hash URL is fetched, the `<hex>` in the path is the declared
  hash. After fetch, verify `sha256(body) == hex`; on success, the blob
  is *already* in `pool/<hex[0:2]>/<hex>` by virtue of content addressing.
- Add a `url_path` row mapping the by-hash URL → that blob_hash; concurrent
  by-hash fetches from different suites hit the same blob.
- Snapshot members reference the same blob across suites for free.

**Open Q7 — index discovery.** Apt fetches by-hash *only* when the
upstream's `Release` advertises `Acquire-By-Hash: yes`. Should the cache
proactively prefetch by-hash variants during adoption (cheap: same blob if
already cached), or fetch only on demand? Recommend on-demand for Phase 2;
proactive prefetch is a Phase 3 hot-package candidate.

### 3.5 Hash validation enforcement

Two enforcement boundaries:

1. **At fetch (write path).** When the cache fetches a member of an active
   snapshot's index (`Packages`, `Sources`, `Translation-*`, `.deb`), it
   knows the declared SHA256 from the indexed metadata. After download,
   validate. Mismatch → discard the partial blob, log as a fetch failure,
   do not insert into the pool, return 502 to the client.
2. **At serve (read path).** Periodic background validation: walk each
   active snapshot's members, hash the on-disk blob, compare against the
   `snapshot_member.declared_sha256`. Catches corruption-at-rest. Frequency
   tunable; default daily.

**Open Q8 — read-path validation cadence.** Daily is cheap (sha256 on disk is
fast) but operators on small VMs might want it off. Configurable knob:
`integrity.validate_at_rest_interval` with `0 = off`?

**Open Q9 — what about pre-Phase-2 blobs?** Existing pool/ entries from a
Phase 1 deployment have no declared hash recorded. After upgrade, are they
"trusted" until a freshness check produces a new snapshot, or is the cache
re-fetched cold? Recommend trusted-until-replaced; the v1→v2 migration
does not touch existing rows beyond schema additions.

---

## 4. Schema migration v1 → v2

```
ALTER TABLE blob (no column change; refcount semantics activated)
ADD TABLE suite_snapshot
ADD TABLE snapshot_member
ADD TABLE package_hash               -- materialized .deb declared-hash index (Q10)
ALTER TABLE suite_freshness ADD COLUMN current_snapshot_id INTEGER
INSERT INTO schema_version VALUES (2)
```

(Q2 resolved as snapshot-scoped lookup, so `url_path` does not gain a
`snapshot_id` column — the snapshot pointer lives on `suite_freshness` and
metadata reads route through it.)

Migration is forward-only (Phase 1 already enforces this). Existing
`url_path` rows survive untouched; new snapshot rows accrete on the first
successful freshness check post-upgrade. Pre-Phase-2 metadata blobs
continue to serve via the Phase 1 `url_path` lookup until a snapshot is
adopted (see Q9).

### 4.1 New configuration keys

```toml
[freshness]
# Phase 2: cap simultaneous adoptions cache-wide. 0 = unlimited (matches
# the Phase 1 status quo). Default 2 keeps adoption load bounded under a
# restart storm; per-host fan-out is still bounded by
# upstream.max_concurrent_per_host. (See Q3.)
max_concurrent_adoptions = 2

[adoption]
# Phase 2: master switch. When false, the cache continues Phase 1 behavior
# (record inrelease_change_seen_at, do not adopt). Behind a flag for the
# shadow-deploy phase of rollout per §6 sequencing.
enabled            = false
require_signature  = true        # GPG verify before adopting; false at operator's risk

[[trusted_signer]]
# Optional per-suite GPG fingerprint pinning (Q4 hybrid). Empty list of
# fingerprints is rejected at startup as an obvious operator footgun.
match_canonical_host = '^archive\.ubuntu\.com$'
fingerprints         = ['F6ECB3762474EDA9D21B7022871920D1991BC93C']
```

---

## 5. Chaos test for Phase 2 (the new gate)

Phase 1's gate was *"hung upstream during cache-hit must not block."* Phase 2's
natural analogue is *"adversarial-or-divergent upstream must not produce a
client-visible hash mismatch."*

```
GIVEN
  a cache with an adopted snapshot A for suite "noble"
  upstream now serving snapshot B (new InRelease + new Packages)
  100 concurrent clients fetching {InRelease, Packages, 5 .deb} mid-update
WHEN
  cache observes new InRelease, begins prefetch of B's members
  during prefetch, all 100 clients complete their fetch sequence
THEN
  every client sees a self-consistent set: either all of A or all of B
  no client sees A's InRelease with B's Packages or vice versa
  any .deb whose hash mismatched its Packages declaration is 502'd, not served
```

A second chaos test for the GPG path: *"upstream serves a forged InRelease
(valid bytes but bad signature) — the cache rejects, never adopts, keeps
serving the prior snapshot."*

---

## 6. Recommended sequencing

1. **Schema + snapshot model** (no behavior change; adds tables, lets
   Phase 1 keep running). Validates migration story end-to-end.
2. **Adoption flow without GPG** (parse InRelease, fetch members, hash-validate,
   atomic-flip). Behind a config flag `adoption.enabled = false` by default
   so a buggy adoption can't break Phase 1 deployments. Run in shadow mode in
   one prod instance for a week.
3. **GPG verification** layered on top. Behind `adoption.require_signature`,
   default true. Operator can disable for an upstream we trust without keys.
4. **by-hash url_path dedup** (mostly bookkeeping at this point — blobs
   already dedupe via content addressing).
5. **Read-path integrity scan** as the last addition; least risky, most
   discoverable failure mode.

Each step is independently shippable and independently chaos-testable.

---

## 7. Questions

### 7.1 Resolved

| ID | Question | Resolution |
|---|---|---|
| Q1 | Streaming-while-fetching + per-byte read timeouts | **Deferred.** Revisit if production measurement on the cache-miss path argues for it. (§1.1) |
| Q2 | Request-path lookup model | **Snapshot-scoped** via `suite_freshness.current_snapshot_id` → `snapshot_member`. `url_path` retains stats but is no longer source-of-truth for which blob a metadata path resolves to. (§3.1) |
| Q3 | Adoption concurrency policy | **Shared `hostsem` + global adoption cap.** Sequential member fetches inside an adoption + cache-wide `freshness.max_concurrent_adoptions` (default 2). One semaphore acquisition site, no nested ordering, robust to restart storms. (§3.2) |
| Q4 | GPG keyring source | **Hybrid**: host apt keyring as default trust set, optional per-suite pinning via `[[trusted_signer]]` blocks. (§3.3) |
| Q5 | OpenPGP library | **`github.com/ProtonMail/go-crypto/openpgp`** (the maintained replacement for the deprecated `x/crypto/openpgp`). (§3.3) |
| Q10 | `.deb` declared-hash storage | **Materialize during adoption** into `package_hash` table; lookup joins through `suite_freshness.current_snapshot_id`. Empty result falls back to Phase 1 trust-upstream behavior. (§3.1) |

### 7.2 Open

| ID | Question |
|---|---|
| Q6 | Detached `Release.gpg` support — required for Phase 2 or only `InRelease` inline? |
| Q7 | Proactive by-hash prefetch during adoption, or on-demand? |
| Q8 | Default cadence for read-path integrity scan, and is it disable-able? |
| Q9 | Pre-Phase-2 pool/ blobs: trusted-until-replaced, or cold re-fetch on upgrade? |

---

## 8. What this document is *not*

It is not the locked Phase 2 SPEC. It is the agenda for the conversation that
produces that SPEC. Every architectural sketch above is a recommendation, not
a commitment. The next step after the open questions are answered is to roll
this and the answers into a SPEC2.md modeled on SPEC.md's structure (numbered
sections, wire contracts, schema, definition of done).
