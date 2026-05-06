# apt-cacher-ultra — Phase 2 Specification

Status: **locked for Phase 2 implementation**. Last updated 2026-05-05.

This document specifies the contract for Phase 2: atomic metadata flip, GPG
verification, hash validation, and `by-hash` dedup. It is a delta over
[SPEC.md](SPEC.md) (Phase 1). Sections that carry forward unchanged say so
explicitly and point at SPEC.md; sections that change describe only the
delta. The companion document
[PHASE-2-SCOPING.md](PHASE-2-SCOPING.md) records the design rationale and
the Q1–Q10 decisions that produced this spec.

Phase 2 is purely additive over Phase 1: a Phase 1 cache directory can be
upgraded in place by starting a Phase 2 binary against it; existing blobs
keep serving via the Phase 1 path until a snapshot adopts an index
referencing them (see §4.3.2 migration and §6.1).

---

## 1. Goals & non-goals

### 1.1 Phase 2 goals

1. **Atomic metadata flip.** When upstream publishes a new `InRelease`,
   the cache adopts it together with every index it references in a
   single transactional swap. No client ever sees a hash-mismatch window
   between "old `InRelease`, new `Packages`" or vice versa.
2. **GPG signature verification of `InRelease` / `Release`.** Before any
   adoption, the inline `InRelease` (or detached `Release` + `Release.gpg`
   pair) is verified against a configured trust set rooted in the host's
   apt keyring. A MITM-or-compromised upstream cannot poison the cache.
3. **Hash validation against `InRelease` and `Packages`.** Every metadata
   blob fetched is hash-validated against its declared `SHA256` in
   `InRelease`; every `.deb` fetched is validated against its declared
   `SHA256` in `Packages` whenever a current snapshot vouches for it.
   Mismatch → discard, log, 502 + `Retry-After`.
4. **`by-hash/SHA256/<hex>` dedup.** Indices fetched via the by-hash
   variant naturally share content via the existing content-addressed
   `pool/`. Phase 2 adds the `url_path` bookkeeping so concurrent
   by-hash requests across suites converge on the same blob.

### 1.2 Phase 2 non-goals (deferred)

- Hot-package proactive refresh (Phase 3).
- Garbage collection of orphan blobs from displaced snapshots (Phase 4 —
  the snapshot model in §4 produces orphans by design and waits for GC).
- Status page / `/metrics` endpoint (Phase 5).
- TLS MITM listener (Phase 6).
- Source-package caching, multi-arch beyond amd64, pdiff (Phase 6+).
- Streaming-while-fetching as a singleflight optimization. Re-evaluate
  if production traffic argues for it; otherwise Phase 3+ polish. (Q1.)
- Per-byte read timeouts on upstream (currently `idle_read_timeout` is
  informational). Same disposition as streaming. (Q1.)
- Operator-triggered manual adoption (admin endpoint or SIGHUP).
  Achievable in Phase 2 if needed but not part of the gating contract.

---

## 2. Wire contracts

### 2.1 Listener
Unchanged — see SPEC.md §2.1.

### 2.2 Proxy mode
Unchanged — see SPEC.md §2.2.

### 2.3 The `http://HTTPS///` convention
Unchanged.

### 2.4 Mirror mode
Unchanged.

### 2.5 Range requests
Unchanged.

### 2.6 HTTP methods
Unchanged.

### 2.7 Response headers (deltas)

Existing headers carry forward exactly: `X-Cache` (with values `HIT`,
`MISS`, `HIT-STALE`, `HIT-COALESCED`), `X-Cache-Age`, `X-Upstream-Status`.

Phase 2 adds **one diagnostic header**:

- `X-Cache-Snapshot`: when the response served a metadata file resolved
  through the snapshot model (§6.1), the decimal `snapshot_id` of the
  current snapshot. Absent for `.deb` responses, for metadata served via
  the Phase 1 fallback path (suite without a current snapshot), and for
  every error response. Operators reading captured traces can correlate a
  served byte to the exact snapshot that vouched for it.

The semantics of `X-Cache: HIT` remain "served from cache without an
upstream call." Phase 2 narrows what "from cache" means for metadata: the
hit must come from the suite's `current_snapshot_id`, not from any blob
that happens to be in the pool. The narrowing is invisible to clients —
the byte stream and headers match the consistent snapshot.

---

## 3. URL canonicalization (Remap)
Unchanged — see SPEC.md §3.

---

## 4. Storage layout

### 4.1 Disk

Phase 2 activates the `staging/` subdirectory that SPEC §4.1 reserved:

```
<cache_dir>/
  cache.db                                  # SQLite, schema_version=2
  pool/                                     # (Phase 1) finalized blobs
  tmp/                                      # (Phase 1) singleflight downloads
  staging/<snapshot_id>/<hex>               # Phase 2: in-flight adoption members
```

Adoption fetches each candidate snapshot member into
`staging/<snapshot_id>/<hex>`, hash-verifies it, then renames it into
`pool/<hex[0:2]>/<hex>` exactly like a Phase 1 cache miss does for
`tmp/`. The per-snapshot subdirectory exists so an aborted adoption can
be reaped wholesale (the partial member set is useless without the rest;
no individual file is recoverable). Filenames inside the subdirectory
are the SHA256 hex of the declared content — collisions across members
of the same snapshot would mean InRelease declared the same hash for two
distinct paths, which is well-formed (and we keep one entry).

### 4.2 Startup cleanup (deltas)

The `tmp/` 5-minute mtime sweep from SPEC §4.2 carries forward. Phase 2
extends it to `staging/`:

- For each subdirectory under `staging/`, take the youngest mtime among
  its files. If older than 5 minutes, remove the entire subdirectory.
  Empty subdirectories (orphan from a directory create that never wrote
  a file) are removed unconditionally.
- The 5-minute cutoff is the same threshold `tmp/` uses; an in-flight
  adoption refreshes member mtimes naturally as new bytes arrive.

The sweep does not consult SQLite — adoption's own atomic-flip
transaction is the single source of truth for what's adopted. A
pre-flip-but-staged-on-disk `staging/<id>/` is by definition garbage
once the process that created it is gone.

### 4.3 SQLite schema

Phase 2 schema is `schema_version = 2`. Migration v1 → v2 is described in
§4.3.2.

#### 4.3.1 Phase 2 schema delta

New tables:

