# Usage statistics implementation plan

Issue [#11](https://github.com/linsomniac/apt-cacher-ultra/issues/11), requested
by [@sdh91](https://github.com/sdh91), asks the admin dashboard to show data
fetched, data served and average transfer speeds to demonstrate caching value.

## Design

1. Add a fixed-size, concurrency-safe collector shared by the upstream client,
   cache handler and admin server. Retain no URLs, hosts, clients or credentials.
   Keep lifetime and process-session totals: upstream body bytes, successful
   client body bytes, cache-hit bytes, requests, cache-hit requests and cumulative
   durations. Use actual I/O return values, never Content-Length. Saturate integer
   counters rather than wrap them.
2. Wrap the existing upstream transport without changing its redirect, TLS,
   proxy or SSRF policies. Account for each closed response, including retries,
   redirects and background freshness/adoption reads. Count bytes read even on
   failure; unconsumed bodies and protocol overhead are outside scope.
3. Count GET/HEAD responses inside the cache handler, after CONNECT dispatch.
   Inner HTTPS requests pass through the same handler once. Exclude admin,
   readiness, CA downloads and CONNECT framing. Count successful response bytes
   (including partial writes); HEAD and 304 have no body. Classify successful
   HIT, HIT-STALE and HIT-COALESCED responses as cache hits.
4. Persist an absolute aggregate in one SQLite row through the existing writer,
   using an additive schema migration. Checkpoint every 30 seconds with a bounded
   timeout, skip unchanged totals, and flush after producers drain at shutdown.
   A failed checkpoint retains all in-memory counts for retry. Absolute snapshots
   make retry safe even if a commit succeeded but acknowledgement was lost.
   Load failures stop startup rather than silently replace existing totals.
5. Add an authenticated-by-existing-policy Usage panel and additive JSON field
   on the admin status page. Show lifetime and session totals, measurement start,
   last successful checkpoint and a persistence-failure warning. No new public
   endpoints, scripts, dependencies, reset controls or arbitrary query dimensions.
6. Define average effective download/upload rates as bytes divided by summed
   elapsed durations of body-bearing upstream responses/client requests. Include
   connection/header latency upstream and cache/fetch waits downstream; overlapping
   operations contribute separately. Empty samples produce null / unavailable.
   Net saved bytes are served minus fetched, may be negative, and are an estimate
   because background/retry costs and body representations differ.
7. Document scope, formulas, schema upgrade and persistence limits; update the
   existing contributor entry and unreleased changelog.

## Review before implementation

Self-review against the fetch, handler, cache writer, admin authentication and
shutdown paths completed before implementation. The review resolved these risks:

- Existing request logs sometimes use placeholder byte counts for errors, and
  histogram labels can hit cardinality caps. Use dedicated aggregate I/O counts.
- Counting cache writes misses discarded retries and conditional fetches; observe
  transport response reads instead. Transparent decompression remains unchanged,
  so these are application bytes rather than wire-bandwidth measurements.
- CONNECT uses hijacking. Install downstream accounting after CONNECT dispatch
  and preserve the existing writer behavior for ordinary cache responses.
- Saving deltas can double count after ambiguous timeout/commit outcomes. Save
  serialized absolute snapshots through the single cache writer.
- Transfers must not wait for persistence. Hold the collector lock only for
  arithmetic/snapshot copies and release it before database I/O.
- A crash can lose completed operations since the last checkpoint and in-flight
  transfers; report this explicitly. Persistence failures must remain visible.
- Do not claim a literal connection speed or guaranteed bandwidth saving.

## Validation

Exercise deterministic arithmetic, empty samples, saturation, concurrent updates,
checkpoint failures/retries, restart recovery, migration from schemas 7 and 8,
and final flush. Cover upstream short reads, restart/resume, redirects and
conditional GET;
client miss/hit/coalescing, range, HEAD/304, disconnects and CONNECT. Verify admin
HTML/JSON and authentication, existing render budgets and JSON compatibility.
Run focused race tests, the repository Go test suite and applicable lint/build
checks. Record actual results in the operator documentation before delivery.

## Implementation review

The implementation review checked the finished data flow and addressed these
additional findings:

- An upstream body can be closed while Read is returning partial data. Accounting
  waits for outstanding reads without holding a mutex across I/O, and records
  once even when Close is called twice.
- HEAD and 304 responses must contribute requests without adding Content-Length
  to body totals. Range responses use actual writes; rejected ranges and errors
  do not count as served data. A partial successful write still counts its bytes.
- The test harness's old readiness helper used ordinary GET `/` probes, which
  correctly counted as cache requests. Usage integration tests use `/healthz` to
  verify that the real health endpoint contributes nothing.
- The historical schema-7 test called the full migration runner but hard-coded
  version 7. It now checks CurrentSchemaVersion while retaining its existing
  migration/data-preservation assertions.
- Status responses now use Cache-Control: no-store. The panel inherits the
  existing admin authentication policy and displays no raw persistence errors.
- Desktop and mobile browser checks of the compiled daemon confirmed that the
  panel fits its container. Pre-existing overflow in other dashboard sections
  remains outside this change.

## Migration integration

Main commit `0c6d2c0` is merged into this branch. Its schema-8 migration for
representation metadata and refresh generations is preserved unchanged. Usage
storage follows as schema 9. Fresh databases run the whole sequence; schema-7
databases run 7 → 8 → 9; schema-8 databases run only 8 → 9.

Migration tests cover both existing schema versions, including preservation of
origin dates, cached blob records and schema-8 refresh generations, followed by
usage checkpoint/restart persistence. The combined handler retains main's ETag
conditional-response policy; the usage regression test exercises 304 through
If-None-Match.

## Validation results

Validated locally on 2026-11-03 with Go 1.26.8:

- `GOMAXPROCS=4 GOFLAGS=-p=2 go test -race -timeout 20m ./...` passed across
  all packages. The longer package timeout accommodates the database simulations
  on this shared machine.
- `go vet ./...`, `go vet -tags e2ehooks ./cmd/...`, and `golangci-lint run ./...`
  passed. The e2ehooks lint build passed too; the tagged test seam and usage
  integration tests passed with the race detector.
- `GOFLAGS='-p=2 -buildvcs=false' make build` passed. VCS stamping was disabled
  for the sandboxed local build; the normal Makefile version linker flag remained.
  The binary has no ELF interpreter or dynamic dependencies.
- A real daemon against a temporary local upstream fetched 1,835,008 bytes once
  and served three copies (5,505,024 bytes), reporting 3,670,016 cache-hit/saved
  bytes. A HEAD request increased requests/hits without adding bytes. Admin JSON
  matched the expected totals exactly.
- Headless Chromium checks of that daemon at 1440px and 390px widths passed;
  the seven usage rows fit their panel at both sizes. Existing JSON goldens and
  rendered HTML size-budget tests passed.
- The log-event naming/documentation contract passed after naming the new WARN
  events `usage_checkpoint_failed` and `usage_final_checkpoint_failed` and adding
  their field definitions to `docs/log-fields.md`.
- `git diff --check` passed. Docker-based real-APT suites were not run; the local
  daemon, HTTP, TLS/CONNECT, storage and concurrency suites provide this change's
  integration coverage.

### Schema reconciliation validation

Validated locally on 2026-11-04 after merging main:

- Compared the schema source with main: schemas 0 through 8 are unchanged; usage
  storage is the sole new migration, from 8 to 9.
- Focused migration, representation-metadata, validator, legacy-refresh and usage
  tests passed with the race detector. Upgrade fixtures for schemas 7 and 8
  preserve existing metadata and verify usage checkpoint/restart persistence.
- `go vet ./...`, `golangci-lint run ./...` and
  `golangci-lint run --build-tags e2ehooks ./cmd/...` passed.
- `GOMAXPROCS=4 GOFLAGS=-p=2 go test -race -timeout 20m ./...` passed
  across the combined tree, including cache, freshness, handler, daemon and
  admin integration tests.
- `git diff --check` and the staged equivalent passed.
