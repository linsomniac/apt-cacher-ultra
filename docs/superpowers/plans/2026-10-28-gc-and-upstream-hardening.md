# GC and upstream hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close five open items for 1.1.0 — two GC defects that predate 1.1.0 and three upstream-trust gaps — so that the cache never holds the database writer for seconds on a GC batch, never lets one failing GC pass starve the others, never adopts a signed anchor some supported apt rejects, never downloads an adoption member past its declared size, and never serves a strict host's `.deb` unchecked because its vouching snapshot went away during the download. In the maintainer's words: "a reliable, robust, and low maintenance system".

**Architecture:** Five small, independent changes in five packages (`internal/gc`, `internal/cache`, `internal/fetch` with its adoption caller in `internal/freshness`, `internal/handler`, `internal/gpg`), each proven by tests written first, plus a manual apt oracle (`make e2e-apt-oracle`) that runs the verifier's fixtures through the real apt of six client images and fails when the cache would accept a shape some apt rejects. No schema change, no new configuration key.

**Tech Stack:** Go 1.25 (language), Go 1.27 (CI and release builds; 1.26 locally), modernc SQLite, `github.com/ProtonMail/go-crypto` v1.4.1, Bash 5 and shellcheck 0.11, Docker, apt 2.2–3.2 in `debian:11`, `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`, `debian:13`, `ubuntu:26.04`.

**Spec:** `docs/open-decisions.md`, section *Open*, these five entries — they are the requirement, each with its recommendation: **"GC's sub-job B deletes displaced snapshots in one transaction"**, **"A GC pass that fails at every tick starves the passes after it"**, **"The inline verifier accepts three shapes apt rejects"**, **"No limit on an upstream body's size"**, **"A vouched `.deb` whose snapshot is retired during its download"**. Background: the 2026-09-08 security review, `docs/security-review-findings.md` in the main checkout (untracked; `/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`), Theme 1 (C-4, VC-1, VC-2) and Theme 3 (A-2, VA-3, VA-4). The maintainer asked for these five to be worked now (2026-10-28), into 1.1.0, collected in `Changelog.md`.

## Global Constraints

- **Nothing goes to GitHub** (push, pull request, tag, release, comment). Local commits on `fix/gc-and-upstream-hardening` only. Do not tag 1.1.0-rc1.
- **No schema change and no configuration key.** Never edit an entry of `migrations` in `internal/cache/schema.go`.
- `go test -race ./...`, `go vet ./...` and `golangci-lint run ./...` are clean at the end of every task that touches Go; `gofmt -l .` lists only the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`). Never run `gofmt -w` on those five; format only the files you change (`gofmt -w <file>` for each, or `goimports`).
- **Every change is proven:** a test written first and seen failing, then passing, and a mutant (the change reverted or inverted in one place) that fails it. The task report names each mutant and the test that caught it.
- **Every doc claim is checked against the code** by a grep with its expected output or a named test. A claim whose check fails is corrected to what the code does.
- `AIDEV-NOTE`s at every subtle point; existing `AIDEV-*` comments are kept and updated, never removed.
- Each user-visible change adds one line to `Changelog.md` under `## Unreleased: 1.1.0` (*Added*, *Changed* or *Fixed*), in the file's style: one sentence, operator's view.
- `git diff` goes through difftastic in this repository: pass `--no-ext-diff` to every diff a step reads.
- Scratch files go under `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/hardening/` (one variable at the top of a step that uses it). The measured apt table is `…/scratchpad/apt-oracle/table.txt`; its generator `gen.py` and runner `in-container.sh` are beside it.
- `git add` names files; never `-A` or `commit -a`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Never amend or rewrite a commit.

Conventions: every command runs from the repository root. Expected outputs quote planning runs (2026-10-28, a 12-core machine, Go 1.26.8, Docker 29). A test name in a step is the name to use.

## Review Focus

The inputs the spec implies that are most likely to bite a person running this, most likely first, each pinned by a test in the task that owns the code:

1. **A legitimate signed repository whose anchor has an unusual but valid shape** — CRLF line ends, trailing spaces on a marker line, no final newline, a `Comment:` armor header, a blank line inside the base64, no CRC line, an extra signature from a key the cache does not know — must still adopt. Every one of these is accepted by all six apt versions (design note 2). Tasks 5 and 6 pin each as a control row.
2. **A repository signed by two keys where one is expired** (a vendor rotating keys keeps signing with the old one for a while): a good signature from the expired key beside a good one from the current key must still adopt (all apt versions accept it); only a signature that fails its cryptographic check rejects. Task 6.
3. **An upgraded cache with a backlog of displaced snapshots:** the first GC passes must retire them quickly (the startup pass gates the listeners), delete them in chunks within `gc.max_tick_duration`, and terminate (no batch selects a snapshot it already retired). Task 2.
4. **A member declared empty, a chunked member response, and a by-hash URL that serves a newer, larger file:** each download stops at the declared size, and the by-hash probe still falls back to the plain path. Task 3.
5. **A non-strict host whose `.deb` loses its vouching during the download** is still served as before; only a strict host refuses it. Task 4.

---

## Design notes and rulings

**DN-1 — Scope.** Exactly the five *Open* entries named in **Spec**, plus what the measurement of design note 2 shows belongs to the same defect: the detached form's framing (`Release.gpg` with lines around its armor, two armor blocks, or binary), which the same apt versions reject and the cache accepts (ruling HD-1). Not in scope, each staying in `docs/open-decisions.md`: the security review's C-1 (dates, rollback), C-3 (weak digests and keys), the request path's and hot prefetch's downloads of bodies with no declared length (no size is stored for them: `package_hash` has no size column, and adding one is a schema change), and `adoption.accept_any_signer`'s bypass (design note 7).

