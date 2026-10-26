# Flat repositories — Phase H3: docs, release notes, the live smoke and the final review — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the flat-repositories branch for a 1.1.0 release candidate: the operator documentation spec §8 asks for (README, `docs/configuration.md`, `docs/admin-ui-spec.md`, `docs/log-fields.md`), the 1.1.0 release notes with every *H3's* carry checked against the code, the manual live-upstream smoke (`e2e/live`, run during planning), the H1 test-output carries, spec revision 13, a home for the decisions that outlive the phase, an optional Go toolchain task for the maintainer to approve, and the final multi-lens review of the whole branch — in the maintainer's words, "a reliable, robust, and low maintenance system".

**Architecture:** No production Go change (an optional Task 8, the Go toolchain, changes only build files, and only if the maintainer approves it). Two test-only tasks remove the test output the H1 reviews flagged and the cmd tests' undrained daemon. `e2e/live/` is a new, manual, bounded smoke in e2e/flat's style: one container (a release-built daemon and real apt 2.6 clients) against five vendors' live flat repositories, sourcing e2e/flat's `lib.sh`. The documentation tasks write their text out in full here, each claim with a pre-flight step that checks it against the code at HEAD. The release notes are a new in-tree file, `docs/release-notes/1.1.0.md`, which the maintainer pastes into the GitHub release (open decision D1). The last task is the release-candidate dry run, the full gate and the final review.

**Tech Stack:** Go 1.25 (toolchain 1.26 locally; Task 8 proposes 1.27), Bash 5 and shellcheck 0.11, Docker with BuildKit, apt 2.6.1 (`debian:12`) for the live smoke, Markdown.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12; Task 7 records this plan's changes as revision 13) — §8 (Documentation) and §9's H row (the definition of done) are this plan's requirement; C10 (strict mode: the PR #8 parity consequences), C11 (the inventory of new events, fields and metrics), §7 (H2's deviations, recorded in revision 13) and §10 (follow-ups) are what it documents. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — the phase table's H3 row, every carry-forward marked *H3's*, "PR #8 parity" section H (the live smoke and the strict-mode docs), the decisions block. The *For H3* sections of `…-H1-sweep.md` (with the overview's three *(H1)* corrections to it) and `…-H2-e2e.md` (items 1–11; where that plan's body and its For H3 list or `e2e/flat/README.md` differ, the list and the README are right). Phase H is three plans run in order: H1 (the sweep), H2 (`e2e/flat`), **H3 (this plan)**.

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- **Nothing goes to GitHub.** No push, pull request, tag, release, issue or comment, and nothing on PR #8. The maintainer tags 1.1.0-rc1 himself; Task 9 prepares and dry-runs it locally.
- **No production Go change**, no schema change, no config key. The Go changes of Tasks 1, 2 and 4 are `_test.go` files only. Never edit an entry of `migrations` in `internal/cache/schema.go`.
- `go test -race ./...` and `golangci-lint run ./...` are clean, and `gofmt -l .` lists only the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task that touches Go. Never run `gofmt -w` on those five.
- **Every doc claim is checked against the code at HEAD** by a step of its task: a grep with its expected output, or a named test. Text written from memory is wrong (the H2 lesson: seven review rounds moved the harness away from its plan). A claim whose check fails is corrected to what the code does, never the reverse, and the correction is reported.
- **Every change is proven:** a test (red, then green, with a mutant that fails it), a guard test, a grep check with its expected output, or a recorded run.
- Shell files are bash with `set -euo pipefail` and pass `shellcheck -x` with no output; no `grep -q` ends a pipe (e2e/flat's design note 18); every wait is a bounded poll (e2e/flat's ruling R-H2-5).
- `AIDEV-NOTE`s at every subtle point; existing `AIDEV-*` comments are kept and updated, never removed.
- Credit: Venkat (@venkatamutyala) stays credited in `Contributors.md` ("flat repository support is based on their PR #8"); the release notes credit them (Task 6; the wording is flagged for the maintainer, open decision D5).
- `git diff` in this repository goes through difftastic: pass `--no-ext-diff` to every diff a step reads.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Every command runs from the repository root unless it says otherwise. Expected outputs quote the planning runs (2026-10-26, a 12-core machine, Go 1.26.8, Docker 29); a time is a range, never an assertion.
- Edits to existing files are exact `old`/`new` blocks; each `old` occurs once in the file as it stands after the previous tasks. New files are given whole.
- A pre-flight step runs **before** its task's edits: it checks the facts the text states. If one fails, stop and report; do not write the text.
- Docker steps need network access to Docker Hub, the distribution mirrors and (Task 3 only) the five vendors. A failure that names a mirror or a vendor's network is retried once before it is debugged (e2e/flat's design note 9).

## Rulings and design notes

Planner's rulings (for the controller to confirm; each says why):

