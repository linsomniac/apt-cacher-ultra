# Flat repositories — Phase H3: docs, release notes, the live smoke and the final review — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the flat-repositories branch for a 1.1.0 release candidate: the operator documentation spec §8 asks for (README, `docs/configuration.md`, `docs/admin-ui-spec.md`, `docs/log-fields.md`), the 1.1.0 release notes with every *H3's* carry checked against the code, the manual live-upstream smoke (`e2e/live`, run during planning), the H1 test-output carries, spec revision 13, a home for the decisions that outlive the phase, the two changes the maintainer decided for 1.1.0 (the package upgrade restarts a running daemon; Go 1.27), and the final multi-lens review of the whole branch — in the maintainer's words, "a reliable, robust, and low maintenance system".

**Architecture:** No production Go change: Task 4 changes the package's maintainer scripts (shell) and Task 5 the build files (the Go toolchain). Two test-only tasks remove the test output the H1 reviews flagged and drain the cmd tests' daemons on every failing exit. `e2e/live/` is a new, manual, bounded smoke in e2e/flat's style: one container (a release-built daemon and real apt 2.6 clients) against five vendors' live flat repositories, sourcing e2e/flat's `lib.sh`. e2e/deb gains a phase that runs the maintainer scripts against a stub `systemctl`. The documentation tasks write their text out in full here, each claim with a pre-flight step that checks it against the code at HEAD. The release notes are a new in-tree file, `docs/release-notes/1.1.0.md`, which the maintainer pastes into the GitHub release (ruling PH3-1). The last task is the release-candidate dry run, the full gate and the final review.

**Tech Stack:** Go 1.25 (the language version; the builds use Go 1.27 after Task 5, the toolchain 1.26 locally), Bash 5 and shellcheck 0.11, POSIX `sh` for the maintainer scripts, dpkg with `deb-systemd-invoke` (Ubuntu 24.04 and 26.04 in e2e/deb), Docker with BuildKit, apt 2.6.1 (`debian:12`) for the live smoke, Markdown.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12; Task 9 records this plan's changes as revision 13) — §8 (Documentation) and §9's H row (the definition of done) are this plan's requirement; C10 (strict mode: the PR #8 parity consequences), C11 (the inventory of new events, fields and metrics), §7 (H2's deviations, recorded in revision 13) and §10 (follow-ups) are what it documents. Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` — the phase table's H3 row, every carry-forward marked *H3's*, "PR #8 parity" section H (the live smoke and the strict-mode docs), the decisions block. The *For H3* sections of `…-H1-sweep.md` (with the overview's three *(H1)* corrections to it) and `…-H2-e2e.md` (items 1–11; where that plan's body and its For H3 list or `e2e/flat/README.md` differ, the list and the README are right). Phase H is three plans run in order: H1 (the sweep), H2 (`e2e/flat`), **H3 (this plan)**. This revision applies the three plan reviews of 2026-10-26 and the rulings on them (`PH3-1` to `PH3-10`, below).

## Global Constraints

Everything in the overview's Global Constraints applies. In particular:

- **Nothing goes to GitHub.** No push, pull request, tag, release, issue or comment, and nothing on PR #8. The maintainer opens the pull request and tags 1.1.0-rc1 himself; Task 10 prepares and dry-runs it locally.
- **No production Go change**, no schema change, no config key. The Go changes of Tasks 1, 2 and 6 are `_test.go` files only. Never edit an entry of `migrations` in `internal/cache/schema.go`. The package's maintainer scripts change (Task 4, the maintainer's decision H-2), proven by e2e/deb.
- `go test -race ./...` and `golangci-lint run ./...` are clean, and `gofmt -l .` lists only the five drift files (`cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) at the end of every task that touches Go. Never run `gofmt -w` on those five.
- **Every doc claim is checked against the code at HEAD** by a step of its task: a grep with its expected output, or a named test. Text written from memory is wrong (the H2 lesson: seven review rounds moved the harness away from its plan). A claim whose check fails is corrected to what the code does, never the reverse, and the correction is reported.
- **Every change is proven:** a test (red, then green, with a mutant that fails it), a guard test, a grep check with its expected output, or a recorded run.
- Shell files are bash with `set -euo pipefail` and pass `shellcheck -x` with no output, but for the maintainer scripts, which stay POSIX `sh` with `set -e` (dpkg runs them with `/bin/sh`); no `grep -q` ends a pipe (e2e/flat's design note 18); every wait is a bounded poll (e2e/flat's ruling R-H2-5).
- `AIDEV-NOTE`s at every subtle point; existing `AIDEV-*` comments are kept and updated, never removed.
- Credit: Venkat (@venkatamutyala) stays credited in `Contributors.md` ("flat repository support is based on their PR #8"); the release notes credit them in the maintainer's words (Task 8; decision D5).
- `git diff` in this repository goes through difftastic: pass `--no-ext-diff` to every diff a step reads.
- Scratch files go under the session scratchpad, `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/` (`h3-notes`, `h3-gate`, `h3-review`, `h3-deb`), each named by one variable set at the top of the step that uses it (ruling PH3-3).
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Every command runs from the repository root unless it says otherwise. Expected outputs quote the planning runs (2026-10-26 for the first draft, 2026-10-27 for this revision; a 12-core machine, Go 1.26.8, Docker 29); a time is a range, never an assertion.
- Edits to existing files are exact `old`/`new` blocks; each `old` occurs once in the file as it stands after the previous tasks. New files are given whole.
- A pre-flight step runs **before** its task's edits: it checks the facts the text states. If one fails, stop and report; do not write the text.
- Docker steps need network access to Docker Hub, the Go module proxy, the distribution mirrors and (Task 3 only) the five vendors. A failure that names a mirror or a vendor's network is retried once before it is debugged (e2e/flat's design note 9).

## Rulings and design notes

The maintainer's decisions (Sean, 2026-10-26/27, on this plan's open decisions and the reviews):

- **D2 — Go 1.27.** Task 5 is required: CI, `release.yaml` and every e2e Dockerfile build with Go 1.27; `go.mod` keeps `go 1.25.0`; CI's golangci-lint moves to v2.14.0.
- **D4 — keep both:** R-H5 (GD-5: an adopted unsigned flat suite whose `Release` disappears keeps its snapshot) and the cache writer's fail-fast on panic. `docs/open-decisions.md` records both under *Decided*, dated 2026-10-26, the alternative kept for the record (Task 9).
- **D5 — the credit, exactly:** "Thanks to Venkat (@venkatamutyala), whose PR #8 identified the flat-repository problem; the flat-repository support in this release is based on it." (Task 8.)
- **R-H6 — keep** (all 43 prose messages this branch added renamed; none released). *Decided*, 2026-10-27 (Task 9).
- **H-2 — the package upgrade restarts the daemon: fixed in 1.1.0** (the 2026-09-08 security review's finding H-2): Task 4.

The controller's rulings on the reviews (2026-10-27):

- **PH3-1 — D1 decided:** the notes live in the tree, `docs/release-notes/1.1.0.md`, pasted into the GitHub release by the maintainer; `release.yaml` unchanged. *Decided* (controller), revisitable.
- **PH3-2 — D3 decided:** `docs/open-decisions.md` is the home of the decisions that outlive H3.
- **PH3-3 — scratch paths** go under the session scratchpad, by one variable at the top of each step (Global Constraints); Task 10's review tars too.
- **PH3-4 — Task 10's reviewers run on Opus at maximum effort** (Fable's credits are exhausted).
- **PH3-5 — D5 decided** (above).
- **PH3-6 — the notes link the release's tag,** `…/blob/1.1.0/…`, not `main`: the links never drift and work whether or not the branch is merged. For a release candidate the maintainer pastes them with `blob/1.1.0/` replaced by `blob/<rc tag>/` (the `sed` line is in `docs/open-decisions.md`). The link checker accepts `main` and `1.1.0` and checks anchors against the tree.
- **PH3-7 — every Important finding accepted**, with the reviewers' text unless a ruling changes it: E-I1, E-I2, E-I3, C-I1, C-I2, C-I3, C-I5, C-I6, V-I1, V-I2, V-I3, V-I4 (with PH3-6) and V-I5; C-I4 is replaced by PH3-8. V-I1: Task 2's helper becomes the carry's pattern, `serveUntilCleanup`, pinned by a guard test that calls it and by mutants, and a readiness failure whose `serveListeners` already returned reports that error (V-M-14). E-I3 with V-I2: the gate runs `go vet ./...`, the `e2ehooks` vet, test and tagged lint, records the linter's version, runs `make e2e` under its own `COMPOSE_PROJECT_NAME`, the apt-repo tests with their tools (no SKIP), and the race suite and the `e2ehooks` tests under `golang:1.27-bookworm` (`-e GOFLAGS=-buildvcs=false`); Step 8 reruns them after the fix wave.
- **PH3-8 — a new task: package upgrades restart a running daemon** (Task 4), before the documentation tasks that describe it.
- **PH3-9 — the minors accepted** with their text: C-M-1 to C-M-16 (C-M-8 as V-I3's text; C-M-10 by PH3-6), V-M-1 to V-M-16 (V-M-4: Task 5 is required and runs before the final task; V-M-9: R-H6 decided; V-M-15: e2e/live runs "before 1.1.0, and before later releases that change the request path, adoption or vouching"), E-M-1 to E-M-13 (E-M-13's `all_adopted` run, not only written), and the exec review's three notes (CI's timing in the pre-RC list; Task 5's script checks every edit before it writes; the final run's row in e2e/live's *Results*).
- **PH3-10 — Task 5 is required** (D2): "optional" is gone throughout; D2 is under *Decided*.
- **PH3-11 — `main_test.go`'s two `TestServe_GracefulShutdown_*` tests take `serveUntilCleanup` too** (a finding of the revision): the same defect as parked item 14, the same helper, failure path only, and the cleanup costs nothing once the test has read `serveDone`. Task 2 converts 14 daemon starts (17 with the three of execution ruling PH3-15, R-H3-6).

The planner's rulings (each says why; the controller confirmed them, as revised above):

- **R-H3-1 — the release notes live in the tree:** `docs/release-notes/1.1.0.md`, which the maintainer pastes into the GitHub release's body (or passes to `gh release edit <tag> --notes-file`); `release.yaml` keeps `generate_release_notes: true`, whose commit list then follows the pasted text. The project has never kept release notes in the tree: no `CHANGELOG`, `NEWS` or release-notes file in its history, and the tags' messages are one line (`apt-cacher-ultra 1.0.2`) or one paragraph (1.0.0). In the tree, the notes are reviewed with the code, checked against it by this plan's steps, versioned with the release they describe, and readable offline; a workflow change (`body_path`) would add a release-time failure (a tag without its file) and a second convention for release candidates, for a paste the maintainer makes once per release. Decided by PH3-1.
- **R-H3-2 — `flip_held_ms` (H2 For H3 item 9):** the row says the value is about 20000 at most, since the hold's 20 s timer can fire a few milliseconds late (Task 6 Step 9). `flipgate.(*Gate).AnchorStart` arms its own `time.NewTimer(g.holdMax)` when the request joins the hold and sets `t.held = now.Sub(joined)` after the timer or the hold's release fires and the gate's mutex is taken (`internal/flipgate/gate.go`), so a request that joined as the hold began can log a few milliseconds over 20000; e2e/flat's `commit-delay` allows 20500 and measured at most 17914. A dashboard bound of exactly 20000 would misfire; the nominal bound stays the documented one.
- **R-H3-3 — a home for the decisions that outlive H3:** a new file, `docs/open-decisions.md`, owned by the maintainer: each entry says what is open, its evidence (files, commits, tests, reports), the recommendation and the date, and moves to a *Decided* section when he rules. The overview is a development plan (consumed when the branch merges) and its decisions block records what was decided; a file in `docs/` beside the operator docs outlives the phase and is where the next contributor looks. The overview's decisions block points at it. Decided by PH3-2.
- **R-H3-4 — the live smoke's shape** (`e2e/live`, Task 3): one `debian:12` container with a release-built daemon (no build tags: the binary a release ships) and apt clients as state directories (e2e/flat's design note 2), sourcing e2e/flat's `lib.sh` (copied into the image, not duplicated; `lib.sh`'s header names what the smoke uses), with the clients using the `http://HTTPS///<host>/…` convention, so no CA is installed. The vendors' signing keys are fetched from the vendors at run time, not committed (the same keys the vendors' install instructions name); the packages' Debian dependencies are installed in the image build, directly, so every apt run of the smoke goes through the cache alone. Adoption on, `integrity.refuse_unvouched_debs = true`, every other setting the shipped default. Manual, never in CI (five vendors' availability would gate merges): before 1.1.0, and before later releases that change the request path, adoption or vouching (PH3-9). It ran green during planning (Task 3's expected output).
- **R-H3-5 — the test-output noise (the overview's H1 carry, final tests review M-8):** one `TestMain` per package (`internal/fetch`, `internal/metrics`) that sets a discarding default logger, instead of a logger in each of `internal/fetch`'s 28 `New(Options{…})` calls and an `ErrorLog` on each TLS server: `slog.SetDefault` also routes the `log` package — where `net/http` writes `http: TLS handshake error` for a server without `ErrorLog` — through the discarding handler, and a test added later is covered without remembering to. The one test that reads the default logger, `internal/metrics`' `TestGauge_CapWarningStaysOneShotAcrossResets`, sets its own and restores it. Measured: 35 `fetch retry`, 5 `TLS handshake error` and 4 `metrics_series_cap_reached` lines before, none after, the same 142 tests.
- **R-H3-6 — the cmd tests' daemon drain (the overview's H1 carry, parked item 14), as revised by PH3-7:** a helper `serveUntilCleanup(t, ctx, cancel, serve)` in a new test file starts the daemon for the 17 tests that cancel it only at their end (`defer cancel()`, the result read last: the 12 readiness waits' tests; by PH3-11, `main_test.go`'s two `TestServe_GracefulShutdown_Kills*` tests; and, by the execution ruling PH3-15 after Task 2's review, `reaper_wiring_test.go`'s `TestServeListeners_StartsTheReaperWhateverGCSays`, `TestServe_GracefulShutdown_DrainsInflight` and `TestServe_ReturnsListenError`) and registers, at the start, a `t.Cleanup` that cancels and waits for `serveListeners` to return — the `ca_constraints_log_test.go` pattern the carry names — so every failing exit, the readiness wait's or a later one, drains the daemon before the test's earlier cleanups run. The rule is that every `serveListeners` start in a cmd test drains: the tests whose own `t.Cleanup` already cancels and waits (the six of the carry and `upstream_proxy_test.go` on `serveDone`, four on a `WaitGroup`) and the two `waitServing` callers keep their pattern, and Task 2 Step 6's census checks every start. When the test passed, the helper logs nothing. (The first draft's `waitReadyOrStop` drained only the readiness failure: review V-I1.)
- **R-H3-7 — version and packaging for the RC:** nothing in the tree carries a version. `make build` and the release workflow take the version from the tag (`git describe --tags`; `main.Version` by `-ldflags`), nfpm takes it from `VERSION`, and `release.yaml` turns `1.1.0-rc1` into the Debian version `1.1.0~rc1` and marks the GitHub release a prerelease, which the apt-repository publisher skips (`scripts/select-stable-tags.sh` drops prereleases). No source file carries a version string (`grep` in Task 10). Task 10 dry-runs both builds with the RC's version.
- **R-H3-8 — the live smoke's results** go in the release notes as they were measured during planning (the overview asks for them there), dated; Task 10 reruns the smoke on the final tree and its Step 9 replaces the notes' *Testing* results with that run's date, versions and times (re-running `check_notes.py`) and adds it to `e2e/live/README.md`'s *Results*; the maintainer's run on the tagged tree replaces them again if it differs (review V-M-5). A different result on the final run is a finding (ruling R-H2-4: report, never work around).

Design notes (choices the spec leaves open):