```sql
CREATE TABLE suite_snapshot (
  snapshot_id        INTEGER PRIMARY KEY AUTOINCREMENT,
  canonical_scheme   TEXT NOT NULL,
  canonical_host     TEXT NOT NULL,
  suite_path         TEXT NOT NULL,
  inrelease_hash     TEXT REFERENCES blob(hash),   -- non-null when adopted from inline
                                                   -- InRelease (clearsigned). Null when
                                                   -- adopted from detached Release pair.
  inrelease_etag     TEXT,
  inrelease_lastmod  TEXT,
  release_hash       TEXT REFERENCES blob(hash),   -- non-null when adopted from a detached
                                                   -- Release+Release.gpg pair (Q6).
  release_gpg_hash   TEXT REFERENCES blob(hash),   -- non-null when adopted from detached
                                                   -- pair; pairs with release_hash.
  created_at         INTEGER NOT NULL,
  adopted_at         INTEGER,                      -- NULL while candidate; set on flip.
  -- Exactly one of (inrelease_hash) or (release_hash AND release_gpg_hash)
  -- must be populated. Enforced at the DB layer with a CHECK using IS
  -- NULL / IS NOT NULL exclusively — those predicates are not subject to
  -- the three-valued-logic pitfalls that bite equality comparisons
  -- across NULLs. Without this CHECK, an all-NULL row would slip through
  -- AND would bypass the COALESCE-based UNIQUE index (since
  -- COALESCE(NULL, NULL) = NULL and SQLite treats NULLs as distinct for
  -- UNIQUE purposes). The adoption code is the first line of defense;
  -- the CHECK is the backstop.
  CHECK (
    (inrelease_hash IS NOT NULL AND release_hash IS NULL AND release_gpg_hash IS NULL)
    OR
    (inrelease_hash IS NULL AND release_hash IS NOT NULL AND release_gpg_hash IS NOT NULL)
  )
);

-- Natural-key UNIQUE: same (suite, verified text) cannot be adopted twice.
-- Expressed as a UNIQUE INDEX rather than a table-level UNIQUE so the
-- COALESCE expression is permitted (SQLite allows expressions in indexes
-- but not in inline UNIQUE constraints). The COALESCE picks whichever of
-- inrelease_hash or release_hash is populated for this snapshot — exactly
-- one is, by construction.
CREATE UNIQUE INDEX idx_suite_snapshot_natural
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path,
                    COALESCE(inrelease_hash, release_hash));

CREATE INDEX idx_suite_snapshot_suite
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path);

CREATE TABLE snapshot_member (
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  path             TEXT NOT NULL,
  blob_hash        TEXT NOT NULL REFERENCES blob(hash),
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  PRIMARY KEY (snapshot_id, path)
);

CREATE INDEX idx_snapshot_member_blob
  ON snapshot_member(blob_hash);

-- Per-snapshot .deb declared-hash index (Q10). Materialized at adoption time
-- by parsing every Packages member; the .deb fetch path looks up its declared
-- SHA256 here in O(1) on a covering index hit, instead of re-parsing 30 MB
-- Packages files on every miss.
CREATE TABLE package_hash (
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  path             TEXT NOT NULL,            -- the .deb path (matches url_path.path)
  declared_sha256  TEXT NOT NULL
                     CHECK (length(declared_sha256) = 64
                            AND declared_sha256 NOT GLOB '*[^0-9a-f]*'),
  snapshot_id      INTEGER NOT NULL REFERENCES suite_snapshot(snapshot_id),
  PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id)
);

CREATE INDEX idx_package_hash_snapshot
  ON package_hash(snapshot_id);
```

Existing-table change:

```sql
ALTER TABLE suite_freshness
  ADD COLUMN current_snapshot_id INTEGER REFERENCES suite_snapshot(snapshot_id);
```

`url_path` and `blob` are unchanged structurally. Phase 2 *does* start
exercising `blob.refcount`: adoption increments by one for every
`snapshot_member` and `package_hash` row that points at a blob;
displacing a prior snapshot decrements. Phase 4 GC will sweep
`refcount = 0` blobs.

#### 4.3.2 Migration v1 → v2

```
migrations[1] = v1 → v2:
  CREATE TABLE suite_snapshot (...);
  CREATE INDEX idx_suite_snapshot_suite ...;
  CREATE TABLE snapshot_member (...);
  CREATE INDEX idx_snapshot_member_blob ...;
  CREATE TABLE package_hash (...);
  CREATE INDEX idx_package_hash_snapshot ...;
  ALTER TABLE suite_freshness ADD COLUMN current_snapshot_id INTEGER ...;
  -- migrate.go bumps schema_version after success.
```

Properties:

- **Forward-only.** Phase 1's `migrate` already enforces this; v2 keeps
  the contract.
- **Pure DDL, no row rewrites.** Migration runtime is bounded by SQLite
  schema-change cost (milliseconds for a typical Phase 1 deployment),
  not by row count.
- **Atomic.** The migration framework runs each migration inside a
  single transaction; an interrupted migration rolls back fully, and the
  next start retries from `schema_version = 1`.
- **Pre-Phase-2 blobs are trusted-until-replaced (Q9).** Existing
  `url_path → blob_hash` mappings keep serving via the Phase 1 path
  until a freshness check observes a new `InRelease`, adopts a snapshot,
  and the affected suite's `current_snapshot_id` becomes non-null. At
  that point new requests resolve through `snapshot_member`; the prior
  blob remains in `pool/` (its `url_path` row no longer authoritative
  for metadata reads) until Phase 4 GC.

### 4.4 Suite identification
Unchanged — see SPEC.md §4.4.

### 4.5 Classifying metadata vs. blob
Unchanged — see SPEC.md §4.5. Phase 2 uses the same classifier to decide
which fetched paths must validate against `snapshot_member.declared_sha256`
versus `package_hash.declared_sha256`.

---

## 5. Configuration (TOML)

### 5.1 Example (deltas)

Existing sections (`[cache]`, `[upstream]`, `[freshness]`, `[serve]`,
`[log]`, `[[remap]]`, `[[mirror]]`) carry forward unchanged from SPEC §5.1.
Phase 2 adds three things and modifies one:

```toml
[freshness]
# Existing keys (cooldown, periodic_refresh) carry forward unchanged.
# Phase 2 adds:
max_concurrent_adoptions = 2              # cache-wide cap on simultaneous
                                          # adoptions; 0 = unlimited (Q3).

[adoption]
enabled               = false              # master switch. False = Phase 1
                                           # behavior (record change, do not
                                           # adopt). Default false during
                                           # rollout; flip to true after the
                                           # shadow-deploy cycle.
require_signature     = true               # reject InRelease that fails GPG
                                           # verification. Operators who
                                           # explicitly trust an unsigned
                                           # upstream can set false; warning
                                           # logged at startup so the choice
                                           # is auditable.
require_pinned_signer = false              # fail-closed when a suite has no
                                           # matching [[trusted_signer]]
                                           # block. Default false to match
                                           # apt's broad-trust default; flip
                                           # to true once every active suite
                                           # has an explicit pin (recommended
                                           # for production — see §7.6.5).

[integrity]
validate_at_rest_interval = "24h"          # cadence for the at-rest sha256
                                           # scan. 0 = disabled (Q8).
validate_at_rest_workers  = 4              # bounded worker pool so the scan
                                           # cannot starve request handling.

# Optional per-suite GPG fingerprint pinning (Q4 hybrid). When a block
# matches a suite's canonical_host, the trust set narrows to keys whose
# fingerprint is in the list. Without any matching block, the suite uses
# the host apt keyring's full key set.
[[trusted_signer]]
match_canonical_host = '^archive\.ubuntu\.com$'
fingerprints         = ['F6ECB3762474EDA9D21B7022871920D1991BC93C']
```

### 5.2 Config validation (deltas)

Phase 1 validation (SPEC §5.2) carries forward. Phase 2 adds:

