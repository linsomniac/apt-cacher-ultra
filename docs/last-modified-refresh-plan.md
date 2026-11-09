# Issues #17 and #18: timestamp and refresh corrections

Reviewed against the issue discussions, the cache writer and GC contracts,
snapshot verification, and Go's `http.ServeContent` precondition handling.

## Findings

- [#17](https://github.com/linsomniac/apt-cacher-ultra/issues/17): pool mtime
  and snapshot adoption time describe local work, not upstream modification.
  Changing pool mtimes is unsafe because a blob can belong to several URLs.
- [#18](https://github.com/linsomniac/apt-cacher-ultra/issues/18): without an
  adopter, changed anchors are detected but neither adopted nor invalidated.
  Both reports confirm `adoption.enabled = false` as the reproducer.

## Implementation

1. Persist upstream Last-Modified by canonical URL and content hash, with a
   blob foreign key and cascading cleanup. Bounded GC batches also remove
   dates whose URL and snapshot owners are gone, after a staging grace period.
   Preserve existing URL and snapshot anchor timestamps on upgrade. Never infer
   an upstream date from disk mtime.
2. Centralize response validators for hits, misses, recovery and stale serves.
   Parse and normalize HTTP dates; omit invalid, absent or future dates.
   Supply a strong SHA-256 ETag. Dates cannot distinguish different bytes with
   the same timestamp, or undo a previous synthetic future timestamp: ignore
   date preconditions and date If-Range, while retaining ETag preconditions
   and ETag range requests. Date-only clients receive full responses.
3. When adoption is disabled, invalidate a changed unadopted suite's mutable
   metadata atomically. The next request fetches current upstream bytes;
   immutable by-hash objects and packages remain cached. Increment a suite
   generation in the same transaction and reject fetches begun against an old
   generation. Do not invalidate an adopted snapshot or bypass verification.
4. Treat invalidation failures as failed checks, with warnings and retry
   pacing. Document the date-only bandwidth tradeoff and schema upgrade.

## Review decisions

- Timestamps are untrusted descriptive metadata, never signature or hash
  evidence. No CR/LF or arbitrary upstream header reaches the response.
- Content hashes, URL identity and snapshot identity prevent cross-repository
  timestamp confusion and stale validator reuse.
- A single writer transaction owns invalidation and generation changes;
  fetch publication checks that generation in its own transaction.
- Adoption-disabled mode keeps its existing trust-upstream policy. Signed
  snapshot validation, downgrade prevention and recovery hash checks remain.
- Existing synthetic timestamps are deliberately not accepted as evidence of
  unchanged content. This supersedes the previous synthetic Last-Modified
  response policy; internal adoption stamps remain bookkeeping.
- HTTP behavior follows [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html)
  and [ServeContent](https://pkg.go.dev/net/http#ServeContent); date validation
  is conservatively unavailable when a trustworthy modification clock is not
  available, even though an upstream date can still be displayed.

## Validation

Test timestamps on GET/HEAD, miss/hit/restart, snapshots and recovery;
deduplicated blobs at different URLs; missing/malformed/future dates; ETags,
ranges and old synthetic dates; inline/detached refresh; suite isolation,
transaction failure, adopted-suite refusal and a fetch racing invalidation.
Run the full Go suite and race checks for affected packages.

## Implementation review follow-up

- Legacy conditional checks use the selected URL row's validators, so fetching
  Release cannot lend its date or ETag to InRelease. An unchanged 200 updates
  the exact row's validators without overwriting concurrent client counters.
- The persistent generation clock prevents token reuse after suite deletion
  and recreation. Invalidations scan paths in bounded batches inside one
  transaction; by-hash downloads are exempt from the mutable metadata fence.
- Publication failures preserve the prior failure streak and emit a warning.
- The previous synthetic timestamp tests now assert origin dates and ETag
  behavior, including same-date and backward-date content changes.

## Completed validation

- Full suite: `go test -p 1 ./...` passed.
- Race detector: `go test -race -p 1 -timeout 10m ./internal/cache ./internal/freshness ./internal/handler` passed.
- Production lint: `golangci-lint run --timeout 5m ./cmd/... ./internal/...` passed.
- E2E hooks lint: `golangci-lint run --timeout 5m --build-tags e2ehooks ./cmd/...` passed.
