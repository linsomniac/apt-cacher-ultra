# apt-cacher-ultra — Phase 2 Scoping

Status: **scoping draft — not locked**. Last updated 2026-05-05 (Q2/Q4/Q5 resolved).

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
- Streaming-while-fetching as a singleflight optimization. SPEC §6.2 lists
  this as a Phase 2 candidate "if measurement shows it matters." Recommend
  deferring unless production traffic argues for it — *Open Q1*.
- Per-byte read timeouts on upstream (currently `idle_read_timeout` is
  informational). Probably also Phase 2 polish if it lands at all — *Open Q1*.

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

ALTER TABLE suite_freshness
  ADD COLUMN current_snapshot_id INTEGER REFERENCES suite_snapshot(snapshot_id);
```

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

**Open Q3 — adoption concurrency.** This work happens off the request path
(extending the existing freshness goroutine pattern) but member fetches consume
the per-host semaphore. A burst of adoptions (operator restarts cache, every
suite re-checks) could starve the request-path miss handler. Options:
(a) keep the same `hostsem` and accept brief contention, (b) reserve a fraction
of the budget for request-path traffic, (c) cap concurrent adoptions globally.

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
| Q2 | Request-path lookup model | **Snapshot-scoped** via `suite_freshness.current_snapshot_id` → `snapshot_member`. `url_path` retains stats but is no longer source-of-truth for which blob a metadata path resolves to. (§3.1) |
| Q4 | GPG keyring source | **Hybrid**: host apt keyring as default trust set, optional per-suite pinning via `[[trusted_signer]]` blocks. (§3.3) |
| Q5 | OpenPGP library | **`github.com/ProtonMail/go-crypto/openpgp`** (the maintained replacement for the deprecated `x/crypto/openpgp`). (§3.3) |

### 7.2 Open

| ID | Question |
|---|---|
| Q1 | Is streaming-while-fetching and per-byte read timeouts in scope for Phase 2, or punted? |
| Q3 | Adoption concurrency: shared `hostsem`, fraction-reserved, or global cap? |
| Q6 | Detached `Release.gpg` support — required for Phase 2 or only `InRelease` inline? |
| Q7 | Proactive by-hash prefetch during adoption, or on-demand? |
| Q8 | Default cadence for read-path integrity scan, and is it disable-able? |
| Q9 | Pre-Phase-2 pool/ blobs: trusted-until-replaced, or cold re-fetch on upgrade? |
| Q10 | `.deb` declared-hash storage — derive at fetch time from the `Packages` blob, or materialize a `package_hash` table during adoption? Performance/complexity tradeoff (§3.1). |

---

## 8. What this document is *not*

It is not the locked Phase 2 SPEC. It is the agenda for the conversation that
produces that SPEC. Every architectural sketch above is a recommendation, not
a commitment. The next step after the open questions are answered is to roll
this and the answers into a SPEC2.md modeled on SPEC.md's structure (numbered
sections, wire contracts, schema, definition of done).