- `freshness.max_concurrent_adoptions` is integer, ≥ 0.
- `adoption.enabled`, `adoption.require_signature`, and
  `adoption.require_pinned_signer` are bool.
- `integrity.validate_at_rest_interval` parses as duration, ≥ 0.
- `integrity.validate_at_rest_workers` is integer, ≥ 1 (when interval > 0).
- Each `[[trusted_signer]]` entry:
  - `match_canonical_host` compiles as a Go regex.
  - `fingerprints` is a non-empty list. An empty list is a footgun (no
    key would ever satisfy verification for matching suites) and is
    rejected at startup with a clear error.
  - Each fingerprint is exactly 40 lowercase or uppercase hex chars (no
    whitespace). Short-form 16-byte fingerprints are rejected — they are
    cryptographically insufficient and apt itself rejects them.

`adoption.require_signature = false` is a permitted but loud
configuration: a `WARN`-level startup log line names every suite that
would adopt unverified metadata, so the operator's choice is visible in
deployment journals.

---

## 6. Request handling

### 6.1 The fast path: cache hit (deltas)

For metadata paths (per SPEC §4.5 classifier), the lookup order is:

```
1. SuitePath := proxy.SuitePath(req.path)              -- "" for non-suite paths
2. If SuitePath != "":
     row := SELECT current_snapshot_id FROM suite_freshness
              WHERE canonical_scheme=? AND canonical_host=? AND suite_path=?
     If row.current_snapshot_id IS NOT NULL:
       -- Snapshot-scoped lookup (the suite has been adopted).
       blob := SELECT blob_hash FROM snapshot_member
                 WHERE snapshot_id=? AND path=?
       If found: serve from pool/<blob>; X-Cache: HIT; X-Cache-Snapshot: <id>.
       If not found: 404. The snapshot is the contract; if the path is
         not in it, no Phase 1 url_path is allowed to satisfy a
         metadata request under an adopted suite. Falling back to
         url_path here would let stale, unverified Phase 1 metadata
         masquerade as part of the verified snapshot.
3. (Non-suite path, or a suite whose current_snapshot_id IS NULL — i.e.
   pre-Phase-2 row, or a suite whose first adoption has not yet
   succeeded.) Look up url_path as in SPEC §6.1; if found, serve.
```

The `current_snapshot_id IS NOT NULL` predicate is the gate that
separates "Phase 1 trust-upstream" suites from "Phase 2 verified" ones.
Once a suite has been adopted at least once, every subsequent metadata
hit on it is served from the snapshot or refused; the `url_path` table
is not consulted for that suite again until/unless the snapshot pointer
is cleared (which Phase 2 never does — only Phase 4 GC after the
suite's last snapshot is reaped).

For blob paths (`.deb` etc.), lookup extends Phase 1 with a
defense-in-depth check against the snapshot's declared hash:

```
1. row := SELECT blob_hash FROM url_path WHERE ...
   If not found: cache miss (§6.2).
2. declared := query as in §6.2 (DISTINCT declared_sha256 from
                package_hash for any current snapshot covering this
                (host, path)).
3. Zero declared rows: serve from pool/<row.blob_hash>.
                       (Phase 1 trust-upstream — no snapshot covers it.)
4. One declared row that matches row.blob_hash: serve from pool.
5. One declared row that does NOT match row.blob_hash: stale Phase 1
   data covered by a Phase 2 snapshot has diverged. Evict the
   url_path row, decrement the prior blob's refcount, log
   `hit_path_hash_evicted`, fall through to §6.2 to re-fetch.
6. Two or more conflicting declared rows: same fail-closed behavior as
   §6.2 (502 + Retry-After: 60, log `package_hash_conflict`).
```

The query in step 2 is the same shape as §6.2's miss-path validation —
share one helper, one log call site. The lookup is O(1) on the
`package_hash` PK index plus a covering join to `suite_freshness`; the
extra latency on a hit is sub-millisecond and only paid for paths that
have a `package_hash` row covering them.

### 6.2 Cache miss: singleflight fetch (deltas)

**For metadata under a suite with `current_snapshot_id`:** this should
not happen — every member of an adopted snapshot was prefetched into
`pool/` during adoption, and `snapshot_member` rows enforce the
mapping. If it does happen (operator deletion of a pool blob, disk
corruption discovered by the integrity scan), the request falls through
to the Phase 1 singleflight fetch path; the resulting fetch is hash-
validated against `snapshot_member.declared_sha256` (§6.5). A second
covering snapshot adoption is *not* triggered — the missing blob is a
local fault, not an upstream change.

**For metadata under a suite with no `current_snapshot_id`** (pre-Phase-2,
or first request for a brand-new suite): Phase 1 behavior. The miss path
fetches and inserts a `url_path` row. The first successful freshness
check on this suite then transitions it into the snapshot model.

**For `.deb` paths:** Phase 1 singleflight fetch carries forward. After
the fetch, before promoting the temp blob into the pool, the handler
queries `package_hash` for every current snapshot covering
`(canonical_scheme, canonical_host, path)`. The query returns
*distinct* declared hashes — the same .deb path can legitimately appear
in multiple suites' snapshots (e.g. `noble` and `noble-updates` both
indexing the same package version), and we must not pick an arbitrary
row when those hashes diverge:

```sql
SELECT DISTINCT declared_sha256
  FROM package_hash p
  JOIN suite_freshness sf ON sf.current_snapshot_id = p.snapshot_id
  WHERE p.canonical_scheme = ? AND p.canonical_host = ? AND p.path = ?;
```

- **Zero rows:** Phase 1 trust-upstream fallback. Insert as in SPEC §6.2.
  Log a `package_hash_miss` Debug event with the canonical host and
  path so operators can monitor coverage gaps.
- **Exactly one row, hash matches the fetched bytes:** continue to pool
  insert.
- **Exactly one row, hash mismatch:** discard the temp blob, log a
  `hash_validation_failure` event with declared/observed/snapshot_id,
  return `502` + `Retry-After: 60`. Do *not* insert the `url_path` row;
  the next request retries.
- **Two or more distinct hashes:** snapshots disagree on what this path
  should contain. Fail closed: discard the temp blob, log
  `package_hash_conflict` with all declared hashes and their
  snapshot_ids, return `502` + `Retry-After: 60`. This is an upstream
  signal (mirror divergence, partial sync) the operator must
  investigate; serving an arbitrary one of the conflicting hashes is
  worse than refusing.

### 6.3 Resumable upstream fetch
Unchanged — see SPEC.md §6.3.

### 6.4 Cache miss with upstream down (deltas)

For metadata: when the upstream fetch fails on a miss path described in
§6.2 (rare; only happens for the local-fault case), `HIT-STALE` from the
`current_snapshot` member set is the natural fallback — the snapshot is
the frozen consistent set by definition. SPEC §6.4 carries forward
unchanged otherwise.

For `.deb`: SPEC §6.4 unchanged. `502` + `Retry-After: 60`.

### 6.5 Hash validation (now active)

Phase 1 deferred this; SPEC §6.5 stated "Phase 2 closes this hole."

