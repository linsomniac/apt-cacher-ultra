# GC and upstream hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close five open items for 1.1.0 — two GC defects that predate 1.1.0 and three upstream-trust gaps — so that the cache never holds the database writer for seconds on a GC batch while it serves, never lets one failing GC pass starve the others, adopts none of the measured signed-anchor shapes that some supported apt rejects, never downloads an adoption member past its declared size, and never serves a strict host's `.deb` unchecked because its vouching snapshot went away during the download. In the maintainer's words: "a reliable, robust, and low maintenance system".

**Architecture:** Small, independent changes in five packages (`internal/gc`, `internal/cache`, `internal/fetch` with its adoption caller in `internal/freshness`, `internal/handler`, `internal/gpg`), each proven by tests written first. The verifier work rests on a committed corpus of signature shapes (`internal/gpg/testdata/aptshapes/`): byte-exact fixtures made by one committed generator, with the verdicts the real apt of six client images gave on those very bytes. A unit test holds the cache to that table; a manual oracle (`make e2e-apt-oracle`) re-measures apt. No schema change, no new configuration key.

**Tech Stack:** Go 1.25 (language), Go 1.27 (CI and release builds; 1.26 locally), modernc SQLite, `github.com/ProtonMail/go-crypto` v1.4.1, Python 3 (stdlib only; ruff, mypy) and gpg 2.4 for the fixture generator, Bash 5 and shellcheck 0.11, Docker, apt 2.2–3.2 in `debian:11`, `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`.

**Spec:** `docs/open-decisions.md`, section *Open*, these five entries — they are the requirement, each with its recommendation: **"GC's sub-job B deletes displaced snapshots in one transaction"**, **"A GC pass that fails at every tick starves the passes after it"**, **"The inline verifier accepts three shapes apt rejects"**, **"No limit on an upstream body's size"**, **"A vouched `.deb` whose snapshot is retired during its download"**. Background: the 2026-09-08 security review, `/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md` (main checkout, untracked), Theme 1 (C-4, VC-1, VC-2) and Theme 3 (A-2, VA-3, VA-4). The maintainer asked for these five to be worked now (2026-10-28), into 1.1.0, collected in `Changelog.md`. This revision applies the two plan reviews of 2026-10-28 (`PE/security-report.md`, `PE/gc-exec-report.md`; `PE` below), their scoped re-reviews (`PE/security-rereview.md`, `PE/gc-exec-rereview.md`) and the rulings on them (HD-1 to HD-10).