- **R-H3-1 — the release notes live in the tree:** `docs/release-notes/1.1.0.md`, which the maintainer pastes into the GitHub release's body (or passes to `gh release edit <tag> --notes-file`); `release.yaml` keeps `generate_release_notes: true`, whose commit list then follows the pasted text. The project has never kept release notes in the tree: no `CHANGELOG`, `NEWS` or release-notes file in its history, and the tags' messages are one line (`apt-cacher-ultra 1.0.2`) or one paragraph (1.0.0). In the tree, the notes are reviewed with the code, checked against it by this plan's steps, versioned with the release they describe, and readable offline; a workflow change (`body_path`) would add a release-time failure (a tag without its file) and a second convention for release candidates, for a paste the maintainer makes once per release. Open decision D1 asks him.
- **R-H3-2 — `flip_held_ms` (H2 For H3 item 9):** the row says the value is about 20000 at most, since the hold's 20 s timer can fire a few milliseconds late (Task 4 Step 9). `flipgate.(*Gate).AnchorStart` arms its own `time.NewTimer(g.holdMax)` when the request joins the hold and sets `t.held = now.Sub(joined)` after the timer or the hold's release fires and the gate's mutex is taken (`internal/flipgate/gate.go`), so a request that joined as the hold began can log a few milliseconds over 20000; e2e/flat's `commit-delay` allows 20500 and measured at most 17914. A dashboard bound of exactly 20000 would misfire; the nominal bound stays the documented one.
- **R-H3-3 — a home for the decisions that outlive H3:** a new file, `docs/open-decisions.md`, owned by the maintainer: each entry says what is open, its evidence (files, commits, tests, reports), the recommendation and the date, and moves to a *Decided* section when he rules. The overview is a development plan (consumed when the branch merges) and its decisions block records what was decided; a file in `docs/` beside the operator docs outlives the phase and is where the next contributor looks. The overview's decisions block points at it. Open decision D3 asks him to confirm the location.
- **R-H3-4 — the live smoke's shape** (`e2e/live`, Task 3): one `debian:12` container with a release-built daemon (no build tags: the binary a release ships) and apt clients as state directories (e2e/flat's design note 2), sourcing e2e/flat's `lib.sh` (copied into the image, not duplicated), with the clients using the `http://HTTPS///<host>/…` convention, so no CA is installed. The vendors' signing keys are fetched from the vendors at run time, not committed (the same keys the vendors' install instructions name); the packages' Debian dependencies are installed in the image build, directly, so every apt run of the smoke goes through the cache alone. Adoption on, `integrity.refuse_unvouched_debs = true`, every other setting the shipped default. Manual, before a release, never in CI (five vendors' availability would gate merges). It ran green during planning (Task 3's expected output).
- **R-H3-5 — the test-output noise (the overview's H1 carry, final tests review M-8):** one `TestMain` per package (`internal/fetch`, `internal/metrics`) that sets a discarding default logger, instead of a logger in each of `internal/fetch`'s 28 `New(Options{…})` calls and an `ErrorLog` on each TLS server: `slog.SetDefault` also routes the `log` package — where `net/http` writes `http: TLS handshake error` for a server without `ErrorLog` — through the discarding handler, and a test added later is covered without remembering to. The one test that reads the default logger, `internal/metrics`' `TestGauge_CapWarningStaysOneShotAcrossResets`, sets its own and restores it. Measured: 35 `fetch retry`, 5 `TLS handshake error` and 4 `metrics_series_cap_reached` lines before, none after, the same 142 tests.
- **R-H3-6 — the cmd tests' readiness drain (the overview's H1 carry, parked item 14):** a helper `waitReadyOrStop(t, addr, cancel, done)` in a new test file, called by the 12 readiness waits whose test cancels its daemon only at its end (`defer cancel()`, `serveDone` read last); on a timeout it cancels the daemon and waits for `serveListeners` to return before `t.Fatalf`, as H1's `waitServing` does. The six tests whose `t.Cleanup` already cancels and waits before their readiness wait keep `waitForDaemonReady` (giving them the helper would make its drain and their cleanup race for the one result on `serveDone`).
- **R-H3-7 — version and packaging for the RC:** nothing in the tree changes. `make build` and the release workflow take the version from the tag (`git describe --tags`; `main.Version` by `-ldflags`), nfpm takes it from `VERSION`, and `release.yaml` turns `1.1.0-rc1` into the Debian version `1.1.0~rc1` and marks the GitHub release a prerelease, which the apt-repository publisher skips (`scripts/select-stable-tags.sh` drops prereleases). No source file carries a version string (`grep` in Task 9). Task 9 dry-runs both builds with the RC's version.
- **R-H3-8 — the live smoke's results** go in the release notes as they were measured during planning (the overview asks for them there), dated, and Task 9 reruns the smoke on the final tree; a different result on that run is a finding (ruling R-H2-4: report, never work around).

Design notes (choices the spec leaves open):

1. **The inventories are computed, not copied.** The release notes' lists of new events and metrics come from the code at 1.0.2 (`main`, 536a1ab — `git rev-parse 1.0.2^{}` is the same commit) and at HEAD: every log message production code logs (`logMessages`, the guard tests' AST walk in `internal/observability/event_names_test.go`, run on both trees) and every `"acu_…"` string literal in non-test Go files. Task 6's pre-flight recomputes both and compares them with the notes' lists. Planning run: 77 log messages only at HEAD (76 events plus the test-only `e2e_commit_delay_enabled`, which the notes mark test-binary-only), 6 only at 1.0.2 (the six removed prose messages, as the overview's *(H1)* carry lists them), 21 metric names only at HEAD, none only at 1.0.2. Spec C11's lists say "include": they omit nine of the 21 (`acu_clock_steps_detected_total`, `acu_legacy_snapshots_quarantined_total`, `acu_serve_byhash_displaced_total` and the flip barrier's six counters), so the notes take the computed list and spec revision 13 says C11's is not exhaustive.
2. **Release notes structure:** *Before you upgrade* (the changes that can surprise a running install, each with what to do) first, then what is new, then logging and metrics, then the tests and the live smoke, then credits. Each item is short and links to the reference section that has the detail; the notes repeat a rule only where an operator must act before upgrading.
3. **The README stays an operator's document** (brief): it says what is supported and how to use it, in a few paragraphs, and links to `docs/configuration.md` for the rules and limits. Its *Status* paragraph is the maintainer's own voice and is left as it is.
4. **The captive-portal aftermath's `Release.gpg`** (H2 For H3 item 3): before the docs call the resulting apt error transient, a handler test pins that an adopted unsigned snapshot answers its suite's `Release.gpg` (and `InRelease`) 404 rather than the cached page (Task 4; red against a mutant that lets such a path fall through to the `url_path` row). Spec §10 item 9 and `TestServeHTTP_SnapshotMissReturns404` pinned it for the inline form only.
5. **The toolchain is the maintainer's** (H2 For H3 item 11; open decision D2). Facts established during planning (2026-10-26): the local toolchain is go1.26.8; go.dev lists go1.27.3 and go1.26.10 as the supported releases, and Go 1.25's last release is go1.25.14 — Go supports each major release until two newer ones exist, so 1.25 gets no security fix since 1.27.0; `golang:1.27-bookworm`, `golang:1.27-alpine`, `golang:1.26-bookworm` and `golang:1.26-alpine` all exist on Docker Hub. At HEAD under `golang:1.27-bookworm` (go1.27.3): `go vet ./...` clean, the static build succeeds, `go test -race -timeout 10m ./...` passes in all 21 packages (315 s), and the `e2ehooks` tests pass; every e2e Dockerfile's build stage builds with `golang:1.27-bookworm` (or `-alpine`). CI's golangci-lint pin, v2.12.2, cannot lint under Go 1.27 (every package fails its type check: `export data version 5 is greater than maximum supported version 4`), while v2.14.0 reports `0 issues.`; under go1.26.10, v2.12.2 reports `0 issues.` too. Five files name Go 1.25 for builds: `ci.yaml` (two `setup-go`), `release.yaml`, `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile` and `e2e/Dockerfile.cache` (the brief named two e2e Dockerfiles; there are three), and Task 3's `e2e/live/Dockerfile` makes a fourth. The 2026-09-08 security review raised it too (finding H-3, which also asks for `govulncheck` in CI); that review's report is untracked, in the main checkout (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`), not in git. Task 8 is optional and nothing depends on it.
6. **The final review's inputs** are split by area, because the branch is large: `git diff --no-ext-diff --numstat main..HEAD` at e65f67d counts 320 files, +159,612/−3,090 lines, of which 87,657 are the phase plans and 2,318 the spec; the Go production diff is 20,714 lines (freshness 10,077, cache 4,502, handler 1,741, aptpath/wallclock/simtest/observability 1,137, flipgate 938, admin 884, gc 583, other internal 472, cmd 380) and the Go test diff 47,545. Task 9 gives each lens its areas.

## Open decisions for the maintainer

Each is carried, with the planner's recommendation, to the final report and to `docs/open-decisions.md` (Task 7). None blocks a task: each task does the recommended thing or nothing, as stated.

- **D1 — where the 1.1.0 release notes live.** Recommendation: in the tree, `docs/release-notes/1.1.0.md`, pasted into the GitHub release (ruling R-H3-1). Alternative: `release.yaml` reads `body_path` for the tag.
- **D2 — the Go toolchain** (before the RC). Recommendation: approve Task 8 — build with Go 1.27 everywhere (CI, release, the four e2e Dockerfiles), keeping `go.mod`'s `go 1.25.0` language line, and move CI's golangci-lint to v2.14.0, which Go 1.27 needs; a 1.1.0 built with Go 1.25 ships without the security fixes made since 1.27.0 shipped. The full test suite passed under go1.27.3 during planning (design note 5). Go 1.26 would also do (golangci-lint unchanged), with six months' less support.
- **D3 — the home of the decisions that outlive H3.** Recommendation: `docs/open-decisions.md` (ruling R-H3-3).
- **D4 — confirm or reverse:** R-H5 (GD-5: an adopted unsigned flat suite whose `Release` disappears keeps its snapshot; recommendation: confirm), and the cache writer's fail-fast on panic (H1 parked item 16: `internal/cache`'s writer goroutine has no `recover`, so a panic inside a write operation ends the process and systemd restarts it; recommendation: confirm — a panic mid-transaction leaves the connection in an unknown state, and a restart rolls the transaction back).
- **D5 — the release notes' credit wording for PR #8** (Task 6). Recommendation: the text in Task 6's credits section.
- **D6 — the post-1.1.0 carries** (H2 For H3 items 3 and 11, the H1 carry about two WARNs): listed in `docs/open-decisions.md` with recommendations; none is in 1.1.0.

## Review Focus

The failure modes this phase's output can have that would bite a person using the software, most likely first; each is pinned by a step of the owning task:

1. **A release note or doc sentence that is wrong** — an operator acts on it (an alert, an upgrade step) and is misled; the H2 lesson. Every claim has a pre-flight check against HEAD (Tasks 4–7), and the notes' inventories are recomputed from the code at 1.0.2 and HEAD and compared mechanically (Task 6, Steps 1 and 4).
2. **An offline or archive cache upgraded without `gc.suite_idle_ttl = "0s"`** loses every suite no client asked for in 30 days, an hour after the first start, `gc.enabled = false` notwithstanding. The notes put it first under *Before you upgrade*; the README's reaping paragraph and `docs/configuration.md` say it too (Tasks 5, 6; the check greps the code's default and the reaper's start).
3. **A script that calls `POST /reconcile`** as on 1.0.x gets 403 (no `X-ACU-Admin: 1`) or, behind a proxy that forwards `Host`, 403 for the name; one that sent a bad scheme gets 400 where it got 409. The notes list each answer, the README's example shows the header and the new answers (Tasks 5, 6; checked against `internal/admin/handlers.go` at 1.0.2 and HEAD).
4. **An alert that matched a removed message, or a substring query that now matches more events,** silently changes on upgrade. The notes list the six removed messages with their replacements and the exact-match queries (Task 6; the replacements are grepped in the code, the removals computed).
5. **A live repository that changes** — a rotated key (pkg.jenkins.io now signs with its 2026 key; the 2023 one no longer verifies), a new Kubernetes minor, a renamed package — fails the live smoke for a reason outside the cache. The smoke names the failing step, the client's apt output and the daemon's log, and its README's *Maintenance* section says which line to change for each (Task 3).

---

## File map

| Area | Files | Task |
|---|---|---|
| Test-output noise | `internal/fetch/main_test.go`, `internal/metrics/main_test.go` (new) | 1 |
| The cmd tests' readiness drain | `cmd/apt-cacher-ultra/daemon_ready_test.go` (new); 11 cmd test files (12 readiness waits) | 2 |
| The live-upstream smoke | `e2e/live/Dockerfile`, `e2e/live/run.sh`, `e2e/live/smoke.sh`, `e2e/live/README.md` (new); `Makefile` (`e2e-live`) | 3 |
| Operator docs and the portal pin | `internal/handler/portal_aftermath_test.go` (new); `docs/configuration.md`, `docs/admin-ui-spec.md`, `docs/log-fields.md` | 4 |
| README | `README.md` | 5 |
| Release notes | `docs/release-notes/1.1.0.md` (new) | 6 |
| Spec, decisions, overview | `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 13), `docs/open-decisions.md` (new), the overview | 7 |
| The Go toolchain (optional, D2) | `.github/workflows/ci.yaml`, `.github/workflows/release.yaml`, `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile`, `e2e/live/Dockerfile`, `e2e/Dockerfile.cache`, `e2e/flat/README.md` | 8 |
| RC dry run, the gate, the final review | none (runs, records, dispatches reviews); fix commits as the review finds | 9 |

No migration, no new Go package, no config key, no production code path.

## Tasks

1. Test-output noise: one `TestMain` in `internal/fetch` and `internal/metrics`
2. The cmd tests' readiness drain: `waitReadyOrStop`
3. The live-upstream smoke: `e2e/live` and `make e2e-live`
4. Operator docs: strict mode on flat hosts, the unsigned confirmation, the captive portal's aftermath (with its pin), binary `Release.gpg`, `flip_held_ms`
5. README: repositories, suite reaping and `/forget`, the admin answers, testing
6. The 1.1.0 release notes
7. Spec revision 13, `docs/open-decisions.md` and the overview
8. (Optional, open decision D2) The Go 1.27 toolchain
9. The release-candidate dry run, the full gate and the final multi-lens review

---
### Task 1: Test-output noise: one `TestMain` in `internal/fetch` and `internal/metrics`

The overview's *(H1)* carry (final tests review M-8): `go test -v` in `internal/fetch` prints the client's `INFO fetch retry` lines (`fetch.New` falls back to `slog.Default()` without `Options.Logger`) and `net/http`'s `http: TLS handshake error` lines (the TLS test servers have no `ErrorLog`, so the server writes to the `log` package), and in `internal/metrics` the registry's `WARN metrics_series_cap_reached` (`slog.Default()`, `internal/metrics/metrics.go`). Test-only (ruling R-H3-5).

**Files:**
- Create: `internal/fetch/main_test.go`, `internal/metrics/main_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing other tasks use. Each package's only `TestMain` (no `TestMain` exists in the repository: `grep -rn 'func TestMain' internal cmd` prints nothing at HEAD).

- [ ] **Step 1: Measure the noise (the red run)**

Run:

```bash
T=$(mktemp -d)
go test -count=1 -v ./internal/fetch/ ./internal/metrics/ > "$T/before.txt" 2>&1; echo "rc=$?"
grep -c 'fetch retry' "$T/before.txt"
grep -c 'TLS handshake error' "$T/before.txt"
grep -c 'metrics_series_cap_reached' "$T/before.txt"
grep -c '^=== RUN' "$T/before.txt"
```

Expected (planning run):

```
rc=0
35
5
4
142
```

- [ ] **Step 2: Write `internal/fetch/main_test.go`**

```go
package fetch

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the default logger for this package's tests (the H1
// final tests review M-8): a Client built without Options.Logger logs its
// "fetch retry" lines to slog.Default(), and the httptest TLS servers,
// which have no ErrorLog, log their handshake errors through the log
// package, which slog.SetDefault routes to the same discarding handler.
// A test that checks a log line passes its own Options.Logger.
//
// AIDEV-NOTE: this is the package's only TestMain; a test that needs the
// default logger's output sets slog.SetDefault itself and restores it.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
```

- [ ] **Step 3: Write `internal/metrics/main_test.go`**

```go
package metrics

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the default logger for this package's tests (the H1
// final tests review M-8): the series-cap tests make the registry log WARN
// metrics_series_cap_reached to slog.Default(), once per metric.
// TestGauge_CapWarningStaysOneShotAcrossResets, which counts that line,
// sets its own default logger and restores this one.
//
// AIDEV-NOTE: this is the package's only TestMain; a test that needs the
// default logger's output sets slog.SetDefault itself and restores it.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
```

- [ ] **Step 4: Measure again (green): no noise, the same tests, nothing else printed**

Run:

```bash
T=$(mktemp -d)
go test -count=1 -v ./internal/fetch/ ./internal/metrics/ > "$T/after.txt" 2>&1; echo "rc=$?"
grep -c 'fetch retry' "$T/after.txt"
grep -c 'TLS handshake error' "$T/after.txt"
grep -c 'metrics_series_cap_reached' "$T/after.txt"
grep -c '^=== RUN' "$T/after.txt"
grep -vE '^(=== RUN|--- PASS|PASS$|ok |\s+--- PASS)' "$T/after.txt" | wc -l
```

Expected:

```
rc=0
0
0
0
142
0
```

The second line from the end is the same 142 tests as Step 1; the last line says nothing but test results was printed. (Removing either file brings that package's lines back: Step 1 is that run.)

- [ ] **Step 5: The packages under `-race`, lint, format**

Run: `go test -race -count=1 ./internal/fetch/ ./internal/metrics/ && golangci-lint run ./internal/fetch/... ./internal/metrics/... && gofmt -l internal/fetch internal/metrics`
Expected: two `ok` lines, `0 issues.`, and no file from `gofmt -l`.

- [ ] **Step 6: Commit**

```bash
git add internal/fetch/main_test.go internal/metrics/main_test.go
git commit -F - <<'EOF'
Tests: silence the default logger in internal/fetch and internal/metrics

go test -v printed 35 "fetch retry" lines (fetch.New falls back to
slog.Default()), 5 "http: TLS handshake error" lines (the TLS test
servers have no ErrorLog, so net/http writes to the log package) and 4
WARN metrics_series_cap_reached lines (the registry logs to
slog.Default()). Each package gets one TestMain that sets a discarding
default logger; slog.SetDefault also routes the log package through it,
so the handshake lines go too, and a test added later is covered. The
same 142 tests run; nothing but their results is printed. The H1 final
tests review M-8 (the overview's H1 carry).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 2: The cmd tests' readiness drain: `waitReadyOrStop`

The overview's *(H1)* carry (H1 final review, parked item 14): 12 of the cmd tests that start the daemon with `serveListeners` only `defer cancel()` and read `serveDone` at their end, so a `t.Fatalf` after `waitForDaemonReady` times out leaves the daemon running past the test, racing the `shutdownTimeout` restore and the `TempDir` removal their cleanups make. Failure path only; it never masks a pass. Test-only (ruling R-H3-6).

**Files:**
- Create: `cmd/apt-cacher-ultra/daemon_ready_test.go`
- Modify: the readiness wait (three lines each) in `cmd/apt-cacher-ultra/connect_log_test.go`, `connect_shutdown_test.go` (two), `mitm_metrics_integration_test.go`, `cert_cache_evicted_test.go`, `inner_get_deny_range_test.go`, `connect_tls_failed_log_test.go`, `cert_issued_test.go`, `shutdown_orphan_blob_test.go`, `clock_skew_log_test.go`, `ca_expiry_test.go`, `mitm_handshake_metric_test.go`

**Interfaces:**
- Consumes: `waitForDaemonReady(t *testing.T, addr string, timeout time.Duration) error` (`cmd/apt-cacher-ultra/connect_shutdown_test.go`, unchanged).
- Produces: `waitReadyOrStop(t *testing.T, addr string, cancel context.CancelFunc, done <-chan error)` and `readyOrStop(t *testing.T, addr string, timeout time.Duration, cancel context.CancelFunc, done <-chan error) error`, test helpers of package `main`.

The six tests left on `waitForDaemonReady` register, before their readiness wait, a `t.Cleanup` that cancels the daemon and waits for `serveDone` (`ca_constraints_log_test.go`, `ca_lifecycle_log_test.go`, `legacy_audit_wiring_test.go`, `reaper_wiring_test.go`, `status_tls_mitm_integration_test.go`, `disabled_mode_parity_daemon_test.go`): they drain already, and the helper's drain would race their cleanup for the one value on `serveDone`.

- [ ] **Step 1: Write the guard test first**

Create `cmd/apt-cacher-ultra/daemon_ready_test.go` with the imports and the test alone:

```go
package main

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadyOrStop_StopsTheDaemonBeforeItReturns pins waitReadyOrStop's
// failure path: a daemon that never answers is cancelled, and readyOrStop
// returns only after it has stopped.
func TestReadyOrStop_StopsTheDaemonBeforeItReturns(t *testing.T) {
	// A listener that never accepts: a dial succeeds, no answer comes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped atomic.Bool
	done := make(chan error, 1)
	go func() { // the daemon: it serves nothing and takes a while to stop
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		stopped.Store(true)
		done <- nil
	}()
	if err := readyOrStop(t, ln.Addr().String(), 300*time.Millisecond, cancel, done); err == nil {
		t.Fatal("readyOrStop reported ready a daemon that never answered")
	}
	if !stopped.Load() {
		t.Fatal("readyOrStop returned before the daemon had stopped")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 -run 'TestReadyOrStop' ./cmd/apt-cacher-ultra/`
Expected: the build fails: `undefined: readyOrStop` (and `"fmt" imported and not used`).

- [ ] **Step 3: Add the helpers**

In `cmd/apt-cacher-ultra/daemon_ready_test.go`, between the import block and the test:

```go
// waitReadyOrStop waits for the daemon on addr as waitForDaemonReady does,
// 10 s at most, and when it never answers stops it before failing t
// (readyOrStop). A test that cancels its daemon only at its end — defer
// cancel(), done read last — calls it, so a readiness failure cannot leave
// the daemon running past the test, racing the test's cleanups (the
// shutdownTimeout restore, the TempDir removal): the H1 final review's
// parked item 14. A test whose own t.Cleanup cancels and waits, registered
// before the wait (ca_constraints_log_test.go), calls waitForDaemonReady.
func waitReadyOrStop(t *testing.T, addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	if err := readyOrStop(t, addr, 10*time.Second, cancel, done); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}
}

// readyOrStop is waitReadyOrStop with its timeout and its error: when the
// daemon does not answer within timeout, it cancels it and returns only
// once serveListeners has returned on done (15 s at most, as waitServing
// waits).
func readyOrStop(t *testing.T, addr string, timeout time.Duration, cancel context.CancelFunc, done <-chan error) error {
	t.Helper()
	err := waitForDaemonReady(t, addr, timeout)
	if err != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			err = fmt.Errorf("%w; serveListeners had not returned 15s after the cancel", err)
		}
	}
	return err
}
```

- [ ] **Step 4: Run it to see it pass, then its mutant fail**

Run: `go test -race -count=1 -run 'TestReadyOrStop' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS: TestReadyOrStop_StopsTheDaemonBeforeItReturns` (0.7 s in planning).

Mutant (revert after): in `readyOrStop`, replace the `select { … }` block (six lines) with `_ = fmt.Sprint(done)`, so it cancels but does not wait. The same command fails: `daemon_ready_test.go:65: readyOrStop returned before the daemon had stopped`.

- [ ] **Step 5: Convert the 12 readiness waits**

Each of the 12 is exactly these three lines, with the variables `cacheAddr`, `cancel` and `serveDone` in scope:

```go
	if err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {
		t.Fatalf("daemon never became ready: %v", err)
	}
```

and becomes:

```go
	waitReadyOrStop(t, cacheAddr, cancel, serveDone)
```

Run (from the repository root):

```bash
python3 - <<'EOF'
import pathlib, sys
files = ["connect_log_test.go", "connect_shutdown_test.go", "mitm_metrics_integration_test.go",
         "cert_cache_evicted_test.go", "inner_get_deny_range_test.go", "connect_tls_failed_log_test.go",
         "cert_issued_test.go", "shutdown_orphan_blob_test.go", "clock_skew_log_test.go",
         "ca_expiry_test.go", "mitm_handshake_metric_test.go"]
old = ("\tif err := waitForDaemonReady(t, cacheAddr, 10*time.Second); err != nil {\n"
       "\t\tt.Fatalf(\"daemon never became ready: %v\", err)\n"
       "\t}\n")
new = "\twaitReadyOrStop(t, cacheAddr, cancel, serveDone)\n"
total = 0
for name in files:
    p = pathlib.Path("cmd/apt-cacher-ultra") / name
    s = p.read_text()
    n = s.count(old)
    if n == 0:
        sys.exit(f"{p}: no readiness block")
    p.write_text(s.replace(old, new))
    total += n
print(f"replaced {total} blocks")
sys.exit(0 if total == 12 else 1)
EOF
```

Expected: `replaced 12 blocks` (two of them in `connect_shutdown_test.go`).

- [ ] **Step 6: Check where each helper is called**

Run:

```bash
grep -c 'waitReadyOrStop(t, cacheAddr, cancel, serveDone)' cmd/apt-cacher-ultra/*_test.go | grep -v ':0$' | sort
grep -l 'waitForDaemonReady(t, ' cmd/apt-cacher-ultra/*_test.go | sort
```

Expected:

```
cmd/apt-cacher-ultra/ca_expiry_test.go:1
cmd/apt-cacher-ultra/cert_cache_evicted_test.go:1
cmd/apt-cacher-ultra/cert_issued_test.go:1
cmd/apt-cacher-ultra/clock_skew_log_test.go:1
cmd/apt-cacher-ultra/connect_log_test.go:1
cmd/apt-cacher-ultra/connect_shutdown_test.go:2
cmd/apt-cacher-ultra/connect_tls_failed_log_test.go:1
cmd/apt-cacher-ultra/inner_get_deny_range_test.go:1
cmd/apt-cacher-ultra/mitm_handshake_metric_test.go:1
cmd/apt-cacher-ultra/mitm_metrics_integration_test.go:1
cmd/apt-cacher-ultra/shutdown_orphan_blob_test.go:1
cmd/apt-cacher-ultra/ca_constraints_log_test.go
cmd/apt-cacher-ultra/ca_lifecycle_log_test.go
cmd/apt-cacher-ultra/daemon_ready_test.go
cmd/apt-cacher-ultra/disabled_mode_parity_daemon_test.go
cmd/apt-cacher-ultra/legacy_audit_wiring_test.go
cmd/apt-cacher-ultra/reaper_wiring_test.go
cmd/apt-cacher-ultra/status_tls_mitm_integration_test.go
```

The second list is the six tests whose cleanup drains already, and `readyOrStop`'s own call.

- [ ] **Step 7: The package under `-race`, vet, lint, format**

Run: `go vet ./cmd/apt-cacher-ultra/ && go test -race -count=1 ./cmd/apt-cacher-ultra/ && golangci-lint run ./cmd/... && gofmt -l cmd`
Expected: `ok` for the package (about 40 s), `0 issues.`, and `gofmt -l` lists only the drift file `cmd/apt-cacher-ultra/ca_print.go`.

- [ ] **Step 8: Commit**

```bash
git add cmd/apt-cacher-ultra/daemon_ready_test.go cmd/apt-cacher-ultra/connect_log_test.go \
    cmd/apt-cacher-ultra/connect_shutdown_test.go cmd/apt-cacher-ultra/mitm_metrics_integration_test.go \
    cmd/apt-cacher-ultra/cert_cache_evicted_test.go cmd/apt-cacher-ultra/inner_get_deny_range_test.go \
    cmd/apt-cacher-ultra/connect_tls_failed_log_test.go cmd/apt-cacher-ultra/cert_issued_test.go \
    cmd/apt-cacher-ultra/shutdown_orphan_blob_test.go cmd/apt-cacher-ultra/clock_skew_log_test.go \
    cmd/apt-cacher-ultra/ca_expiry_test.go cmd/apt-cacher-ultra/mitm_handshake_metric_test.go
git commit -F - <<'EOF'
cmd tests: stop the daemon when it never becomes ready

Twelve readiness waits belong to tests that cancel their daemon only at
their end (defer cancel(), serveDone read last), so a readiness failure
left the daemon running past the test, racing its cleanups (the
shutdownTimeout restore, the TempDir removal). waitReadyOrStop cancels
it and waits for serveListeners to return before failing, as
waitServing does. The six tests whose cleanup already cancels and waits
before their readiness wait keep waitForDaemonReady. The H1 final
review's parked item 14 (the overview's H1 carry).

Pinned by TestReadyOrStop_StopsTheDaemonBeforeItReturns; readyOrStop
without its wait fails it (daemon_ready_test.go:65: readyOrStop
returned before the daemon had stopped).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 3: The live-upstream smoke: `e2e/live` and `make e2e-live`

The overview's "PR #8 parity" H (*H3's*): with adoption enabled and `integrity.refuse_unvouched_debs = true`, front live pkgs.k8s.io (two minors, one client per source form), NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com; for each, `adoption_success` is logged, `apt-get update` is clean under e2e/flat's error grep (its final form, with `^(E|Error):` on these hosts), one package installs, and `unvouched_deb_refused` is zero. Manual, not CI (ruling R-H3-4). The spec and the overview ask for no unsigned or Release-less live repository (they name these five), so the smoke has none; `e2e/flat` covers both shapes with fixtures.

What the live repositories looked like on 2026-10-26 (planning probes, 18:53 UTC; the smoke then ran at 19:00 and 19:21 UTC):

| Repository | Sources line (both k8s forms) | Anchors upstream | Hash blocks | `Date` | Packages |
|---|---|---|---|---|---|
| pkgs.k8s.io v1.36, v1.37 | `deb <uri>/core:/stable:/v1.3x/deb/ /` and `deb <uri>/core:/stable:/v1.3x/deb ./` | `InRelease`, `Release`, `Release.gpg` (each redirects once to `prod-cdn.packages.k8s.io`); v1.33–v1.37 published, v1.38 answers 403 | MD5Sum, SHA1, SHA256 | `Wed Sep 23 … 2026` (asctime form) | `amd64/…` Filenames; `kubectl` depends on nothing |
| NVIDIA CUDA debian12/x86_64 | `deb <uri>/ /` | `InRelease`, `Release`, `Release.gpg`; `Acquire-By-Hash: no` | MD5Sum, SHA1, SHA256 | `Mon, 26 Oct 2026 15:30:24 +0000`, the day of the probe | 9,758 stanzas, `./…` Filenames |
| libnvidia-container stable/deb/amd64 | `deb <uri>/ /` | `InRelease` only (`Release`, `Release.gpg` 404) | SHA512 only, over `Packages` and `Packages.xz` | `Fri, 27 Apr 2018` (frozen) | `./…` Filenames |
| pkg.jenkins.io debian-stable | `deb <uri> binary/` | `Release` and `Release.gpg` (`InRelease` 404): detached, signed by the 2026 key `7198F4B714ABFC68` | MD5Sum, SHA1, SHA256, SHA512 | `Wed, 30 Sep 2026` | 181 stanzas, `binary/…` and `direct/…` Filenames (one level above the `Release`); `jenkins` depends on `adduser`, `net-tools`, `sysvinit-utils`, `init-system-helpers` |
| download.sublimetext.com | `deb <uri>/ apt/stable/` | `InRelease`, `Release`, `Release.gpg` | MD5Sum, SHA1, SHA256, SHA512 | `Fri, 25 Sep 2026` | `files/…` Filenames (at the host root); `sublime-merge` depends on `libgtk-3-0`, `libcurl4` |

**Files:**
- Create: `e2e/live/Dockerfile`, `e2e/live/run.sh`, `e2e/live/smoke.sh`, `e2e/live/README.md`
- Modify: `Makefile` (`.PHONY` and the `e2e-live` target)

**Interfaces:**
- Consumes: e2e/flat's `lib.sh` (copied into the image by the Dockerfile; unchanged): the 21 helpers `scenario`, `passed`, `fail`, `show_logs`, `poll_quietly`, `log_mark`, `log_lines`, `log_has`, `event`, `kv`, `suite_kv`, `panics_in_log`, `start_daemon`, `stop_daemon`, `suite`, `adopted_as`, `client`, `capt`, `apt_update`, `apt_errors`, `candidate`, and the settings `CONFIG`, `DAEMON_LOG`, `PROXY`, `ADMIN`, `CURL`. `smoke.sh` overrides `say`, `NGINX_LOG` and `APT_SIGNED_HOSTS` after sourcing it.
- Produces: `make e2e-live`; `E2E_LIVE_NAME` (the image and container name, default `acu-e2e-live`). The results go in the release notes (Task 6) and are rerun in Task 9.

- [ ] **Step 1: Pre-flight: lib.sh's helpers, and the live repositories as the smoke expects them**

Run:

```bash
grep -cE '^(scenario|passed|fail|show_logs|poll_quietly|log_mark|log_lines|log_has|event|kv|suite_kv|panics_in_log|start_daemon|stop_daemon|suite|adopted_as|client|capt|apt_update|apt_errors|candidate)\(\) \{' e2e/flat/lib.sh
grep -cE '^(CONFIG|DAEMON_LOG|PROXY|ADMIN|CURL)=' e2e/flat/lib.sh
for u in https://pkgs.k8s.io/core:/stable:/v1.36/deb/InRelease https://pkgs.k8s.io/core:/stable:/v1.37/deb/InRelease \
    https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/InRelease \
    https://nvidia.github.io/libnvidia-container/stable/deb/amd64/InRelease \
    https://pkg.jenkins.io/debian-stable/binary/InRelease https://pkg.jenkins.io/debian-stable/binary/Release.gpg \
    https://download.sublimetext.com/apt/stable/InRelease; do
    printf '%s %s\n' "$(curl -sL -o /dev/null -w '%{http_code}' --max-time 30 "$u")" "$u"
done
```

Expected: `21`, `5`, then `200` for every URL but Jenkins' `InRelease`, which is `404` (a detached-only repository). A different answer is the vendor's change: update the smoke's values (the README's *Maintenance*) before going on, and say so in the commit.

- [ ] **Step 2: Write `e2e/live/Dockerfile`**

```dockerfile
# e2e/live: the live-upstream smoke (the flat-repositories overview, "PR #8
# parity", H): real apt clients against the live flat repositories
# pkgs.k8s.io, NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and
# download.sublimetext.com through apt-cacher-ultra, in one container.
# Manual, before a release; never in CI (it depends on five vendors).
# e2e/live/run.sh builds and runs it; e2e/live/README.md says what it
# checks.
#
# Two stages:
#   build  the daemon exactly as `make build` builds it (no build tags:
#          the binary a release ships).
#   test   debian:12 with curl and jq, the dependencies the smoke's
#          packages need from Debian (installed here, directly, so every
#          apt run of the smoke goes through the cache alone), and
#          e2e/flat's lib.sh beside smoke.sh.
#
# Build context is the repo root:
#   docker build -f e2e/live/Dockerfile -t acu-e2e-live .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.Version=0.0.0-e2e-live" \
    -o /out/apt-cacher-ultra ./cmd/apt-cacher-ultra

# debian:12 because the CUDA repository is per distribution (debian12).
# The second list is what jenkins and sublime-merge depend on beyond the
# base image; when a vendor adds a dependency, the install's apt error
# names it, and it goes here.
FROM debian:12 AS test
ENV DEBIAN_FRONTEND=noninteractive
RUN rm -f /etc/apt/apt.conf.d/docker-* && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        curl jq ca-certificates \
        adduser net-tools sysvinit-utils init-system-helpers \
        libgtk-3-0 libcurl4 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    mkdir -p /run/acu-e2e /var/log/acu-e2e
COPY --from=build /out/apt-cacher-ultra /usr/local/bin/apt-cacher-ultra
COPY e2e/flat/lib.sh e2e/live/smoke.sh /usr/local/lib/acu-e2e/
CMD ["bash", "/usr/local/lib/acu-e2e/smoke.sh"]
```

- [ ] **Step 3: Write `e2e/live/run.sh`**

```bash
#!/usr/bin/env bash
# e2e/live driver: builds e2e/live/Dockerfile and runs the live-upstream
# smoke (e2e/live/README.md). Manual, before a release; it reaches five
# vendors' live repositories, so it is never run in CI.
#
#   E2E_LIVE_NAME  image and container name (default acu-e2e-live)
#
#   make e2e-live
#   bash e2e/live/run.sh
set -euo pipefail
trap 'echo "[e2e-live] interrupted"; exit 130' INT
trap 'echo "[e2e-live] terminated"; exit 143' TERM
cd "$(dirname "$0")/../.."

name="${E2E_LIVE_NAME:-acu-e2e-live}"
if ! [[ $name =~ ^[a-z0-9][a-z0-9_.-]*$ ]]; then
    echo "[e2e-live] invalid E2E_LIVE_NAME '$name'"
    exit 2
fi

start=$SECONDS
if ! docker build -f e2e/live/Dockerfile -t "$name" .; then
    echo "[e2e-live] FAIL (build)"
    exit 1
fi
built=$((SECONDS - start))
# A container left by an interrupted run would take the name. --init puts
# a real init first, so Ctrl-C and docker stop end the smoke.
docker rm -f "$name" > /dev/null 2>&1 || true
if ! docker run --rm --init --name "$name" "$name"; then
    echo "[e2e-live] FAIL (smoke; build ${built}s, smoke $((SECONDS - start - built))s)"
    exit 1
fi
echo "[e2e-live] PASS (build ${built}s, smoke $((SECONDS - start - built))s)"
```

- [ ] **Step 4: Write `e2e/live/smoke.sh`**

```bash
#!/bin/bash
# e2e/live in-container smoke (the flat-repositories overview, "PR #8
# parity", H): real apt clients against live flat repositories through
# apt-cacher-ultra, with adoption enabled and strict mode on. For each
# repository: its suites adopt (adoption_success, in the form its upstream
# publishes), apt-get update is clean under e2e/flat's error grep, one
# package installs with its .deb vouched (validated_hash=true), and no
# .deb is refused but the controls'. e2e/live/run.sh builds the image and
# runs this as its CMD; e2e/live/README.md says what each step proves.
#
# AIDEV-NOTE: a repository that misbehaves through the cache is a finding
# to report, never something to work around here (e2e/flat's ruling
# R-H2-4). The values below change only when a vendor changes its
# repository (a key, a package, a minor version): README, "Maintenance".
set -euo pipefail
# shellcheck source=e2e/flat/lib.sh
. /usr/local/lib/acu-e2e/lib.sh

# lib.sh's settings that differ here: no nginx, and the five live hosts
# as apt names them through the http://HTTPS/// convention (a host-root
# source, Sublime's, has a space after the host: lib.sh's AIDEV-NOTE).
NGINX_LOG=/dev/null
KEYS=/etc/acu-e2e/keys
APT_SIGNED_HOSTS='HTTPS///(pkgs\.k8s\.io|developer\.download\.nvidia\.com|nvidia\.github\.io|pkg\.jenkins\.io|download\.sublimetext\.com)[/ ]'
# The adoptions' bound: the planning run took 221 s, most of it the wait
# for the scheduler's tick (periodic_refresh / 4, 225 s by default) that
# admits the first adoptions a client's check found no slot for.
ADOPT_LIMIT=900

say() {
    printf '[e2e-live %4ds] %s\n' "$SECONDS" "$*"
}

on_exit() {
    local rc=$?
    if ((rc != 0 && FAILED == 0)); then
        say "unexpected exit $rc in step $SCENARIO (command: $BASH_COMMAND)"
        show_logs
    fi
}
trap on_exit EXIT
trap 'fail "interrupted by a signal"' INT TERM

# The repositories. The k8s minors are the two newest; the keys are the
# ones each vendor's install instructions name (README, "Maintenance").
K8S_A=v1.36
K8S_B=v1.37
K8S_KEY=https://pkgs.k8s.io/core:/stable:/$K8S_B/deb/Release.key
CUDA_KEY=https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/3bf863cc.pub
NVIDIA_KEY=https://nvidia.github.io/libnvidia-container/gpgkey
JENKINS_KEY=https://pkg.jenkins.io/debian-stable/jenkins.io-2026.key
SUBLIME_KEY=https://download.sublimetext.com/sublimehq-pub.gpg

# src KEY REST: a sources line for the cache's http://HTTPS/// convention.
src() { printf 'deb [signed-by=%s/%s.asc] http://HTTPS///%s' "$KEYS" "$1" "$2"; }

# suite_expectations: one line per suite the cache tracks: host, suite
# path, the form it adopts in.
suite_expectations() {
    cat << EOF
pkgs.k8s.io /core:/stable:/$K8S_A/deb inline
pkgs.k8s.io /core:/stable:/$K8S_A/deb/. inline
pkgs.k8s.io /core:/stable:/$K8S_B/deb inline
pkgs.k8s.io /core:/stable:/$K8S_B/deb/. inline
developer.download.nvidia.com /compute/cuda/repos/debian12/x86_64 inline
nvidia.github.io /libnvidia-container/stable/deb/amd64 inline
pkg.jenkins.io /debian-stable/binary detached
download.sublimetext.com /apt/stable inline
EOF
}

write_config() {
    mkdir -p /var/cache/acu-e2e
    # The shipped defaults (packaging/config/config.toml.default), but
    # adoption on, strict mode on, the vendors' keys, and no MITM (the
    # clients use http://HTTPS///).
    cat > "$CONFIG" << EOF
[cache]
dir = "/var/cache/acu-e2e"
listen = "127.0.0.1:3142"

[adoption]
enabled = true
keyring_dirs = ["$KEYS"]

[integrity]
refuse_unvouched_debs = true

[tls_mitm]
enabled = false

[log]
level = "info"
format = "text"
EOF
}

fetch_keys() {
    mkdir -p "$KEYS"
    local k name url
    for k in "k8s $K8S_KEY" "cuda $CUDA_KEY" "nvidia $NVIDIA_KEY" \
        "jenkins $JENKINS_KEY" "sublime $SUBLIME_KEY"; do
        read -r name url <<< "$k"
        # Directly from the vendor, not through the cache: the keys are the
        # smoke's trust anchors, for the clients and the daemon alike.
        "${CURL[@]}" -fsSL --retry 3 -o "$KEYS/$name.asc" "$url" ||
            fail "could not fetch the $name signing key from $url"
        grep -q 'BEGIN PGP PUBLIC KEY BLOCK' "$KEYS/$name.asc" ||
            fail "the $name signing key from $url is not an armored key"
    done
}

setup() {
    scenario setup "keys, config, the daemon, the clients"
    say "apt $(apt-get --version | head -n 1 | cut -d' ' -f2); started $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    : > "$DAEMON_LOG"
    ! poll_quietly 0 false || fail "poll_quietly returned success for a condition that never held"
    fetch_keys
    write_config
    # shellcheck disable=SC2119 # no NAME=VALUE environment for this daemon
    start_daemon
    client k8s-slash "$PROXY" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_A/deb/ /")" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_B/deb/ /")"
    client k8s-dot "$PROXY" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_A/deb ./")" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_B/deb ./")"
    client cuda "$PROXY" "$(src cuda "developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/ /")"
    client nvidia "$PROXY" "$(src nvidia "nvidia.github.io/libnvidia-container/stable/deb/amd64/ /")"
    client jenkins "$PROXY" "$(src jenkins "pkg.jenkins.io/debian-stable binary/")"
    client sublime "$PROXY" "$(src sublime "download.sublimetext.com/ apt/stable/")"
    # The grep's control: a minor that does not exist answers 403 upstream
    # (as pkgs.k8s.io answers any missing file), and apt must print an
    # error that apt_errors catches, or every clean update below proves
    # nothing.
    client control "$PROXY" "$(src k8s "pkgs.k8s.io/core:/stable:/v1.0/deb/ /")"
    passed
}

CLIENTS=(k8s-slash k8s-dot cuda nvidia jenkins sublime)

first_contact() {
    scenario first-contact "every client's first apt-get update, clean"
    local c
    for c in "${CLIENTS[@]}"; do
        apt_update "$c"
        say "$c: first update clean"
    done
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/control/update.$APT_RUNS.log
    capt control apt-get update > "$APT_LOG" 2>&1 || true
    [[ -n $(apt_errors "$APT_LOG") ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "the control's update (a minor that does not exist) printed no error apt_errors catches"
    }
    say "control: the error grep catches the missing minor's error"
    passed
}

all_adopted() {
    local host path form
    while read -r host path form; do
        adopted_as "$host" "$path" "$form" || return 1
        log_has 0 "$(event adoption_success)" "$(kv canonical_host "$host")" "$(suite_kv "$path")" ||
            return 1
    done < <(suite_expectations)
}

adoptions() {
    scenario adoptions "every suite adopts, in its upstream's form"
    poll_quietly "$ADOPT_LIMIT" all_adopted || {
        say "the suites' status entries:"
        "${CURL[@]}" -fsS "$ADMIN/?format=json" | jq -c '.suites[] |
            {host, suite_path, anchor_form, current_snapshot_id, adoption_fail_reason, check_fail_count}'
        fail "not every suite adopted in its form within ${ADOPT_LIMIT}s"
    }
    say "all $(suite_expectations | wc -l) suites adopted, ${POLL_TOOK}s after first contact"
    local host path form id
    while read -r host path form; do
        id=$(suite "$host" "$path" current_snapshot_id)
        say "  $host$path: $form, snapshot $id, $(log_lines 0 "$(event adoption_success)" \
            "$(suite_kv "$path")" | tail -n 1 | grep -oE 'package_hash_count=[0-9]+')"
    done < <(suite_expectations)
    passed
}

updates() {
    scenario updates "every client's apt-get update from the snapshots, clean"
    local c
    for c in "${CLIENTS[@]}"; do
        apt_update "$c"
        say "$c: update clean"
    done
    passed
}

# strict_controls: on each host, a .deb no Packages declares is refused
# (502, unvouched_deb_refused) before any upstream request: strict mode is
# on there, so the installs below are vouched under it and whole_run's
# zero-refusals check is not vacuous.
STRICT_CONTROLS=(
    "pkgs.k8s.io/core:/stable:/$K8S_B/deb/amd64/acu-live-control_1.0_amd64.deb"
    developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/acu-live-control_1.0_amd64.deb
    nvidia.github.io/libnvidia-container/stable/deb/amd64/acu-live-control_1.0_amd64.deb
    pkg.jenkins.io/debian-stable/binary/acu-live-control_1.0_all.deb
    download.sublimetext.com/files/acu-live-control_1.0_amd64.deb
)

strict_controls() {
    scenario strict "strict mode refuses an undeclared .deb on every host"
    local u code
    for u in "${STRICT_CONTROLS[@]}"; do
        code=$("${CURL[@]}" -sS --path-as-is -o /dev/null -w '%{http_code}' \
            -x "$PROXY" "http://HTTPS///$u" || true)
        [[ $code == 502 ]] || fail "http://HTTPS///$u answered $code, want 502 (strict mode off for its host?)"
        log_has 0 "$(event unvouched_deb_refused)" "$(kv path "/${u#*/}")" ||
            fail "no unvouched_deb_refused line for /${u#*/}"
        say "refused, as strict mode should: ${u%%/*}"
    done
    passed
}

# live_install NAME PACKAGE [VERSION]: installs PACKAGE (VERSION, or the
# candidate) on client NAME through the cache, requires dpkg to report it
# installed at that version and the daemon to have served its .deb vouched
# (validated_hash=true), then purges it.
live_install() {
    local name=$1 pkg=$2 ver=${3:-} mark spec got
    [[ -n $ver ]] || ver=$(candidate "$name" "$pkg")
    [[ -n $ver && $ver != "(none)" ]] || fail "client $name has no candidate for $pkg"
    spec="$pkg=$ver"
    mark=$(log_mark)
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/install.$APT_RUNS.log
    if ! capt "$name" apt-get install -y --no-install-recommends "$spec" > "$APT_LOG" 2>&1 ||
        [[ -n $(apt_errors "$APT_LOG") ]]; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get install $spec on client $name failed (above)"
    fi
    got=$(dpkg-query -W -f='${Status} ${Version}' "$pkg" 2> /dev/null || true)
    [[ $got == "install ok installed $ver" ]] ||
        fail "client $name: dpkg reports '$got' for $pkg, want 'install ok installed $ver'"
    log_has "$mark" "$(event request)" '\.deb( |$)' 'validated_hash=true' || {
        log_lines "$mark" "$(event request)" '\.deb' | sed 's/^/    | /'
        fail "client $name: the daemon served no vouched .deb (validated_hash=true) for $spec"
    }
    say "$name: installed $spec, its .deb vouched ($(log_lines "$mark" "$(event request)" '\.deb' \
        'validated_hash=true' | grep -oE ' path=[^ ]+' | head -n 1 | sed 's/^ //'))"
    dpkg --purge "$pkg" > /dev/null 2>&1 || fail "dpkg --purge $pkg failed"
}

installs() {
    scenario installs "one package per repository, its .deb vouched"
    local older
    live_install k8s-slash kubectl
    # The other source form installs from the other minor.
    older=$(capt k8s-dot apt-cache madison kubectl |
        awk -v m="${K8S_A#v}." '$3 ~ "^" m {print $3; exit}')
    [[ -n $older ]] || fail "client k8s-dot lists no kubectl of $K8S_A"
    live_install k8s-dot kubectl "$older"
    live_install cuda cuda-toolkit-12-config-common
    live_install nvidia nvidia-container-toolkit-base
    live_install jenkins jenkins
    live_install sublime sublime-merge
    passed
}

whole_run() {
    scenario whole-run "no refusal but the controls', no panic"
    local refused
    refused=$(log_lines 0 "$(event unvouched_deb_refused)" | grep -v 'acu-live-control_' || true)
    [[ -z $refused ]] || {
        printf '%s\n' "$refused" | sed 's/^/    | /'
        fail "unvouched_deb_refused for a package the smoke installed (above)"
    }
    [[ -z $(panics_in_log) ]] || {
        panics_in_log | head -n 20 | sed 's/^/    | /'
        fail "the daemon logged a panic or a hash conflict (above)"
    }
    stop_daemon "the end"
    passed
}

setup
first_contact
adoptions
updates
strict_controls
installs
whole_run
say "timings: ${TIMINGS[*]}"
say "PASS: live smoke, finished $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
```

- [ ] **Step 5: Write `e2e/live/README.md`**

````markdown
# e2e/live: the live-upstream smoke

A manual check, before a release, that apt-cacher-ultra fronts real flat
repositories as their users configure them: pkgs.k8s.io, NVIDIA CUDA,
libnvidia-container, pkg.jenkins.io and download.sublimetext.com. It runs a
daemon built as a release builds it, with snapshot adoption on and strict
mode on (`integrity.refuse_unvouched_debs = true`), and real apt clients
(apt 2.6, `debian:12`) in one container.

`e2e/flat` tests every repository shape against fixtures, on six apt
versions, in CI; this smoke checks the live repositories those fixtures
copy. It depends on five vendors' servers, so it never runs in CI.

## Running it

```sh
make e2e-live
# or, with another image and container name:
E2E_LIVE_NAME=my-e2e-live bash e2e/live/run.sh
```

It needs Docker and network access to Docker Hub, `deb.debian.org` (the
image build) and the five vendors. It takes about 5 minutes: the build
(under a minute with cached layers) and about 4 minutes of smoke, most of
it waiting for the scheduler's tick (`periodic_refresh / 4`, 225 s by
default) that admits the first adoptions a client's check found no free
slot for. Ctrl-C stops it.

## What it checks

| Step | What it proves |
|---|---|
| `setup` | The vendors' keys (fetched from the vendors, for the clients and the daemon) and the daemon start; each client is an apt state directory with its own sources. |
| `first-contact` | Every client's first `apt-get update` through the cache is clean under e2e/flat's error grep (`apt_errors`: failures, timeouts, hash mismatches, and any signature error or `E:`/`Error:` line naming one of the five hosts). A control source — a Kubernetes minor that does not exist, answered 403 — must print an error the grep catches. |
| `adoptions` | Every suite adopts, in the form its upstream publishes (inline, or detached for pkg.jenkins.io), and logs `adoption_success`: both source forms of two Kubernetes minors (four suites), CUDA, libnvidia-container (a SHA512-only `Release`), Jenkins and Sublime Text (a host-root base). |
| `updates` | Every client's next `apt-get update`, from the snapshots, is clean. |
| `strict` | On each of the five hosts, a `.deb` no `Packages` declares is refused (502, `unvouched_deb_refused`) before any upstream request: strict mode is on, so the installs below happen under it. |
| `installs` | One package per repository installs through the cache, and the daemon served its `.deb` vouched (`validated_hash=true`): `kubectl` from each Kubernetes minor (one per source form), `cuda-toolkit-12-config-common` (CUDA's `./` Filenames), `nvidia-container-toolkit-base` (libnvidia-container's `./` Filenames), `jenkins` (Filenames one level above its `Release`) and `sublime-merge` (Filenames at the host root). |
| `whole-run` | No `unvouched_deb_refused` but the controls', no panic or hash conflict in the log, and a clean stop. |

## A failure: the cache, or the vendor?

The output names the step that failed and prints what it saw: apt's
output, or the suites' status entries, then the daemon log's tail.

- **The vendor:** a key that does not download or no longer signs (apt
  prints `NO_PUBKEY`, and the daemon's adoption fails its signature
  check), a minor or package that is gone (`no candidate`), or a server
  that answers 5xx or not at all (`Failed to fetch`). Check it directly
  (`curl -fsSI https://pkgs.k8s.io/core:/stable:/v1.37/deb/InRelease`);
  fix the smoke's values (*Maintenance*) or run it again later.
- **The cache:** anything else — an adoption that fails while upstream
  answers and the key is right, an update error a direct `apt update`
  does not show, a refusal of a package `Packages` declares, a panic. That
  is a finding: reproduce it in a Go test or in `e2e/flat`, fix it there,
  and never change the smoke to pass around it.

## Maintenance

The values to change sit at the top of `smoke.sh`:

- **Kubernetes minors** (`K8S_A`, `K8S_B`): the two newest. A new minor
  appears three times a year; a minor pkgs.k8s.io does not publish
  answers 403.
- **Keys** (`*_KEY`): the URLs the vendors' install instructions name.
  pkg.jenkins.io moved from `jenkins.io-2023.key` to `jenkins.io-2026.key`
  in 2026: its `Release.gpg` is signed by key `7198F4B714ABFC68`.
- **CUDA's distribution:** the repository is per distribution
  (`debian12`) and must match the image (`debian:12`); move both
  together, with the key file the repository's directory lists
  (`3bf863cc.pub` today).
- **Packages:** each is small and needs nothing outside the image. When a
  vendor adds a dependency, the install's apt error names it: add it to
  the Dockerfile's second package list.

## Results

| Date | Result | Times |
|---|---|---|
| 2026-10-26, 19:00 and 19:21 UTC (planning, phase H3) | both runs: all 8 suites adopted (4 Kubernetes, inline; CUDA, libnvidia-container and Sublime Text inline; Jenkins detached), every update clean, 6 packages installed vouched, 5 strict refusals (the controls), no other refusal | build 51 and 20 s, smoke 245 and 240 s (adoptions 221 and 224 s) |
````

- [ ] **Step 6: The Makefile target**

`old`:

```make
.PHONY: build test test-race lint fmt deb e2e e2e-flat deb-test apt-repo-test clean
```

`new`:

```make
.PHONY: build test test-race lint fmt deb e2e e2e-flat e2e-live deb-test apt-repo-test clean
```

`old`:

```make
e2e-flat:
	bash e2e/flat/run.sh
```

`new`:

```make
e2e-flat:
	bash e2e/flat/run.sh

# The manual live-upstream smoke (e2e/live/README.md): a release-built
# daemon and real apt clients against pkgs.k8s.io, NVIDIA CUDA,
# libnvidia-container, pkg.jenkins.io and download.sublimetext.com, with
# adoption and strict mode on; about 5 minutes. It needs network access to
# those five vendors, so it is run before a release, never in CI.
# E2E_LIVE_NAME names its image and container (default acu-e2e-live).
e2e-live:
	bash e2e/live/run.sh
```

Run: `make -n e2e-live`
Expected: `bash e2e/live/run.sh`.

- [ ] **Step 7: Lint**

Run: `chmod +x e2e/live/run.sh e2e/live/smoke.sh && bash -n e2e/live/run.sh && bash -n e2e/live/smoke.sh && shellcheck -x e2e/live/*.sh && shellcheck -x e2e/flat/*.sh && echo clean`
Expected: `clean` (shellcheck prints nothing for either directory).

- [ ] **Step 8: Run the smoke**

Run: `make e2e-live 2>&1 | tee "$(mktemp)" | grep -E '^\[e2e-live'`
Expected (the planning run of these files, 2026-10-26 19:21 UTC; snapshot ids and `package_hash_count`s vary with the order of adoptions and what the vendors publish):

```
[e2e-live    0s] === scenario setup: keys, config, the daemon, the clients
[e2e-live    0s] apt 2.6.1; started 2026-10-26 19:21:57 UTC
[e2e-live    1s] starting the daemon
[e2e-live    3s] --- scenario setup passed (3s)
[e2e-live    3s] === scenario first-contact: every client's first apt-get update, clean
[e2e-live    3s] k8s-slash: first update clean
[e2e-live    3s] k8s-dot: first update clean
[e2e-live    4s] cuda: first update clean
[e2e-live    4s] nvidia: first update clean
[e2e-live    4s] jenkins: first update clean
[e2e-live    5s] sublime: first update clean
[e2e-live    5s] control: the error grep catches the missing minor's error
[e2e-live    5s] --- scenario first-contact passed (2s)
[e2e-live    5s] === scenario adoptions: every suite adopts, in its upstream's form
[e2e-live  229s] all 8 suites adopted, 224s after first contact
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.36/deb: inline, snapshot 1, package_hash_count=84
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.36/deb/.: inline, snapshot 2, package_hash_count=84
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.37/deb: inline, snapshot 7, package_hash_count=36
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.37/deb/.: inline, snapshot 3, package_hash_count=36
[e2e-live  229s]   developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64: inline, snapshot 4, package_hash_count=9758
[e2e-live  229s]   nvidia.github.io/libnvidia-container/stable/deb/amd64: inline, snapshot 8, package_hash_count=170
[e2e-live  229s]   pkg.jenkins.io/debian-stable/binary: detached, snapshot 5, package_hash_count=180
[e2e-live  229s]   download.sublimetext.com/apt/stable: inline, snapshot 6, package_hash_count=78
[e2e-live  229s] --- scenario adoptions passed (224s)
[e2e-live  229s] === scenario updates: every client's apt-get update from the snapshots, clean
[e2e-live  229s] k8s-slash: update clean
[e2e-live  229s] k8s-dot: update clean
[e2e-live  229s] cuda: update clean
[e2e-live  229s] nvidia: update clean
[e2e-live  229s] jenkins: update clean
[e2e-live  230s] sublime: update clean
[e2e-live  230s] --- scenario updates passed (1s)
[e2e-live  230s] === scenario strict: strict mode refuses an undeclared .deb on every host
[e2e-live  230s] refused, as strict mode should: pkgs.k8s.io
[e2e-live  230s] refused, as strict mode should: developer.download.nvidia.com
[e2e-live  230s] refused, as strict mode should: nvidia.github.io
[e2e-live  230s] refused, as strict mode should: pkg.jenkins.io
[e2e-live  230s] refused, as strict mode should: download.sublimetext.com
[e2e-live  230s] --- scenario strict passed (0s)
[e2e-live  230s] === scenario installs: one package per repository, its .deb vouched
[e2e-live  231s] k8s-slash: installed kubectl=1.37.1-1.1, its .deb vouched (path=/core:/stable:/v1.37/deb/amd64/kubectl_1.37.1-1.1_amd64.deb)
[e2e-live  232s] k8s-dot: installed kubectl=1.36.5-1.1, its .deb vouched (path=/core:/stable:/v1.36/deb/amd64/kubectl_1.36.5-1.1_amd64.deb)
[e2e-live  233s] cuda: installed cuda-toolkit-12-config-common=12.9.79-1, its .deb vouched (path=/compute/cuda/repos/debian12/x86_64/./cuda-toolkit-12-config-common_12.9.79-1_all.deb)
[e2e-live  235s] nvidia: installed nvidia-container-toolkit-base=1.20.1-1, its .deb vouched (path=/libnvidia-container/stable/deb/amd64/./nvidia-container-toolkit-base_1.20.1-1_amd64.deb)
[e2e-live  237s] jenkins: installed jenkins=2.580.1, its .deb vouched (path=/debian-stable/binary/jenkins_2.580.1_all.deb)
[e2e-live  239s] sublime: installed sublime-merge=2132, its .deb vouched (path=/files/sublime-merge_build-2132_amd64.deb)
[e2e-live  239s] --- scenario installs passed (9s)
[e2e-live  239s] === scenario whole-run: no refusal but the controls', no panic
[e2e-live  239s] stopping the daemon (the end)
[e2e-live  240s] --- scenario whole-run passed (1s)
[e2e-live  240s] timings: setup 3s first-contact 2s adoptions 224s updates 1s strict 0s installs 9s whole-run 1s
[e2e-live  240s] PASS: live smoke, finished 2026-10-26 19:25:57 UTC
[e2e-live] PASS (build 20s, smoke 240s)
```

The first planning run (19:00 UTC, the same checks with lib.sh's `[e2e-flat live …]` prefix) passed the same way: adoptions 221 s, smoke 245 s, build 51 s. A failure here is triaged as the README's *A failure: the cache, or the vendor?* says: a vendor's change is fixed in the smoke's values; anything the cache does is a finding for the controller, never worked around (ruling R-H2-4).

- [ ] **Step 9: The strict step has teeth**

Mutant (scratch only, revert after): in `smoke.sh`'s `write_config`, `refuse_unvouched_debs = true` → `false`. Run Step 8's command.
Expected: FAIL in the `strict` scenario, at its first control: `[e2e-live  228s] FAIL in scenario 'strict': http://HTTPS///pkgs.k8s.io/core:/stable:/v1.37/deb/amd64/acu-live-control_1.0_amd64.deb answered 403, want 502 (strict mode off for its host?)` (planning run: smoke 230 s), followed by the daemon log's tail. Restore the line; `git diff --no-ext-diff e2e/live/smoke.sh` prints nothing once it is committed (Step 10).

- [ ] **Step 10: Commit**

```bash
git add e2e/live/Dockerfile e2e/live/run.sh e2e/live/smoke.sh e2e/live/README.md Makefile
git commit -F - <<'EOF'
e2e/live: a manual smoke against live flat repositories

The live-upstream smoke the overview's PR #8 parity section asks of
phase H: a release-built daemon, with adoption and strict mode on, and
real apt 2.6 clients in one debian:12 container, against pkgs.k8s.io
(v1.36 and v1.37, in both source forms), NVIDIA CUDA (debian12),
libnvidia-container, pkg.jenkins.io and download.sublimetext.com. Every
suite adopts in its upstream's form (adoption_success), every update is
clean under e2e/flat's error grep (with a control that must fail it),
one package per repository installs with its .deb vouched
(validated_hash=true), strict mode refuses an undeclared .deb on each of
the five hosts (the controls), and nothing else is refused. It sources
e2e/flat's lib.sh, fetches the vendors' keys at run time and is never
run in CI. make e2e-live; e2e/live/README.md.

Planning runs, 2026-10-26: both passed, all 8 suites adopted in 221 and
224 s (the scheduler's tick), smoke 245 and 240 s. With
refuse_unvouched_debs off the strict step fails at its first control.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 4: Operator docs: strict mode on flat hosts, the unsigned confirmation, the captive portal's aftermath, binary `Release.gpg`, `flip_held_ms`

The docs carries marked *H3's*: the overview's "PR #8 parity" H (strict mode's three consequences in `docs/configuration.md`), H2's For H3 items 2 (the unsigned confirmation's real duration in `docs/configuration.md` safeguard 2 and `docs/admin-ui-spec.md`; the README's home is Task 5), 3 (the captive portal's aftermath, `Release.gpg` included), 4 (a binary `Release.gpg`) and 9 (the `flip_held_ms` row, ruling R-H3-2). Before item 3's text calls the `Release.gpg` error transient, a handler test pins that an adopted unsigned snapshot never serves the cached page (design note 4).

**Files:**
- Create: `internal/handler/portal_aftermath_test.go`
- Modify: `docs/configuration.md` (four places), `docs/admin-ui-spec.md` (§4.5, *Unsigned and absent-anchor refusals*), `docs/log-fields.md` (`flip_held_ms`)

**Interfaces:**
- Consumes: the handler test helpers `newFlipStack`, `(*flipStack).cacheRow`, `.track`, `.current`, `.members` (`internal/handler/flip_stack_test.go`, `suite_rows_test.go`), `plainVerifier`, `flipSimHost`, `hdrXCacheSnapshot`; `freshness.NewAdopter`, `(*freshness.Adopter).RunUnsigned`.
- Produces: `TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage`. The README (Task 5), the release notes (Task 6) and spec revision 13 (Task 7) cite these sections by name: `docs/configuration.md` *Flat repositories* under `[freshness]`, *Anchor forms and unsigned repositories* (safeguard 2), and the `[integrity]` section's new paragraph *Flat repositories follow the same host-wide rule*.

- [ ] **Step 1: Pre-flight: what the docs will say, against the code**

Run:

```bash
go test -count=1 -run 'TestServeHTTP_FlatStrictMatrix' -v ./internal/handler/ 2>&1 | grep -E '^\s+--- (PASS|FAIL)' | grep -E 'flat-only|incomplete_flat|third_minor' | sed -E 's/ \([0-9.]+s\)$//'
grep -n '"unvouched_deb_passthrough_no_coverage",\|"incomplete_snapshot_id", snapshotID\|key := host + "|" + path + "|" + strconv.FormatInt(hourBucket, 10)' internal/handler/handler.go
grep -n '"reason", "no_sha256",\|"reason", "no_packages_members",' internal/freshness/adoption.go
grep -n 'retryBaseDelay  = 2 \* time.Minute\|retryJitterFrac = 0.25' internal/freshness/retry.go
grep -n 'minConfirmGap = 5 \* time.Minute\|return max(c.cooldown, minConfirmGap)' internal/freshness/first_adoption.go
grep -n '{{"InRelease", t.inRelease}, {"Release", t.release}}' internal/freshness/anchorless.go
grep -n 'HoldMax = 20 \* time.Second\|timer := time.NewTimer(g.holdMax)\|t.held = now.Sub(joined)' internal/flipgate/gate.go
go test -count=1 -run 'TestSniffReleaseGPG$|TestVerifyDetached_HappyPath_Binary$' ./internal/aptpath/ ./internal/gpg/
```

Expected:

```
    --- PASS: TestServeHTTP_FlatStrictMatrix/a_flat-only_host:_an_undeclared_.deb_elsewhere
    --- PASS: TestServeHTTP_FlatStrictMatrix/complete_dists_and_an_incomplete_flat_snapshot:_an_unvouched_dists_.deb
    --- PASS: TestServeHTTP_FlatStrictMatrix/pkgs.k8s.io:_a_third_minor's_first_adoption_is_running
    --- PASS: TestServeHTTP_FlatStrictMatrix/pkgs.k8s.io:_a_third_minor's_first_adoption_is_queued
2924:	key := host + "|" + path + "|" + strconv.FormatInt(hourBucket, 10)
2949:	h.logger.Info("unvouched_deb_passthrough_no_coverage",
2952:		"incomplete_snapshot_id", snapshotID,
2706:			"reason", "no_packages_members",
2740:			"reason", "no_sha256",
12:	retryBaseDelay  = 2 * time.Minute
14:	retryJitterFrac = 0.25
31:	minConfirmGap = 5 * time.Minute
45:func (c *Checker) confirmGap() time.Duration { return max(c.cooldown, minConfirmGap) }
267:	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
66:	HoldMax = 20 * time.Second
370:	timer := time.NewTimer(g.holdMax)
387:	t.held = now.Sub(joined)
ok  	github.com/linsomniac/apt-cacher-ultra/internal/aptpath	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/gpg	…
```

What each line backs: the four strict-matrix rows are the three PR #8 parity consequences (a flat-only host is strict; an incomplete flat snapshot turns strict mode off for its host, naming it in `unvouched_deb_passthrough_no_coverage`; a third minor's first adoption is waited for while it runs and refused while it is queued); the passthrough line is INFO and keyed per host, path and hour; the two coverage reasons exist; the unsigned confirmation is the first retry (2 minutes, doubling, ±25 %) at least `max(cooldown, 5 min)` after the first observation; the anchorless refresh deletes only the cached `InRelease` and `Release` (so a cached `Release.gpg` stays); a hold's waiter arms its own 20 s timer from when it joined and its wait is measured after the gate's mutex (so `flip_held_ms` can exceed 20000 by the timer's lateness); the cache accepts a binary `Release.gpg` (its sniff and its detached verification). apt's refusal of a binary detached signature is e2e/flat's planning observation on all six images (the H2 plan's deviation 1), not a property of this code. Line numbers are HEAD's (e65f67d); a moved line is fine, a missing one is a stop.

- [ ] **Step 2: Write the portal pin**

Create `internal/handler/portal_aftermath_test.go`:

```go
package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// The captive portal's aftermath (docs/configuration.md, *Flat
// repositories*; e2e/flat's anchorless scenario): a client's miss during
// the outage caches the portal's page as a Release-less flat repository's
// InRelease, Release and Release.gpg, and the anchorless refresh deletes
// only the first two. When that repository later publishes an unsigned
// Release and adopts it, the page cached as its Release.gpg (or InRelease)
// is never served again: an unsigned snapshot's one anchor is its Release,
// and an adopted suite answers a path its snapshot lacks 404, never from
// the url_path row (spec §6.1, "the snapshot is the contract"; spec §10
// item 9 says the same of the inline form). So apt's "Signed file isn't
// valid, got 'NODATA'" for the page ends with the adoption.
func TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := newFlipStack(t, 0)
		const suite = "/fury"
		portal := []byte("<html><body>Sign in to the guest network</body></html>\n")
		s.cacheRow(suite+"/Release.gpg", portal, 1)
		s.cacheRow(suite+"/InRelease", portal, 1)
		pkgs := []byte("Package: p\nArchitecture: amd64\nVersion: 1\nFilename: p_1_amd64.deb\nSize: 1\n" +
			"SHA256: 0000000000000000000000000000000000000000000000000000000000000001\n\n")
		s.members.put("http://"+flipSimHost+suite+"/Packages", pkgs)
		rel := []byte(fmt.Sprintf("Label: fury\nSHA256:\n %x %d Packages\n", sha256.Sum256(pkgs), len(pkgs)))
		s.track(suite)
		get := func(name string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+suite+"/"+name, nil))
			return rec
		}
		if rec := get("Release.gpg"); rec.Code != http.StatusOK || rec.Body.String() != string(portal) {
			t.Fatalf("premise: before the adoption Release.gpg is %d, the portal's page %v; want the cached page",
				rec.Code, rec.Body.String() == string(portal))
		}
		ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: s.c, Fetcher: s.members, Verifier: plainVerifier{},
			HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger(), AdoptUnsigned: true})
		if err != nil {
			t.Fatal(err)
		}
		ref := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}
		if err := ad.RunUnsigned(ctx, ref, rel, "", ""); err != nil {
			t.Fatal(err)
		}
		snap := s.current(suite)
		if snap == "" {
			t.Fatal("premise: the unsigned adoption committed no snapshot")
		}
		for _, name := range []string{"Release.gpg", "InRelease"} {
			if rec := get(name); rec.Code != http.StatusNotFound || rec.Body.String() == string(portal) {
				t.Fatalf("%s after the unsigned adoption: status %d, the portal's page %v; want 404 from the snapshot",
					name, rec.Code, rec.Body.String() == string(portal))
			}
		}
		if rec := get("Release"); rec.Code != http.StatusOK || rec.Body.String() != string(rel) ||
			rec.Header().Get(hdrXCacheSnapshot) != snap {
			t.Fatalf("Release: status %d, snapshot %q, the adopted Release %v; want 200 from snapshot %s",
				rec.Code, rec.Header().Get(hdrXCacheSnapshot), rec.Body.String() == string(rel), snap)
		}
	})
}
```

- [ ] **Step 3: Run it; then its mutant**

Run: `go test -race -count=1 -run 'TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage' -v ./internal/handler/`
Expected: `--- PASS: TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage` (it pins behaviour HEAD already has; the mutant is its red).

Mutant (revert after): in `internal/handler/handler.go`'s `trySnapshotHit`, just before `http.Error(w, "not in snapshot", http.StatusNotFound)`, insert

```go
		if memberPath == "Release.gpg" || memberPath == "InRelease" {
			return false, 0, 0, false, false
		}