**On metadata fetch (during adoption):**
- Fetcher wraps the destination so `sha256(stream)` is computed
  incrementally. Compared to `snapshot_member.declared_sha256` after the
  body completes. Mismatch → discard the staging file, abort the
  adoption (the candidate snapshot is incomplete and never flips), log
  `adoption_member_mismatch` with declared/observed/path/snapshot_id.

**On `.deb` fetch (during a request-path miss):**
- As described in §6.2 above. The fetcher always validates against
  `package_hash` when a row exists; mismatch produces a `502` and never
  inserts the blob.

**At rest (periodic):**
- A dedicated worker pool (`integrity.validate_at_rest_workers`) walks
  every `snapshot_member` and `package_hash` row whose blob is on disk,
  hashing the file and comparing to the declared SHA256. Detected
  mismatches log `at_rest_corruption` with blob hash and the declaring
  snapshot, then remove the blob from `pool/`. Subsequent requests miss,
  re-fetch, and re-validate against the same declared hash, restoring a
  good blob. Cadence: `integrity.validate_at_rest_interval` (default
  24h, `0` disables).

### 6.6 Upstream allowlist
Unchanged — see SPEC.md §6.6. The allowlist applies equally to adoption
member fetches and request-path fetches; both go through `fetch.Client`.

---

## 7. Freshness and adoption

### 7.1 Triggers
Unchanged from SPEC §7.1: T1 (request-path metadata hit) and T2 (periodic
suite scan) are the only triggers. Phase 2 does not introduce a manual
trigger for the locked spec; an admin endpoint can be added later
without touching the contract.

### 7.2 Algorithm (deltas)

The 200-with-changed-bytes branch of SPEC §7.2 — which Phase 1 logged
"awaiting Phase 2 atomic flip" and returned — now invokes the **adoption
flow** (§7.5). Specifically:

```
On freshness check that observed a new InRelease at upstream:
  if adoption.enabled == false:
    // Phase 1 behavior: record diagnostic, do not adopt.
    cur.InReleaseChangeSeenAt = nowUnix
    persist cur; log "InRelease changed; adoption disabled"; return.
  else:
    runAdoption(cur, new_InRelease_bytes, new_etag, new_lastmod)
```

All other branches of SPEC §7.2 (cooldown, 304, 200-bytes-unchanged,
error) are unchanged.

### 7.3 Off the request path
Unchanged.

### 7.4 Periodic scheduler
Unchanged (5s floor on the fast tick interval, etc.).

### 7.5 Adoption flow (NEW)

Adoption is a goroutine spawned by the freshness check that observed
new bytes. The same per-suite `sync.Mutex` from SPEC §7.3 guards the
entire adoption to prevent two overlapping adoptions on the same suite.

```
runAdoption(suite, new_bytes, etag, lastmod, mode):
  // mode ∈ { "inline", "detached" }; the freshness check that triggered
  // adoption knows which of InRelease or Release+Release.gpg it
  // observed.

  // Step 0: global concurrency cap (§9.3.1).
  acquire freshness.max_concurrent_adoptions slot (or skip if cap=0)
  defer release

  // Step 1: GPG verify (§7.6).
  release_text := verify(new_bytes, suite)         // verified plaintext
  if verification failed:
    log "adoption_gpg_failed"; persist InReleaseChangeSeenAt; return.

  // Step 2: persist the verified Release-equivalent blob(s) into pool/
  // BEFORE any suite_snapshot row references them. Required by the
  // suite_snapshot.inrelease_hash / release_hash / release_gpg_hash FK
  // constraints — the candidate row in step 4 cannot insert until the
  // referenced blob row is durable.
  if mode == "inline":
    inrelease_hash := writeBlob(new_bytes)         // sha256 of clearsigned bytes
  else: // mode == "detached"
    release_hash      := writeBlob(release_bytes)
    release_gpg_hash  := writeBlob(release_gpg_bytes)
  // writeBlob is idempotent: if pool/<hash> already exists it rehashes
  // the file (M3 defense; see "rehash on reuse" below) and either
  // confirms or evicts-and-rewrites.

  // Step 3: parse Release/InRelease, build the declared SHA256 → relative path map.
  members := parse_sha256_block(release_text)
  if no members or parse error:
    log "adoption_parse_failed"; persist InReleaseChangeSeenAt; return.

  // Step 4: insert candidate suite_snapshot row (adopted_at = NULL).
  // FK to inrelease_hash / release_hash / release_gpg_hash all resolve
  // because step 2 stored them.
  snapshot_id := INSERT INTO suite_snapshot ...;

  // Step 5: prefetch declared members sequentially.
  for path, declared_sha256 in members:
    if pool/<declared_sha256> already exists:
      actual := sha256(pool/<declared_sha256>)
      if actual == declared_sha256:
        // Content addressing held — free dedup.
        record snapshot_member row in candidate set; continue.
      // Pool blob has been corrupted at rest. Remove the corrupted
      // file, log "pool_corruption_during_adoption" with path/blob,
      // proceed to fetch fresh.
      remove pool/<declared_sha256>
      decrement blob.refcount, drop blob row if 0
    fetch member into staging/<snapshot_id>/<declared_sha256>
    hash-verify against declared_sha256; mismatch → abort (§7.5.2).
    rename into pool/<declared_sha256>.
    record snapshot_member row in candidate set, keyed on the suite-
    relative `path` from the Release file.

  // Step 6: insert "metadata-self" snapshot_member rows so request-
  // path lookups for the verified InRelease / Release / Release.gpg
  // hit the snapshot directly. Without these the §6.1 snapshot-scoped
  // lookup would 404 on the very paths apt fetches first.
  if mode == "inline":
    record snapshot_member row { path: "InRelease",
                                 blob_hash: inrelease_hash,
                                 declared_sha256: inrelease_hash }
  else:
    record snapshot_member row { path: "Release",
                                 blob_hash: release_hash,
                                 declared_sha256: release_hash }
    record snapshot_member row { path: "Release.gpg",
                                 blob_hash: release_gpg_hash,
                                 declared_sha256: release_gpg_hash }
  // For metadata-self rows the declared_sha256 is the verified blob's
  // own hash — the verification step is GPG, not a Release-listed
  // SHA256 (the Release file cannot list itself). The CHECK on
  // declared_sha256's shape (64 hex chars) is satisfied because every
  // pool hash is sha256.

  // Step 7: insert by-hash alias snapshot_member rows. apt's
  // Acquire-By-Hash clients fetch from <suite>/<component>/by-hash/
  // SHA256/<declared_sha256>; without an alias row those requests
  // would 404 under the §6.1 snapshot-scoped lookup. The alias path
  // is constructed by stripping the filename component from the
  // declared path and appending `by-hash/SHA256/<declared_sha256>`.
  for path, declared_sha256 in members:
    alias := byHashAliasPath(path, declared_sha256)
    record snapshot_member row { path: alias,
                                 blob_hash: declared_sha256,
                                 declared_sha256: declared_sha256 }

  // Step 8: parse every Packages member to populate package_hash rows.
  for each member whose path matches Packages*:
    parse the Packages text:
      for each pkg with Filename and SHA256:
        record package_hash row in candidate set.

  // Step 9: atomic flip (§7.5.1).

  // Step 10: log "adoption_success" with snapshot_id, prior_snapshot_id,
  //          member_count, alias_count, package_hash_count.
```