1. **The inventories are computed, not copied.** The release notes' lists of new events and metrics come from the code at 1.0.2 (`main`, 536a1ab — `git rev-parse 1.0.2^{}` is the same commit) and at HEAD: every log message production code logs (`logMessages`, the guard tests' AST walk in `internal/observability/event_names_test.go`, run on both trees) and every `"acu_…"` string literal in non-test Go files. Task 8's pre-flight recomputes both and compares them with the notes' lists. Planning run: 77 log messages only at HEAD (76 events plus the test-only `e2e_commit_delay_enabled`, which the notes mark test-binary-only), 6 only at 1.0.2 (the six removed prose messages, as the overview's *(H1)* carry lists them), 21 metric names only at HEAD, none only at 1.0.2. Spec C11's lists say "include": they omit nine of the 21 (`acu_clock_steps_detected_total`, `acu_legacy_snapshots_quarantined_total`, `acu_serve_byhash_displaced_total` and the flip barrier's six counters), so the notes take the computed list and spec revision 13 says C11's is not exhaustive. The checker checks names (and the events' levels), not defaults or labels: the keys' defaults have their own grep (review E-M-8), the additive keys on existing lines their own script (review C-M-6).
2. **Release notes structure:** what all of it needs (adoption enabled: review C-I1), then *Before you upgrade* (the changes that can surprise a running install, each with what to do), then what is new, then logging and metrics, then the tests and the live smoke, then credits. Each item is short and links to the reference section that has the detail; the notes repeat a rule only where an operator must act before upgrading.
3. **The README stays an operator's document** (brief): it says what is supported and how to use it, in a few paragraphs, and links to `docs/configuration.md` for the rules and limits. Its *Status* paragraph is the maintainer's own voice and is left as it is.
4. **The captive-portal aftermath's `Release.gpg`** (H2 For H3 item 3): before the docs call the resulting apt error transient, a handler test pins that an adopted unsigned snapshot answers its suite's `Release.gpg` (and `InRelease`) 404 rather than the cached page (Task 6; red against a mutant that lets such a path fall through to the `url_path` row). Spec §10 item 9 and `TestServeHTTP_SnapshotMissReturns404` pinned it for the inline form only.
5. **The toolchain** (H2 For H3 item 11; decision D2: Go 1.27). Facts established during planning (2026-10-26, rechecked 2026-10-27): the local toolchain is go1.26.8; go.dev lists go1.27.3 and go1.26.10 as the supported releases, and Go 1.25's last release is go1.25.14 — Go supports each major release until two newer ones exist, so 1.25 gets no security fix since 1.27.0; `golang:1.27-bookworm` and `golang:1.27-alpine` exist on Docker Hub. Under `golang:1.27-bookworm` (go1.27.3): `go vet ./...` clean, the static build succeeds, `go test -race -timeout 10m ./...` passes in all 21 packages, and the `e2ehooks` tests pass; every e2e image builds with it, and e2e/flat (six images), e2e/deb (both), the compose test and the live smoke pass on those images (Task 10's planning). CI's golangci-lint pin, v2.12.2, cannot lint under Go 1.27 (every package fails its type check: `export data version 5 is greater than maximum supported version 4`), while v2.14.0 reports `0 issues.`. Five files name Go 1.25 for builds — `ci.yaml` (two `setup-go`), `release.yaml`, `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile` and `e2e/Dockerfile.cache` — and Task 3's `e2e/live/Dockerfile` makes a sixth. The 2026-09-08 security review raised it too (finding H-3, which also asks for `govulncheck`, dependabot and module bumps: an *Open* entry of their own in `docs/open-decisions.md`); that review's report is untracked, in the main checkout (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`), not in git. Task 5 does it, before the documentation tasks that state it.
6. **The final review's inputs** are split by area, because the branch is large: `git diff --no-ext-diff --numstat main..HEAD` at e65f67d counts 320 files, +159,612/−3,090 lines, of which 87,657 are the phase plans and 2,318 the spec; the Go production diff is 20,714 lines (freshness 10,077, cache 4,502, handler 1,741, aptpath/wallclock/simtest/observability 1,137, flipgate 938, admin 884, gc 583, other internal 472, cmd 380) and the Go test diff 47,545. Task 10 gives each lens its areas.
7. **The upgrade restart** (Task 4) is what `dh_installsystemd --no-start --restart-after-upgrade` generates, written out because nfpm has no debhelper: `deb-systemd-invoke try-restart` on an upgrade and on dpkg's `abort-*` actions, which honours `policy-rc.d` and restarts a running unit whether or not it is enabled; `init-system-helpers`, which ships it, is `Essential: yes` on every supported release, so the package needs no new dependency.

## Decisions for the maintainer

All six of the first draft's open decisions are decided (2026-10-26/27); `docs/open-decisions.md` records them (Task 9):

- **D1 — where the 1.1.0 release notes live:** in the tree, `docs/release-notes/1.1.0.md`, pasted into the GitHub release (ruling PH3-1, the controller's; revisitable).
- **D2 — the Go toolchain:** Go 1.27, `go.mod`'s `go 1.25.0` kept, golangci-lint v2.14.0 (the maintainer; Task 5).
- **D3 — the home of the decisions that outlive H3:** `docs/open-decisions.md` (ruling PH3-2).
- **D4 — R-H5 and the cache writer's fail-fast on panic:** both kept (the maintainer, 2026-10-26), with the alternative recorded: a panic that persisted data triggers repeats at every start (a crash loop, as the 2026-09 clearsign panic was); the alternative is a recover that rolls back, fails that one write and reopens the connection. R-H6 confirmed too (2026-10-27).
- **D5 — the release notes' credit for PR #8:** the maintainer's wording (above; Task 8).
- **D6 — the post-1.1.0 carries** (H2 For H3 items 3 and 11, the H1 carry about two WARNs): listed in `docs/open-decisions.md` with recommendations; none is in 1.1.0. Still open there, besides: `govulncheck`, dependabot and the module bumps (the security review's H-3).

## Review Focus

The failure modes this phase's output can have that would bite a person using the software, most likely first; each is pinned by a step of the owning task:

1. **A release note or doc sentence that is wrong** — an operator acts on it (an alert, an upgrade step) and is misled; the H2 lesson. Every claim has a pre-flight check against HEAD (Tasks 4–9), the notes' inventories are recomputed from the code at 1.0.2 and HEAD and compared mechanically (Task 8, Steps 1, 2 and 4), and the final gate reruns the pre-flights and rebuilds the inventories after the review's fixes (Task 10, Steps 3 and 8).
2. **An offline or archive cache upgraded without `suite_idle_ttl = "0s"` in `[gc]`** loses every suite no client asked for in 30 days, an hour after the first start, `gc.enabled = false` notwithstanding — and since the package upgrade now restarts a running daemon (Task 4), that first start is the upgrade. The notes put the restart and the setting first under *Before you upgrade*, say where the key goes and how to see it took (the startup line's `gc_suite_idle_ttl`), and the README's reaping paragraph and `docs/configuration.md` say it too (Tasks 6, 7, 8; the checks grep the code's default, the reaper's start and the startup line).
3. **A script that calls `POST /reconcile`** as on 1.0.x gets 403 (no `X-ACU-Admin: 1`) or, behind a proxy that forwards `Host`, 403 for the name; one that sent a bad scheme gets 400 where it got 409. The notes list each answer, the README's example shows the header and the new answers (Tasks 7, 8; checked against `internal/admin/handlers.go` at 1.0.2 and HEAD).
4. **An alert that matched a removed message, or a substring query that now matches more events,** silently changes on upgrade. The notes list the six removed messages with their replacements and the exact-match queries (Task 8; the replacements are grepped in the code, the removals computed).
5. **A live repository that changes** — a rotated key (pkg.jenkins.io now signs with its 2026 key; the 2023 one no longer verifies), a new Kubernetes minor, a renamed package, a vendor that adds or drops `InRelease` — fails the live smoke for a reason outside the cache. The smoke names the failing step, the client's apt output and the daemon's log, and its README's *Maintenance* section says which line to change for each (Task 3).

---

## File map

| Area | Files | Task |
|---|---|---|
| Test-output noise | `internal/fetch/main_test.go`, `internal/metrics/main_test.go` (new) | 1 |
| The cmd tests' daemon drain | `cmd/apt-cacher-ultra/serve_until_cleanup_test.go` (new); 13 cmd test files (17 daemon starts) | 2 |
| The live-upstream smoke | `e2e/live/Dockerfile`, `e2e/live/run.sh`, `e2e/live/smoke.sh`, `e2e/live/README.md` (new); `Makefile` (`e2e-live`); `e2e/flat/lib.sh` (a header comment) | 3 |
| The package upgrade's restart | `packaging/scripts/postinstall.sh`, `packaging/scripts/preremove.sh` (its comment); `e2e/deb/Dockerfile`, `e2e/deb/test.sh` (phase 5) | 4 |
| The Go 1.27 toolchain | `.github/workflows/ci.yaml`, `.github/workflows/release.yaml`, `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile`, `e2e/live/Dockerfile`, `e2e/Dockerfile.cache`, `e2e/flat/README.md` | 5 |
| Operator docs and the portal pin | `internal/handler/portal_aftermath_test.go` (new); `docs/configuration.md`, `docs/admin-ui-spec.md`, `docs/log-fields.md` | 6 |
| README | `README.md` | 7 |
| Release notes | `docs/release-notes/1.1.0.md` (new) | 8 |
| Spec, decisions, overview | `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 13), `docs/open-decisions.md` (new), the overview | 9 |
| RC dry run, the gate, the final review | none (runs, records, dispatches reviews); fix commits as the review finds; `docs/open-decisions.md`, the release notes' *Testing*, `e2e/live/README.md`'s *Results*, the overview's stamp | 10 |

No migration, no new Go package, no config key, no production Go code path; the package's postinst changes (Task 4).

## Tasks

1. Test-output noise: one `TestMain` in `internal/fetch` and `internal/metrics`
2. The cmd tests' daemon drain: `serveUntilCleanup`
3. The live-upstream smoke: `e2e/live` and `make e2e-live`
4. The package upgrade restarts a running daemon
5. The Go 1.27 toolchain
6. Operator docs: strict mode on flat hosts, the unsigned confirmation, the captive portal's aftermath (with its pin), binary `Release.gpg`, `flip_held_ms`, the restart on upgrade
7. README: repositories, suite reaping and `/forget`, the admin answers, testing
8. The 1.1.0 release notes
9. Spec revision 13, `docs/open-decisions.md` and the overview
10. The release-candidate dry run, the full gate and the final multi-lens review

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
// default logger's output sets slog.SetDefault itself and restores it, and
// does not call t.Parallel.
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
// default logger's output sets slog.SetDefault itself and restores it, and
// does not call t.Parallel.
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

As executed, Task 1 is two commits: the one above (c07e974) and, after the task's review, its one Minor (4fa2c78, execution ruling PH3-14): the AIDEV-NOTEs gained "and does not call t.Parallel", as Steps 2 and 3 now show them. The default logger is process-global, so a parallel test that swapped it would capture other tests' lines; neither package calls `t.Parallel` (`grep -rn 't\.Parallel()' internal/fetch internal/metrics` prints nothing). Steps 4 and 5 print the same on the reworded files.

```bash
git add internal/fetch/main_test.go internal/metrics/main_test.go
git commit -F - <<'EOF'
Tests: a test that swaps the default logger does not run in parallel

The TestMain AIDEV-NOTEs in internal/fetch and internal/metrics now
say that a test that needs the default logger's output, besides setting
slog.SetDefault itself and restoring it, does not call t.Parallel: the
default logger is process-global, so a parallel test that swaps it
would capture concurrently running tests' lines (a count assertion such
as TestGauge_CapWarningStaysOneShotAcrossResets' would flake) and undo
the silencing for them while it runs. Neither package calls t.Parallel
today. Comment only. The H3 Task 1 review's Minor (ruling PH3-14).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 2: The cmd tests' daemon drain: `serveUntilCleanup`

The overview's *(H1)* carry (H1 final review, parked item 14): 17 of the cmd tests that run `serveListeners` in a goroutine have no drain of their own — they only `defer cancel()` and read its result at their end: 12 with a readiness wait (the carry's callers), `main_test.go`'s two `TestServe_GracefulShutdown_Kills*` tests, which have the same defect without one (ruling PH3-11), and three in other shapes that the task's review found (execution ruling PH3-15): `reaper_wiring_test.go`'s `TestServeListeners_StartsTheReaperWhateverGCSays`, and `main_test.go`'s `TestServe_GracefulShutdown_DrainsInflight` and `TestServe_ReturnsListenError` — so any `t.Fatal*` before that end — a readiness wait's, or one of the later ones (40 in the twelve: a failed dial, a failed assertion) — returns with the daemon still running, racing the `shutdownTimeout` restore (a data race under `-race`: `serveListeners` reads it for its drain, `cmd/apt-cacher-ultra/main.go:836`) and the `TempDir` removal their cleanups make. Failure path only; it never masks a pass. The carry's remedy is a helper that starts `serveListeners` and registers cancel-plus-wait at the start (the `ca_constraints_log_test.go` pattern), so every exit drains. Test-only (ruling R-H3-6, as revised by PH3-7 for review V-I1 and by PH3-15).

**Files:**
- Create: `cmd/apt-cacher-ultra/serve_until_cleanup_test.go`
- Modify: the daemon start (four lines each) in `cmd/apt-cacher-ultra/connect_log_test.go`, `connect_shutdown_test.go` (two), `mitm_metrics_integration_test.go`, `cert_cache_evicted_test.go`, `inner_get_deny_range_test.go`, `connect_tls_failed_log_test.go`, `cert_issued_test.go`, `shutdown_orphan_blob_test.go`, `clock_skew_log_test.go`, `ca_expiry_test.go`, `mitm_handshake_metric_test.go`, `main_test.go` (two, and two in other shapes: `TestServe_GracefulShutdown_DrainsInflight`'s `WaitGroup` start and its end, `TestServe_ReturnsListenError`'s start), `reaper_wiring_test.go` (its second test's one-line start)

**Interfaces:**
- Consumes: `serveListeners` (`cmd/apt-cacher-ultra/main.go`, unchanged); each converted test's `ctx`, `cancel` (from `context.WithCancel`) and its later reads of the result (`serveDone`; `done` in `TestServe_ReturnsListenError`), unchanged but for `TestServe_GracefulShutdown_DrainsInflight`'s `wg.Wait()` and `serveErr` check, which become a read of `serveDone`.
- Produces: `serveUntilCleanup(t testing.TB, ctx context.Context, cancel context.CancelFunc, serve func(context.Context) error) <-chan error`, a test helper of package `main`: it starts `serve(ctx)` in a goroutine, returns the channel that carries its result (buffered, one value), and registers a `t.Cleanup` that cancels and waits for `serve` to return (15 s at most, then `t.Errorf`), and, when the test failed and `serve` had returned before any cancel, logs `serve`'s error (a test that passed logs nothing).

The tests that drain already keep their own drain, a `t.Cleanup` registered right after the start that cancels the daemon and waits: on `serveDone` in the six of the carry (`ca_constraints_log_test.go`, `ca_lifecycle_log_test.go`, `legacy_audit_wiring_test.go`, `reaper_wiring_test.go`'s first test, `TestServe_ForgetIsWired`, `status_tls_mitm_integration_test.go`, `disabled_mode_parity_daemon_test.go`) and `upstream_proxy_test.go` (its `t.Cleanup(shutdown)`), or on a `WaitGroup` in `main_test.go`'s `TestServe_EndToEnd_MirrorFetchAndCache` and `TestServe_DisallowedHostReturnsForbidden`, `upstream_invalid_cert_test.go` and `upstream_redirect_test.go`; `suite_caps_wiring_test.go` and `shutdown_drain_test.go` cancel and wait right after `waitServing`, which drains on its own timeout. `main_test.go`'s two `TestServe_GracefulShutdown_Kills*` tests have no readiness wait, but the same shape and the same defect (a `t.Fatalf` such as "warm get" or "slow dial" before their own cancel), so they take the helper too (ruling PH3-11): failure path only, and once a test has read `serveDone` at its end the cleanup costs nothing — those two time their own shutdown after their own `cancel()`, which the helper does not touch. The task's review found three more with the same defect in other shapes, which the plan's list had missed (Step 6's `grep -l` works per file, and `reaper_wiring_test.go`'s first test drains): `TestServeListeners_StartsTheReaperWhateverGCSays` (a one-line start; its readiness `t.Fatal`s precede its cancel), `TestServe_GracefulShutdown_DrainsInflight` (a `WaitGroup` start; `t.Fatalf`s before and after its cancel, all before its `wg.Wait()`) and `TestServe_ReturnsListenError` (its daemon returns on the listener's close by design, so only its 5 s timeout can leave it running; it is the one caller whose passing run reaches the early log's `t.Failed()` check, which the fourth subtest pins). Ruling PH3-15 converts them, so the rule is "every `serveListeners` start in a cmd test drains", not a list of exceptions; Step 6's census checks it.

- [ ] **Step 1: Write the guard test first**

Create `cmd/apt-cacher-ultra/serve_until_cleanup_test.go` with the imports, the fake `testing.TB` and the test alone:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTB records what serveUntilCleanup asks of a testing.TB: its cleanups
// (runCleanups runs them, last first, as the testing package does when a
// test returns or calls t.Fatal), its Errorf and Logf lines, and the
// Failed answer the test sets.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errs     []string
	logs     []string
	failed   bool
}

func (f *fakeTB) Helper()           {}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Failed() bool { return f.failed }

func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// TestServeUntilCleanup pins serveUntilCleanup: a test that fails before
// its end leaves a daemon that its cleanup cancels and waits for; a test
// that read done at its end costs the cleanup nothing; and a daemon that
// returned before any cancel has its error logged when the test failed,
// and only then.
func TestServeUntilCleanup(t *testing.T) {
	// slowServe is a daemon that serves until ctx is cancelled, then takes
	// 200 ms to stop, and sets stopped when it has.
	slowServe := func(stopped *atomic.Bool) func(context.Context) error {
		return func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond)
			stopped.Store(true)
			return nil
		}
	}

	t.Run("a test that fails before its end", func(t *testing.T) {
		ft := &fakeTB{TB: t, failed: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stopped atomic.Bool
		serveUntilCleanup(ft, ctx, cancel, slowServe(&stopped))
		ft.runCleanups() // the test's t.Fatal: done is never read
		if !stopped.Load() {
			t.Fatal("the cleanup returned before the daemon had stopped")
		}
		if len(ft.errs) != 0 || len(ft.logs) != 0 {
			t.Fatalf("errors %q, logs %q; want none", ft.errs, ft.logs)
		}
	})

	t.Run("a test that read done at its end", func(t *testing.T) {
		ft := &fakeTB{TB: t}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stopped atomic.Bool
		done := serveUntilCleanup(ft, ctx, cancel, slowServe(&stopped))
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("serve returned %v", err)
		}
		start := time.Now()
		ft.runCleanups()
		if took := time.Since(start); took > time.Second || len(ft.errs) != 0 {
			t.Fatalf("the cleanup took %v and reported %q; want at once, nothing", took, ft.errs)
		}
	})

	t.Run("a daemon that returned before any cancel", func(t *testing.T) {
		ft := &fakeTB{TB: t, failed: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := serveUntilCleanup(ft, ctx, cancel, func(context.Context) error {
			return errors.New("listen tcp 127.0.0.1:1: bind: address already in use")
		})
		<-done // as a readiness wait that timed out would find it
		ft.runCleanups()
		if len(ft.logs) != 1 || !strings.Contains(ft.logs[0], "returned before any cancel: listen tcp") {
			t.Fatalf("logs %q; want serve's error, once", ft.logs)
		}
	})

	t.Run("a passing test whose daemon returned before any cancel", func(t *testing.T) {
		ft := &fakeTB{TB: t}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := serveUntilCleanup(ft, ctx, cancel, func(context.Context) error {
			return errors.New("http: accept tcp 127.0.0.1:1: use of closed network connection")
		})
		<-done // as TestServe_ReturnsListenError reads it
		ft.runCleanups()
		if len(ft.errs) != 0 || len(ft.logs) != 0 {
			t.Fatalf("errors %q, logs %q; want none", ft.errs, ft.logs)
		}
	})
}
```

The fake `testing.TB` is how the test observes a failing test's exit: a real `t.Fatal` would fail this test too, so the fake records the helper's cleanups and the test runs them, last first, as the testing package does after a `t.Fatal` (embedding `testing.TB` supplies the interface's unexported method; every method the helper calls is overridden).

- [ ] **Step 2: Run it to see it fail**

Run: `go test -count=1 -run 'TestServeUntilCleanup' ./cmd/apt-cacher-ultra/`
Expected: the build fails: `undefined: serveUntilCleanup` (four times, one per subtest).

- [ ] **Step 3: Add the helper**

In `cmd/apt-cacher-ultra/serve_until_cleanup_test.go`, between the import block and `// fakeTB records`:

```go
// serveUntilCleanup starts serve on ctx and registers a t.Cleanup that
// cancels it and waits for it to return (15 s at most). Any t.Fatal* after
// the start — the readiness wait's or a later one — then stops the daemon
// before the cleanups the test registered earlier (the shutdownTimeout
// restore, the TempDir removal) run: the H1 final review's parked item 14,
// done with the ca_constraints_log_test.go pattern. done carries serve's
// result for a test that reads it at its end; the cleanup waits on stopped,
// which the goroutine closes after sending, so a test that already read
// done costs it nothing. When the test failed and serve had returned
// before any cancel (a bind or configuration error, say), the cleanup logs
// serve's error: a readiness failure then says why the daemon never
// answered. A test that passed logs nothing, even when serve returned
// before any cancel (TestServe_ReturnsListenError's does, by design).
//
// AIDEV-NOTE: every cmd test that runs serveListeners in a goroutine drains
// it on every exit. The 17 with no drain of their own (defer cancel(), the
// result read last) start it with this helper: the 12 with a readiness
// wait, reaper_wiring_test.go's
// TestServeListeners_StartsTheReaperWhateverGCSays, and main_test.go's
// three TestServe_GracefulShutdown_* tests and TestServe_ReturnsListenError.
// The others keep their own: a t.Cleanup, registered right after the
// start, that cancels and waits, on serveDone in ca_constraints_log_test.go,
// ca_lifecycle_log_test.go, disabled_mode_parity_daemon_test.go,
// legacy_audit_wiring_test.go, reaper_wiring_test.go's
// TestServe_ForgetIsWired, status_tls_mitm_integration_test.go and
// upstream_proxy_test.go, or on a WaitGroup in main_test.go's EndToEnd and
// DisallowedHost tests, upstream_invalid_cert_test.go and
// upstream_redirect_test.go; suite_caps_wiring_test.go and
// shutdown_drain_test.go cancel and wait right after waitServing, which
// drains on its own timeout. A new daemon test starts with this helper.
// TestServeUntilCleanup pins it through a fake testing.TB whose cleanups
// it runs as a t.Fatal would.
func serveUntilCleanup(t testing.TB, ctx context.Context, cancel context.CancelFunc,
	serve func(context.Context) error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	stopped := make(chan struct{})
	var serveErr error
	var early bool
	go func() {
		err := serve(ctx)
		serveErr, early = err, ctx.Err() == nil
		done <- err
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Errorf("serveListeners had not returned 15s after the cancel")
			return
		}
		if early && t.Failed() {
			t.Logf("serveListeners returned before any cancel: %v", serveErr)
		}
	})
	return done
}
```

`serveErr` and `early` are written before `stopped` is closed and read only after the cleanup has seen it closed, so the read is ordered after the write (`-race` agrees; the "no wait" mutant below shows it).

- [ ] **Step 4: Run it to see it pass, then its mutants fail**

Run: `go test -race -count=1 -run 'TestServeUntilCleanup' -v ./cmd/apt-cacher-ultra/`
Expected: `--- PASS: TestServeUntilCleanup` and its four subtests (0.4 s in planning and as built).

Mutants (one at a time, each reverted after), with the same command:

| Mutant (in `serveUntilCleanup`'s cleanup) | Fails with |
|---|---|
| no cancel: delete the cleanup's `cancel()` | `serve_until_cleanup_test.go:125: the cleanup returned before the daemon had stopped` (after the 15 s bound) |
| no wait: delete the `select { … }` block (six lines) | `serve_until_cleanup_test.go:120: the cleanup returned before the daemon had stopped`, then a `WARNING: DATA RACE` on `early` (the goroutine's write, `:54`, against the cleanup's read, `:60`), charged to the second subtest: the write comes 200 ms after the read |
| waits on `done`: `case <-stopped:` → `case <-done:` in that block | `:146: the cleanup took 15.0…s and reported ["serveListeners had not returned 15s after the cancel"]; want at once, nothing`, then `:160: logs []; want serve's error, once` and `:174: errors ["serveListeners had not returned 15s after the cancel"], logs []; want none` |
| early log inverted: `if early && t.Failed() {` → `if early && !t.Failed() {` | `:160: logs []; want serve's error, once`, then `:174: errors [], logs ["serveListeners returned before any cancel: http: accept tcp 127.0.0.1:1: use of closed network connection"]; want none` |
| early log unconditional: `if early && t.Failed() {` → `if early {` | `:174: errors [], logs ["serveListeners returned before any cancel: http: accept tcp 127.0.0.1:1: use of closed network connection"]; want none` (the first three subtests pass: only the fourth pins the `t.Failed()` half) |

(The line numbers are the mutated file's; unmutated, the first two failures sit at line 126.)

- [ ] **Step 5: Convert the 17 daemon starts**

Fourteen are exactly these four lines (`logger` is `newTestLogger()` in some), with `ctx` and `cancel` from `context.WithCancel` in scope:

```go
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	}()
```

and becomes:

```go
	serveDone := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {
		return serveListeners(ctx, cfg, logger, cacheLn, nil, nil, nil)
	})
```

The readiness wait, `defer cancel()` and the test's final read of `serveDone` stay as they are. Run (from the repository root):

```bash
python3 - <<'EOF'
import pathlib
import re
import sys

files = ["connect_log_test.go", "connect_shutdown_test.go", "mitm_metrics_integration_test.go",
         "cert_cache_evicted_test.go", "inner_get_deny_range_test.go", "connect_tls_failed_log_test.go",
         "cert_issued_test.go", "shutdown_orphan_blob_test.go", "clock_skew_log_test.go",
         "ca_expiry_test.go", "mitm_handshake_metric_test.go", "main_test.go"]
old = re.compile(r"\tserveDone := make\(chan error, 1\)\n"
                 r"\tgo func\(\) \{\n"
                 r"\t\tserveDone <- (serveListeners\(ctx, [^\n]*\))\n"
                 r"\t\}\(\)\n")
new = ("\tserveDone := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {\n"
       "\t\treturn \\1\n"
       "\t})\n")
total = 0
for name in files:
    p = pathlib.Path("cmd/apt-cacher-ultra") / name
    s, n = old.subn(new, p.read_text())
    if n == 0:
        sys.exit(f"{p}: no serveListeners start")
    p.write_text(s)
    total += n
print(f"replaced {total} blocks")
sys.exit(0 if total == 14 else 1)
EOF
```

Expected: `replaced 14 blocks` (two each in `connect_shutdown_test.go` and `main_test.go`).

The other three have other shapes (ruling PH3-15): `reaper_wiring_test.go`'s second test starts the daemon in one line; `TestServe_GracefulShutdown_DrainsInflight` starts it under a `WaitGroup` and ends with `wg.Wait()` and a check of `serveErr`, which become a read of `serveDone` (the `sync` import stays: `sync.Once` is still used); `TestServe_ReturnsListenError` keeps its channel's name, `done`. The script checks every edit before it writes. Run:

```bash
python3 - <<'EOF'
import pathlib
import sys

d = pathlib.Path("cmd/apt-cacher-ultra")
start = "\tserveDone := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {\n"
edits = {
    "reaper_wiring_test.go": [
        ("\tserveDone := make(chan error, 1)\n"
         "\tgo func() { serveDone <- serveListeners(ctx, cfg, silentBuildLogger(), ln, nil, nil, nil) }()\n",
         start + "\t\treturn serveListeners(ctx, cfg, silentBuildLogger(), ln, nil, nil, nil)\n\t})\n"),
    ],
    "main_test.go": [
        ("\tvar wg sync.WaitGroup\n\twg.Add(1)\n\tvar serveErr error\n\tgo func() {\n\t\tdefer wg.Done()\n"
         "\t\tserveErr = serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)\n\t}()\n",
         start + "\t\treturn serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)\n\t})\n"),
        ('\twg.Wait()\n\tif serveErr != nil {\n\t\tt.Fatalf("serveListeners: %v", serveErr)\n\t}\n}\n',
         '\tif err := <-serveDone; err != nil {\n\t\tt.Fatalf("serveListeners: %v", err)\n\t}\n}\n'),
        ("\tdone := make(chan error, 1)\n\tgo func() {\n"
         "\t\tdone <- serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)\n\t}()\n",
         "\tdone := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {\n"
         "\t\treturn serveListeners(ctx, cfg, newTestLogger(), cacheLn, nil, nil, nil)\n\t})\n"),
    ],
}
out = {}
for name, pairs in edits.items():
    p = d / name
    s = p.read_text()
    for old, new in pairs:
        if s.count(old) != 1:
            sys.exit(f"{p}: {s.count(old)} matches for {old!r}; nothing written")
        s = s.replace(old, new)
    out[p] = s
for p, s in out.items():
    p.write_text(s)
print("converted 3 tests")
EOF
```

Expected: `converted 3 tests`.

- [ ] **Step 6: Check where the helper is called, and which daemons are still started by hand**

Run:

```bash
grep -c 'serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {' cmd/apt-cacher-ultra/*_test.go | grep -v ':0$' | sort
grep -l 'serveDone := make(chan error, 1)' cmd/apt-cacher-ultra/*_test.go | sort
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
cmd/apt-cacher-ultra/main_test.go:4
cmd/apt-cacher-ultra/mitm_handshake_metric_test.go:1
cmd/apt-cacher-ultra/mitm_metrics_integration_test.go:1
cmd/apt-cacher-ultra/reaper_wiring_test.go:1
cmd/apt-cacher-ultra/shutdown_orphan_blob_test.go:1
cmd/apt-cacher-ultra/ca_constraints_log_test.go
cmd/apt-cacher-ultra/ca_lifecycle_log_test.go
cmd/apt-cacher-ultra/disabled_mode_parity_daemon_test.go
cmd/apt-cacher-ultra/legacy_audit_wiring_test.go
cmd/apt-cacher-ultra/reaper_wiring_test.go
cmd/apt-cacher-ultra/status_tls_mitm_integration_test.go
cmd/apt-cacher-ultra/upstream_proxy_test.go
```

The first list is the 17 starts in 13 files. The second is the six tests whose own cleanup drains and `upstream_proxy_test.go` (its `t.Cleanup(shutdown)`); `reaper_wiring_test.go` is in both, since `grep -l` works per file (its first test drains, its second takes the helper). So the rule — every `serveListeners` start in a cmd test drains — is checked by a census of every call, one line per form:

```bash
grep -h 'serveListeners(' cmd/apt-cacher-ultra/*_test.go | sed -E 's/^\s+//; s/serveListeners\(.*/serveListeners(…)/' | LC_ALL=C sort | uniq -c
```

Expected:

```
      1 _ = serveListeners(…)
      1 err = serveListeners(…)
      2 go func() { done <- serveListeners(…)
      2 go func() { serveDone <- serveListeners(…)
     17 return serveListeners(…)
      5 serveDone <- serveListeners(…)
      3 serveErr := serveListeners(…)
      3 serveErr = serveListeners(…)
```

`return` is the helper's 17 callers; `serveDone <-` (seven, two of them one-line) the seven with their own `t.Cleanup` on `serveDone`; `serveErr =` and `_ =` the four `WaitGroup` starts with a `t.Cleanup` that cancels and waits; `done <-` the two `waitServing` callers (`suite_caps_wiring_test.go`, `shutdown_drain_test.go`); `serveErr :=` and `err =` four synchronous calls, which return before the test goes on. A start in a new form changes this output, and its drain is then checked by hand.

- [ ] **Step 7: The package under `-race`, vet, lint, format**

Run: `go vet ./cmd/apt-cacher-ultra/ && go test -race -count=1 ./cmd/apt-cacher-ultra/ && golangci-lint run ./cmd/... && gofmt -l cmd`
Expected: `ok` for the package (about 32–40 s), `0 issues.`, and `gofmt -l` lists only the drift file `cmd/apt-cacher-ultra/ca_print.go`.

- [ ] **Step 8: Commit**

```bash
git add cmd/apt-cacher-ultra/serve_until_cleanup_test.go cmd/apt-cacher-ultra/connect_log_test.go \
    cmd/apt-cacher-ultra/connect_shutdown_test.go cmd/apt-cacher-ultra/mitm_metrics_integration_test.go \
    cmd/apt-cacher-ultra/cert_cache_evicted_test.go cmd/apt-cacher-ultra/inner_get_deny_range_test.go \
    cmd/apt-cacher-ultra/connect_tls_failed_log_test.go cmd/apt-cacher-ultra/cert_issued_test.go \
    cmd/apt-cacher-ultra/shutdown_orphan_blob_test.go cmd/apt-cacher-ultra/clock_skew_log_test.go \
    cmd/apt-cacher-ultra/ca_expiry_test.go cmd/apt-cacher-ultra/mitm_handshake_metric_test.go \
    cmd/apt-cacher-ultra/main_test.go
git commit -F - <<'EOF'
cmd tests: stop the daemon on every failing exit

Fourteen cmd tests cancel their daemon only at their end (defer
cancel(), serveDone read last): twelve with a readiness wait and
main_test.go's two graceful-shutdown tests. Any t.Fatal before that
end - a readiness wait's or a later one - left the daemon running past
the test, racing its cleanups (the shutdownTimeout restore, the TempDir
removal).
serveUntilCleanup starts serveListeners and registers, at the start,
a cleanup that cancels it and waits for it to return (the
ca_constraints_log_test.go pattern the carry names); when the test
failed and serveListeners had returned before any cancel, the cleanup
logs its error, so a readiness failure says why. The six tests whose
own cleanup drains, and upstream_proxy_test.go, keep their pattern.
The H1 final review's parked item 14 (the overview's H1 carry).

Pinned by TestServeUntilCleanup, through a fake testing.TB whose
cleanups it runs as a t.Fatal would: a cleanup without its cancel or
its wait fails it (serve_until_cleanup_test.go: the cleanup returned
before the daemon had stopped), as do one that waits on done and one
that drops the early error.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

As executed, Task 2 is two commits: the one above (5cc1d08: the helper with its first AIDEV-NOTE and three subtests, and the fourteen four-line starts) and, after the task's review, fix round 1 (766e7eb, execution ruling PH3-15: Step 5's three other starts, the fourth subtest, and the doc comments and AIDEV-NOTE as Steps 1 and 3 now show them). Steps 1–7 describe the tree after both.

```bash
git add cmd/apt-cacher-ultra/serve_until_cleanup_test.go cmd/apt-cacher-ultra/main_test.go \
    cmd/apt-cacher-ultra/reaper_wiring_test.go
git commit -F - <<'EOF'
cmd tests: every daemon start drains, three more tests and a quiet pass

5cc1d08's fourteen conversions missed three tests with the same
failure-path defect (the H3 Task 2 review's Important 1, ruling PH3-15):
reaper_wiring_test.go's TestServeListeners_StartsTheReaperWhateverGCSays
(a one-line start whose early t.Fatal raced its shutdownTimeout and
runReaper restore and its TempDir removal: with a t.Fatal injected after
the start, 17 of 40 runs ended with "TempDir RemoveAll cleanup: ...
directory not empty", none of 40 once converted), main_test.go's
TestServe_GracefulShutdown_DrainsInflight (a WaitGroup start; its
wg.Wait() and serveErr check become a read of serveDone) and
TestServe_ReturnsListenError (its daemon returns on the listener's
close; only its 5 s timeout could leave it running). They start the
daemon with serveUntilCleanup now, so every cmd test that runs
serveListeners in a goroutine drains it on every exit; the helper's
AIDEV-NOTE states that rule and names the 17 callers and the tests that
keep their own drain.

TestServeUntilCleanup gains a fourth subtest: a passing test whose
daemon returned before any cancel (TestServe_ReturnsListenError's case)
logs nothing. The t.Failed() half of the early log was unpinned: with
"if early {" the three subtests passed; the fourth fails it
(serve_until_cleanup_test.go:174: logs ["serveListeners returned before
any cancel: ..."]; want none).

Not changed (pre-existing, failure path only): DrainsInflight's "upstream
never saw the in-flight request" exit can hang in its deferred
upstream.Close() when the request reaches the upstream late.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 3: The live-upstream smoke: `e2e/live` and `make e2e-live`

The overview's "PR #8 parity" H (*H3's*): with adoption enabled and `integrity.refuse_unvouched_debs = true`, front live pkgs.k8s.io (two minors, one client per source form), NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and download.sublimetext.com; for each, `adoption_success` is logged, `apt-get update` is clean under e2e/flat's error grep (its final form, with `^(E|Error):` on these hosts), one package installs, and `unvouched_deb_refused` is zero. Manual, not CI (ruling R-H3-4): before 1.1.0, and before later releases that change the request path, adoption or vouching (review V-M-15). The spec and the overview ask for no unsigned or Release-less live repository (they name these five), so the smoke has none; `e2e/flat` covers both shapes with fixtures.

What the live repositories looked like on 2026-10-26 (planning probes, 18:53 UTC; the smoke then ran at 19:00 and 19:21 UTC, and the revised smoke on 2026-10-27 at 14:00 UTC, against the same versions):

| Repository | Sources line (both k8s forms) | Anchors upstream | Hash blocks | `Date` | Packages |
|---|---|---|---|---|---|
| pkgs.k8s.io v1.36, v1.37 | `deb <uri>/core:/stable:/v1.3x/deb/ /` and `deb <uri>/core:/stable:/v1.3x/deb ./` | `InRelease`, `Release`, `Release.gpg` (each redirects once to `prod-cdn.packages.k8s.io`); v1.33–v1.37 published, v1.38 answers 403 | MD5Sum, SHA1, SHA256 | `Wed Sep 23 … 2026` (asctime form) | `amd64/…` Filenames; `kubectl` depends on nothing |
| NVIDIA CUDA debian12/x86_64 | `deb <uri>/ /` | `InRelease`, `Release`, `Release.gpg`; `Acquire-By-Hash: no` | MD5Sum, SHA1, SHA256 | `Mon, 26 Oct 2026 15:30:24 +0000`, the day of the probe | 9,758 stanzas, `./…` Filenames |
| libnvidia-container stable/deb/amd64 | `deb <uri>/ /` | `InRelease` only (`Release`, `Release.gpg` 404) | SHA512 only, over `Packages` and `Packages.xz` | `Fri, 27 Apr 2018` (frozen) | `./…` Filenames |
| pkg.jenkins.io debian-stable | `deb <uri> binary/` | `Release` and `Release.gpg` (`InRelease` 404): detached, signed by the 2026 key `7198F4B714ABFC68` | MD5Sum, SHA1, SHA256, SHA512 | `Wed, 30 Sep 2026` | 181 stanzas, `binary/…` and `direct/…` Filenames (one level above the `Release`); `jenkins` depends on `adduser`, `net-tools`, `sysvinit-utils`, `init-system-helpers` |
| download.sublimetext.com | `deb <uri>/ apt/stable/` | `InRelease`, `Release`, `Release.gpg` | MD5Sum, SHA1, SHA256, SHA512 | `Fri, 25 Sep 2026` | `files/…` Filenames (at the host root); `sublime-merge` depends on `libgtk-3-0`, `libcurl4` |

**Files:**
- Create: `e2e/live/Dockerfile`, `e2e/live/run.sh`, `e2e/live/smoke.sh`, `e2e/live/README.md`
- Modify: `Makefile` (`.PHONY` and the `e2e-live` target), `e2e/flat/lib.sh` (a header comment)

**Interfaces:**
- Consumes: e2e/flat's `lib.sh` (copied into the image by the Dockerfile; its code unchanged, its header gains an `AIDEV-NOTE` naming what the smoke uses, Step 6): the 20 helpers `scenario`, `passed`, `fail`, `show_logs`, `poll_quietly`, `log_mark`, `log_lines`, `log_has`, `event`, `kv`, `suite_kv`, `panics_in_log`, `start_daemon`, `stop_daemon`, `suite`, `client`, `capt`, `apt_update`, `apt_errors`, `candidate`, the settings `CONFIG`, `DAEMON_LOG`, `PROXY`, `ADMIN`, `CURL`, `APT_SIGNED_ERRORS`, and the run state `SCENARIO`, `FAILED`, `APT_RUNS`, `APT_LOG`, `TIMINGS`, `POLL_TOOK`. `smoke.sh` overrides `say`, `NGINX_LOG` and `APT_SIGNED_HOSTS` after sourcing it.
- Produces: `make e2e-live`; `E2E_LIVE_NAME` (the image and container name, default `acu-e2e-live`). The results go in the release notes (Task 8) and are rerun in Task 10.

- [ ] **Step 1: Pre-flight: lib.sh's helpers, and the live repositories as the smoke expects them**

Run:

```bash
grep -cE '^(scenario|passed|fail|show_logs|poll_quietly|log_mark|log_lines|log_has|event|kv|suite_kv|panics_in_log|start_daemon|stop_daemon|suite|client|capt|apt_update|apt_errors|candidate)\(\) \{' e2e/flat/lib.sh
grep -cE '^(CONFIG|DAEMON_LOG|PROXY|ADMIN|CURL|APT_SIGNED_ERRORS)=' e2e/flat/lib.sh
for u in https://pkgs.k8s.io/core:/stable:/v1.36/deb/InRelease https://pkgs.k8s.io/core:/stable:/v1.37/deb/InRelease \
    https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/InRelease \
    https://nvidia.github.io/libnvidia-container/stable/deb/amd64/InRelease \
    https://pkg.jenkins.io/debian-stable/binary/InRelease https://pkg.jenkins.io/debian-stable/binary/Release.gpg \
    https://download.sublimetext.com/apt/stable/InRelease https://download.sublimetext.com/apt/acu-no-such/InRelease; do
    printf '%s %s\n' "$(curl -sL -o /dev/null -w '%{http_code}' --max-time 30 "$u")" "$u"
done
```

Expected: `20`, `6`, then `200` for every URL but Jenkins' `InRelease`, which is `404` (a detached-only repository), and the host-root control's `apt/acu-no-such/InRelease`, `404` too (a suite that does not exist). A different answer is the vendor's change: update the smoke's values (the README's *Maintenance*) before going on, and say so in the commit.

- [ ] **Step 2: Write `e2e/live/Dockerfile`**

```dockerfile
# e2e/live: the live-upstream smoke (the flat-repositories overview, "PR #8
# parity", H): real apt clients against the live flat repositories
# pkgs.k8s.io, NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and
# download.sublimetext.com through apt-cacher-ultra, in one container.
# Manual, before 1.1.0 and before later releases that change the request
# path, adoption or vouching; never in CI (it depends on five vendors).
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
# smoke (e2e/live/README.md). Manual, before 1.1.0 and before later
# releases that change the request path, adoption or vouching; it reaches
# five vendors' live repositories, so it is never run in CI.
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

# The repositories. The k8s minors are any two pkgs.k8s.io publishes; the
# keys are the ones each vendor's install instructions name (README,
# "Maintenance").
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
    # The signed-host filter's host-root control: Sublime's shape (apt
    # names it with a space after the host) at a suite that does not
    # exist, answered 404, which prints only an "E:" line.
    client control-root "$PROXY" "$(src sublime "download.sublimetext.com/ apt/acu-no-such/")"
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
    # APT_ERRORS alone catches the 403 ("Failed to fetch"); a signature
    # error (NO_PUBKEY, a bad signature) prints only "GPG error" and "is
    # not signed" lines, which only APT_SIGNED_HOSTS sees: so the control's
    # "is not signed" line must match this smoke's APT_SIGNED_HOSTS too.
    [[ -n $(grep -E -- "$APT_SIGNED_HOSTS" "$APT_LOG" | grep -E -- 'is not signed' || true) ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "APT_SIGNED_HOSTS missed the control's 'is not signed' line (above): a signature error on these hosts would pass unseen"
    }
    say "control: the error grep catches the missing minor's error"
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/control-root/update.$APT_RUNS.log
    capt control-root apt-get update > "$APT_LOG" 2>&1 || true
    [[ -n $(grep -E -- "$APT_SIGNED_HOSTS" "$APT_LOG" | grep -E -- "$APT_SIGNED_ERRORS" || true) ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "APT_SIGNED_HOSTS missed the host-root control's error (above): an error on Sublime's source would pass unseen"
    }
    say "control-root: the signed-host filter catches a host-root source's error"
    passed
}

# all_adopted: every suite serves a snapshot of its form and logged
# adoption_success. One status fetch per poll, not two per suite (each
# fetch logs an admin_request line).
all_adopted() {
    local json host path form
    json=$("${CURL[@]}" -fsS "$ADMIN/?format=json") || return 1
    while read -r host path form; do
        jq -e --arg h "$host" --arg p "$path" --arg f "$form" \
            '.suites[] | select(.host == $h and .suite_path == $p and .current_snapshot_id != null and .anchor_form == $f)' \
            <<< "$json" > /dev/null || return 1
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

A manual check — before 1.1.0, and before later releases that change the
request path, adoption or vouching — that apt-cacher-ultra fronts real flat
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

It needs Docker on an amd64 host (the CUDA repository and the packages are
amd64), and network access to Docker Hub, the Go module proxy (the build
stage's `go mod download`), `deb.debian.org` (the image build) and the five
vendors. It takes about 5 minutes: the build
(under a minute with cached layers) and about 4 minutes of smoke, most of
it waiting for the scheduler's tick (`periodic_refresh / 4`, 225 s by
default) that admits the first adoptions a client's check found no free
slot for. Ctrl-C stops it.

## What it checks

| Step | What it proves |
|---|---|
| `setup` | The vendors' keys (fetched from the vendors, for the clients and the daemon) and the daemon start; each client is an apt state directory with its own sources. |
| `first-contact` | Every client's first `apt-get update` through the cache is clean under e2e/flat's error grep (`apt_errors`: failures, timeouts, hash mismatches, and any signature error or `E:`/`Error:` line naming one of the five hosts). A control source — a Kubernetes minor that does not exist, answered 403 — must print an error the grep catches, and its "is not signed" line must match the smoke's signed-host filter; a second control, a Sublime-shaped (host-root) source at a suite that does not exist, answered 404, must be caught by that filter alone. |
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
  that answers 5xx or not at all (`Failed to fetch`). So is a strict
  control answered 403 or 404 instead of 502 while the daemon's log has
  INFO `package_coverage_incomplete` for its host: one of the vendor's
  snapshots lacks a SHA256 or a root `Packages`, which turns strict mode
  off for the whole host by design. Check the vendor directly (`curl
  -fsSIL https://pkgs.k8s.io/core:/stable:/v1.37/deb/InRelease`:
  pkgs.k8s.io redirects every path, so only `-L` makes a missing file
  fail); fix the smoke's values (*Maintenance*) or run it again later.
- **The cache:** anything else — an adoption that fails while upstream
  answers and the key is right, an update error a direct `apt update`
  does not show, a refusal of a package `Packages` declares, a panic. That
  is a finding: reproduce it in a Go test or in `e2e/flat`, fix it there,
  and never change the smoke to pass around it.

## Maintenance

The values to change sit at the top of `smoke.sh`:

- **Kubernetes minors** (`K8S_A`, `K8S_B`): any two pkgs.k8s.io
  publishes (it answers 403 for one it does not); move them when one is
  retired.
- **Forms** (`suite_expectations`): the form each suite adopts in
  (`inline`, or `detached` for pkg.jenkins.io); a vendor that adds or
  drops `InRelease` changes it, and `adoptions` then fails with "not
  every suite adopted in its form".
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
| 2026-10-27, 14:00 UTC (planning, phase H3, with the host-root control) | the same, and both controls caught by the error grep | build 17 s, smoke 238 s (adoptions 223 s) |

Each run adds its row here.
````

- [ ] **Step 6: The Makefile target, and `lib.sh`'s note**

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
# those five vendors, so it is run by hand before 1.1.0 and before later
# releases that change the request path, adoption or vouching, never in CI.
# E2E_LIVE_NAME names its image and container (default acu-e2e-live).
e2e-live:
	bash e2e/live/run.sh
```

Run: `make -n e2e-live`
Expected: `bash e2e/live/run.sh`.

`e2e/flat/lib.sh` (review V-M-6: the smoke runs by hand, so a change made for e2e/flat would otherwise break it unseen until release day):

`old`:

```bash
# lib.sh: helpers for e2e/flat's test.sh, which sources it. Everything runs
# inside the test container: nginx (the upstream), the daemon and the apt
# clients share it.
```

`new`:

```bash
# lib.sh: helpers for e2e/flat's test.sh, which sources it. Everything runs
# inside the test container: nginx (the upstream), the daemon and the apt
# clients share it.
#
# AIDEV-NOTE: e2e/live/smoke.sh, the manual live smoke, sources this file
# too. It uses the helpers scenario, passed, fail, show_logs, poll_quietly,
# log_mark, log_lines, log_has, event, kv, suite_kv, panics_in_log,
# start_daemon, stop_daemon, suite, client, capt, apt_update, apt_errors
# and candidate, the settings CONFIG, DAEMON_LOG, PROXY, ADMIN, CURL and
# APT_SIGNED_ERRORS, and the run state SCENARIO, FAILED, APT_RUNS, APT_LOG,
# TIMINGS and POLL_TOOK; it overrides say, NGINX_LOG and APT_SIGNED_HOSTS.
# It runs by hand, not in CI, so after changing any of these, lint it
# (shellcheck -x e2e/live/*.sh), and run it (make e2e-live) when a
# helper's behaviour changed. No line of this note may begin with the
# word shellcheck: the linter reads such a comment as a directive.
```

- [ ] **Step 7: Lint**

Run: `chmod +x e2e/live/run.sh e2e/live/smoke.sh && bash -n e2e/live/run.sh && bash -n e2e/live/smoke.sh && shellcheck -x e2e/live/*.sh && shellcheck -x e2e/flat/*.sh && echo clean`
Expected: `clean` (shellcheck prints nothing for either directory). `lib.sh`'s change is a comment, so e2e/flat needs no run beyond one image as a check (`E2E_FLAT_IMAGES=debian:12 make e2e-flat`: `[e2e-flat] OVERALL PASS (debian:12)`, about 2.5 minutes plus the build).

- [ ] **Step 8: Run the smoke**

Run: `make e2e-live 2>&1 | tee "$(mktemp)" | grep -E '^\[e2e-live'`
Expected (the planning run of these files, 2026-10-27 14:00 UTC; snapshot ids and `package_hash_count`s vary with the order of adoptions and what the vendors publish):

```
[e2e-live    0s] === scenario setup: keys, config, the daemon, the clients
[e2e-live    0s] apt 2.6.1; started 2026-10-27 14:00:41 UTC
[e2e-live    2s] starting the daemon
[e2e-live    3s] --- scenario setup passed (3s)
[e2e-live    3s] === scenario first-contact: every client's first apt-get update, clean
[e2e-live    3s] k8s-slash: first update clean
[e2e-live    4s] k8s-dot: first update clean
[e2e-live    4s] cuda: first update clean
[e2e-live    4s] nvidia: first update clean
[e2e-live    5s] jenkins: first update clean
[e2e-live    6s] sublime: first update clean
[e2e-live    6s] control: the error grep catches the missing minor's error
[e2e-live    6s] control-root: the signed-host filter catches a host-root source's error
[e2e-live    6s] --- scenario first-contact passed (3s)
[e2e-live    6s] === scenario adoptions: every suite adopts, in its upstream's form
[e2e-live  229s] all 8 suites adopted, 223s after first contact
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.36/deb: inline, snapshot 1, package_hash_count=84
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.36/deb/.: inline, snapshot 3, package_hash_count=84
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.37/deb: inline, snapshot 2, package_hash_count=36
[e2e-live  229s]   pkgs.k8s.io/core:/stable:/v1.37/deb/.: inline, snapshot 4, package_hash_count=36
[e2e-live  229s]   developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64: inline, snapshot 5, package_hash_count=9758
[e2e-live  229s]   nvidia.github.io/libnvidia-container/stable/deb/amd64: inline, snapshot 7, package_hash_count=170
[e2e-live  229s]   pkg.jenkins.io/debian-stable/binary: detached, snapshot 6, package_hash_count=180
[e2e-live  229s]   download.sublimetext.com/apt/stable: inline, snapshot 8, package_hash_count=78
[e2e-live  229s] --- scenario adoptions passed (223s)
[e2e-live  229s] === scenario updates: every client's apt-get update from the snapshots, clean
[e2e-live  229s] k8s-slash: update clean
[e2e-live  229s] k8s-dot: update clean
[e2e-live  229s] cuda: update clean
[e2e-live  229s] nvidia: update clean
[e2e-live  229s] jenkins: update clean
[e2e-live  229s] sublime: update clean
[e2e-live  229s] --- scenario updates passed (0s)
[e2e-live  229s] === scenario strict: strict mode refuses an undeclared .deb on every host
[e2e-live  229s] refused, as strict mode should: pkgs.k8s.io
[e2e-live  229s] refused, as strict mode should: developer.download.nvidia.com
[e2e-live  229s] refused, as strict mode should: nvidia.github.io
[e2e-live  229s] refused, as strict mode should: pkg.jenkins.io
[e2e-live  229s] refused, as strict mode should: download.sublimetext.com
[e2e-live  229s] --- scenario strict passed (0s)
[e2e-live  229s] === scenario installs: one package per repository, its .deb vouched
[e2e-live  230s] k8s-slash: installed kubectl=1.37.1-1.1, its .deb vouched (path=/core:/stable:/v1.37/deb/amd64/kubectl_1.37.1-1.1_amd64.deb)
[e2e-live  231s] k8s-dot: installed kubectl=1.36.5-1.1, its .deb vouched (path=/core:/stable:/v1.36/deb/amd64/kubectl_1.36.5-1.1_amd64.deb)
[e2e-live  232s] cuda: installed cuda-toolkit-12-config-common=12.9.79-1, its .deb vouched (path=/compute/cuda/repos/debian12/x86_64/./cuda-toolkit-12-config-common_12.9.79-1_all.deb)
[e2e-live  233s] nvidia: installed nvidia-container-toolkit-base=1.20.1-1, its .deb vouched (path=/libnvidia-container/stable/deb/amd64/./nvidia-container-toolkit-base_1.20.1-1_amd64.deb)
[e2e-live  235s] jenkins: installed jenkins=2.580.1, its .deb vouched (path=/debian-stable/binary/jenkins_2.580.1_all.deb)
[e2e-live  237s] sublime: installed sublime-merge=2132, its .deb vouched (path=/files/sublime-merge_build-2132_amd64.deb)
[e2e-live  237s] --- scenario installs passed (8s)
[e2e-live  237s] === scenario whole-run: no refusal but the controls', no panic
[e2e-live  237s] stopping the daemon (the end)
[e2e-live  238s] --- scenario whole-run passed (1s)
[e2e-live  238s] timings: setup 3s first-contact 3s adoptions 223s updates 0s strict 0s installs 8s whole-run 1s
[e2e-live  238s] PASS: live smoke, finished 2026-10-27 14:04:39 UTC
[e2e-live] PASS (build 17s, smoke 238s)
```

The first draft's planning runs (2026-10-26, 19:00 and 19:21 UTC, without the host-root control) passed the same way: adoptions 221 and 224 s, smoke 245 and 240 s, build 51 and 20 s. A failure here is triaged as the README's *A failure: the cache, or the vendor?* says: a vendor's change is fixed in the smoke's values; anything the cache does is a finding for the controller, never worked around (ruling R-H2-4).

- [ ] **Step 9: The strict step has teeth**

Mutant (scratch only, revert after): in `smoke.sh`'s `write_config`, `refuse_unvouched_debs = true` → `false`. Run Step 8's command.
Expected: FAIL in the `strict` scenario, at its first control: `[e2e-live  228s] FAIL in scenario 'strict': http://HTTPS///pkgs.k8s.io/core:/stable:/v1.37/deb/amd64/acu-live-control_1.0_amd64.deb answered 403, want 502 (strict mode off for its host?)` (planning runs: smoke 230 s on 2026-10-26 and 228 s on 2026-10-27), followed by the daemon log's tail. Restore the line; `git diff --no-ext-diff e2e/live/smoke.sh` prints nothing once it is committed (Step 10).

- [ ] **Step 10: Commit**

```bash
git add e2e/live/Dockerfile e2e/live/run.sh e2e/live/smoke.sh e2e/live/README.md Makefile e2e/flat/lib.sh
git commit -F - <<'EOF'
e2e/live: a manual smoke against live flat repositories

The live-upstream smoke the overview's PR #8 parity section asks of
phase H: a release-built daemon, with adoption and strict mode on, and
real apt 2.6 clients in one debian:12 container, against pkgs.k8s.io
(v1.36 and v1.37, in both source forms), NVIDIA CUDA (debian12),
libnvidia-container, pkg.jenkins.io and download.sublimetext.com. Every
suite adopts in its upstream's form (adoption_success), every update is
clean under e2e/flat's error grep (with two controls that must fail it,
one for each form of its signed-host filter),
one package per repository installs with its .deb vouched
(validated_hash=true), strict mode refuses an undeclared .deb on each of
the five hosts (the controls), and nothing else is refused. It sources
e2e/flat's lib.sh (whose header now says so: an AIDEV-NOTE naming the
helpers and settings the smoke uses), fetches the vendors' keys at run
time and is run by hand before 1.1.0 and before later releases that
change the request path, adoption or vouching, never in CI. make
e2e-live; e2e/live/README.md.

Planning runs, 2026-10-26 (two) and 2026-10-27 (with both controls):
all passed, all 8 suites adopted in 221 to 224 s (the scheduler's
tick), smoke 238 to 245 s. With refuse_unvouched_debs off the strict
step fails at its first control.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 4: The package upgrade restarts a running daemon

The 2026-09-08 security review's finding H-2 (review C-I4), which the maintainer decided to fix in 1.1.0 (2026-10-27; ruling PH3-8): `packaging/scripts/postinstall.sh` only reloads systemd, so after `apt-get upgrade` the old binary keeps running — `dpkg -l` says the new version — until a reboot or a crash, and the upgrade's migration, trust audit and reaper start at a moment the operator did not choose. `preremove.sh`'s comment says the new package's postinst restarts the service; it does not. This task makes the postinst do what `dh_installsystemd --no-start --restart-after-upgrade` generates (nfpm has no debhelper): on an upgrade, and on dpkg's `abort-*` actions, `deb-systemd-invoke try-restart` — a running daemon moves to the new binary, a stopped one stays stopped, a fresh install starts nothing — guarded by `/run/systemd/system`, never failing the package operation. The install message prints on a fresh install only. It comes before the documentation tasks, which describe it (Tasks 6, 8 and 9).

Design (planning research, 2026-10-27, on `ubuntu:24.04` and `ubuntu:26.04`, e2e/deb's images):

- **`deb-systemd-invoke`, not `systemctl`.** `deb-systemd-invoke try-restart` first asks `/usr/sbin/policy-rc.d` (exit 101 forbids service actions — every Docker image of Ubuntu and Debian ships one — and container builds and chroots rely on it), then `exec`s `systemctl try-restart` as it is: only `start` and `restart` get its is-enabled check, so a running unit is restarted whether or not it is enabled, which suits a package that never enables its unit. It is what debhelper's snippet (`postinst-systemd-restartnostart`, Step 1) calls.
- **No new dependency.** `deb-systemd-invoke` ships in `init-system-helpers`, `Essential: yes` on Ubuntu 22.04 to 26.04 and Debian 11 to 13 (versions 1.60 to 1.69, all six images probed), and current `dh_installsystemd` adds no `init-system-helpers` dependency either.
- **Upgrading from 1.0.x.** dpkg runs the old package's prerm (`upgrade`), the new preinst, the old postrm (`upgrade`), then the new postinst (`configure <old version>`). 1.0.2's maintainer scripts are this tree's (Step 1's diff is empty): its prerm stops nothing on upgrade and its postrm only reloads systemd, so the daemon runs the old binary until the new postinst's try-restart. Step 7 runs that path with 1.0.2's own package.
- **The proof without systemd.** e2e/deb runs no systemd (its header says why), so its new phase 5 makes the scripts see one: `/run/systemd/system` exists, and a `systemctl` stub first on `PATH` records each call. dpkg passes its `PATH` on to the maintainer scripts, and `deb-systemd-invoke` finds `systemctl` on it, so the stub sees what systemd would be asked (planning: it does, on both images). The image's `policy-rc.d` is moved aside except for the step that checks it is honoured.
- **A reinstall after a remove (not a purge)** also passes the removed version as `$2` (dpkg's rule; planning: it does), as for debhelper's packages: nothing runs to restart, and no install message prints, though the remove disabled the unit. Recorded in the postinst's `AIDEV-NOTE`.

**Files:**
- Modify: `packaging/scripts/postinstall.sh` (rewritten), `packaging/scripts/preremove.sh` (its comment), `e2e/deb/Dockerfile` (a second package, at a higher version, for the upgrade), `e2e/deb/test.sh` (phase 5)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: e2e/deb's phase 5 and its knob `UPGRADE_FROM` (the package to upgrade from, default this tree's), the Dockerfile's `ARG UPGRADE_VERSION` (default `0.0.1-e2e`). Task 6 (`docs/configuration.md`, *Upgrading an existing cache*), Task 8 (the release notes' *Before you upgrade*) and Task 9 (`docs/open-decisions.md`: H-2 decided and done) describe it.

- [ ] **Step 1: Pre-flight: the facts the design rests on**

Run (from the repository root):

```bash
git diff --no-ext-diff --stat 1.0.2 -- packaging/scripts/
for v in 24.04 26.04; do
    docker run --rm "ubuntu:$v" sh -c 'dpkg-query -W -f="\${Package} \${Version} Essential: \${Essential}\n" init-system-helpers; tail -n 1 /usr/sbin/policy-rc.d; grep -c "returned 101, not running" /usr/bin/deb-systemd-invoke; grep -n "exec(.systemctl., @ARGV);" /usr/bin/deb-systemd-invoke'
done
docker run --rm ubuntu:26.04 sh -c 'apt-get -qq update && cd /tmp && apt-get -qq download libdebhelper-perl && dpkg-deb -x libdebhelper-perl_*.deb dh && cat dh/usr/share/debhelper/autoscripts/postinst-systemd-restartnostart'
```

Expected (planning, 2026-10-27): nothing from the diff (the maintainer scripts are 1.0.2's); for each image, `init-system-helpers` with `Essential: yes`, the image's `policy-rc.d` ending `exit 101`, `deb-systemd-invoke`'s policy refusal (`1`) and its plain `exec` of `systemctl` for every action but `start`, `restart` and the reloads:

```
init-system-helpers 1.66ubuntu1 Essential: yes
exit 101
1
186:    exec('systemctl', @ARGV);
init-system-helpers 1.69 Essential: yes
exit 101
1
186:    exec('systemctl', @ARGV);
if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ] ; then
	if [ -z "$DPKG_ROOT" ] && [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null || true
		if [ -n "$2" ]; then
			deb-systemd-invoke #RESTART_ACTION# #UNITFILES# >/dev/null || true
		fi
	fi
fi
```

The last block is debhelper's snippet, which `dh_installsystemd --no-start --restart-after-upgrade` fills with `try-restart` and the unit (`dh_installsystemd`: `$snippet = 'postinst-systemd-restartnostart'; $replace->{RESTART_ACTION} = 'try-restart'`); Step 4 writes it out. A different version or line is fine; a missing `Essential: yes` or a `deb-systemd-invoke` that checks is-enabled for `try-restart` is a stop.

- [ ] **Step 2: Write the test first: e2e/deb's second package and phase 5**

In `e2e/deb/Dockerfile`, the header:

`old`:

```dockerfile
#   3. `test`      — install on ubuntu:${UBUNTU_VERSION}, validate the
#                    package contract, run the daemon to bind :3142,
#                    THEN restart it under an adoption-mode config,
#                    drive a freshness fetch through the proxy, and
#                    assert a snapshot was adopted.
```

`new`:

```dockerfile
#   3. `test`      — install on ubuntu:${UBUNTU_VERSION}, validate the
#                    package contract, run the daemon to bind :3142,
#                    THEN restart it under an adoption-mode config,
#                    drive a freshness fetch through the proxy, and
#                    assert a snapshot was adopted; then (phase 5)
#                    purge, install and upgrade the package under a
#                    stub systemctl.
```

the second package, after the first:

`old`:

```dockerfile
    --target /out/apt-cacher-ultra_${VERSION}_amd64.deb

```

`new`:

```dockerfile
    --target /out/apt-cacher-ultra_${VERSION}_amd64.deb
# The same binary packaged at a higher version, for test.sh's phase 5:
# `dpkg -i` of it over the package above is an upgrade (the maintainer
# scripts' upgrade path). Under /out/upgrade/, so the test stage's
# /out/*.deb copy still finds exactly one package for /tmp.
ARG UPGRADE_VERSION=0.0.1-e2e
RUN mkdir -p /out/upgrade && VERSION=${UPGRADE_VERSION} nfpm pkg \
    --packager deb \
    --config packaging/nfpm.yaml \
    --target /out/upgrade/apt-cacher-ultra_${UPGRADE_VERSION}_amd64.deb

```

and its copy into the test stage:

`old`:

```dockerfile
COPY --from=build /out/*.deb /tmp/
```

`new`:

```dockerfile
COPY --from=build /out/*.deb /tmp/
COPY --from=build /out/upgrade/*.deb /upgrade/
```

In `e2e/deb/test.sh`, the header:

`old`:

```bash
# NoNewPrivileges, etc.) are NOT exercised. That coverage is the
# job of step 4 (production deployment) per SPEC §14.
```

`new`:

```bash
# NoNewPrivileges, etc.) are NOT exercised. That coverage is the
# job of step 4 (production deployment) per SPEC §14.
#
# Phase 5 checks the maintainer scripts' calls to systemd on a fresh
# install, an upgrade and a removal, against a stub systemctl (its
# AIDEV-NOTE says how).
```

and phase 5, after phase 4:

`old`:

```bash
echo "[deb-test] phase 4 (Phase 3 hot-prefetch) PASS"
echo "[deb-test] OVERALL PASS"
```

`new`:

```bash
echo "[deb-test] phase 4 (Phase 3 hot-prefetch) PASS"

# ============================================================
# Phase 5: the maintainer scripts under systemd (the 2026-09 security
# review's H-2). A fresh install starts nothing and prints the install
# message; an upgrade asks systemd for exactly one try-restart (a running
# daemon moves to the new binary, a stopped one stays stopped), stops
# nothing first and prints no install message; a failed restart does not
# fail dpkg; policy-rc.d's "no" is honoured; a removal stops and disables
# the unit.
#
# AIDEV-NOTE: no systemd runs here (see the header), so this phase makes
# the scripts see one: /run/systemd/system exists, and a systemctl stub
# first on PATH records each call (its arguments, one line) and succeeds,
# or fails a try-restart while $stubs/fail-try-restart exists. dpkg passes
# its PATH on to the maintainer scripts, and deb-systemd-invoke looks
# systemctl up on it, so the stub sees what systemd would be asked. The
# image's /usr/sbin/policy-rc.d (exit 101: Docker's images forbid service
# actions) is moved aside, except while the policy step checks that it is
# honoured. UPGRADE_FROM names the package to upgrade from (by default
# this tree's, the /tmp one phase 1 installed); the upgrade goes to
# /upgrade's, the same binary packaged at a higher version (the
# Dockerfile's build stage).
# ============================================================

echo
echo "[deb-test] phase 5: stop phase-4 daemon"
stop_daemon

shopt -s nullglob
upgrade_debs=(/upgrade/apt-cacher-ultra_*.deb)
shopt -u nullglob
if [[ ${#upgrade_debs[@]} -ne 1 ]]; then
    echo "FAIL: expected 1 .deb in /upgrade, got ${#upgrade_debs[@]}: ${upgrade_debs[*]:-<none>}"
    exit 1
fi
new_deb=${upgrade_debs[0]}
old_deb=${UPGRADE_FROM:-${debs[0]}}
new_version=$(dpkg-deb -f "$new_deb" Version)
old_version=$(dpkg-deb -f "$old_deb" Version)
if dpkg --compare-versions "$new_version" le "$old_version"; then
    echo "FAIL: phase 5 would upgrade from $old_version to $new_version, which is not newer"
    exit 1
fi

stubs=/acu-stubs
calls=$stubs/calls
mkdir -p "$stubs" /run/systemd/system
cat > "$stubs/systemctl" <<'EOF'
#!/bin/sh
echo "$*" >> /acu-stubs/calls
if [ "$1" = try-restart ] && [ -e /acu-stubs/fail-try-restart ]; then
    echo "systemctl stub: try-restart fails" >&2
    exit 1
fi
exit 0
EOF
chmod 0755 "$stubs/systemctl"
mv /usr/sbin/policy-rc.d /usr/sbin/policy-rc.d.acu

# dpkg_step NAME ARGS...: runs `dpkg ARGS` with the stub first on PATH and
# an empty call record; its output goes to /tmp/p5.NAME.out, and a dpkg
# failure fails the test with that output.
dpkg_step() {
    local name=$1
    shift
    : > "$calls"
    if ! PATH="$stubs:$PATH" dpkg "$@" > "/tmp/p5.$name.out" 2>&1; then
        echo "FAIL: phase-5 $name: dpkg $* failed:"
        cat "/tmp/p5.$name.out"
        exit 1
    fi
}

# expect_count NAME WHAT WANT FILE PATTERN: FILE has WANT lines matching
# the extended regex PATTERN, or the test fails naming WHAT and showing
# the step's dpkg output and systemctl calls.
expect_count() {
    local name=$1 what=$2 want=$3 file=$4 pattern=$5 got
    got=$(grep -cE -- "$pattern" "$file" || true)
    if [[ "$got" -ne "$want" ]]; then
        echo "FAIL: phase-5 $name: $got $what, want $want"
        echo "=== dpkg output ==="
        cat "/tmp/p5.$name.out"
        echo "=== systemctl calls ==="
        cat "$calls"
        exit 1
    fi
}

# Any call that starts or restarts a unit, after systemctl's options.
restarts='^(--[a-z-]+ )*(start|restart|try-restart|reload-or-restart|try-reload-or-restart) '
message='^apt-cacher-ultra installed\.$'

echo "[deb-test] phase 5: purge the phase-1 install"
dpkg_step purge --purge apt-cacher-ultra

echo "[deb-test] phase 5: fresh install of $old_version: no restart, the install message"
dpkg_step install -i "$old_deb"
expect_count install "start or restart calls" 0 "$calls" "$restarts"
expect_count install "install messages" 1 /tmp/p5.install.out "$message"

echo "[deb-test] phase 5: upgrade to $new_version: one try-restart, no stop, no install message"
dpkg_step upgrade -i "$new_deb"
expect_count upgrade "try-restart calls" 1 "$calls" '^try-restart apt-cacher-ultra\.service$'
expect_count upgrade "start or restart calls" 1 "$calls" "$restarts"
expect_count upgrade "stop calls" 0 "$calls" '^(--[a-z-]+ )*stop '
expect_count upgrade "install messages" 0 /tmp/p5.upgrade.out "$message"
installed=$(dpkg-query -W -f='${Version}' apt-cacher-ultra)
if [[ "$installed" != "$new_version" ]]; then
    echo "FAIL: phase-5 upgrade: installed version is $installed, want $new_version"
    exit 1
fi

echo "[deb-test] phase 5: a failed try-restart does not fail dpkg"
touch "$stubs/fail-try-restart"
dpkg_step failed-restart -i "$new_deb"
rm "$stubs/fail-try-restart"
expect_count failed-restart "try-restart calls" 1 "$calls" '^try-restart apt-cacher-ultra\.service$'
status=$(dpkg-query -W -f='${Status}' apt-cacher-ultra)
if [[ "$status" != "install ok installed" ]]; then
    echo "FAIL: phase-5 failed-restart: package status is '$status', want 'install ok installed'"
    exit 1
fi

echo "[deb-test] phase 5: policy-rc.d's 101 is honoured"
mv /usr/sbin/policy-rc.d.acu /usr/sbin/policy-rc.d
dpkg_step policy -i "$new_deb"
mv /usr/sbin/policy-rc.d /usr/sbin/policy-rc.d.acu
expect_count policy "start or restart calls" 0 "$calls" "$restarts"
expect_count policy "policy-rc.d refusals" 1 /tmp/p5.policy.out 'policy-rc\.d returned 101'

echo "[deb-test] phase 5: remove: stop and disable"
dpkg_step remove -r apt-cacher-ultra
expect_count remove "stop calls" 1 "$calls" '^stop apt-cacher-ultra\.service$'
expect_count remove "disable calls" 1 "$calls" '^disable apt-cacher-ultra\.service$'

echo "[deb-test] phase 5 (maintainer scripts under systemd) PASS"
echo "[deb-test] OVERALL PASS"
```

What each check pins: the fresh install (from a purge) calls no `start`/`restart`/`try-restart` and prints the install message once; the upgrade calls exactly one `try-restart apt-cacher-ultra.service`, no other start or restart and no `stop`, prints no install message, and leaves the new version installed; a `try-restart` that fails leaves dpkg's exit status 0 and the package `install ok installed`; with the image's `policy-rc.d` (exit 101) in place, no restart reaches `systemctl` and `deb-systemd-invoke`'s refusal is printed; a removal calls `stop` and `disable` once each (`preremove.sh`, unchanged). The `restarts` pattern allows `systemctl`'s leading options, so a `deb-systemd-invoke restart` (which calls `systemctl --quiet restart …`) would count too.

- [ ] **Step 3: Run it to see it fail (the current postinst restarts nothing)**

Run: `make deb-test 2>&1 | tee "$(mktemp)" | grep -E '^\[deb-test\] (phase 5|ubuntu|OVERALL)|^FAIL'`
Expected, for each image (planning, 2026-10-27: about 1 to 1.5 minutes an image with cached layers):

```
[deb-test] ubuntu:24.04
[deb-test] phase 5: stop phase-4 daemon
[deb-test] phase 5: purge the phase-1 install
[deb-test] phase 5: fresh install of 0.0.0~e2e: no restart, the install message
[deb-test] phase 5: upgrade to 0.0.1~e2e: one try-restart, no stop, no install message
FAIL: phase-5 upgrade: 0 try-restart calls, want 1
[deb-test] ubuntu:24.04 FAIL (test)
```

(and the same for `ubuntu:26.04`), then `[deb-test] OVERALL FAIL: 24.04:test 26.04:test`. The failure prints the step's dpkg output — with today's install message after `Setting up apt-cacher-ultra (0.0.1~e2e)`, which the upgrade will no longer print — and the stub's calls: `daemon-reload` twice (the old package's postrm and the new postinst), nothing else. nfpm turns the semantic versions `0.0.0-e2e` and `0.0.1-e2e` into the Debian versions `0.0.0~e2e` and `0.0.1~e2e`.

- [ ] **Step 4: The postinst, and preremove's comment**

Write `packaging/scripts/postinstall.sh`:

```sh
#!/bin/sh
set -e

mkdir -p /var/cache/apt-cacher-ultra
chown apt-cacher-ultra:apt-cacher-ultra /var/cache/apt-cacher-ultra
chmod 0750 /var/cache/apt-cacher-ultra

# AIDEV-NOTE: what `dh_installsystemd --no-start --restart-after-upgrade`
# generates (debhelper's postinst-systemd-restartnostart snippet), written
# out because nfpm has no debhelper. On an upgrade (configure with $2, the
# version configured before) and on the abort-* actions, a running daemon
# is restarted onto this package's binary; a stopped one stays stopped
# (try-restart), and a fresh install starts nothing (the operator enables
# the unit). The old package's prerm stops nothing on upgrade
# (preremove.sh), so the daemon runs until this restart. deb-systemd-invoke
# rather than systemctl: it honours /usr/sbin/policy-rc.d (exit 101 forbids
# service actions, as in container builds and chroots), then passes
# try-restart to systemctl as it is, so a running unit is restarted whether
# or not it is enabled. It ships in init-system-helpers, Essential: yes on
# every supported release (Ubuntu 22.04-26.04, Debian 11-13), so the
# package needs no dependency for it. A failed restart never fails the
# package operation: systemctl's error is printed, journalctl -u
# apt-cacher-ultra says why, and the unit's Restart=always keeps trying.
# A reinstall after a remove (not a purge) also passes $2, as with
# debhelper: nothing is running to restart, and no message is printed,
# though the remove disabled the unit.
if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ]; then
    if [ -z "${DPKG_ROOT:-}" ] && [ -d /run/systemd/system ]; then
        systemctl --system daemon-reload >/dev/null 2>&1 || :
        if [ -n "$2" ]; then
            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
        fi
    fi
fi

if [ "$1" = "configure" ] && [ -z "$2" ]; then
    cat <<'EOF'
apt-cacher-ultra installed.
Edit /etc/apt-cacher-ultra/config.toml, then:
  sudo systemctl enable --now apt-cacher-ultra
EOF
fi
```

In `packaging/scripts/preremove.sh`:

`old`:

```sh
# Debian prerm is invoked with $1 in {remove, upgrade, deconfigure,
# failed-upgrade}. Only stop and disable on outright remove; on upgrade,
# the new package's postinst will restart the service, and disabling here
# would leave it disabled afterwards.
```

`new`:

```sh
# Debian prerm is invoked with $1 in {remove, upgrade, deconfigure,
# failed-upgrade}. Only stop and disable on outright remove. On upgrade
# this (the old package's) prerm stops nothing, so the daemon keeps
# running the old binary until the new package's postinst restarts it
# (try-restart: only if it is running); disabling here would leave it
# disabled afterwards.
```

- [ ] **Step 5: Run it to see it pass**

Run: `make deb-test 2>&1 | tee "$(mktemp)" | grep -E '^\[deb-test\] (phase 5|ubuntu|OVERALL)|^FAIL'`
Expected, for each image:

```
[deb-test] ubuntu:24.04
[deb-test] phase 5: stop phase-4 daemon
[deb-test] phase 5: purge the phase-1 install
[deb-test] phase 5: fresh install of 0.0.0~e2e: no restart, the install message
[deb-test] phase 5: upgrade to 0.0.1~e2e: one try-restart, no stop, no install message
[deb-test] phase 5: a failed try-restart does not fail dpkg
[deb-test] phase 5: policy-rc.d's 101 is honoured
[deb-test] phase 5: remove: stop and disable
[deb-test] phase 5 (maintainer scripts under systemd) PASS
[deb-test] OVERALL PASS
[deb-test] ubuntu:24.04 PASS
```

(and `ubuntu:26.04`), then `[deb-test] OVERALL PASS (24.04 26.04)` (the first `OVERALL PASS` is the image's own test, the last `run.sh`'s). Phase 5 takes a few seconds; phases 1–4 are unchanged.

- [ ] **Step 6: Its mutants**

Each mutant (one at a time, reverted after) fails phase 5 with Step 3's command, at the line shown (the test prints the step's dpkg output and calls after it):

| Mutant of `postinstall.sh` | Fails with |
|---|---|
| the restart line replaced by `:` (deleting it leaves an empty `then`, a syntax error dpkg reports at the first install) | `FAIL: phase-5 upgrade: 0 try-restart calls, want 1` |
| `systemctl try-restart` in place of `deb-systemd-invoke try-restart` | `FAIL: phase-5 policy: 1 start or restart calls, want 0` |
| the install message unconditional (its `if` reduced to `if [ "$1" = "configure" ]; then`) | `FAIL: phase-5 upgrade: 1 install messages, want 0` |

The first is the ruling's red, run on both images (planning: both failed as shown); the other two need only `UBUNTU_VERSIONS=24.04` (planning: as shown). Restore the file; `git diff --no-ext-diff packaging/` then shows only Step 4's change.

- [ ] **Step 7: The upgrade from 1.0.2, with its own package**

Build 1.0.2's package from its tag with its own e2e/deb build stage (the release's nfpm config and maintainer scripts), and run phase 5 from it to this tree's package, packaged at `1.1.0-e2e` so that it is newer (run from the repository root):

```bash
S=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-deb
rm -rf "$S"; mkdir -p "$S/1.0.2"
git archive -o "$S/1.0.2.tar" 1.0.2
tar -x -C "$S/1.0.2" -f "$S/1.0.2.tar"
docker build -q -f "$S/1.0.2/e2e/deb/Dockerfile" --build-arg VERSION=1.0.2 --target build -t acu-h3-deb-build:1.0.2 "$S/1.0.2" > /dev/null
docker create --name acu-h3-deb-1.0.2 acu-h3-deb-build:1.0.2 > /dev/null
docker cp acu-h3-deb-1.0.2:/out/apt-cacher-ultra_1.0.2_amd64.deb "$S/"
docker rm acu-h3-deb-1.0.2 > /dev/null
docker build -q -f e2e/deb/Dockerfile --build-arg UBUNTU_VERSION=24.04 --build-arg UPGRADE_VERSION=1.1.0-e2e --target test -t acu-h3-deb-test:from-1.0.2 . > /dev/null
docker run --rm -v "$S/apt-cacher-ultra_1.0.2_amd64.deb:/old.deb:ro" -e UPGRADE_FROM=/old.deb acu-h3-deb-test:from-1.0.2 2>&1 | grep -E '^\[deb-test\] (phase 5|OVERALL)|^FAIL'
```

Expected:

```
[deb-test] phase 5: stop phase-4 daemon
[deb-test] phase 5: purge the phase-1 install
[deb-test] phase 5: fresh install of 1.0.2: no restart, the install message
[deb-test] phase 5: upgrade to 1.1.0~e2e: one try-restart, no stop, no install message
[deb-test] phase 5: a failed try-restart does not fail dpkg
[deb-test] phase 5: policy-rc.d's 101 is honoured
[deb-test] phase 5: remove: stop and disable
[deb-test] phase 5 (maintainer scripts under systemd) PASS
[deb-test] OVERALL PASS
```

1.0.2's postinst runs on the fresh install (no restart; its message, which it prints always), then the upgrade's old-package scripts are 1.0.2's and the new postinst's try-restart is the one call. Remove the two images afterwards (`docker rmi acu-h3-deb-build:1.0.2 acu-h3-deb-test:from-1.0.2`).

- [ ] **Step 8: Lint**

Run: `shellcheck -x e2e/deb/*.sh packaging/scripts/*.sh && echo clean`
Expected: `clean`.

- [ ] **Step 9: Commit**

```bash
git add packaging/scripts/postinstall.sh packaging/scripts/preremove.sh e2e/deb/Dockerfile e2e/deb/test.sh
git commit -F - <<'EOF'
Package: an upgrade restarts a running daemon

postinstall.sh only reloaded systemd, so after apt-get upgrade the old
binary kept running until the next reboot or crash, and the upgrade's
migration, audit and reaper began then (the 2026-09-08 security
review's H-2; the maintainer's decision to fix it in 1.1.0). The
postinst now does what dh_installsystemd --no-start
--restart-after-upgrade generates: on an upgrade, and on dpkg's abort-*
actions, deb-systemd-invoke try-restart, guarded by /run/systemd/system
and never failing the package operation; a stopped daemon stays
stopped, and a fresh install starts nothing. deb-systemd-invoke honours
policy-rc.d and passes try-restart to systemctl as it is; it ships in
init-system-helpers, Essential on every supported release, so the
package needs no new dependency. The install message prints on a fresh
install only. preremove.sh's comment said the new postinst restarts the
service; it now says what happens.

e2e/deb's phase 5 proves it against a stub systemctl first on PATH,
with /run/systemd/system present: a fresh install asks for no restart
and prints the message, an upgrade asks for exactly one try-restart and
nothing else, a failing restart leaves dpkg green, policy-rc.d's 101 is
honoured, and a removal stops and disables. Red without the restart
line on ubuntu:24.04 and 26.04, green after; the upgrade from 1.0.2's
own package passes the same phase.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 5: The Go 1.27 toolchain

**Required: the maintainer decided D2 on 2026-10-26/27 — Go 1.27** (H2's For H3 item 11; design note 5; ruling PH3-10). It runs before the documentation tasks, which state it as done: the release notes (Task 8) and `docs/open-decisions.md`, where D2 is under *Decided* (Task 9). It builds with Go 1.27 everywhere a build names a Go version — CI's `test` and `lint` jobs, the release workflow and the four e2e Dockerfiles (`e2e/flat`, `e2e/deb`, `e2e/live` from Task 3, and `e2e/Dockerfile.cache`, which the brief did not name) — and keeps `go.mod`'s `go 1.25.0`: the language version and the GODEBUG defaults stay those the code was tested with, while the toolchain, and so the standard library a release ships, gets Go 1.27's fixes. It also moves CI's golangci-lint pin from v2.12.2 to v2.14.0: planning found that v2.12.2, run with Go 1.27, fails every package's type check (`could not load export data: internal error in importing "internal/goarch" (cannot decode "internal/goarch", export data version 5 is greater than maximum supported version 4)`), while v2.14.0 reports `0 issues.` (Go 1.26 would keep v2.12.2: it reports `0 issues.` with go1.26.10.)

Why 1.27 rather than 1.26 (the recommendation the maintainer took): Go supports a release until two newer ones exist, so 1.26 loses fixes when 1.28 ships (about February 2027) and 1.27 when 1.29 does (about August 2027); 1.1.x maintenance releases built with 1.27 stay supported six months longer, for one more line of CI change. The maintainer's local toolchain is go1.26.8, which ran every H1–H3 gate; with this task CI tests on 1.27, and Task 10's gate runs the race suite and the `e2ehooks` tests under `golang:1.27-bookworm` too (ruling PH3-7). The local golangci-lint is already v2.14.0 (built with go1.27.2), so the lint bump also makes CI lint with the version every local gate used (review V-M-4).

**Files:**
- Modify: `.github/workflows/ci.yaml` (two `go-version`, the golangci-lint `version` and its comment), `.github/workflows/release.yaml` (`go-version`), `e2e/flat/Dockerfile`, `e2e/deb/Dockerfile`, `e2e/live/Dockerfile`, `e2e/Dockerfile.cache` (the `FROM golang:…` line and a comment), `e2e/flat/README.md` (its list of Docker Hub inputs)

**Interfaces:**
- Consumes: `e2e/live/Dockerfile` (Task 3) and `e2e/deb/Dockerfile` as Task 4 left it (its `FROM` line two lines lower).
- Produces: Go 1.27 for every build: CI, the release, and the e2e images Task 10's gate builds. Task 8 (the notes) and Task 9 (`docs/open-decisions.md`) record it.

- [ ] **Step 1: Pre-flight: the toolchain's facts today**

Run:

```bash
go version
curl -fsS --max-time 30 'https://go.dev/dl/?mode=json' | python3 -c 'import json, sys; print(" ".join(r["version"] for r in json.load(sys.stdin)))'
docker manifest inspect golang:1.27-bookworm > /dev/null && echo "golang:1.27-bookworm exists"
docker manifest inspect golang:1.27-alpine > /dev/null && echo "golang:1.27-alpine exists"
grep -rn "go-version: '1.25'\|FROM golang:1.25\|golang:1.25-\|version: v2.12.2" .github e2e --include='*.yaml' --include='Dockerfile*' --include='*.md'
```

Expected (planning run, 2026-10-27, Tasks 1–4 applied): `go version go1.26.8 linux/amd64`; `go1.27.3 go1.26.10`; both images exist; and the ten lines this task changes (in any order):

```
.github/workflows/ci.yaml:19:          go-version: '1.25'
.github/workflows/ci.yaml:59:          go-version: '1.25'
.github/workflows/ci.yaml:66:          version: v2.12.2
.github/workflows/release.yaml:41:          go-version: '1.25'
e2e/flat/Dockerfile:21:FROM golang:1.25-bookworm AS build
e2e/flat/README.md:39:- Docker Hub (`golang:1.25-bookworm`, `debian:bookworm-slim`, the client
e2e/deb/Dockerfile:41:FROM golang:1.25-bookworm AS build
e2e/live/Dockerfile:20:FROM golang:1.25-bookworm AS build
e2e/Dockerfile.cache:2:# binary from the current source tree in a golang:1.25-alpine stage and
e2e/Dockerfile.cache:7:FROM golang:1.25-alpine AS build
```

A newer 1.27 patch release is fine (the tags float); a 1.28 on go.dev's list means the decision is due again with 1.28 in mind — stop and ask.

- [ ] **Step 2: Apply the edits**

Run (from the repository root):

```bash
python3 - <<'EOF'
#!/usr/bin/env python3
"""H3 Task 5 (the maintainer's decision D2): build with Go 1.27 everywhere,
go.mod's `go 1.25.0` unchanged. Run from the repository root; each old
text must occur exactly as often as stated. Every edit is checked before
any file is written, so a mismatch leaves the tree as it was."""
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
files = {}
counts = []
for rel, old, new, want in EDITS:
    s = files.get(rel) or pathlib.Path(rel).read_text()
    n = s.count(old)
    if n != want:
        sys.exit(f"{rel}: {old.strip()!r} occurs {n} times, want {want}; nothing written")
    files[rel] = s.replace(old, new)
    counts.append(f"{rel}: {n}")
for rel, s in files.items():
    pathlib.Path(rel).write_text(s)
print("\n".join(counts))
EOF
```

Expected: one line per edit, printed after every file is written — `.github/workflows/ci.yaml: 2`, `.github/workflows/release.yaml: 1`, `e2e/flat/Dockerfile: 1`, `e2e/deb/Dockerfile: 1`, `e2e/live/Dockerfile: 1`, `e2e/Dockerfile.cache: 1` (twice), `e2e/flat/README.md: 1`, `.github/workflows/ci.yaml: 1`; an edit that does not match stops the script before it writes anything (review: collect, then write). Then `grep -rn "go-version: '1.25'\|FROM golang:1.25\|golang:1.25-\|version: v2.12.2" .github e2e --include='*.yaml' --include='Dockerfile*' --include='*.md'` prints nothing, and `head -3 go.mod` still shows `go 1.25.0`.

- [ ] **Step 3: The module under Go 1.27**

Run (the module mounted into the Go 1.27 image; the test cache stays in the container; `GOFLAGS=-buildvcs=false` because the container's root does not own the bind-mounted checkout, so git refuses it and `go build`'s VCS stamping would stop with `error obtaining VCS status: exit status 128` in a checkout whose `.git` is a directory — review E-M-10):

```bash
docker run --rm -e GOFLAGS=-buildvcs=false -v "$(pwd)":/src -w /src golang:1.27-bookworm bash -c 'go version && go vet ./... && CGO_ENABLED=0 go build -trimpath -o /tmp/acu ./cmd/apt-cacher-ultra && go test -race -timeout 10m ./... && go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra/'
```

Expected: `go version go1.27.3 linux/amd64` (or a later 1.27), then every package `ok` (21 packages) and the tagged package `ok`, exit status 0 (planning, 2026-10-27, Tasks 1–4 applied: go1.27.3; 21 packages `ok` and the tagged package `ok`, 377 s in all, `internal/freshness` the slowest at 268 s (the exec review measured 468 s under load)).

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

Expected: actionlint prints only the finding that predates this branch's H3 work (`.github/workflows/release.yaml:126:9: shellcheck reported issue in this script: SC2035:info:21:23: …`, in `docs/open-decisions.md`) and exits 1 for it; and four `build stage OK` lines (planning: 68, 89, 32 and 77 s cold). The e2e suites themselves run on the new images in Task 10's gate.

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
decision D2 of the H3 plan (2026-10-26/27).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```


---

### Task 6: Operator docs: strict mode on flat hosts, the unsigned confirmation, the captive portal's aftermath, binary `Release.gpg`, `flip_held_ms`, the restart on upgrade

The docs carries marked *H3's*: the overview's "PR #8 parity" H (strict mode's three consequences in `docs/configuration.md`), H2's For H3 items 2 (the unsigned confirmation's real duration in `docs/configuration.md` safeguard 2 and `docs/admin-ui-spec.md`; the README's home is Task 7), 3 (the captive portal's aftermath, `Release.gpg` included), 4 (a binary `Release.gpg`) and 9 (the `flip_held_ms` row, ruling R-H3-2). Before item 3's text calls the `Release.gpg` error transient, a handler test pins that an adopted unsigned snapshot never serves the cached page (design note 4). The strict-mode text says when a flat host turns strict and who is refused then (review C-I2), *Upgrading an existing cache* says the package's upgrade now restarts a running daemon (Task 4; ruling PH3-8), and the pre-flight checks that spec §8's `docs/configuration.md` items are all there (review V-M-13).

**Files:**
- Create: `internal/handler/portal_aftermath_test.go`
- Modify: `docs/configuration.md` (five places), `docs/admin-ui-spec.md` (§4.5, *Unsigned and absent-anchor refusals*), `docs/log-fields.md` (`flip_held_ms`)

**Interfaces:**
- Consumes: the handler test helpers `newFlipStack`, `(*flipStack).cacheRow`, `.track`, `.current`, `.members` (`internal/handler/flip_stack_test.go`, `suite_rows_test.go`), `plainVerifier`, `flipSimHost`, `hdrXCacheSnapshot`; `freshness.NewAdopter`, `(*freshness.Adopter).RunUnsigned`.
- Produces: `TestServeHTTP_AnUnsignedSnapshotNeverServesACachedPortalPage`. The README (Task 7), the release notes (Task 8) and spec revision 13 (Task 9) cite these sections by name: `docs/configuration.md` *Flat repositories* under `[freshness]`, *Anchor forms and unsigned repositories* (safeguard 2), and the `[integrity]` section's new paragraph *Flat repositories follow the same host-wide rule*.

- [ ] **Step 1: Pre-flight: what the docs will say, against the code**

Run:

```bash
go test -count=1 -run 'TestServeHTTP_FlatStrictMatrix' -v ./internal/handler/ 2>&1 | grep -E '^\s+--- (PASS|FAIL)' | grep -E 'flat-only|incomplete_flat|third_minor' | sed -E 's/ \([0-9.]+s\)$//'
grep -n '"unvouched_deb_passthrough_no_coverage",\|"incomplete_snapshot_id", snapshotID\|key := host + "|" + path + "|" + strconv.FormatInt(hourBucket, 10)' internal/handler/handler.go
grep -n "AND sf.current_snapshot_id = ss.snapshot_id\|AND ss.anchor_form     <> 'unsigned'" internal/cache/queries.go
grep -n "AND ss.anchor_form    <> 'unsigned'" internal/cache/adoption.go
grep -n 'if a.hosts\[host\] {' internal/freshness/admission.go
grep -n 'h.adoptions.AdoptionsInFlight(req.CanonicalHost)' internal/handler/handler.go
grep -n '"reason", "no_sha256",\|"reason", "no_packages_members",' internal/freshness/adoption.go
grep -n 'retryBaseDelay  = 2 \* time.Minute\|retryJitterFrac = 0.25' internal/freshness/retry.go
grep -n 'minConfirmGap = 5 \* time.Minute\|deniedConfirmGap = time.Hour\|return max(c.cooldown, minConfirmGap)' internal/freshness/first_adoption.go
grep -n '{{"InRelease", t.inRelease}, {"Release", t.release}}' internal/freshness/anchorless.go
grep -n 'HoldMax = 20 \* time.Second\|timer := time.NewTimer(g.holdMax)\|t.held = now.Sub(joined)' internal/flipgate/gate.go
go test -count=1 -v -run 'TestSniffReleaseGPG$|TestVerifyDetached_HappyPath_Binary$' ./internal/aptpath/ ./internal/gpg/ | grep -c '^--- PASS'
grep -n 'deb-systemd-invoke try-restart apt-cacher-ultra.service' packaging/scripts/postinstall.sh
for p in '^| `adopt_unsigned` |' '^| `flip_quiet_period` |' '^| `max_duration` |' '^| `max_suites` |' '^| `suite_idle_ttl` |' \
    '^### Anchor forms and unsigned repositories$' 'never-downgrade' 'LAN client' 'require_signature = false' \
    '^### Upgrading an existing cache$' 'never counts for strict mode' 'Strict posture changes as eager first adoptions land' \
    '^### Eager first adoption$' '^### Suite reaping$' "a hold lasts at most 20 s, below apt's 30 s timeout" \
    'at most a tenth of it per upstream host'; do
    printf '%s %s\n' "$(grep -c -- "$p" docs/configuration.md)" "$p"
done
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
1028:   AND sf.current_snapshot_id = ss.snapshot_id
1031:   AND ss.anchor_form     <> 'unsigned'`
977:   AND ss.anchor_form    <> 'unsigned'`
1102:   AND ss.anchor_form    <> 'unsigned'`
49:	if a.hosts[host] {
77:	if a.hosts[host] {
1073:			if mayWait && h.adoptions != nil && h.adoptions.AdoptionsInFlight(req.CanonicalHost) {
1993:					if h.adoptions != nil && h.adoptions.AdoptionsInFlight(req.CanonicalHost) {
2706:			"reason", "no_packages_members",
2740:			"reason", "no_sha256",
12:	retryBaseDelay  = 2 * time.Minute
14:	retryJitterFrac = 0.25
31:	minConfirmGap = 5 * time.Minute
36:	deniedConfirmGap = time.Hour
45:func (c *Checker) confirmGap() time.Duration { return max(c.cooldown, minConfirmGap) }
267:	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
66:	HoldMax = 20 * time.Second
370:	timer := time.NewTimer(g.holdMax)
387:	t.held = now.Sub(joined)
2
31:            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
1 ^| `adopt_unsigned` |
1 ^| `flip_quiet_period` |
1 ^| `max_duration` |
1 ^| `max_suites` |
1 ^| `suite_idle_ttl` |
1 ^### Anchor forms and unsigned repositories$
1 never-downgrade
2 LAN client
1 require_signature = false
1 ^### Upgrading an existing cache$
2 never counts for strict mode
1 Strict posture changes as eager first adoptions land
1 ^### Eager first adoption$
1 ^### Suite reaping$
1 a hold lasts at most 20 s, below apt's 30 s timeout
1 at most a tenth of it per upstream host
```

What each line backs: the four strict-matrix rows are the three PR #8 parity consequences (a flat-only host is strict; an incomplete flat snapshot turns strict mode off for its host, naming it in `unvouched_deb_passthrough_no_coverage`; a third minor's first adoption is waited for while it runs and refused while it is queued); the passthrough line is INFO and keyed per host, path and hour; strict mode counts the host's *current* signed snapshots only, so the first signed flat snapshot makes the host strict while its other suites are still unadopted (`HostCurrentSnapshotsCoverage`, `internal/cache/queries.go`); an unsigned snapshot vouches nothing (`declaredExactSQL` and its fallback `declaredBySuffixSQL` both require `anchor_form <> 'unsigned'`); a host admits one adoption at a time (`admission.tryAcquire`), and the strict wait covers only an adoption in flight (`AdoptionsInFlight`), so a queued one's packages are refused; the two coverage reasons exist; the unsigned confirmation is the first retry (2 minutes, doubling, ±25 %) at least `max(cooldown, 5 min)` after the first observation, an hour when the confirming check sees a 401/403 on a signature file (`deniedConfirmGap`); the anchorless refresh deletes only the cached `InRelease` and `Release` (so a cached `Release.gpg` stays); a hold's waiter arms its own 20 s timer from when it joined and its wait is measured after the gate's mutex (so `flip_held_ms` can exceed 20000 by the timer's lateness); the cache accepts a binary `Release.gpg` (its sniff and its detached verification: `2`, both tests ran — a renamed test would print `0`, review E-M-6); the postinst's try-restart (Task 4); and the loop's sixteen counts, each at least 1, are spec §8's `docs/configuration.md` items: the five new keys' rows (C11), the unsigned threat notes, never-downgrade, the LAN-client note, the changed `require_signature = false`, the legacy audit at upgrade, strict mode's unsigned and first-use behaviour (its flat behaviour is this task's), eager first adoption, suite reaping, the barrier's bound below apt's timeout and the per-host suite cap. apt's refusal of a binary detached signature is e2e/flat's planning observation on all six images (the H2 plan's deviation 1), not a property of this code. Line numbers are HEAD's (planning: 559fd7f with Tasks 1–5 applied); a moved line is fine, a missing one or a `0` count is a stop.

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

- A host that serves only flat repositories is strict as soon as one of
  them has a signed snapshot, as long as every signed snapshot on the
  host has complete coverage: a `.deb` there that no signed snapshot's
  `Packages` declares — a file the vendor serves beside the repository
  without listing it, a package of an unsigned repository, or one of a
  repository not adopted yet (after an upgrade the host's repositories
  adopt one at a time) — is refused, where earlier releases passed it
  through.
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

- [ ] **Step 10: `docs/configuration.md`, *Upgrading an existing cache*: the package restarts a running daemon (ruling PH3-8; Task 4)**

`old`:

```markdown
### Upgrading an existing cache

On its first start after an upgrade, the daemon audits every metadata snapshot
```

`new`:

```markdown
### Upgrading an existing cache

Upgrading the Debian package to 1.1.0 or later restarts a running daemon
(`try-restart`: a stopped daemon stays stopped until you start it), so its
first start after the upgrade, and what that start does below, begins as the
package is upgraded; earlier packages left the old binary running until its
next start.

On its first start after an upgrade, the daemon audits every metadata snapshot
```

- [ ] **Step 11: Check the edits and the guards**

Run:

```bash
grep -c 'Flat repositories follow the same host-wide rule' docs/configuration.md
grep -c 'strict as soon as one of' docs/configuration.md
grep -c 'Upgrading the Debian package to 1.1.0 or later restarts a running daemon' docs/configuration.md
grep -c 'usually 5 to 15' docs/configuration.md
grep -c 'usually 5 to 15' docs/admin-ui-spec.md
grep -c "Signed file isn't valid, got 'NODATA'" docs/configuration.md
grep -c 'unsupported binary format' docs/configuration.md
grep -c 'about 20000 at most' docs/log-fields.md
go test -count=1 -run 'TestEveryEventIsDocumented|TestSweptEventNames|TestLogMessagesAreSnakeCase|TestLogMethodsAreOnlyCalledDirectly' ./internal/observability/
gofmt -l internal/handler; golangci-lint run ./internal/handler/...
```

Expected: `1` from each of the eight greps, `ok` for `internal/observability`, no file from `gofmt -l`, and `0 issues.`

- [ ] **Step 12: Commit**

```bash
git add internal/handler/portal_aftermath_test.go docs/configuration.md docs/admin-ui-spec.md docs/log-fields.md
git commit -F - <<'EOF'
Docs: strict mode on flat hosts, the unsigned wait, a portal's aftermath

docs/configuration.md: flat repositories under strict mode's host-wide
rule (a flat-only host is strict as soon as one of its repositories has
a signed snapshot, so unsigned repositories' packages and those of
repositories not adopted yet are refused there; an incomplete flat
snapshot turns it off for its host; a newly added suite's first install
is refused while its first adoption is queued), as the overview's PR #8
parity section asks; the package's upgrade now restarts a running
daemon (Upgrading an existing cache); the unsigned confirmation's real duration (at least 5
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

### Task 7: README: repositories, suite reaping and `/forget`, the admin answers, testing

Spec §8's README items — supported repository layouts (flat forms, the `/` and `./` twins, the host-root limitation, unsigned, Release-less and SHA512-only support), the adoption-enabled requirement, GC and `/forget` — and the carries marked *H3's*: the overview's *(E)* (unsigned and SHA512-only repositories, eager first adoption, `adopt_unsigned`'s threat notes), *(F)* (suite reaping and `POST /forget` beside *Recovering a degraded repository*, and `/forget` in the sentence that names `POST /reconcile`), *(G)* (flat repositories), H2's For H3 items 1 (testing, pointing at e2e/flat's triage subsection by name) and 2 (the unsigned confirmation's real duration), and the 1.1.0 `/reconcile` answers (400 and 413, which the README's example omits). The README stays an operator's document (design note 3): what is supported and how to use it, linking to `docs/configuration.md` for the rules. Its *Status* paragraph is the maintainer's and stays as it is.

**Files:**
- Modify: `README.md` (eight places)

**Interfaces:**
- Consumes: the `docs/configuration.md` sections Task 6 finished (*Flat repositories*, *Anchor forms and unsigned repositories*, *Eager first adoption*, *Suite reaping*, *Forgetting a suite (`POST /forget`)*, the `[integrity]` section); `e2e/live/README.md` (Task 3); `e2e/flat/README.md`'s *Flake, environment or daemon bug?*.
- Produces: the README sections `## Repositories`, `## Suites no client uses`, `## Forgetting a suite`, which the release notes (Task 8) link to; `check_links.py` (Step 8), which Tasks 8, 9 and 10 run too.

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
grep -n '"apt-cacher-ultra starting",\|"gc_suite_idle_ttl", cfg.GC.SuiteIdleTTL.Duration' cmd/apt-cacher-ultra/main.go
grep -c 'Undecoded' internal/config/config.go
```

Expected: the seven `docs/configuration.md` headings (the README's anchors are GitHub's slugs of them: `#eager-first-adoption`, `#flat-repositories`, `#anchor-forms-and-unsigned-repositories`, `#suite-reaping`, `#forgetting-a-suite-post-forget`, `#integrity-corruption-checks`, `#admin-status-metrics-and-maintenance`); e2e/flat's triage heading (slug `#flake-environment-or-daemon-bug`); `e2e/live/README.md exists`; the 720 h default of `gc.suite_idle_ttl`; the reaper's hourly interval and its one-hour floor; `/reconcile`'s and `/forget`'s 400, 413 and 403 answers (`parseSuiteForm`, `requireMutationGuard`); `flat_host_root_unsupported` in the handler; CI's and the release workflow's `ACU_E2E_` guard; the startup line's `gc_suite_idle_ttl` key (the check an operator runs after setting it) and no `Undecoded` check in `internal/config` (a key in the wrong section is ignored without a word: the security review's H-1, open). Planning run, at HEAD (559fd7f with Tasks 1–6 applied):

```
274:### Eager first adoption
330:### Flat repositories
550:### Anchor forms and unsigned repositories
825:## `[integrity]`: corruption checks
942:### Suite reaping
1067:## `[admin]`: status, metrics, and maintenance
1120:### Forgetting a suite (`POST /forget`)
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
350:	logger.Info("apt-cacher-ultra starting",
394:		"gc_suite_idle_ttl", cfg.GC.SuiteIdleTTL.Duration,
0
```

(Line numbers are HEAD's; a moved line is fine, a missing one is a stop. `parseSuiteForm` (716–735) and `requireMutationGuard` (789–802) serve both endpoints; 590 is `/reconcile`'s missing host or suite.)

The other facts the new text states are Task 6's pre-flight (the unsigned confirmation and its hour after a 401/403, strict mode) and `docs/configuration.md`'s own sections, which earlier phases checked: a suite reaped is tracked again at its next request (*Suite reaping*), `/forget` is the way out of `upstream_older`, `rollback_refused` and `unsigned_downgrade_refused`, but lifts the last only once no other suite of the path (its `/.` twin, or its copy under the other scheme) has a row, and its 202 body names the suite, the signed-path records deleted and any suite that kept them (*Forgetting a suite*; `forgottenText`, `internal/admin/handlers.go`), with adoption disabled suites are only checked for changes (`adoption.enabled`'s row), a Release-less flat repository is refreshed file by file and an HTML page never replaces an index (*Flat repositories*), and e2e/flat's times (its README's *Running it*: 24.5 minutes for all six; the H2 plan's For H3 item 1: about 45 at worst, cold).

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
  `apt update` through the cache; an hour when its host answers 401 or
  403 for the missing signature file, as Amazon S3 does), and a
  repository the cache has seen signed is never adopted unsigned. Read the [threat
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
must keep serving suites nobody asks for sets `suite_idle_ttl = "0s"` in
the `[gc]` section — before upgrading from 1.0, whose suites count as idle
from the last request before the upgrade ([Suite
reaping](docs/configuration.md#suite-reaping)); the `apt-cacher-ultra
starting` log line then shows `gc_suite_idle_ttl=0s` (a key in another
section is ignored without a warning).

## Forgetting a suite

`POST /forget` reaps one suite at once, however recently it was used. It
is the way out of two refusal states the status page shows: a deliberate
upstream rollback (`refused: upstream_older` or `rollback_refused`) and a
vendor that stopped signing (`refused: unsigned_downgrade_refused`). The
next request makes the suite new again and, with adoption enabled, adopts
what upstream serves then (for `unsigned_downgrade_refused`, forget the
path's other suites too — its `/.` twin, or its copy under the other
scheme — or it is refused again). It takes the same header and host rules as
`POST /reconcile`; pass the scheme, host and suite path the status page
shows:

```sh
curl -sS -X POST http://127.0.0.1:6789/forget -H 'X-ACU-Admin: 1' \
    --data-urlencode 'scheme=https' \
    --data-urlencode 'host=packages.microsoft.com' \
    --data-urlencode 'suite=/ubuntu/24.04/prod/dists/noble'
# 202 Accepted = forgotten; the body names the suite, the signed-path
#                records deleted and any suite that kept them
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
checks the live repositories those fixtures copy, by hand before 1.1.0
and before later releases that change the request path, adoption or
vouching ([e2e/live](e2e/live/README.md)). A binary built with `-tags
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
apt-cacher-ultra/blob/main/<path> or …/blob/1.1.0/<path> (the release
notes link the release's tag: ruling PH3-6) — names an existing file of
this tree, and each #anchor names a heading of that file (GitHub's slug:
lower case, every character but letters, digits, spaces, hyphens and
underscores dropped, spaces made hyphens). A link to this repository's
blob/ under any other ref is broken. Links with a title
([x](path "title")) and reference-style links ([x]: path) are not checked;
these documents have none. Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = re.compile(r"https://github\.com/linsomniac/apt-cacher-ultra/blob/([^/]+)/")
REFS = ("main", "1.1.0")


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
        blob = BLOB.match(target)
        if blob:
            rel = target[blob.end():]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = (not blob or blob.group(1) in REFS) and f.is_file() and (not anchor or anchor in slugs(f))
        bad += not ok
        print(f"{'ok  ' if ok else 'BAD '} {name}: {target}")
sys.exit(1 if bad else 0)
EOF
python3 "$T/check_links.py" README.md | grep -c '^ok'; python3 "$T/check_links.py" README.md | grep '^BAD' || echo "no broken link"
```

Expected: the sections in this order — `Status`, `Features`, `Quickstart`, `Configuration`, `Repositories`, `Inspecting the cache`, `Recovering a degraded repository`, `Suites no client uses`, `Forgetting a suite`, `Build`, `License`; `1`; `21` links checked `ok` (the planning run's count on the edited README); and `no broken link`. The checker resolves every repository link — relative, or `https://github.com/linsomniac/apt-cacher-ultra/blob/main/…` or `…/blob/1.1.0/…` — to a file and a heading slug of this tree; a link to a heading that is missing (a broken one, as a mutation in planning showed: `BAD  linkmut.md: docs/configuration.md#no-such-heading`) prints `BAD`.

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
make before upgrading in the [gc] section and the startup line that
shows it took; POST /forget beside POST /reconcile, whose example now
lists its 400 and 413 answers, with the twin caveat of
unsigned_downgrade_refused; and testing: make e2e-flat (with e2e/flat's
triage subsection) and make e2e-live. The overview's E, F and G carries
and the H2 plan's For H3 items 1 and 2.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 8: The 1.1.0 release notes

`docs/release-notes/1.1.0.md` (ruling R-H3-1; decided by ruling PH3-1), with every carry marked *H3's*: *Upgrade* — `gc.suite_idle_ttl` for offline and archive caches (the overview's *(F)* carry), the G-1 header and `Host` guard on `POST /reconcile` (*(G-1)*), `/reconcile`'s 400s (*(H1)*), the `require_signature = false` and suite-seed behaviour changes (*(H1)*), the legacy trust audit's corrupt-anchor deletion and deferred snapshots kept at every `gc.keep_displaced` value (the H1 plan's *For H3* with the overview's correction); *Logging* — the six removed 1.0.x messages with their replacements, the log-query note, the two repeating WARNs, the computed inventory of new events, fields, values and metrics since 1.0.x (design note 1; the H1 *For H3* lists names R-H3's and R-H6's renames use, which were never released, so the notes list new events by their names only), `e2e_commit_delay_enabled` marked test-binary-only (H2 For H3 item 6); the alert recipes (reaper, GC); the captive portal's aftermath, `Release.gpg` included (H2 For H3 item 3; Task 6 pinned and documented it); the e2e harness and the live smoke's results (Task 3; ruling R-H3-8); and, from the reviews and the maintainer's decisions, that all of it needs adoption enabled (review C-I1), who is refused on a newly strict flat host (C-I2), where `suite_idle_ttl` goes and how to see it took (C-I3), the suite caps for very large hosts (C-I6), the package upgrade's restart (Task 4; ruling PH3-8), Go 1.27 (Task 5; decision D2) and the credit the maintainer chose (D5). Design note 2 gives the structure. Links are absolute, to the release's tag (`…/blob/1.1.0/…`, ruling PH3-6), so they never drift and work whether or not the branch is merged; for a release candidate, the pasted copy's `blob/1.1.0/` becomes `blob/<rc tag>/` (`docs/open-decisions.md`, *Before the 1.1.0 release candidate*, gives the `sed` line). The link checker resolves them against this tree.

The H1 *For H3* list said `acu_clock_steps_detected_total` "now also counts the reaper's forward steps" and that `suite_reaped` "gains" fields; the overview's *(H1)* correction applies: both are new since 1.0.x, so the notes list them as new. It said R-H1 applies "with `gc.keep_displaced = 0`"; it applies at every value (the notes say "whatever `gc.keep_displaced` is").

**Files:**
- Create: `docs/release-notes/1.1.0.md`

**Interfaces:**
- Consumes: the README sections of Task 7, the `docs/configuration.md` sections of Task 6, `e2e/live`'s results (Task 3), the postinst of Task 4, the toolchain of Task 5, `check_links.py` (Task 7 Step 8).
- Produces: `docs/release-notes/1.1.0.md`, which Task 9 records in the spec and the overview, and Task 10 checks again on the final tree (Steps 1 and 4 rerun there) and updates with the final tree's live smoke (Task 10 Step 9).

- [ ] **Step 1: Pre-flight: the inventories, from the code at 1.0.2 and at HEAD**

Run (from the repository root; `git archive` writes a file and `tar` reads it, two commands, no pipe):

```bash
D=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes; rm -rf "$D"; mkdir -p "$D/old" "$D/new"
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

Run (`O` is 1.0.2's tree from Step 1; `D` as in Step 1):

```bash
D=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes
O=$D/old
grep -n 'deb-systemd-invoke try-restart apt-cacher-ultra.service' packaging/scripts/postinstall.sh
grep -n '^enabled               = false' packaging/config/config.toml.default
grep -c 'ADOPTION DISABLED &mdash; METADATA NOT REFRESHED' internal/admin/status_template.go
grep -n 'const CurrentSchemaVersion = ' internal/cache/schema.go $O/internal/cache/schema.go
grep -n 'refusing to downgrade' $O/internal/cache/schema.go
grep -n 'filepath.Join(dir, "cache.db")\|journal_mode(WAL)' internal/cache/cache.go
grep -n 'legacyAuditBatchSize        = 100\|logger.Info("legacy_trust_audit_progress"' internal/freshness/legacy_audit.go
grep -c 'Nothing was changed' internal/freshness/legacy_audit.go
grep -n 'none of the %d queued inline anchors exist' internal/freshness/legacy_audit.go | cut -c1-90
grep -n 'reapInterval = time.Hour\|reapUptimeFloor = time.Hour\|regardlessReapsPerRun = 250' internal/freshness/reaper.go
grep -n 'cfg.GC.SuiteIdleTTL.Duration = 720 \* time.Hour' internal/config/config.go
grep -n 'const interestWindowSeconds = 7 \* 24 \* 60 \* 60\|now-interestWindowSeconds' internal/cache/schema.go
grep -n '"apt-cacher-ultra starting",\|"gc_suite_idle_ttl", cfg.GC.SuiteIdleTTL.Duration' cmd/apt-cacher-ultra/main.go
grep -c 'Undecoded' internal/config/config.go
grep -n 'w.Header().Set("Allow", "POST, OPTIONS")' internal/admin/server.go
grep -n 'request body too large\\n", http.StatusRequestEntityTooLarge\|reconcile not triggered' $O/internal/admin/handlers.go
grep -n 'logger.Warn("adoption_require_signature_disabled")' cmd/apt-cacher-ultra/main.go
grep -n 'unsigned upstream metadata would be adopted' $O/cmd/apt-cacher-ultra/main.go
grep -n 'handler: put url row' internal/handler/handler.go
grep -n 'seed suite_freshness failed' $O/internal/handler/handler.go
grep -n 'perHost := maxSuites / 10\|logger.Warn("suite_caps_exceeded"' cmd/apt-cacher-ultra/main.go
grep -n 'h.logger.Warn("suite_seed_rejected"' internal/handler/suite_rows.go
grep -c 'no clearsign line; apt rejects it too' docs/configuration.md
grep -n 'cfg.Freshness.MaxSuites = 5000\|cfg.Adoption.MaxDuration.Duration = 30 \* time.Minute\|cfg.Adoption.FlipQuietPeriod.Duration = 10 \* time.Second\|AdoptUnsigned: true,' internal/config/config.go
grep -n 'c.logger.Warn("freshness: persist failure-bump failed"\|c.logger.Info("freshness check failed"\|errors.New("unexpected success status")\|errUnexpectedStatus, res.Status' internal/freshness/check.go
grep -n 'every path refused by validateMemberPath\|no stanza with Package, Directory and a well-formed Checksums-Sha256 entry' internal/freshness/sources_parse.go
grep -n 'no stanzas with Package + Directory + Checksums-Sha256' $O/internal/freshness/sources_parse.go
grep -n 'a.logger.Warn("adoption_member_skipped"' internal/freshness/adoption.go $O/internal/freshness/adoption.go
grep -n 'freshnessCheckTotal.Inc(e.result, host)' internal/freshness/check.go
grep -n '"adopted_stale", warnStale,\|"paced_stale", absentStale,' internal/freshness/scheduler.go
grep -n 'retryBaseDelay  = 2 \* time.Minute\|retryMaxDelay   = time.Hour\|retryJitterFrac = 0.25' internal/freshness/retry.go
grep -n 'const checkWorkers = 4' internal/freshness/scheduler.go
grep -n 'strictWait = 15 \* time.Second' internal/freshness/inflight.go
grep -n 'db.SetMaxIdleConns(maxIdleConns())' internal/cache/cache.go
grep -n '"acu_gc_last_run_unixtime",' -A 3 internal/gc/metrics.go
grep -c "go-version: '1.27'" .github/workflows/ci.yaml .github/workflows/release.yaml
grep -n '^go ' go.mod
go test -count=1 -v -run 'TestMutatingEndpoints_|TestServeListeners_StartsTheReaperWhateverGCSays|TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory|TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot|TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor|TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic|TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals|TestStartupPass_ReturnsAPanicAsAnError|TestAptEncodePath_Table|TestServeHTTP_FlatStrictMatrix' ./cmd/apt-cacher-ultra/ ./internal/admin/ ./internal/cache/ ./internal/handler/ ./internal/gpg/ ./internal/freshness/ ./internal/gc/ ./internal/aptpath/ | grep -c '^--- PASS'
cat > "$D/additive_keys.py" <<'EOF'
#!/usr/bin/env python3
"""Check the notes' "additive keys on existing lines" (phase H3): each
(message, key) pair is a key a log call of that message carries at HEAD
and no call of it carries at 1.0.2, and the message itself is logged at
1.0.2 (an existing line). A call is the text from the message's string
literal to the parenthesis that closes the call it is an argument of.
Usage: additive_keys.py OLD-TREE NEW-TREE"""
import pathlib
import re
import sys

PAIRS = [
    ("freshness_check", "reason"),
    ("adoption_member_skipped", "declared_sha512"),
    ("adoption_member_mismatch", "declared_sha512"),
    ("adoption_member_mismatch", "actual_sha512"),
    ("pool_corruption_during_adoption", "declared_sha512"),
    ("pool_corruption_during_adoption", "derived_sha256"),
    ("package_coverage_incomplete", "no_sha256_stanzas"),
    ("adoption_reconcile_failed", "stage"),
    ("snapshot_member_blob_missing", "snapshot_displaced"),
    ("apt-cacher-ultra starting", "adoption_adopt_unsigned"),
    ("apt-cacher-ultra starting", "adoption_flip_quiet_period"),
    ("apt-cacher-ultra starting", "freshness_max_suites"),
    ("apt-cacher-ultra starting", "gc_suite_idle_ttl"),
    ("phase2 adoption enabled", "adopt_unsigned"),
    ("phase2 adoption enabled", "flip_quiet_period"),
    ("phase2 adoption enabled", "max_duration"),
]


def calls(tree: pathlib.Path, msg: str) -> list:
    out = []
    for p in list(tree.glob("internal/**/*.go")) + list(tree.glob("cmd/**/*.go")):
        if p.name.endswith("_test.go"):
            continue
        s = p.read_text()
        for m in re.finditer(re.escape('"' + msg + '"'), s):
            depth, i = 1, m.end()
            while i < len(s) and depth:
                depth += {"(": 1, ")": -1}.get(s[i], 0)
                i += 1
            call = s[m.end():i]
            # A call that passes a slice of attributes (logAttrs...) carries
            # what the slice was built with: from its definition to the call.
            v = re.search(r"\b([A-Za-z_]\w*)\.\.\.", call)
            if v:
                d = s.rfind(v.group(1) + " := []any{", 0, m.start())
                if d >= 0:
                    call = s[d:m.start()] + call
            out.append(call)
    return out


old, new = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
bad = 0
for msg, key in PAIRS:
    oc, nc = calls(old, msg), calls(new, msg)
    lit = '"' + key + '"'
    ok = bool(oc) and bool(nc) and any(lit in c for c in nc) and not any(lit in c for c in oc)
    bad += not ok
    print(f"{'ok  ' if ok else 'FAIL'} {msg}: {key} (calls at 1.0.2 {len(oc)}, at HEAD {len(nc)})")
sys.exit(1 if bad else 0)
EOF
python3 "$D/additive_keys.py" "$O" .
```

Expected (the planning run, 559fd7f with Tasks 1–7 applied; line numbers are HEAD's and 1.0.2's, the order of a multi-pattern grep's lines may differ):

```
31:            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
71:enabled               = false
1
internal/cache/schema.go:18:const CurrentSchemaVersion = 7
/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes/old/internal/cache/schema.go:18:const CurrentSchemaVersion = 6
309:		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); refusing to downgrade",
114:	db, err := openDB(filepath.Join(dir, "cache.db"))
174:	q.Add("_pragma", "journal_mode(WAL)")
26:	legacyAuditBatchSize        = 100
190:		logger.Info("legacy_trust_audit_progress",
2
256:		return fmt.Errorf("legacy trust audit: none of the %d queued inline anchors exist un
28:	reapInterval = time.Hour
33:	reapUptimeFloor = time.Hour
41:	regardlessReapsPerRun = 250
804:		cfg.GC.SuiteIdleTTL.Duration = 720 * time.Hour
380:const interestWindowSeconds = 7 * 24 * 60 * 60
390:  ?)`, now-interestWindowSeconds); err != nil {
350:	logger.Info("apt-cacher-ultra starting",
394:		"gc_suite_idle_ttl", cfg.GC.SuiteIdleTTL.Duration,
0
402:				w.Header().Set("Allow", "POST, OPTIONS")
423:				w.Header().Set("Allow", "POST, OPTIONS")
433:				w.Header().Set("Allow", "POST, OPTIONS")
565:		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
580:		http.Error(w, "reconcile not triggered (busy, unknown, or no current snapshot)\n", http.StatusConflict)
416:		logger.Warn("adoption_require_signature_disabled")
402:		logger.Warn("adoption.require_signature = false: unsigned upstream metadata would be adopted (auditable per SPEC2 §5.1)")
2235:		return sfResult{err: fmt.Errorf("handler: put url row: %w", err), status: fres.Status}
1570:			h.logger.Warn("seed suite_freshness failed",
1133:	perHost := maxSuites / 10
1142:	logger.Warn("suite_caps_exceeded",
243:	h.logger.Warn("suite_seed_rejected",
1
716:		cfg.Freshness.MaxSuites = 5000
744:		cfg.Adoption.MaxDuration.Duration = 30 * time.Minute
749:		cfg.Adoption.FlipQuietPeriod.Duration = 10 * time.Second
897:			AdoptUnsigned: true,
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
/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes/old/internal/freshness/adoption.go:1108:					a.logger.Warn("adoption_member_skipped",
/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes/old/internal/freshness/adoption.go:1164:				a.logger.Warn("adoption_member_skipped",
/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes/old/internal/freshness/adoption.go:1764:			a.logger.Warn("adoption_member_skipped",
273:	freshnessCheckTotal.Inc(e.result, host)
694:			"adopted_stale", warnStale,
695:			"paced_stale", absentStale,
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
.github/workflows/ci.yaml:2
.github/workflows/release.yaml:1
3:go 1.25.0
17
ok   freshness_check: reason (calls at 1.0.2 2, at HEAD 1)
ok   adoption_member_skipped: declared_sha512 (calls at 1.0.2 4, at HEAD 4)
ok   adoption_member_mismatch: declared_sha512 (calls at 1.0.2 2, at HEAD 3)
ok   adoption_member_mismatch: actual_sha512 (calls at 1.0.2 2, at HEAD 3)
ok   pool_corruption_during_adoption: declared_sha512 (calls at 1.0.2 2, at HEAD 3)
ok   pool_corruption_during_adoption: derived_sha256 (calls at 1.0.2 2, at HEAD 3)
ok   package_coverage_incomplete: no_sha256_stanzas (calls at 1.0.2 4, at HEAD 5)
ok   adoption_reconcile_failed: stage (calls at 1.0.2 1, at HEAD 3)
ok   snapshot_member_blob_missing: snapshot_displaced (calls at 1.0.2 1, at HEAD 2)
ok   apt-cacher-ultra starting: adoption_adopt_unsigned (calls at 1.0.2 1, at HEAD 1)
ok   apt-cacher-ultra starting: adoption_flip_quiet_period (calls at 1.0.2 1, at HEAD 1)
ok   apt-cacher-ultra starting: freshness_max_suites (calls at 1.0.2 1, at HEAD 1)
ok   apt-cacher-ultra starting: gc_suite_idle_ttl (calls at 1.0.2 1, at HEAD 1)
ok   phase2 adoption enabled: adopt_unsigned (calls at 1.0.2 1, at HEAD 1)
ok   phase2 adoption enabled: flip_quiet_period (calls at 1.0.2 1, at HEAD 1)
ok   phase2 adoption enabled: max_duration (calls at 1.0.2 1, at HEAD 1)
```

What each line backs, in the notes' order: the postinst's try-restart (Task 4); adoption off in the packaged config and the status page's "ADOPTION DISABLED — METADATA NOT REFRESHED"; the one-way migration and 1.0.x's refusal message; the database file and its WAL (the copy to keep); the audit's progress line per 100 snapshots and its two "Nothing was changed" pool refusals (an empty pool, or one that holds none of two or more queued anchors); the reaper's hourly interval, one-hour floor and 250 non-canonical or junk suites a run, the 720 h default, and the backfill that counts a suite with no request from 7 days before the upgrade (720 h − 7 d ≈ 23 days); the startup line's `gc_suite_idle_ttl` and no `Undecoded` check (a key in the wrong section is ignored without a word); `OPTIONS`' `Allow` (`/reconcile`'s and `/forget`'s 405s name it too: three lines; review E-M-3) (and 1.0.x's 413 for any unparseable body and its 409 text); the renamed `require_signature` WARN and 1.0.x's text; the suite seed inside the store's transaction and 1.0.x's WARN; the suite caps (a tenth per host), their startup WARN and the seed refusal's WARN; the plain-text `InRelease` rule (*Tracked suites*); the new keys' defaults (`check_notes.py` checks their names, not their values: review E-M-8); the replacements of the removed messages and the `unexpected success status <code>` error; `source_parse_failed`'s new and old texts; `adoption_member_skipped` at WARN in both releases (the first of each three is the `arch_not_in_allowlist` skip); the new `freshness_check` results counted by `acu_freshness_check_total{result}`, and the stale-suites WARN's `adopted_stale` without the paced suites it reports as `paced_stale`; the retry schedule, the four check workers, the 15 s strict wait, the idle connections kept; the GC gauge's `phase` label (`max()` in the alert covers both phases); Go 1.27 in CI and the release, `go 1.25.0` kept (Task 5). The tests (`17` `--- PASS` lines: each pattern matched a test, so a renamed one would lower the count — review E-M-6): the admin guard and answers (`TestMutatingEndpoints_*`: the header, cross-origin, rebinding hosts, `htpasswd_file`, 400s, one newline, `OPTIONS`), the reaper started whatever `gc.enabled` says, R-H1 at `keep_displaced` 0 and 1, R-H2's refetch, the I-1 and I-7 fixes, GC's startup panic, the `+`/`~` encoding, strict mode on flat hosts. The last block is `additive_keys.py`: each additive key the notes list on an existing line is carried by a call of that line at HEAD and by none at 1.0.2 (review C-M-6; it follows a call's `logAttrs...` slice back to its definition). The rest — the eager-adoption, unsigned, flat, flip and refusal rules the notes summarise, the hold's 20 s and the hour of a 401/403 confirmation (Task 6's pre-flight), and the two ERRORs that are disk faults (`docs/log-fields.md`'s rows) — is the docs Tasks 6 and 7 checked and link to; `e2e/live`'s results are Task 3's run.

- [ ] **Step 3: Write `docs/release-notes/1.1.0.md`**

````markdown
# apt-cacher-ultra 1.1.0

1.1.0 makes flat apt repositories — pkgs.k8s.io, NVIDIA CUDA,
libnvidia-container, pkg.jenkins.io, Sublime Text and the like — first-class:
with snapshot adoption enabled, the cache now refreshes them, adopts them as
coherent snapshots and vouches their packages for strict mode. It also
adopts unsigned and SHA512-only repositories, refreshes Release-less ones
file by file, adopts a new repository at its first `apt update`, backs off
from failing upstreams instead of retrying them at every check, keeps
clients from straddling a snapshot switch, and drops suites no client uses.

All of this needs snapshot adoption (`[adoption] enabled = true`, after
loading your repositories' signing keys), which the packaged
configuration leaves off. With adoption off, 1.1.0 serves metadata as
1.0.x does: each `InRelease`, `Release` and index file — a flat
repository's included — is served as the cache first stored it, and
suites are only checked for changes (the status page reads "ADOPTION
DISABLED — METADATA NOT REFRESHED"). See
[Repositories](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/README.md#repositories).

**Read *Before you upgrade* below first.** Some changes can
surprise a running installation: the package upgrade now restarts a
running daemon, a one-way database migration, an hourly reaper that
deletes idle suites whatever `gc.enabled` says, and a new header that
`POST /reconcile` requires.

## Before you upgrade

- **The package upgrade restarts a running daemon.** Upgrading the
  `.deb` to 1.1.0 restarts the daemon if it is running (`try-restart`;
  1.0.x packages left the old binary running until its next start), so
  the migration and the audit below begin as the package is upgraded, and
  the suite reaper an hour later: make the settings below before you
  upgrade. A stopped daemon stays stopped and does all this at its next
  start. See [Upgrading an existing
  cache](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#upgrading-an-existing-cache).
- **The database is migrated one way (schema 6 to 7), and audited.** At the
  first start the daemon migrates `<cache.dir>/cache.db`, then audits every
  snapshot an earlier release adopted before it serves anything (INFO
  `legacy_trust_audit_progress` every 100 snapshots; connections made
  meanwhile wait, and apt clients may time out once). A 1.0.x binary refuses
  the migrated database (`database schema version 7 is newer than this
  binary supports (6); refusing to downgrade`): to keep a way back, stop the
  daemon and copy `cache.db` (and any `cache.db-wal` beside it) before
  upgrading. The daemon refuses to start, changing nothing, when the
  cache's `pool/` directory is missing, unreadable or empty — or holds
  none of the queued snapshots' files — while inline snapshots await the
  audit: mount or restore it, or wipe the cache directory to start empty.
  A snapshot whose stored `InRelease` is not a
  clean signed file, or whose pool file is missing or corrupt, is
  quarantined (WARN `legacy_snapshot_quarantined`): its suite is served
  from the ordinary cache until it adopts again, and a corrupt one's cached
  copies and pool file are deleted, so clients fetch a fresh one. One whose
  pool file cannot be read at all is deferred (ERROR
  `legacy_trust_audit_deferred` at every start) and kept, by snapshot GC
  too, whatever `gc.keep_displaced` is, until the pool is fixed or the
  suite is reaped or forgotten. See [Upgrading an existing
  cache](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#upgrading-an-existing-cache).
- **Offline and archive caches: set `suite_idle_ttl = "0s"` in the `[gc]`
  section before upgrading.** A new suite reaper runs an hour after the
  start and then hourly, whatever `gc.enabled` says, and deletes the suites
  no client has asked for in `gc.suite_idle_ttl` (720 h, 30 days, by
  default), with their cached index files — counted from the last request
  before the upgrade (from 7 days before it for a suite with none, so such
  a suite goes about 23 days after the upgrade). Its first runs also reap,
  however recently used, the rows 1.0.x created for paths this version no
  longer tracks and suites without a snapshot whose cached `InRelease`,
  `Release` and `Release.gpg` are all error pages (INFO `suite_reaped`,
  `reason=non_canonical` or `junk`, up to 250 a run). An install that
  relied on `gc.enabled = false` to keep everything needs the setting too.
  A key in the wrong section is ignored without a warning: after the first
  start, check that the `apt-cacher-ultra starting` line shows
  `gc_suite_idle_ttl=0s`. A reaped suite comes back at its next request,
  fetched from upstream. See [Suite
  reaping](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#suite-reaping).
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
  listener](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#admin-status-metrics-and-maintenance).
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
  `integrity.refuse_unvouched_debs = true` (and adoption enabled), a host
  that serves only flat repositories becomes strict as soon as one of them
  has a signed snapshot, as long as every signed snapshot on the host has
  complete coverage. A `.deb` there that no signed snapshot vouches for is
  then refused (502) where 1.0.x passed it through: a file the vendor
  serves beside a repository without listing it, every package of an
  unsigned or Release-less repository on that host, and — right after the
  upgrade — the packages of the host's flat repositories that have not
  adopted yet. A host adopts one suite at a time, so its repositories
  adopt one after another, usually within a few minutes, each one's
  packages refused until its own adoption; later, the first install from a
  newly added flat repository is refused while its first adoption waits
  for a slot. A flat snapshot with incomplete coverage turns strict mode
  off for its whole host, as a `dists/` one does. See [strict
  mode](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#integrity-corruption-checks).
- **Caches in front of a very large host: raise `freshness.max_suites`
  first.** The cache now tracks at most `freshness.max_suites` suites
  (5000 by default) and a tenth of that per upstream host. A cache that
  already holds more keeps them and logs WARN `suite_caps_exceeded` at
  start, but a host at its cap gets no new suite: its new repositories are
  served as first cached and never refreshed or adopted (WARN
  `suite_seed_rejected`) until idle suites are reaped. Raise the key
  before upgrading a cache that fronts an Artifactory, aptly or Nexus host
  with hundreds of suites. See [Tracked
  suites](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#tracked-suites).
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
  tracked. A plain-text `InRelease` (no clearsign line, which apt rejects
  too) no longer tracks a suite.
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
  repositories](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#flat-repositories).
- **Unsigned repositories** are adopted as unsigned snapshots — served
  coherently, never trusted, never vouching — after two checks see the same
  `Release` at least 5 minutes apart (usually 5 to 15 minutes after the
  first `apt update`; an hour when its host answers 401 or 403 for the
  missing signature file, as Amazon S3 does), and never once the cache has
  seen the repository signed. **Release-less flat repositories** (Gemfury, goreleaser) are
  refreshed file by file. **SHA512-only `Release` files** adopt, and by-hash
  requests are answered for SHA256 and SHA512. Read the [threat
  notes](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#anchor-forms-and-unsigned-repositories) before
  serving unsigned repositories; `adoption.adopt_unsigned = false` turns
  them off.
- **Outages that answer with HTML** — a captive portal, a vendor's
  maintenance page — never replace a Release-less repository's cached
  index. The pages a client's own update cached during the outage as the
  repository's `InRelease` and `Release` are deleted by the first check
  after it, so the next update is clean; one cached as its `Release.gpg`
  stays, harmless while the repository publishes no `Release`, and is never
  served once the repository adopts one. See [Flat
  repositories](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#flat-repositories).
- **Eager first adoption:** a new repository is adopted at its first
  `apt update` through the cache, when an adoption slot is free, in
  whichever form its upstream publishes; a suite nobody asked for in 7 days
  costs no upstream request. See [Eager first
  adoption](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#eager-first-adoption).
- **Retry discipline:** a failed adoption is retried after 2 minutes,
  doubling to an hour (±25 %), instead of at every check, and new upstream
  content is tried at once; seven outcomes are refusals — states the status
  page shows as "refused: …" — not failures. Checks run on four workers with
  per-host fairness and their own backoff. See [How failed adoptions are
  retried](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#how-failed-adoptions-are-retried) and
  [Capacity and fairness](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#capacity-and-fairness).
- **Flip consistency:** a new snapshot waits until clients that received
  the old `InRelease` have had `adoption.flip_quiet_period` to fetch its
  indexes, holding new `InRelease`/`Release` requests at most 20 s (below
  apt's 30 s timeout), and by-hash requests are served from displaced
  snapshots. See [Flip
  consistency](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#flip-consistency).
- **Suite lifecycle:** the hourly suite reaper (above) and `POST /forget`,
  which reaps one suite at once — the way out of a deliberate upstream
  rollback and of a vendor that stopped signing. See [Forgetting a
  suite](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#forgetting-a-suite-post-forget).
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
- **Built with Go 1.27** (1.0.x: Go 1.25, which no longer gets security
  fixes); `go.mod`'s language version stays 1.25.

## Logging and metrics

Every new log message is a snake_case event name, and a test keeps it so;
the prose messages 1.0.x logs are unchanged but for the six below. Every
event this release adds is documented in [the log field
reference](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/log-fields.md).

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

One changed text an alert may match: `source_parse_failed`'s `error`
field said `sources: no stanzas with Package + Directory +
Checksums-Sha256`; it now says `sources: every path refused by
validateMemberPath (N skipped)` or `sources: no stanza with Package,
Directory and a well-formed Checksums-Sha256 entry`.

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
WARN, exclude them; whether a later release logs them at INFO is an open
decision.

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
  (`freshness: adopted suites stale beyond threshold`) gains `paced_stale`,
  the suites paced by a refusal record, which its `adopted_stale` now
  leaves out.
- Additive keys on existing lines: `freshness_check`'s `reason` (on the
  results that have one); `adoption_member_skipped`'s `declared_sha512`;
  `adoption_member_mismatch`'s `declared_sha512` and `actual_sha512`;
  `pool_corruption_during_adoption`'s `declared_sha512` and
  `derived_sha256`; `package_coverage_incomplete`'s `no_sha256_stanzas`;
  `adoption_reconcile_failed`'s `stage`; `snapshot_member_blob_missing`'s
  `snapshot_displaced`; the startup line `apt-cacher-ultra starting`'s
  `adoption_adopt_unsigned`, `adoption_flip_quiet_period`,
  `freshness_max_suites` and `gc_suite_idle_ttl`, and `phase2 adoption
  enabled`'s `adopt_unsigned`, `flip_quiet_period` and `max_duration`. The
  new `freshness_check` results above are counted in
  `acu_freshness_check_total{result}` too.

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
  consistency](https://github.com/linsomniac/apt-cacher-ultra/blob/1.1.0/docs/configuration.md#flip-consistency)).
- The `…_panicked` ERRORs above, and `acu_freshness_unlogged_panics_total`
  or `acu_flip_gate_panics_total` above 0: each is a bug; please report
  it. `legacy_trust_audit_deferred` and `legacy_anchor_discard_failed` are
  disk faults in the cache directory.

## Testing

- **`make e2e-flat`** is new: real apt clients — apt 2.2 to 3.2, on six
  Debian and Ubuntu images — against fixtures of every supported repository
  layout through the cache, about 25 minutes in sequence; CI runs one job
  per image. Fixtures only: it never reaches a live repository. `e2e/deb`,
  the package-install check, was updated for eager first adoption, and now
  checks the package's install, upgrade (the restart above) and removal
  against a stub `systemctl`.
- **`make e2e-live`** is the manual smoke against live flat repositories,
  run before 1.1.0 and before later releases that change the request path,
  adoption or vouching. On 2026-10-27, with adoption and strict mode on:
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

  Every `apt-get update` was clean (and both of the error grep's controls
  were caught), strict mode refused an undeclared `.deb` on each of the
  five hosts, and nothing else was refused.
- Release binaries never contain the end-to-end tests' seam: CI and the
  release workflow refuse a binary built with `-tags e2ehooks`.

## Credits

Thanks to Venkat (@venkatamutyala), whose PR #8 identified the
flat-repository problem; the flat-repository support in this release is
based on it.
````

The credits paragraph is the maintainer's wording, verbatim (decision D5, 2026-10-26/27); the intro no longer repeats the credit.

- [ ] **Step 4: Check the notes against the code, and their links**

Run (with Step 1's `$D`):

```bash
D=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes
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
apt-cacher-ultra/blob/main/<path> or …/blob/1.1.0/<path> (the release
notes link the release's tag: ruling PH3-6) — names an existing file of
this tree, and each #anchor names a heading of that file (GitHub's slug:
lower case, every character but letters, digits, spaces, hyphens and
underscores dropped, spaces made hyphens). A link to this repository's
blob/ under any other ref is broken. Links with a title
([x](path "title")) and reference-style links ([x]: path) are not checked;
these documents have none. Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = re.compile(r"https://github\.com/linsomniac/apt-cacher-ultra/blob/([^/]+)/")
REFS = ("main", "1.1.0")


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
        blob = BLOB.match(target)
        if blob:
            rel = target[blob.end():]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = (not blob or blob.group(1) in REFS) and f.is_file() and (not anchor or anchor in slugs(f))
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
14
no broken link
```

The checker has teeth (planning mutations): dropping `suite_touch_failed` and `acu_flip_barrier_active` from the notes printed `FAIL new events …: missing ['suite_touch_failed']` and `FAIL new metrics: missing ['acu_flip_barrier_active']`; listing `gc_panicked` under WARN printed `FAIL level of gc_panicked: listed WARN, documented ERROR`.

- [ ] **Step 5: Commit**

```bash
git add docs/release-notes/1.1.0.md
git commit -F - <<'EOF'
Release notes for 1.1.0

docs/release-notes/1.1.0.md, for the maintainer to paste into the
GitHub release (ruling R-H3-1). It opens with what all of it needs:
snapshot adoption enabled. Before you upgrade: the package's restart
of a running daemon, the one-way schema migration and the trust audit,
suite_idle_ttl in [gc] for offline and archive caches, the X-ACU-Admin
header and Host guard on POST /reconcile and its new answers,
require_signature = false no longer adopting unsigned metadata, a
suite-row error now failing the store, strict mode on flat hosts and
who it refuses, the suite caps for very large hosts, first adoptions
after the upgrade, the new keys. Then what is new, the logging changes (the six removed messages
and their replacements, exact-match queries, the two repeating WARNs,
76 new events by level, new fields, values, additive keys and 21
metrics), alert recipes, Go 1.27, the end-to-end tests and the live
smoke's results, and the credit in the maintainer's words.

Every inventory is checked against the code at 1.0.2 and at HEAD
(check_notes.py: the names of events, removed messages, metrics, config
keys, admin JSON fields and values, and the events' levels; the keys'
defaults by the pre-flight's grep; the additive keys by
additive_keys.py), every link against its heading in this tree (the
links name the 1.1.0 tag), and every other claim by a grep or a named
test (the task's pre-flight).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 9: Spec revision 13, `docs/open-decisions.md` and the overview

The spec's revision 13 records H2's deviations 1 and 2 as built, H2's design notes 5 and 7, the commit-delay full-grep deviation (H2's For H3 item 5), the error grep as built, the live smoke in §7, and what H3 changes (C11's list not exhaustive, `flip_held_ms`, §8's two new documents, §9's H row), keeping the revision table's convention: the newest revision first in the `Date` line's parentheses. `docs/open-decisions.md` is the home that outlives H3 (ruling R-H3-3; confirmed by ruling PH3-2) for the H2 For H3 item 11 carries, the captive-portal `Release.gpg` follow-up (item 3), the pre-RC steps only the maintainer can take (H2 items 7 and 8, with the reviews' corrections: a pull request runs CI, the notes' links name the tag), the two repeating WARNs and the simulation near its budget (the overview's *(H1)* carries), `govulncheck` and the module bumps (the security review's H-3, an entry of its own: review V-M-10), and, under *Decided*, the maintainer's decisions of 2026-10-26/27 (D2: Go 1.27; D4: R-H5 and the writer's fail-fast, with the alternative kept; R-H6; D5: the credit; H-2: the restart on upgrade) and the controller's (PH3-1, PH3-2, e2e/live's cadence); each entry names the maintainer as owner, its evidence in the repository (not a review's ID) and a recommendation. The overview marks every *H3's* carry done with its task and points its decisions block at the new file.

**Files:**
- Create: `docs/open-decisions.md`
- Modify: `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (the `Date` line, §7, §8, §9's H row, C11), `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (the 16 *H3's* carries, the H3 row, the spec revision, the cross-phase interfaces, the decisions block)

**Interfaces:**
- Consumes: Tasks 1–8's results (their names appear in the done markers); the live smoke's run (Task 3); `check_links.py` (Task 7 Step 8).
- Produces: `docs/open-decisions.md`, to which Task 10 appends what the final review defers (and updates the simulations entry).

- [ ] **Step 1: Pre-flight: the evidence `docs/open-decisions.md` cites**

Run:

```bash
grep -n 'func (c \*Cache) writer()' internal/cache/cache.go; grep -c 'recover()' internal/cache/cache.go
grep -n '{{"InRelease", t.inRelease}, {"Release", t.release}}' internal/freshness/anchorless.go
grep -n 'func SniffReleaseGPG' internal/aptpath/*.go
grep -n '"acu_adoption_members_skipped_total",\|"acu_adoption_sources_parsed_total",' internal/freshness/metrics.go
grep -c 'Nor is an unsigned flat repository the cache' docs/configuration.md
grep -n 'deb-systemd-invoke try-restart apt-cacher-ultra.service' packaging/scripts/postinstall.sh
grep -n -A3 '^on:' .github/workflows/ci.yaml
grep -c 'concurrency:' .github/workflows/ci.yaml
grep -n 'E2E_STEP_MINUTES: \|gating check\|timeout -s INT -k 60' .github/workflows/ci.yaml
grep -n 'NFPM_VERSION: 2.43.0\|sha256sum \* > SHA256SUMS\|name: Verify no e2e test seam' .github/workflows/release.yaml
grep -n 'nfpm@v2.40.0' e2e/deb/Dockerfile
grep -n 'aculan-deb-test:' e2e/deb/run.sh
grep -c "go-version: '1.27'" .github/workflows/ci.yaml
grep -c "go-version: '1.27'" .github/workflows/release.yaml
grep -n 'FROM golang:1.27' e2e/flat/Dockerfile e2e/deb/Dockerfile e2e/live/Dockerfile e2e/Dockerfile.cache
grep -n '^go \|cloudflare/circl \|golang.org/x/net \|golang.org/x/text ' go.mod
grep -rl govulncheck .github || echo "no govulncheck in .github"
test -e .github/dependabot.yml || echo "no .github/dependabot.yml"
curl -fsS --max-time 30 'https://go.dev/dl/?mode=json' | python3 -c 'import json, sys; print(" ".join(r["version"] for r in json.load(sys.stdin)))'
```

Expected (planning run, 559fd7f with Tasks 1–8 applied):

```
264:func (c *Cache) writer() {
0
267:	}{{"InRelease", t.inRelease}, {"Release", t.release}} {
internal/aptpath/sniff.go:44:func SniffReleaseGPG(head []byte) bool {
227:		"acu_adoption_members_skipped_total",
285:		"acu_adoption_sources_parsed_total",
1
31:            deb-systemd-invoke try-restart apt-cacher-ultra.service >/dev/null || :
3:on:
4-  push:
5-    branches: [master, main]
6-  pull_request:
0
89:  # under the packaged user. This is the gating check that the
137:      - run: timeout -s INT -k 60 "${E2E_STEP_MINUTES}m" bash e2e/flat/run.sh
141:          E2E_STEP_MINUTES: ${{ matrix.unsigned && 27 || 12 }}
29:      NFPM_VERSION: 2.43.0
108:      - name: Verify no e2e test seam
147:          (cd dist && sha256sum * > SHA256SUMS)
53:RUN GOBIN=/usr/local/bin go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.40.0
60:            -t "aculan-deb-test:${ver}" . ; then
66:    if ! docker run --rm "aculan-deb-test:${ver}"; then
2
1
e2e/flat/Dockerfile:21:FROM golang:1.27-bookworm AS build
e2e/deb/Dockerfile:41:FROM golang:1.27-bookworm AS build
e2e/live/Dockerfile:20:FROM golang:1.27-bookworm AS build
e2e/Dockerfile.cache:7:FROM golang:1.27-alpine AS build
3:go 1.25.0
14:	github.com/cloudflare/circl v1.6.2 // indirect
20:	golang.org/x/net v0.53.0 // indirect
22:	golang.org/x/text v0.36.0 // indirect
no govulncheck in .github
no .github/dependabot.yml
go1.27.3 go1.26.10
```

(The order of two-file greps' lines may differ. go.dev's list moves with Go's patch releases: what matters is that 1.25 is not on it. `aculan-deb-test:` is matched without `${ver}`, so the pattern means the same to every grep.) A line that differs is a stop: correct the entry that cites it.

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
Design follow-ups that are not decisions yet are listed in [the
flat-repositories spec](superpowers/specs/2026-09-27-flat-repositories-design.md),
§10.

## Before the 1.1.0 release candidate

Steps only the maintainer can take, since they need a push or a tag (delete
this section once 1.1.0-rc1 is tagged):

1. **Open a pull request from `feature/flat-repos` to `main`; its CI run
   must be green in full** (raised 2026-10-26, the H2 plan's For H3 item
   7). CI runs on pull requests and on pushes to `main` only (`ci.yaml`'s
   `on:`), so pushing the branch alone runs nothing. It is the first run
   on GitHub's runners of the `e2e-flat` matrix (its `unsigned` boolean,
   the derived timeouts, the `timeout -s INT` wrapper), of the `test`
   job's `e2ehooks seam` step and its binary guard, of e2e/deb's
   eager-adoption fix and its upgrade phase, and of Go 1.27 with
   golangci-lint v2.14.0. Re-run its `e2e-flat` matrix a few times: every
   time so far comes from a 12-core machine, and public runners have 4
   vCPUs. Watch the `debian:11` row's build time (`archive.debian.org` is
   its slowest input); `REACH`, the bound that depends most on runner
   speed (the `${POLL_TOOK}s after the publish` lines of `pr8-table`,
   `frozen-date`, `anchorless`, `two-suites` and `mirror-mode`: 4–8 s
   locally, against 22 s); and the `test` job's `internal/freshness` time
   against its `-timeout 10m` (307 s on 4 local CPUs under `-race`).
2. **The first CI run's job times against the timeouts** (the H2 plan's
   For H3 item 8). A short job over 10 minutes, or a long-wait job over
   25: raise its `timeout-minutes` with the measured reason, and its
   `E2E_STEP_MINUTES` with it (3 minutes under the timeout). Expected:
   short jobs 5–8 minutes; long-wait jobs 8–12, and 16–21 when an
   unsigned record waits for its third retry (about one CI run in four).
3. **`make e2e-live`** on the tree you tag (`e2e/live/README.md`; add its
   row to that file's *Results*): the H3 plan ran it during planning and
   again on its final tree.
4. **Tag `1.1.0-rc1`**, on `main` once the pull request is merged or on
   the branch: the release notes' links name the tag, not a branch, so
   they work either way. The release workflow's `Verify no e2e test seam`
   step runs first on the tag: check it there.
5. **The release notes** (`docs/release-notes/1.1.0.md`): paste them into
   the GitHub release. Their links name the `1.1.0` tag; for a release
   candidate, paste them with that tag replaced by the candidate's:
   `sed 's|/blob/1\.1\.0/|/blob/1.1.0-rc1/|g' docs/release-notes/1.1.0.md`.

## Open

- **`govulncheck`, dependabot and the module bumps** (raised 2026-09-08,
  the security review's finding H-3; its report is not in git: the main
  checkout's `docs/security-review-findings.md`). The review asks for
  `govulncheck` and dependabot in CI, and for `golang.org/x/net`,
  `golang.org/x/text` and `github.com/cloudflare/circl` at their latest
  versions (`go.mod` pins v0.53.0, v0.36.0 and v1.6.2). Go 1.27
  (*Decided*, below) brings the standard library's fixes, not these.
  None of the workflows runs `govulncheck`, and there is no
  `.github/dependabot.yml`. Recommendation: decide before the RC whether
  1.1.0 takes the module bumps (`go get` of the three at `@latest`, then
  the full gate); add a `govulncheck` step to CI either way.
- **Two WARNs that repeat at every adoption become INFO** (raised
  2026-10-25, the H1 final ops review M-10): `source_parse_failed` for a
  `Sources` file whose every path was refused, and
  `adoption_member_skipped` with `reason=arch_not_in_allowlist`, one per
  member `adoption.architectures` filters out. Both are on 1.0.x already,
  so changing their level is a behaviour change; the 1.1.0 release notes
  tell operators who page on WARN to exclude them and say the level
  change is open. Recommendation: INFO in the next minor release, the
  counters (`acu_adoption_members_skipped_total`,
  `acu_adoption_sources_parsed_total`) as the signal.
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
- **A `concurrency` group in `ci.yaml`** (raised 2026-10-26, the H2 final
  review): `ci.yaml` has no `concurrency:` key, so a superseded
  pull-request push keeps all its jobs running, the two long-wait
  `e2e-flat` rows included. Recommendation:
  `group: ci-${{ github.event.pull_request.number || github.ref }}` with
  `cancel-in-progress` for pull requests only, so `main` is never
  cancelled.
- **actionlint in CI** (raised 2026-10-26, the H2 final review): a
  10-second job, after fixing the one finding it reports today,
  shellcheck's SC2035 in `release.yaml`'s *Stage release artifacts* step
  (`sha256sum * > SHA256SUMS`; `sha256sum -- *` keeps the names).
  Recommendation: add it.
- **One nfpm version** (raised 2026-10-26, the H2 final review):
  `e2e/deb/Dockerfile` packages with nfpm 2.40.0 (`nfpm@v2.40.0`), the
  release with 2.43.0 (`release.yaml`'s `NFPM_VERSION`), while `ci.yaml`
  calls `deb-test` the check that the release artifact works.
  Recommendation: move e2e/deb to 2.43.0.
- **e2e/deb's fixed image name** (raised 2026-10-26, the H2 final
  review): `e2e/deb/run.sh` tags and runs `aculan-deb-test:<ver>`, so two
  runs on one host retag each other's image. Recommendation: an
  `E2E_DEB_NAME`, validated as `E2E_FLAT_NAME` is; CI is unaffected (one
  VM per job).
- **The `timeout -s INT` wrapper's two gaps** (raised 2026-10-26, the H2
  final fix wave): CI's `e2e-flat` step runs
  `timeout -s INT -k 60 "${E2E_STEP_MINUTES}m" bash e2e/flat/run.sh`;
  locally `docker run` relayed the test's last output in 6 of 7 trials,
  and a command stuck for good never reaches the test's INT trap.
  Recommendation: `e2e/flat/run.sh` falls back to `docker logs` when a
  run is interrupted; an in-container watchdog only if a stuck command is
  ever seen.
- **Simulations near their budget** (raised 2026-10-25, the H1 final
  review's parked item 17): each simulation must finish in under 4.5 s
  under `-race` (ruling PE-8). On 2026-10-26 (a 12-core machine, load
  near 2) the slowest were `TestSimRetry_LongRunDateRulesAndClockSteps`
  4.28–5.27 s, `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds`
  4.04–4.32 s, `TestSimScheduler_StallingHostWithAdoptionContention`
  4.14–4.23 s, `TestSimCapacity_UpgradeBurstSameHost` 4.12 s and
  `TestServeHTTP_MintedSuitesAtScale` 3.97–4.02 s (3.92–4.07 s quiet);
  the H3 plan's final task records the final tree's. Recommendation:
  coarsen one, every assertion kept, as phase H1 did the 500-suite
  simulation, if it crosses 4.5 s on a quiet machine or makes CI flaky.

## Decided

- **The Go toolchain: Go 1.27** (the maintainer, 2026-10-26/27; raised
  2026-09-08 by the security review's finding H-3 and 2026-10-25 by the
  H2 plan's For H3 item 11). Go supports a release until two newer ones
  exist, so Go 1.25 has had no fix since 1.27.0. CI, the release
  workflow and the four e2e Docker builds use Go 1.27; `go.mod` keeps
  `go 1.25.0`, the language version and GODEBUG defaults the code was
  tested with; CI's golangci-lint moved from v2.12.2, which cannot read
  Go 1.27's export data, to v2.14.0. Done in the H3 plan's Task 5.
- **The package upgrade restarts a running daemon** (the maintainer,
  2026-10-27, for 1.1.0; raised 2026-09-08, the security review's finding
  H-2). `packaging/scripts/postinstall.sh` runs `deb-systemd-invoke
  try-restart` on an upgrade, as `dh_installsystemd --no-start
  --restart-after-upgrade` would: a stopped daemon stays stopped, a fresh
  install starts nothing, and `policy-rc.d` is honoured. Done in the H3
  plan's Task 4, pinned by e2e/deb's phase 5.
- **R-H5 (GD-5) stands: an adopted unsigned flat suite whose `Release`
  disappears keeps its snapshot** (the maintainer, 2026-10-26; raised
  2026-10-25). It shows "refused: anchor_absent" and logs
  `adoption_refused_persistently` until `POST /forget` turns it into a
  Release-less suite. Evidence: the flat-repositories spec, §10 item 12;
  the H1 plan's ruling R-H5; `docs/configuration.md`, *Flat
  repositories*, the end of the paragraph on repositories that publish no
  `Release`. Why: it is rare, the kept snapshot keeps serving when an
  upstream dies outright, and an automatic drop would be a new state
  transition with trust and serving consequences.
- **The cache's writer stays fail-fast on a panic** (the maintainer,
  2026-10-26; raised 2026-10-25, the H1 final review's parked item 16).
  `internal/cache`'s writer goroutine (`(*Cache).writer`,
  `internal/cache/cache.go`) has no `recover`: a panic inside a write
  operation ends the process, and systemd restarts it. Why: a panic
  mid-transaction leaves the one write connection in an unknown state,
  and a restart rolls the transaction back. (The freshness, GC and
  request-handling goroutines recover theirs.) Against, for the record: a
  panic that persisted data triggers repeats at every start (a crash
  loop, as the 2026-09 clearsign panic was); the alternative is a recover
  that rolls back, fails that one write and reopens the connection.
  Still to do: an `AIDEV-NOTE` at the writer that records this (phase H3
  changed no production code).
- **R-H6 stands: every prose message the flat-repositories branch added
  is a snake_case event** (the maintainer, 2026-10-27): all 43 were
  renamed in phase H1, and none was ever released.
- **The release notes' credit for PR #8** (the maintainer, 2026-10-26/27):
  "Thanks to Venkat (@venkatamutyala), whose PR #8 identified the
  flat-repository problem; the flat-repository support in this release
  is based on it."
- **The 1.1.0 release notes live in the tree**,
  `docs/release-notes/1.1.0.md`, pasted into the GitHub release;
  `release.yaml` is unchanged (the H3 controller's ruling PH3-1,
  revisitable).
- **This file is where decisions that outlive a phase live** (the H3
  controller's ruling PH3-2).
- **`make e2e-live` runs before 1.1.0, and before later releases that
  change the request path, adoption or vouching** (the H3 controller's
  ruling PH3-9, from that plan's review V-M-15).
````

- [ ] **Step 3: Spec revision 13**

Run (from the repository root):

```bash
python3 - <<'EOF'
#!/usr/bin/env python3
"""H3 Task 9's spec edits (revision 13), run from the repository root; each
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

**Live-upstream smoke** (`e2e/live/`, `make e2e-live`; manual, before 1.1.0
and before later releases that change the request path, adoption or
vouching, never in CI): a release-built daemon, with adoption and strict mode
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
     """H2: `e2e/flat/`; H3: documentation (README, the strict-mode, unsigned and captive-portal docs; the config file was complete, `TestDefaultConfigFileDocumentsEveryKey`), the 1.1.0 release notes, the live-upstream smoke (`e2e/live`), the H1 test carries, the package upgrade's restart of a running daemon (the security review's H-2) and Go 1.27 (the maintainer's decisions), `docs/open-decisions.md`, final full-diff review. |"""),
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
"""H3 Task 9's overview edits: every carry-forward marked *H3's* is marked
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
    ("the `adopt_unsigned` threat notes, beside the flat forms H documents. ", "*Done in H3 (Task 7).*"),
    ("(*Suite reaping* under `[gc]`, *Forgetting a suite* under `[admin]`). ", "*Done in H3 (Task 7).*"),
    ("an existing cache* and *Suite reaping*, says so; the Task 11 review). ", "*Done in H3 (Task 8; the README too, Task 7).*"),
    ("*Flat repositories*) — beside H's other README items. ", "*Done in H3 (Task 7).*"),
    ("proxy send `Host: 127.0.0.1` (the maintainer's decision, 2026-10-24). ", "*Done in H3 (Task 8; the README's example, Task 7).*"),
    ("`https` and for a `host` holding `://` (it answered 409). ", "*Done in H3 (Task 8; the README's example, Task 7).*"),
    ("no error: `suite_seed_rejected`, and the file is still cached. ", "*Done in H3 (Task 8).*"),
    ("a panic in GC no longer stops the daemon, at startup or in a tick. ", "*Done in H3 (Task 8).*"),
    ("msg=\"freshness_check\"` or `| logfmt | msg=\"freshness_check\"`. ", "*Done in H3 (Task 8).*"),
    ("new events and metrics since 1.0.x (phases A–H1), not only H1's. ",
     "*Done in H3 (Task 8: computed from the code at 1.0.2 and HEAD — 76 events, a test-binary one, 21 metrics; C11's list missed nine metrics).*"),
    ("and capture or silence the default logger in the metrics tests. ", "*Done in H3 (Task 1: one `TestMain` per package).*"),
    ("fixes all 17 callers. Failure path only; it never masks a pass. ",
     "*Done in H3 (Task 2: the 17 tests whose only drain was their end — the twelve readiness waits', `reaper_wiring_test.go`'s second test, `main_test.go`'s three graceful-shutdown tests and `TestServe_ReturnsListenError` — start the daemon with `serveUntilCleanup`; every other daemon start drained on its own already, in a cleanup or in `waitServing`).*"),
    ("as the signal; the release notes may warn operators who page on WARN. ",
     "*Done in H3 (Task 8 warns; the level change is open in `docs/open-decisions.md`, Task 9).*"),
    ("coarsening candidate if it crosses it, as C-38 was (parked item 17). ",
     "*Listed in `docs/open-decisions.md` (H3 Task 9); Task 10 records the final measurement.*"),
    ("zero `unvouched_deb_refused`. Record the results in the release notes. ",
     "*Done in H3 (Task 3, `e2e/live`; the results in the release notes, Task 8).*"),
    ("queued; strict mode now refuses undeclared `.deb`s on flat-only hosts. ", "*Done in H3 (Task 6).*"),
]
OTHER = [
    ("""| H3. Docs and release | to write | §8 (README), release notes, the strict-mode docs (`docs/configuration.md`), the live-upstream smoke, final full-diff review; inputs: the *For H3* sections of the H1 and H2 plans, and the carry-forwards below marked *H3's* | H1, H2 | to write after H2 |""",
     """| H3. Docs and release | `…-H3-release.md` | §8 (README), release notes, the strict-mode docs (`docs/configuration.md`), the live-upstream smoke, final full-diff review; inputs: the *For H3* sections of the H1 and H2 plans, and the carry-forwards below marked *H3's* | H1, H2 | planned (Task 10 stamps it done) |"""),
    ("""**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 12).""",
     """**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 13)."""),
    ("""- H: PR #8's end-to-end table in `e2e/flat`""",
     """- H3: `e2e/live/` (`make e2e-live`, `E2E_LIVE_NAME`; it sources `e2e/flat/lib.sh`), `docs/release-notes/1.1.0.md`, `docs/open-decisions.md`; the package's restart of a running daemon on upgrade (`packaging/scripts/postinstall.sh`; e2e/deb's phase 5 and its `UPGRADE_FROM`); the test helper `serveUntilCleanup` (`cmd/apt-cacher-ultra/serve_until_cleanup_test.go`) and the `TestMain`s of `internal/fetch` and `internal/metrics`.
- H: PR #8's end-to-end table in `e2e/flat`"""),
    ("""**Decisions (spec-level; one awaits the maintainer's confirmation: GD-5):**
""",
     """**Decisions (spec-level):**

The decisions that outlive the branch — the maintainer's of 2026-10-26/27 (Go 1.27; the package upgrade's restart, the security review's H-2; R-H5 and the cache writer's fail-fast on a panic confirmed; R-H6 confirmed) and the open ones (`govulncheck` and the module bumps, the post-1.1.0 carries of H2's For H3 list) — are in `docs/open-decisions.md` (phase H3, Task 9), the maintainer's list.
"""),
    ("""- *Decided for 1.1.0 by ruling R-H5 (phase H1), for the maintainer's confirmation: keep the behaviour; revisit if seen in the field.*""",
     """- *Decided for 1.1.0 by ruling R-H5 (phase H1), confirmed by the maintainer on 2026-10-26: keep the behaviour; revisit if seen in the field.*"""),
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

Expected: `overview: 16 carries marked done, 5 other edits` (the fifth: GD-5's entry, which the maintainer confirmed on 2026-10-26). The script stops, changing nothing, when an old text does not occur exactly once or a carry is left marked *H3's*.

- [ ] **Step 5: Check the three files**

Run:

```bash
grep -c "\*H3's\.\*" docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
grep -c 'Done in H3 (Task' docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
grep -c 'revision 13, 2026-10-26' docs/superpowers/specs/2026-09-27-flat-repositories-design.md
grep -c '^\*\*Live-upstream smoke\*\*' docs/superpowers/specs/2026-09-27-flat-repositories-design.md
awk '/^## Open$/,/^## Decided$/' docs/open-decisions.md | grep -c '^- \*\*'
awk '/^## Decided$/,0' docs/open-decisions.md | grep -c '^- \*\*'
T=$(mktemp -d)
cat > "$T/check_links.py" <<'EOF'
#!/usr/bin/env python3
"""Check the repository links of Markdown files (phase H3): each link to a
file of this repository — relative, or https://github.com/linsomniac/
apt-cacher-ultra/blob/main/<path> or …/blob/1.1.0/<path> (the release
notes link the release's tag: ruling PH3-6) — names an existing file of
this tree, and each #anchor names a heading of that file (GitHub's slug:
lower case, every character but letters, digits, spaces, hyphens and
underscores dropped, spaces made hyphens). A link to this repository's
blob/ under any other ref is broken. Links with a title
([x](path "title")) and reference-style links ([x]: path) are not checked;
these documents have none. Usage: check_links.py FILE.md...; run from the
repository root. Prints each link checked and exits 1 on a broken one."""
import pathlib
import re
import sys

BLOB = re.compile(r"https://github\.com/linsomniac/apt-cacher-ultra/blob/([^/]+)/")
REFS = ("main", "1.1.0")


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
        blob = BLOB.match(target)
        if blob:
            rel = target[blob.end():]
            base = pathlib.Path(".")
        elif re.match(r"[a-z]+:", target):
            continue  # another site: not checked
        else:
            rel = target
            base = src.parent
        path, _, anchor = rel.partition("#")
        f = (base / path) if path else src
        ok = (not blob or blob.group(1) in REFS) and f.is_file() and (not anchor or anchor in slugs(f))
        bad += not ok
        print(f"{'ok  ' if ok else 'BAD '} {name}: {target}")
sys.exit(1 if bad else 0)
EOF
python3 "$T/check_links.py" docs/open-decisions.md | grep -c '^ok'; python3 "$T/check_links.py" docs/open-decisions.md | grep '^BAD' || echo "no broken link"
```

Expected: `0`, `15`, `1`, `1`, `9` (the *Open* entries) and `9` (the *Decided* ones), then `1` (the one Markdown link, to the spec) and `no broken link`. (Of the 16 carries 15 read *Done in H3 (Task …)*; the 16th, the simulation near its budget, reads *Listed in `docs/open-decisions.md` (H3 Task 9); Task 10 records the final measurement.*)

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
branch: the steps before the RC that need a push or a tag, what is
open (govulncheck and the module bumps, the post-1.1.0 carries), each
with its evidence and a recommendation, and what he decided on
2026-10-26/27 (Go 1.27, the restart on upgrade, R-H5 and the writer's
fail-fast with its alternative, R-H6, the credit), with the
controller's rulings on the notes' home and this file's.

The overview marks every carry-forward that was H3's done, with its task.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 10: The release-candidate dry run, the full gate and the final multi-lens review

The last task, run by the executing controller: a local dry run of the 1.1.0-rc1 build (ruling R-H3-7: the tag is the maintainer's), the full gate on the final tree — everything CI runs, and the race suite and the `e2ehooks` tests under Go 1.27 too (ruling PH3-7) — and the final review of the whole branch, `main..HEAD`, by eight reviewers on Opus at maximum effort (ruling PH3-4: Fable's credits are exhausted), its findings fixed test-first or recorded, and the gate run again.

**Files:**
- None for Steps 1–5. The review's fixes are commits of their own (Step 7), each with the test that fails without it. `docs/open-decisions.md` takes what the review defers (Step 9); the release notes and `e2e/live/README.md` take the final tree's live smoke (Step 9); the overview's H3 row is stamped done (Step 9).

**Interfaces:**
- Consumes: every earlier task; `docs/release-notes/1.1.0.md`'s checkers (Task 8 Step 4, with `$D` from its Step 1 — always rebuilt here, Steps 1 and 4 of Task 8, from the tree this task checks: a reused one is the tree as Task 8 left it); `check_links.py` (Task 7 Step 8); the doc pre-flights (Task 6 Step 1, Task 7 Step 1, Task 8 Step 2).
- Produces: the review reports (outside the tree, in `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-review/`), the fix commits, the stamped overview, the final report.

- [ ] **Step 1: The release-candidate dry run (local; no tag)**

Run (from the repository root):

```bash
grep -rnE '"1\.[0-9]+\.[0-9]+(-rc[0-9]+)?"' --include='*.go' cmd internal | grep -v _test.go || echo "no version string in the source"
make build VERSION=1.1.0-rc1 > /dev/null && ./build/apt-cacher-ultra -version
make deb VERSION=1.1.0-rc1 DEB_VERSION=1.1.0~rc1 2>&1 | tail -1
if grep -q ACU_E2E_ build/apt-cacher-ultra; then echo "SEAM IN THE BINARY"; else echo "no e2e seam in the binary"; fi
go version -m build/apt-cacher-ultra | grep -c e2ehooks
nix-shell -p dpkg apt --run 'bash e2e/apt-repo/test.sh' 2>&1 | grep -E '^(PASS|FAIL|SKIP)'
```

Expected (planning run, with `GOFLAGS=-buildvcs=false` outside a git checkout):

```
no version string in the source
1.1.0-rc1
created package: build/apt-cacher-ultra_1.1.0~rc1_amd64.deb
no e2e seam in the binary
0
PASS test-build-repo (unsigned)
PASS test-build-repo (signed)
PASS test-select-tags
```

`make build` and the release workflow take the version from the tag (`git describe --tags`, `-X main.Version`), `release.yaml` turns `1.1.0-rc1` into the Debian version `1.1.0~rc1` and marks the release a prerelease, and the apt-repository publisher skips prereleases (`test-select-tags` drops its fixture's `0.10.4-rc2`): an RC reaches the GitHub release page, never `linsomniac.github.io`'s apt repository. The apt-repo tests need `dpkg-deb` and `apt-ftparchive`, which `nix-shell -p dpkg apt` provides on NixOS (elsewhere: `dpkg-dev` and `apt-utils`); without them two of the three print `SKIP`, and a SKIP is not a pass (review V-I2: CI's `apt-repo` job installs them). `make build` here is the local toolchain's (go1.26.8); CI and the release build with Go 1.27 (Task 5). Remove `build/` afterwards (`make clean`); nothing else changed (`git status --short` is empty).

- [ ] **Step 2: The gate, part 1: the module**

On a quiet machine (no other build or test running), run (from the repository root; `G` holds the gate's outputs, for the final report and reviewer R8):

```bash
G=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-gate; rm -rf "$G"; mkdir -p "$G"
go test -race -count=1 ./... 2>&1 | tee "$G/race.txt"           # 1
golangci-lint run ./... 2>&1 | tee "$G/lint.txt"                 # 2
gofmt -l .                                                       # 3
go test -count=1 -v -run 'TestEveryEventIsDocumented|TestSweptEventNames|TestLogMessagesAreSnakeCase|TestLogMethodsAreOnlyCalledDirectly' ./internal/observability/   # 4
go test -race -p 1 -count=3 -v -run '^(TestSim[A-Za-z0-9_]*|TestDoD_[A-Za-z0-9_]*|TestServeHTTP_Minted[A-Za-z0-9_]*AtScale|TestLedger_[A-Za-z0-9_]*|TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds)$' ./internal/flipgate/ ./internal/freshness/ ./internal/handler/ ./internal/simtest/ > "$G/sims.txt" 2>&1; grep -E '^--- ' "$G/sims.txt" | sort -t'(' -k2 -rn | head -12; grep -c '^--- FAIL' "$G/sims.txt"; nproc   # 5
go test -race -count=20 -run 'TestPhase2Chaos_AdoptionFlipUnderConcurrency$|TestPhase2DetachedChaos_AdoptionFlipUnderConcurrency$' ./internal/handler/ 2>&1 | tee "$G/chaos.txt"   # 6
shellcheck -x e2e/flat/*.sh e2e/live/*.sh e2e/deb/*.sh packaging/scripts/*.sh   # 8
docker run --rm --network none -v "$(pwd)":/repo -w /repo rhysd/actionlint:1.7.12 -no-color -oneline .github/workflows/ci.yaml .github/workflows/release.yaml .github/workflows/pages.yaml 2>&1 | tee "$G/actionlint.txt"   # 9
go vet ./...                                                     # 10
go vet -tags e2ehooks ./cmd/apt-cacher-ultra/ && go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra/ && golangci-lint run --build-tags e2ehooks ./cmd/...   # 11
golangci-lint --version                                          # 12
docker run --rm -e GOFLAGS=-buildvcs=false -v "$(pwd)":/src -w /src golang:1.27-bookworm bash -c 'go version && go test -race -count=1 -timeout 10m ./... && go test -tags e2ehooks -count=1 ./cmd/apt-cacher-ultra/' 2>&1 | tee "$G/race-go1.27.txt"   # 13
```

Expected:

1. every package `ok`, no `FAIL` (21 packages);
2. `0 issues.`;
3. exactly the five drift files of the Global Constraints;
4. four `--- PASS` lines;
5. every line `PASS` and under 4.5 s (ruling PE-8), then `0` from the FAIL count (review E-M-11: a fast `--- FAIL` in one of the three runs would sort out of the twelve slowest), then the core count;
6. `ok` (the two chaos tests, 20 runs each);
7. the migrations check below: `migrations: 6 released, 7 now; changed: none`;
8. no output;
9. one line, the known `.github/workflows/release.yaml:126:9: shellcheck reported issue in this script: SC2035:info:21:23: …` (in `docs/open-decisions.md`), and exit status 1 for it alone;
10. no output (CI's `test` job runs it);
11. `ok` for the package, then `0 issues.` (CI's `test` job runs the first two; H2's gate ran the third);
12. the local version (planning: `golangci-lint has version 2.14.0 built with go1.27.2`), recorded in the final report beside CI's pin, v2.14.0 since Task 5: the two must match;
13. `go version go1.27.3 linux/amd64` (or a later 1.27), every package `ok` and the tagged package `ok` (CI's and the release's toolchain; planning, Task 5 Step 3: 377 s in all).

Item 7, the migrations check (no released migration may change: the overview's first Global Constraint; it compares each entry of `migrations` at 1.0.2 with HEAD's, comments aside):

```bash
G=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-gate
git show 1.0.2:internal/cache/schema.go > "$G/schema-1.0.2.go"
cat > "$G/migrations_unchanged.py" <<'EOF'
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
python3 "$G/migrations_unchanged.py" "$G/schema-1.0.2.go" internal/cache/schema.go
```

Planning results (2026-10-26, the first draft's scratch tree with its Tasks 1–7 applied): items 2, 3, 4, 7 and 8 as expected; item 5's slowest on a machine with a background load average near 2 (12 cores): `TestSimRetry_LongRunDateRulesAndClockSteps` 4.28–5.27 s, `TestCheck_BothSignedFormsFailingUnderJitterStayWithinTheirBounds` 4.04–4.32 s, `TestSimScheduler_StallingHostWithAdoptionContention` 4.14–4.23 s, `TestSimCapacity_UpgradeBurstSameHost` 4.12 s, `TestServeHTTP_MintedSuitesAtScale` 3.97–4.02 s, `TestDoD_NinetyDaysOfReapingWithRestarts` 3.84–4.05 s (H1's quiet-machine replay had them at 3.4–4.0 s). A line over 4.5 s means a busy machine first: rerun that test alone with `-count=5` on a quiet machine; an over-budget time that repeats there fails the gate, and the fix is a coarsening that keeps every assertion (as H1's C-38 and R-H7), test-first. Record the three slowest and `nproc` in the final report, and update `docs/open-decisions.md`'s *Simulations near their budget* entry with them (Step 9). Item 1: 21 packages `ok` in 307 s (load average 2.6); item 6: `ok` in 152 s; item 9 as expected. The revision's planning (2026-10-27, 559fd7f with the revised Tasks 1–9 applied): items 10 and 11 as expected (the exec review had them `ok` and `0 issues.` on its tree), item 12 `golangci-lint has version 2.14.0 built with go1.27.2`, item 13 as Task 5 Step 3.

- [ ] **Step 3: The gate, part 2: the documents**

Rebuild the inventories from the final tree first: Task 8 Step 1 (it deletes and recreates `$D`, so its HEAD side is the tree under review), then Task 8 Step 4 in full (it rewrites `check_notes.py` and `check_links.py` there and runs both): a stale `$D` would compare the notes with the tree as Task 8 left it, so an event a fix adds would pass unseen (review E-I2). Then run the link checker on every document this branch changed:

```bash
D=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-notes
python3 "$D/check_links.py" README.md docs/configuration.md docs/log-fields.md docs/release-notes/1.1.0.md docs/open-decisions.md e2e/flat/README.md e2e/live/README.md | grep '^BAD' || echo "no broken link"
```

Expected: Task 8 Step 4's nine `ok` lines, its link count and `no broken link`, then `no broken link` here. (`docs/admin-ui-spec.md` is left out: its `[SPEC5.md](../SPEC5.md)` link was broken on 1.0.2 already — that file was never in the repository — and nothing in H3 touches it. `e2e/flat/README.md` and `e2e/live/README.md` have no Markdown link today, review E-M-7; they stay in the list so that one added later is checked.)

- [ ] **Step 4: The gate, part 3: the end-to-end suites**

Run, one after another (they share the machine's Docker; about 55 minutes in all):

```bash
G=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-gate
COMPOSE_PROJECT_NAME=acu-h3-e2e make e2e 2>&1 | tee "$G/e2e.log" | grep -E '^\[e2e\] (PASS|FAIL)'
make e2e-flat 2>&1 | tee "$G/e2e-flat.log" | grep -E '^\[e2e-flat\] '
make deb-test 2>&1 | tee "$G/deb-test.log" | grep -E '^\[deb-test\] (phase 5 \(|ubuntu:[0-9.]+ (PASS|FAIL)|OVERALL)'
make e2e-live 2>&1 | tee "$G/e2e-live.log" | grep -E '^\[e2e-live\] PASS|FAIL'
```

Expected: `[e2e] PASS (cache HITs observed: N)` (CI's `e2e` job, the compose test, which no phase's gate had run before: reviews E-I3 and V-I2; its own project name keeps a concurrent checkout's stack out of its `docker compose down -v`, and its last line is the stack's teardown, hence the grep); `[e2e-flat] OVERALL PASS (debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04)` with each image's build and test times; for each Ubuntu image `[deb-test] phase 5 (maintainer scripts under systemd) PASS`, `[deb-test] OVERALL PASS` and `[deb-test] ubuntu:<ver> PASS`, then `[deb-test] OVERALL PASS (24.04 26.04)`; `[e2e-live] PASS (…)`. Planning (2026-10-27, 559fd7f with the revised Tasks 1–9 applied, so every image built with Go 1.27): `[e2e] PASS (cache HITs observed: 1)`; `e2e-flat` passed on all six images — tests of 419, 137, 142, 134, 137 and 336 s, the first and last waiting for the unsigned confirmation (run a few images at a time); `deb-test` passed on both images, phase 5 included; the live smoke passed (smoke 241 s, adoptions 220 s, build 18 s). The first draft's planning (2026-10-26, Go 1.25): `e2e-flat` tests of 382, 139, 143, 138, 136 and 444 s, about 24 minutes in all with cached builds. A live smoke that fails for the vendor's reason is fixed in its values (`e2e/live/README.md`, *Maintenance*) and rerun; one that fails for the cache's is a finding (ruling R-H2-4), reported to the maintainer before anything else.

- [ ] **Step 5: The review's inputs**

The branch's diff is large — at e65f67d, `git diff --no-ext-diff --numstat main..HEAD` counts 320 files, +159,612/−3,090 lines, 87,657 of them the phase plans (design note 6) — so each lens reads the areas it owns, not the whole diff. Run (from the repository root):

```bash
R=/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-review; rm -rf "$R"; mkdir -p "$R"
cat > "$R/inputs.sh" <<'EOF'
#!/bin/bash
# Phase H3, Task 10: the final review's input diffs (main..HEAD), one per
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
    "$F/reaper.go" "$F/forget.go" "$F/first_adoption.go" internal/cache/suite_rows.go \
    internal/handler/suite_rows.go internal/cache/marker_sweep.go internal/cache/signed_seen.go \
    internal/config/config.go internal/gc cmd/apt-cacher-ultra/main.go packaging/scripts "$X"
d 3-security.diff internal/admin internal/gpg internal/aptpath internal/fetch "$F/unsigned.go" \
    "$F/current_form.go" "$F/first_adoption.go" internal/cache/signed_seen.go internal/cache/adoption.go \
    internal/handler/handler.go internal/handler/suite_rows.go internal/proxy/classify.go \
    cmd/apt-cacher-ultra/e2ehooks.go cmd/apt-cacher-ultra/e2ehooks_off.go .github/workflows "$X"
d 4-docs.diff README.md docs/configuration.md docs/log-fields.md docs/admin-ui-spec.md packaging \
    e2e/flat/README.md e2e/live/README.md docs/release-notes docs/open-decisions.md
d 5-tests-ci.diff internal/simtest internal/observability e2e .github Makefile .dockerignore
d 5-tests-go.diff '*_test.go'
EOF
bash "$R/inputs.sh" "$R"
```

Expected (planning, at 559fd7f, before H3's own changes, which add about 2,500 lines, most of them to `2-upgrade.diff` (the packaging scripts), `4-docs.diff` and `5-tests-ci.diff`): `1a-freshness-checks.diff` 6,089 lines, `1b-freshness-adoption.diff` 5,536, `1c-cache-gc.diff` 5,995, `1d-handler-admin-rest.diff` 7,341, `2-upgrade.diff` 6,665, `3-security.diff` 7,564, `4-docs.diff` 2,171, `5-tests-ci.diff` 4,917, `5-tests-go.diff` 51,675, and no `not in a list` line (every production file of `internal/freshness` is in one of the two freshness inputs). `2-upgrade.diff` holds the first hours after an upgrade too — first adoption, suite-row onboarding, the signed-seen markers and their sweep (review V-I5) — and the package's maintainer scripts; `3-security.diff` holds the vouching lookup (`internal/cache/adoption.go`) and the metadata-or-package classification (`internal/proxy/classify.go`, ruling PG-2; review V-M-12). Each reviewer also reads the files themselves, at HEAD, for context.

- [ ] **Step 6: Dispatch the eight reviewers**

One reviewer per row, all at once, each on Opus at maximum effort (ruling PH3-4), each a fresh context with this brief: the common part, then its row.

*Common brief.* You review `main..HEAD` of `feature/flat-repos` in `/home/sean/projects/apt-cacher-ultra/.claude/worktrees/flat-repos` — the flat-repositories work (phases A–H3: suite identity for flat repositories, eager first adoption, unsigned and Release-less forms, retry discipline, the flip barrier, suite reaping, the H1 sweep, e2e/flat, e2e/live, the package's restart on upgrade, Go 1.27, the docs) — before a 1.1.0 release candidate. The maintainer's goal: "a reliable, robust, and low maintenance system". Read the spec (`docs/superpowers/specs/2026-09-27-flat-repositories-design.md`, revision 13) and the overview (`docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`: Global Constraints, deviations, rulings by name); your input diffs are in `/tmp/claude-1000/-home-sean-projects-apt-cacher-ultra/99c125aa-f038-423f-88d6-3cb913c207d4/scratchpad/h3-review/`. Report findings only: change no file in the worktree; to run or mutate code, work in a scratch copy under that same scratchpad (`git archive -o <scratchpad>/<you>.tar HEAD`, then `tar -x` it into an empty directory). For each: severity (**Critical**: data loss, a trust bypass, wrong bytes served, a crash loop, an upgrade that breaks an install; **Important**: a real bug or a doc that would lead an operator to harm; **Minor**: robustness or clarity), `file:line`, the code or text quoted, why it is wrong, and how to show it — the failing test you would write, or a reproduction. Check each claim against the code before you report it; say whether it predates the branch (`git show 1.0.2:<path>`). Do not re-raise what a ruling or a declined deferred minor already settled (H1's disposition table, `docs/open-decisions.md`'s *Decided*) unless the code contradicts the ruling. Rank them; at most 15. `git diff` goes through difftastic here: pass `--no-ext-diff`.

| Reviewer | Lens | Inputs | Look for |
|---|---|---|---|
| R1 | Correctness and concurrency: checks and scheduling | `1a-freshness-checks.diff` | a `sync.Mutex` held across a wait (Global Constraints: block on channels and timers only); a goroutine without its panic recovery (`grep -n 'go func' internal/freshness/*.go`); context and shutdown ordering; the scheduler's queues, slots and backoff (spec C4, C7: the latency bound, healthy → never-checked → failing); wall-clock versus monotonic time (`wallclock` for stamps, `time.Since` for durations); unbounded memory or goroutines; an I/O fault taken as evidence; with R4, C10's strict-mode wait (`inflight.go` and the handler that waits on it: read each other's files) |
| R2 | Correctness and concurrency: adoption and lifecycle | `1b-freshness-adoption.diff` | the adoption's steps and its commit (spec C5, C8); never-downgrade at both points; the flip barrier's bounds; the reaper and `/forget` (spec C9: compare-and-delete, budgets, clock steps); the legacy trust audit's resumability; parsing of upstream input (`Release`, `Packages`, `Sources`, dates) for panics and unbounded work; with R4, C8 end to end (`flip_barrier.go` here, `internal/flipgate` and the handler's anchor hook there: read each other's files) |
| R3 | Correctness and concurrency: the cache and GC | `1c-cache-gc.diff` | SQL: atomic transactions, refcounts balanced, no SQLite clock function (`TestNoSQLiteClockFunctionsInProductionCode`), indexes the queries need; the single writer goroutine (no pool I/O or foreign code on it: ruling PD-5); GC's sub-jobs and their deadlines; suite-row creation and deletes (spec C3, C9) |
| R4 | Correctness and concurrency: the handler, the flip gate, admin and the rest | `1d-handler-admin-rest.diff` | the request path (spec C1–C3, C6, C8, C10): canonicalisation, the strict-mode decision, the flip gate's holds and waiters, the miss path's single operation; the admin endpoints; `cmd/apt-cacher-ultra/main.go`'s startup and shutdown ordering; C8 with R2 and C10 with R1 (above) |
| R5 | Upgrade and data safety | `2-upgrade.diff`, `docs/configuration.md` *Upgrading an existing cache*, `docs/release-notes/1.1.0.md` *Before you upgrade* | the v6 → v7 migration on a large cache (time, locks, rollback; released migrations are never altered: Step 2 row 7); the trust audit's refusal-to-start conditions and what it deletes; what the reaper, GC and cap eviction delete, and when; config defaults and validation (a 1.0.x config must load); disk use after the upgrade; the first hours after the upgrade (the overview's Review Focus 1 and 6): the dormant backfill and eager first adoption (the interest window in `schema.go`, `first_adoption.go`: no burst of dormant suites, the reserved slot), legacy onboarding of `dists/` and flat anchors a 1.0.x binary cached (`handler/suite_rows.go`, `cache/suite_rows.go`: no row-less legacy suite, markers written), and every path that deletes a signed-seen marker (`marker_sweep.go`, the deletes' `PF-10`/`PF-12` upserts); the package's restart on upgrade (`packaging/scripts/postinstall.sh`, e2e/deb's phase 5: a 1.0.x install upgraded with apt starts the migration and the audit at once); whether *Before you upgrade* is complete and right |
| R6 | Security | `3-security.diff`, the 2026-09-08 security review (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`, untracked, in the main checkout), the G-1 commits (`27f30b5..38c5448`) | for each finding of that review the branch touches: fixed, unchanged, or made worse; trust never inferred from "adopted" (Global Constraints); the inline verification (decodable clearsign only); signed-seen markers and never-downgrade; which declared hash vouches a flat `.deb` under strict mode (`DeclaredHashesForPath`, `flatSplits`); what is metadata and what is a package (`classify.go`); the unsigned threat model as documented; strict mode's fail-open and fail-closed choices; SSRF (allowlist, deny ranges, redirects); the admin guard (header, `Sec-Fetch-Site`, `Host`); the bounds on bodies, waiters, suites and memory; the `e2ehooks` seam never in a release binary; attacker-controlled values in log fields |
| R7 | Operator docs against behaviour | `4-docs.diff` and the code | every default, number, event, field, status code, metric and procedure in `README.md`, `docs/configuration.md`, `docs/log-fields.md`, `docs/admin-ui-spec.md`, `packaging/config/config.toml.default`, `docs/release-notes/1.1.0.md`, `docs/open-decisions.md`, `e2e/flat/README.md` and `e2e/live/README.md` matches the code (grep it); nothing describes behaviour the branch removed (the H2 plan's body still describes a residual allowance, a "second retry" and a "vacuous 6 %" — no operator doc may: H2 parked item 8); an operator can follow each procedure as written |
| R8 | Tests and CI | `5-tests-ci.diff`; `5-tests-go.diff` searched, not read through (51,675 lines); Steps 2–4's outputs (in `$G`) | a test that cannot fail (spot-check three high-value tests by mutating the code they pin); `time.Sleep` standing in for a condition; `t.Parallel()` with `wallclock.SetOffsetForTest`; `t.Skip` without a reason; timing bounds that a 4-vCPU runner breaks; the simulations' 4.5 s budget; CI's matrix, timeouts and guards (`ACU_E2E_`, the release workflow's test step); e2e/flat's, e2e/deb's and e2e/live's bounds, diagnosability and maintenance |

- [ ] **Step 7: Triage, and the fix wave**

The controller reads the eight reports and rules on each finding (a ruling per finding, recorded in the final report):

- **Critical and Important:** fixed now, each test-first — the test that fails without the fix (its failure line recorded), the fix, the test passing, a mutant of the fix that fails it — one commit per fix or per closely related group, its message naming the reviewer and the finding. A Critical finding is reported to the maintainer at once, before the fix wave goes on. A fix that changes behaviour a document states changes that document in the same commit (review V-M-2).
- **Minor:** fixed now when the fix is small, local and testable; otherwise recorded in `docs/open-decisions.md` (Step 9) with its evidence and a recommendation.
- **Declined:** only with a reason that cites the code, a ruling or the spec; the reviewer who raised it sees the reason in the re-review.

Then one re-review per lens that had a finding fixed, by a fresh reviewer of the same lens, on the fix commits' diff, with the findings and rulings: "approve" or new findings, which go through this step again, at most twice per lens; a finding still open after the second re-review is ruled by the controller and, if Critical or Important, reported to the maintainer with its evidence instead of fixed blind (review V-M-1).

- [ ] **Step 8: The gate again**

After the last fix: Step 2 in full (items 10–13 included); Step 3 (Task 8 Steps 1 and 4 rerun on the tree after the last fix); the doc pre-flights, Task 6 Step 1, Task 7 Step 1 and Task 8 Step 2 (line numbers may move; a missing line or a changed count is a stop: review V-M-2); and from Step 4 every suite whose inputs a fix touched (`make e2e-flat` and `make e2e` after a change to the daemon, `e2e/flat/` or the e2e images; `make deb-test` after a change to packaging or `e2e/deb/`; `make e2e-live` after any daemon change). Expected: as in Steps 2–4 and the three pre-flights.

- [ ] **Step 9: Record, stamp, commit**

First the deferred items and the final tree's results, in a commit of their own:

- Append to `docs/open-decisions.md`'s *Open* list each finding the review deferred (with its reviewer, its evidence in the repository and a recommendation), and update its *Simulations near their budget* entry with Step 2's numbers.
- Replace the release notes' *Testing* results (the `make e2e-live` paragraph: its date, versions and outcome) with the final tree's run from Step 4 (or Step 8's rerun), and rerun Task 8 Step 4's checkers (review V-M-5); the maintainer's run on the tagged tree replaces them again if it differs.
- Append the final tree's run as a row of `e2e/live/README.md`'s *Results*.

Run `python3 "$D/check_links.py" docs/open-decisions.md docs/release-notes/1.1.0.md e2e/live/README.md | grep '^BAD' || echo "no broken link"` (`D` as in Step 3; expected `no broken link`), then commit:

```bash
git add docs/open-decisions.md docs/release-notes/1.1.0.md e2e/live/README.md
git commit -F - <<'EOF'
Phase H3: the final review's deferred items and the final tree's results

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

Then the stamp, alone (review V-M-3; as H1 and H2 were stamped): in the overview, the H3 row's status `planned (Task 10 stamps it done)` becomes `done (commit <the previous commit's short hash>)`, as every earlier phase's row reads:

```bash
git add docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md
git commit -F - <<'EOF'
Phase H3: mark it done in the overview

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

- [ ] **Step 10: The report to the maintainer**

Report, briefly: the gate's results (Steps 2–4, with the three slowest simulations, `nproc`, the local golangci-lint version beside CI's pin, and the Go 1.27 run); the review's findings by severity and their rulings and commits; the live smoke's result on the final tree; and what is his — `docs/open-decisions.md`'s *Before the 1.1.0 release candidate* list (the pull request, the CI runs, the tag and the paste: nothing goes to GitHub without him) and its *Open* entries, `govulncheck` and the module bumps first.

---

## Found while planning (for the controller)

- **No daemon issue.** Every live-smoke run passed (2026-10-26 twice, 2026-10-27 twice: with the revised Task 3, and on the tree with Tasks 1–9 applied and Go 1.27): all five repositories, eight suites, adopted in their upstreams' forms; every update clean; both error-grep controls caught; six packages installed vouched; strict mode on all five hosts. With Tasks 1–9 applied (Go 1.27 everywhere), `e2e/flat` passed on all six images, `e2e/deb` on both (phase 5 included), the compose test, the race suite under go1.26.8 and go1.27.3, and the apt-repo tests with their tools (Task 10's planning).
- **Carries that were incomplete or inexact:** the toolchain touches four e2e Dockerfiles, not two (`e2e/Dockerfile.cache`, and `e2e/live/Dockerfile` once Task 3 lands), and moving to Go 1.27 also needs CI's golangci-lint moved from v2.12.2 to v2.14.0 (design note 5, Task 5); spec C11's metric list misses nine of the 21 new metrics (design note 1; revision 13 says so); the overview's "17 callers" of `waitForDaemonReady` are 18 waits, 12 of which belong to tests whose only drain was their end, as do `main_test.go`'s two shutdown tests (Task 2, ruling PH3-11); the README's `/reconcile` example lacked its 400 and 413 answers (Task 7); H2's open question on the captive portal's `Release.gpg` is answered — an adopted unsigned snapshot answers it 404 — and now pinned (Task 6).
- **Found while revising (2026-10-27):**
  - E-M-13's single-fetch `all_adopted` runs (Task 3), but its premise does not hold: in the strict mutant's failure output 56 of the 80 log-tail lines are still `admin_request` (51 before), because the poll fetches the status once a second through the ~220 s wait for the scheduler's tick; the per-suite fetches were never the bulk. A `show_logs` in `smoke.sh` that drops `admin_request` lines would fix the tail; not done (outside the ruling), the comment says only what the change does.
  - `make e2e`'s last line is the compose stack's teardown (`Network … Removed`), not `[e2e] PASS (…)`: the reviews' `| tail -1` would show the teardown, so Task 10 greps for the PASS line.
  - `main_test.go`'s two `TestServe_GracefulShutdown_*` tests have the same defect as parked item 14 (`defer cancel()`, `serveDone` read last, `t.Fatalf` before it) but no readiness wait, so they were outside the carry's callers; ruling PH3-11 folds them into Task 2 (`replaced 14 blocks`).
  - A comment line in `lib.sh` that begins with the word `shellcheck` is read as a directive (the smoke's lint then fails, SC1094): the `AIDEV-NOTE` Task 3 adds there is worded around it, and says so.
  - Deleting the postinst's restart line leaves an empty `then`, a syntax error dpkg reports at the first install; Task 4's mutant replaces it with `:`.
  - `preremove.sh` stops and disables with `systemctl` directly, not `deb-systemd-invoke` (debhelper's prerm honours `policy-rc.d`); harmless here, since the `/run/systemd/system` guard skips both where no systemd runs, and the ruling asked only for its comment.
  - A reinstall after a remove (not a purge) passes the removed version to the postinst, as for debhelper's packages: nothing restarts, and no install message prints although the remove disabled the unit (the postinst's `AIDEV-NOTE`).
  - `grep -n '"aculan-deb-test:${ver}"'` matches nothing under ugrep (a shell where `grep` is ugrep reads `$` differently): Task 9 matches `aculan-deb-test:`.
- **Outside H3's scope, recorded:** the 2026-09-08 security review's report the brief cites, `docs/security-review-findings.md`, is untracked: it exists only in the main checkout (`/home/sean/projects/apt-cacher-ultra/docs/security-review-findings.md`), so the final review reads it there (Task 10, R6); its findings H-3 (the toolchain, done in Task 5; `govulncheck` and the module bumps, open) and H-2 (the restart, done in Task 4) are this plan's. `docs/admin-ui-spec.md` links `../SPEC5.md`, a file the repository never had (broken on 1.0.2 already; Task 10 Step 3 leaves it out). pkg.jenkins.io now signs with `jenkins.io-2026.key`; the smoke uses it.

## Self-review (done while writing, and again for the revision)

- **Coverage of the brief, the carries and the reviews.** Spec §8's README items: Task 7; its `docs/configuration.md` items: Task 6 (each grepped by its pre-flight). The strict-mode, unsigned, captive-portal, binary-signature, `flip_held_ms` and upgrade-restart docs: Task 6. Spec revision 13 (H2 deviations 1 and 2 as built, design notes 5 and 7, the full-grep deviation, the live smoke): Task 9. The release notes with every *H3's* carry — the upgrade items, the six removed messages, the log-query note, the repeating WARNs, the computed inventory, the additive keys, the alert recipes, the captive portal, the harness, the smoke's results — and what the reviews added (adoption enabled, who strict mode refuses, `[gc]`, the caps, the restart, Go 1.27): Task 8. The H1 test carries: Tasks 1 and 2 (and the simulation near its budget: Task 10's measurement, `docs/open-decisions.md`). The live smoke: Task 3. The restart on upgrade (H-2): Task 4. The toolchain (D2): Task 5. The durable decisions: Task 9. Version and packaging: ruling R-H3-7, Task 10 Step 1. The final review: Task 10. The overview's sixteen *H3's* carries are each marked done by Task 9's script, which stops if one is left. Every finding and ruling of the reviews, with the plan lines it changed and its verification: the revision log (`planH3-review/revise.md`).
- **Replayed, not transcribed.** For the revision, a fresh copy of 559fd7f took Tasks 1–9 from this plan's text by script — each whole file written from its block, each `old` required to occur exactly once, each script block run as written (the `git archive`/`git show` lines aside: no git in the scratch copy) — and every expected output in Tasks 1–9 is what that tree printed. The red steps and mutants of Tasks 2 and 4 failed as stated (Task 4's on both Ubuntu images), the live smoke and its strict mutant ran (Task 3), Task 5's module and linter steps ran under Go 1.27, and Task 10's Steps 1–4 ran on the result (Step 1 with `GOFLAGS=-buildvcs=false`, as its expected output says), its Step 5 against this worktree's HEAD.
- **Not replayed:** Task 10's Steps 6–10 (the controller's review, fixes and stamp), and Task 6's portal mutant and Task 1's measurements, whose text is the first draft's (the exec review ran both).