```

(an adopted suite's anchor its snapshot lacks falls through to the `url_path` row). The same command fails: `portal_aftermath_test.go:65: Release.gpg after the unsigned adoption: status 200, the portal's page true; want 404 from the snapshot`.

- [ ] **Step 4: `docs/configuration.md`, `[integrity]`: strict mode on flat hosts (the overview's PR #8 parity H)**

`old`:

```markdown
strict mode off for its whole host. A `.deb` whose name begins like an
index file (a package named `icons-foo`, a flat repository's
`Packages_1.0_all.deb`) is a package all the same: vouched, checked and
refused like any other.
```

`new`:

```markdown
strict mode off for its whole host. A `.deb` whose name begins like an
index file (a package named `icons-foo`, a flat repository's
`Packages_1.0_all.deb`) is a package all the same: vouched, checked and
refused like any other.

Flat repositories follow the same host-wide rule. Releases before 1.1.0
never adopted a flat repository, so one never made its host strict; now:

- A host that serves only flat repositories is strict once their signed
  snapshots all have complete coverage: a `.deb` there that no `Packages`
  declares — a file the vendor serves beside the repository without
  listing it — is refused, where earlier releases passed it through.
- A flat snapshot with incomplete coverage turns strict mode off for its
  whole host, as an incomplete `dists/` snapshot does: a `Packages` stanza
  without a SHA256 (`no_sha256`), say, or a signed `Release` that lists no
  root `Packages` (`no_packages_members`). The adoption logs INFO
  `package_coverage_incomplete` with the snapshot and the reason, and a
  `.deb` then served unvouched on that host logs INFO
  `unvouched_deb_passthrough_no_coverage` (at most once an hour per path)
  naming the snapshot as `incomplete_snapshot_id`.
- The first install from a flat repository a client has just added to a
  strict host — a new Kubernetes minor on pkgs.k8s.io, say — waits up to
  15 seconds while that repository's first adoption runs, and is refused
  while the adoption is still waiting for a slot; an attempt after the
  adoption has committed succeeds.
- A flat repository refreshed without a `Release` has no snapshot (see
  *Flat repositories* under `[freshness]`): it never changes its host's
  posture, and on a strict host its packages are refused like any
  unvouched package.
```

- [ ] **Step 5: `docs/configuration.md`, *Anchor forms and unsigned repositories*: safeguard 2's real duration (H2 For H3 item 2; ruling R-H2-6)**

`old`:

```markdown
   since a firewall or an S3 permission error can hide a signature (a 403
   that later turns into 404s confirms after the shorter gap). Until then
   the status page shows "refused: unsigned_unconfirmed". A `Release`
```

`new`:

```markdown
   since a firewall or an S3 permission error can hide a signature (a 403
   that later turns into 404s confirms after the shorter gap). The
   confirming check is the first of the suite's retries (2 minutes,
   doubling, each ±25 %) that comes that long after the first
   observation, so with the default `cooldown` a new unsigned repository
   is adopted at least 5 minutes after its first `apt update` through the
   cache — usually 5 to 15. Until then the status page shows "refused:
   unsigned_unconfirmed". A `Release`
```

- [ ] **Step 6: `docs/configuration.md`, *Flat repositories*: the captive portal's aftermath (H2 For H3 item 3)**

`old`:

```markdown
(`anchorless_unchanged`) during the outage. Flat repositories rarely
publish a plain `Contents-*`.
```

`new`:

```markdown
(`anchorless_unchanged`) during the outage. Flat repositories rarely
publish a plain `Contents-*`.

After an outage that answered every file with an HTML page (a captive
portal, a vendor's maintenance page), a Release-less repository's
clients can see one more error. A client's update during the outage
caches the page as the repository's `InRelease`, `Release` and
`Release.gpg` (a miss stores what upstream answers; apt reports the page
as an invalid signed file, as it would fetching directly). The first
`apt update` after the outage can still get those pages; the check it
triggers finds `InRelease` and `Release` answered 404 again (or, behind
Amazon S3 or CloudFront, 403) and deletes them, so the next update is
clean. The page cached as `Release.gpg` stays: apt never asks for it
while the repository publishes no `Release`, but if the repository later
publishes one, apt fetches `Release.gpg` beside it and prints a
signature error for the page (`Signed file isn't valid, got 'NODATA'`)
until the cache adopts the repository — a signed one at its first check,
an unsigned one after its confirmation (see *Anchor forms and unsigned
repositories*, safeguard 2). An adopted snapshot never serves that page:
it answers `Release.gpg` with its own signature or, for an inline or
unsigned snapshot, 404. `POST /forget` deletes it at once (*Forgetting a
suite* under `[admin]` says when).
```

- [ ] **Step 7: `docs/configuration.md`, *Anchor forms and unsigned repositories*: a binary `Release.gpg` (H2 For H3 item 4)**

`old`:

```markdown
optional file that lists only a SHA512 hash and fails its integrity check
is not retried by automatic repair; the reconcile on unchanged checks
still heals the index files apt requests.
```

`new`:

```markdown
optional file that lists only a SHA512 hash and fails its integrity check
is not retried by automatic repair; the reconcile on unchanged checks
still heals the index files apt requests.

A `Release.gpg` in binary form (not ASCII-armored) is accepted: the cache
adopts a repository that publishes `Release` with such a signature. apt
2.2 to 3.2 refuse it, through the cache or directly ("Detached signature
file … is in unsupported binary format"), so such a repository is
unusable unless it also publishes an `InRelease`, which apt then uses
instead (Jitsi's repository does).
```

- [ ] **Step 8: `docs/admin-ui-spec.md` §4.5: the unsigned confirmation's real duration (H2 For H3 item 2, the final docs review H-1)**

`old`:

```markdown
**Unsigned and absent-anchor refusals (phase E).** `unsigned_unconfirmed`
is an unsigned repository waiting for its second observation
(`max(cooldown, 5 min)` after the first, an hour when the confirming check
sees a 401/403 on a signature file) and clears itself when it adopts;
```

`new`:

```markdown
**Unsigned and absent-anchor refusals (phase E).** `unsigned_unconfirmed`
is an unsigned repository waiting for its second observation — at least
`max(cooldown, 5 min)` after the first, an hour when the confirming check
sees a 401/403 on a signature file; the confirming check is the record's
first retry that late (2 minutes, doubling, each ±25 %), so with the
default `cooldown` the repository adopts usually 5 to 15 minutes after
its first observation — and clears itself when it adopts;
```

- [ ] **Step 9: `docs/log-fields.md`: `flip_held_ms` (H2 For H3 item 9; ruling R-H3-2)**

`old`:

```markdown
- `flip_held_ms` — emitted only when an `InRelease`/`Release` request
  waited in a flip hold (spec C8): how long it waited, in milliseconds
  (at most 20000; part of `duration_ms`). A request waits when it
```

`new`:

```markdown
- `flip_held_ms` — emitted only when an `InRelease`/`Release` request
  waited in a flip hold (spec C8): how long it waited, in milliseconds
  (about 20000 at most: the hold's 20 s bound, whose timer can fire a few
  milliseconds late; part of `duration_ms`). A request waits when it
```

- [ ] **Step 10: Check the edits and the guards**

Run:

```bash
grep -c 'Flat repositories follow the same host-wide rule' docs/configuration.md
grep -c 'usually 5 to 15' docs/configuration.md
grep -c 'usually 5 to 15' docs/admin-ui-spec.md
grep -c "Signed file isn't valid, got 'NODATA'" docs/configuration.md
grep -c 'unsupported binary format' docs/configuration.md
grep -c 'about 20000 at most' docs/log-fields.md
go test -count=1 -run 'TestEveryEventIsDocumented|TestSweptEventNames|TestLogMessagesAreSnakeCase|TestLogMethodsAreOnlyCalledDirectly' ./internal/observability/
gofmt -l internal/handler; golangci-lint run ./internal/handler/...
```

Expected: `1` from each of the six greps, `ok` for `internal/observability`, no file from `gofmt -l`, and `0 issues.`

- [ ] **Step 11: Commit**

```bash
git add internal/handler/portal_aftermath_test.go docs/configuration.md docs/admin-ui-spec.md docs/log-fields.md
git commit -F - <<'EOF'
Docs: strict mode on flat hosts, the unsigned wait, a portal's aftermath

docs/configuration.md: flat repositories under strict mode's host-wide
rule (a flat-only host is now strict; an incomplete flat snapshot turns
it off for its host; a newly added suite's first install is refused
while its first adoption is queued), as the overview's PR #8 parity
section asks; the unsigned confirmation's real duration (at least 5
minutes, usually 5 to 15; ruling R-H2-6), here and in
docs/admin-ui-spec.md; the captive portal's aftermath, including the
page cached as Release.gpg, which an adopted snapshot never serves; and
a binary Release.gpg, which the cache adopts and apt refuses.
docs/log-fields.md: flip_held_ms is about 20000 at most, since the
hold's timer can fire late (ruling R-H3-2).

TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage pins the
portal claim: after an unsigned adoption the cached page under
Release.gpg and InRelease is answered 404. A mutant that lets an
adopted suite's missing anchor fall through to its url_path row fails
it (portal_aftermath_test.go:65).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 5: README: repositories, suite reaping and `/forget`, the admin answers, testing

Spec §8's README items — supported repository layouts (flat forms, the `/` and `./` twins, the host-root limitation, unsigned, Release-less and SHA512-only support), the adoption-enabled requirement, GC and `/forget` — and the carries marked *H3's*: the overview's *(E)* (unsigned and SHA512-only repositories, eager first adoption, `adopt_unsigned`'s threat notes), *(F)* (suite reaping and `POST /forget` beside *Recovering a degraded repository*, and `/forget` in the sentence that names `POST /reconcile`), *(G)* (flat repositories), H2's For H3 items 1 (testing, pointing at e2e/flat's triage subsection by name) and 2 (the unsigned confirmation's real duration), and the 1.1.0 `/reconcile` answers (400 and 413, which the README's example omits). The README stays an operator's document (design note 3): what is supported and how to use it, linking to `docs/configuration.md` for the rules. Its *Status* paragraph is the maintainer's and stays as it is.