`PE` = `.superpowers/sdd/2026-10-28-gc-and-upstream-hardening/planning-evidence/` in the worktree (git-ignored; it survives reboots, the session scratchpad does not): the planning generators (`gen.py`, the security review's `gen2.py`–`gen6.py`, `pgp.py` and `rereview/gen7.py`), `in-container.sh`, the verdict tables (`table.txt`, `combined.txt`, `combined-fixed.txt`, `rereview/table-fx7.txt`, `rereview/combined-v2.txt` and `combined-v2nos1.txt`), the reviewer's prototypes of the verifier rules (`proto/fixed.go` with `go.mod`/`go.sum`, today's verifier copied in `proto/main.go`; `rereview/proto2/planv2.go`, the rules exactly as this plan words them, with `rereview/mutants.sh`), the vendor survey (`vendor/`, `vendor-keys/`, `vendors.txt`, `vendor-keys.txt`, `fetch-vendors.sh`, `fetch-keys.sh`, `survey.py`, `survey.txt`, `vendor-verdicts.txt`, `vendor-verdicts-fixed.txt`, `rereview/vendor-verdicts-v2.txt`) and the four review reports.

## Global Constraints

- **Nothing goes to GitHub** (push, pull request, tag, release, comment). Local commits on `fix/gc-and-upstream-hardening` only. Do not tag 1.1.0-rc1.
- **No schema change and no configuration key.** Never edit an entry of `migrations` in `internal/cache/schema.go`.
- `go test -race ./...`, `go vet ./...` and `golangci-lint run ./...` are clean at the end of every task that touches Go; `gofmt -l .` lists only the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`). Never run `gofmt -w` on those five; format only the files you change.
- Python files are typed (every function annotated), formatted with `ruff format`, and pass `ruff check` and `mypy --strict`; stdlib only, run as `python3 -I <script>` (no packaging).
- **Every change is proven:** a test written first and seen failing, then passing, and a mutant (the change reverted or inverted in one place) that fails it. The task report names each mutant and the test that caught it.
- **Every doc claim is checked against the code** by a grep with its expected output or a named test. A claim whose check fails is corrected to what the code does.
- `AIDEV-NOTE`s at every subtle point; existing `AIDEV-*` comments are kept and updated, never removed.
- Each user-visible change adds one entry to `Changelog.md` under `## Unreleased: 1.1.0` (*Added*, *Changed* or *Fixed*), in the file's style: one sentence, operator's view, wrapped at the file's width (about 76 columns).
- `git diff` goes through difftastic in this repository: pass `--no-ext-diff` to every diff a step reads.
- Scratch files go under `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/hardening/` (one variable at the top of a step that uses it).
- No private key is ever committed: the generator makes its keys in a temporary GnuPG home and discards them; only public keys and signed fixtures are committed.
- `git add` names files; never `-A` or `commit -a`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never amend or rewrite a commit.

Conventions: every command runs from the repository root. Expected outputs quote planning runs (2026-10-28, a 12-core machine, Go 1.26.8, Docker 29). A test name in a step is the name to use.

## Review Focus

The inputs the spec implies that are most likely to bite a person running this, most likely first, each pinned by a test in the task that owns the code:

1. **A legitimate signed repository whose anchor has an unusual but valid shape** — CRLF line ends, spaces or tabs after a marker, no final newline, a `Comment:` or `Version:` armor header, a blank line in the base64, no CRC line, an extra signature from a key the cache does not know, a subkey signature, a SHA-1 extra signature beside a good one, a `Hash:` header naming SHA-1 or SHA-512 or none — must still adopt. Every such shape is a control row of the corpus (Task 5), accepted by all six apt versions; Task 6 and 7's table test holds the cache to it, and Task 8's gate re-runs the 81 real vendors' anchors the security review collected (68 `InRelease` that verify today must still verify).
2. **A repository signed by two keys where one is expired** (a vendor rotating keys): a good signature from the expired key beside a good one must still adopt; only a signature that fails its cryptographic check rejects. Task 7.
3. **An upgraded cache with a backlog of displaced snapshots, and a daemon that never runs an hour** (a CI sidecar on a persistent cache): the startup pass still deletes them (ruling HD-7), and periodic ticks retire them and delete them in chunks within `gc.max_tick_duration`; no batch selects a snapshot it retired. Task 2.
4. **A member declared empty, a chunked member response, a resumed download, and a by-hash URL that serves a newer, larger file:** each download stops at the declared size, and the by-hash probe still falls back to the plain path. Task 3.
5. **A non-strict host whose `.deb` loses its vouching during the download** is still served as before; only a strict host refuses it. Task 4.

---

## Design notes and rulings

**DN-1 — Scope.** The five *Open* entries named in **Spec**, plus what the measurement of DN-2 shows belongs to the same defect: the detached form's framing (`Release.gpg` with lines around its armor or a binary signature) and the packet-level shapes go-crypto skips (ruling HD-1). Not in scope, each staying in `docs/open-decisions.md` (Task 8): the security review's C-1 (dates, rollback) and C-3 (weak digests and keys: `x3i_sha1_only`, an `InRelease` whose only signature uses SHA-1, is rejected by every apt and adopted by the cache, today and after this plan; only the signer can make it); downloads with no declared size — the request path's misses, hot prefetch, and the handler's snapshot-member recovery `runSnapshotMemberFetch` (`cache.SnapshotMember` and `package_hash` have no size; adding one is a schema change; repair and reconcile go through `adoptMember` and get the cap); and `adoption.accept_any_signer`'s bypass (DN-8).

**DN-2 — What apt does (measured, 2026-10-28).** 135 signature shapes, most built without a key from a legitimately signed `InRelease` or `Release.gpg` (the critical-notation, SHA-1 and dash-escaped-END shapes need the signer) (key A trusted; key E in the client's `signed-by`, expired in 2021; key S, a primary that only certifies with a signing subkey; key B unknown to the client), as a flat `file:` repository, through `apt-get update` in each client image: `debian:11` apt 2.2.4 (gpgv 2.2.27), `ubuntu:22.04` apt 2.4.14 (gpgv 2.2.27), `ubuntu:24.04` apt 2.8.3 (gpgv 2.4.4), `debian:12` apt 2.6.1 (gpgv 2.2.40), `debian:13` apt 3.0.3 (Sequoia `sqv`, no gpgv), `ubuntu:26.04` apt 3.2.0 (gpgv 2.4.8). The controller measured 47 (`PE/gen.py`, `PE/table.txt`), the security review 81 more (`PE/gen2.py`–`gen6.py`) and its re-review 7 more (`PE/rereview/gen7.py`, `table-fx7.txt`); **Appendix A** holds all 135 verdicts and the verdict the rules below give. The classes:

- **Every apt rejects:** any line after the signature's END line (empty, spaces, text, a lone CR line); text after the END marker on the END line itself (`x7*_end_trailing_*`); a blank line or spaces before BEGIN (inline and detached); a binary `Release.gpg`; an armor header without `: ` (`Junk:junk`, `Comment:<TAB>x`, no colon); a `-----END X-----` or `-----END PGP SIGNATURE-----x` line before the real END; a second `-----BEGIN` line inside the armor; a v3 garbage packet naming a known key; a UTF-8 BOM before the armor; the only signature bad, or critical-notation only.
- **Every gpgv-based apt rejects, `debian:13`'s sqv accepts:** a forged CRC line; a bad signature from a known key beside a good one (whatever the order, inline or detached, from the expired key too, from a subkey too, with its unhashed issuer rewritten or an unhashed issuer fingerprint added, in a second inline block); a critical notation beside a good signature; the expired key's signature alone.
- **Only some apt rejects:** a base64 line, or a second `=` line, between the CRC line and END (d13); an empty armor header value `Comment:` (d13); an unknown hash or an unknown key's v3 packet, a marker, an unknown packet tag, a user-ID packet or trailing zero bytes beside a good signature (d13 or u26; u26's marker refusal is a gpgv 2.4.8 crash, "exited unexpectedly", stable over five reruns, not a policy); a literal-data packet (all but u26); an invalid base64 character (d13); `xi_goodsig_issuer_rewritten` (d11, u22, d12).
- **Every apt accepts:** CRLF throughout; spaces, tabs or extra CRs after a marker (`x7i_sigbegin_double_cr` included, a deviation); no final newline; a CR before EOF; `Comment:`, `Version:`, several or unknown armor headers, a header with a leading space, a space in its key, UTF-8 or control bytes; a blank line before or after the CRC line; no CRC line; a CRC line with spaces around it; base64 lines of 76 characters or with a leading space; an extra signature from an unknown key; the expired key's good signature beside a good one; a subkey signature; a SHA-1 extra signature beside a good one; a `Hash:` header naming SHA-1 or SHA-512, or none; dash-escaped extra plaintext lines; two good armor blocks, or a good and an unknown-key block (`d_twoblocks`, the controller's row, is rejected for its second block's BADSIG, not its framing).

`i_dash_escaped_end` (a Release line `-----END PGP SIGNATURE-----`, dash-escaped) is rejected by apt's Release parser, not its signature check; it stays in the table, because the cache's `ParseRelease` ignores the stray line and would adopt an anchor every client then fails to parse (rule F0 keeps refusing it, as today's code does).

**DN-3 — The rule (ruling HD-2).** The verifier rejects a shape when **any** supported apt rejects it, and accepts a shape **every** supported apt accepts, except for the exceptions of DN-6, each with its reason: eleven strict deviations (refused, every apt accepts) and one known-unsafe shape (accepted, every apt refuses: `x3i_sha1_only`, C-3, out of scope). Why "any": a cache that accepts what some client rejects breaks that client's `apt update` for the suite until upstream publishes again (forever for a frozen pocket); a cache that rejects what every client accepts only keeps its previous snapshot, a staleness no surveyed vendor meets (Task 8's vendor re-run). The rules, applied in this order, each failure fatal (ruling HD-8, the security review's recommended set, prototyped in `PE/proto/fixed.go`):

| Rule | What it requires | Forms |
|---|---|---|
| F1 | The first line is the BEGIN marker (`-----BEGIN PGP SIGNED MESSAGE-----` inline, `-----BEGIN PGP SIGNATURE-----` detached), followed only by spaces, tabs and CRs. Detached: nothing before it — a binary signature fails F1. | both (inline: unchanged, ruling R-D2) |
| F0 | The first occurrence of `-----END PGP SIGNATURE-----` anywhere in the body begins the last line (inline: today's substring rule, kept; detached: new). It catches `-----END PGP SIGNATURE-----x` and a dash-escaped END in the signed text, which no line-based rule sees. | both |
| F2 | The signature armor's first END line (`-----END PGP SIGNATURE-----` followed only by spaces, tabs and CRs — never other text: `x7*_end_trailing_*`) is the last line: after it nothing, or one LF. (F0 already implies the last-line clause; F2's own work is the END line's form.) | both |
| F3 | A CRC line (`=` and four base64 characters, after trimming spaces, tabs and CRs) inside the armor, if present, equals the CRC-24 of the signature packets the cache decoded, and only blank lines follow it before END. | both |
| F4 | No line strictly between the armor's BEGIN line and its END line starts with `-`. | both |
| F5 | Every non-blank line between the armor's BEGIN line and the first blank line (the armor headers) contains `: `. | both |
| F6 | The decoded signature bytes are signature packets only, each parsed by go-crypto: a packet-framing walk (old and new formats; a partial or indeterminate length refused) finds tag 2 only, and `packet.Reader.NextWithUnsupported()` returns one `*packet.Signature` per framed packet, never an `*packet.UnsupportedPacket`. | both |
| S1 | Every signature packet whose **hashed** issuer (DN-5) names a signing key in the host keyring passes its cryptographic check against one of the keyring's entities that hold that key ID, with no `SignatureError`; and its unhashed issuer does not contradict the hashed one. A packet whose issuer the keyring lacks is ignored. | both |
| S2 | At least one packet passes the trust check and verifies (unchanged). | both |

F0–F6 are structural: they also apply under `accept_any_signer`'s bypass. S1 does not (DN-8). Result over Appendix A (the re-review ran the rules exactly as worded here, `PE/rereview/proto2/planv2.go`: 0 rows differ): 1 shape unsafe (`x3i_sha1_only`, C-3, out of scope) and 11 stricter than every apt (DN-6); over the review's real vendors, every anchor that verifies today still verifies (68 `InRelease`, 67 `Release.gpg`), and the only newly refused are the binary `Release.gpg` of nginx, steam and syncthing, which every measured apt refuses too.

**DN-4 — Errors and reasons.** Inline F0–F6 failures wrap `ErrMalformedClearsign` (reason `malformed_clearsign`, as today). Detached F0–F6 failures wrap a new `ErrMalformedSignature` = `errors.New("Release.gpg is not one ASCII-armored signature block apt accepts")` (reason `malformed_signature`), as `fmt.Errorf("%w: %v", ErrMalformedSignature, detail)` with the rule's detail, as the inline path does. S1 failures wrap a new `ErrBadSignature` = `errors.New("a signature from a known key failed verification")` (reason `bad_signature`). `ClassifyVerifyErr` gains both cases before its `crypto_verify_failed` default. Where the reasons surface: `classifyAdoptionReason` (`internal/freshness/metrics.go`) carries them into `recent_adoptions[].reason` and `adoption_fail_reason` (the adoption counter's outcome stays `gpg_failed`); the operator-facing text is `reasonTooltip` (`internal/admin/status.go`, one case per reason, pinned by `TestReasonTooltip`) and the `Reason` field comment beside it. No document lists the gpg reasons; none is added (`docs/admin-ui-spec.md`'s example near line 370 stays an example).

**DN-5 — S1's "known", "fails" and the issuer (rulings HD-3, HD-8).** go-crypto takes a signature's issuer from the last issuer subpacket it parses, unhashed area after hashed (`packet/signature.go`), and looks keys up by that key ID only (`openpgp/read.go:523`); gpgv looks up by the hashed issuer. So S1 reads the issuer from the **hashed** area, `sig.HashSuffix` (the hashed subpacket area verbatim: bytes `[6:6+len]` for v4, `[8:8+len]` for v6): the issuer fingerprint (subpacket 33; key ID = bytes 12–19 of a v4 fingerprint, 0–7 of a v6) first, else the issuer key ID (16); when the hashed area names neither, `*sig.IssuerKeyId`. Known = `v.keyring.EntityList().KeysByIdUsage(id, packet.KeyFlagSign)` is non-empty. A known packet fails when `sig.IssuerKeyId` is nil or differs from the hashed id (an unhashed issuer contradicting the hashed one: gpgv's BADSIG), or when no entity holding the key ID passes: for each such entity `e`, `openpgp.VerifyDetachedSignature(openpgp.EntityList{e}, signed, onePacket, nil)` passes iff it returns a non-nil signature and its error is not an `errors.SignatureError` (an unknown critical notation, `checkMessageSignatureDetails`; `ErrKeyExpired`, `ErrSignatureExpired` and `ErrKeyRevoked` are tolerated, as apt tolerates EXPKEYSIG and REVKEYSIG beside a good signature). One entity at a time, because `verifyDetachedSignature` hashes once and tries every candidate key with that one hash, which only the first can pass (a key-ID collision would turn a valid signature into a failure). S2 (`verifyAnyTrusted`) is unchanged and keeps that quirk: a host key colliding with the signer's key ID and loaded ahead of it still refuses the anchor (`crypto_verify_failed`), as today (ruling HD-10: recorded *Open*, not changed here), so S1's per-entity check is tested on `requireKnownSignaturesVerify` directly. The host keyring, not the per-suite trust set: a client that installed any of the cache's keys sees a BADSIG from it. S1 protects clients exactly for the keys the cache also holds: a client trusting a key the cache lacks still rejects a bad signature from it, which the cache ignores (inherent; the operator installs in the cache every key the clients' `signed-by` names).

**DN-6 — The exceptions (ruling HD-9).** Each shape in the first table is accepted by all six apt and rejected by the cache; `aptShapeDeviations` lists each with its reason. The one shape in the second is rejected by all six and accepted by the cache; `aptShapeKnownUnsafe` holds exactly it. The table test fails on any other disagreement.

| Strict deviations | Reason |
|---|---|
| `xi_twoblocks_goodgood`, `xi_twoblocks_good_unknown`, `xd_twoblocks_goodgood`, `xd_twoblocks_good_unknown` | one armor block: the decoders read only the first, so S1 and S2 cannot check a second; no surveyed vendor publishes two (2026-10-28) |
| `i_sigbegin_trailing_ws`, `x7i_sigbegin_double_cr`, `xi_cleartext_comment_hdr` | go-crypto's decoder refuses the shape (`clearsign.Decode` strips one CR and then compares the signature BEGIN line exactly; rejected today too) |
| `x3i_armor_no_blank`, `x3d_armor_no_blank` | go-crypto's decoder refuses the shape, and so does F5 (with no blank line, every armor line counts as a header without `: `): a go-crypto upgrade accepting it would leave it refused |
| `xi_good_plus_unknownpk_A` | go-crypto cannot parse an unknown public-key algorithm, so the packet cannot be told from garbage (F6); a vendor adding a signature with an algorithm go-crypto v1.4.1 lacks is refused until go-crypto supports it |
| `x3i_goodsig_unhashed_fpr_added` | today's S2 refuses it already (the unhashed fingerprint overrides the issuer go-crypto looks up) |

| Known unsafe | Reason |
|---|---|
| `x3i_sha1_only` | C-3, weak digests: out of scope, recorded *Open* (Task 8 (f)); only the signer can make it |

**DN-7 — Sub-job B retires in the periodic ticks and deletes at startup (rulings HD-4, HD-7).** `RunSnapshotGCBatch` gains `retireDisplaced bool` (and `gcStore` with it). It keeps its selection (sub-jobs A ∪ B, `gc.snapshot_batch_size` at a time). With `retireDisplaced` (periodic ticks), the displaced snapshots (class `displaced`) get `retired_at` in one short `UPDATE` and sub-job C deletes their rows, 10,000 rows a transaction, in the same snapshot pass (A ∪ B, then C) or the retired run an hour after a start. Without it (the startup pass), they are deleted as today: the startup pass runs before the freshness scheduler and before any listener (`cmd/apt-cacher-ultra/main.go`, `gcsvc.StartupPass`), so its long transactions hold up no other writer and no request in flight (the listeners open after it: a large backlog delays the start, up to `gc.max_tick_duration`, as today; the periodic ticks keep that backlog small, only the displacements since the last tick), and a daemon that never lives min(1 h, `gc.interval`) still deletes them at every start, with their blobs in the same startup pass's blob pass (G-I1: with retirement at startup, such a daemon would delete none). One residual: when a periodic tick's sub-job C is cut by its deadline, the snapshots it retired are no longer B-selectable at the next start; they wait for the next periodic tick or the retired run an hour after a start, so a daemon that never lives that long keeps them (the H3 final review's R3 #2 residual, for reaped suites, now also for those). Orphan candidates (class `orphan`) are deleted at once in both modes: only `CommitAdoption` inserts child rows, in the transaction that sets `adopted_at`, so an orphan holds a header only. Why retiring is safe: every reader that serves, vouches or judges history filters `retired_at` or reads current snapshots only (signed history `signedHistorySnapshotSQL`, retained by-hash `snapshot_reads.go:84`, the natural-key index and lookup behind `rollback_refused`, `declaredExactSQL`/`declaredBySuffixSQL`, coverage and the url_path guards); GC's blob reachability, `adoptionStampTx`'s stamp floor, `GetSuiteStats`' `AdoptedTotal` (the `acu_snapshots_displaced` gauge) and the id-keyed reads see retired rows until C deletes them, all benign; and B's selection has `retired_at IS NULL`, so a batch never selects what it retired. Telemetry: in a periodic tick, a displaced snapshot beyond `keep_displaced` counts once in `displaced_reaped` / `acu_gc_displaced_reaped_total` when retired and again in `retired_reaped` / `acu_gc_retired_reaped_total` when deleted; snapshots deleted = orphan + retired (startup: orphan + displaced, as today).

**DN-8 — Not changed, deliberately.** `adoption.accept_any_signer`'s bypass verifies no signature, so S1 is not applied there (it would be the only signature check in that mode; recorded *Open*). The handler's sniff of a fetched `Release.gpg` (`aptpath.SniffReleaseGPG`) still accepts a binary signature as a signed anchor: its marker only makes the suite never adopt unsigned, the fail-safe direction (the four surveyed vendors with a binary `Release.gpg` all publish an `InRelease`, which the checker prefers). The upgrade's trust audit classifies 1.0.x snapshots with its own structural classifier (`classifyLegacyInline`), not the verifier, so the stricter verifier quarantines nothing new at the upgrade.

**DN-9 — The download cap (ruling HD-5).** (a) The upstream transport sets `DisableCompression: true`: Go then sends no `Accept-Encoding: gzip` and decodes nothing, so a response's `Content-Length` is what lands on disk (VA-3: a decoded gzip body had length −1 and skipped the size check; VA-4: a resumed download appended compressed bytes to a decoded prefix; it also fixes an Apache serving `.gz` files with `Content-Encoding: gzip`, which Go decoded into the wrong bytes). apt's http method sends no `Accept-Encoding` either; the HTTP/2 transport honours the setting. Its one cost: an uncompressed file an upstream would gzip on the fly when asked now downloads at full size. (b) `fetch.Target` gains `MaxBytes`: a response whose declared length exceeds it fails before its body is read, and a body that runs past it fails as soon as it does, with `ErrBodyTooLarge`, never retried, outcome `body_too_large` (the label `Conditional` already uses). Adoption sets it to the member's declared size for the by-hash probe and the plain-path fetch (`memberMaxBytes`: one byte for a member declared empty, since 0 means no limit). This bounds a signed member exactly and an unsigned member by what its `Release` declares.

**DN-10 — Strict mode, vouching lost during the download (ruling HD-6).** `prefetchCheck` reports `vouched` when its read, or `prefetchRecheck`'s second look, found the path vouched; when `runFetch`'s read after the download finds no row for a `.deb` that was vouched before it, strict mode decides there (`strictAfterDownload`), as it already does when the read before the download failed: refused on a strict host (or, with a signed adoption in flight on the host, the miss waits and retries once), served unchecked as today elsewhere.

**Rulings on the plan reviews (2026-10-28).** HD-7 (G-I1) is DN-7's startup mode. HD-8 (S-C1, S-C2, S-I2, S-M1, S-M3) is DN-3's F0, F4–F6 and DN-5's S1. HD-9 (S-I1, S-M2; with the re-review's S-I1 and S-M5) is DN-6. HD-10 (the security re-review's S-I2): S2 stays as it is; S1's per-entity check is tested directly, and S2's key-ID collision quirk is recorded *Open*. Every other finding of the four reports is accepted with the reviewer's text, folded into the task it names.

---

### Task 1: A GC pass that fails at every tick no longer starves the passes after it

**Files:**
- Modify: `internal/gc/gc.go` (`skipPanickedPass`, new `skipPass` and `passFailed`, `runTick`, `periodicTick`'s WARN condition, the `nextPass` field comment), `internal/gc/snapshot.go` (`retiredRun`'s WARN condition)
- Test: `internal/gc/gc_store_test.go`
- Modify: `Changelog.md`

**Interfaces:**
- Consumes: `fakeStore`, `storeGC`, `gcEvents`, `captureLogger`, `openTestCache` (existing `internal/gc` tests).
- Produces: `func (g *GC) skipPass()`; `func (g *GC) passFailed(ctx context.Context, err error) error`.

- [ ] **Step 1: Extend `fakeStore`** (add `"errors"` to the test file's imports):

```go
	batchErrs int    // RunSnapshotGCBatch fails on this many calls after the batchPanics ones
	batchHook func() // called at the start of every RunSnapshotGCBatch call
	chunkErrs bool   // RunRetiredSnapshotGCChunk fails
	chunkHook func() // called at the start of every RunRetiredSnapshotGCChunk call
```

In `RunSnapshotGCBatch`, after the panic check (which stays first, so `batchPanics` alone behaves as today): call `batchHook` if set, then `if n <= f.batchPanics+f.batchErrs { return cache.SnapshotGCBatchResult{}, errors.New("snapshot batch fault") }`. In `RunRetiredSnapshotGCChunk`, after its panic check: call `chunkHook` if set, then `if f.chunkErrs { return 0, false, errors.New("retired chunk fault") }`. (Task 2 changes `RunSnapshotGCBatch`'s signature; this step uses today's.)

- [ ] **Step 2: Write the failing tests.**

```go
// The H3 final review R3 #5: a pass that fails at every tick (a fault
// limited to its tables) moves the rotation on as a panicking one does, so
// the blob pass keeps reclaiming disk while the fault waits for its fix.
func TestPeriodicTick_AFailingPassDoesNotStarveTheNext(t *testing.T) {
	logger, buf := captureLogger()
	f := &fakeStore{batchErrs: 1000}
	g, _ := storeGC(t, f, 2, logger)
	ctx := context.Background()
	g.periodicTick(ctx) // the url-path pass (off: no TTL), then the snapshot pass fails
	if g.nextPass != blobPass {
		t.Fatalf("after a failed snapshot pass the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
	g.periodicTick(ctx) // the blob pass
	if g.nextPass != urlPathPass {
		t.Fatalf("after the blob pass the rotation is at pass %d, want the url-path pass (%d)", g.nextPass, urlPathPass)
	}
	g.periodicTick(ctx) // the snapshot pass fails again
	if n := len(gcEvents(t, buf, "gc_tick_failed")); n != 2 {
		t.Fatalf("%d gc_tick_failed lines, want 2 (the snapshot pass, twice)", n)
	}
}

// A pass stopped because the GC's context ended (shutdown) keeps the
// rotation where it is, and logs no gc_tick_failed: it did not fail.
func TestPeriodicTick_AnEndedContextKeepsTheRotation(t *testing.T) {
	logger, buf := captureLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeStore{batchErrs: 1000, batchHook: cancel}
	g, _ := storeGC(t, f, 2, logger)
	g.periodicTick(ctx)
	if g.nextPass != snapshotPass {
		t.Fatalf("after a pass stopped by its context the rotation is at pass %d, want the snapshot pass (%d)", g.nextPass, snapshotPass)
	}
	if n := len(gcEvents(t, buf, "gc_tick_failed")); n != 0 {
		t.Fatalf("%d gc_tick_failed lines, want 0", n)
	}
}

// The startup pass's runTick moves the rotation past a failed pass too, and
// still returns the failure (main logs it).
func TestStartupPass_AFailingPassMovesTheRotation(t *testing.T) {
	logger, _ := captureLogger()
	g, _ := storeGC(t, &fakeStore{batchErrs: 1}, 2, logger)
	if err := g.StartupPass(context.Background()); err == nil {
		t.Fatal("StartupPass returned nil, want the snapshot pass's failure")
	}
	if g.nextPass != blobPass {
		t.Fatalf("the rotation is at pass %d, want the blob pass (%d)", g.nextPass, blobPass)
	}
}
```

Also, in the style of `TestSkipPanickedPass_MovesPastEachPass`: `TestPassFailed_MovesPastEachPassUnlessTheContextEnded`, a table over the three passes × {live context, cancelled context}: `g.nextPass = p; g.passFailed(ctx, errors.New("x"))` moves to the next pass (url-path → snapshot → blob → url-path) with a live context and leaves it with a cancelled one, and returns the error. And two wiring tests on a real cache (`storeGC` with the real cache as its store, or `openTestCache` and `New`), each breaking one pass's table the way the handler tests do (`ALTER TABLE … RENAME TO …` through a second `sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))`): `TestPeriodicTick_AFailingURLPathPassMovesToTheSnapshotPass` (`URLPathTTL > 0`, `url_path` renamed: one tick leaves `nextPass == snapshotPass`) and `TestPeriodicTick_AFailingBlobPassMovesToTheURLPathPass` (`g.nextPass = blobPass`, `blob` renamed: one tick leaves `nextPass == urlPathPass`). If a rename does not make the pass fail (SQLite may keep a prepared statement), report it and use another fault confined to that pass's table. And `TestRetiredRun_LogsAFailureOnlyWhileTheContextLives`: `retiredRun` with `fakeStore{chunkErrs: true}` and a live context logs one `gc_tick_failed`; with `chunkHook: cancel` (the context ends during the chunk), none.

- [ ] **Step 3: Run them; expect the failures** (`go test ./internal/gc -run 'AFailing|AnEndedContext|PassFailed|RetiredRun_Logs' -v`): the rotation stays at the failed pass (and `passFailed` does not exist yet).

- [ ] **Step 4: Implement.** In `internal/gc/gc.go`:

```go
// skipPass moves the rotation past the pass in progress (g.nextPass).
func (g *GC) skipPass() {
	switch g.nextPass {
	case urlPathPass:
		g.nextPass = snapshotPass
	case snapshotPass:
		g.nextPass = blobPass
	default:
		g.nextPass = urlPathPass
	}
}

// passFailed is runTick's return for a pass that failed: it moves the
// rotation past that pass, as skipPanickedPass does for a panic, unless the
// GC's context has ended (a shutdown stopped the pass; it did not fail).
//
// AIDEV-NOTE: the H3 final review R3 #5. Before, only a panic moved the
// rotation: a pass that returned an error at every tick (a fault limited to
// its tables) was retried first at every tick, and the passes after it —
// the blob pass, which reclaims disk — never ran; the only sign was WARN
// gc_tick_failed per tick. A failing url-path or snapshot pass now runs
// every other tick, the passes after it in between (a failing blob pass, the
// last, still runs every tick, after the others); a failed url-path pass
// resumes, next time, at the cursor its last deadline saved.
func (g *GC) passFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		g.skipPass()
	}
	return err
}
```

`skipPanickedPass` keeps its early return and then calls `g.skipPass()`. In `runTick`, each of the three `if err != nil { return res, err }` after a pass becomes `return res, g.passFailed(ctx, err)`. `periodicTick` and `retiredRun` log `gc_tick_failed` when `err != nil && ctx.Err() == nil` (the same test as `passFailed`; today they test the error's shape). Update the `nextPass` field's comment: "A pass that panicked or failed moves it too, past that pass (skipPanickedPass, passFailed)".

- [ ] **Step 5: Run the package's tests** (`go test -race ./internal/gc`); all pass, including `TestPeriodicTick_APanickingPassDoesNotStarveTheNext` and `TestSkipPanickedPass_MovesPastEachPass`.

- [ ] **Step 6: Mutants.** (a) `passFailed` never calls `skipPass` → the first, third and table tests fail. (b) `passFailed` ignores `ctx` → the second and the table's cancelled rows fail. (c) No `passFailed` at the url-path return → `…URLPathPassMovesToTheSnapshotPass` fails. (d) None at the blob return → `…BlobPassMovesToTheURLPathPass` fails. (e) `periodicTick` tests the error's shape again → `…AnEndedContextKeepsTheRotation`'s `gc_tick_failed` count fails. (f) `retiredRun` tests the error's shape again → `TestRetiredRun_LogsAFailureOnlyWhileTheContextLives`' cancelled case fails. Restore each.

- [ ] **Step 7: Changelog** — under *Fixed*:

```
- A GC pass that fails at every tick no longer keeps the passes after it,
  the blob pass included, from running.
```

- [ ] **Step 8: Commit** `internal/gc/gc.go internal/gc/snapshot.go internal/gc/gc_store_test.go Changelog.md`: "GC: a pass that fails moves the rotation on, as a panic does (H3 final review R3 #5)".

### Task 2: Periodic GC retires displaced snapshots and deletes them in chunks

**Files:**
- Modify: `internal/cache/gc.go` (`RunSnapshotGCBatch` and its doc, `SnapshotGCBatchResult`'s doc, `RunRetiredSnapshotGCChunk`'s AIDEV-NOTE), `internal/cache/queries.go` (an AIDEV-NOTE at `GetSuiteStats`), `internal/gc/snapshot.go` (`runSnapshotPass`, its doc and AIDEV-NOTE, `retiredRun`'s AIDEV-NOTE), `internal/gc/gc.go` (`gcStore`), `internal/gc/metrics.go` (the help of `acu_gc_displaced_reaped_total` and `acu_gc_retired_reaped_total`)
- Test: `internal/cache/gc_test.go`, `internal/gc/retired_test.go`, `internal/gc/gc_store_test.go` (`fakeStore`'s signature), and the existing tests listed in Step 5
- Modify: `docs/configuration.md`, `docs/admin-ui-spec.md`, `docs/log-fields.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (spec C9's definition of `retired_at`), `Changelog.md`

**Interfaces:**
- Produces: `RunSnapshotGCBatch(ctx context.Context, batchSize int, staleGraceSeconds int64, keepDisplaced int, retireDisplaced bool) (SnapshotGCBatchResult, error)`; `gcStore` with the same method; `DisplacedReaped` counts displaced snapshots retired (`retireDisplaced`) or deleted (not).

- [ ] **Step 1: Pre-flight.** Confirm DN-7's facts, each with its expected output:
  - `grep -n "INSERT INTO snapshot_member\|INSERT INTO snapshot_skipped_member\|INSERT INTO package_hash" internal/cache/*.go | grep -v _test` lists `adoption.go` (inside `CommitAdoption`), `reconcile.go` and `skipped_member.go` only; reconcile and repair act on a current snapshot only (`reconcile.go` ~150–159, `skipped_member.go` ~104–113).
  - `grep -n "retired_at IS NULL" internal/cache/*.go | grep -v _test` includes `signed_seen.go` (`signedHistorySnapshotSQL`), `snapshot_reads.go`, `adoption.go` (the natural-key lookup), `gc.go` (B's selection), `suite_reap.go`, `legacy_audit.go`, `schema.go` (the partial index).
  - `grep -n "StartupPass\|scheduler" cmd/apt-cacher-ultra/main.go`: `gcsvc.StartupPass(ctx)` comes before the scheduler's start and before `serveListeners` serves.
  If any fails, stop and report.

- [ ] **Step 2: Write the failing tests** in `internal/cache/gc_test.go`. Seed with the existing helpers: `adoptCandidate` (`gc_test.go`, headers only), `adoptWith` (`suite_reap_test.go`), `seedPackageHash` (`gc_urlpath_test.go`, a drift file: call it, never edit it), `childRows` (`snapshot_gc_retired_test.go`), `sk`/`seedRow` (`suite_evict_test.go`) and the raw `snapshot_skipped_member` INSERT of `snapshot_gc_retired_test.go`; the tests stub `nowUnix` with `stubNow`.
  - `TestRunSnapshotGCBatch_RetiresTheDisplacedBeyondKeep`: one current and five displaced snapshots, each with `snapshot_member`, `snapshot_skipped_member` and `package_hash` rows; `keepDisplaced = 3`, batch size 10, `retireDisplaced = true`. The batch returns `DisplacedReaped == 2`; the two oldest displaced have `retired_at` equal to the stubbed `nowUnix()` and every child row still there (counted per table); the others are untouched. A second batch returns `Total() == 0`. Then `RunRetiredSnapshotGCChunk(ctx, 0)` until `more == false` deletes the two retired snapshots' rows and headers and nothing else.
  - `TestRunSnapshotGCBatch_DeletesTheDisplacedWhenNotRetiring`: the same seed with `retireDisplaced = false` deletes the two oldest displaced snapshots' rows and headers in the batch (today's behaviour, the startup mode).
  - `TestRunSnapshotGCBatch_ARetiredDisplacedSnapshotIsNoLongerRead`, three legs, each asserting the same answer after a retirement as after a deletion: (signed history) a suite whose only signed history is displaced snapshots — no current pointer, `keep_displaced = 0`, no marker — is refused by `CheckUnsignedDowngrade` before the batch and admitted after (seed it with `adoptCandidate`, raw headers and no marker, and `setCurrentSnapshot(…, 0)`, or call `ClearSignedSeenForTest` (`internal/cache/testing_helpers.go`) after `adoptWith`, which commits through `CommitAdoption` and so writes the signed-seen marker that keeps the suite refused); (retained by-hash) `DisplacedSnapshotMembers` returns the kept snapshots' ids and not the retired ones'; (rollback) `InsertCandidateSnapshot` with a retired snapshot's natural key inserts a new candidate, and with a kept one's returns `*NaturalKeyAdoptedError`. Its mutant: drop `retired_at IS NULL` from `snapshot_reads.go:84` and the by-hash leg fails.
  - Orphans are already pinned by `TestRunSnapshotGCBatch_OrphanReap_HeartbeatStale`; run it in both modes (add the parameter).
  In `internal/gc/retired_test.go`: extend `TestRun_DeletesRetiredSnapshotsAnHourAfterTheStart` with a displaced snapshot beyond `keep_displaced` (the startup pass deletes it: the startup mode), and add `TestPeriodicTick_RetiresAndDeletesTheDisplacedSnapshots` (one periodic tick retires and deletes them; its `gc_run_complete` shows both `displaced_reaped` and `retired_reaped` counting them).

- [ ] **Step 3: Run them; the retiring ones fail** (no parameter; B deletes at once).

- [ ] **Step 4: Implement.** In `RunSnapshotGCBatch`, after the SELECT, split the ids by class. Orphans: the existing cascade delete, with placeholders for the orphan ids only. Displaced: with `retireDisplaced` false, the same cascade delete (today's behaviour); with it true:

```go
		if retireDisplaced && len(displaced) > 0 {
			// AIDEV-NOTE: rulings HD-4/HD-7 (the H3 final review R3 #4). A
			// periodic tick's B used to delete a displaced snapshot's rows here,
			// in this one writer transaction: six snapshots of 70,000
			// package_hash rows held the writer 4.7 s in a probe, so a full
			// batch held every write (a miss's store, a flip's commit) about
			// 8 s. It now only retires them, and sub-job C deletes them in
			// chunks (retiredChunkRows a transaction), in the same snapshot
			// pass. Every reader that serves, vouches or judges history filters
			// retired_at or reads current snapshots only, so a retired displaced
			// snapshot is invisible to them exactly as a deleted one was, and
			// B's selection (retired_at IS NULL) never selects it again. The
			// startup pass still deletes (retireDisplaced false): it runs before
			// the scheduler and the listeners, so its long transactions hold up
			// no other writer and no request in flight (a large backlog delays
			// the start, as before; the periodic ticks keep it small), and a
			// daemon that never lives until sub-job C runs still deletes its
			// displaced snapshots at every start.
			res, err := tx.ExecContext(ctx,
				`UPDATE suite_snapshot SET retired_at = ? WHERE retired_at IS NULL AND snapshot_id IN (`+placeholders(len(displaced))+`)`,
				append([]any{now}, displaced...)...)
			if err != nil {
				return fmt.Errorf("RunSnapshotGCBatch: retire displaced: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("RunSnapshotGCBatch: retire displaced: %w", err)
			}
			if n != int64(len(displaced)) {
				return fmt.Errorf("RunSnapshotGCBatch: retired %d displaced snapshots, want %d", n, len(displaced))
			}
		}
```

(`placeholders(n)` builds `?,?,…`; reuse the loop that builds `idList` today or add a helper beside it.) `runSnapshotPass` passes `phase != phaseStartup`. Comments: `RunSnapshotGCBatch`'s doc (steps 3–4 by mode; the closing "blobs … reaped by the blob pass … later in the same tick" holds for the startup mode, and for periodic ticks after C), `SnapshotGCBatchResult`'s ("rows reaped" → reaped or retired), `runSnapshotPass`'s and its AIDEV-NOTE, `retiredRun`'s AIDEV-NOTE (C now also deletes B's retirements), and `RunRetiredSnapshotGCChunk`'s AIDEV-NOTE: its "every retirer holds the suite's mutex" justification does not cover B, which holds none; state B's own reason — B retires only adopted, non-current snapshots, `CommitAdoption` commits only an unadopted candidate and is the only writer of `current_snapshot_id`, so a retired displaced snapshot never becomes current again. Add at `GetSuiteStats` (`queries.go`): "AIDEV-NOTE: `AdoptedTotal` includes retired snapshots (sub-job B's too, until sub-job C deletes them), so `acu_snapshots_displaced` counts them meanwhile."

- [ ] **Step 5: Fix the existing tests that pinned B's deletion**, each asserting the retirement (the child rows kept) and then running `RunRetiredSnapshotGCChunk(ctx, 0)` until `!more` before asserting the deletion — or passing `retireDisplaced = false` where the test is about the startup mode; never weaken an assertion about what clients are served:
  - `internal/cache/gc_test.go`: `TestRunSnapshotGCBatch_FiveAdopted_KeepDisplaced3_ReapsOldest`, `…_KeepDisplaced0_ReapsAllDisplaced`, `…_NullCurrent_AllAdoptedReaped`, `…_TieBreakOnSnapshotIDDesc`;
  - `internal/cache/f24_gc_test.go`: `TestRunSnapshotGCBatch_KeepsTheAuditsVerdict/keep`, `TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot`;
  - `internal/cache/skipped_member_test.go`: `TestSnapshotGC_ReapsSkippedMemberRows`;
  - `internal/cache/chaos_evict_race_test.go`: `TestChaos_BlobGC_EvictRace_OneZeroNegOne` (its later blob reap needs the snapshot's `snapshot_member` rows gone);
  - `internal/cache/chaos_snapshot_race_test.go`: `TestChaos_SnapshotGC_DisplacementDuringTick_VariantOne` — its invariant changes, not one assertion: the outcomes become "S1 current at the SELECT, not retired" and "S1 retired, `DisplacedReaped` 1";
  Every other call site only gains the argument (`false` keeps its meaning): `snapshot_gc_retired_test.go`, `chaos_snapshot_race_test.go`, `f24_gc_test.go`, `gc_test.go`'s others; and `slowBatchStore` (`internal/gc/gc_store_test.go`) implements `gcStore` beside `fakeStore` and takes the new signature (the compiler finds them all). Run the whole suite (`go test -race ./...`) and list in the report every test changed, with why. Unaffected, confirm they pass unchanged: `internal/gc/retired_test.go`'s `TestRunTick_SubJobCReapsRetiredSnapshotsAndTheirBlobs` (the startup pass deletes as before; the first periodic tick finds nothing new to retire, so `retired_reaped` stays 2), and, since a non-startup `runTick` runs C in the same pass, `TestGCEndToEnd_DisplacedSnapshot_DeadBlobReaped` and `TestForensicRetention_KeepDisplacedThree`/`…Zero`.

- [ ] **Step 6: Probe (recorded, not committed).** In a scratch test, seed six displaced snapshots of 70,000 `package_hash` rows each beyond `keep_displaced`; time `RunSnapshotGCBatch(…, true)` and each `RunRetiredSnapshotGCChunk` call; record the numbers in the report (expected: the batch in milliseconds, each chunk under 200 ms).

- [ ] **Step 7: Mutants.** (a) Delete instead of retire with `retireDisplaced` → `…RetiresTheDisplacedBeyondKeep` fails. (b) Retire also at startup (ignore the parameter) → `…DeletesTheDisplacedWhenNotRetiring` and the extended `TestRun_DeletesRetiredSnapshotsAnHourAfterTheStart` fail. (c) `runSnapshotPass` passes `false` always → `TestPeriodicTick_RetiresAndDeletesTheDisplacedSnapshots`'s `retired_reaped` assertion fails. Restore each.

- [ ] **Step 8: Docs**, each claim checked against Step 1's greps and this task's tests:
  - `docs/configuration.md`, the `[gc]` table: `snapshot_batch_size` → "Maximum snapshots one GC batch handles: abandoned adoption candidates are deleted; displaced snapshots beyond `keep_displaced` are deleted by the startup pass and retired by the periodic runs, whose retired snapshots' rows are then deleted 10,000 rows a transaction, so a large suite never holds the database for long while the cache serves. At least `1`."; `max_tick_duration` (~line 970): its list of retired snapshots gains "and the displaced snapshots a periodic run retired beyond `keep_displaced`".
  - `docs/admin-ui-spec.md` (~line 457): the `gc.retired_reaped` row's list gains the displaced snapshots retired by a periodic run, and states the double count of DN-7 (`displaced_reaped` counts them when retired; `retired_reaped` when deleted).
  - `docs/log-fields.md`: `acu_gc_retired_reaped_total` (~lines 231–234) and `gc_retired_run_complete` (~line 404) gain the same, with the double count.
  - `internal/gc/metrics.go`: `acu_gc_displaced_reaped_total`'s help → "Total displaced suite_snapshot rows beyond keep_displaced reaped (startup) or retired (periodic; deleted later and counted in acu_gc_retired_reaped_total)"; `acu_gc_retired_reaped_total`'s help gains the displaced ones.
  - The spec's C9 paragraph defining `retired_at` (~lines 1456–1458): it also marks displaced snapshots beyond `keep_displaced` that a periodic GC run retired (this plan, ruling HD-4).

- [ ] **Step 9: Changelog** — under *Fixed*:

```
- GC no longer holds every database write for seconds while it deletes old
  snapshots: periodic runs retire the displaced snapshots beyond
  `gc.keep_displaced` and delete them 10,000 rows a transaction.
```

- [ ] **Step 10: Commit** the changed files: "GC: periodic sub-job B retires displaced snapshots and sub-job C deletes them in chunks (H3 final review R3 #4)".

### Task 3: No transparent gzip; adoption stops a member's download at its declared size

**Files:**
- Modify: `internal/fetch/dialer.go` (`newTransport`), `internal/fetch/fetch.go` (`Target`, `ErrBodyTooLarge`, `doAttempt`, `streamBody`, `isRetryable`), `internal/fetch/classify.go` (the case and the row comments), `internal/fetch/conditional.go` (its stale comment)
- Modify: `internal/freshness/adoption.go` (`tryByHashFetch`, `fetchMemberURL`, new `memberMaxBytes`)
- Test: `internal/fetch/fetch_test.go`, `internal/fetch/classify_test.go`, `internal/freshness/adoption_test.go`
- Modify: `docs/configuration.md` (the unsigned threat note), `Changelog.md`

**Interfaces:**
- Produces: `fetch.Target.MaxBytes int64`; `fetch.ErrBodyTooLarge`; `func memberMaxBytes(m ReleaseMember) int64`.

- [ ] **Step 1: Write the failing fetch tests** (httptest servers through `newTestClient`, `bufDst`):
  - `TestFetch_SendsNoAcceptEncoding`: the server records `r.Header.Values("Accept-Encoding")` and answers gzip only when asked; no value was sent and `dst` holds the identity body. `TestFetch_SendsNoAcceptEncodingOverHTTP2`: the same over HTTP/2 (`httptest.NewUnstartedServer` with `EnableHTTP2 = true`, `StartTLS`; install the server's CA with `SetRootCAsForTest`, its restore deferred, **before** building the client — `newTransport` reads the pool once, at construction; the template is `dialer_tls_seam_test.go`'s `TestSetRootCAsForTest_GatesUpstreamVerification`), asserting `r.ProtoMajor == 2`.
  - `TestFetch_StoresAGzipBodyItDidNotAskFor`: the server always sends `Content-Encoding: gzip` and a gzip body; `dst` holds the compressed bytes, unchanged.
  - `TestFetch_MaxBytesStopsAChunkedBodyThatRunsOver`: a chunked 200 (flush after each 4 KiB, 1 MiB in all), `MaxBytes: 10000`: `errors.Is(err, ErrBodyTooLarge)`, `dst.Written() <= 10001`, and the server saw one request (not retried).
  - `TestFetch_MaxBytesRefusesADeclaredLengthOverIt`: `Content-Length: 50000`, `MaxBytes: 10000`: `ErrBodyTooLarge` with `dst.Written() == 0`.
  - `TestFetch_MaxBytesAcceptsABodyOfExactlyIt`: 10,000 bytes, with and without `Content-Length`, `MaxBytes: 10000`: success.
  - `TestFetch_MaxBytesRefusesAResumeDeclaringMore`: a chunked first 200 with an `ETag`, cut after 5,000 bytes; the 206 declares `bytes 5000-49999/50000`; `MaxBytes: 10000`: `ErrBodyTooLarge`, no third request.
  - `TestFetch_MaxBytesStopsAResumedBodyThatRunsOver`: the same first 200; the 206 declares `bytes 5000-9999/*` and its body runs past 10,000 bytes: `ErrBodyTooLarge`, no third request (without the cap at the 206 call site it ends as `ErrSizeMismatch`, which is retried).
  - `TestFetch_ZeroMaxBytesIsNoLimit`: a 100,000-byte chunked body with `MaxBytes` 0 succeeds.
  In `classify_test.go`: a `Fetch` error wrapping `ErrBodyTooLarge` classifies as `body_too_large`.

- [ ] **Step 2: Run them; they fail** (Go sends `Accept-Encoding: gzip` and decodes; no `MaxBytes`).

- [ ] **Step 3: Implement fetch.** `newTransport`: add `DisableCompression: true` to the `http.Transport`, with:

```go
		// AIDEV-NOTE: the 2026-09 security review's VA-3/VA-4 (ruling HD-5).
		// With compression enabled Go asks every upstream for gzip and
		// decodes it in transit: the decoded body has ContentLength -1, which
		// skipped the size check, and a resumed download (Go does not ask for
		// gzip on a Range request) appended compressed bytes to a decoded
		// prefix. The cache stores and serves bytes as the upstream sent
		// them, never with a Content-Encoding, so it must not ask for any —
		// as apt's own http method does not. The HTTP/2 transport honours it.
		DisableCompression: true,
```

`Target` gains:

```go
	// MaxBytes, when > 0, is the most body bytes the fetch accepts: a
	// response whose declared length exceeds it fails before its body is
	// read, and a body that runs past it fails as soon as it does, both
	// with ErrBodyTooLarge, which is not retried. 0 is no limit.
	MaxBytes int64
```

New sentinel beside the others: `ErrBodyTooLarge = errors.New("fetch: response body exceeds the size expected")`. `doAttempt`, case 200, before the truncate: `if target.MaxBytes > 0 && resp.ContentLength > target.MaxBytes { return out, fmt.Errorf("%w: declared %d bytes, at most %d expected", ErrBodyTooLarge, resp.ContentLength, target.MaxBytes) }`; case 206, after the existing total checks: `if target.MaxBytes > 0 && (total > target.MaxBytes || last >= target.MaxBytes) { return out, fmt.Errorf("%w: …", ErrBodyTooLarge, …) }` (`last` is an index: an exact-size resume passes). `streamBody` takes the limit:

```go
// streamBody copies resp.Body into dst, … (keep the existing text). With max
// > 0 it reads at most one byte past max and fails with ErrBodyTooLarge when
// dst then holds more than max bytes.
func streamBody(resp *http.Response, dst FetchDst, max int64) error {
	src := io.Reader(resp.Body)
	if max > 0 {
		left := max - dst.Written()
		if left < 0 {
			left = 0
		}
		src = io.LimitReader(resp.Body, left+1) // one byte past max tells "runs over" from "ends there"
	}
	if _, err := io.Copy(&writeErrTagger{w: dst}, src); err != nil {
		return err
	}
	if max > 0 && dst.Written() > max {
		return fmt.Errorf("%w: more than %d bytes", ErrBodyTooLarge, max)
	}
	return nil
}
```

Both call sites pass `target.MaxBytes`. `isRetryable`: `if errors.Is(err, ErrBodyTooLarge) { return false }` beside `ErrCacheWriteFailed`'s. `classifyError`: `case errors.Is(err, ErrBodyTooLarge): return "body_too_large"` in the switch that holds `ErrSizeMismatch`, before it; update the row comments ("The 23 rows" → 24; the `conditional` parameter's comment: `body_too_large` is now also a `Fetch` outcome). `conditional.go` (~line 148): drop "(and when the transport decompresses)" from the `ContentLength` comment.

- [ ] **Step 4: Run the fetch tests**; all pass, the existing ones included.

- [ ] **Step 5: Write the failing adoption tests** in `internal/freshness/adoption_test.go`:
  - `TestAdoptMember_LimitsTheDownloadToTheDeclaredSize`: the `fakeFetcher` gains a `targets []fetch.Target` field recording each call; a member of declared size 1234 is fetched with `MaxBytes == 1234` on the by-hash probe and on the plain path (make the probe fail so both run); a member declared size 0 with `MaxBytes == 1`.
  - `TestAdoptMember_AMemberLongerThanDeclaredStopsAtItsSize`: a real `fetch.Client` and an httptest upstream that serves a member of declared size 1 MiB as a 16 MiB chunked body in flushed 4 KiB writes, counting the bytes it wrote. Adoption builds a member's URL from `suite.CanonicalHost` with no port and `Fetch` requires the URL's host to equal it, so the httptest server is reached as a **forward proxy**, as `internal/fetch/upstream_proxy_test.go` (~300–316) does: `fetch.New(fetch.Options{Proxy: srv.URL, AllowedHostRegex: []string{`^archive\.example\.com$`}})` (no deny ranges: `New` refuses a proxy with them), the suite's `CanonicalHost` is `archive.example.com`, and the handler serves `r.URL.Path`. Call `a.adoptMember(ctx, suite, m, false)` directly (no by-hash probe; `MemberRetryCount` 0). Assert `errors.Is(err, ErrAdoptionMemberFetchFailed)` (before the change it is `ErrAdoptionMemberMismatch`: a chunked body skips the size checks and only the hash fails), the `*AdoptionMemberError`'s `Detail` contains `fetch.ErrBodyTooLarge.Error()`, no pool file and no temp file remain for it, and the upstream wrote fewer than declared + 1 MiB bytes.

- [ ] **Step 6: Implement adoption.**

```go
// memberMaxBytes is a member download's limit (fetch.Target.MaxBytes): its
// declared size (ruling HD-5), so a member never downloads past what its
// Release declares — for an unsigned Release, what the repository itself
// declares. A member declared empty gets one byte, since 0 means no limit:
// any byte means it is not the declared member, which verifyMember rejects.
func memberMaxBytes(m ReleaseMember) int64 {
	if m.Size < 1 {
		return 1
	}
	return m.Size
}
```

Set `MaxBytes: memberMaxBytes(m)` in the `fetch.Target` of `tryByHashFetch` and of `fetchMemberURL`. (A by-hash probe that fails `ErrBodyTooLarge` falls back to the plain path like any probe error but `ErrAdoptionDBFailed`; repair and reconcile go through `adoptMember` and so get the cap — say so in the report.)

- [ ] **Step 7: Run** `go test -race ./internal/fetch ./internal/freshness`; all pass.

- [ ] **Step 8: Mutants.** (a) Drop `DisableCompression` → both `…SendsNoAcceptEncoding…` tests and `…StoresAGzipBodyItDidNotAskFor` fail. (b) `streamBody` ignores `max` → the chunked test fails. (c) Drop the declared-length check → `…RefusesADeclaredLengthOverIt` fails. (d) Retry `ErrBodyTooLarge` → the one-request assertion fails. (e) Drop `max` at the 206 call site → `…StopsAResumedBodyThatRunsOver` fails. (f) Drop `MaxBytes` from `fetchMemberURL` → both adoption tests fail. (g) Drop `MaxBytes` from `tryByHashFetch` → `…LimitsTheDownloadToTheDeclaredSize` fails (its by-hash target). (h) `memberMaxBytes` returns `m.Size` (0 for an empty member) → `…LimitsTheDownloadToTheDeclaredSize` fails. Restore each.

- [ ] **Step 9: Docs.** `docs/configuration.md`, the unsigned threat note (~line 693): replace "but not in bytes: each file can be as large as its server sends within `upstream.total_timeout` (no response-body ceiling, and gzip is decompressed in transit: the 2026-09 security review's A-2 and VA-3, open)," with "and in bytes by the sizes its own `Release` declares: each file's download stops at its declared size, and nothing is decompressed in transit (the 2026-09 security review's A-2 and VA-3), but an unsigned `Release` declares what its server wants,". Read the paragraph whole afterwards: the sentence must still parse. No document lists `acu_fetch_total`'s outcomes (`grep -rn "acu_fetch_total\|body_too_large" docs/ --exclude-dir=superpowers`: no output); the code comments of Step 3 are the record.

- [ ] **Step 10: Changelog** — under *Fixed*:

```
- Upstream downloads are no longer decompressed in transit (a gzip-encoded
  body skipped the size check, and a resumed one could be corrupted), and
  adoption stops a member's download at the size its `Release` declares.
```

- [ ] **Step 11: Commit** the changed files: "Fetch: no transparent gzip; adoption caps each member's download at its declared size (security review A-2, VA-3, VA-4)".

### Task 4: Strict mode refuses a `.deb` whose vouching ends during its download

**Files:**
- Modify: `internal/handler/handler.go` (`prefetchCheck`, `prefetchRecheck`, `runFetch`, the doc comments of `prefetchCheck` and `strictAfterDownload`)
- Test: `internal/handler/strict_wait_test.go`
- Modify: `Changelog.md`

**Interfaces:**
- Consumes: `strictWaitRig` (strict mode on), `newPhase3StrictHandlerCapturing(t, false, true, nil)` with `adoptCoverageComplete` (strict mode off), `commitVouching` (returns nothing), `commitStrictSnap` (`flat_strict_test.go`, returns the snapshot id), `fakeAdoptions` (its `asked` hook), `firstFetchHook`, `(*cache.Cache).ForgetSuite(…, wantCurrent)` (existing).
- Produces: `prefetchCheck(ctx, req) (res sfResult, end, unread, vouched bool)`; `prefetchRecheck(ctx, req, count) (res sfResult, end, vouched bool)`.

- [ ] **Step 1: Write the failing test** `TestServeHTTP_StrictMissDecidesWhenVouchingEndsDuringTheDownload`, table-driven like `TestServeHTTP_StrictMissDecidesAfterAFailedPrefetchRead`. A fetch hook that runs **after** the upstream body is written ends the vouching — `firstFetchHook` gains an `after func()` that runs once, after the first fetch only, as `before` runs once before it (guard both against nil; today a nil `before` panics in `once.Do`); in the in-flight row the retried fetch must not end the vouching again, so its `during` commit vouches on a suite of its own — by forgetting the vouching suite (`ForgetSuite`; `commitStrictSnap` gives the id if needed) or committing a new current snapshot that does not list the path. Rows:
  - strict host, the `.deb` vouched before the request, nothing in flight → 502, `Retry-After: 60`, one `unvouched_deb_refused`, the file not served;
  - strict host, a signed adoption in flight that commits rows vouching the same bytes while the miss waits → 200 with the body after one wait;
  - strict host, **no** row before the request: `fakeAdoptions.asked` commits the vouching suite once, as `TestServeHTTP_StrictRefusalLooksUpOnceMoreWhenTheAdoptionJustEnded` does, so `prefetchRecheck`'s second look finds it; the after-download hook forgets it → 502, `Retry-After: 60`, one `unvouched_deb_refused`;
  - strict mode off (`newPhase3StrictHandlerCapturing(t, false, true, nil)`, `adoptCoverageComplete`, `commitVouching`) → 200, the body served (unchanged behaviour).

- [ ] **Step 2: Run it; the strict rows fail** (the file is served).

- [ ] **Step 3: Implement.** `prefetchCheck` returns `vouched = true` when its read found exactly one distinct declared hash, and passes on `prefetchRecheck`'s `vouched` (true in its `len(rows) > 0` case). In `runFetch`, keep `prefetchVouched` beside `prefetchUnread`, and in the read after the download, case 0:

```go
				// Final review R4 #2 and ruling HD-6: strict mode's decision is
				// made here when the check before the download could not read
				// the vouching hashes, or when it found the path vouched and the
				// vouching is gone now (its snapshot retired, or displaced by one
				// that does not list it, during the download) — a .deb that was
				// vouched is never served unchecked on a strict host.
				if (prefetchUnread || prefetchVouched) && isDebPath(req.Path) {
```

Update `prefetchCheck`'s and `strictAfterDownload`'s doc comments to name both cases.

- [ ] **Step 4: Run** `go test -race ./internal/handler`; all pass.

- [ ] **Step 5: Mutants.** (a) Drop `|| prefetchVouched` → the three strict rows fail. (b) `prefetchRecheck` never reports `vouched` → the no-row-before row fails. Restore each.

- [ ] **Step 6: Changelog** — under *Fixed*:

```
- With `integrity.refuse_unvouched_debs`, a `.deb` whose vouching snapshot
  is retired or replaced while it downloads is refused like any unvouched
  `.deb`, instead of being served unchecked.
```

- [ ] **Step 7: Commit** the changed files: "Strict mode: a .deb whose vouching ends during its download is decided after it (H3 final review, R6 re-review)".

### Task 5: The apt-shape corpus — fixtures, their generator, apt's verdicts and the oracle

No verifier change in this task: it builds the corpus Tasks 6 and 7 hold the verifier to, and the manual oracle that re-measures apt.

**Files:**
- Create: `e2e/apt-oracle/gen_shapes.py` (the generator), `e2e/apt-oracle/in-container.sh`, `e2e/apt-oracle/run.sh`, `e2e/apt-oracle/README.md`
- Create: `internal/gpg/testdata/aptshapes/` — `keys.gpg`, `Release`, one `<name>.InRelease` or `<name>.Release.gpg` per shape (plus `<name>.Release` for a shape whose detached signature covers another `Release`), `verdicts.txt`
- Modify: `Makefile` (target `e2e-apt-oracle` beside `e2e-live`, in `.PHONY`; `SHELL_SCRIPTS` gains `e2e/apt-oracle/*.sh`), `docs/open-decisions.md` (the pre-RC list), `Changelog.md`

**Interfaces:**
- Produces: the corpus layout above; `verdicts.txt` lines `<name> <d11> <u22> <u24> <d12> <d13> <u26>` (`ok`/`REJ`, DN-2's image order) after a `#` header naming the date and each image's apt and verifier versions; `make e2e-apt-oracle` (manual, like `make e2e-live`; not in CI); `bash e2e/apt-oracle/run.sh --record` (rewrites `verdicts.txt` from a measurement).

- [ ] **Step 1: The generator.** `e2e/apt-oracle/gen_shapes.py OUT` consolidates `PE/gen.py`, `PE/gen2.py`–`gen6.py`, `PE/rereview/gen7.py` and `PE/pgp.py` into one typed script (stdlib + the `gpg` binary): one temporary GnuPG home (deleted at exit), keys A (RSA-3072, signs), B (unknown to the client), E (made with `--faked-system-time 20200101T000000!`, valid one year: expired 2021; its signatures made at `20200601T000000!`) and S (a certify-only primary with a signing subkey); one shared `Release` (a flat repository: `Packages` empty, its SHA256 line, a fixed `Date:` in the past); every one of Appendix A's 135 shapes, by the same name and built exactly as the planning generator built it; `keys.gpg` = the binary export of A, E and S (never B, never a secret key). Header comment: what each key is and why, and that fixtures and verdicts must be regenerated and re-measured together. `ruff format`, `ruff check`, `mypy --strict` clean.

- [ ] **Step 2: Generate** into `internal/gpg/testdata/aptshapes/` (`python3 -I e2e/apt-oracle/gen_shapes.py internal/gpg/testdata/aptshapes`). Expected: 135 shape files plus `keys.gpg` and `Release`, a few hundred KiB in all.

- [ ] **Step 3: The oracle.** `e2e/apt-oracle/in-container.sh` is `PE/in-container.sh` adapted: it reads the corpus at `/fx`, builds each shape's flat repository in a temp directory (the shape file as `InRelease`, or `Release` + `Release.gpg`; an empty `Packages`), runs `apt-get update` with `signed-by=/fx/keys.gpg`, and prints `<name> <rc> <first E:/W: line>`. It runs `set -uo pipefail` without `-e` around the capture: `if out=$(apt-get update …); then rc=0; else rc=$?; fi` (an assignment takes its substitution's status, so under `set -e` the first refused shape would end the script), and `|| true` on the `grep` that extracts the line (it exits 1 when apt printed none). `e2e/apt-oracle/run.sh`: for each image in `${E2E_APT_ORACLE_IMAGES:-debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04}` runs `timeout 600 docker run --rm --network none -v "<repo>/internal/gpg/testdata/aptshapes:/fx:ro" -v "<repo>/e2e/apt-oracle/in-container.sh:/in-container.sh:ro" "<image>" bash /in-container.sh`, collects the verdicts, prints one table (shape × image), and compares them with `verdicts.txt`: any difference is **apt changed** (exit 1; update Appendix A's row in this plan's successor record, `verdicts.txt` via `--record`, and the README's date). With `--record` it writes `verdicts.txt` from the run instead. `set -euo pipefail`; `shellcheck -x` clean (`make shellcheck`).

- [ ] **Step 4: Record and compare.** `bash e2e/apt-oracle/run.sh --record`, then compare the recorded verdicts with Appendix A's apt columns, shape by shape. Expected: identical (a fresh key changes no verdict). Any difference: stop and report it with the shape's bytes; do not edit Appendix A. Then `make e2e-apt-oracle`: exit 0, about one minute with the images cached. Record both runs' output in the report and the README's *Results* (date, tree, the six apt versions).

- [ ] **Step 5: Mutant.** Flip one recorded verdict in `verdicts.txt` → `make e2e-apt-oracle` reports **apt changed** for that shape and exits 1. Restore.

- [ ] **Step 6: Docs.** `e2e/apt-oracle/README.md`: what the corpus is (the shapes, the keys, how to add one: a builder in the generator, regenerate, `--record`, review the new row), what the oracle checks (that apt still gives the recorded verdicts; the cache's verdicts are the Go table test's job, Tasks 6–7), that the "unsafe" guarantee is only as wide as the corpus, that `xi_good_plus_marker`'s u26 refusal is a gpgv 2.4.8 crash ("exited unexpectedly"), not a policy — if a later image fixes it, the oracle reports **apt changed** and the cache's refusal becomes a strict deviation to list — the images, when to run it (before a release, after a go-crypto upgrade, after a change to `internal/gpg`'s armor or signature checks), and *Results*. `docs/open-decisions.md`, the pre-RC list: add "`make e2e-apt-oracle` on the tree you tag" beside `make e2e-live`.

- [ ] **Step 7: Changelog** — under *Added*:

```
- `make e2e-apt-oracle`: checks that the real apt of six client images
  still gives the recorded verdicts on the verifier's 135 signature-shape
  fixtures (manual).
```

- [ ] **Step 8: Commit** the generator, scripts, README, corpus, `Makefile`, `docs/open-decisions.md`, `Changelog.md`: "e2e/apt-oracle: 135 signature shapes and apt 2.2-3.2's verdicts on them".

### Task 6: The verifier refuses the shapes apt refuses: structure (F0–F6), both forms

**Files:**
- Create: `internal/gpg/armor_shape.go` (F0, F2–F5, CRC-24, the line splitter), `internal/gpg/packet_shape.go` (F6), `internal/gpg/aptshape_test.go` (the table test over the corpus)
- Modify: `internal/gpg/verifier.go` (`VerifyInline`, `VerifyDetached`, `requireBareClearsigned`, `decodeMaybeArmoredSignature`, `isArmoredSignature`, `ClassifyVerifyErr`, the sentinels, the AIDEV-NOTEs that list these shapes as open), `internal/gpg/verifier_test.go` and `verifier_detached_test.go` (Step 5's tests), `internal/admin/status.go` (`reasonTooltip`, the `Reason` field comment), `internal/admin/status_test.go` (`TestReasonTooltip`)
- Modify: `docs/configuration.md` if it says a binary `Release.gpg` is accepted (`grep -n "binary" docs/configuration.md`), `Changelog.md`

**Interfaces:**
- Consumes: Task 5's corpus.
- Produces: `ErrMalformedSignature`; `func checkSignatureArmor(b []byte, lines []lineSpan, begin int, decoded []byte) error`; `func requireSignaturePacketsOnly(sigBytes []byte) error`; `func crc24(b []byte) uint32`; in the tests, `aptShapeDeviations map[string]string`, `aptShapeKnownUnsafe map[string]string` and `pendingS1 map[string]bool` (Task 7 deletes the last).

- [ ] **Step 1: The table test** `TestVerifier_FollowsTheAptShapeTable` in `aptshape_test.go`: load `testdata/aptshapes/` — the host keyring from `keys.gpg` alone, `LoadKeyringWithEmbedded(nil, []EmbeddedSource{{Name: "keys.gpg", Data: b}}, logger)` (`LoadKeyring` on the directory would also try every `<name>.Release.gpg` as a keyring and WARN for each), and `verdicts.txt`; for each shape run `VerifyInline` (an `.InRelease` file) or `VerifyDetached` (a `.Release.gpg`, with `Release` or the shape's own `.Release`) with default options (`require_signature = true`, no pins, `allow_short_keyid = true`). Want: rejected iff any apt rejects (DN-3); for a shape in `aptShapeDeviations` the opposite (rejected, every apt accepts), with a non-empty reason; for a shape in `aptShapeKnownUnsafe` the opposite (accepted, every apt rejects), with a non-empty reason; a shape in `pendingS1` is skipped with its name logged. Also `TestAptShapeDeviations_NameShapesInTheTable` (no stale entry in either map) and `TestAptShapeKnownUnsafe_IsExactlyC3` (the map is exactly `{"x3i_sha1_only": …}`, so nobody adds to it lightly). `aptShapeDeviations` holds DN-6's eleven strict deviations with DN-6's reasons; `aptShapeKnownUnsafe` its one known-unsafe shape. `pendingS1` holds the shapes only S1 refuses (Appendix A's *rule* column marks them `S1`); Task 7 empties it. Add `TestCRC24_RFC4880CheckValue` (`crc24([]byte("123456789")) == 0x21CF02`). Run: the structural rows fail (accepted today).

- [ ] **Step 2: Implement `armor_shape.go`**, starting from `PE/proto/fixed.go`'s `checkArmorFixed`, `isMarkerLineRelaxed` and the inline F0 check (adapt names to the package; keep its error texts plain, the callers wrap them):

```go
// The apt-shape checks (spec: docs/open-decisions.md, "The inline verifier
// accepts three shapes apt rejects"; rulings HD-2, HD-8). Each rule is what
// apt 2.2–3.2 enforce on an InRelease or Release.gpg, measured on the corpus
// in testdata/aptshapes (`make e2e-apt-oracle` re-measures it): DN-3 of
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md.
//
// AIDEV-NOTE: go-crypto's decoders are lenient where apt is not — the armor
// reader skips a header line without a colon to the next BEGIN line, ends the
// body at any line starting "-----END ", takes any line with a colon as a
// header, and ignores the CRC (RFC 9580); clearsign.Decode cuts the armor at
// the first substring "\n-----END PGP SIGNATURE-----". These checks run on the
// raw bytes beside the decode, so what the decoder skipped cannot reach the
// pool. Re-check them when go-crypto is upgraded (the table test and the
// oracle pin them).
```

Rules, from DN-3: F2 (the first line after the armor's BEGIN that is `-----END PGP SIGNATURE-----` followed only by spaces, tabs and CRs — never `HasPrefix`: text after the marker fails, `x7*_end_trailing_*` — is the END line, and it is the last line); F3 (the first line after the headers whose trimmed form — spaces, tabs and CRs trimmed — starts with `=` is the CRC line: its trimmed form is `=` and four base64 characters decoding to three bytes equal to `crc24(decoded)`, and only blank lines follow it before END); F4 (no line strictly inside the armor starts with `-`); F5 (every non-blank line before the first blank one contains `: `); F0 (inline and detached: `bytes.Index(b, []byte("-----END PGP SIGNATURE-----"))` is the start of the last line). Lines split on LF; a CR before LF belongs to the line (the marker test trims it); a final LF ends the last line and does not start an empty one, as `PE/proto/main.go`'s `splitLines` does — otherwise F0's "begins the last line" fails on every file.

- [ ] **Step 3: Implement `packet_shape.go`** (F6), from `PE/proto/fixed.go`'s `rawTags` and `requireSignaturePacketsOnly`: the framing walk (old format lengths 0–2, indeterminate refused; new format one-, two- and five-octet lengths, partial refused; every packet tag 2), then `packet.NewReader(…).NextWithUnsupported()` until `io.EOF`, refusing an `*packet.UnsupportedPacket` or any non-signature packet, and requiring one parsed signature per framed packet. AIDEV-NOTE: `packet.Reader.Next()` silently skips v3/v5 signatures, unknown algorithms, markers and unknown tags (`packet/reader.go`), so every later walk over the signature (S1, S2) would not see them; a vendor adding a signature with an algorithm go-crypto lacks is refused until go-crypto supports it (`xi_good_plus_unknownpk_A`, a listed deviation). The checks back each other up: the framing walk alone refuses markers and unknown tags; `NextWithUnsupported`'s refusal of an `UnsupportedPacket` and the per-packet count each refuse what `Next()` would skip, so dropping one of those two shows in no corpus row — keep both.

- [ ] **Step 4: Wire it in.**
  - Inline: `requireBareClearsigned` keeps its F1 check (`aptpath.SniffInRelease`) and its F0 substring rule (today's END check, now "the first END marker begins the last line" rather than "only whitespace follows it"); F2–F5 move to `checkSignatureArmor`. In `VerifyInline`, after `readArmoredSignature` (on the bypass and the trusted path): split `inRelease` into lines, find the armor's BEGIN line — the first line from line 1 on that is `-----BEGIN PGP SIGNATURE-----` followed only by spaces, tabs and CRs (F4 then refuses an un-escaped such line in the signed text, which go-crypto would take as plaintext: dash-escaping is optional to it); `aptpath.DecodableClearsign` rewrites only line 0, so line indexes on `inRelease` stay valid — and call `checkSignatureArmor`, then `requireSignaturePacketsOnly(sigBytes)`; wrap a failure as `fmt.Errorf("%w: %v", ErrMalformedClearsign, err)`. Keep the wording "extraneous bytes after the END marker" for the F2/F0 failure after END (`TestVerifier_RejectsSuffixGarbage` asserts "END marker"). A body with the BEGIN marker but no END line keeps today's error from the decoder.
  - Detached: the signature must start at byte 0 with `-----BEGIN PGP SIGNATURE-----` followed only by spaces, tabs and CRs (F1; `isArmoredSignature`'s whitespace skip goes); anything else — a binary signature included — is `fmt.Errorf("%w: …", ErrMalformedSignature)`, on the bypass path too. After `armor.Decode` and the size cap: F0 and `checkSignatureArmor(sigBytes, lines, 0, out)`, then `requireSignaturePacketsOnly(out)`; wrap failures in `ErrMalformedSignature`. Remove "binary Release.gpg also exists in the wild; both forms are accepted" from `VerifyDetached`'s comment and say what is enforced now.
  - `ClassifyVerifyErr`: `case errors.Is(err, ErrMalformedSignature): return "malformed_signature"`; a `TestClassifyVerifyErr` row for it.
  - `reasonTooltip`: a `malformed_signature` case ("the upstream `Release.gpg` is not one ASCII-armored signature block as apt requires — bytes around it, a binary signature, or a damaged armor"); amend `malformed_clearsign`'s text to cover the new refusals (a line after the signature, a forged checksum, a damaged armor, packets apt refuses); the `Reason` field comment lists the new sentinel; `TestReasonTooltip` gains the case.
  - Rewrite the two AIDEV-NOTEs that list VC-2, C-4 and VC-1 as open (`VerifyDetached`'s and `requireBareClearsigned`'s): VC-2 and C-4 are enforced (name F0–F6), VC-1 is Task 7's, C-3 remains; keep the R-D2 history.

- [ ] **Step 5: Fix the existing tests that pinned the old behaviour**, each listed in the report:
  - `TestVerifyDetached_HappyPath_Binary` (`verifier_detached_test.go`) expects `ErrMalformedSignature` (reason `malformed_signature`);
  - `TestVerifyDetached_TamperedSignature` and `TestVerifyDetached_MultiSig_AllStaleRejects` use binary signatures and would now pass vacuously (refused as binary before any crypto check): move them to armored input and assert their sentinel;
  - `TestVerifyDetached_MultiSig_Binary_StaleAccompaniesCurrent` moves to armored input (Task 7 then rewrites its stale packet);
  - `TestVerifier_AcceptsTrailingNewlines` (appends `"\n\n  \r\n"`) becomes "one final LF accepted; a blank line after END refused";
  - grep `internal/gpg`, `internal/freshness` and `internal/handler` tests for binary `Release.gpg` fixtures (`detachSignWith(…, false)`) and whitespace after END: each now sees a refusal — fix the fixture (never the rule).

- [ ] **Step 6: Run** `go test -race ./internal/gpg ./internal/freshness ./internal/handler ./internal/admin`; all pass, the table test's structural rows included.

- [ ] **Step 7: Mutants**, each caught by the table test (name the rows in the report; `PE/rereview/mutants.sh` ran each on the prototype): (a) F2's END test as `HasPrefix` (text may follow the marker) → `x7i_end_trailing_text`, `x7i_end_trailing_space_text`, `x7d_end_trailing_text` (F2's last-line clause is implied by F0 and needs no mutant); (b) skip the CRC comparison (F3) → `*_crc_forged`, `xi_crc_forged_leading_ws`; (c) allow a non-blank line between the CRC line and END → `*_line_after_crc`; (d) accept a binary signature (F1 detached) → `d_binary`; (e) refuse an extra CR after END (F2 too strict) → `*_end_double_cr`; (f) drop F0 → `i_dash_escaped_end`; (g) drop F4 → `*_endx_nocrc`; (h) drop F5 → `*_hdr_colon_nospace`, `x5i_hdr_tab_after_colon`, `x5i_hdr_empty_value`; (i) F6 neither refuses an `UnsupportedPacket` nor counts packets (`Next()` semantics) → `*_v3garbage_*`, `xi_good_plus_unknownhash_A`, and the deviation `xi_good_plus_unknownpk_A` flips to accepted; (j) drop F6's framing walk → `xi_good_plus_marker`, `x6i_good_plus_unknowntag`; (k) drop the `malformed_signature` case in `ClassifyVerifyErr` → its row fails (`crypto_verify_failed`). Restore each.

- [ ] **Step 8: Docs and Changelog.** If `docs/configuration.md` says a binary `Release.gpg` is accepted, say it is refused. Changelog under *Fixed*:

```
- Adoption refuses an `InRelease` or `Release.gpg` that apt rejects: a
  line after the signature, a forged armor checksum, damaged armor lines,
  packets apt refuses, a `Release.gpg` with bytes around its armor, or a
  binary one (apt 2.2 to 3.2 reject each; the cache adopted them and its
  clients' `apt update` then failed). A second armor block is refused too.
```

- [ ] **Step 9: Commit** the changed files: "Verifier: the armor and packet shapes apt refuses, inline and detached (security review VC-2, C-4)".

### Task 7: Every signature from a known key must verify (S1)

**Files:**
- Modify: `internal/gpg/verifier.go` (new `hashedIssuer` and `requireKnownSignaturesVerify`, called by `VerifyInline` and `VerifyDetached` on the trusted path; `ErrBadSignature`; `ClassifyVerifyErr`; the AIDEV-NOTEs), `internal/gpg/aptshape_test.go` (empty `pendingS1`), `internal/gpg/verifier_test.go` and `verifier_detached_test.go`, `internal/admin/status.go` and `status_test.go`
- Modify: `docs/configuration.md` (one sentence on keyring parity), `Changelog.md`

**Interfaces:**
- Consumes: Task 6's table test and `pendingS1`; `newExpiredEntity`, `buildMultiSigBlock`.
- Produces: `ErrBadSignature`; `func hashedIssuer(sig *packet.Signature) (uint64, bool)`; `func (v *Verifier) requireKnownSignaturesVerify(signed, sigBytes []byte) error`.

- [ ] **Step 1: Empty `pendingS1`** and run the table test: the twelve rows Appendix A marks `REJ(S1)` fail (accepted today and after Task 6): `d_badsig_badfirst`, `d_badsig_goodfirst`, `i_badsig_badfirst`, `i_badsig_goodfirst`, `i_expired_bad_plus_good`, `x3d_badsig_unhashed_fpr_added`, `x3i_badsig_unhashed_fpr_added`, `x3i_subkey_bad_plus_good`, `xd_badsig_issuer_rewritten`, `xi_badsig_issuer_removed`, `xi_badsig_issuer_rewritten`, `xi_crit_plus_good`. (Confirm separately that `xi_twoblocks_good_badsig` and `i_badsig_only` stay refused, by F2 and S2: they are not S1 rows.)

- [ ] **Step 2: Focused tests** (`verifier_test.go`, `verifier_detached_test.go`): `TestVerifyInline_ABadSignatureFromAKnownKeyRejects` (both orders; `errors.Is(err, ErrBadSignature)`; `ClassifyVerifyErr` → `bad_signature`, a `TestClassifyVerifyErr` row), `TestVerifyInline_AKnownKeyOutsideThePinsStillCounts` (a pin for host H names key A only; the keyring also holds key C; A's good signature plus C's bad one → `ErrBadSignature`), `TestVerifyInline_AnExpiredKeysGoodSignatureBesideAGoodOneAdopts` (E made with a lifetime that ends after its signature's time: add a `clearsignAt(t, e, text, when)` helper, since `newExpiredEntity`'s key is valid one second and `clearsignWith` signs at `time.Now`), `TestVerifyInline_ACriticalNotationFailsS1`, `TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature` (on `requireKnownSignaturesVerify` directly, not through `VerifyInline`, whose S2 keeps the collision quirk, ruling HD-10: two entities in the keyring sharing the signer's key ID, the collider first — patch the collider's key ID as `PE/proto/collision_test.go` and `PE/rereview/proto2/collision2_test.go` do — and a valid signature returns nil), `TestHashedIssuer` (v4 with hashed 33 only, hashed 16 only, hashed 33 and unhashed 16, hashed nothing → false), and the detached twins of the first two.

- [ ] **Step 3: Implement**, from `PE/proto/fixed.go`'s `hashedIssuer` and `requireKnownSignaturesVerifyFixed`, with S-I2's per-entity verification:

```go
// requireKnownSignaturesVerify applies apt's rule that a bad signature from a
// known key is fatal (VC-1; rulings HD-3, HD-8): every signature packet whose
// hashed issuer names a signing key in the host keyring must pass its
// cryptographic check against one of the keyring's entities holding that key
// ID, and its unhashed issuer must not contradict the hashed one. A packet
// whose check passes but whose key or signature is expired or revoked passes
// (gpgv's EXPKEYSIG and REVKEYSIG, which apt ignores beside a good
// signature); an unknown critical notation fails (gpgv's BADSIG); a packet
// whose issuer the keyring lacks is ignored (gpgv's NO_PUBKEY).
//
// AIDEV-NOTE: go-crypto takes the issuer from the last issuer subpacket it
// parses, unhashed after hashed, and looks keys up by that key ID only; gpgv
// uses the hashed issuer. So "known" is read from sig.HashSuffix
// (hashedIssuer), and a contradicting unhashed issuer is a failure — else an
// on-path attacker rewrites a bad signature's unhashed issuer and it is
// ignored here while every gpgv-based apt reports BADSIG
// (xi_badsig_issuer_rewritten). openpgp.VerifyDetachedSignature returns a
// non-nil signature exactly when the cryptographic check passed, then reports
// expiry, revocation or an unknown critical notation as its error (go-crypto
// v1.4.1 openpgp/read.go, verifyDetachedSignature and
// checkMessageSignatureDetails). Each candidate entity is tried alone: the
// library hashes once and tries every key with that one hash, which only the
// first candidate can pass. The host keyring, not the suite's trust set: a
// client that installed any of the cache's keys sees the BADSIG. Re-check all
// of this when go-crypto is upgraded (the table test and this file's tests
// pin it).
func (v *Verifier) requireKnownSignaturesVerify(signed, sigBytes []byte) error
```

Body: walk the packets (F6 has already refused anything but signatures); for each, `id, hashed := hashedIssuer(sig)`; if not hashed, use `*sig.IssuerKeyId` (skip when nil); `keys := keyring.KeysByIdUsage(id, packet.KeyFlagSign)`; skip when empty; fail when `sig.IssuerKeyId == nil || *sig.IssuerKeyId != id`; re-serialize the packet alone; for each distinct entity of `keys`, `got, _, verr := openpgp.VerifyDetachedSignature(openpgp.EntityList{e}, bytes.NewReader(signed), bytes.NewReader(one), nil)` passes iff `got != nil && !errors.As(verr, new(pgperrors.SignatureError))`; fail (`fmt.Errorf("%w: issuer %016X", ErrBadSignature, id)`) when none passes. Call it in `VerifyInline` after `readArmoredSignature` with `(block.Bytes, sigBytes)`, and in `VerifyDetached` after `decodeMaybeArmoredSignature` with `(releaseBytes, binarySig)` — on the trusted path only, after `len(trustSet) == 0`'s check and before `verifyAnyTrusted`, never on the bypass path (DN-8). `ClassifyVerifyErr`: `case errors.Is(err, ErrBadSignature): return "bad_signature"`. `reasonTooltip`: a `bad_signature` case ("the upstream anchor carries a signature from a key the cache knows that fails verification — apt rejects it; often tampering on the path"), `TestReasonTooltip` gains it. Update the AIDEV-NOTEs Task 6 rewrote: VC-1 is enforced; C-3 stays open.

- [ ] **Step 4: Fix the existing tests that pinned a bad signature beside a good one**, keeping the per-packet loop's coverage (SPEC2 §7.6.3: the loop walks past a packet that fails S2 and accepts on a later one): `TestVerifier_MultiSig_StaleAndCurrent` and the armored `TestVerifyDetached_MultiSig_…StaleAccompaniesCurrent` get a first packet that fails S2 but not S1 — the expired key E's good signature (`ErrKeyExpired`, so `verifyAnyTrusted` moves on) or B's unknown-key one — and still expect acceptance; add `…_StaleAndCurrentRejects` twins with A's stale signature expecting `ErrBadSignature`; the two `AllStale` tests assert `ErrBadSignature`. Grep `internal/freshness` and `internal/handler` tests for `buildMultiSigBlock` and fix any fixture S1 now refuses (never the rule). List each in the report.

- [ ] **Step 5: Run** `go test -race ./internal/gpg ./internal/freshness ./internal/handler ./internal/admin`; all pass, every table row included.

- [ ] **Step 6: Mutants.** (a) Treat any non-nil error as a failure → `i_expired_good_plus_good`, `i_good_plus_expired_good` and `…AnExpiredKeysGoodSignatureBesideAGoodOneAdopts` fail. (b) Tolerate a `SignatureError` → `xi_crit_plus_good` and `…ACriticalNotationFailsS1` fail. (c) Use `sig.IssuerKeyId` for "known" → `*_badsig_issuer_rewritten`, `*_badsig_unhashed_fpr_added` fail. (d) Verify against the whole keyring at once → `TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature` fails (`ErrBadSignature`). (e) Use the trust set instead of the host keyring → `…AKnownKeyOutsideThePinsStillCounts` fails. (f) Skip the call in `VerifyDetached` → `d_badsig_*` fail. (g) Drop the `bad_signature` case in `ClassifyVerifyErr` → its row fails. Restore each.

- [ ] **Step 6b: Re-run the oracle** (`make e2e-apt-oracle`): apt's verdicts unchanged; and the table test with the final rules: Appendix A's *rule* column exactly (`aptShapeKnownUnsafe`: `x3i_sha1_only`, C-3; `aptShapeDeviations`: DN-6's eleven).

- [ ] **Step 7: Docs and Changelog.** `docs/configuration.md`, where `[[trusted_signer]]` or the host keyring is described: one sentence — "The cache refuses an anchor that carries a bad signature from any key in its keyring, as apt does; install in the cache every key your clients' `signed-by` names, so that it refuses what they would." Changelog under *Fixed*:

```
- Adoption refuses a signed anchor that carries a bad signature from a key
  the cache knows beside a good one, as apt does.
```

- [ ] **Step 8: Commit**: "Verifier: a bad signature from a known key rejects, as apt's BADSIG does (security review VC-1)".

### Task 8: Records, the gate and the final review

**Files:**
- Modify: `docs/open-decisions.md`, `Changelog.md` (read it whole: one entry per change, the file's style)

- [ ] **Step 1: `docs/open-decisions.md`.** Move the five entries from *Open* to *Decided*, each dated "the maintainer, 2026-10-28: worked for 1.1.0", saying what was done and naming its commits (the GC entry records Task 2's probe numbers and ruling HD-7). Add to *Open*, each with its evidence and a recommendation: (a) downloads with no declared size — request-path misses, hot prefetch, the handler's `runSnapshotMemberFetch` — keep only `Content-Length` and `upstream.total_timeout` (a cap needs a size per file: a schema change; `runSnapshotMemberFetch` could use the blob row's size when it survives); (b) `accept_any_signer`'s bypass applies no S1 check (DN-8); (c) the handler's `SniffReleaseGPG` still treats a binary `Release.gpg` as a signed anchor (fail-safe; DN-8); (d) `debian:13`'s `sqv` accepts a forged CRC and a bad signature beside a good one, which the cache now refuses (stricter than that client, never looser); (e) the eleven strict deviations of DN-6, revisited when go-crypto changes; (f) the security review's C-3 (weak digests and keys: `x3i_sha1_only` is adopted, all six apt reject it; the table's one known-unsafe shape) and C-1 (dates, rollback) stay open; (g) S1 covers only the keys the cache holds (DN-5); (h) S2's key-ID collision quirk (ruling HD-10): a host key that shares the signer's key ID and is loaded ahead of it makes `verifyAnyTrusted` refuse a valid anchor (`crypto_verify_failed`), as before this plan; recommendation: verify S2's fingerprint path against the single entity owning the issuer fingerprint, with a test, if it is ever met; (i) a periodic tick's retirements that its sub-job C did not reach wait for the next tick or the retired run an hour after a start (DN-7's residual).

- [ ] **Step 2: The gate**, on the final tree, results in `…/scratchpad/hardening/gate/summary.txt`: `go vet ./...`; `go test -race ./...` and `go test -race -tags e2ehooks ./...`; `golangci-lint run ./...` and with `--build-tags e2ehooks`; `gofmt -l .` (the five drift files only); `ruff format --check`, `ruff check` and `mypy --strict` on `e2e/apt-oracle/gen_shapes.py`; `make shellcheck`; the simulations under 4.5 s each under `-race`; the race suite under `golang:1.27-bookworm`; `make e2e`; `E2E_FLAT_IMAGES=debian:12 make e2e-flat`; `make e2e-apt-oracle`; `make lint`. And the **vendor re-run**, the security review's survey (`PE/vendor/`, `PE/vendor-keys/`, `PE/vendors.txt`, `PE/vendor-keys.txt`; `PE/vendor-verdicts.txt` holds the verdicts of the verifier at `b0aa019` on these bytes, its `today=` column) through the final verifier, by a scratch `_test.go` copied into `internal/gpg` for the run and removed after it, never committed: build one host keyring by parsing **every** file of `PE/vendor-keys/` with the package's keyring-file parser whatever its extension (64 of the 70 are `*.key`, which `LoadKeyring` skips: it reads only `*.gpg` and `*.asc`), deduplicated as the loader does; no pins, `allow_short_keyid = true`, no bypass; for each vendor, `VerifyInline` on its `InRelease` and `VerifyDetached` on its `Release` + `Release.gpg`; compare with the `today=` column. Expected: every anchor that verifies today still verifies (68 `InRelease`, 67 `Release.gpg`), and the only newly refused are the binary `Release.gpg` of nginx, steam and syncthing (the 11 refused both before and after stay refused). If PE's survey is gone, re-fetch it with `PE/fetch-vendors.sh` and `PE/fetch-keys.sh` and recompute the baseline on the new bytes (vendors re-sign daily) by running the same test on `main` in a second worktree, or with `PE/proto`'s `today` path; if even that is impossible, record that the step could not run.

- [ ] **Step 3: The final review** — four lenses, each an independent reviewer on the branch's whole diff from `main` (`git diff --no-ext-diff main...HEAD`): (1) the verifier against DN-2/DN-3 and apt's behaviour, including shapes beyond the corpus; (2) GC data lifecycle (retire vs delete by mode, every reader of `retired_at`, the rotation); (3) fetch and the handler (the cap, retry and classification, strict mode); (4) tests, docs and operability (each claim against the code, the Changelog). Triage, fix, re-review (at most two rounds per lens), re-gate.

- [ ] **Step 4: Commit** the records: "GC and upstream hardening: records".

---

## Appendix A — The planning verdicts (2026-10-28)

Each shape: apt's verdict in `debian:11`, `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`; then **rule**, the verdict DN-3's rules give (from the security review's prototype, `PE/proto/fixed.go`), with the rule that refuses it where only S1 does (`S1`) and the class: `ok` (agrees with DN-3), `dev` (a DN-6 strict deviation), `UNSAFE` (DN-6's known-unsafe shape: C-3, out of scope). Batch `fx` is the controller's (`PE/gen.py`), `fx2`–`fx6` the security review's (`PE/gen2.py`–`gen6.py`), `fx7` its re-review's (`PE/rereview/gen7.py`). Source: `PE/combined-fixed.txt` and `PE/rereview/table-fx7.txt`, checked by the re-review against the rules exactly as DN-3 words them (`PE/rereview/compare-appendix.txt`: 0 differing rows); Task 5 Step 4 re-measures the apt columns on the committed corpus.

```
batch shape                           d11 u22 u24 d12 d13 u26  rule      class
fx    d_badsig_badfirst               REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx    d_badsig_goodfirst              REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx    d_begin_trailing_ws             ok  ok  ok  ok  ok  ok   ok        ok
fx    d_binary                        REJ REJ REJ REJ REJ REJ  REJ       ok
fx    d_blank_in_base64               ok  ok  ok  ok  ok  ok   ok        ok
fx    d_comment_header                ok  ok  ok  ok  ok  ok   ok        ok
fx    d_crc_forged                    REJ REJ REJ REJ ok  REJ  REJ       ok
fx    d_crc_missing                   ok  ok  ok  ok  ok  ok   ok        ok
fx    d_crlf_all                      ok  ok  ok  ok  ok  ok   ok        ok
fx    d_end_crlf                      ok  ok  ok  ok  ok  ok   ok        ok
fx    d_end_trailing_ws               ok  ok  ok  ok  ok  ok   ok        ok
fx    d_good                          ok  ok  ok  ok  ok  ok   ok        ok
fx    d_lead_nl                       REJ REJ REJ REJ REJ REJ  REJ       ok
fx    d_lead_ws                       REJ REJ REJ REJ REJ REJ  REJ       ok
fx    d_line_after_crc                ok  ok  ok  ok  REJ ok   REJ       ok
fx    d_no_final_nl                   ok  ok  ok  ok  ok  ok   ok        ok
fx    d_trail_lf                      REJ REJ REJ REJ REJ REJ  REJ       ok
fx    d_twoblocks                     REJ REJ REJ REJ ok  REJ  REJ       ok
fx    d_unknown_goodfirst             ok  ok  ok  ok  ok  ok   ok        ok
fx    i_badsig_badfirst               REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx    i_badsig_goodfirst              REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx    i_badsig_only                   REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_begin_trailing_ws             ok  ok  ok  ok  ok  ok   ok        ok
fx    i_blank_in_base64               ok  ok  ok  ok  ok  ok   ok        ok
fx    i_comment_header                ok  ok  ok  ok  ok  ok   ok        ok
fx    i_crc_forged                    REJ REJ REJ REJ ok  REJ  REJ       ok
fx    i_crc_lowercase_eq_ws           ok  ok  ok  ok  ok  ok   ok        ok
fx    i_crc_missing                   ok  ok  ok  ok  ok  ok   ok        ok
fx    i_crc_ok_rearmored              ok  ok  ok  ok  ok  ok   ok        ok
fx    i_crlf_all                      ok  ok  ok  ok  ok  ok   ok        ok
fx    i_dash_escaped_end              REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_end_crlf                      ok  ok  ok  ok  ok  ok   ok        ok
fx    i_end_trailing_ws               ok  ok  ok  ok  ok  ok   ok        ok
fx    i_expired_bad_plus_good         REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx    i_expired_good_plus_good        ok  ok  ok  ok  ok  ok   ok        ok
fx    i_expired_only                  REJ REJ REJ REJ ok  REJ  REJ       ok
fx    i_good                          ok  ok  ok  ok  ok  ok   ok        ok
fx    i_good_plus_expired_good        ok  ok  ok  ok  ok  ok   ok        ok
fx    i_lead_nl                       REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_line_after_crc                ok  ok  ok  ok  REJ ok   REJ       ok
fx    i_no_final_nl                   ok  ok  ok  ok  ok  ok   ok        ok
fx    i_sigbegin_trailing_ws          ok  ok  ok  ok  ok  ok   REJ       dev
fx    i_trail_lf                      REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_trail_text                    REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_trail_ws                      REJ REJ REJ REJ REJ REJ  REJ       ok
fx    i_unknown_goodfirst             ok  ok  ok  ok  ok  ok   ok        ok
fx    i_unknown_unknownfirst          ok  ok  ok  ok  ok  ok   ok        ok
fx2   xd_badsig_issuer_rewritten      REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx2   xd_blank_after_crc              ok  ok  ok  ok  ok  ok   ok        ok
fx2   xd_control_good                 ok  ok  ok  ok  ok  ok   ok        ok
fx2   xd_end_double_cr                ok  ok  ok  ok  ok  ok   ok        ok
fx2   xd_endsuffix_nocrc              REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xd_endx_nocrc                   REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xd_good_plus_literal            REJ REJ REJ REJ REJ ok   REJ       ok
fx2   xd_good_plus_v3garbage_A        REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xd_hdrjunk_begin                REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xd_twoblocks_good_unknown       ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xd_twoblocks_goodgood           ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xi_armor_hdr_nocolon            REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_badsig_issuer_removed        REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx2   xi_badsig_issuer_rewritten      REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx2   xi_blank_after_crc              ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_cleartext_comment_hdr        ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xi_control_good                 ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_crc_forged_leading_ws        REJ REJ REJ REJ ok  REJ  REJ       ok
fx2   xi_crc_trailing_ws              ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_crit_only                    REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_crit_plus_good               REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx2   xi_end_double_cr                ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_endsuffix_nocrc              REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_endx_nocrc                   REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_good_plus_literal            REJ REJ REJ REJ REJ ok   REJ       ok
fx2   xi_good_plus_marker             ok  ok  ok  ok  ok  REJ  REJ       ok
fx2   xi_good_plus_unknownhash_A      ok  ok  ok  ok  REJ ok   REJ       ok
fx2   xi_good_plus_unknownpk_A        ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xi_good_plus_v3garbage_A        REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_good_plus_v3garbage_B        ok  ok  ok  ok  REJ ok   REJ       ok
fx2   xi_goodsig_issuer_rewritten     REJ REJ ok  REJ ok  ok   REJ       ok
fx2   xi_hash_none                    ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_hash_sha1                    ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_hash_sha512                  ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_hdrjunk_begin                REJ REJ REJ REJ REJ REJ  REJ       ok
fx2   xi_text_dash_escaped_extra      ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_text_trailing_ws             ok  ok  ok  ok  ok  ok   ok        ok
fx2   xi_two_crc_lines                ok  ok  ok  ok  REJ ok   REJ       ok
fx2   xi_twoblocks_good_badsig        REJ REJ REJ REJ ok  REJ  REJ       ok
fx2   xi_twoblocks_good_unknown       ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xi_twoblocks_goodgood           ok  ok  ok  ok  ok  ok   REJ       dev
fx2   xi_unknown_armor_header         ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3d_armor_no_blank              ok  ok  ok  ok  ok  ok   REJ       dev
fx3   x3d_badsig_unhashed_fpr_added   REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx3   x3d_bom                         REJ REJ REJ REJ REJ REJ  REJ       ok
fx3   x3d_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3d_end_cr_eof                  ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_after_end_cr_line           REJ REJ REJ REJ REJ REJ  REJ       ok
fx3   x3i_armor_no_blank              ok  ok  ok  ok  ok  ok   REJ       dev
fx3   x3i_badsig_unhashed_fpr_added   REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx3   x3i_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_end_cr_eof                  ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_end_tab                     ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_goodsig_unhashed_fpr_added  ok  ok  ok  ok  ok  ok   REJ       dev
fx3   x3i_hdr_blank_ws                ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_sha1_only                   REJ REJ REJ REJ REJ REJ  ok        UNSAFE
fx3   x3i_sha1_plus_good              ok  ok  ok  ok  ok  ok   ok        ok
fx3   x3i_subkey_bad_plus_good        REJ REJ REJ REJ ok  REJ  REJ(S1)   ok
fx3   x3i_subkey_good                 ok  ok  ok  ok  ok  ok   ok        ok
fx4   x4d_b64_invalid_char            ok  ok  ok  ok  REJ ok   REJ       ok
fx4   x4d_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx4   x4d_hdr_colon_nospace           REJ REJ REJ REJ REJ REJ  REJ       ok
fx4   x4i_b64_invalid_char            ok  ok  ok  ok  REJ ok   REJ       ok
fx4   x4i_b64_leading_space           ok  ok  ok  ok  ok  ok   ok        ok
fx4   x4i_b64_line_76                 ok  ok  ok  ok  ok  ok   ok        ok
fx4   x4i_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx4   x4i_hdr_colon_nospace           REJ REJ REJ REJ REJ REJ  REJ       ok
fx4   x4i_hdr_leading_space           ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_hdr_ctrl                    ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_hdr_empty_value             ok  ok  ok  ok  REJ ok   REJ       ok
fx5   x5i_hdr_no_key                  ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_hdr_space_in_key            ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_hdr_tab_after_colon         REJ REJ REJ REJ REJ REJ  REJ       ok
fx5   x5i_hdr_utf8                    ok  ok  ok  ok  ok  ok   ok        ok
fx5   x5i_hdr_version_multi           ok  ok  ok  ok  ok  ok   ok        ok
fx6   x6i_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx6   x6i_good_plus_emptyliteral      REJ REJ REJ REJ REJ ok   REJ       ok
fx6   x6i_good_plus_trailing_zero     ok  ok  ok  ok  REJ ok   REJ       ok
fx6   x6i_good_plus_unknowntag        ok  ok  ok  ok  REJ ok   REJ       ok
fx6   x6i_good_plus_userid            ok  ok  ok  ok  REJ ok   REJ       ok
fx7   x7d_begin_double_cr             ok  ok  ok  ok  ok  ok   ok        ok
fx7   x7d_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx7   x7d_end_trailing_text           REJ REJ REJ REJ REJ REJ  REJ       ok
fx7   x7i_control_good                ok  ok  ok  ok  ok  ok   ok        ok
fx7   x7i_end_trailing_space_text     REJ REJ REJ REJ REJ REJ  REJ       ok
fx7   x7i_end_trailing_text           REJ REJ REJ REJ REJ REJ  REJ       ok
fx7   x7i_sigbegin_double_cr          ok  ok  ok  ok  ok  ok   REJ       dev
```

The twelve `REJ(S1)` rows are exactly those only S1 refuses (refused with S1, accepted without it), verified by the re-review on the prototype (`PE/rereview/combined-v2nos1.txt` against `combined-v2.txt`); Task 6's `pendingS1` is that set. Any row the table test shows otherwise is reported, never forced.
