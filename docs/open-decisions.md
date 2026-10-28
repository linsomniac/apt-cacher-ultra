# Open decisions

Decisions that outlive the work that raised them, each the maintainer's
(Sean Reifschneider). An entry says what is open, its evidence, the
recommendation and when it was raised. When the maintainer rules, the
entry moves to *Decided* with the date and the ruling, and whoever does
the work cites it.
The flat-repositories branch (phases A–H, 2026-09 to 2026-10) seeded this
file; the decisions it made along the way are in its overview's decisions
block (`docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md`).
Design follow-ups that are not decisions yet are listed in [the
flat-repositories spec](superpowers/specs/2026-09-27-flat-repositories-design.md),
§10.

## Before the 1.1.0 release candidate

Steps only the maintainer can take, since they need a push or a tag (delete
this section once 1.1.0-rc1 is tagged). Before the first, decide whether
1.1.0 takes the module bumps (*Open*, the `govulncheck` entry): a bump is a
commit, and the pull request's CI run should include it.

1. **Open a pull request from `feature/flat-repos` to `main`; its CI run
   must be green in full** (raised 2026-10-26, the H2 plan's For H3 item
   7). CI runs on pull requests and on pushes to `main` only (`ci.yaml`'s
   `on:`), so pushing the branch alone runs nothing. It is the first run
   on GitHub's runners of the `e2e-flat` matrix (its `unsigned` boolean,
   the derived timeouts, the `timeout -s INT` wrapper), of the `test`
   job's `e2ehooks seam` step and its binary guard, of e2e/deb's
   eager-adoption fix and its upgrade phase, of Go 1.27 with
   golangci-lint v2.14.0, and of the `lint` job's two later steps (the
   `e2ehooks` lint and `make shellcheck`, which runs the pinned
   `koalaman/shellcheck:v0.11.0` image). Re-run its `e2e-flat` matrix a few times: every
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
4. **Run the release candidate once on a copy of a production 1.0.x
   cache** (raised 2026-10-27, the H3 final review R5 #5): every test
   builds its 1.0.x database by hand, so only a real `cache.db` and
   `pool/` show what the trust audit makes of real vendors' `InRelease`
   files, and its quarantines cannot be undone. Copy `cache.db` (with
   its `-wal`) with 1.0.x stopped, and `pool/`; point the candidate at
   the copy with the production configuration, never at the production
   pool (the audit and GC delete pool files). Read `schema migrated` and
   its time; `legacy_trust_audit_completed`, expecting `quarantined=0`
   and `deferred=0` on a healthy cache (explain any quarantine's reason
   before tagging); the startup `gc_run_complete`'s `duration_ms` (the
   listeners wait for it); and, an hour later,
   `suite_reaper_run_complete`, the `suite_reaped` reasons and
   `gc_retired_run_complete`.
5. **Tag `1.1.0-rc1`**, on `main` once the pull request is merged or on
   the branch: the release notes' links name the tag, not a branch, so
   they work either way. The release workflow's `Verify no e2e test seam`
   step runs first on the tag: check it there.