The `writeBlob` helper used in step 2 is the same primitive used by the
request path's miss handler (SPEC §9.4 cache writer). It serializes
through the writer goroutine, performs the temp-file → rename promotion,
and inserts the `blob` row as part of the same SQLite transaction that
publishes it. Calling it during adoption ensures one rule: every
`blob.hash` value referenced by a `suite_snapshot` column or
`snapshot_member.blob_hash` row is durable on disk *before* the row that
references it is committed.

Step 5's "rehash on reuse" defense matters because pool blobs predating
Phase 2 were inserted under the trust-upstream model — their on-disk
content was not verified against a declared hash at the time. If a
prior fetch wrote corrupt bytes (ENOSPC race, kernel bug, exotic FS
behavior) and the blob's filename happens to match a declared hash now,
recording it as a `snapshot_member` would promote the corruption into
the verified set. Re-hashing on reuse pays an O(filesize) cost per
adoption-time reuse but bounds the trust-set to bytes we have *just*
verified. Members re-fetched from upstream don't pay this cost; the
fetch path's stream-side hashing is sufficient.

#### 7.5.1 Atomic flip transaction

A single SQLite transaction performed by the cache writer goroutine
(SPEC §9.4):

```sql
BEGIN;
  -- Insert all snapshot_member rows for the candidate snapshot.
  INSERT INTO snapshot_member ...;          -- one per member

  -- Insert all package_hash rows.
  INSERT INTO package_hash ...;             -- one per .deb in any Packages member

  -- Bump refcounts for every blob now referenced by the new snapshot.
  -- Each blob counted once (multiple snapshot_member or package_hash rows
  -- pointing at the same blob count once for refcount purposes — the
  -- adoption goroutine deduplicates the bump set before the transaction).
  UPDATE blob SET refcount = refcount + 1
    WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ?);

  -- Read the prior snapshot for this suite (NULL on first adoption).
  prior_id := SELECT current_snapshot_id FROM suite_freshness ...;

  -- Flip the pointer.
  UPDATE suite_freshness SET current_snapshot_id = ? WHERE ...;

  -- Mark the new snapshot adopted.
  UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?;

  -- Decrement refcounts for the prior snapshot's members (no DELETEs;
  -- Phase 4 GC reaps refcount=0 blobs).
  IF prior_id IS NOT NULL:
    UPDATE blob SET refcount = refcount - 1
      WHERE hash IN (SELECT blob_hash FROM snapshot_member WHERE snapshot_id = prior_id);
COMMIT;
```

The flip is a single transaction. Either every read after the commit
sees the new `current_snapshot_id` and every member it implies, or every
read before the commit sees the prior one. There is no half-flipped
state visible to a request goroutine.

#### 7.5.2 Failure handling

If any step before the COMMIT fails (GPG verify, parse, member fetch,
member hash mismatch, candidate row insert):

- The candidate `suite_snapshot` row's `adopted_at` is never set; it
  remains `NULL` in the database. Phase 4 GC will reap orphan candidate
  snapshots; for Phase 2 the rows are harmless (no foreign-key
  references to them, no `current_snapshot_id` pointing at them).
- The `staging/<snapshot_id>/` directory is left for the SPEC §4.2 sweep
  to reap on the next start-up cycle, or — if the adoption goroutine
  exits cleanly — removed inline.
- `suite_freshness.last_check_at` is bumped (so the cooldown gate
  applies to the next attempt), `last_success_at` is *not* bumped (the
  prior `last_success_at` continues to drive the periodic scheduler).
- `suite_freshness.inrelease_change_seen_at` is bumped so an operator
  reading the diagnostic sees the divergence is still pending adoption.
- A structured log event names the failure category
  (`adoption_gpg_failed`, `adoption_member_fetch_failed`,
  `adoption_member_mismatch`, `adoption_parse_failed`,
  `adoption_db_failed`).

The next periodic tick retries; the cooldown gate prevents thrash.

### 7.6 GPG verification (NEW)

#### 7.6.1 Trust set

At startup the cache constructs an in-memory keyring by reading every
file under `/etc/apt/trusted.gpg.d/` and `/etc/apt/keyrings/` that is
either a binary keyring (`*.gpg`) or an ASCII-armored keyring (`*.asc`).
Files that fail to parse are logged at WARN and skipped — the cache
proceeds with whatever subset parsed cleanly. An empty resulting keyring
is a startup error iff `adoption.enabled = true` AND
`adoption.require_signature = true`.

For each `[[trusted_signer]]` block, the cache compiles the regex and
canonicalizes the fingerprint list (uppercase, no whitespace).

#### 7.6.2 Per-suite trust set resolution

For a suite identified by `(canonical_scheme, canonical_host, suite_path)`:

```
matching_blocks := every [[trusted_signer]] whose regex matches canonical_host
if matching_blocks is empty:
  if adoption.require_pinned_signer == true:
    // Fail closed: no pin, no adoption.
    log "adoption_unpinned_suite" with canonical_host, suite_path; abort.
  else:
    trust_set := the entire host keyring
else:
  union := UNION of matching_blocks[*].fingerprints
  trust_set := host_keyring keys whose fingerprint is in union
```

The intersection semantics are deliberate: a `[[trusted_signer]]` block
*narrows* trust below the host keyring; it cannot grant trust to a key
the host doesn't already trust. To add a key, the operator adds it to
the host apt keyring (or symlinks a key into
`/etc/apt/trusted.gpg.d/`). To pin to a subset, they add a
`[[trusted_signer]]` block.

#### 7.6.3 Verification

The verifier accepts both forms (Q6):

- **Inline `InRelease`** (clearsigned message). Verify the signature
  against `trust_set`. Output: the verified plaintext (the Release-style
  text used to extract the SHA256 → path map).
- **Detached `Release` + `Release.gpg`.** When the cache receives a 404
  on `<suite>/InRelease` (or the upstream's `Release` advertises no
  inline form), fetch `<suite>/Release` and `<suite>/Release.gpg` in
  parallel. Verify the detached signature over the `Release` bytes.
  Output: the `Release` bytes verbatim.

Both paths converge on "verified Release-equivalent text" before §7.5
parsing.

A signature is valid iff:
- The signing key is in `trust_set`.
- The signature's `Issuer Fingerprint` matches a key in `trust_set`
  (long-form fingerprints; short key-id matches are insufficient and
  rejected).
- The signing key is not expired or revoked at the time of verification.
- The signature is cryptographically valid.

All other outcomes — including missing signatures when
`require_signature = true`, and signatures from keys outside `trust_set`
— produce a verification failure that aborts adoption (§7.5.2).

#### 7.6.4 Library

`github.com/ProtonMail/go-crypto/openpgp` (Q5). Pure Go, no cgo, tagged
module, drop-in API equivalent to the deprecated
`golang.org/x/crypto/openpgp`. Used by ProtonMail and HashiCorp Vault
against real-world keyring corpora.

#### 7.6.5 Threat model and recommended posture