**Files:**
- Modify: `README.md` (eight places)

**Interfaces:**
- Consumes: the `docs/configuration.md` sections Task 4 finished (*Flat repositories*, *Anchor forms and unsigned repositories*, *Eager first adoption*, *Suite reaping*, *Forgetting a suite (`POST /forget`)*, the `[integrity]` section); `e2e/live/README.md` (Task 3); `e2e/flat/README.md`'s *Flake, environment or daemon bug?*.
- Produces: the README sections `## Repositories`, `## Suites no client uses`, `## Forgetting a suite`, which the release notes (Task 6) link to.

- [ ] **Step 1: Pre-flight: the facts the README states, and the link targets**

Run:

```bash
grep -n '^### Eager first adoption$\|^### Flat repositories$\|^### Anchor forms and unsigned repositories$\|^### Suite reaping$\|^### Forgetting a suite (`POST /forget`)$\|^## `\[integrity\]`: corruption checks$\|^## `\[admin\]`: status, metrics, and maintenance$' docs/configuration.md
grep -n '^### Flake, environment or daemon bug?$' e2e/flat/README.md
test -f e2e/live/README.md && echo "e2e/live/README.md exists"
grep -n 'cfg.GC.SuiteIdleTTL.Duration = 720 \* time.Hour' internal/config/config.go
grep -n '^\s*reapInterval = time.Hour\|^\s*reapUptimeFloor = time.Hour' internal/freshness/reaper.go
grep -n 'http.StatusBadRequest\|http.StatusRequestEntityTooLarge\|http.StatusForbidden' internal/admin/handlers.go
grep -c 'flat_host_root_unsupported' internal/handler/flat_root.go
grep -n 'grep -q ACU_E2E_ build/apt-cacher-ultra' .github/workflows/ci.yaml
grep -n 'grep -q ACU_E2E_ build/apt-cacher-ultra' .github/workflows/release.yaml
```

Expected: the seven `docs/configuration.md` headings (the README's anchors are GitHub's slugs of them: `#eager-first-adoption`, `#flat-repositories`, `#anchor-forms-and-unsigned-repositories`, `#suite-reaping`, `#forgetting-a-suite-post-forget`, `#integrity-corruption-checks`, `#admin-status-metrics-and-maintenance`); e2e/flat's triage heading (slug `#flake-environment-or-daemon-bug`); `e2e/live/README.md exists`; the 720 h default of `gc.suite_idle_ttl`; the reaper's hourly interval and its one-hour floor; `/reconcile`'s and `/forget`'s 400, 413 and 403 answers (`parseSuiteForm`, `requireMutationGuard`); `flat_host_root_unsupported` in the handler; CI's and the release workflow's `ACU_E2E_` guard. Planning run, at HEAD:

```
274:### Eager first adoption
330:### Flat repositories
530:### Anchor forms and unsigned repositories
787:## `[integrity]`: corruption checks
875:### Suite reaping
1000:## `[admin]`: status, metrics, and maintenance
1053:### Forgetting a suite (`POST /forget`)
323:### Flake, environment or daemon bug?
e2e/live/README.md exists
804:		cfg.GC.SuiteIdleTTL.Duration = 720 * time.Hour
28:	reapInterval = time.Hour
33:	reapUptimeFloor = time.Hour
590:		http.Error(w, "host and suite are required", http.StatusBadRequest)
665:		http.Error(w, "host and suite are required; scheme is http or https", http.StatusBadRequest)
716:			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
718:			http.Error(w, "malformed form body", http.StatusBadRequest)
728:		http.Error(w, "host and suite are required; scheme is http or https", http.StatusBadRequest)
735:			http.StatusBadRequest)
789:		http.Error(w, "this endpoint requires the header X-ACU-Admin: 1", http.StatusForbidden)
796:			http.StatusForbidden)
802:			"or set admin.htpasswd_file", http.StatusForbidden)
2
32:          if grep -q ACU_E2E_ build/apt-cacher-ultra; then
111:          if grep -q ACU_E2E_ build/apt-cacher-ultra; then
```

(Line numbers are HEAD's; after Task 4 the `docs/configuration.md` headings sit lower. `parseSuiteForm` (716–735) and `requireMutationGuard` (789–802) serve both endpoints; 590 is `/reconcile`'s missing host or suite.)