**DN-2 — What apt does (measured, 2026-10-28).** The controller generated 47 signature shapes with `gpg` 2.4.9 and a throwaway RSA-3072 key A (key E: expired 2021, also in the client's `signed-by`; key B: unknown to the client), each as a flat `file:` repository, and ran `apt-get update` in each client image (`…/scratchpad/apt-oracle/`, `table.txt`). `ok` = accepted, `REJ` = rejected. Images: `debian:11` apt 2.2.4 (gpgv 2.2.27), `ubuntu:22.04` apt 2.4.14 (gpgv 2.2.27), `ubuntu:24.04` apt 2.8.3 (gpgv 2.4.4), `debian:12` apt 2.6.1 (gpgv 2.2.40), `debian:13` apt 3.0.3 (sqv, no gpgv), `ubuntu:26.04` apt 3.2.0 (gpgv 2.4.8).

| Shape (`i_` = `InRelease`, `d_` = `Release.gpg`) | d11 | u22 | u24 | d12 | d13 | u26 |
|---|---|---|---|---|---|---|
| `i_good`, `d_good` | ok | ok | ok | ok | ok | ok |
| `i_trail_lf`, `i_trail_ws`, `i_trail_text` (a line after END: empty, spaces, text) | REJ | REJ | REJ | REJ | REJ | REJ |
| `d_trail_lf` | REJ | REJ | REJ | REJ | REJ | REJ |
| `i_no_final_nl`, `i_end_crlf`, `i_end_trailing_ws`, `d_no_final_nl`, `d_end_crlf`, `d_end_trailing_ws` | ok | ok | ok | ok | ok | ok |
| `i_lead_nl` (blank line before BEGIN) | REJ | REJ | REJ | REJ | REJ | REJ |
| `d_lead_nl`, `d_lead_ws` (blank line, or spaces, before BEGIN) | REJ | REJ | REJ | REJ | REJ | REJ |
| `i_begin_trailing_ws`, `i_sigbegin_trailing_ws`, `d_begin_trailing_ws` (spaces after a BEGIN marker) | ok | ok | ok | ok | ok | ok |
| `i_crlf_all`, `d_crlf_all` | ok | ok | ok | ok | ok | ok |
| `i_crc_forged`, `d_crc_forged` (one character of the `=XXXX` line changed) | REJ | REJ | REJ | REJ | ok | REJ |
| `i_crc_missing`, `d_crc_missing`, `i_crc_ok_rearmored` | ok | ok | ok | ok | ok | ok |
| `i_line_after_crc`, `d_line_after_crc` (a base64 line between the CRC line and END) | ok | ok | ok | ok | REJ | ok |
| `i_crc_lowercase_eq_ws` (a space before the `=XXXX` line) | ok | ok | ok | ok | ok | ok |
| `i_blank_in_base64`, `d_blank_in_base64` (a blank line before the CRC line) | ok | ok | ok | ok | ok | ok |
| `i_comment_header`, `d_comment_header` | ok | ok | ok | ok | ok | ok |
| `i_badsig_goodfirst`, `i_badsig_badfirst`, `d_badsig_goodfirst`, `d_badsig_badfirst` (A's good signature plus A's signature over other text) | REJ | REJ | REJ | REJ | ok | REJ |
| `i_badsig_only` | REJ | REJ | REJ | REJ | REJ | REJ |
| `i_unknown_goodfirst`, `i_unknown_unknownfirst`, `d_unknown_goodfirst` (plus B's good signature) | ok | ok | ok | ok | ok | ok |
| `i_expired_good_plus_good`, `i_good_plus_expired_good` (E's good signature, made while E was valid, plus A's) | ok | ok | ok | ok | ok | ok |
| `i_expired_bad_plus_good` (E's signature over other text plus A's) | REJ | REJ | REJ | REJ | ok | REJ |
| `i_expired_only` | REJ | REJ | REJ | REJ | ok | REJ |
| `d_binary` (the good signature, de-armored) | REJ | REJ | REJ | REJ | REJ | REJ |
| `d_twoblocks` (two armor blocks, the second A's signature over other text) | REJ | REJ | REJ | REJ | ok | REJ |
| `i_dash_escaped_end` (a Release line `-----END PGP SIGNATURE-----`, dash-escaped) | REJ | REJ | REJ | REJ | REJ | REJ |

`i_dash_escaped_end` is rejected by apt's Release parser ("Unable to parse package file"), not by its signature check, so it says nothing about framing; keep it out of the verifier's table. `debian:13` verifies with Sequoia's `sqv`, which ignores the CRC (RFC 9580) and accepts a bad signature beside a good one; every gpgv-based apt rejects those.

**DN-3 — The rule (ruling HD-2).** The verifier rejects a shape when **any** supported apt rejects it, and accepts a shape **every** supported apt accepts, except for deviations listed, each with its reason, in the test table (`aptShapeDeviations`, Task 5). Why "any": a cache that accepts what some client rejects breaks that client's `apt update` for the suite until upstream publishes again (forever for a frozen pocket); a cache that rejects what every client accepts only keeps its previous snapshot, a staleness a legitimate repository never meets, since such a shape fails other clients too. Consequences:

| Rule | Inline (`InRelease`) | Detached (`Release.gpg`) |
|---|---|---|
| F1. The first line is the BEGIN marker (spaces, tabs and a CR may follow it) | unchanged (ruling R-D2) | **new**: no byte before it; a binary signature is rejected |
| F2. The first END line of the signature armor is the last line: only spaces, tabs, one CR and one LF may follow the marker, or nothing | **new** (was: any whitespace after END) | **new**; it also rejects two armor blocks |
| F3. A CRC line (`=` and four base64 characters, after trimming spaces, tabs and CR) present in the armor must equal the CRC-24 of the signature packets the cache decoded, and the line after it is the END line; a second line starting with `=` is rejected | **new** | **new** |
| S1. A signature packet whose issuer is a key in the host keyring must pass its cryptographic check (an expired or revoked key's packet that passes it is not a failure); a packet whose issuer the keyring lacks is ignored | **new** | **new** |
| S2. At least one packet passes the trust check and verifies | unchanged | unchanged |

F1–F3 are structural: they also apply under `accept_any_signer`'s bypass. S1 does not (design note 7).

**DN-4 — Errors and reasons.** Inline F1–F3 failures wrap `ErrMalformedClearsign` (reason `malformed_clearsign`, as today). Detached F1–F3 failures wrap a new `ErrMalformedSignature` = `errors.New("Release.gpg is not one ASCII-armored signature block")` (reason `malformed_signature`). S1 failures wrap a new `ErrBadSignature` = `errors.New("a signature from a known key failed verification")` (reason `bad_signature`). `ClassifyVerifyErr` gains both cases, before its `crypto_verify_failed` default. These reasons are what the adoption failure lines and `acu_adoption_total`'s outcome carry today for the other sentinels; check where the existing reasons are documented (`grep -rn "malformed_clearsign\|untrusted_signer" docs/`) and add the two new ones there.

**DN-5 — S1's "known" and "fails" (ruling HD-3).** go-crypto's `openpgp.VerifyDetachedSignature(keyring, signed, oneSignaturePacket, nil)` returns a non-nil `*packet.Signature` exactly when the cryptographic check passed — and then also reports `ErrKeyExpired`, `ErrSignatureExpired` or `ErrKeyRevoked` from `checkMessageSignatureDetails` (`openpgp/read.go:546–553`, v1.4.1); it returns a nil signature when the check failed, and `errors.ErrUnknownIssuer` when the keyring has no signing key with the packet's issuer key ID. So for each signature packet, re-serialized alone (as `verifyAnyTrusted` does today): known = the packet carries an issuer key ID and `v.keyring.EntityList().KeysByIdUsage(id, packet.KeyFlagSign)` is non-empty; failed = known and `VerifyDetachedSignature(v.keyring.EntityList(), …)` returned a nil signature. apt agrees (design note 2: `i_expired_good_plus_good` ok, `i_expired_bad_plus_good` REJ). The host keyring, not the per-suite trust set: a client that installed any of the cache's keys sees a BADSIG from it.

**DN-6 — Sub-job B retires; sub-job C deletes (ruling HD-4).** `RunSnapshotGCBatch` keeps its selection (sub-jobs A ∪ B, `gc.snapshot_batch_size` at a time) but, for the displaced snapshots (class `displaced`), stamps `retired_at` in one short `UPDATE` instead of deleting their rows; sub-job C (`RunRetiredSnapshotGCChunk`, 10,000 rows a transaction) then deletes them, in the same snapshot pass of a periodic tick (A ∪ B, then C) or at the retired run an hour after a start. Orphan candidates (class `orphan`, never adopted) are still deleted at once: their rows are inserted only by `CommitAdoption`, which rolls back as a whole, so an orphan holds a header and nothing else. Why retiring is safe: every reader filters `retired_at` (spec C9; the natural key's partial index `idx_suite_snapshot_natural_v7` is `WHERE retired_at IS NULL`; signed history `signedHistorySnapshotSQL`, retained by-hash `snapshot_reads.go:84` and the rollback check all read non-retired rows), so a retired displaced snapshot is invisible exactly as a deleted one is; and B's own selection already has `retired_at IS NULL`, so a batch never selects what it retired, and the loop ends. Consequences: the startup pass retires (fast) and deletes nothing (ruling PF-15 keeps C out of it), so the listeners wait less; `gc_run_complete`'s `displaced_reaped` counts displaced snapshots retired, and their deletion is counted again in `retired_reaped` when C deletes them.

**DN-7 — Not changed, deliberately.** `adoption.accept_any_signer`'s bypass verifies no signature, so S1 is not applied there (it would be the only signature check in that mode; recorded *Open*). The handler's sniff of a fetched `Release.gpg` (`aptpath.SniffReleaseGPG`) still accepts a binary signature as a signed anchor: its marker only makes the suite never adopt unsigned, the fail-safe direction. The upgrade's trust audit classifies 1.0.x snapshots with its own structural classifier (`classifyLegacyInline`), not the verifier, so the stricter verifier quarantines nothing new at the upgrade.

**DN-8 — The download cap (ruling HD-5).** Two changes. (a) The upstream transport sets `DisableCompression: true`: Go then sends no `Accept-Encoding: gzip` and decodes nothing, so a response's `Content-Length` is what lands on disk (VA-3: a decoded gzip body had length −1 and skipped the size check; VA-4: a resumed download appended compressed bytes to a decoded prefix). The cache serves pool bytes without a `Content-Encoding` header, and nothing in the tree relies on transparent gzip (the review's finding). (b) `fetch.Target` gains `MaxBytes`: a response whose declared length exceeds it fails before its body is read, and a body that runs past it fails as soon as it does, with `ErrBodyTooLarge`, never retried, outcome `body_too_large` (the label `Conditional` already uses). Adoption sets it to the member's declared size for both the by-hash probe and the plain-path fetch (`memberMaxBytes`: one byte for a member declared empty, since 0 means no limit). This bounds a signed member exactly and an unsigned member by what its `Release` declares; the request path's and hot prefetch's downloads keep only `Content-Length` and `upstream.total_timeout` (DN-1).

**DN-9 — Strict mode, vouching lost during the download (ruling HD-6).** `prefetchCheck` reports `vouched` when its read (or `prefetchRecheck`'s) found the path vouched; when `runFetch`'s read after the download finds no row for a `.deb` that was vouched before it, strict mode decides there (`strictAfterDownload`), as it already does when the read before the download failed: refused on a strict host (or, with a signed adoption in flight on the host, the miss waits and retries once), served unchecked as today elsewhere.

---

### Task 1: A GC pass that fails at every tick no longer starves the passes after it

**Files:**
- Modify: `internal/gc/gc.go` (`skipPanickedPass`, `runTick`, the `nextPass` field comment)
- Test: `internal/gc/gc_store_test.go`
- Modify: `Changelog.md`

**Interfaces:**
- Consumes: `fakeStore`, `storeGC`, `gcEvents`, `captureLogger` (existing, `internal/gc` tests).
- Produces: `func (g *GC) skipPass()`; `func (g *GC) passFailed(ctx context.Context, err error) error`.

- [ ] **Step 1: Extend `fakeStore`** with an error mode beside its panic mode:

```go
	batchErrs   int    // RunSnapshotGCBatch fails on this many first calls (after batchPanics)
	batchHook   func() // called at the start of every RunSnapshotGCBatch call
```

and in `RunSnapshotGCBatch`, after the panic check:

```go
	if f.batchHook != nil {
		f.batchHook()
	}
	if n <= f.batchPanics+f.batchErrs {
		return cache.SnapshotGCBatchResult{}, errors.New("snapshot batch fault")
	}
```

(keep the panic check first; adjust the counting so `batchPanics` alone behaves exactly as today).

- [ ] **Step 2: Write the failing tests**

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

// A pass that fails because the GC's context ended (shutdown) keeps the
// rotation where it is: the pass did not fail, it was stopped.
func TestPeriodicTick_AnEndedContextKeepsTheRotation(t *testing.T) {
	logger, _ := captureLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeStore{batchErrs: 1000, batchHook: cancel}
	g, _ := storeGC(t, f, 2, logger)
	g.periodicTick(ctx)
	if g.nextPass != snapshotPass {
		t.Fatalf("after a pass stopped by its context the rotation is at pass %d, want the snapshot pass (%d)", g.nextPass, snapshotPass)
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

- [ ] **Step 3: Run them; expect the first and third to fail** (`go test ./internal/gc -run 'AFailingPass|AnEndedContext' -v`): the rotation stays at the snapshot pass.

- [ ] **Step 4: Implement.** In `internal/gc/gc.go`, split the rotation step out of `skipPanickedPass`, add `passFailed`, and use it at each pass's error return in `runTick`:

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
// its tables) was retried first at every tick and the passes after it —
// the blob pass, which reclaims disk — never ran; the only sign was WARN
// gc_tick_failed per tick. The failing pass now runs every third tick.
func (g *GC) passFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		g.skipPass()
	}
	return err
}
```

`skipPanickedPass` keeps its early return and then calls `g.skipPass()`. In `runTick`, each of the three `if err != nil { return res, err }` after a pass becomes `return res, g.passFailed(ctx, err)` (at each, `g.nextPass` is the pass that failed). Update the `nextPass` field's comment: "A pass that panicked or failed moves it too, past that pass (skipPanickedPass, passFailed)".

- [ ] **Step 5: Run the package's tests** (`go test -race ./internal/gc`); all pass, including `TestPeriodicTick_APanickingPassDoesNotStarveTheNext` and `TestSkipPanickedPass_MovesPastEachPass`.

- [ ] **Step 6: Mutants.** (a) `passFailed` never calls `skipPass` → the first and third tests fail. (b) `passFailed` ignores `ctx` → the second fails. Restore.

- [ ] **Step 7: Changelog** — under *Fixed*: `- A GC pass that fails at every tick no longer keeps the passes after it, the blob pass included, from running.`

- [ ] **Step 8: Commit** `internal/gc/gc.go internal/gc/gc_store_test.go Changelog.md`: "GC: a pass that fails moves the rotation on, as a panic does (H3 final review R3 #5)".

### Task 2: Sub-job B retires the displaced snapshots; sub-job C deletes them in chunks

**Files:**
- Modify: `internal/cache/gc.go` (`RunSnapshotGCBatch` and its doc comment, `RunRetiredSnapshotGCChunk`'s AIDEV-NOTE)
- Modify: `internal/gc/snapshot.go` (`runSnapshotPass`'s doc comment and AIDEV-NOTE)
- Test: `internal/cache/gc_test.go`, `internal/gc/integration_test.go` (or the file holding the gc package's real-cache snapshot tests), and any test that expected B to delete rows
- Modify: `docs/configuration.md` (the `[gc]` table's `snapshot_batch_size` and `keep_displaced` rows), `docs/admin-ui-spec.md` (`gc.displaced_reaped` and `gc.retired_reaped`), `docs/log-fields.md` (`gc_run_complete`'s `displaced_reaped`/`retired_reaped`, if described), `Changelog.md`

**Interfaces:**
- Consumes: `RunRetiredSnapshotGCChunk(ctx, 0)` (unchanged).
- Produces: `RunSnapshotGCBatch` with the same signature and result type; `DisplacedReaped` now counts displaced snapshots retired.

- [ ] **Step 1: Pre-flight.** Confirm the facts DN-6 rests on, each with its expected output:
  - `grep -n "INSERT INTO snapshot_member\|INSERT INTO snapshot_skipped_member\|INSERT INTO package_hash" internal/cache/*.go | grep -v _test` lists only `adoption.go` (inside `CommitAdoption`), `reconcile.go` and `skipped_member.go` (both act on adopted snapshots). If another writer inserts child rows for an unadopted snapshot, stop and report.
  - `grep -n "retired_at IS NULL" internal/cache/*.go | grep -v _test` includes `signed_seen.go` (`signedHistorySnapshotSQL`), `snapshot_reads.go`, `adoption.go` (the natural-key lookup) and `gc.go` (B's selection). Find the rollback check's read (`grep -rn "rollback_refused" internal --include=*.go | grep -v _test`) and confirm it reads non-retired snapshots only; if it does not, stop and report.

- [ ] **Step 2: Write the failing tests** in `internal/cache/gc_test.go`, reusing its seeding helpers (a suite with a current snapshot and displaced ones, each with `snapshot_member`, `snapshot_skipped_member` and `package_hash` rows):
  - `TestRunSnapshotGCBatch_RetiresTheDisplacedBeyondKeep`: one current and five displaced snapshots, `keepDisplaced = 3`, batch size 10. The batch returns `DisplacedReaped == 2`; the two oldest displaced have `retired_at` set (= the batch's time, within a second of now) and **all** their child rows are still there (counted per table); the other snapshots are untouched. A second batch returns `Total() == 0`. Then `RunRetiredSnapshotGCChunk(ctx, 0)` until `more == false` deletes the two retired snapshots' rows and headers and nothing else.
  - `TestRunSnapshotGCBatch_StillDeletesOrphanCandidates`: an orphan candidate (unadopted, heartbeat older than the grace, not current) is deleted by the batch, header gone, `OrphanReaped == 1` (if an existing test already pins this, name it in the report instead).
  - `TestRunSnapshotGCBatch_ARetiredDisplacedSnapshotIsNoLongerRead`: after the batch, the reads DN-6 names (signed history for the suite's base, retained by-hash's displaced read, the rollback check) see the kept displaced snapshots and not the retired ones — the same answers as after a deletion.
  In `internal/gc` (real cache): `TestStartupPass_RetiresDisplacedAndLeavesTheirRowsToSubJobC` (after `StartupPass`, the displaced snapshot beyond keep is retired and its rows remain) and `TestPeriodicTick_DeletesTheDisplacedSnapshotsItRetires` (one periodic tick retires and deletes them; `gc_run_complete` shows `displaced_reaped` and `retired_reaped` both counting them).

- [ ] **Step 3: Run them; the first and the gc ones fail** (B deletes the rows at once).

- [ ] **Step 4: Implement.** In `RunSnapshotGCBatch`, after the SELECT, split the ids by class; delete the orphans with the existing cascade (placeholders for the orphan ids only); retire the displaced:

```go
		if len(displaced) > 0 {
			// AIDEV-NOTE: ruling HD-4 (the H3 final review R3 #4). B used to
			// delete a displaced snapshot's rows here, in this one writer
			// transaction: six snapshots of 70,000 package_hash rows held the
			// writer 4.7 s in a probe, so a full batch held every write (a
			// miss's store, a flip's commit) about 8 s. B now only retires
			// them, and sub-job C deletes them in chunks (retiredChunkRows a
			// transaction). Every reader filters retired_at, so a retired
			// displaced snapshot is invisible exactly as a deleted one was,
			// and B's selection (retired_at IS NULL) never selects it again.
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

(`placeholders(n)` builds `?,?,…`; reuse the loop that builds `idList` today or add a small helper beside it.) Update the function's doc comment (steps 3–4: orphans cascade-deleted, displaced retired), `runSnapshotPass`'s comment ("Each batch … cascade DELETE" → retire for B), and `RunRetiredSnapshotGCChunk`'s AIDEV-NOTE (its callers now delete B's displaced snapshots too).

- [ ] **Step 5: Run the whole suite** (`go test -race ./...`). Tests that asserted B's deletion directly now see retired rows: change each to run sub-job C (or a periodic tick) before asserting the deletion, or to assert the retirement — never weaken an assertion about what clients are served. List every test changed, with why, in the report.

- [ ] **Step 6: Probe (recorded, not committed).** In a scratch test, seed six displaced snapshots of 70,000 `package_hash` rows each beyond `keep_displaced`, time `RunSnapshotGCBatch` and each `RunRetiredSnapshotGCChunk` call, and record the numbers in the report (expected: the batch in milliseconds, each chunk under 200 ms).

- [ ] **Step 7: Mutant.** Restore the deletion of displaced rows in the batch → `TestRunSnapshotGCBatch_RetiresTheDisplacedBeyondKeep` fails. Restore.

- [ ] **Step 8: Docs.** `docs/configuration.md`, `snapshot_batch_size`: "Maximum snapshots one GC batch removes: displaced snapshots beyond `keep_displaced` are retired, and their rows are then deleted in chunks of 10,000 rows a transaction, so a large suite never holds the database for long. At least `1`." `keep_displaced`: keep the row, adding nothing unless it says displaced snapshots are deleted at once. `docs/admin-ui-spec.md`: `gc.displaced_reaped` "Displaced snapshots beyond `gc.keep_displaced` the last GC run retired; their rows are deleted with the retired snapshots (`retired_reaped`), normally in the same run"; `gc.retired_reaped` gains "and the displaced snapshots retired beyond `gc.keep_displaced`". Grep `docs/log-fields.md` for `displaced_reaped` and align it. Check each claim against the code (Step 1's greps and this task's tests).

- [ ] **Step 9: Changelog** — under *Changed*: `- GC deletes old snapshots in bounded chunks: the displaced snapshots beyond \`gc.keep_displaced\` are retired and then deleted 10,000 rows a transaction, so a large suite no longer holds every database write for seconds.`

- [ ] **Step 10: Commit** the changed files: "GC: sub-job B retires displaced snapshots and sub-job C deletes them in chunks (H3 final review R3 #4)".

### Task 3: No transparent gzip; adoption stops a member's download at its declared size

**Files:**
- Modify: `internal/fetch/dialer.go` (`newTransport`), `internal/fetch/fetch.go` (`Target`, `ErrBodyTooLarge`, `doAttempt`, `streamBody`, `isRetryable`), `internal/fetch/classify.go`
- Modify: `internal/freshness/adoption.go` (`tryByHashFetch`, `fetchMemberURL`, new `memberMaxBytes`)
- Test: `internal/fetch/fetch_test.go`, `internal/fetch/classify_test.go`, a `internal/freshness` adoption test file (the one with member-fetch tests)
- Modify: `docs/configuration.md` (the unsigned threat note at "but not in bytes"), the doc that lists `acu_fetch_total`'s outcomes (`grep -rn "body_too_large" docs/ e2e/ internal --include=*.md`), `Changelog.md`

**Interfaces:**
- Produces: `fetch.Target.MaxBytes int64`; `fetch.ErrBodyTooLarge`; `func memberMaxBytes(m ReleaseMember) int64`.

- [ ] **Step 1: Write the failing fetch tests** (httptest servers through `newTestClient`):
  - `TestFetch_SendsNoAcceptEncoding`: the server records `r.Header.Values("Accept-Encoding")` and answers gzip only when asked; after a fetch, no value was sent and `dst` holds the identity body.
  - `TestFetch_StoresAGzipBodyItDidNotAskFor`: the server always sends `Content-Encoding: gzip` and a gzip body; `dst` holds the compressed bytes, unchanged (nothing is decoded in transit).
  - `TestFetch_MaxBytesStopsAChunkedBodyThatRunsOver`: a chunked 200 (flush after each 4 KiB, 1 MiB in all), `MaxBytes: 10000`: `errors.Is(err, ErrBodyTooLarge)`, `dst.Written() <= 10001`, and the server saw one request (not retried).
  - `TestFetch_MaxBytesRefusesADeclaredLengthOverIt`: `Content-Length: 50000`, `MaxBytes: 10000`: `ErrBodyTooLarge` with `dst.Written() == 0`.
  - `TestFetch_MaxBytesAcceptsABodyOfExactlyIt`: 10,000 bytes, with and without `Content-Length`, `MaxBytes: 10000`: success.
  - `TestFetch_MaxBytesBoundsAResumedDownload`: a chunked first 200 with an `ETag`, cut after 5,000 bytes; the resume's 206 declares `bytes 5000-49999/50000`; `MaxBytes: 10000`: `ErrBodyTooLarge`.
  - `TestFetch_ZeroMaxBytesIsNoLimit`: a 100,000-byte chunked body with `MaxBytes` 0 succeeds (the existing tests cover this too; keep this one explicit).
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
		// them, never with a Content-Encoding, so it must not ask for any.
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

New sentinel beside the others: `ErrBodyTooLarge = errors.New("fetch: response body exceeds the size expected")`. `doAttempt`, case 200, before the truncate: `if target.MaxBytes > 0 && resp.ContentLength > target.MaxBytes { return out, fmt.Errorf("%w: declared %d bytes, at most %d expected", ErrBodyTooLarge, resp.ContentLength, target.MaxBytes) }`; case 206, after the existing total checks: `if target.MaxBytes > 0 && (total > target.MaxBytes || last >= target.MaxBytes) { return out, fmt.Errorf("%w: …", ErrBodyTooLarge, …) }`. `streamBody` takes the limit:

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

Both call sites pass `target.MaxBytes`. `isRetryable`: `if errors.Is(err, ErrBodyTooLarge) { return false }` beside `ErrCacheWriteFailed`'s. `classifyError`: `case errors.Is(err, ErrBodyTooLarge): return "body_too_large"` in the switch that holds `ErrSizeMismatch`, before it; update the function's row comment.

- [ ] **Step 4: Run the fetch tests**; all pass, the existing ones included.

- [ ] **Step 5: Write the failing adoption tests.** With the freshness tests' fake fetcher (record each `*fetch.Target`): `TestAdoptMember_LimitsTheDownloadToTheDeclaredSize` — a member of declared size 1234 is fetched with `MaxBytes == 1234` on the by-hash probe and on the plain path (make the probe fail so both run); a member declared size 0 with `MaxBytes == 1`. With a real `fetch.Client` and an httptest upstream that serves a member chunked and longer than declared: `TestAdoptMember_AMemberLongerThanDeclaredStopsAtItsSize` — the member fails (`ErrAdoptionMemberFetchFailed`), no pool file and no temp file remain for it, and the upstream handler observed a write error before it had sent twice the declared size.

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

Set `MaxBytes: memberMaxBytes(m)` in the `fetch.Target` of `tryByHashFetch` and of `fetchMemberURL`.

- [ ] **Step 7: Run** `go test -race ./internal/fetch ./internal/freshness`; all pass.

- [ ] **Step 8: Mutants.** (a) Drop `DisableCompression` → `TestFetch_SendsNoAcceptEncoding` and `TestFetch_StoresAGzipBodyItDidNotAskFor` fail. (b) `streamBody` ignores `max` → the chunked test fails. (c) Drop the declared-length check → `TestFetch_MaxBytesRefusesADeclaredLengthOverIt` fails (`Written` > 0). (d) Retry `ErrBodyTooLarge` → the chunked test's one-request assertion fails. (e) Drop `MaxBytes` from `fetchMemberURL` → both adoption tests fail. Restore each.

- [ ] **Step 9: Docs.** `docs/configuration.md`, the unsigned threat note: replace "but not in bytes: each file can be as large as its server sends within `upstream.total_timeout` (no response-body ceiling, and gzip is decompressed in transit: the 2026-09 security review's A-2 and VA-3, open)," with "and in bytes by the sizes its own `Release` declares: each file's download stops at its declared size, and nothing is decompressed in transit (the 2026-09 security review's A-2 and VA-3), but an unsigned `Release` declares what its server wants,". Read the paragraph whole afterwards: the sentence must still parse. Add `body_too_large` to wherever `acu_fetch_total`'s outcomes are listed for `Fetch` (it is already listed for conditional fetches).

- [ ] **Step 10: Changelog** — under *Fixed*: `- Upstream downloads are no longer decompressed in transit (a gzip-encoded body skipped the size check, and a resumed one could be corrupted), and adoption stops a member's download at the size its \`Release\` declares.`

- [ ] **Step 11: Commit** the changed files: "Fetch: no transparent gzip; adoption caps each member's download at its declared size (security review A-2, VA-3, VA-4)".

### Task 4: Strict mode refuses a `.deb` whose vouching ends during its download

**Files:**
- Modify: `internal/handler/handler.go` (`prefetchCheck`, `prefetchRecheck`, `runFetch`, `strictAfterDownload`'s comment)
- Test: `internal/handler/strict_wait_test.go`
- Modify: `Changelog.md`

**Interfaces:**
- Consumes: `strictWaitRig`, `commitVouching`, `fakeAdoptions`, `firstFetchHook` (existing handler tests).
- Produces: `prefetchCheck(ctx, req) (res sfResult, end, unread, vouched bool)`; `prefetchRecheck(ctx, req, count) (res sfResult, end, vouched bool)`.

- [ ] **Step 1: Write the failing test** `TestServeHTTP_StrictMissDecidesWhenVouchingEndsDuringTheDownload`, table-driven like `TestServeHTTP_StrictMissDecidesAfterAFailedPrefetchRead`: the `.deb` is vouched by a current snapshot before the request; a fetch hook that runs **after** the upstream body is written (extend `firstFetchHook` with an `after func()`, or add a sibling hook type) ends the vouching — retire the vouching snapshot the way a reap or forget does (find the cache call: `grep -n "func (c \*Cache) \(Forget\|Reap\|Retire\)" internal/cache/*.go`), or commit a new current snapshot that does not list the path. Rows:
  - strict host, nothing in flight → 502, `Retry-After: 60`, one `unvouched_deb_refused`, the file not served;
  - strict host, a signed adoption in flight that commits rows vouching the same bytes while the miss waits → 200 with the body after one wait;
  - a host with strict mode off → 200, the body served (unchanged behaviour).

- [ ] **Step 2: Run it; the strict rows fail** (the file is served: today the read after the download finds no row, and strict mode is decided there only after a failed read before it).

- [ ] **Step 3: Implement.** `prefetchCheck` returns `vouched = true` when its read found exactly one distinct declared hash, and passes on `prefetchRecheck`'s `vouched` (true when the second look found rows). In `runFetch`, keep `prefetchVouched` beside `prefetchUnread`, and in the read after the download, case 0:

```go
				// Final review R4 #2 and ruling HD-6: strict mode's decision is
				// made here when the check before the download could not read
				// the vouching hashes, or when it found the path vouched and the
				// vouching is gone now (its snapshot retired or displaced by one
				// that does not list it, during the download) — a .deb that was
				// vouched is never served unchecked on a strict host.
				if (prefetchUnread || prefetchVouched) && isDebPath(req.Path) {
```

Update `prefetchCheck`'s and `strictAfterDownload`'s doc comments to name both callers' cases.

- [ ] **Step 4: Run** `go test -race ./internal/handler`; all pass.

- [ ] **Step 5: Mutant.** Drop `|| prefetchVouched` → the strict rows fail. Restore.

- [ ] **Step 6: Changelog** — under *Fixed*: `- With \`integrity.refuse_unvouched_debs\`, a \`.deb\` whose vouching snapshot is retired or replaced while it downloads is refused like any unvouched \`.deb\`, instead of being served unchecked.`

- [ ] **Step 7: Commit** the changed files: "Strict mode: a .deb whose vouching ends during its download is decided after it (H3 final review, R6 re-review)".

### Task 5: The verifier rejects the anchor shapes apt rejects: framing and CRC, both forms

**Files:**
- Create: `internal/gpg/armor_shape.go` (the F1–F3 checks and CRC-24), `internal/gpg/aptshape_test.go` (fixture builders, the measured table, the table test)
- Modify: `internal/gpg/verifier.go` (`VerifyInline`, `VerifyDetached`, `requireBareClearsigned`, `decodeMaybeArmoredSignature`, `isArmoredSignature`, `ClassifyVerifyErr`, the sentinels, the AIDEV-NOTEs that list these shapes as open), existing tests that pin the old behaviour (a binary `Release.gpg` accepted, whitespace after END accepted)
- Modify: the doc listing the verification reasons (DN-4), `docs/configuration.md` if it says a binary `Release.gpg` is accepted (`grep -n "binary" docs/configuration.md`), `Changelog.md`

**Interfaces:**
- Produces: `ErrMalformedSignature`; `aptShape` fixtures `func aptShapes(t *testing.T) []aptShape` with `type aptShape struct { name string; inline []byte; release, sig []byte; keys openpgp.EntityList }`; `aptVerdicts map[string][6]bool` (true = that apt accepts; the image order of DN-2); `aptShapeDeviations map[string]string` (shape → why the cache's verdict differs from DN-3's rule). Task 6 adds rows to the same table; Task 7 writes the same fixtures to disk.

- [ ] **Step 1: Write the fixture builders and the measured table.** In `aptshape_test.go`, build every DN-2 shape except the S1 rows (`*_badsig_*`, `i_badsig_only`, `*_unknown_*`, `i_expired_*`, which Task 6 adds) and `i_dash_escaped_end`, with go-crypto (`newTestEntity`, `clearsignWith`, `detachSignWith`, `assembleClearsignedBlock`, and an armor writer that can write a forged, missing or misplaced CRC line, headers and blank lines). Name each fixture exactly as DN-2 does. The table `aptVerdicts` holds DN-2's verdicts for those rows (copy them from `…/scratchpad/apt-oracle/table.txt`). Add a CRC-24 test against RFC 4880's check value (`crc24([]byte("123456789")) == 0x21CF02`).

- [ ] **Step 2: Write the table test** `TestVerifier_AcceptsWhatEveryAptAccepts`: for each shape, run `VerifyInline` (or `VerifyDetached`) with a verifier whose host keyring holds the shape's keys, default options (`require_signature = true`, no pins, `allow_short_keyid = true`); want accepted iff every apt accepts (DN-3), unless the shape is in `aptShapeDeviations`, in which case want the opposite and the deviation's reason is non-empty. Also a test that every key of `aptShapeDeviations` names a shape in the table (no stale deviation). Run it: the F2/F3 rows and the detached F1 rows fail (they are accepted today).

- [ ] **Step 3: Implement `armor_shape.go`.**

```go
// Package-level apt-shape checks (spec: docs/open-decisions.md, "The inline
// verifier accepts three shapes apt rejects"; ruling HD-2). Each rule is
// what apt 2.2–3.2 enforce on an InRelease or Release.gpg (the measured table
// in docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md, DN-2;
// `make e2e-apt-oracle` re-measures it).

const (
	sigArmorBegin = "-----BEGIN PGP SIGNATURE-----"
	sigArmorEnd   = "-----END PGP SIGNATURE-----"
)

// isMarkerLine reports whether line (without its LF) is marker followed only
// by spaces, tabs and one trailing CR — apt's reading of a marker line.
func isMarkerLine(line []byte, marker string) bool

// crc24 is OpenPGP's armor checksum (RFC 4880 §6.1): init 0xB704CE,
// polynomial 0x1864CFB.
func crc24(b []byte) uint32

// checkSignatureArmor applies F2 and F3 to the signature armor that starts
// at the line `begin` of b (the index of its BEGIN line) and returns the
// index just past its END line. decoded is what go-crypto decoded from the
// armor (the signature packets). Errors are plain (the callers wrap them in
// their form's sentinel).
func checkSignatureArmor(b []byte, begin int, decoded []byte) error
```

Rules for `checkSignatureArmor`, from DN-3: scan the lines after the BEGIN line; the first line that `isMarkerLine(…, sigArmorEnd)` is the END line; there must be one (else error). After the END line's LF there is nothing; or the END line ends at EOF. Among the lines between BEGIN and END, a line whose trimmed form (spaces, tabs, CR) starts with `=` is a CRC line: at most one; if there is one, it is the line right before END, its trimmed form is `=` plus four base64 characters that decode to three bytes, and that 24-bit value equals `crc24(decoded)`. Keep it a pure function with no go-crypto calls, so it is cheap to test line by line.

- [ ] **Step 4: Wire it in.**
  - Inline: `requireBareClearsigned` keeps its F1 check (`aptpath.SniffInRelease`) and drops the whitespace-after-END rule (F2 moves to `checkSignatureArmor`). In `VerifyInline`, after `readArmoredSignature` (on both the bypass and the trusted path), locate the signature armor's BEGIN line in `inRelease` — the first line after the signed text that `isMarkerLine(…, sigArmorBegin)` (dash-escaping guarantees no plaintext line is one) — and call `checkSignatureArmor(inRelease, begin, sigBytes)`; wrap a failure as `fmt.Errorf("%w: %v", ErrMalformedClearsign, err)`. A body with the BEGIN marker but no END line keeps today's error from the decoder.
  - Detached: replace `isArmoredSignature`'s whitespace skip: the signature must start at byte 0 with a line that `isMarkerLine(…, sigArmorBegin)` (F1); anything else — a binary signature included — is `fmt.Errorf("%w: …", ErrMalformedSignature)`, on the bypass path too. After `armor.Decode` and the size cap, call `checkSignatureArmor(sigBytes, 0, out)`; wrap a failure in `ErrMalformedSignature`. Remove the "binary Release.gpg also exists in the wild; both forms are accepted" text from `VerifyDetached`'s comment and say what is enforced now.
  - `ClassifyVerifyErr`: `case errors.Is(err, ErrMalformedSignature): return "malformed_signature"`.
  - Rewrite the two AIDEV-NOTEs that list VC-2, C-4 and VC-1 as open (`VerifyDetached`'s and `requireBareClearsigned`'s): VC-2 and C-4 are enforced (name the rules), VC-1 and C-3 remain (Task 6 closes VC-1); keep the R-D2 history.

- [ ] **Step 5: Run** `go test -race ./internal/gpg`. Fix the existing tests that pinned the old behaviour (a binary `Release.gpg` adopted; whitespace lines after END accepted): each now expects rejection with its sentinel; list them in the report. If a DN-2 control row fails (the cache rejects a shape every apt accepts), first try to accept it; if go-crypto itself cannot decode it (a decoder limit, not a rule of ours), add it to `aptShapeDeviations` with that reason, and report it.

- [ ] **Step 6: Mutants.** (a) Allow a blank line after END → `i_trail_lf`/`d_trail_lf` rows fail. (b) Skip the CRC comparison → `*_crc_forged` fail. (c) Allow a line between CRC and END → `*_line_after_crc` fail. (d) Accept a binary signature → `d_binary` fails. (e) Reject a trailing CR on the END line → `*_end_crlf` fail. Restore each.

- [ ] **Step 7: Run the freshness and handler packages** (`go test -race ./internal/freshness ./internal/handler`): adoption tests that build their anchors with the helpers must still pass; any that wrote whitespace after END or a binary `Release.gpg` now see a refusal — fix the fixture (never the rule) and list it.

- [ ] **Step 8: Docs and Changelog.** Add `malformed_signature` beside the existing reasons (DN-4's grep). If `docs/configuration.md` says binary `Release.gpg` is accepted, say it is refused. Changelog under *Fixed*: `- Adoption refuses an \`InRelease\` or \`Release.gpg\` that apt rejects: a line after the signature, a forged armor checksum, a \`Release.gpg\` with bytes around its armor, two armor blocks, or a binary signature (apt 2.2 to 3.2 reject each; the cache adopted them and its clients' \`apt update\` then failed).`

- [ ] **Step 9: Commit** the changed files: "Verifier: framing and armor CRC as apt checks them, inline and detached (security review VC-2, C-4)".

### Task 6: Every signature from a known key must verify

**Files:**
- Modify: `internal/gpg/verifier.go` (new `requireKnownSignaturesVerify`, called by `VerifyInline` and `VerifyDetached` on the trusted path; `ErrBadSignature`; `ClassifyVerifyErr`; the AIDEV-NOTEs)
- Test: `internal/gpg/aptshape_test.go` (the S1 rows), `internal/gpg/verifier_test.go` / `verifier_detached_test.go` (focused tests)
- Modify: the reasons doc (DN-4), `Changelog.md`

**Interfaces:**
- Consumes: Task 5's `aptShape`, `aptVerdicts`, table test; `newExpiredEntity`.
- Produces: `ErrBadSignature`; `func (v *Verifier) requireKnownSignaturesVerify(signed, sigBytes []byte) error`.

- [ ] **Step 1: Add the S1 rows** to the fixture builders and `aptVerdicts`: `i_badsig_goodfirst`, `i_badsig_badfirst`, `i_badsig_only`, `d_badsig_goodfirst`, `d_badsig_badfirst`, `i_unknown_goodfirst`, `i_unknown_unknownfirst`, `d_unknown_goodfirst`, `i_expired_good_plus_good`, `i_good_plus_expired_good`, `i_expired_bad_plus_good`, `i_expired_only` — key A trusted; B not in the keyring; E in the keyring, expired, its signatures made (go-crypto `packet.Config.Time`) while it was valid. Run the table test: the `badsig` rows with a good signature and `i_expired_bad_plus_good` fail (accepted today).

- [ ] **Step 2: Focused tests** (`verifier_test.go`): `TestVerifyInline_ABadSignatureFromAKnownKeyRejects` (both orders; `errors.Is(err, ErrBadSignature)`; `ClassifyVerifyErr` → `bad_signature`), `TestVerifyInline_AKnownKeyOutsideThePinsStillCounts` (a pin for host H names key A only; the keyring also holds key C; A's good signature plus C's bad one → `ErrBadSignature`), `TestVerifyInline_AnExpiredKeysGoodSignatureBesideAGoodOneAdopts`, and the detached twins of the first two.

- [ ] **Step 3: Implement.**

```go
// requireKnownSignaturesVerify applies apt's rule that a bad signature from a
// known key is fatal (VC-1; ruling HD-3): every signature packet whose issuer
// key ID names a signing key in the host keyring must pass its cryptographic
// check against the host keyring. A packet whose check passes but whose key
// or signature is expired or revoked is not a failure (gpgv's EXPKEYSIG and
// REVKEYSIG, which apt ignores beside a good signature), and a packet whose
// issuer the keyring lacks is ignored (gpgv's NO_PUBKEY).
//
// AIDEV-NOTE: openpgp.VerifyDetachedSignature returns a non-nil signature
// exactly when the cryptographic check passed, then reports expiry or
// revocation as its error (go-crypto v1.4.1 openpgp/read.go,
// verifyDetachedSignature and checkMessageSignatureDetails), so "failed" is
// a nil signature, never a non-nil error. Re-check this if go-crypto is
// upgraded (TestVerifyInline_AnExpiredKeysGoodSignatureBesideAGoodOneAdopts
// pins it). The host keyring, not the suite's trust set: a client that
// installed any of the cache's keys sees the BADSIG.
func (v *Verifier) requireKnownSignaturesVerify(signed, sigBytes []byte) error {
	keyring := v.keyring.EntityList()
	reader := packet.NewReader(bytes.NewReader(sigBytes))
	for {
		pkt, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read signature packet: %w", err)
		}
		sig, ok := pkt.(*packet.Signature)
		if !ok || sig.IssuerKeyId == nil || len(keyring.KeysByIdUsage(*sig.IssuerKeyId, packet.KeyFlagSign)) == 0 {
			continue
		}
		var one bytes.Buffer
		if err := sig.Serialize(&one); err != nil {
			return fmt.Errorf("re-serialize signature packet: %w", err)
		}
		if ok, _, _ := openpgp.VerifyDetachedSignature(keyring, bytes.NewReader(signed), &one, nil); ok == nil {
			return fmt.Errorf("%w: issuer %016X", ErrBadSignature, *sig.IssuerKeyId)
		}
	}
}
```

(name the first result something other than `ok`, which already names the type assertion above). Call it in `VerifyInline` and `VerifyDetached` on the trusted path only — after `len(trustSet) == 0`'s check and before `verifyAnyTrusted` — never on the bypass path (DN-7). `ClassifyVerifyErr`: `case errors.Is(err, ErrBadSignature): return "bad_signature"`. Update the AIDEV-NOTEs Task 5 rewrote: VC-1 is enforced; C-3 stays open.

- [ ] **Step 4: Run** `go test -race ./internal/gpg ./internal/freshness`; all pass.

- [ ] **Step 5: Mutants.** (a) Treat a non-nil error as a failure → `i_expired_good_plus_good` and `i_good_plus_expired_good` fail. (b) Use the trust set instead of the host keyring → `TestVerifyInline_AKnownKeyOutsideThePinsStillCounts` fails. (c) Skip the call in `VerifyDetached` → `d_badsig_*` fail. Restore each.

- [ ] **Step 6: Docs and Changelog.** `bad_signature` beside the reasons (DN-4). Changelog under *Fixed*: `- Adoption refuses a signed anchor that carries a bad signature from a key the cache knows beside a good one, as apt does.`

- [ ] **Step 7: Commit**: "Verifier: a bad signature from a known key rejects, as apt's BADSIG does (security review VC-1)".

### Task 7: `make e2e-apt-oracle` — the verifier's fixtures through real apt

**Files:**
- Modify: `internal/gpg/aptshape_test.go` (`TestWriteAptOracleFixtures`)
- Create: `e2e/apt-oracle/run.sh`, `e2e/apt-oracle/in-container.sh`, `e2e/apt-oracle/README.md`
- Modify: `Makefile` (target `e2e-apt-oracle`, beside `e2e-live`), `docs/open-decisions.md` (the pre-RC list: run it on the tree you tag), `Changelog.md`

**Interfaces:**
- Consumes: Tasks 5–6's `aptShapes`, `aptVerdicts`, `aptShapeDeviations` and the verifier.
- Produces: `make e2e-apt-oracle` (manual, like `make e2e-live`; not in CI).

- [ ] **Step 1: The fixture writer.** `TestWriteAptOracleFixtures` skips unless `ACU_APT_ORACLE_DIR` is set; then, for each shape, writes `<dir>/<name>/` as a flat repository (`Packages` empty; `Release` with its SHA256 line; `InRelease`, or `Release` and `Release.gpg`), the client keyring `<dir>/keys.gpg` (binary export of every key the shapes trust or know: A and E, never B), and `<dir>/expected.txt`, one line per shape: `<name> <cache verdict: ok|REJ> <the six recorded apt verdicts in DN-2's image order>`. The cache verdict is computed by running the verifier on the shape, not copied from the table.

- [ ] **Step 2: The runner.** `e2e/apt-oracle/in-container.sh` is the controller's `…/scratchpad/apt-oracle/in-container.sh` with the key file name `keys.gpg` (bash, `set -euo pipefail` except around `apt-get update`, whose exit status it records). `e2e/apt-oracle/run.sh`: makes a temp dir (removed on exit), runs `ACU_APT_ORACLE_DIR=<dir> go test ./internal/gpg -run '^TestWriteAptOracleFixtures$' -count=1`, copies `in-container.sh` in, and for each image in `${E2E_APT_ORACLE_IMAGES:-debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04}` runs `docker run --rm --network none -v "<dir>:/fx:ro" "<image>" bash /fx/in-container.sh` (bounded: `timeout 600`). It prints one table (shape × image) and fails when, for any shape: the cache accepts and some apt rejects (**unsafe**); an apt verdict differs from the recorded one (**apt changed**: update DN-2's table, `aptVerdicts` and the README's date); or every apt accepts and the cache rejects and the shape is not a listed deviation (**too strict**). `shellcheck -x` clean.

- [ ] **Step 3: Run it** (`make e2e-apt-oracle`); expected: every row agrees, exit 0, about one minute with the images cached. Record the run's output in the report and the README's *Results* (date, tree, the six apt versions).

- [ ] **Step 4: Mutant.** Temporarily make `checkSignatureArmor` accept a line after END → the oracle reports `i_trail_lf` and `d_trail_lf` **unsafe** and exits non-zero. Restore.

- [ ] **Step 5: Docs.** `e2e/apt-oracle/README.md`: what it checks, the three failure kinds and what to do for each, the images, when to run it (before a release, and after a go-crypto upgrade or a change to `internal/gpg`), results. `docs/open-decisions.md`, the pre-RC list: add "`make e2e-apt-oracle` on the tree you tag" beside `make e2e-live`. Changelog under *Added*: `- \`make e2e-apt-oracle\`: runs the verifier's signature-shape fixtures through the real apt of six client images (manual).`

- [ ] **Step 6: Commit**: "e2e/apt-oracle: the verifier's shapes through real apt 2.2–3.2".

### Task 8: Records, the gate and the final review

**Files:**
- Modify: `docs/open-decisions.md`, `Changelog.md` (read it whole: one line per change, the file's style)

- [ ] **Step 1: `docs/open-decisions.md`.** Move the five entries from *Open* to *Decided*, each dated "the maintainer, 2026-10-28: worked for 1.1.0", saying what was done and naming its commits. Add to *Open*, each with its evidence and a recommendation: (a) downloads with no declared length on the request path and in hot prefetch keep only `upstream.total_timeout` (a cap needs a size per `.deb`: a `package_hash` column, a schema change); (b) `accept_any_signer`'s bypass applies no S1 check (DN-7); (c) the handler's `SniffReleaseGPG` still treats a binary `Release.gpg` as a signed anchor (fail-safe; DN-7); (d) `debian:13`'s `sqv` accepts a forged CRC and a bad signature beside a good one, which the cache now refuses (stricter than that client, never looser); (e) the security review's C-3 (weak digests and keys) and C-1 (dates, rollback) stay open.

- [ ] **Step 2: The gate**, on the final tree, results recorded in `…/scratchpad/hardening/gate/summary.txt`: `go vet ./...`; `go test -race ./...` (the `e2ehooks` build tag too: `go test -race -tags e2ehooks ./...`); `golangci-lint run ./...` and with `--build-tags e2ehooks`; `gofmt -l .` (the five drift files only); `make shellcheck`; the simulations under 4.5 s each under `-race`; the race suite under `golang:1.27-bookworm`; `make e2e`; `E2E_FLAT_IMAGES=debian:12 make e2e-flat`; `make e2e-apt-oracle`; `make lint`.

- [ ] **Step 3: The final review** — four lenses, each an independent reviewer on the branch's whole diff from `main`: (1) the verifier against DN-2/DN-3 and apt's behaviour; (2) GC data lifecycle (retire vs delete, every reader of `retired_at`, the rotation); (3) fetch and the handler (the cap, the retry and classification, strict mode); (4) tests, docs and operability (each claim against the code, the Changelog). Triage, fix, re-review (at most two rounds per lens), re-gate.

- [ ] **Step 4: Commit** the records: "GC and upstream hardening: records".