The default trust set (the whole host apt keyring) is convenient but
broad: any key in `trusted.gpg.d` or `/etc/apt/keyrings/` can sign any
suite the cache is asked to adopt. This matches apt's own historical
default — apt itself only narrowed it with the `Signed-By:` source
option in recent releases. The risk:

- A compromised third-party PPA key could in theory be used to sign a
  forged Ubuntu archive `Release` and adopt it as the canonical view of
  `archive.ubuntu.com`. The adoption would succeed because the signing
  key's fingerprint is in the keyring even though apt clients on the
  fleet would never honor that key for that origin.
- The blast radius is bounded — the fleet's apt clients enforce their
  own per-source `Signed-By:` rules and would reject the forged
  metadata when the cache served it on. But the cache would still have
  spent disk and bandwidth on a bogus snapshot, and concurrent clients
  could see the forged set transiently before any one of them rejected
  the signature. Not a quiet exposure, but not zero either.

**Recommended production posture:**

1. Add a `[[trusted_signer]]` block for every active suite. Most fleets
   only adopt a small set of canonical hosts (`archive.ubuntu.com`,
   `security.ubuntu.com`, one or two PPAs), so the configuration cost
   is bounded.
2. Set `adoption.require_pinned_signer = true`. With this enabled, an
   unpinned suite fails closed and the failure is logged with the
   suite identity, making it impossible to accidentally adopt a suite
   whose trust hasn't been explicitly scoped.
3. Treat the host apt keyring as a *superset bound* on trust, never as
   the trust set itself in production.

The default for `require_pinned_signer` is `false` only because flipping
the default would break first-time deployments where no
`[[trusted_signer]]` blocks have been written yet. The intended
operational lifecycle is:

- Deploy with `enabled = false` (Phase 1 behavior).
- Add `[[trusted_signer]]` blocks for every suite the deployment adopts.
- Flip `enabled = true` and `require_pinned_signer = true` together.

A future Phase could parse `Signed-By:` from
`/etc/apt/sources.list.d/*.list` and `*.sources` to derive pins
automatically. That is out of scope for Phase 2 — the parser would
need to handle apt's full sources-list syntax, including options
quoting and the deb822 format, which is its own correctness surface.

---

## 8. Stale-and-Valid-Until (deltas)

SPEC §8 carries forward. Phase 2 narrows the meaning of "served from
frozen consistent set during freshness divergence":

- The frozen set is the `current_snapshot` of the suite, which by
  construction (§7.5.1 atomic flip) is always self-consistent: every
  index references blobs whose declared SHA256s match what's in
  `snapshot_member`.
- When a new `InRelease` is detected mid-update and adoption is in
  progress, request-path traffic continues to be served from the prior
  `current_snapshot`. Adoption only flips the pointer at the very end
  of §7.5.1, so a client cannot land mid-flip.
- The previously diagnostic-only `inrelease_change_seen_at` column
  retains its meaning: it surfaces if adoption is *failing* (e.g. GPG
  verification keeps failing). Operators monitor it as a "we keep seeing
  upstream change but can't adopt" signal.

`Acquire::Check-Valid-Until=false` guidance (SPEC §8 last bullet) is
unchanged.

---

## 9. Concurrency & deadlines

### 9.1 Per-request
Unchanged.

### 9.2 Singleflight
Unchanged.

### 9.3 Per-host concurrency on upstream
Unchanged — the same `hostsem.Sem` shared by the handler, the freshness
checker, and (now) the adoption goroutine. Adoption member fetches
contend with request-path miss fetches for the same per-host slots.

#### 9.3.1 Global adoption cap (NEW)

A separate cache-wide semaphore `freshness.max_concurrent_adoptions`
(default 2; 0 = unlimited) bounds how many adoptions run concurrently
across the whole cache. Acquired at the top of the adoption goroutine
(§7.5 step 0), released after the atomic flip or abort. Combined with
sequential member fetches inside an adoption (§7.5 step 4), the
in-flight adoption load on any one host is bounded by the cap — a
restart storm of N suites adopting against the same host produces at
most `cap` concurrent member fetches to that host, instead of N.

The acquisition is non-nested: at the top of the adoption goroutine,
once. No two semaphores are held simultaneously by the adoption code,
so there is no deadlock surface (Q3 rationale: option (b)
fraction-reserved would have introduced one; this hybrid avoids it).

### 9.4 SQLite concurrency
Unchanged. The atomic-flip transaction (§7.5.1) is a single multi-statement
write submitted to the writer goroutine; reads (request-path lookups)
share the connection pool freely throughout.

### 9.5 Graceful shutdown (deltas)

The SPEC §9.5 sequence extends with one new step. The full Phase 2
sequence:

1. Stop accepting new connections (concurrent `Shutdown` on plain + TLS
   listeners).
2. Wait up to 30s for in-flight requests to drain.
3. Force-close any connection that did not drain within the budget.
4. Stop the periodic freshness scheduler.
5. **(NEW)** Cancel any in-flight adoption goroutines. Adoption uses
   `lifecycleCtx`; a shutdown cancel propagates into the member fetcher
   and the verify step. Aborted adoptions leave their `staging/<id>/`
   subdirectory for the next start-up sweep (§4.2). The candidate
   `suite_snapshot` row is harmless residue (no `current_snapshot_id`
   pointing at it) and is ignored on the next start until Phase 4 GC.
6. Cancel any in-flight upstream fetches (handler + freshness paths).
7. Stop the at-rest integrity scanner (it reads no upstream state, so
   shutdown is a simple ctx cancel).
8. Flush SQLite.
9. Exit.

The 30s drain budget covers both request-path traffic and the
shutdown-cancel of adoption + scanner. In practice adoption is the
longest-running goroutine class — a member fetch can use the full
`upstream.total_timeout` (default 5m) — so adoption goroutines may not
finish within the drain budget. They terminate on `lifecycleCtx` cancel
just like Phase 1's miss-path fetches (SPEC §9.5 step 3 → Phase 2 step 6),
and the in-progress staging files become orphans for the sweep. This is
deliberate: leaving a half-fetched snapshot mid-flight at shutdown is
the safe default; the next start retries from scratch.

---

## 10. Logging (deltas)

Phase 1 logging (SPEC §10) carries forward exactly. Phase 2 adds:

### 10.1 Per-request line additions

- `snapshot_id` (when the response served from a snapshot, i.e. the
  snapshot model resolved this metadata path). Absent for blob
  responses, fallback metadata responses, and error paths.

### 10.2 New structured events

- **Adoption attempts:** `adoption_started` and `adoption_finished` Info
  with `canonical_host`, `suite_path`, `snapshot_id`, `prior_snapshot_id`
  (or 0 on first adoption), and `result` ∈ {`success`, `gpg_failed`,
  `parse_failed`, `member_fetch_failed`, `member_mismatch`, `db_failed`,
  `aborted`}. `member_count` and `duration_ms` on `adoption_finished`.
- **GPG verification failures:** `gpg_verify_failed` Info with
  `canonical_host`, `suite_path`, `signing_key_fingerprint` (when the
  signature parsed at all; empty otherwise), and `reason` ∈
  {`untrusted_key`, `expired_key`, `revoked_key`, `invalid_signature`,
  `missing_signature`, `parse_error`}.