The other facts the new text states are Task 4's pre-flight (the unsigned confirmation, strict mode) and `docs/configuration.md`'s own sections, which earlier phases checked: a suite reaped is tracked again at its next request (*Suite reaping*), `/forget` is the way out of `upstream_older`, `rollback_refused` and `unsigned_downgrade_refused` (*Forgetting a suite*), with adoption disabled suites are only checked for changes (`adoption.enabled`'s row), a Release-less flat repository is refreshed file by file and an HTML page never replaces an index (*Flat repositories*), and e2e/flat's times (its README's *Running it*: 24.5 minutes for all six; the H2 plan's For H3 item 1: about 45 at worst, cold).

- [ ] **Step 2: The two feature bullets**

`old`:

```markdown
- Freshness control — periodic and on-request InRelease checks with cooldown; hot-package proactive refresh.
```

`new`:

```markdown
- Freshness control — periodic and on-request InRelease checks with cooldown; hot-package proactive refresh.
- Flat, unsigned and Release-less repositories — refreshed like `dists/` repositories, with adoption enabled (see
  [Repositories](#repositories)).
```

`old`:

```markdown
- Garbage collection — refcounted blobs are swept when no snapshot references them.
```

`new`:

```markdown
- Garbage collection — refcounted blobs are swept when no snapshot references them.
- Suite reaping — a suite no client has asked for in 30 days is dropped with its cached index files (see
  [Suites no client uses](#suites-no-client-uses)).
```

- [ ] **Step 3: `/forget` beside `/reconcile` in *Configuration***

`old`:

```markdown
Snapshot adoption is initially disabled. The admin interface, including
`POST /reconcile`, listens on `127.0.0.1:6789`. Review these settings before
making either listener available beyond your trusted network.
```

`new`:

```markdown
Snapshot adoption is initially disabled. The admin interface, including
`POST /reconcile` and `POST /forget`, listens on `127.0.0.1:6789`. Review
these settings before making either listener available beyond your trusted
network.
```

- [ ] **Step 4: The *Repositories* section, before *Inspecting the cache***

`old`:

```markdown
## Inspecting the cache
```

`new`:

```markdown
## Repositories

Snapshot adoption (`[adoption] enabled = true`, after loading the
repositories' signing keys) is what keeps metadata fresh: the cache checks
each repository its clients use and serves every change as one coherent
snapshot, adopted at the repository's first `apt update` through the cache
when an adoption slot is free. It is off in the packaged configuration:
until you enable it, each metadata file is served as the cache first
stored it, and suites are only checked for changes.

- **`dists/` repositories** (Debian, Ubuntu, most vendors), signed with an
  `InRelease` or with `Release` and `Release.gpg`; a suite moves between
  the two forms by itself. `Release` files that list only SHA512 hashes
  are supported.
- **Flat repositories**, with no `dists/` tree, configured either way apt
  accepts: `deb https://pkgs.k8s.io/core:/stable:/v1.34/deb/ /` or
  `deb https://pkgs.k8s.io/core:/stable:/v1.34/deb ./`, and with a
  subdirectory, `deb https://pkg.jenkins.io/debian-stable binary/`. The
  `/` and `./` forms of one directory are tracked as two suites. A flat
  repository at the root of its host (`deb https://example.com/ ./`) is
  not supported: it is served from the cache but never refreshed, with a
  WARN `flat_host_root_unsupported`.
- **Unsigned repositories** (a `Release` with no signature, for
  `[trusted=yes]` clients) are adopted as unsigned snapshots, served
  coherently but never trusted: they vouch for no package. A new one is
  adopted once two checks have seen the same `Release` at least 5 minutes
  apart (with the retry schedule, usually 5 to 15 minutes after its first
  `apt update` through the cache), and a repository the cache has seen
  signed is never adopted unsigned. Read the [threat
  notes](docs/configuration.md#anchor-forms-and-unsigned-repositories)
  before serving them; `adoption.adopt_unsigned = false` turns this off.
- **Flat repositories that publish no `Release` at all** (Gemfury,
  goreleaser) are refreshed file by file: each cached index is
  revalidated with upstream, and an HTML page never replaces one.

The [configuration reference](docs/configuration.md) has the rules and
limits: [eager first adoption](docs/configuration.md#eager-first-adoption),
[flat repositories](docs/configuration.md#flat-repositories), and [strict
mode](docs/configuration.md#integrity-corruption-checks)
(`integrity.refuse_unvouched_debs`), which now protects flat hosts too.

## Inspecting the cache
```

- [ ] **Step 5: `/reconcile`'s answers**

`old`:

```text
# 202 Accepted = reconcile triggered (runs asynchronously)
# 403 = the header is missing, or another host name without admin.htpasswd_file
# 409 = busy, unknown suite, no current snapshot, or no free adoption slot
```

`new`:

```text
# 202 Accepted = reconcile triggered (runs asynchronously)
# 400 = host or suite missing, a scheme other than http or https, a host
#       holding scheme:// (pass the scheme in scheme), or a body that is not a form
# 403 = the header is missing, a browser request from another origin, or
#       another host name without admin.htpasswd_file
# 409 = busy, unknown suite, no current snapshot, or no free adoption slot
# 413 = a body over 8 KiB
```

- [ ] **Step 6: *Suites no client uses* and *Forgetting a suite*, after *Recovering a degraded repository***

`old`:

```markdown
suite heals.

## Build
```

`new`:

````markdown
suite heals.

## Suites no client uses

A suite no client has asked for in `gc.suite_idle_ttl` (30 days by
default) is reaped by a pass that runs every hour: it is no longer
tracked, and its cached index files and snapshots are deleted. Its
packages stay cached, without the version retention its snapshot gave
them. A client that comes back makes it new again, fetched from upstream.
The pass runs whatever `gc.enabled` says. An offline or archive cache that
must keep serving suites nobody asks for sets `gc.suite_idle_ttl = "0s"`
— before upgrading from 1.0, whose suites count as idle from the last
request before the upgrade ([Suite reaping](docs/configuration.md#suite-reaping)).

## Forgetting a suite

`POST /forget` reaps one suite at once, however recently it was used. It
is the way out of two refusal states the status page shows: a deliberate
upstream rollback (`refused: upstream_older` or `rollback_refused`) and a
vendor that stopped signing (`refused: unsigned_downgrade_refused`). The
next request makes the suite new again and, with adoption enabled, adopts
what upstream serves then. It takes the same header and host rules as
`POST /reconcile`; pass the scheme, host and suite path the status page
shows:

```sh
curl -sS -X POST http://127.0.0.1:6789/forget -H 'X-ACU-Admin: 1' \
    --data-urlencode 'scheme=https' \
    --data-urlencode 'host=packages.microsoft.com' \
    --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
# 202 Accepted = forgotten; the body names what was deleted
# 409 = busy (try again) or no such suite
```

[Forgetting a suite](docs/configuration.md#forgetting-a-suite-post-forget)
lists every answer and what forgetting deletes.

## Build
````

- [ ] **Step 7: Testing, under *Build***

`old`:

```text
make deb             # .deb package (nfpm must be installed)
make clean
```

`new`:

```text
make deb             # .deb package (nfpm must be installed)
make e2e-flat        # real apt against fixture repositories through the cache (Docker)
make e2e-live        # the manual smoke against live flat repositories (Docker, network)
make clean
```

`old`:

```markdown
Like the container build, this uses Go's DNS resolver. This also applies to
`make deb`; race-enabled tests keep cgo available.
```

`new`:

```markdown
Like the container build, this uses Go's DNS resolver. This also applies to
`make deb`; race-enabled tests keep cgo available.

`make e2e-flat` runs real apt clients — apt 2.2 to 3.2, on six Debian and
Ubuntu images — against fixtures of every supported repository layout
through the cache: about 25 minutes in sequence with cached image builds,
up to about 45 cold. CI runs one job per image; when CI's `e2e-flat` is
red, start at [Flake, environment or daemon
bug?](e2e/flat/README.md#flake-environment-or-daemon-bug). `make e2e-live`
checks the live repositories those fixtures copy, by hand before a
release ([e2e/live](e2e/live/README.md)). A binary built with `-tags
e2ehooks` is a test binary and must never ship: CI and the release
workflow refuse one.
```

- [ ] **Step 8: Check the README: its sections and every link**

Run:

```bash
grep -n '^## ' README.md
grep -c 'POST /reconcile` and `POST /forget`' README.md
T=$(mktemp -d)
cat > "$T/check_links.py" <<'EOF'
#!/usr/bin/env python3
"""Check the repository links of Markdown files (phase H3): each link to a
file of this repository — relative, or https://github.com/linsomniac/
apt-cacher-ultra/blob/main/<path> — names an existing file, and each
#anchor names a heading of that file (GitHub's slug: lower case, every
character but letters, digits, spaces, hyphens and underscores dropped,
spaces made hyphens). Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = "https://github.com/linsomniac/apt-cacher-ultra/blob/main/"


def slugs(path: pathlib.Path) -> set:
    out = set()
    fence = False
    for line in path.read_text().splitlines():
        if line.startswith("```"):
            fence = not fence
        m = re.match(r"#+ (.*)", line)
        if m and not fence:
            out.add(re.sub(r"[^a-z0-9 _-]", "", m.group(1).lower()).replace(" ", "-"))
    return out


bad = 0
for name in sys.argv[1:]:
    src = pathlib.Path(name)
    for target in sorted(set(re.findall(r"\]\(([^)\s]+)\)", src.read_text()))):
        if target.startswith(BLOB):
            rel = target[len(BLOB):]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = f.is_file() and (not anchor or anchor in slugs(f))
        bad += not ok
        print(f"{'ok  ' if ok else 'BAD '} {name}: {target}")
sys.exit(1 if bad else 0)
EOF
python3 "$T/check_links.py" README.md | grep -c '^ok'; python3 "$T/check_links.py" README.md | grep '^BAD' || echo "no broken link"
```

Expected: the sections in this order — `Status`, `Features`, `Quickstart`, `Configuration`, `Repositories`, `Inspecting the cache`, `Recovering a degraded repository`, `Suites no client uses`, `Forgetting a suite`, `Build`, `License`; `1`; `21` links checked `ok` (the planning run's count on the edited README); and `no broken link`. The checker resolves every repository link — relative, or `https://github.com/linsomniac/apt-cacher-ultra/blob/main/…` — to a file and a heading slug; a link to a heading that is missing (a broken one, as a mutation in planning showed: `BAD  linkmut.md: docs/configuration.md#no-such-heading`) prints `BAD`.

- [ ] **Step 9: Commit**

```bash
git add README.md
git commit -F - <<'EOF'
README: flat, unsigned and Release-less repositories; reaping; /forget

Spec section 8's README items: the repository layouts the cache keeps
fresh (dists/, flat in both source forms and the host-root limitation,
signed in either form, SHA512-only, unsigned with its 5-to-15-minute
confirmation and threat notes, Release-less), and that adoption must be
enabled for any of it; suite reaping, with the offline-cache setting to
make before upgrading; POST /forget beside POST /reconcile, whose
example now lists its 400 and 413 answers; and testing: make e2e-flat
(with e2e/flat's triage subsection) and make e2e-live. The overview's
E, F and G carries and the H2 plan's For H3 items 1 and 2.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 6: The 1.1.0 release notes

`docs/release-notes/1.1.0.md` (ruling R-H3-1; open decision D1), with every carry marked *H3's*: *Upgrade* — `gc.suite_idle_ttl` for offline and archive caches (the overview's *(F)* carry), the G-1 header and `Host` guard on `POST /reconcile` (*(G-1)*), `/reconcile`'s 400s (*(H1)*), the `require_signature = false` and suite-seed behaviour changes (*(H1)*), the legacy trust audit's corrupt-anchor deletion and deferred snapshots kept at every `gc.keep_displaced` value (the H1 plan's *For H3* with the overview's correction); *Logging* — the six removed 1.0.x messages with their replacements, the log-query note, the two repeating WARNs, the computed inventory of new events, fields, values and metrics since 1.0.x (design note 1; the H1 *For H3* lists names R-H3's and R-H6's renames use, which were never released, so the notes list new events by their names only), `e2e_commit_delay_enabled` marked test-binary-only (H2 For H3 item 6); the alert recipes (reaper, GC); the captive portal's aftermath, `Release.gpg` included (H2 For H3 item 3; Task 4 pinned and documented it); the e2e harness and the live smoke's results (Task 3; ruling R-H3-8). Design note 2 gives the structure. Links are absolute, to `main`, as `packaging/config/config.toml.default`'s are, so they work in the tree and pasted into GitHub.

The H1 *For H3* list said `acu_clock_steps_detected_total` "now also counts the reaper's forward steps" and that `suite_reaped` "gains" fields; the overview's *(H1)* correction applies: both are new since 1.0.x, so the notes list them as new. It said R-H1 applies "with `gc.keep_displaced = 0`"; it applies at every value (the notes say "whatever `gc.keep_displaced` is").

**Files:**
- Create: `docs/release-notes/1.1.0.md`

**Interfaces:**
- Consumes: the README sections of Task 5, the `docs/configuration.md` sections of Task 4, `e2e/live`'s results (Task 3).
- Produces: `docs/release-notes/1.1.0.md`, which Task 7 records in the spec and the overview, and Task 9 checks again on the final tree (Step 4's two checkers).

- [ ] **Step 1: Pre-flight: the inventories, from the code at 1.0.2 and at HEAD**

Run (from the repository root; `git archive` writes a file and `tar` reads it, two commands, no pipe):

```bash
D=/tmp/h3-notes; rm -rf "$D"; mkdir -p "$D/old" "$D/new"
git archive -o "$D/old.tar" 1.0.2
git archive -o "$D/new.tar" HEAD
tar -x -C "$D/old" -f "$D/old.tar"
tar -x -C "$D/new" -f "$D/new.tar"
cp internal/observability/event_names_test.go "$D/old/internal/observability/"
cat > "$D/zz_inventory_test.go" <<'EOF'
package observability

import (
	"fmt"
	"os"
	"sort"
	"testing"
)

// TestZZInventory writes every production log message with its first
// site, sorted, to $INV_OUT (phase H3: the 1.1.0 release notes'
// inventory). A planning aid run on archive copies, never committed.
func TestZZInventory(t *testing.T) {
	msgs := logMessages(t)
	keys := make([]string, 0, len(msgs))
	for k := range msgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f, err := os.Create(os.Getenv("INV_OUT"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, k := range keys {
		fmt.Fprintf(f, "%s\t%s\n", k, msgs[k])
	}
}
EOF
for d in old new; do
    cp "$D/zz_inventory_test.go" "$D/$d/internal/observability/"
    (cd "$D/$d" && INV_OUT="$D/$d.tsv" go test -count=1 -run TestZZInventory ./internal/observability/ > /dev/null) || echo "inventory of $d failed"
done
wc -l < "$D/old.tsv"; wc -l < "$D/new.tsv"
```

Expected: `182` and `253` (the log messages production code logs at 1.0.2 and at HEAD; the inventory runs the guard tests' AST walk, `logMessages`, which 1.0.2 lacks, so its file is copied in). The trees and the test file stay outside the worktree. `git rev-parse 1.0.2^{commit}` is `536a1ab…`, the branch's merge base with `main`: the notes cover `main..HEAD`.

- [ ] **Step 2: Pre-flight: the facts the notes state**

Run (`O` is 1.0.2's tree from Step 1):

```bash
O=/tmp/h3-notes/old
grep -n 'const CurrentSchemaVersion = ' internal/cache/schema.go $O/internal/cache/schema.go
grep -n 'refusing to downgrade' $O/internal/cache/schema.go
grep -n 'filepath.Join(dir, "cache.db")\|journal_mode(WAL)' internal/cache/cache.go
grep -n 'legacyAuditBatchSize        = 100\|logger.Info("legacy_trust_audit_progress"' internal/freshness/legacy_audit.go
grep -c 'Nothing was changed' internal/freshness/legacy_audit.go
grep -n 'reapInterval = time.Hour\|reapUptimeFloor = time.Hour' internal/freshness/reaper.go
grep -n 'cfg.GC.SuiteIdleTTL.Duration = 720 \* time.Hour' internal/config/config.go
grep -n 'const interestWindowSeconds = 7 \* 24 \* 60 \* 60\|now-interestWindowSeconds' internal/cache/schema.go
grep -n 'w.Header().Set("Allow", "POST, OPTIONS")' internal/admin/server.go
grep -n 'request body too large\\n", http.StatusRequestEntityTooLarge\|reconcile not triggered' $O/internal/admin/handlers.go
grep -n 'logger.Warn("adoption_require_signature_disabled")' cmd/apt-cacher-ultra/main.go
grep -n 'unsigned upstream metadata would be adopted' $O/cmd/apt-cacher-ultra/main.go
grep -n 'handler: put url row' internal/handler/handler.go
grep -n 'seed suite_freshness failed' $O/internal/handler/handler.go
grep -n 'c.logger.Warn("freshness: persist failure-bump failed"\|c.logger.Info("freshness check failed"\|errors.New("unexpected success status")\|errUnexpectedStatus, res.Status' internal/freshness/check.go
grep -n 'every path refused by validateMemberPath\|no stanza with Package, Directory and a well-formed Checksums-Sha256 entry' internal/freshness/sources_parse.go
grep -n 'no stanzas with Package + Directory + Checksums-Sha256' $O/internal/freshness/sources_parse.go
grep -n 'a.logger.Warn("adoption_member_skipped"' internal/freshness/adoption.go $O/internal/freshness/adoption.go
grep -n 'retryBaseDelay  = 2 \* time.Minute\|retryMaxDelay   = time.Hour\|retryJitterFrac = 0.25' internal/freshness/retry.go
grep -n 'const checkWorkers = 4' internal/freshness/scheduler.go
grep -n 'strictWait = 15 \* time.Second' internal/freshness/inflight.go
grep -n 'db.SetMaxIdleConns(maxIdleConns())' internal/cache/cache.go
grep -n '"acu_gc_last_run_unixtime",' -A 3 internal/gc/metrics.go
go test -count=1 -run 'TestMutatingEndpoints_|TestServeListeners_StartsTheReaperWhateverGCSays|TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory|TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot|TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor|TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic|TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals|TestStartupPass_ReturnsAPanicAsAnError|TestAptEncodePath_Table|TestServeHTTP_FlatStrictMatrix' ./cmd/apt-cacher-ultra/ ./internal/admin/ ./internal/cache/ ./internal/handler/ ./internal/gpg/ ./internal/freshness/ ./internal/gc/ ./internal/aptpath/
```

Expected (the planning run; line numbers are HEAD's and 1.0.2's, the order of a multi-pattern grep's lines may differ):

```
internal/cache/schema.go:18:const CurrentSchemaVersion = 7
/tmp/h3-notes/old/internal/cache/schema.go:18:const CurrentSchemaVersion = 6
309:		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); refusing to downgrade",
114:	db, err := openDB(filepath.Join(dir, "cache.db"))
174:	q.Add("_pragma", "journal_mode(WAL)")
26:	legacyAuditBatchSize        = 100
190:		logger.Info("legacy_trust_audit_progress",
2
28:	reapInterval = time.Hour
33:	reapUptimeFloor = time.Hour
804:		cfg.GC.SuiteIdleTTL.Duration = 720 * time.Hour
380:const interestWindowSeconds = 7 * 24 * 60 * 60
390:  ?)`, now-interestWindowSeconds); err != nil {
402:				w.Header().Set("Allow", "POST, OPTIONS")
565:		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
580:		http.Error(w, "reconcile not triggered (busy, unknown, or no current snapshot)\n", http.StatusConflict)
416:		logger.Warn("adoption_require_signature_disabled")
402:		logger.Warn("adoption.require_signature = false: unsigned upstream metadata would be adopted (auditable per SPEC2 §5.1)")
2235:		return sfResult{err: fmt.Errorf("handler: put url row: %w", err), status: fres.Status}
1570:			h.logger.Warn("seed suite_freshness failed",
307:		c.logger.Warn("freshness: persist failure-bump failed",
313:	c.logger.Info("freshness check failed",
349:var errUnexpectedStatus = errors.New("unexpected success status")
370:		return fmt.Errorf("%w %d", errUnexpectedStatus, res.Status)
213:		return nil, stats, fmt.Errorf("sources: every path refused by validateMemberPath (%d skipped)", stats.PathsSkipped)
216:		return nil, stats, errors.New("sources: no stanza with Package, Directory and a well-formed Checksums-Sha256 entry")
191:		return nil, stats, errors.New("sources: no stanzas with Package + Directory + Checksums-Sha256")
internal/freshness/adoption.go:1397:					a.logger.Warn("adoption_member_skipped",
internal/freshness/adoption.go:1454:				a.logger.Warn("adoption_member_skipped",
internal/freshness/adoption.go:2239:			a.logger.Warn("adoption_member_skipped",
/tmp/h3-notes/old/internal/freshness/adoption.go:1108:					a.logger.Warn("adoption_member_skipped",
/tmp/h3-notes/old/internal/freshness/adoption.go:1164:				a.logger.Warn("adoption_member_skipped",
/tmp/h3-notes/old/internal/freshness/adoption.go:1764:			a.logger.Warn("adoption_member_skipped",
12:	retryBaseDelay  = 2 * time.Minute
13:	retryMaxDelay   = time.Hour
14:	retryJitterFrac = 0.25
100:const checkWorkers = 4
17:	strictWait = 15 * time.Second
210:	db.SetMaxIdleConns(maxIdleConns())
127:		"acu_gc_last_run_unixtime",
128-		"Unix-seconds timestamp of the last GC run completion, by phase (SPEC5 §10.4.5).",
129-		metrics.DefaultMaxSeries,
130-		"phase",
ok  	github.com/linsomniac/apt-cacher-ultra/cmd/apt-cacher-ultra	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/admin	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/cache	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/handler	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/gpg	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/freshness	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/gc	…
ok  	github.com/linsomniac/apt-cacher-ultra/internal/aptpath	…
```

What each line backs, in the notes' order: the one-way migration and 1.0.x's refusal message; the database file and its WAL (the copy to keep); the audit's progress line per 100 snapshots and its two "Nothing was changed" pool refusals; the reaper's hourly interval and one-hour floor, the 720 h default, and the backfill that counts a suite with no request from 7 days before the upgrade (720 h − 7 d ≈ 23 days); `OPTIONS /reconcile`'s `Allow` (and 1.0.x's 413 for any unparseable body and its 409 text); the renamed `require_signature` WARN and 1.0.x's text; the suite seed inside the store's transaction and 1.0.x's WARN; the replacements of the removed messages and the `unexpected success status <code>` error; `source_parse_failed`'s new and old texts; `adoption_member_skipped` at WARN in both releases (the first of each three is the `arch_not_in_allowlist` skip); the retry schedule, the four check workers, the 15 s strict wait, the idle connections kept; the GC gauge's `phase` label (`max()` in the alert covers both phases). The tests: the admin guard and answers (`TestMutatingEndpoints_*`: the header, cross-origin, rebinding hosts, `htpasswd_file`, 400s, one newline, `OPTIONS`), the reaper started whatever `gc.enabled` says, R-H1 at `keep_displaced` 0 and 1, R-H2's refetch, the I-1 and I-7 fixes, GC's startup panic, the `+`/`~` encoding, strict mode on flat hosts. The rest — the eager-adoption, unsigned, flat, flip and refusal rules the notes summarise — is the docs Tasks 4 and 5 checked and link to; `e2e/live`'s results are Task 3's run.

- [ ] **Step 3: Write `docs/release-notes/1.1.0.md`**

````markdown
# apt-cacher-ultra 1.1.0

1.1.0 makes flat apt repositories — pkgs.k8s.io, NVIDIA CUDA,
libnvidia-container, pkg.jenkins.io, Sublime Text and the like — first-class:
the cache now refreshes them, adopts them as coherent snapshots and vouches
their packages for strict mode. It also adopts unsigned, Release-less and
SHA512-only repositories, adopts a new repository at its first
`apt update`, backs off from failing upstreams instead of retrying them at
every check, keeps clients from straddling a snapshot switch, and drops
suites no client uses. The flat-repository problem was reported and first
analysed by Venkat (@venkatamutyala) in PR #8; the flat-repository support
here is based on that work.

**Read *Before you upgrade* below first.** Some changes can
surprise a running installation: a one-way database migration, an hourly
reaper that deletes idle suites whatever `gc.enabled` says, and a new
header that `POST /reconcile` requires.

## Before you upgrade

- **The database is migrated one way (schema 6 to 7), and audited.** At the
  first start the daemon migrates `<cache.dir>/cache.db`, then audits every
  snapshot an earlier release adopted before it serves anything (INFO
  `legacy_trust_audit_progress` every 100 snapshots; connections made
  meanwhile wait, and apt clients may time out once). A 1.0.x binary refuses
  the migrated database (`database schema version 7 is newer than this
  binary supports (6); refusing to downgrade`): to keep a way back, stop the
  daemon and copy `cache.db` (and any `cache.db-wal` beside it) before
  upgrading. The audit refuses to start, changing nothing, when the cache's
  `pool/` directory is missing, unreadable or empty while snapshots need
  it: mount or restore it. A snapshot whose stored `InRelease` is not a
  clean signed file, or whose pool file is missing or corrupt, is
  quarantined (WARN `legacy_snapshot_quarantined`): its suite is served
  from the ordinary cache until it adopts again, and a corrupt one's cached
  copies and pool file are deleted, so clients fetch a fresh one. One whose
  pool file cannot be read at all is deferred (ERROR
  `legacy_trust_audit_deferred` at every start) and kept, by snapshot GC
  too, whatever `gc.keep_displaced` is, until the pool is fixed or the
  suite is reaped or forgotten. See [Upgrading an existing
  cache](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#upgrading-an-existing-cache).
- **Offline and archive caches: set `gc.suite_idle_ttl = "0s"` before
  upgrading.** A new suite reaper runs an hour after the start and then
  hourly, whatever `gc.enabled` says, and deletes the suites no client has
  asked for in `gc.suite_idle_ttl` (720 h, 30 days, by default), with their
  cached index files — counted from the last request before the upgrade
  (from 7 days before it for a suite with none, so such a suite goes about
  23 days after the upgrade). An install that relied on `gc.enabled = false`
  to keep everything needs the setting too. A reaped suite comes back at its
  next request, fetched from upstream. See [Suite
  reaping](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#suite-reaping).
- **`POST /reconcile` needs the header `X-ACU-Admin: 1`**, as the new
  `POST /forget` does: a script that calls it must add it (403 without). And
  without `admin.htpasswd_file` both refuse (403) a `Host` that is not an IP
  address, `localhost` or the host name in `admin.listen` — a guard against
  DNS rebinding. Behind a reverse proxy that forwards the client's `Host`,
  set `htpasswd_file` or have the proxy send `Host: 127.0.0.1`. A browser
  request from another origin is refused too. Its other answers: 400 (was
  413) for a body that is not a form, 400 (was 409) for a `scheme` other
  than `http` or `https` and for a `host` holding `://`, 409 also when no
  adoption slot is free, 413 for a body over 8 KiB; every error body ends
  in one newline (it was two), and `OPTIONS /reconcile` names
  `Allow: POST, OPTIONS`. See [the admin
  listener](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#admin-status-metrics-and-maintenance).
- **`adoption.require_signature = false` no longer adopts unsigned
  metadata.** It only lets the daemon start with an empty keyring; the
  inline form is adopted only from a decodable clearsigned `InRelease`, in
  every mode. Unsigned repositories are adopted by
  `adoption.adopt_unsigned` (default `true`) instead, as unsigned snapshots
  that vouch for no package. This affects only a deployment whose
  `InRelease` URL serves a body that is not clearsigned — which apt itself
  rejects on its first line. The startup WARN that names the setting is now
  `adoption_require_signature_disabled`.
- **A database error creating a suite's row now fails the request.** The
  row is created in the same transaction as the file's cached copy, so such
  an error is a 502 with ERROR `unclassified fetch error` (`err`
  `handler: put url row: …`), where 1.0.x served the file and logged WARN
  `seed suite_freshness failed`. A refusal at the new `freshness.max_suites`
  caps is no error: WARN `suite_seed_rejected`, and the file is still served
  and cached.
- **Strict mode now covers flat repositories.** With
  `integrity.refuse_unvouched_debs = true`, a host whose flat repositories'
  signed snapshots are all complete becomes strict as they adopt: a `.deb`
  there that no `Packages` declares is refused (502) where 1.0.x passed it
  through; the first install from a newly added flat repository is refused
  while its first adoption waits for a slot; and a flat snapshot with
  incomplete coverage turns strict mode off for its whole host, as a
  `dists/` one does. See [strict
  mode](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#integrity-corruption-checks).
- **First adoptions after the upgrade.** With adoption enabled, each tracked
  suite without a snapshot that a client asked for in the 7 days before the
  upgrade, or asks for later, is adopted at its first check — one at a time
  with the default `freshness.max_concurrent_adoptions = 2`, while signed
  updates keep a slot of their own; `3` runs two at once. Snapshots adopted
  before the upgrade carry no SHA512 by-hash names: such requests get 404
  from the snapshot, and apt then fetches the file by name, until the
  suite's next adoption.
- **Smaller behaviour changes.** Upstream request lines with `+` or `~` in
  the path are now encoded exactly as apt encodes them. A few `dists/`
  paths apt never sends no longer get a suite: they are served but not
  tracked. A repository that publishes only `Release` and `Release.gpg`
  (pkg.jenkins.io) and that the cache first sees after the upgrade is not
  tracked while adoption is disabled; one 1.0.x already tracked stays
  tracked.
- **New configuration keys**, each with the default shown; the packaged
  `config.toml` lists them with comments, and a key left out of yours takes
  its default:

  | Key | Default | Rule |
  |---|---|---|
  | `adoption.adopt_unsigned` | `true` | adopt repositories that publish an unsigned `Release` |
  | `adoption.flip_quiet_period` | `"10s"` | `0s` disables the flip barrier; otherwise `1s` to `15s` |
  | `adoption.max_duration` | `"30m"` | at least `1m` |
  | `freshness.max_suites` | `5000` | at least 10; a tenth of it per upstream host |
  | `gc.suite_idle_ttl` | `"720h"` | `0s` disables suite reaping; otherwise at least `24h` |

## What's new

- **Flat repositories** are tracked, refreshed and adopted like `dists/`
  ones, in both source forms (`deb <uri>/ /` and `deb <uri> ./`, two suites
  that share their signing history) and with a subdirectory
  (`deb <uri> binary/`); their packages are vouched by the paths apt
  requests, including `./…` Filenames and Filenames above the `Release`
  (pkg.jenkins.io, Sublime Text). A flat repository at the root of its host
  is not supported (WARN `flat_host_root_unsupported`). See [Flat
  repositories](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#flat-repositories).
- **Unsigned repositories** are adopted as unsigned snapshots — served
  coherently, never trusted, never vouching — after two checks see the same
  `Release` at least 5 minutes apart (usually 5 to 15 minutes after the
  first `apt update`), and never once the cache has seen the repository
  signed. **Release-less flat repositories** (Gemfury, goreleaser) are
  refreshed file by file. **SHA512-only `Release` files** adopt, and by-hash
  requests are answered for SHA256 and SHA512. Read the [threat
  notes](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#anchor-forms-and-unsigned-repositories) before
  serving unsigned repositories; `adoption.adopt_unsigned = false` turns
  them off.
- **Outages that answer with HTML** — a captive portal, a vendor's
  maintenance page — never replace a Release-less repository's cached
  index. The pages a client's own update cached during the outage as the
  repository's `InRelease` and `Release` are deleted by the first check
  after it, so the next update is clean; one cached as its `Release.gpg`
  stays, harmless while the repository publishes no `Release`, and is never
  served once the repository adopts one. See [Flat
  repositories](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#flat-repositories).
- **Eager first adoption:** a new repository is adopted at its first
  `apt update` through the cache, when an adoption slot is free, in
  whichever form its upstream publishes; a suite nobody asked for in 7 days
  costs no upstream request. See [Eager first
  adoption](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#eager-first-adoption).
- **Retry discipline:** a failed adoption is retried after 2 minutes,
  doubling to an hour (±25 %), instead of at every check, and new upstream
  content is tried at once; seven outcomes are refusals — states the status
  page shows as "refused: …" — not failures. Checks run on four workers with
  per-host fairness and their own backoff. See [How failed adoptions are
  retried](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#how-failed-adoptions-are-retried) and
  [Capacity and fairness](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#capacity-and-fairness).
- **Flip consistency:** a new snapshot waits (bounded below apt's 30 s
  timeout) until clients that received the old `InRelease` have had
  `adoption.flip_quiet_period` to fetch its indexes, and by-hash requests
  are served from displaced snapshots. See [Flip
  consistency](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#flip-consistency).
- **Suite lifecycle:** the hourly suite reaper (above) and `POST /forget`,
  which reaps one suite at once — the way out of a deliberate upstream
  rollback and of a vendor that stopped signing. See [Forgetting a
  suite](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#forgetting-a-suite-post-forget).
- **Strict mode waits** up to 15 seconds for a signed adoption running on
  the request's host before it refuses a `.deb`, so a package from an
  update being adopted is served.
- **The status page and JSON** gain (additive): `suites[]`'s `scheme`,
  `flat`, `anchorless`, `anchor_form`, `last_requested_unixtime`,
  `check_fail_count`, `adoption_fail_reason`, `adoption_fail_count`,
  `adoption_fail_first_unixtime`, `adoption_next_retry_unixtime` and
  `adoption_refused`; `recent_adoptions[].scheme`; `gc.retired_reaped`; and
  on the page a *Last requested* column, failures and refusals in the
  Adoption cell (`checks failing ×k · backoff ends in …`, `refreshed without
  a Release`), and `scheme://host` cells.
- **Robustness:** an `InRelease` the signature library cannot decode no
  longer crashes the daemon; a backward clock step no longer freezes
  freshness checks; panics in freshness checks, adoptions, the reaper and
  GC — its startup pass included — are recovered and logged; idle SQLite
  read connections are kept between requests (fewer connection churns
  starving the writer).

## Logging and metrics

Every new log message is a snake_case event name, and a test keeps it so;
the prose messages 1.0.x logs are unchanged but for the six below. Every
event this release adds is documented in [the log field
reference](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/log-fields.md).

### Removed messages

An alert that matches one of these stops firing on upgrade:

| 1.0.x message | Level | 1.1.0 instead |
|---|---|---|
| `adoption.require_signature = false: unsigned upstream metadata would be adopted (auditable per SPEC2 §5.1)` | WARN | WARN `adoption_require_signature_disabled` — and a behaviour change: see *Before you upgrade* |
| `freshness: no baseline blob hash, skipping` | DEBUG | DEBUG `freshness_check_skipped_no_baseline_anchor_hash` (`anchor` is `InRelease` or `Release`) |
| `freshness: no baseline Release blob hash, skipping` | DEBUG | the same event |
| `freshness: unexpected success status` (`status`) | ERROR | INFO `freshness check failed` with `err` `unexpected success status <code>` and `result=failed`, counted in `acu_freshness_check_total{result="failed"}`: match the `err` text, not the level |
| `freshness: persist anomaly failed` | WARN | WARN `freshness: persist failure-bump failed`: the anomaly now goes through the failed-check bookkeeping |
| `seed suite_freshness failed` | WARN | no line of its own: a 502 and ERROR `unclassified fetch error` (see *Before you upgrade*) |

One changed text: `source_parse_failed`'s `error` field said `sources: no
stanzas with Package + Directory + Checksums-Sha256`; it now says
`sources: every path refused by validateMemberPath (N skipped)` or
`sources: no stanza with Package, Directory and a well-formed
Checksums-Sha256 entry`.

### Matching an event exactly

On 1.0.x `freshness_check` was the only event with that prefix; 1.1.0 adds
`freshness_check_cancelled`, `freshness_check_ended_by_shutdown`,
`freshness_check_skipped_no_baseline_anchor_hash` (DEBUG) and
`freshness_check_panicked` (ERROR), so a substring query such as
`|= "freshness_check"` matches them too, and so does `msg=freshness_check`.
Match the name exactly: in JSON logs `"msg":"freshness_check"` with its
closing quote; in text logs, where the name is written unquoted,
`msg=freshness_check` followed by a space or the end of the line (the regex
`msg=freshness_check( |$)`); or a parsed field, such as LogQL's
`| json | msg="freshness_check"` or `| logfmt | msg="freshness_check"`.

### WARNs that repeat at every adoption

Two WARNs 1.0.x already logs repeat at every adoption with nothing for the
operator to do: `source_parse_failed` for a `Sources` file whose every path
was refused (`source_paths_skipped`, INFO, gives the count), and
`adoption_member_skipped` with `reason=arch_not_in_allowlist`, one per
member your own `adoption.architectures` filters out. If you page on every
WARN, exclude them; a later release makes them INFO.

### New events

ERROR — alert on these: `adoption_panicked`, `freshness_check_panicked`,
`gc_panicked`, `handler_goroutine_panicked`, `legacy_anchor_discard_failed`,
`legacy_trust_audit_deferred`, `shutdown_wait_panicked`.

WARN: `admin_forget_failed`, `adoption_failing_persistently`,
`adoption_failure_record_failed`, `adoption_read_prior_snapshot_failed`,
`adoption_refused_persistently`, `adoption_require_signature_disabled`,
`adoption_retries_reschedule_failed`, `adoption_served_anchor_read_failed`,
`adoption_signed_anchor_remark_failed` (ERROR when a cached anchor's pool
file cannot be read), `clock_step_detected`,
`displaced_snapshot_lookup_failed`, `flat_host_root_unsupported`,
`freshness_anchorless_index_over_cap`,
`freshness_clamp_adoption_retry_failed`,
`freshness_clear_adoption_failure_failed`,
`freshness_delete_anchorless_rows_failed`,
`freshness_persist_failure_bump_after_panic_failed`,
`freshness_read_anchor_rows_failed`,
`freshness_read_current_snapshot_failed`,
`freshness_read_suite_row_after_panic_failed`,
`legacy_snapshot_quarantined`, `reconcile_snapshot_read_failed`,
`snapshot_adopted_at_lookup_failed`, `suite_cached_anchor_read_failed`,
`suite_caps_exceeded`, `suite_cap_usage_read_failed`,
`suite_file_head_read_failed`, `suite_onboarding_failed`,
`suite_reaper_delete_metadata_failed`,
`suite_reaper_junk_anchor_reads_failed`,
`suite_reaper_list_suites_failed`,
`suite_reaper_list_unadopted_anchors_failed`,
`suite_reaper_list_unadopted_flat_indexes_failed`,
`suite_reaper_marker_sweep_failed`,
`suite_reaper_read_signed_evidence_failed`,
`suite_reaper_read_suite_row_failed`, `suite_reaper_reap_failed`,
`suite_seed_rejected`, `suite_signed_seen_backstop_read_failed`,
`suite_signed_seen_mark_failed`. Several log at DEBUG instead when a
shutdown cut the work short.

INFO: `admin_forget_requested`, `adoption_abandoned_at_shutdown`,
`adoption_deferred`, `adoption_flip_barrier`, `adoption_refused`,
`adoption_retries_rescheduled`, `legacy_trust_audit_completed`,
`legacy_trust_audit_progress`, `shutdown_waiting_for_adoptions`,
`signed_seen_markers_swept`, `source_paths_skipped`, `suite_evicted`,
`suite_forgotten`, `suite_reaped`, `suite_reaper_run_complete`.

DEBUG: `adoption_repair_declarations_unavailable`,
`freshness_anchorless_index_sniff_failed_kept`,
`freshness_check_cancelled`, `freshness_check_ended_by_shutdown`,
`freshness_check_skipped_no_baseline_anchor_hash`,
`freshness_deferred_never_adopted_suite_paced_until_retry`,
`freshness_never_adopted_suite_dormant_no_client_created_row`,
`freshness_never_adopted_suite_dormant_no_recent_interest`,
`freshness_refused_suite_paced_until_retry`,
`freshness_repair_deferred_no_adoption_slot`,
`legacy_trust_audit_cached_anchor_skipped`, `suite_not_forgotten` (WARN
for `reason=changed`, which should never occur), `suite_reaper_suite_busy`,
`suite_touch_failed`.

`docs/log-fields.md` also documents `e2e_commit_delay_enabled`, which only
the end-to-end tests' binary (built with `-tags e2ehooks`) logs: a release
binary never does, and `ACU_E2E_*` variables are not operator settings.

### New fields and values

- The `request` line: `snapshot_displaced` (a by-hash request served from
  a displaced snapshot) and `flip_held_ms` (an `InRelease`/`Release`
  request that waited in a flip hold).
- `freshness_check`: `result` gains `changed_deferred`, `changed_queued`,
  `first_adoption`, `first_adoption_queued`, `form_changed`,
  `anchor_absent`, `anchorless_refreshed` and `anchorless_unchanged`;
  `form` gains `unsigned`, `probe` and `anchorless`; the anchorless results
  carry `rows_replaced` and `rows_deleted`.
- The adoption outcome — `acu_adoption_total{outcome}`,
  `acu_adoption_duration_seconds{outcome}` and `recent_adoptions[].outcome`
  — gains `refused`, `release_date_in_future`, `timed_out` and `panicked`.
- `package_hash_conflict`'s `declared` entries name each row's
  `suite_path`; `admin_reconcile_requested` gains `canonical_scheme` beside
  `scheme`; `gc_run_complete` gains `retired_reaped`; the stale-suites WARN
  (`freshness: adopted suites stale beyond threshold`) gains `paced_stale`.

### New metrics

Counters: `acu_anchorless_sniff_rejected_total{host}`,
`acu_clock_steps_detected_total`, `acu_flip_gate_panics_total`,
`acu_flip_hold_expired_total`, `acu_flip_hold_skipped_total`,
`acu_flip_quiet_cut_short_total`, `acu_flip_slow_anchor_reads_total`,
`acu_flip_waiters_over_cap_total`, `acu_freshness_unlogged_panics_total`,
`acu_gc_retired_reaped_total`, `acu_legacy_snapshots_quarantined_total`,
`acu_repair_deferred_total{host}`, `acu_serve_byhash_displaced_total{host}`,
`acu_strict_adoption_waits_total{host}`,
`acu_suite_seed_rejected_total{host}`, `acu_suites_reaped_total{reason}`.

Gauges: `acu_adoption_failing_suites`, `acu_flip_barrier_active`,
`acu_freshness_waiting_for_slot`, `acu_suite_reaper_last_run_unixtime`,
`acu_suites_seed_rejected`.

None carries a suite label.

### Alerts worth adding

- **The suite reaper stopped:** `absent(acu_suite_reaper_last_run_unixtime)`
  held by a Prometheus `for: 3h`, or
  `time() - acu_suite_reaper_last_run_unixtime > 3 * 3600`. A restart or
  one failed run never pages; a reaper that stopped completing runs pages
  within about three hours.
- **GC:** ERROR `gc_panicked`, and
  `time() - max(acu_gc_last_run_unixtime) > 2 * 86400 + 3600` with the
  default 24 h `gc.interval` (two intervals and an hour): a panic in GC no
  longer stops the daemon, at startup or in a tick, so the gauge's age is
  what shows GC stopped completing; one failed tick never pages.
- **Flips slower than their hold:** `rate(acu_flip_hold_expired_total[1h]) > 0`
  (a commit slower than 20 s; see [Flip
  consistency](https://github.com/linsomniac/apt-cacher-ultra/blob/main/docs/configuration.md#flip-consistency)).
- Any ERROR above, and `acu_freshness_unlogged_panics_total` or
  `acu_flip_gate_panics_total` above 0: each is a bug; please report it.

## Testing

- **`make e2e-flat`** is new: real apt clients — apt 2.2 to 3.2, on six
  Debian and Ubuntu images — against fixtures of every supported repository
  layout through the cache, about 25 minutes in sequence; CI runs one job
  per image. Fixtures only: it never reaches a live repository. `e2e/deb`,
  the package-install check, was updated for eager first adoption.
- **`make e2e-live`** is the manual smoke against live flat repositories,
  run before a release. On 2026-10-26, with adoption and strict mode on:
  - pkgs.k8s.io v1.36 and v1.37, each in both source forms (four suites):
    adopted inline; `kubectl` 1.37.1 and 1.36.5 installed, vouched.
  - NVIDIA CUDA (debian12/x86_64; 9,758 packages): adopted inline;
    `cuda-toolkit-12-config-common` installed, vouched (`./…` Filenames).
  - libnvidia-container (a SHA512-only `Release` dated 2018): adopted
    inline; `nvidia-container-toolkit-base` installed, vouched.
  - pkg.jenkins.io (`binary/`, `Release` and `Release.gpg` only): adopted
    detached; `jenkins` 2.580.1 installed, vouched (Filenames one level
    above the `Release`).
  - download.sublimetext.com (`apt/stable/`): adopted inline;
    `sublime-merge` 2132 installed, vouched (Filenames at the host root).

  Every `apt-get update` was clean, strict mode refused an undeclared
  `.deb` on each of the five hosts, and nothing else was refused.
- Release binaries never contain the end-to-end tests' seam: CI and the
  release workflow refuse a binary built with `-tags e2ehooks`.

## Credits

Thanks to Venkat (@venkatamutyala), who reported and first analysed the
flat-repository problem in PR #8 ("Refresh flat repositories: give them a
suite identity"); the flat-repository support in this release is based on
that work. Their PR also served as the reference for the end-to-end and
strict-mode tests that port its cases.
````

The credits paragraph is open decision D5: the maintainer confirms or rewords it.

- [ ] **Step 4: Check the notes against the code, and their links**

Run (with Step 1's `/tmp/h3-notes`):

```bash
D=/tmp/h3-notes
cat > "$D/check_notes.py" <<'EOF'
#!/usr/bin/env python3
"""Check docs/release-notes/1.1.0.md's inventories against the code.

Usage: check_notes.py OLD-TREE NEW-TREE OLD-INVENTORY NEW-INVENTORY NOTES
The trees are `git archive` copies of 1.0.2 and HEAD; the inventories are
the TSVs TestZZInventory wrote (every production log message, tab, its
first site). Prints one line per check and exits 1 on any mismatch."""
import pathlib
import re
import sys

old, new = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
inv_old = {l.split("\t")[0] for l in pathlib.Path(sys.argv[3]).read_text().splitlines()}
inv_new = {l.split("\t")[0] for l in pathlib.Path(sys.argv[4]).read_text().splitlines()}
notes = pathlib.Path(sys.argv[5]).read_text()
bad = 0


def section(title: str) -> str:
    """The notes' text from heading `title` to the next heading."""
    m = re.search(r"^#+ " + re.escape(title) + r"\n(.*?)(?=^#+ |\Z)", notes, re.M | re.S)
    if not m:
        sys.exit(f"no section {title!r} in the notes")
    return m.group(1)


def names(text: str, pattern: str = r"[a-z][a-z0-9_]*") -> set:
    return set(re.findall(r"`(" + pattern + r")(?:\{[a-z_]+\})?`", text))


def check(what: str, got: set, want: set) -> None:
    global bad
    if got == want:
        print(f"ok   {what}: {len(want)}")
        return
    bad += 1
    print(f"FAIL {what}: missing {sorted(want - got)}, extra {sorted(got - want)}")


def go_sources(tree: pathlib.Path):
    for p in list(tree.glob("internal/**/*.go")) + list(tree.glob("cmd/**/*.go")):
        if not p.name.endswith("_test.go"):
            yield p.read_text()


def literals(tree: pathlib.Path, pattern: str) -> set:
    out = set()
    for s in go_sources(tree):
        out |= set(re.findall(r'"(' + pattern + r')"', s))
    return out


# 1. Events: the new ones (every message only HEAD logs) and the removed
# ones (every message only 1.0.2 logs).
snake = re.compile(r"^[a-z][a-z0-9]*(_[a-z0-9]+)*$")
new_events = {m for m in inv_new - inv_old}
check("new events (snake_case, test-binary one included)", names(section("New events")), new_events)
assert all(snake.match(e) for e in new_events), "a new message is not snake_case"
removed = set(re.findall(r"^\| `([^`]+)`", section("Removed messages"), re.M))
check("removed messages", removed, inv_old - inv_new)

# 2. Metrics: every "acu_..." literal only HEAD has, none only 1.0.2 has.
acu = r"acu_[a-z0-9_]+"
check("new metrics", names(section("New metrics"), acu), literals(new, acu) - literals(old, acu))
check("metrics removed (none)", set(), literals(old, acu) - literals(new, acu))

# 3. Config keys: every toml tag only HEAD's config has.
def tags(tree):
    return set(re.findall(r'toml:"([a-z_]+)"', (tree / "internal/config/config.go").read_text()))


table = re.findall(r"^\s*\| `([a-z_]+)\.([a-z_]+)` \|", section("Before you upgrade"), re.M)
check("new config keys", {k for _, k in table}, tags(new) - tags(old))
check("config keys removed (none)", set(), tags(old) - tags(new))

# 4. The admin JSON: suites[]'s new fields, gc.retired_reaped and
# recent_adoptions[].scheme, from the structs.
def struct_tags(tree, struct):
    for s in (p.read_text() for p in (tree / "internal/admin").glob("*.go") if not p.name.endswith("_test.go")):
        m = re.search(r"type " + struct + r" struct \{(.*?)\n\}", s, re.S)
        if m:
            return set(re.findall(r'json:"([a-z_]+)', m.group(1)))
    sys.exit(f"no struct {struct}")


status = notes[notes.find("**The status page and JSON**"):]
status = status[:status.find("\n- **")]
want = {"suites[]." + f for f in struct_tags(new, "suiteEntry") - struct_tags(old, "suiteEntry")}
want |= {"recent_adoptions[]." + f for f in struct_tags(new, "adoptionEntry") - struct_tags(old, "adoptionEntry")}
want |= {"gc." + f for f in struct_tags(new, "gcInfo") - struct_tags(old, "gcInfo")}
got = {"suites[]." + f for f in names(status.split("`recent_adoptions")[0])}
got |= {m for m in re.findall(r"`((?:recent_adoptions\[\]|gc)\.[a-z_]+)`", status)}
check("admin JSON fields", got, want)

# 5. The new result, form and outcome values: string literals in HEAD's
# production code that 1.0.2's lacks.
vals = section("New fields and values")
named = set()
for line in re.findall(r"`result` gains (.*?);|`form` gains (.*?);|— gains (.*?)\.", vals, re.S):
    for part in line:
        named |= names(part)
for v in sorted(named):
    in_new, in_old = v in literals(new, r"[a-z_]+"), v in literals(old, r"[a-z_]+")
    if not in_new or in_old:
        bad += 1
        print(f"FAIL value {v}: in HEAD {in_new}, in 1.0.2 {in_old}")
print(f"ok   new values, each a literal only HEAD has: {len(named)}" if named else "FAIL no values parsed")

# 6. Each new event's level group is its docs/log-fields.md row's level
# (the first word of the Level cell: "WARN; ERROR when …" is WARN).
levels = {}
for line in (new / "docs/log-fields.md").read_text().splitlines():
    if line.startswith("| `"):
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        lvl = re.match(r"(ERROR|WARN|INFO|DEBUG)\b", cells[1]) if len(cells) > 1 else None
        for n in re.findall(r"`([a-z0-9_]+)`", cells[0]) if lvl else []:
            levels.setdefault(n, lvl.group(1))
grouped = set()
for para in section("New events").split("\n\n"):
    m = re.match(r"(ERROR|WARN|INFO|DEBUG)\b", para.strip())
    if not m:
        continue
    for n in names(para):
        if n in new_events:
            grouped.add(n)
            if levels.get(n) != m.group(1):
                bad += 1
                print(f"FAIL level of {n}: listed {m.group(1)}, documented {levels.get(n)}")
check("events in a level group (all but the test-binary one)", grouped, new_events - {"e2e_commit_delay_enabled"})

sys.exit(1 if bad else 0)
EOF
python3 "$D/check_notes.py" "$D/old" "$D/new" "$D/old.tsv" "$D/new.tsv" docs/release-notes/1.1.0.md
cat > "$D/check_links.py" <<'EOF'
#!/usr/bin/env python3
"""Check the repository links of Markdown files (phase H3): each link to a
file of this repository — relative, or https://github.com/linsomniac/
apt-cacher-ultra/blob/main/<path> — names an existing file, and each
#anchor names a heading of that file (GitHub's slug: lower case, every
character but letters, digits, spaces, hyphens and underscores dropped,
spaces made hyphens). Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = "https://github.com/linsomniac/apt-cacher-ultra/blob/main/"


def slugs(path: pathlib.Path) -> set:
    out = set()
    fence = False
    for line in path.read_text().splitlines():
        if line.startswith("```"):
            fence = not fence
        m = re.match(r"#+ (.*)", line)
        if m and not fence:
            out.add(re.sub(r"[^a-z0-9 _-]", "", m.group(1).lower()).replace(" ", "-"))
    return out


bad = 0
for name in sys.argv[1:]:
    src = pathlib.Path(name)
    for target in sorted(set(re.findall(r"\]\(([^)\s]+)\)", src.read_text()))):
        if target.startswith(BLOB):
            rel = target[len(BLOB):]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = f.is_file() and (not anchor or anchor in slugs(f))
        bad += not ok
        print(f"{'ok  ' if ok else 'BAD '} {name}: {target}")
sys.exit(1 if bad else 0)
EOF
python3 "$D/check_links.py" docs/release-notes/1.1.0.md | grep -c '^ok'; python3 "$D/check_links.py" docs/release-notes/1.1.0.md | grep '^BAD' || echo "no broken link"
```

Expected:

```
ok   new events (snake_case, test-binary one included): 77
ok   removed messages: 6
ok   new metrics: 21
ok   metrics removed (none): 0
ok   new config keys: 5
ok   config keys removed (none): 0
ok   admin JSON fields: 13
ok   new values, each a literal only HEAD has: 15
ok   events in a level group (all but the test-binary one): 76
12
no broken link
```

The checker has teeth (planning mutations): dropping `suite_touch_failed` and `acu_flip_barrier_active` from the notes printed `FAIL new events …: missing ['suite_touch_failed']` and `FAIL new metrics: missing ['acu_flip_barrier_active']`; listing `gc_panicked` under WARN printed `FAIL level of gc_panicked: listed WARN, documented ERROR`.

- [ ] **Step 5: Commit**

```bash
git add docs/release-notes/1.1.0.md
git commit -F - <<'EOF'
Release notes for 1.1.0

docs/release-notes/1.1.0.md, for the maintainer to paste into the
GitHub release (ruling R-H3-1). Before you upgrade: the one-way schema
migration and the trust audit, gc.suite_idle_ttl for offline and
archive caches, the X-ACU-Admin header and Host guard on POST
/reconcile and its new answers, require_signature = false no longer
adopting unsigned metadata, a suite-row error now failing the store,
strict mode on flat hosts, first adoptions after the upgrade, the new
keys. Then what is new, the logging changes (the six removed messages
and their replacements, exact-match queries, the two repeating WARNs,
76 new events by level, new fields, values and 21 metrics), alert
recipes, the end-to-end tests and the live smoke's results, credits.

Every inventory is checked against the code at 1.0.2 and at HEAD
(check_notes.py: events, removed messages, metrics, config keys, admin
JSON fields, values, levels), every link against its heading, and every
other claim by a grep or a named test (the task's pre-flight).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 7: Spec revision 13, `docs/open-decisions.md` and the overview

The spec's revision 13 records H2's deviations 1 and 2 as built, H2's design notes 5 and 7, the commit-delay full-grep deviation (H2's For H3 item 5), the error grep as built, the live smoke in §7, and what H3 changes (C11's list not exhaustive, `flip_held_ms`, §8's two new documents, §9's H row), keeping the revision table's convention: the newest revision first in the `Date` line's parentheses. `docs/open-decisions.md` is the home that outlives H3 (ruling R-H3-3; open decision D3) for the H2 For H3 item 11 carries, the captive-portal `Release.gpg` follow-up (item 3), R-H5/GD-5 for confirmation, the cache writer's fail-fast on panic (H1 parked item 16), the toolchain (D2), the pre-RC steps only the maintainer can take (H2 items 7 and 8), the two repeating WARNs and the simulation near its budget (the overview's *(H1)* carries); each entry names the maintainer as owner, its evidence and a recommendation. The overview marks every *H3's* carry done with its task and points its decisions block at the new file.

**Files:**
- Create: `docs/open-decisions.md`
- Modify: `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (the `Date` line, §7, §8, §9's H row, C11), `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (the 16 *H3's* carries, the H3 row, the spec revision, the cross-phase interfaces, the decisions block)

**Interfaces:**
- Consumes: Tasks 1–6's results (their names appear in the done markers); the live smoke's run (Task 3).
- Produces: `docs/open-decisions.md`, to which Task 9 appends what the final review defers.

- [ ] **Step 1: Pre-flight: the evidence `docs/open-decisions.md` cites**

Run:

```bash
grep -n 'func (c \*Cache) writer()' internal/cache/cache.go; grep -c 'recover()' internal/cache/cache.go
grep -n '{{"InRelease", t.inRelease}, {"Release", t.release}}' internal/freshness/anchorless.go
grep -n 'func SniffReleaseGPG' internal/aptpath/*.go
grep -n '"acu_adoption_members_skipped_total",\|"acu_adoption_sources_parsed_total",' internal/freshness/metrics.go
grep -c 'concurrency:' .github/workflows/ci.yaml
grep -n 'E2E_STEP_MINUTES: \|gating check' .github/workflows/ci.yaml
grep -n 'NFPM_VERSION: 2.43.0\|sha256sum \* > SHA256SUMS' .github/workflows/release.yaml
grep -n 'nfpm@v2.40.0' e2e/deb/Dockerfile
grep -n '"aculan-deb-test:${ver}"' e2e/deb/run.sh
grep -c "go-version: '1.25'" .github/workflows/ci.yaml .github/workflows/release.yaml
grep -n 'FROM golang:1.25' e2e/flat/Dockerfile e2e/deb/Dockerfile e2e/Dockerfile.cache
curl -fsS --max-time 30 'https://go.dev/dl/?mode=json' | python3 -c 'import json, sys; print(" ".join(r["version"] for r in json.load(sys.stdin)))'
```

Expected (planning run):

```
264:func (c *Cache) writer() {
0
267:	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
internal/aptpath/sniff.go:44:func SniffReleaseGPG(head []byte) bool {
227:		"acu_adoption_members_skipped_total",
285:		"acu_adoption_sources_parsed_total",
0
86:  # under the packaged user. This is the gating check that the
138:          E2E_STEP_MINUTES: ${{ matrix.unsigned && 27 || 12 }}
29:      NFPM_VERSION: 2.43.0
147:          (cd dist && sha256sum * > SHA256SUMS)
51:RUN GOBIN=/usr/local/bin go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.40.0
60:            -t "aculan-deb-test:${ver}" . ; then
.github/workflows/ci.yaml:2
.github/workflows/release.yaml:1
e2e/flat/Dockerfile:21:FROM golang:1.25-bookworm AS build
e2e/deb/Dockerfile:39:FROM golang:1.25-bookworm AS build
e2e/Dockerfile.cache:7:FROM golang:1.25-alpine AS build
go1.27.3 go1.26.10
```

(The order of two-file greps' lines may differ. go.dev's list moves with Go's patch releases: what matters is that 1.25 is not on it.) A line that differs is a stop: correct the entry that cites it.

- [ ] **Step 2: Write `docs/open-decisions.md`**

````markdown
# Open decisions

Decisions that outlive the work that raised them, each the maintainer's
(Sean Reifschneider). An entry says what is open, its evidence, the
recommendation and when it was raised. When he rules, the entry moves to
*Decided* with the date and the ruling, and whoever does the work cites it.
The flat-repositories branch (phases A–H, 2026-09 to 2026-10) seeded this
file; the decisions it made along the way are in its overview's decisions
block (`docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`).

## Before the 1.1.0 release candidate

Steps only the maintainer can take, since they need a push or a tag:

1. **The first CI run must be green in full** (raised 2026-10-26, the H2
   plan's For H3 item 7). It is the first run on GitHub's runners of the
   `e2e-flat` matrix (its `unsigned` boolean, the derived timeouts, the
   `timeout -s INT` wrapper), of the `test` job's `e2ehooks seam` step and
   its binary guard, and of e2e/deb's eager-adoption fix. Re-run the
   matrix a few times on the pushed branch: every time so far comes from a
   12-core machine, and public runners have 4 vCPUs. Watch the
   `debian:11` row's build time (`archive.debian.org` is its slowest
   input) and `REACH`, the bound that depends most on runner speed: the
   `${POLL_TOOK}s after the publish` lines of `pr8-table`, `frozen-date`,
   `anchorless`, `two-suites` and `mirror-mode` (4–8 s locally, against
   22 s). The release workflow's `Verify no e2e test seam` step runs first
   on the RC tag: check it there.
2. **The first CI run's job times against the timeouts** (the H2 plan's For
   H3 item 8). A short job over 10 minutes, or a long-wait job over 25:
   raise its `timeout-minutes` with the measured reason, and its
   `E2E_STEP_MINUTES` with it (3 minutes under the timeout). Expected:
   short jobs 5–8 minutes; long-wait jobs 8–12, and 16–21 when an unsigned
   record waits for its third retry (about one CI run in four).
3. **The Go toolchain** (below): approve the H3 plan's Task 8 or not.
4. **`make e2e-live`** on the tree you tag (e2e/live/README.md): the H3
   plan ran it during planning and again on its final tree.
5. **The release notes** (`docs/release-notes/1.1.0.md`): paste them into
   the GitHub release, and confirm their location and their credit wording
   for PR #8 (the H3 plan's open decisions D1 and D5).

## Open

- **The Go toolchain** (raised 2026-09-08 by the security review's
  finding H-3, and 2026-10-25 by the H2 plan's For H3 item 11; facts
  2026-10-26). CI, the release workflow and the four e2e Docker builds
  use Go 1.25 (`setup-go` `'1.25'`, `golang:1.25-bookworm`,
  `golang:1.25-alpine`; `go.mod` says `go 1.25.0`). Go supports a major
  release until two newer ones exist: 1.25 has had no fix since 1.27.0
  (its last release is go1.25.14; go.dev lists go1.27.3 and go1.26.10 as
  supported). Under go1.27.3 the full test suite, vet, the build, the
  `e2ehooks` tests and every e2e image's build stage passed during the H3
  planning; CI's golangci-lint must then move from v2.12.2, which cannot
  read Go 1.27's export data, to v2.14.0, which reports no issue.
  Recommendation: build with Go 1.27 everywhere, keeping `go.mod`'s
  `go 1.25.0` (the H3 plan's Task 8), before the RC. The security review
  also asks for `govulncheck` in CI (H-3): a separate decision.
- **Confirm ruling R-H5 (GD-5): an adopted unsigned flat suite whose
  `Release` disappears keeps its snapshot** (raised 2026-10-25). It shows
  "refused: anchor_absent" and logs `adoption_refused_persistently` until
  `POST /forget` turns it into a Release-less suite. Evidence: the
  flat-repositories spec, §10 item 12; the H1 plan's ruling R-H5;
  `docs/configuration.md`, *Flat repositories*, last paragraph.
  Recommendation: confirm — it is rare, the kept snapshot keeps serving
  when an upstream dies outright, and an automatic drop would be a new
  state transition with trust and serving consequences.
- **Confirm that the cache's writer is fail-fast on a panic** (raised
  2026-10-25, the H1 final review's parked item 16). `internal/cache`'s
  writer goroutine (`(*Cache).writer`, `internal/cache/cache.go`) has no
  `recover`: a panic inside a write operation ends the process, and
  systemd restarts it. Recommendation: confirm, and record it in an
  `AIDEV-NOTE` there — a panic mid-transaction leaves the one write
  connection in an unknown state, and a restart rolls the transaction
  back. (The freshness, GC and request-handling goroutines recover
  theirs.)
- **Delete a cached `Release.gpg` that fails its check in the refresh
  without a `Release`** (raised 2026-10-26, the H2 plan's For H3 item 3).
  A captive portal's page cached as a Release-less repository's
  `Release.gpg` stays after the outage (`internal/freshness/anchorless.go`
  deletes only the cached `InRelease` and `Release`); harmless until the
  repository publishes a `Release`, after which apt prints a signature
  error until the cache adopts it (`docs/configuration.md`, *Flat
  repositories*; pinned by
  `TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage`).
  Recommendation: do it after 1.1.0 — the anchors' delete loop takes a
  third name, judged by the existing `Release.gpg` check
  (`aptpath.SniffReleaseGPG`).
- **Two WARNs that repeat at every adoption become INFO** (raised
  2026-10-25, the H1 final ops review M-10): `source_parse_failed` for a
  `Sources` file whose every path was refused, and
  `adoption_member_skipped` with `reason=arch_not_in_allowlist`, one per
  member `adoption.architectures` filters out. Both are on 1.0.x already,
  so changing their level is a behaviour change; the 1.1.0 release notes
  announce it. Recommendation: INFO in the next minor release, the
  counters (`acu_adoption_members_skipped_total`,
  `acu_adoption_sources_parsed_total`) as the signal.
- **A `concurrency` group in `ci.yaml`** (raised 2026-10-26, the H2 final
  CI review M-1). A superseded pull-request push keeps all its jobs
  running, the two long-wait `e2e-flat` rows included. Recommendation:
  `group: ci-${{ github.event.pull_request.number || github.ref }}` with
  `cancel-in-progress` for pull requests only, so `main` is never
  cancelled.
- **actionlint in CI** (raised 2026-10-26, the H2 final review M-6): a
  10-second job, after fixing the shellcheck finding SC2035 in
  `release.yaml`'s *Stage release artifacts* step
  (`sha256sum * > SHA256SUMS`; `sha256sum -- *` keeps the names).
  Recommendation: add it.
- **One nfpm version** (raised 2026-10-26, the H2 final review M-8):
  `e2e/deb/Dockerfile` packages with nfpm 2.40.0, the release with 2.43.0
  (`release.yaml`'s `NFPM_VERSION`), while `ci.yaml` calls `deb-test` the
  check that the release artifact works. Recommendation: move e2e/deb to
  2.43.0.
- **e2e/deb's fixed image name** (raised 2026-10-26, the H2 final review's
  parked item 4): `aculan-deb-test:<ver>`, so two runs on one host retag
  each other's image. Recommendation: an `E2E_DEB_NAME`, validated as
  `E2E_FLAT_NAME` is; CI is unaffected (one VM per job).
- **The `timeout -s INT` wrapper's two gaps** (raised 2026-10-26, the H2
  final wave report): `docker run` relayed the test's last output in 6 of
  7 local trials, and a command stuck for good never reaches the test's
  INT trap. Recommendation: `e2e/flat/run.sh` falls back to `docker logs`
  when a run is interrupted; an in-container watchdog only if a stuck
  command is ever seen.
- **Simulations near their budget** (raised 2026-10-25, the H1 final
  review's parked item 17): each simulation must finish in under 4.5 s
  under `-race` (ruling PE-8). `TestServeHTTP_MintedSuitesAtScale`
  measured 3.92–4.07 s on a quiet machine. Recommendation: coarsen it,
  every assertion kept, as phase H1 did the 500-suite simulation, if it
  crosses 4.5 s on a quiet machine or makes CI flaky.

## Decided

None yet.
````

- [ ] **Step 3: Spec revision 13**

Run (from the repository root):

```bash
python3 - <<'EOF'
#!/usr/bin/env python3
"""H3 Task 7's spec edits (revision 13), run from the repository root; each
old block must occur exactly once."""
import pathlib
import sys

root = pathlib.Path(".")
SPEC = "docs/superpowers/specs/2026-09-27-flat-repositories-design.md"
EDITS = [
    ("""- **Date:** 2026-09-27 (revision 12, 2026-10-24: phase H1, the sweep — """,
     """- **Date:** 2026-09-27 (revision 13, 2026-10-26: phase H3, documentation and release — §7 records phase H2's end-to-end suite as built: apt never fetches Jitsi's binary `Release.gpg` (apt 2.2–3.2 refuse a binary detached signature, so the fixture serves an `InRelease` beside it and the cache's side is pinned in Go; H2 deviation 1); the 403 unsigned variant is not adopted within a run (its confirmation takes an hour), and the run checks it is still unadopted after a refusal written 305 s or more after its first observation (H2 deviation 2, as built); "within one cooldown" means carried by the client's own check, before the scheduler could have checked the suite (H2 design note 5); the unsigned first adoption lands at its record's first retry at least `max(cooldown, 5 min)` after the first observation, 5 to 15 minutes in practice (H2 design note 7, ruling R-H2-6); the 45 s commit runs every update under the full error grep, stricter than the scoped grep first planned (H2's third deviation); the error grep also counts signature errors and `E:`/`Error:` lines that name the signed host; and §7 gains the manual live-upstream smoke (`e2e/live`), run green during phase H3's planning against pkgs.k8s.io, NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com; C10's flat consequences are documented in `docs/configuration.md`; C11's metric list is not exhaustive (the 1.1.0 release notes list all 21 new metrics, computed from the code); `flip_held_ms` can exceed 20000 by the hold timer's lateness (ruling R-H3-2); §8 adds the 1.1.0 release notes and the maintainer's open decisions; §9's H row names H3's work; revision 12, 2026-10-24: phase H1, the sweep — """),
    ("""`dists/` repo. Signed repos live on one host name, unsigned and Release-less""",
     """`dists/` repo. Jitsi's binary `Release.gpg` is never fetched: apt 2.2–3.2
refuse a binary detached signature, so the fixture serves an `InRelease`
beside it, and the cache's side (the sniff, a detached adoption) is pinned
in Go. The 403 unsigned variant needs an hour between its observations,
past any run: the run checks it is still unadopted after a refusal written
305 s or more after its first observation, where a 5-minute gap would have
confirmed it. Signed repos live on one host name, unsigned and Release-less"""),
    ("""failed|timed out|Hash Sum mismatch|Mirror sync in progress` (exit status is
useless); zero `unvouched_deb_refused` on the signed host; a v2 publish of a
signed or anchorless repo reaches clients within one cooldown; an older-Date
publish is ignored; state survives a restart. A commit-delay hook of 45 s
asserts no client timeouts, with its own scoped grep (the phase-B-expiry
straddle is a documented residual). The unsigned first-adoption scenario
(5-minute confirmation) runs on the oldest and newest images only.
`go test -race ./...`, `golangci-lint run ./...` and `gofmt` are clean at
every phase.
""",
     """failed|timed out|Hash Sum mismatch|Mirror sync in progress` (exit status is
useless), and, on lines naming the signed host, for signature errors and any
`E:`/`Error:` line; zero `unvouched_deb_refused` on the signed host; a v2
publish of a signed or anchorless repo reaches clients within one cooldown,
carried by the client's own check before the scheduler could have checked
the suite; an older-Date publish is ignored; state survives a restart. A
commit-delay hook of 45 s asserts no client timeouts under the full grep:
its client holds the old lists and gets 304s, and its by-hash requests are
served from the displaced snapshot, so the phase-B-expiry straddle, a
documented residual, does not reach it. The unsigned first-adoption
scenario runs on the oldest and newest images only: it adopts at its
record's first retry at least 5 minutes after first contact, 5 to 15
minutes in practice (C5's two observations at least `max(cooldown, 5 min)`
apart, on C7's retry schedule). `go test -race ./...`,
`golangci-lint run ./...` and `gofmt` are clean at every phase.

**Live-upstream smoke** (`e2e/live/`, `make e2e-live`; manual, before a
release, never in CI): a release-built daemon, with adoption and strict mode
on, and apt 2.6 clients against pkgs.k8s.io (two minors, both source forms),
NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and
download.sublimetext.com — every suite adopts in its upstream's form, every
`apt-get update` is clean under the e2e grep, one package per repository
installs vouched, and strict mode refuses only the controls' undeclared
`.deb`s.
"""),
    ("""- `docs/log-fields.md`, `docs/admin-ui-spec.md`,
  `packaging/config/config.toml.default`.
""",
     """- `docs/log-fields.md`, `docs/admin-ui-spec.md`,
  `packaging/config/config.toml.default`.
- `docs/release-notes/1.1.0.md`: the 1.1.0 release notes, for the GitHub
  release; their inventories of new events and metrics are computed from
  the code at 1.0.2 and at the release.
- `docs/open-decisions.md`: the decisions that outlive the work, each the
  maintainer's, with its evidence and a recommendation.
"""),
    ("""H2: `e2e/flat/`; H3: documentation (README, release notes), config file, final full-diff review. |""",
     """H2: `e2e/flat/`; H3: documentation (README, the strict-mode, unsigned and captive-portal docs; the config file was complete, `TestDefaultConfigFileDocumentsEveryKey`), the 1.1.0 release notes, the live-upstream smoke (`e2e/live`), the H1 test carries, `docs/open-decisions.md`, final full-diff review. |"""),
    ("""a manual live-upstream smoke (plan overview, "PR #8 parity") before the release that ships G; docs complete. |""",
     """a manual live-upstream smoke (plan overview, "PR #8 parity"; `e2e/live`, green in phase H3's planning on 2026-10-26) before the release that ships G; docs complete. |"""),
    ("""`acu_suites_reaped_total`, labelled by its three reasons. They are""",
     """`acu_suites_reaped_total`, labelled by its three reasons. The list
above is not exhaustive: the flip barrier's six counters (C8),
`acu_serve_byhash_displaced_total{host}`, `acu_clock_steps_detected_total`
and `acu_legacy_snapshots_quarantined_total` are new too; the 1.1.0 release
notes list all 21, computed from the code. They are"""),
]

p = root / SPEC
s = p.read_text()
for old, new in EDITS:
    n = s.count(old)
    if n != 1:
        sys.exit(f"spec: old block occurs {n} times:\n{old[:120]}")
    s = s.replace(old, new)
p.write_text(s)
print(f"spec: {len(EDITS)} edits applied")
EOF
```

Expected: `spec: 7 edits applied`.

- [ ] **Step 4: The overview**

Run (from the repository root):

```bash
python3 - <<'EOF'
#!/usr/bin/env python3
"""H3 Task 7's overview edits: every carry-forward marked *H3's* is marked
done with its task, the H3 row names its plan, the spec revision is 13,
the cross-phase interfaces gain H3's line, and the decisions block points
at docs/open-decisions.md. Run from the repository root; each old text must
occur exactly once."""
import pathlib
import sys

root = pathlib.Path(".")
OVERVIEW = "docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md"
H3 = "*H3's.*"
DONE = [
    ("the `adopt_unsigned` threat notes, beside the flat forms H documents. ", "*Done in H3 (Task 5).*"),
    ("(*Suite reaping* under `[gc]`, *Forgetting a suite* under `[admin]`). ", "*Done in H3 (Task 5).*"),
    ("an existing cache* and *Suite reaping*, says so; the Task 11 review). ", "*Done in H3 (Task 6; the README too, Task 5).*"),
    ("*Flat repositories*) — beside H's other README items. ", "*Done in H3 (Task 5).*"),
    ("proxy send `Host: 127.0.0.1` (the maintainer's decision, 2026-10-24). ", "*Done in H3 (Task 6; the README's example, Task 5).*"),
    ("`https` and for a `host` holding `://` (it answered 409). ", "*Done in H3 (Task 6; the README's example, Task 5).*"),
    ("no error: `suite_seed_rejected`, and the file is still cached. ", "*Done in H3 (Task 6).*"),
    ("a panic in GC no longer stops the daemon, at startup or in a tick. ", "*Done in H3 (Task 6).*"),
    ("msg=\"freshness_check\"` or `| logfmt | msg=\"freshness_check\"`. ", "*Done in H3 (Task 6).*"),
    ("new events and metrics since 1.0.x (phases A–H1), not only H1's. ",
     "*Done in H3 (Task 6: computed from the code at 1.0.2 and HEAD — 76 events, a test-binary one, 21 metrics; C11's list missed nine metrics).*"),
    ("and capture or silence the default logger in the metrics tests. ", "*Done in H3 (Task 1: one `TestMain` per package).*"),
    ("fixes all 17 callers. Failure path only; it never masks a pass. ",
     "*Done in H3 (Task 2: 12 of the 18 waits needed it; the other six drain in their cleanup).*"),
    ("as the signal; the release notes may warn operators who page on WARN. ",
     "*Done in H3 (Task 6 warns; the level change is in `docs/open-decisions.md`, Task 7).*"),
    ("coarsening candidate if it crosses it, as C-38 was (parked item 17). ",
     "*H3 measured it (Task 9) and listed it in `docs/open-decisions.md` (Task 7).*"),
    ("zero `unvouched_deb_refused`. Record the results in the release notes. ",
     "*Done in H3 (Task 3, `e2e/live`; the results in the release notes, Task 6).*"),
    ("queued; strict mode now refuses undeclared `.deb`s on flat-only hosts. ", "*Done in H3 (Task 4).*"),
]
OTHER = [
    ("""| H3. Docs and release | to write | §8 (README), release notes, the strict-mode docs (`docs/configuration.md`), the live-upstream smoke, final full-diff review; inputs: the *For H3* sections of the H1 and H2 plans, and the carry-forwards below marked *H3's* | H1, H2 | to write after H2 |""",
     """| H3. Docs and release | `…-H3-release.md` | §8 (README), release notes, the strict-mode docs (`docs/configuration.md`), the live-upstream smoke, final full-diff review; inputs: the *For H3* sections of the H1 and H2 plans, and the carry-forwards below marked *H3's* | H1, H2 | planned (Task 9 stamps it done) |"""),
    ("""**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12).""",
     """**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 13)."""),
    ("""- H: PR #8's end-to-end table in `e2e/flat`""",
     """- H3: `e2e/live/` (`make e2e-live`, `E2E_LIVE_NAME`; it sources `e2e/flat/lib.sh`), `docs/release-notes/1.1.0.md`, `docs/open-decisions.md`; the test helpers `waitReadyOrStop` and `readyOrStop` (`cmd/apt-cacher-ultra/daemon_ready_test.go`) and the `TestMain`s of `internal/fetch` and `internal/metrics`.
- H: PR #8's end-to-end table in `e2e/flat`"""),
    ("""**Decisions (spec-level; one awaits the maintainer's confirmation: GD-5):**
""",
     """**Decisions (spec-level; one awaits the maintainer's confirmation: GD-5):**

The decisions that outlive the branch — R-H5's confirmation, the cache writer's fail-fast on a panic, the Go toolchain and the post-1.1.0 carries of H2's For H3 list — are in `docs/open-decisions.md` (phase H3, Task 7), the maintainer's list.
"""),
]

p = root / OVERVIEW
s = p.read_text()
for prefix, mark in DONE:
    old = prefix + H3
    n = s.count(old)
    if n != 1:
        sys.exit(f"overview: {old!r} occurs {n} times")
    s = s.replace(old, prefix + mark)
for old, new in OTHER:
    n = s.count(old)
    if n != 1:
        sys.exit(f"overview: {old[:80]!r} occurs {n} times")
    s = s.replace(old, new)
if H3 in s:
    sys.exit("overview: an *H3's* carry is left")
p.write_text(s)
print(f"overview: {len(DONE)} carries marked done, {len(OTHER)} other edits")
EOF
```

Expected: `overview: 16 carries marked done, 4 other edits`. The script stops, changing nothing, when an old text does not occur exactly once or a carry is left marked *H3's*.

- [ ] **Step 5: Check the three files**

Run:

```bash
grep -c "\*H3's\.\*" docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
grep -c 'Done in H3 (Task' docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
grep -c 'revision 13, 2026-10-26' docs/superpowers/specs/2026-09-27-flat-repositories-design.md
grep -c '^\*\*Live-upstream smoke\*\*' docs/superpowers/specs/2026-09-27-flat-repositories-design.md
grep -c '^- \*\*' docs/open-decisions.md
T=$(mktemp -d)
cat > "$T/check_links.py" <<'EOF'
#!/usr/bin/env python3
"""Check the repository links of Markdown files (phase H3): each link to a
file of this repository — relative, or https://github.com/linsomniac/
apt-cacher-ultra/blob/main/<path> — names an existing file, and each
#anchor names a heading of that file (GitHub's slug: lower case, every
character but letters, digits, spaces, hyphens and underscores dropped,
spaces made hyphens). Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = "https://github.com/linsomniac/apt-cacher-ultra/blob/main/"


def slugs(path: pathlib.Path) -> set:
    out = set()
    fence = False
    for line in path.read_text().splitlines():
        if line.startswith("```"):
            fence = not fence
        m = re.match(r"#+ (.*)", line)
        if m and not fence:
            out.add(re.sub(r"[^a-z0-9 _-]", "", m.group(1).lower()).replace(" ", "-"))
    return out


bad = 0
for name in sys.argv[1:]:
    src = pathlib.Path(name)
    for target in sorted(set(re.findall(r"\]\(([^)\s]+)\)", src.read_text()))):
        if target.startswith(BLOB):
            rel = target[len(BLOB):]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = f.is_file() and (not anchor or anchor in slugs(f))
        bad += not ok
        print(f"{'ok  ' if ok else 'BAD '} {name}: {target}")
sys.exit(1 if bad else 0)
EOF
python3 "$T/check_links.py" docs/open-decisions.md | grep '^BAD' || echo "no broken link"
```

Expected: `0`, `15`, `1`, `1`, `11` (the *Open* entries), `no broken link`. (Of the 16 carries 15 read *Done in H3 (Task …)*; the 16th, the simulation near its budget, reads *H3 measured it (Task 9) …*.)

- [ ] **Step 6: Commit**

```bash
git add docs/open-decisions.md docs/superpowers/specs/2026-09-27-flat-repositories-design.md \
    docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
git commit -F - <<'EOF'
Spec revision 13; the maintainer's open decisions; the overview

Spec revision 13 records phase H2's end-to-end suite as built (apt never
fetches Jitsi's binary Release.gpg; the 403 unsigned variant is checked
unadopted, never vacuously; "within one cooldown" is the client's own
check; the unsigned confirmation lands at a retry, 5 to 15 minutes in;
the 45 s commit runs under the full grep), adds the live-upstream smoke
to section 7, says C11's metric list is not exhaustive, and names the
release notes and the open decisions in section 8.

docs/open-decisions.md is the maintainer's list of what outlives the
branch: the steps before the RC that need a push or a tag, the Go
toolchain, R-H5 and the writer's fail-fast to confirm, and the
post-1.1.0 carries, each with its evidence and a recommendation.

The overview marks every carry-forward that was H3's done, with its task.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 8: (Optional, open decision D2) The Go 1.27 toolchain

**Run this task only if the maintainer approves it** (H2's For H3 item 11; design note 5). Nothing else depends on it: skipped, every other task stands, and the decision stays in `docs/open-decisions.md`. It builds with Go 1.27 everywhere a build names a Go version — CI's `test` and `lint` jobs, the release workflow and the four e2e Dockerfiles (`e2e/flat`, `e2e/deb`, `e2e/live` from Task 3, and `e2e/Dockerfile.cache`, which the brief did not name) — and keeps `go.mod`'s `go 1.25.0`: the language version and the GODEBUG defaults stay those the code was tested with, while the toolchain, and so the standard library a release ships, gets Go 1.27's fixes. It also moves CI's golangci-lint pin from v2.12.2 to v2.14.0: planning found that v2.12.2, run with Go 1.27, fails every package's type check (`could not load export data: internal error in importing "internal/goarch" (cannot decode "internal/goarch", export data version 5 is greater than maximum supported version 4)`), while v2.14.0 reports `0 issues.` (Go 1.26 would keep v2.12.2: it reports `0 issues.` with go1.26.10.)

Why 1.27 rather than 1.26: Go supports a release until two newer ones exist, so 1.26 loses fixes when 1.28 ships (about February 2027) and 1.27 when 1.29 does (about August 2027); 1.1.x maintenance releases built with 1.27 stay supported six months longer, for one more line of CI change. The maintainer's local toolchain is go1.26.8, which ran every H1–H3 gate; with this task CI tests on 1.27 and local runs on 1.26 until he moves.

**Files:**
- Modify: `.github/workflows/ci.yaml` (two `go-version`, the golangci-lint `version` and its comment), `.github/workflows/release.yaml` (`go-version`), `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile`, `e2e/live/Dockerfile`, `e2e/Dockerfile.cache` (the `FROM golang:…` line and a comment), `e2e/flat/README.md` (its list of Docker Hub inputs)

**Interfaces:**
- Consumes: `e2e/live/Dockerfile` (Task 3).
- Produces: nothing other tasks use. Task 9's gate builds every e2e image with whatever toolchain this task leaves.

- [ ] **Step 1: Pre-flight: the toolchain's facts today**

Run:

```bash
go version
curl -fsS --max-time 30 'https://go.dev/dl/?mode=json' | python3 -c 'import json, sys; print(" ".join(r["version"] for r in json.load(sys.stdin)))'
docker manifest inspect golang:1.27-bookworm > /dev/null && echo "golang:1.27-bookworm exists"
docker manifest inspect golang:1.27-alpine > /dev/null && echo "golang:1.27-alpine exists"
grep -rn "go-version: '1.25'\|FROM golang:1.25\|golang:1.25-\|version: v2.12.2" .github e2e --include='*.yaml' --include='Dockerfile*' --include='*.md'
```

Expected (planning run, 2026-10-26): `go version go1.26.8 linux/amd64`; `go1.27.3 go1.26.10`; both images exist; and the ten lines this task changes (in any order):

```
.github/workflows/ci.yaml:19:          go-version: '1.25'
.github/workflows/ci.yaml:59:          go-version: '1.25'
.github/workflows/ci.yaml:66:          version: v2.12.2
.github/workflows/release.yaml:41:          go-version: '1.25'
e2e/flat/Dockerfile:21:FROM golang:1.25-bookworm AS build
e2e/flat/README.md:39:- Docker Hub (`golang:1.25-bookworm`, `debian:bookworm-slim`, the client
e2e/deb/Dockerfile:39:FROM golang:1.25-bookworm AS build
e2e/live/Dockerfile:19:FROM golang:1.25-bookworm AS build
e2e/Dockerfile.cache:2:# binary from the current source tree in a golang:1.25-alpine stage and
e2e/Dockerfile.cache:7:FROM golang:1.25-alpine AS build
```

A newer 1.27 patch release is fine (the tags float); a 1.28 on go.dev's list means the decision is due again with 1.28 in mind — stop and ask.

- [ ] **Step 2: Apply the edits**

Run (from the repository root):

```bash
python3 - <<'EOF'
#!/usr/bin/env python3
"""H3 Task 8 (optional, open decision D2): build with Go 1.27 everywhere,
go.mod's `go 1.25.0` unchanged. Run from the repository root; each old
text must occur exactly as often as stated."""
import pathlib
import sys

EDITS = [
    (".github/workflows/ci.yaml", "          go-version: '1.25'\n", "          go-version: '1.27'\n", 2),
    (".github/workflows/release.yaml", "          go-version: '1.25'\n", "          go-version: '1.27'\n", 1),
    ("e2e/flat/Dockerfile", "FROM golang:1.25-bookworm AS build\n", "FROM golang:1.27-bookworm AS build\n", 1),
    ("e2e/deb/Dockerfile", "FROM golang:1.25-bookworm AS build\n", "FROM golang:1.27-bookworm AS build\n", 1),
    ("e2e/live/Dockerfile", "FROM golang:1.25-bookworm AS build\n", "FROM golang:1.27-bookworm AS build\n", 1),
    ("e2e/Dockerfile.cache", "FROM golang:1.25-alpine AS build\n", "FROM golang:1.27-alpine AS build\n", 1),
    ("e2e/Dockerfile.cache", "golang:1.25-alpine stage", "golang:1.27-alpine stage", 1),
    ("e2e/flat/README.md", "(`golang:1.25-bookworm`,", "(`golang:1.27-bookworm`,", 1),
    (".github/workflows/ci.yaml",
     """          # `latest` drift behind your back. v2.x is required for Go 1.25
          # targets (v1.x was built with go1.23/1.24).
          version: v2.12.2
""",
     """          # `latest` drift behind your back. v2.x is required for Go 1.25
          # targets (v1.x was built with go1.23/1.24), and v2.14 or later
          # for the Go 1.27 toolchain: v2.12.2 cannot read Go 1.27's export
          # data ("export data version 5 is greater than maximum supported
          # version 4").
          version: v2.14.0
""", 1),
]
for rel, old, new, want in EDITS:
    p = pathlib.Path(rel)
    s = p.read_text()
    n = s.count(old)
    if n != want:
        sys.exit(f"{rel}: {old.strip()!r} occurs {n} times, want {want}; nothing written for it")
    p.write_text(s.replace(old, new))
    print(f"{rel}: {n}")
EOF
```

Expected: one line per edit — `.github/workflows/ci.yaml: 2`, `.github/workflows/release.yaml: 1`, `e2e/flat/Dockerfile: 1`, `e2e/deb/Dockerfile: 1`, `e2e/live/Dockerfile: 1`, `e2e/Dockerfile.cache: 1` (twice), `e2e/flat/README.md: 1`, `.github/workflows/ci.yaml: 1`. Then `grep -rn "go-version: '1.25'\|FROM golang:1.25\|golang:1.25-\|version: v2.12.2" .github e2e --include='*.yaml' --include='Dockerfile*' --include='*.md'` prints nothing, and `head -3 go.mod` still shows `go 1.25.0`.

- [ ] **Step 3: The module under Go 1.27**

Run (the module mounted into the Go 1.27 image; the test cache stays in the container):

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.27-bookworm bash -c 'go version && go vet ./... && CGO_ENABLED=0 go build -trimpath -o /tmp/acu ./cmd/apt-cacher-ultra && go test -race -timeout 10m ./... && go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra/'
```

Expected: `go version go1.27.3 linux/amd64` (or a later 1.27), then every package `ok` (21 packages; planning: 315 s for the race run) and the tagged package `ok`, exit status 0.

- [ ] **Step 4: CI's linter under Go 1.27**

Run:

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.27-bookworm bash -c 'go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 > /tmp/install.log 2>&1 || { tail -5 /tmp/install.log; exit 1; }; "$(go env GOPATH)/bin/golangci-lint" run ./...'
```

Expected: `0 issues.` (planning: golangci-lint 2.14.0 built with go1.27.3). The same command with `@v2.12.2` is the red: five `typecheck` issues, `export data version 5 is greater than maximum supported version 4`.

- [ ] **Step 5: The workflows and the images**

Run:

```bash
docker run --rm --network none -v "$(pwd)":/repo -w /repo rhysd/actionlint:1.7.12 -no-color -oneline .github/workflows/ci.yaml .github/workflows/release.yaml .github/workflows/pages.yaml
for f in e2e/flat/Dockerfile e2e/deb/Dockerfile e2e/live/Dockerfile e2e/Dockerfile.cache; do docker build -q --target build -f "$f" . > /dev/null && echo "build stage OK: $f"; done
```

Expected: actionlint prints only the finding that predates this branch's H3 work (`.github/workflows/release.yaml:126:9: shellcheck reported issue in this script: SC2035:info:21:23: …`, in `docs/open-decisions.md`) and exits 1 for it; and four `build stage OK` lines (planning: 68, 89, 32 and 77 s cold). The e2e suites themselves run on the new images in Task 9's gate.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/ci.yaml .github/workflows/release.yaml e2e/flat/Dockerfile e2e/deb/Dockerfile \
    e2e/live/Dockerfile e2e/Dockerfile.cache e2e/flat/README.md
git commit -F - <<'EOF'
Build with Go 1.27

Go 1.25 gets no security fix since Go 1.27 shipped (Go supports a
release until two newer ones exist), so a 1.1.0 built with it would ship
without them. CI, the release workflow and the four e2e Dockerfiles
build with Go 1.27; go.mod keeps go 1.25.0, so the language version and
the GODEBUG defaults are the ones the code was tested with. CI's
golangci-lint moves from v2.12.2, which cannot read Go 1.27's export
data, to v2.14.0. Under go1.27.3: vet, the static build, the race suite
(21 packages) and the e2ehooks tests pass, golangci-lint v2.14.0 reports
0 issues, and every e2e image's build stage builds. The maintainer's
decision D2 of the H3 plan.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

Then in `docs/open-decisions.md`, move the *The Go toolchain* entry to *Decided* with the date and "approved; built with Go 1.27 (commit …)", and commit that file alone (`Open decisions: the Go toolchain is decided`).

---

### Task 9: The release-candidate dry run, the full gate and the final multi-lens review

The last task, run by the executing controller: a local dry run of the 1.1.0-rc1 build (ruling R-H3-7: the tag is the maintainer's), the full gate on the final tree, and the final review of the whole branch, `main..HEAD`, through five lenses on the most capable model available (Fable, at maximum effort, as the branch's earlier max-effort reviews), its findings fixed test-first or recorded, and the gate run again.

**Files:**
- None for Steps 1–5. The review's fixes are commits of their own (Step 7), each with the test that fails without it. `docs/open-decisions.md` takes what the review defers (Step 9); the overview's H3 row is stamped done (Step 9).

**Interfaces:**
- Consumes: every earlier task; `docs/release-notes/1.1.0.md`'s two checkers (Task 6 Step 4, with `/tmp/h3-notes` from its Step 1 — rebuild it with Task 6 Step 1 if it is gone); `check_links.py` (Task 5 Step 8).
- Produces: the review reports (outside the tree, in `/tmp/h3-review/`), the fix commits, the stamped overview, the final report.

- [ ] **Step 1: The release-candidate dry run (local; no tag)**

Run (from the repository root):

```bash
grep -rnE '"1\.[0-9]+\.[0-9]+(-rc[0-9]+)?"' --include='*.go' cmd internal | grep -v _test.go || echo "no version string in the source"
make build VERSION=1.1.0-rc1 > /dev/null && ./build/apt-cacher-ultra -version
make deb VERSION=1.1.0-rc1 DEB_VERSION=1.1.0~rc1 2>&1 | tail -1
if grep -q ACU_E2E_ build/apt-cacher-ultra; then echo "SEAM IN THE BINARY"; else echo "no e2e seam in the binary"; fi
go version -m build/apt-cacher-ultra | grep -c e2ehooks
bash e2e/apt-repo/test.sh 2>&1 | grep -E '^(PASS|FAIL|SKIP)'
```

Expected (planning run, with `GOFLAGS=-buildvcs=false` outside a git checkout):

```
no version string in the source
1.1.0-rc1
created package: build/apt-cacher-ultra_1.1.0~rc1_amd64.deb
no e2e seam in the binary
0
SKIP test-build-repo: dpkg-deb missing
SKIP signed: dpkg-deb missing
PASS test-select-tags
```

`make build` and the release workflow take the version from the tag (`git describe --tags`, `-X main.Version`), `release.yaml` turns `1.1.0-rc1` into the Debian version `1.1.0~rc1` and marks the release a prerelease, and the apt-repository publisher skips prereleases (`test-select-tags` drops its fixture's `0.10.4-rc2`): an RC reaches the GitHub release page, never `linsomniac.github.io`'s apt repository. The two SKIPs need `dpkg-deb` (on NixOS, `nix-shell -p dpkg`); CI runs them. Remove `build/` afterwards (`make clean`); nothing else changed (`git status --short` is empty).

- [ ] **Step 2: The gate, part 1: the module**

On a quiet machine (no other build or test running), run (from the repository root):

```bash
go test -race -count=1 ./...                                   # 1
golangci-lint run ./...                                        # 2
gofmt -l .                                                     # 3
go test -count=1 -v -run 'TestEveryEventIsDocumented|TestSweptEventNames|TestLogMessagesAreSnakeCase|TestLogMethodsAreOnlyCalledDirectly' ./internal/observability/   # 4
go test -race -p 1 -count=3 -v -run '^(TestSim[A-Za-z0-9_]*|TestDoD_[A-Za-z0-9_]*|TestServeHTTP_Minted[A-Za-z0-9_]*AtScale|TestLedger_[A-Za-z0-9_]*|TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds)$' ./internal/flipgate/ ./internal/freshness/ ./internal/handler/ ./internal/simtest/ > /tmp/h3-sims.txt 2>&1; grep -E '^--- ' /tmp/h3-sims.txt | sort -t'(' -k2 -rn | head -12; nproc   # 5
go test -race -count=20 -run 'TestPhase2Chaos_AdoptionFlipUnderConcurrency$|TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency$' ./internal/handler/   # 6
shellcheck -x e2e/flat/*.sh e2e/live/*.sh                      # 8
docker run --rm --network none -v "$(pwd)":/repo -w /repo rhysd/actionlint:1.7.12 -no-color -oneline .github/workflows/ci.yaml .github/workflows/release.yaml .github/workflows/pages.yaml   # 9
```

Expected:

1. every package `ok`, no `FAIL` (21 packages);
2. `0 issues.`;
3. exactly the five drift files of the Global Constraints;
4. four `--- PASS` lines;
5. every line `PASS` and under 4.5 s (ruling PE-8);
6. `ok` (the two chaos tests, 20 runs each);
7. the migrations check below: `migrations: 6 released, 7 now; changed: none`;
8. no output;
9. one line, the known `.github/workflows/release.yaml:126:9: shellcheck reported issue in this script: SC2035:info:21:23: …` (in `docs/open-decisions.md`), and exit status 1 for it alone.

Item 7, the migrations check (no released migration may change: the overview's first Global Constraint; it compares each entry of `migrations` at 1.0.2 with HEAD's, comments aside):

```bash
D=/tmp/h3-gate; rm -rf "$D"; mkdir -p "$D"
git show 1.0.2:internal/cache/schema.go > "$D/schema-1.0.2.go"
cat > "$D/migrations_unchanged.py" <<'EOF'
#!/usr/bin/env python3
"""Check that no released migration changed (phase H3's gate): the SQL of
each entry of `migrations` in OLD's internal/cache/schema.go equals the
same entry in NEW's, and NEW only appends. Comments are not compared.
Usage: migrations_unchanged.py OLD-SCHEMA.go NEW-SCHEMA.go"""
import re
import sys


def entries(path: str) -> list:
    src = open(path, encoding="utf-8").read()
    body = re.search(r"^var migrations = \[\]string\{\n(.*?)^\}\n", src, re.M | re.S).group(1)
    return re.findall(r"`([^`]*)`", body)


old, new = entries(sys.argv[1]), entries(sys.argv[2])
changed = [i for i in range(len(old)) if i >= len(new) or old[i] != new[i]]
print(f"migrations: {len(old)} released, {len(new)} now; changed: {changed or 'none'}")
sys.exit(1 if changed or len(new) < len(old) else 0)
EOF
python3 "$D/migrations_unchanged.py" "$D/schema-1.0.2.go" internal/cache/schema.go
```

Planning results (2026-10-26, the scratch tree with Tasks 1–7 applied): items 2, 3, 4, 7 and 8 as expected; item 5's slowest on a machine with a background load average near 2 (12 cores): `TestSimRetry_LongRunDateRulesAndClockSteps` 4.28–5.27 s, `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` 4.04–4.32 s, `TestSimScheduler_StallingHostWithAdoptionContention` 4.14–4.23 s, `TestSimCapacity_UpgradeBurstSameHost` 4.12 s, `TestServeHTTP_MintedSuitesAtScale` 3.97–4.02 s, `TestDoD_NinetyDaysOfReapingWithRestarts` 3.84–4.05 s (H1's quiet-machine replay had them at 3.4–4.0 s). A line over 4.5 s means a busy machine first: rerun that test alone with `-count=5` on a quiet machine; an over-budget time that repeats there fails the gate, and the fix is a coarsening that keeps every assertion (as H1's C-38 and R-H7), test-first. Record the three slowest and `nproc` in the final report, and update `docs/open-decisions.md`'s *Simulations near their budget* entry with them. Item 1: 21 packages `ok` in 307 s (load average 2.6); item 6: `ok` in 152 s; item 9 as expected (on HEAD and with Task 8's edits alike).

- [ ] **Step 3: The gate, part 2: the documents**

Run Task 6 Step 4's two checkers again on the final tree (`check_notes.py` with `/tmp/h3-notes`, rebuilt by Task 6 Step 1 if gone), then the link checker on every document this branch changed that has repository links:

```bash
python3 /tmp/h3-notes/check_links.py README.md docs/configuration.md docs/log-fields.md docs/release-notes/1.1.0.md docs/open-decisions.md e2e/flat/README.md e2e/live/README.md | grep '^BAD' || echo "no broken link"
```

Expected: the nine `ok` lines of Task 6 Step 4, and `no broken link`. (`docs/admin-ui-spec.md` is left out: its `[SPEC5.md](../SPEC5.md)` link was broken on 1.0.2 already — that file was never in the repository — and nothing in H3 touches it.)

- [ ] **Step 4: The gate, part 3: the end-to-end suites**

Run, one after another (they share the machine's Docker; about 50 minutes in all):

```bash
make e2e-flat 2>&1 | tee /tmp/h3-gate/e2e-flat.log | grep -E '^\[e2e-flat\] '
make deb-test 2>&1 | tee /tmp/h3-gate/deb-test.log | tail -3
make e2e-live 2>&1 | tee /tmp/h3-gate/e2e-live.log | grep -E '^\[e2e-live\] PASS|FAIL'
```

Expected: `[e2e-flat] OVERALL PASS (debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04)` with each image's build and test times; `deb-test`'s pass line for 24.04 and 26.04; `[e2e-live] PASS (…)`. Planning (the scratch tree with Tasks 1–7 applied): `[e2e-flat] OVERALL PASS (debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04)` — tests of 382, 139, 143, 138, 136 and 444 s, the first and last waiting for the unsigned confirmation, about 24 minutes in all with cached builds; `[deb-test] OVERALL PASS (24.04 26.04)`; and the live smoke's two runs (Task 3). A live smoke that fails for the vendor's reason is fixed in its values (`e2e/live/README.md`, *Maintenance*) and rerun; one that fails for the cache's is a finding (ruling R-H2-4), reported to the maintainer before anything else.

- [ ] **Step 5: The review's inputs**

The branch's diff is large — at e65f67d, `git diff --no-ext-diff --numstat main..HEAD` counts 320 files, +159,612/−3,090 lines, 87,657 of them the phase plans (design note 6) — so each lens reads the areas it owns, not the whole diff. Run (from the repository root):

```bash
R=/tmp/h3-review; rm -rf "$R"; mkdir -p "$R"
cat > "$R/inputs.sh" <<'EOF'
#!/bin/bash
# Phase H3, Task 9: the final review's input diffs (main..HEAD), one per
# reviewer, and their sizes. Run from the repository root; writes into $1.
set -u
R=$1
X=':(exclude)*_test.go'
d() {
    local out=$1
    shift
    git diff --no-ext-diff main..HEAD -- "$@" > "$R/$out"
    printf '%-30s %7d lines\n' "$out" "$(wc -l < "$R/$out")"
}
F=internal/freshness
CHECKS="freshness.go check.go scheduler.go current_form.go first_adoption.go anchorless.go probe.go admission.go
    inflight.go suite_lock.go clockstep.go retry.go metrics.go"
ADOPT="adoption.go reaper.go legacy_audit.go release_date.go unsigned.go adoption_failure.go reconcile.go
    member_verify.go forget.go packages_parse.go flip_barrier.go release_parse.go repair.go sources_parse.go
    hot_set.go hot_prefetch.go pdiff_parse.go"
checks=() adopt=()
for f in $CHECKS; do checks+=("$F/$f"); done
for f in $ADOPT; do adopt+=("$F/$f"); done
# Every production file of internal/freshness is in one of the two lists.
for p in "$F"/*.go; do
    case "$p" in *_test.go) continue ;; esac
    case " ${checks[*]} ${adopt[*]} " in *" $p "*) ;; *) echo "not in a list: $p" ;; esac
done
d 1a-freshness-checks.diff "${checks[@]}"
d 1b-freshness-adoption.diff "${adopt[@]}"
d 1c-cache-gc.diff internal/cache internal/gc "$X"
d 1d-handler-admin-rest.diff internal/handler internal/flipgate internal/admin internal/aptpath internal/wallclock \
    internal/simtest internal/observability internal/gpg internal/config internal/proxy internal/fetch \
    internal/metrics internal/integrity cmd "$X"
d 2-upgrade.diff internal/cache/schema.go internal/cache/legacy_audit.go "$F/legacy_audit.go" \
    internal/cache/suite_reap.go internal/cache/suite_evict.go internal/cache/flat_reap.go internal/cache/gc.go \
    "$F/reaper.go" "$F/forget.go" internal/config/config.go internal/gc cmd/apt-cacher-ultra/main.go "$X"
d 3-security.diff internal/admin internal/gpg internal/aptpath internal/fetch "$F/unsigned.go" \
    "$F/current_form.go" "$F/first_adoption.go" internal/cache/signed_seen.go internal/handler/handler.go \
    internal/handler/suite_rows.go cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go \
    .github/workflows "$X"
d 4-docs.diff README.md docs/configuration.md docs/log-fields.md docs/admin-ui-spec.md packaging \
    e2e/flat/README.md e2e/live/README.md docs/release-notes docs/open-decisions.md
d 5-tests-ci.diff internal/simtest internal/observability e2e .github Makefile .dockerignore
d 5-tests-go.diff '*_test.go'
EOF
bash "$R/inputs.sh" "$R"
```

Expected (planning, at e65f67d; H3's own changes add about 1,500 lines, most of them to `4-docs.diff` and `5-tests-ci.diff`): `1a-freshness-checks.diff` 6,089 lines, `1b-freshness-adoption.diff` 5,536, `1c-cache-gc.diff` 5,995, `1d-handler-admin-rest.diff` 7,341, `2-upgrade.diff` 4,664, `3-security.diff` 6,424, `4-docs.diff` 2,171, `5-tests-ci.diff` 4,917, `5-tests-go.diff` 51,675, and no `not in a list` line (every production file of `internal/freshness` is in one of the two freshness inputs). Each reviewer also reads the files themselves, at HEAD, for context.

- [ ] **Step 6: Dispatch the eight reviewers**

One reviewer per row, all at once, each on the most capable model at maximum effort, each a fresh context with this brief: the common part, then its row.

*Common brief.* You review `main..HEAD` of `feature/flat-repos` in `/home/sean/projects/apt-cacher-ultra/.claude/worktrees/flat-repos` — the flat-repositories work (phases A–H3: suite identity for flat repositories, eager first adoption, unsigned and Release-less forms, retry discipline, the flip barrier, suite reaping, the H1 sweep, e2e/flat, e2e/live, the docs) — before a 1.1.0 release candidate. The maintainer's goal: "a reliable, robust, and low maintenance system". Read the spec (`docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, revision 13) and the overview (`docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`: Global Constraints, deviations, rulings by name); your input diffs are in `/tmp/h3-review/`. Report findings only: change no file in the worktree; to run or mutate code, work in a scratch copy (`git archive -o /tmp/<you>.tar HEAD`, then `tar -x` it into an empty directory). For each: severity (**Critical**: data loss, a trust bypass, wrong bytes served, a crash loop, an upgrade that breaks an install; **Important**: a real bug or a doc that would lead an operator to harm; **Minor**: robustness or clarity), `file:line`, the code or text quoted, why it is wrong, and how to show it — the failing test you would write, or a reproduction. Check each claim against the code before you report it; say whether it predates the branch (`git show 1.0.2:<path>`). Do not re-raise what a ruling or a declined deferred minor already settled (H1's disposition table) unless the code contradicts the ruling. Rank them; at most 15. `git diff` goes through difftastic here: pass `--no-ext-diff`.

| Reviewer | Lens | Inputs | Look for |
|---|---|---|---|
| R1 | Correctness and concurrency: checks and scheduling | `1a-freshness-checks.diff` | a `sync.Mutex` held across a wait (Global Constraints: block on channels and timers only); a goroutine without its panic recovery (`grep -n 'go func' internal/freshness/*.go`); context and shutdown ordering; the scheduler's queues, slots and backoff (spec C4, C7: the latency bound, healthy → never-checked → failing); wall-clock versus monotonic time (`wallclock` for stamps, `time.Since` for durations); unbounded memory or goroutines; an I/O fault taken as evidence |
| R2 | Correctness and concurrency: adoption and lifecycle | `1b-freshness-adoption.diff` | the adoption's steps and its commit (spec C5, C8); never-downgrade at both points; the flip barrier's bounds; the reaper and `/forget` (spec C9: compare-and-delete, budgets, clock steps); the legacy trust audit's resumability; parsing of upstream input (`Release`, `Packages`, `Sources`, dates) for panics and unbounded work |
| R3 | Correctness and concurrency: the cache and GC | `1c-cache-gc.diff` | SQL: atomic transactions, refcounts balanced, no SQLite clock function (`TestNoSQLiteClockFunctionsInProductionCode`), indexes the queries need; the single writer goroutine (no pool I/O or foreign code on it: ruling PD-5); GC's sub-jobs and their deadlines; suite-row creation and deletes (spec C3, C9) |
| R4 | Correctness and concurrency: the handler, the flip gate, admin and the rest | `1d-handler-admin-rest.diff` | the request path (spec C1–C3, C6, C8, C10): canonicalisation, the strict-mode decision, the flip gate's holds and waiters, the miss path's single operation; the admin endpoints; `cmd/apt-cacher-ultra/main.go`'s startup and shutdown ordering |
| R5 | Upgrade and data safety | `2-upgrade.diff`, `docs/configuration.md` *Upgrading an existing cache*, `docs/release-notes/1.1.0.md` *Before you upgrade* | the v6 → v7 migration on a large cache (time, locks, rollback; released migrations are never altered: Step 2 row 7); the trust audit's refusal-to-start conditions and what it deletes; what the reaper, GC and cap eviction delete, and when; config defaults and validation (a 1.0.x config must load); disk use after the upgrade; whether *Before you upgrade* is complete and right |
| R6 | Security | `3-security.diff`, the 2026-09-08 security review (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`, untracked, in the main checkout), the G-1 commits (`27f30b5..38c5448`) | for each finding of that review the branch touches: fixed, unchanged, or made worse; trust never inferred from "adopted" (Global Constraints); the inline verification (decodable clearsign only); signed-seen markers and never-downgrade; the unsigned threat model as documented; strict mode's fail-open and fail-closed choices; SSRF (allowlist, deny ranges, redirects); the admin guard (header, `Sec-Fetch-Site`, `Host`); the bounds on bodies, waiters, suites and memory; the `e2ehooks` seam never in a release binary; attacker-controlled values in log fields |
| R7 | Operator docs against behaviour | `4-docs.diff` and the code | every default, number, event, field, status code, metric and procedure in `README.md`, `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, `docs/release-notes/1.1.0.md`, `docs/open-decisions.md`, `e2e/flat/README.md` and `e2e/live/README.md` matches the code (grep it); nothing describes behaviour the branch removed (the H2 plan's body still describes a residual allowance, a "second retry" and a "vacuous 6 %" — no operator doc may: H2 parked item 8); an operator can follow each procedure as written |
| R8 | Tests and CI | `5-tests-ci.diff`; `5-tests-go.diff` searched, not read through (51,675 lines); Steps 2–4's outputs | a test that cannot fail (spot-check three high-value tests by mutating the code they pin); `time.Sleep` standing in for a condition; `t.Parallel()` with `wallclock.SetOffsetForTest`; `t.Skip` without a reason; timing bounds that a 4-vCPU runner breaks; the simulations' 4.5 s budget; CI's matrix, timeouts and guards (`ACU_E2E_`, the release workflow's test step); e2e/flat's and e2e/live's bounds, diagnosability and maintenance |

- [ ] **Step 7: Triage, and the fix wave**

The controller reads the eight reports and rules on each finding (a ruling per finding, recorded in the final report):

- **Critical and Important:** fixed now, each test-first — the test that fails without the fix (its failure line recorded), the fix, the test passing, a mutant of the fix that fails it — one commit per fix or per closely related group, its message naming the reviewer and the finding. A Critical finding is reported to the maintainer at once, before the fix wave goes on.
- **Minor:** fixed now when the fix is small, local and testable; otherwise recorded in `docs/open-decisions.md` (Step 9) with its evidence and a recommendation.
- **Declined:** only with a reason that cites the code, a ruling or the spec; the reviewer who raised it sees the reason in the re-review.

Then one re-review per lens that had a finding fixed, by a fresh reviewer of the same lens, on the fix commits' diff, with the findings and rulings: "approve" or new findings, which go through this step again.

- [ ] **Step 8: The gate again**

After the last fix: Step 2 in full, Step 3, and from Step 4 every suite whose inputs a fix touched (`make e2e-flat` after a change to the daemon, `e2e/flat/` or the e2e images; `make deb-test` after a change to packaging or `e2e/deb/`; `make e2e-live` after any daemon change). Expected: as in Steps 2–4.

- [ ] **Step 9: Record, stamp, commit**

Append to `docs/open-decisions.md`'s *Open* list each finding the review deferred (with its reviewer, evidence and recommendation) and update its *Simulations near their budget* entry with Step 2's numbers. In the overview, the H3 row's status `planned (Task 9 stamps it done)` becomes `done (commit <the last commit's short hash>)`, as every earlier phase's row reads. Commit:

```bash
git add docs/open-decisions.md docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
git commit -F - <<'EOF'
Phase H3: the final review's deferred items; H3 done in the overview

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

(Stamp the row with the hash of the commit before this one, the last that changed code or docs; this commit records it.)

- [ ] **Step 10: The report to the maintainer**

Report, briefly: the gate's results (Steps 2–4, with the three slowest simulations and `nproc`); the review's findings by severity and their rulings and commits; the live smoke's result on the final tree; and what is his — the open decisions D1–D6 with their recommendations, and `docs/open-decisions.md`'s *Before the 1.1.0 release candidate* list (the pushes and the tag only he makes: nothing goes to GitHub without him).

---

## Found while planning (for the controller)

- **No daemon issue.** Both live-smoke runs passed: all five repositories, eight suites, adopted in their upstreams' forms; every update clean; six packages installed vouched; strict mode on all five hosts (Task 3). `e2e/flat` passed on all six images and `e2e/deb` on both, with Tasks 1–7 applied (Task 9).
- **Carries that were incomplete or inexact:** the toolchain touches four e2e Dockerfiles, not two (`e2e/Dockerfile.cache`, and `e2e/live/Dockerfile` once Task 3 lands), and moving to Go 1.27 also needs CI's golangci-lint moved from v2.12.2 to v2.14.0 (design note 5, Task 8); spec C11's metric list misses nine of the 21 new metrics (design note 1; revision 13 says so); the overview's "17 callers" of `waitForDaemonReady` are 18 waits, 12 of which needed the drain (Task 2); the README's `/reconcile` example lacked its 400 and 413 answers (Task 5); H2's open question on the captive portal's `Release.gpg` is answered — an adopted unsigned snapshot answers it 404 — and now pinned (Task 4).
- **Outside H3's scope, recorded:** the 2026-09-08 security review's report the brief cites, `docs/security-review-findings.md`, is untracked: it exists only in the main checkout (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`), so the final review reads it there (Task 9, R6); its finding H-3 is the toolchain decision. `docs/admin-ui-spec.md` links `../SPEC5.md`, a file the repository never had (broken on 1.0.2 already; Task 9 Step 3 leaves it out). pkg.jenkins.io now signs with `jenkins.io-2026.key`; the smoke uses it.

## Self-review (done while writing)

- **Coverage of the brief and the carries.** Spec §8's README items: Task 5. The strict-mode, unsigned, captive-portal, binary-signature and `flip_held_ms` docs: Task 4. Spec revision 13 (H2 deviations 1 and 2 as built, design notes 5 and 7, the full-grep deviation, the live smoke): Task 7. The release notes with every *H3's* carry — the upgrade items, the six removed messages, the log-query note, the repeating WARNs, the computed inventory, the alert recipes, the captive portal, the harness, the smoke's results: Task 6. The H1 test carries: Tasks 1 and 2 (and the simulation near its budget: Task 9's measurement, `docs/open-decisions.md`). The live smoke: Task 3. The durable decisions: Task 7. The toolchain: Task 8 (optional). Version and packaging: ruling R-H3-7, Task 9 Step 1. The final review: Task 9. The overview's sixteen *H3's* carries are each marked done by Task 7's script, which stops if one is left.
- **Replayed, not transcribed.** Every file this plan creates whole is byte for byte the file the planning runs used (a script compared each embedded block with the scratch tree's file). The sixteen `old`/`new` pairs of Tasks 3–5, applied in order to HEAD's files, give exactly the tested files. Task 2's conversion script and its Steps 1 and 3 assembled, Task 6 Step 4's checkers, Task 7's two scripts and Task 8's script were each run from the plan's text on fresh copies of HEAD, with the outputs the steps state. The red steps and mutants of Tasks 1, 2 and 4, the live smoke's strict mutant and the checkers' mutants failed as stated.
- **Not replayed:** Task 9's review (Steps 5–10 are the controller's), and Task 8's full gate under Go 1.27 beyond the module, the linter and the images' build stages (the e2e suites run on the new images only in Task 9).