6. **The release notes** (`docs/release-notes/1.1.0.md`): paste them into
   the GitHub release. Their links name the `1.1.0` tag; for a release
   candidate, paste them with that tag replaced by the candidate's:
   `sed 's|/blob/1\.1\.0/|/blob/1.1.0-rc1/|g' docs/release-notes/1.1.0.md`.
   The file is wrapped at about 72 columns: check the release editor's
   *Preview*, and if GitHub breaks the lines where the file does, paste
   the paragraphs unwrapped instead (lists, tables and headings kept):
   `sed 's|/blob/1\.1\.0/|/blob/1.1.0-rc1/|g' docs/release-notes/1.1.0.md | perl -0777 -pe 's/([^\n|])\n *+(?![-|#\n]|\z)/$1 /g'`
   (for 1.1.0 itself, drop the `sed` and pass the file name to `perl`:
   `perl -0777 -pe '…' docs/release-notes/1.1.0.md`).

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
  the full gate); add a `govulncheck` step to CI either way. The review
  also asks for a `toolchain` line in `go.mod` (there is none: a builder
  whose Go is older than 1.25 downloads exactly go1.25.0, which gets no
  fixes). Recommendation: leave it unset: it needs a bump at every Go
  patch release, and it makes any older local Go download a toolchain,
  while the release artifacts are built with the pinned Go 1.27 anyway.
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
  repository publishes a `Release` without an `InRelease`, after which apt
  prints a signature error until the cache adopts it
  (`docs/configuration.md`, *Flat repositories*; pinned by
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
  `TestServeHTTP_MintedSuitesAtScale` 3.97–4.02 s (3.92–4.07 s quiet).
  The H3 plan's final gate (12 cores, three runs each): on 2026-10-27,
  with the load at 2.9 after the race suite,
  `TestSimRetry_LongRunDateRulesAndClockSteps` took 4.79 s once and
  3.87–4.02 s in five runs alone, `TestServeHTTP_MintedSuitesAtScale`
  4.03–4.22 s and `TestSimCapacity_UpgradeBurst` 4.12 s; on the final
  tree (2026-10-28, load 1.8) the slowest were
  `TestServeHTTP_MintedSuitesAtScale` 3.93–4.05 s,
  `TestDoD_NinetyDaysOfReapingWithRestarts` 3.79–3.89 s and
  `TestSimRetry_LongRunDateRulesAndClockSteps` 3.64–3.82 s. Recommendation:
  coarsen one, every assertion kept, as phase H1 did the 500-suite
  simulation, if it crosses 4.5 s on a quiet machine or makes CI flaky.
- **A refused client check waits behind an old failure record** (raised
  2026-10-27, the H3 final review R1 #3). A client's check that finds
  new content but no free adoption slot leaves the suite due, and the
  next scheduler tick queues it, except when the suite already holds a
  failure record that is not yet due: then the new content waits until
  the suite goes stale (`periodic_refresh`), the record's retry, or a
  later client check that wins a slot (spec C4 states this bound;
  `TestSuiteDue`'s "pending change with a record waits for its retry").
  Trust and what is served are unaffected; only adoption latency under
  slot contention. Recommendation: leave it; if it is ever reported, an
  in-memory mark set by a queued client check (like `absentAnswered`)
  makes the suite due at the next tick.
- **GC's sub-job B deletes displaced snapshots in one transaction**
  (raised 2026-10-27, the H3 final review R3 #4; unchanged since 1.0.x).
  `RunSnapshotGCBatch` (`internal/cache/gc.go`) deletes up to
  `gc.snapshot_batch_size` snapshots' `package_hash` rows at once: six
  snapshots of 70,000 rows held the writer 4.7 s in a probe, so a full
  batch holds every write (a miss's store, a flip's commit) about 8 s.
  Sub-job C was chunked for this reason (10,000 rows a transaction).
  Recommendation: have B retire displaced snapshots beyond
  `keep_displaced` and let C delete them in chunks.
- **A GC pass that fails at every tick starves the passes after it**
  (raised 2026-10-27, the H3 final review R3 #5; unchanged since 1.0.x
  for the url_path pass and sub-jobs A and B). `runTick`
  (`internal/gc/gc.go`) moves the pass rotation on after a panic but not
  after an error, so a pass that errors each time (a fault limited to its
  tables) keeps the blob pass from running; the only sign is WARN
  `gc_tick_failed` per tick. Recommendation: advance the rotation after
  an error too, as after a panic.
- **The inline verifier accepts three shapes apt rejects** (raised
  2026-09-08, the security review's theme one: VC-2, C-4, VC-1; confirmed
  2026-10-27 by the H3 final review R6 #2). `VerifyInline`
  (`internal/gpg/verifier.go`) accepts a valid `InRelease` followed by
  blank lines, a forged CRC line, and an extra bad signature from the
  same trusted key; its comments now say what is enforced. None makes the
  cache serve unsigned content as signed. Recommendation: tighten each in
  a later release, one test per shape, after checking what apt 2.2–3.2
  do with it.
- **No limit on an upstream body's size** (raised 2026-09-08, the
  security review's A-2 and VA-3; raised again 2026-10-27, the H3 final
  review R6 #3). An unsigned `Release` declares its own members' sizes,
  and the size check runs after the download; with `adopt_unsigned = true`
  (the default) a client that fetches an unsigned repository through the
  cache makes it adopt that repository's members, so this reach is new in
  1.1.0 (`docs/configuration.md`, the unsigned threat notes, says bytes
  are not bounded). Recommendation:
  cap each member's download at its declared size (plus a margin) and
  turn off transparent gzip decoding for members, in the next release.
- **CI keeps e2e/flat's full logs** (raised 2026-10-27, the H3 final
  review R8 #3). A failed `e2e-flat` job leaves only the harness's
  80-line tail (now without `admin_request` lines): `e2e/flat/run.sh`
  runs the container with `--rm` and the job uploads nothing.
  Recommendation: copy the daemon and apt logs out of the container on
  failure and upload them with `actions/upload-artifact`.
- **`internal/freshness` under `-race` takes about 5 minutes on 4 CPUs**
  (raised 2026-10-27, the H3 final review R8 I-1). The overview's Global
  Constraint asked for each package under 5 minutes under `-race`; the
  package took 297 s on 4 CPUs and 261–280 s on 12. `make test-race` and
  CI now allow 10 minutes. Recommendation: keep the 10-minute bound; split
  the package's simulations into their own test binary if it nears it.
- **Packaging and test loose ends** (raised 2026-10-27, the H3 execution
  and its reviews; each harmless today):
  `packaging/scripts/preremove.sh` stops and disables with `systemctl`
  directly, so it ignores `policy-rc.d` (debhelper's prerm uses
  `deb-systemd-invoke`); `dpkg --purge` warns that `/lib/systemd/system`
  is not empty; a masked unit gets the install hint, whose
  `systemctl enable --now` fails on it; the maintainer scripts ignore
  `DPKG_ROOT` for their directories, so `dpkg --root` installs are not
  supported; a fresh install prints adduser's note that the home directory
  cannot be accessed (the preinst names it before the postinst creates
  it); `TestServe_GracefulShutdown_DrainsInflight` can hang in
  `defer upstream.Close()` on one failure path; the list of daemon starts
  in `serve_until_cleanup_test.go`'s `AIDEV-NOTE` is checked by the H3
  plan's census, not by a test. Recommendation: fix the first three with
  the next packaging change; the rest when their code is next touched.
- **A vouched `.deb` whose snapshot is retired during its download**
  (raised 2026-10-27, the H3 final review's R6 re-review; unchanged since
  1.0.x). On a strict host, a `.deb` miss that was vouched before its
  download is served unchecked once if its vouching snapshot is retired
  while it downloads.
  Recommendation: in the read after the download, treat a `.deb` that
  was vouched before it and has no row now as a strict refusal, not a
  pass, with a test that retires the snapshot mid-download.

## Decided

- **The Go toolchain: Go 1.27** (the maintainer, 2026-10-26/27; raised
  2026-09-08 by the security review's finding H-3 and 2026-10-26 by the
  H2 final CI review, the H2 plan's For H3 item 11). Go supports a
  release until two newer ones exist, so Go 1.25 has had no fix since
  1.27.0. CI, the release
  workflow and the four e2e Docker builds use Go 1.27; `go.mod` keeps
  `go 1.25.0`, the language version and GODEBUG defaults the code was
  tested with; CI's golangci-lint moved from v2.12.2, which cannot read
  Go 1.27's export data, to v2.14.0. Done in the H3 plan's Task 5.
  For the next bump,
  `grep -rn "go-version: '\|FROM golang:\|golang:1\.\|version: v2\." .github e2e`
  finds every pin: the seven Go pins (`ci.yaml`'s two `go-version` lines,
  `release.yaml`'s, and the `FROM golang:` lines of `e2e/flat`, `e2e/deb`,
  `e2e/live` and `e2e/Dockerfile.cache`), CI's two golangci-lint
  `version:` lines (the plain and the `e2ehooks` lint steps),
  and the image named in `e2e/flat/README.md` and in
  `e2e/Dockerfile.cache`'s header comment. The lint pin is coupled to the
  Go version, so check it at every Go bump: v2.12.2 cannot read Go 1.27's
  export data, v2.14.0 can.
- **The package upgrade restarts a running daemon** (the maintainer,
  2026-10-27, for 1.1.0; raised 2026-09-08, the security review's finding
  H-2). `packaging/scripts/postinstall.sh` runs `deb-systemd-invoke
  try-restart` on an upgrade, as `dh_installsystemd --no-start
  --restart-after-upgrade` would: a stopped daemon stays stopped, a fresh
  install starts nothing, and `policy-rc.d` is honoured. Done in the H3
  plan's Task 4, pinned by e2e/deb's phase 5.
- **R-H5 (GD-5) stands: an adopted unsigned flat suite whose `Release`
  disappears keeps its snapshot** (the maintainer, 2026-10-26; raised
  2026-10-23 as phase G's deferred minor GD-5, and put to the maintainer
  2026-10-24 by the H1 plan's ruling R-H5). It shows "refused:
  anchor_absent" and logs `adoption_refused_persistently` until
  `POST /forget` turns it into a
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
  that rolls back, fails that one write and reopens the connection. An
  `AIDEV-NOTE` at the writer's loop records this decision.
- **R-H6 stands: every prose message the flat-repositories branch added
  is a snake_case event** (the maintainer, 2026-10-27): all 43 were
  renamed in phase H1, and none was ever released. Evidence:
  `TestLogMessagesAreSnakeCase` (`internal/observability/event_names_test.go`)
  fails the build on a new prose message.
- **The release notes' credit for PR #8** (the maintainer, 2026-10-26/27):
  "Thanks to Venkat (@venkatamutyala), whose PR #8 identified the
  flat-repository problem; the flat-repository support in this release
  is based on it." Evidence: `docs/release-notes/1.1.0.md`, *Credits*,
  and `Contributors.md`.
- **The 1.1.0 release notes live in the tree**,
  `docs/release-notes/1.1.0.md`, pasted into the GitHub release;
  `release.yaml` is unchanged (the H3 controller's ruling PH3-1,
  revisitable).
- **This file is where decisions that outlive a phase live** (the H3
  controller's ruling PH3-2).
- **`make e2e-live` runs before 1.1.0, and before later releases that
  change the request path, adoption or vouching** (the H3 controller's
  ruling PH3-9, from that plan's review V-M-15). Evidence:
  `e2e/live/README.md`, its first paragraph.