- **Hash validation failures:**
  - `adoption_member_mismatch` Error during adoption: `canonical_host`,
    `suite_path`, `path`, `declared_sha256`, `observed_sha256`,
    `snapshot_id`.
  - `package_hash_mismatch` Error during a `.deb` miss:
    `canonical_host`, `path`, `declared_sha256`, `observed_sha256`,
    `snapshot_id` (the snapshot whose `package_hash` row vouched).
  - `package_hash_conflict` Error when two or more current snapshots
    declare different sha256s for the same `(canonical_host, path)`:
    `canonical_host`, `path`, plus a JSON array of
    `{snapshot_id, declared_sha256}` pairs.
  - `hit_path_hash_evicted` Warn when §6.1's blob-hit validator finds
    a `url_path` row whose `blob_hash` disagrees with the snapshot's
    `package_hash.declared_sha256`: `canonical_host`, `path`,
    `evicted_blob_hash`, `declared_sha256`, `snapshot_id`. The eviction
    is the corrective action; the next request re-fetches.
  - `at_rest_corruption` Error from the integrity scanner: `blob_hash`,
    `declared_sha256` (whichever `snapshot_member` or `package_hash` row
    surfaced the mismatch — first-found is reported), `snapshot_id`.
  - `pool_corruption_during_adoption` Warn when the §7.5 step-5 reuse
    rehash finds an existing pool blob whose content no longer matches
    its filename: `path`, `expected_sha256`, `observed_sha256`. Adoption
    re-fetches the member from upstream after the eviction.
- **Trust-set events:** `adoption_unpinned_suite` Warn when
  `adoption.require_pinned_signer = true` and a triggering suite has no
  matching `[[trusted_signer]]` block: `canonical_host`, `suite_path`.
  Adoption aborts before the verify step. Operator surface: a deployed
  cache rejecting adoptions for a suite the operator hadn't pinned.
- **At-rest scan:** `at_rest_scan_started` and `at_rest_scan_finished`
  Info with `blob_count`, `mismatch_count`, `duration_ms`.
- **Coverage gaps:** `package_hash_miss` Debug per `.deb` cache miss
  whose canonical (host, path) is not vouched for by any current
  snapshot. Debug-level so noisy deployments aren't drowned, but
  available for ops to count.

### 10.3 Startup config dump (additions)

Append: `adoption_enabled`, `adoption_require_signature`,
`adoption_require_pinned_signer`, `integrity_validate_at_rest_interval`,
`integrity_validate_at_rest_workers`, `max_concurrent_adoptions`,
`trusted_signer_blocks` (count of compiled blocks; details would explode
the line and aren't useful in the journal), `apt_keyring_keys` (count of
trusted keys loaded at startup — proves the keyring path resolved).

---

## 11. Failure-mode catalog (deltas)

Phase 1 rows (SPEC §11) carry forward unchanged. Phase 2 adds:

| Scenario | Phase 2 behavior |
|---|---|
| Upstream serves valid signed `InRelease`, but a referenced index fails to fetch after retries | Adoption aborts; prior snapshot stays current; periodic_refresh retries on next tick; `adoption_member_fetch_failed` logged. |
| Upstream serves valid signed `InRelease`, member fetched but its sha256 mismatches the declared hash | Adoption aborts; staging member discarded; `adoption_member_mismatch` logged loudly; prior snapshot stays current. |
| Upstream serves a forged `InRelease` (valid bytes but bad signature, or signing key not in trust set) | Adoption aborts; cache keeps serving prior snapshot; `gpg_verify_failed` Info; `adoption_finished` `result=gpg_failed`. The defining Phase 2 test scenario. |
| Upstream serves an unsigned `InRelease` (or `Release` without `Release.gpg`) and `require_signature=true` | Adoption aborts (`reason=missing_signature`); same operator surface as forgery. |
| `.deb` cache miss whose declared sha256 (in `package_hash`) mismatches the body upstream returned | `502` + `Retry-After: 60`; blob discarded; `package_hash_mismatch` logged; row not inserted; next request retries. |
| `.deb` cache miss with no matching `package_hash` row (suite hasn't adopted, or .deb is in a suite the cache hasn't seen) | Phase 1 fallback: trust upstream, insert as in SPEC §6.2; `package_hash_miss` Debug. |
| At-rest scan finds blob whose on-disk content disagrees with the declared sha256 | Blob removed from `pool/`; `at_rest_corruption` Error; next request misses, re-fetches, re-validates. |
| `v1 → v2` migration interrupted | Tx rolls back; next start retries from `schema_version = 1`. |
| Operator flips `adoption.enabled = false` mid-run | New observations of changed `InRelease` log "adoption disabled" and persist `inrelease_change_seen_at`; existing `current_snapshot_id`s keep serving normally. |
| `[[trusted_signer]]` block has a regex matching a suite, but the fingerprint list doesn't intersect the host keyring | Trust set for that suite is empty → every adoption attempt fails `gpg_verify_failed` `reason=untrusted_key`. Detectable at startup config validation if the host keyring is also empty; not detectable otherwise (we cannot know which fingerprints are *expected* to be present at startup). |
| `adoption.require_pinned_signer=true` and a freshness check observes a new `InRelease` for a suite with no matching `[[trusted_signer]]` block | Adoption aborts before the verify step; `adoption_unpinned_suite` Warn logs the canonical host and suite path; prior snapshot continues to serve. Operators expect this surface and use it to drive their pinning rollout. |
| Two distinct current snapshots reference the same `(canonical_host, .deb path)` with different declared sha256s | Both the §6.1 hit path and the §6.2 miss path fail closed with `502 + Retry-After: 60` and a `package_hash_conflict` log. This blocks .deb traffic for the affected path until the upstream divergence is resolved or one of the snapshots is re-adopted; the operator surface is unmistakable. |
| §6.1 blob hit finds `url_path.blob_hash` disagreeing with `package_hash.declared_sha256` (stale Phase 1 row covered by a Phase 2 snapshot) | Evict the `url_path` row, decrement the prior blob's refcount, log `hit_path_hash_evicted`, fall through to §6.2 to re-fetch. The corrective re-fetch then runs the §6.2 validation; either it lands on the correct hash (steady state) or it produces a `package_hash_mismatch` if upstream is also wrong. |
| §7.5 step 5 finds an existing `pool/<hash>` whose content no longer matches its filename | Remove the corrupted file and `blob` row, log `pool_corruption_during_adoption`, fall through to fetch the member fresh from upstream. The member's hash is then validated against the declared sha256 as in any new fetch. |
| Adoption sees a path mentioned in `Release` whose `by-hash` alias collides with a snapshot member at a different declared sha256 | The candidate snapshot's `snapshot_member` PK is `(snapshot_id, path)`, so the second INSERT fails inside the candidate transaction; adoption aborts with `adoption_db_failed`. This is a malformed `Release` (apt's own validators would reject it); the failure mode is loud rather than silent. |

---

## 12. Test strategy (deltas)

Phase 1's tests (SPEC §12.1–§12.5) all carry forward and must continue to
pass. Phase 2 adds:

### 12.1 Unit tests (additions)

