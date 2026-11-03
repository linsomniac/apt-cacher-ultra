# Usage statistics

The admin dashboard's **Usage** panel reports data fetched, data served, cache
hits, estimated savings and average effective transfer rates. It shows both
**Lifetime** (since collection began) and **Since start** (this daemon process).
The same data is available as `usage` in `GET /?format=json` or with
`Accept: application/json` on the admin listener. No new configuration is needed.
The dashboard keeps its existing 60-second refresh; JSON samples current
completed-operation totals when requested.

Requested by [@sdh91](https://github.com/sdh91) in
[issue #11](https://github.com/linsomniac/apt-cacher-ultra/issues/11).

## Measurements

| Measurement | Meaning |
| --- | --- |
| Data fetched | Upstream response body bytes actually read, including background freshness/adoption, retries, redirect bodies read by the HTTP client and partial failed downloads. Recorded when each response closes. |
| Data served | Successful 2xx GET response body bytes accepted by the client response writer, including partial writes before a disconnect. Recorded when the request ends. |
| Cache-hit data served | Served bytes on successful HIT, HIT-STALE or HIT-COALESCED responses. Shared downloads count once upstream and once for each client served. |
| Requests / cache hits | GET/HEAD requests reaching the cache handler, including rejected/failed requests. Hits count successful 2xx or 304 cache-hit responses. HEAD and 304 contribute no body bytes. |
| Net bytes saved | Served bytes minus fetched bytes. An estimate: background downloads and retries can make it negative, especially while warming the cache. Cache eviction does not change these totals. |
| Average download rate | Fetched bytes divided by the sum of elapsed times of upstream responses that delivered body bytes, from transport dispatch through body close. Includes connection/header latency and time spent consuming/writing the body. |
| Average upload rate | Served bytes divided by the sum of elapsed times of client requests that wrote successful body bytes. Includes cache lookup, waiting for an upstream fetch and serving the response. |

Rates are weighted by duration, not averages of individual speeds. Concurrent
operations each contribute their elapsed time. Idle periods, downtime and
responses with no body bytes do not enter the denominator. No samples display
`n/a` in HTML and `null` in JSON. Durations use Go's monotonic clock; changing the
wall clock does not produce negative durations. JSON timestamps are Unix seconds;
the dashboard localizes displayed timestamps to the browser's time zone.

These are **application body-byte measurements**, not NIC counters or network
line speeds. HTTP headers/framing, TLS/TCP overhead, socket-buffer read-ahead,
unconsumed upstream bodies, error response bodies sent to clients, admin traffic,
health probes and CA downloads are excluded. Upstream transparent decompression
keeps its existing behavior, so fetched bytes can be decoded bytes. Multipart
range boundaries are included in served body bytes. A successful write means the
server accepted the bytes; it does not prove that the client received them.

HTTP, direct HTTPS and GET/HEAD inside a CONNECT TLS session use the same
collector. CONNECT handshakes/tunnel framing are excluded. Counts appear after
operations finish; an active transfer has not yet contributed its partial bytes.

## Persistence and access

One aggregate row in `cache.db` retains lifetime totals across restarts; the
session view starts from zero. Storage follows `cache.db_dir`, falling back to
`cache.dir`. Collection continues when `admin.enabled = false`. Nothing is
reconstructed from old log files or existing cached blobs at upgrade.

The daemon checkpoints changed totals every 30 seconds, with a five-second
checkpoint timeout, and makes a final attempt after request and background
workers drain during graceful shutdown. This does no per-request database I/O
and retains no per-client, per-URL or per-host data. Memory and database storage
are bounded independently of traffic volume. Integer counters saturate at
9,223,372,036,854,775,807; summed durations use nanoseconds (about 292 years of
combined operation time at that limit).

A crash or forced termination can lose completed operations since the last
successful checkpoint, plus in-flight operations. Under normal storage conditions
the completed-operation window is about 30 seconds plus checkpoint time. A disk
failure can extend it: the admin panel shows a warning and the last successful
checkpoint timestamp, while the daemon logs `usage_checkpoint_failed` and retries
without discarding in-memory totals. Final-flush failure logs
`usage_final_checkpoint_failed`. Absolute checkpoints prevent double counting
when retrying after an ambiguous database commit result. Loading invalid or
unreadable saved statistics stops startup rather than silently resetting them.

This feature upgrades the database from schema 7 to **schema 8**. Older binaries
refuse the newer schema. Before upgrading, stop the daemon and retain a backup
of the database and any adjacent WAL file if rollback is needed; see
[upgrading an existing cache](configuration.md#upgrading-an-existing-cache).
Statistics survive garbage collection, suite forgetting and package eviction.
Replacing the database starts a new measurement period. There is no reset API.

The data uses the existing admin listener and `admin.htpasswd_file` policy;
there is no new endpoint on proxy ports. Without an htpasswd file, access is as
open as the rest of the admin status page. Keep that listener restricted to
trusted operators and use HTTPS when authenticating over a network, as described
in [admin configuration](configuration.md#admin-status-metrics-and-maintenance).
Status responses include `Cache-Control: no-store`.

## JSON fields

`usage.lifetime` and `usage.session` each contain:

- `since_unixtime`: the beginning of that measurement period.
- `fetched_bytes`, `served_bytes`, `cache_hit_bytes`: integer byte totals.
- `requests`, `cache_hit_requests`: integer request totals.
- `fetch_nanoseconds`, `serve_nanoseconds`: summed durations of body-bearing
  operations used by the rate calculations.
- `net_saved_bytes`: signed `served_bytes - fetched_bytes`.
- `average_download_bytes_per_second`, `average_upload_bytes_per_second`:
  floating point rates, or `null` without samples.
- `cache_hit_percent`: `100 * cache_hit_requests / requests`, or `null` without
  requests.

`usage.last_checkpoint_unixtime` is the latest successful checkpoint in the
current process; `usage.persistence_error` reports whether the last attempted
checkpoint failed. Existing status fields retain their names and types.
Consumers needing exact large integers should use an integer-preserving JSON
parser; JavaScript's `Number` cannot represent every integer above 2^53.
The usage block is present in the daemon even before traffic arrives. Embedders
that construct the admin server without a collector omit the optional block.
Existing Prometheus metrics retain their original semantics; these persistent
totals are exposed through the status JSON rather than changing those counters.