- **GPG keyring loader.** Parsing real-world keyring files (a fixture
  bundling the ubuntu archive key, a debian archive key, and a known-bad
  ASCII-armored file) against expected fingerprint sets.
- **GPG signature verification.** Both inline and detached forms; valid,
  invalid, expired, revoked, and key-outside-trust-set scenarios.
- **`Release` / `InRelease` parser.** Extracts the SHA256 → path block;
  rejects malformed input; tolerates the canonical Debian-style headers
  including the `MD5Sum` and `SHA1` blocks the cache ignores.
- **`Packages` parser.** `Filename` + `SHA256` line extraction across the
  paragraph format. Compressed (`Packages.gz`, `Packages.xz`) handled.
- **Snapshot atomic flip.** Goldens for refcount math: insert candidate,
  flip, verify refcounts; flip again, verify prior decrement.
- **Migration v1 → v2.** Apply against a Phase 1 snapshot, verify
  schema; idempotent re-apply is a no-op; an interrupted migration
  rolls back cleanly.

### 12.2 Integration tests (additions)

- `FakeUpstream` extended to optionally serve a signed `InRelease`
  (using a fixture key the test injects into the cache's trust set) and
  to swap the served `InRelease`/`Packages` mid-test for divergence
  scenarios.
- Test cases for every row added to §11.

### 12.3 Phase 2 chaos test: mid-adoption divergence (the gate)

```
GIVEN
  a cache with adopted snapshot A for suite "noble"
  upstream now publishing snapshot B (new signed InRelease + new Packages
    referencing five distinct .debs from A)
WHEN
  the cache observes the new InRelease (T2 fires)
  adoption begins; member prefetch is in flight
  during prefetch, 100 concurrent clients each issue
    {GET InRelease, GET Packages, GET 5 .deb} via apt-style requests
THEN
  every one of the 700 client requests returns 200
  every response body matches either A's bytes or B's bytes (never mixed)
  no client receives A's InRelease together with B's Packages or vice versa
  the .deb hashes received by every client match the InRelease/Packages
    consistent with that same client's metadata
  cache RSS stays under 256 MB throughout
  adoption either completes during the test (B becomes current) or aborts
    cleanly (A remains current) — no partial state visible
```

The test uses an upstream that swaps content mid-test (e.g. between
client batch 50 and batch 51 the upstream begins serving B). The
correctness assertion is byte-level consistency per client across its
own request sequence.

### 12.4 Phase 2 chaos test: GPG forgery rejection

```
GIVEN
  a cache with adopted snapshot A for suite "noble"
  upstream now publishing a new InRelease whose bytes parse cleanly but
    whose signature is from a key NOT in the host keyring
WHEN
  the cache observes the new InRelease (T2 fires)
THEN
  adoption attempt aborts with result=gpg_failed
  cache continues serving snapshot A
  inrelease_change_seen_at is set so operators see the divergence
  no candidate suite_snapshot row materializes (or, if it does, has
    adopted_at = NULL forever)
  no staging/<id>/ subdirectory persists past the next §4.2 sweep
```

### 12.5 At-rest scan correctness

A small fixture: prime the cache with two snapshots, corrupt one blob
in `pool/` directly via the filesystem, run the scan, verify the
corrupted blob is removed and a `at_rest_corruption` event is logged
with the correct snapshot_id. A subsequent request for that path
re-fetches and serves the good content.

### 12.6 v1 → v2 migration end-to-end

Spin up the Phase 1 binary against a fresh `cache_dir`, prime it with a
representative request set (one `InRelease` fetch, one `Packages`, one
`.deb`). Stop the binary. Start the Phase 2 binary against the same
directory with `adoption.enabled = true`. Verify:

- The DB migrates cleanly (`schema_version` becomes 2).
- The original `url_path` rows still serve the existing requests.
- The next `InRelease` change at upstream triggers a successful
  adoption.
- After adoption, the same metadata path now serves with
  `X-Cache-Snapshot` set in the response.

This test runs in CI alongside the existing `e2e/` flow.

### 12.7 Soak (manual / nightly)

Phase 1's soak (SPEC §12.5) extends to: assert no leak in `staging/`
across rolling adoptions, no growth in the candidate-snapshot count
beyond what Phase 4 would reap.

---

## 13. Project layout & tooling (deltas)

Phase 1 layout (SPEC §13) carries forward. Phase 2 adds:

```
internal/
  freshness/        # Phase 1 + Phase 2 adoption flow extends this package.
                    #   adoption.go: runAdoption, atomic flip, member prefetch.
                    #   release_parse.go: InRelease/Release SHA256 → path map.
                    #   packages_parse.go: Packages → declared SHA256 per .deb.
  gpg/              # NEW. Keyring loading from /etc/apt/trusted.gpg.d/ and
                    # /etc/apt/keyrings/, [[trusted_signer]] resolution,
                    # InRelease + detached Release.gpg verification.
  integrity/        # NEW. At-rest scanner: ticker, worker pool, corruption
                    # handling. Reads cache state via the cache package; no
                    # upstream calls.
```

Adoption is intentionally inside `freshness/` rather than a parallel
`adoption/` package — adoption is "what the freshness checker does when
it observes a change," sharing the per-suite mutex map and the
periodic-scheduler integration. A new package would split tightly-bound
state across two import boundaries for no benefit.

`go.mod` additions:

- `github.com/ProtonMail/go-crypto/openpgp` (and its required
  transitive deps).

CI jobs from SPEC §13 carry forward. The `go test -race ./...` job now
includes the §12.3 and §12.4 chaos tests; the `e2e/` job runs the §12.6
migration test as a new check.

---

## 14. Definition of done

Phase 2 is done when:

1. Every Phase 1 chaos test (SPEC §12.3) and end-to-end test (SPEC §12.4)
   continues to pass — Phase 2 must not regress Phase 1 behavior.
2. The Phase 2 mid-adoption divergence chaos test (§12.3) passes 10
   consecutive runs with no flakes.
3. The Phase 2 GPG forgery rejection chaos test (§12.4) passes 10
   consecutive runs.
4. The at-rest scan correctness test (§12.5) passes.
5. The `v1 → v2` migration end-to-end test (§12.6) passes; an existing
   Phase 1 deployment can be upgraded by replacing the binary and
   restarting, with no data loss and continued service of pre-Phase-2
   blobs until the first adoption.
6. The `.deb` package builds, installs, and starts on Ubuntu 24.04 +
   26.04 with `adoption.enabled = true` and at least one snapshot
   adopted during the deb-install harness run. (Extends `e2e/deb/`.)
7. The cache is deployed to one production environment with
   `adoption.enabled = true` for at least one week. Monitoring shows:
   - zero `adoption_member_mismatch` events that aren't
     attributable to a known-bad upstream;
   - zero `package_hash_mismatch` events on .debs from
     allowlisted hosts;
   - the at-rest scan completes successfully on its configured
     cadence;
   - bounded RSS / FD count;
   - adoption goroutines drain cleanly on graceful shutdown
     (no leaked `staging/` subdirectories at start-up sweep).
8. SPEC2.md reflects as-built reality (this document is updated as we
   go, not just before).
