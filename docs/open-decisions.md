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
The GC and upstream hardening plan of 2026-10-28 (the hardening plan
below; `docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md`)
worked five *Open* entries of this file for 1.1.0, now under *Decided*.
Its rulings HD-1 to HD-10 are in its *Design notes and rulings*; HD-11
to HD-15 were made while it was carried out; HD-16 to HD-18 (the hybrid
verifier) are in its addendum of 2026-10-29 and HD-19 (format rules and
crypto policy, the signer downgrade guard) in its addendum of
2026-11-03; the controller's rulings R1 to R3 (the key reader, its
Task 9b) and G1 to G6 (the guard, its Task 9d) were made while those
tasks were carried out. The *Decided* entries of its work say what each
decided.

Evidence is cited by what a reader of the repository can reach: a corpus
row (`internal/gpg/testdata/aptshapes/verdicts.txt`), a test, a commit or
a doc section. `PE/` is the hardening plan's planning evidence (its
line 13): the directory
`.superpowers/sdd/2026-10-28-gc-and-upstream-hardening/planning-evidence/`,
with the plan's task reports and ledger beside it, kept on the
maintainer's machine under the git-ignored `.superpowers/` and not
shipped; a citation of `PE/` is planning evidence, not in the tree. The
final review's measurement rows that no tree file holds (the key-file
rows `kr*` of its V lens, for one) are in `PE/final-review/V/rr5/` to
`rr10/` (each round's generator, `meas/` and `allkr*.txt`). Each *Open*
entry states its severity class: R2 (integrity: the cache counts or
adopts what some supported apt treats as revoked, expired, unbound or
bad), R3 (availability: the cache refuses what apt accepts, or a gap that
needs a malformed or unusual operator file; the content is still
authenticated by a trusted key's good signature), or none (the cache
agrees with apt, or the item is operational); the entries before the
hardening plan's (CI, packaging, tests and logs, from `govulncheck` to the
packaging loose ends) are all class none, operational: none touches what
the cache trusts or serves.

## Before the 1.1.0 release candidate

Steps only the maintainer can take, since they need a push or a tag (delete
this section once 1.1.0-rc1 is tagged). More changes go into 1.1.0 before
the candidate; each adds its entry to [Changelog.md](../Changelog.md).

1. **The first CI run of the merged branch must be green in full**
   (raised 2026-10-26, the H2 plan's For H3 item 7). The branch was
   merged into `main` without a pull request (the maintainer,
   2026-10-28), so that run is the one for the first push of `main`
   (CI runs on pull requests and on pushes to `main`: `ci.yaml`'s `on:`).
   It is the first run
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
4. **`make e2e-apt-oracle`** and **`make e2e-apt-oracle-old`** on the
   tree you tag (`e2e/apt-oracle/README.md`; add their row to that file's
   *Results*), after `docker pull` of the eleven images (the README's
   *Running it*: a cached tag is never refreshed, so a run without the
   pull measures the images of 2026-10-29 and 2026-11-05 again and passes
   by construction): the real apt of the six client images, and of the
   five older ones (debian:10, ubuntu:20.04, 18.04, 16.04 and 14.04),
   must still give the verdicts and error classes recorded for the
   verifier's 904 signature-shape fixtures; about 90 and 100 seconds with
   the images cached (87 to 92 s and 98 to 101 s, 2026-11-05). A difference blocks the tag
   until it is recorded and the table test agrees (the README's *When apt
   changes*). The GC
   and upstream hardening plan recorded them (2026-10-29; the older
   images 2026-11-05, its Task 9e). This item outlives the section: the
   oracle runs before every release (*Decided*, below).
5. **Run the release candidate once on a copy of a production 1.0.x
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
   `gc_retired_run_complete`. The copy also runs the new verifier against
   the operator's real repositories, beyond the 81 vendors surveyed: read
   every `adoption_gpg_failed` of the first hour whose reason is
   `malformed_clearsign`, `malformed_signature`, `bad_signature` or
   `crypto_verify_failed`, every `adoption_signer_downgrade`, and the
   start's `keyring_file_skipped`, `keyring_file_truncated` and
   `keyring: parse failed; skipping file` lines, and compare each with
   what the clients' apt does on that repository (the hardening plan's
   final review, D-M10). A snapshot 1.0.x adopted keeps serving whatever
   the new rules make of its anchor (the trust audit does not run the
   verifier); it is the suite's next adoption that the rules decide.
6. **Tag `1.1.0-rc1`** on `main`. The release workflow's
   `Verify no e2e test seam` step runs first on the tag: check it there.
7. **The release notes** (`docs/release-notes/1.1.0.md`): bring them up
   to date with what `Changelog.md` collected since this branch, then
   paste them into the GitHub release, as
   [docs/releasing.md](releasing.md)'s step 6 says (it gives the
   commands that rewrite the links for a release candidate and unwrap the
   paragraphs for GitHub's editor; the per-release steps that outlive this
   section are there). The GC and upstream hardening plan's Task 8 rebuilt
   them from the Changelog on 2026-11-05, main's changes included: the
   verifier and its reasons, the signer downgrade guard and its recourse,
   the six vendors with no strong signer, the SHA-1 refusal, the key
   reader's changes, the HTTP 204 rule, issue #14's GC fix, the
   older-client oracle, the new events and metrics (29 metrics since
   1.0.3), the GC counters' double count and the refusal count, eight. A
   later change to `Changelog.md` before the tag needs its line there
   too.

## Open

- **`govulncheck` and dependabot in CI, and `go 1.26` for the last
  module fixes** (raised 2026-09-08, the security review's finding H-3;
  its report is not in git: the main checkout's
  `docs/security-review-findings.md`). The module bumps are done
  (*Decided*, below). None of the workflows runs `govulncheck`, and
  there is no `.github/dependabot.yml`. `govulncheck` under Go 1.27
  (2026-10-28) finds no vulnerability the code reaches, and 10 in
  required modules that it does not call (7 `golang.org/x/net`, 3
  `golang.org/x/crypto`), fixed only in releases that require
  `go 1.26.0` in `go.mod` (x/net 0.60.0, x/crypto 0.56.0): moving the
  `go` line turns on Go 1.26's GODEBUG defaults (among them
  `urlstrictcolons`, for a cache whose paths hold colons, as
  pkgs.k8s.io's do), so it is a change of its own, with the full gate.
  Class none: a process item; no vulnerability the code reaches.
  Recommendation: add a `govulncheck` step to CI (a failure on a
  vulnerability the code reaches); move to `go 1.26.0` and those
  releases in the next release, after checking each GODEBUG change. The
  review also asks for a `toolchain` line in `go.mod` (there is none: a builder
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
  change is open. Class none: log levels only.
  Recommendation: INFO in the next minor release, the
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
  Class none: fail-safe; harmless until the repository changes form.
  Recommendation: do it after 1.1.0 — the anchors' delete loop takes a
  third name, judged by the existing `Release.gpg` check
  (`aptpath.SniffReleaseGPG`).
- **A `concurrency` group in `ci.yaml`** (raised 2026-10-26, the H2 final
  review): `ci.yaml` has no `concurrency:` key, so a superseded
  pull-request push keeps all its jobs running, the two long-wait
  `e2e-flat` rows included. Class none: CI cost only.
  Recommendation:
  `group: ci-${{ github.event.pull_request.number || github.ref }}` with
  `cancel-in-progress` for pull requests only, so `main` is never
  cancelled.
- **actionlint in CI** (raised 2026-10-26, the H2 final review): a
  10-second job, after fixing the one finding it reports today,
  shellcheck's SC2035 in `release.yaml`'s *Stage release artifacts* step
  (`sha256sum * > SHA256SUMS`; `sha256sum -- *` keeps the names).
  Class none: CI only.
  Recommendation: add it.
- **One nfpm version** (raised 2026-10-26, the H2 final review):
  `e2e/deb/Dockerfile` packages with nfpm 2.40.0 (`nfpm@v2.40.0`), the
  release with 2.43.0 (`release.yaml`'s `NFPM_VERSION`), while `ci.yaml`
  calls `deb-test` the check that the release artifact works.
  Class none: packaging tests only.
  Recommendation: move e2e/deb to 2.43.0.
- **e2e/deb's fixed image name** (raised 2026-10-26, the H2 final
  review): `e2e/deb/run.sh` tags and runs `aculan-deb-test:<ver>`, so two
  runs on one host retag each other's image. Class none: tests only.
  Recommendation: an
  `E2E_DEB_NAME`, validated as `E2E_FLAT_NAME` is; CI is unaffected (one
  VM per job).
- **The `timeout -s INT` wrapper's two gaps** (raised 2026-10-26, the H2
  final fix wave): CI's `e2e-flat` step runs
  `timeout -s INT -k 60 "${E2E_STEP_MINUTES}m" bash e2e/flat/run.sh`;
  locally `docker run` relayed the test's last output in 6 of 7 trials,
  and a command stuck for good never reaches the test's INT trap.
  Class none: CI diagnostics only.
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
  `TestSimRetry_LongRunDateRulesAndClockSteps` 3.64–3.82 s.
  Class none: test timing only.
  Recommendation:
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
  slot contention.
  Class none: adoption latency only; trust and serving unaffected.
  Recommendation: leave it; if it is ever reported, an
  in-memory mark set by a queued client check (like `absentAnswered`)
  makes the suite due at the next tick.
- **CI keeps e2e/flat's full logs** (raised 2026-10-27, the H3 final
  review R8 #3). A failed `e2e-flat` job leaves only the harness's
  80-line tail (now without `admin_request` lines): `e2e/flat/run.sh`
  runs the container with `--rm` and the job uploads nothing.
  Class none: CI diagnostics only.
  Recommendation: copy the daemon and apt logs out of the container on
  failure and upload them with `actions/upload-artifact`.
- **`internal/freshness` under `-race` takes about 5 minutes on 4 CPUs**
  (raised 2026-10-27, the H3 final review R8 I-1). The overview's Global
  Constraint asked for each package under 5 minutes under `-race`; the
  package took 297 s on 4 CPUs and 261–280 s on 12. `make test-race` and
  CI now allow 10 minutes. Class none: test time only.
  Recommendation: keep the 10-minute bound; split
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
  plan's census, not by a test.
  Class none: packaging and tests, each harmless today.
  Recommendation: fix the first three with
  the next packaging change; the rest when their code is next touched.
- **Downloads with no declared size have no byte limit** (raised 2026-10-28,
  the hardening plan's DN-1). Adoption stops a member's download at its
  declared size (*Decided*, below), but a client request's miss, hot
  prefetch's `.deb`s and the handler's re-fetch of a snapshot member whose
  pool file is gone (`runSnapshotMemberFetch`,
  `internal/handler/handler.go`) have no size to stop at: their
  `fetch.Target` sets no `MaxBytes`, and they are bounded only by a
  response's `Content-Length`, when it sends one, which the body must
  match, and by `upstream.total_timeout` (5 minutes by default), which
  alone bounds a chunked body. A cap needs a size per
  file, which `snapshot_member` and `package_hash` lack: a schema change.
  Class none: a resource bound (time and disk), not a trust question; what
  is served is still checked against its hash where one is declared.
  Recommendation: keep `Content-Length` and `upstream.total_timeout` as
  their bounds; `runSnapshotMemberFetch` could take a cap from the blob
  row's size when that row survives.
- **`accept_any_signer`'s bypass applies no S1** (raised 2026-10-28, the
  hardening plan's DN-8). Under `adoption.accept_any_signer` with no pin
  matched, the verifier checks an anchor's structure — the hybrid
  verifier's layers T, A and P, the same code as on the trusted path
  (`TestVerifier_BypassRefusesEveryMalformedRow`), so at least one signature
  packet — and verifies no signature, so S1, which refuses a bad signature
  from a key in the host keyring, is not applied: it would be the bypass's
  only signature check (the AIDEV-NOTE in `checkSignatures`'s bypass,
  `internal/gpg/verifier.go`; `docs/configuration.md`, *Trust and signer
  pins*). The cache can then adopt an anchor that a client holding the key
  refuses. Nor does the signer downgrade guard ever fire on the bypass: it
  counts no signer (ruling G1). Class none, by configuration: the operator
  who sets the bypass chooses the clients' verification as the boundary.
  Recommendation: leave it, since the bypass
  makes the clients' own verification the trust boundary, as
  `docs/configuration.md` says.
- **The `Release.gpg` sniff still passes a binary signature** (raised
  2026-10-28, the hardening plan's DN-8). The verifier now refuses a
  `Release.gpg` that does not start with its armor's BEGIN line, a binary
  one included, as every measured apt does, but the sniff the handler and
  the freshness probe share (`aptpath.SniffReleaseGPG`, spec C3;
  `TestSniffReleaseGPG`) still passes a binary signature, or whitespace
  before the armor (it trims spaces, tabs, CRs and LFs; the final
  review's D-M12): such a file still earns its suite a signed-seen marker,
  and the probe still takes a suite without an `InRelease` for a detached
  one, whose adoption the verifier then refuses (`malformed_signature`).
  Both only keep the suite from adopting unsigned, the fail-safe direction,
  and the four vendors in the vendor survey of the hardening plan's security
  review (2026-10-28) that publish a binary `Release.gpg` all publish an
  `InRelease`, which the check prefers. Class none: fail-safe.
  Recommendation: leave it.
- **The cache is stricter than `debian:13`'s apt** (raised 2026-10-28, the
  hardening plan's DN-2 and DN-3; counts of 2026-11-05, Task 9c, recounted
  for the 904-shape corpus by its Task 8). `debian:13`'s apt 3.0.3
  verifies with Sequoia's `sqv` (the image has no gpgv) and accepts 92 of
  the corpus' 904 shapes that every gpgv-based apt refuses, among them a
  forged CRC line, a bad signature from a known key beside a good one and
  an expired key's signature alone (`verdicts.txt`). The cache refuses all
  92 (ruling HD-2: what any supported apt refuses is refused; 22
  `bad_signature`, 55 `malformed_*`, 9 `crypto_verify_failed`, 1
  `no_usable_signature` (`e2_unknownB_good_sha1A`, since gpgv 1.4's
  first-packet rule), 5 `untrusted_signer`), so it does not adopt such an
  anchor — the suite keeps its previous snapshot, if it has one — where a
  `debian:13` client fetching directly accepts it: stricter than that
  client, never looser. The other way round, `debian:13` alone refuses 132
  shapes; the cache refuses 119 of them and accepts 13, each a key only
  sqv's policy refuses (brainpool, secp256k1, SHA-1- or SHA-224-bound,
  bound after the signature, or a weak primary under an Ed25519 subkey:
  `aptShapeKeyPolicy`), since crypto policy is not intersected across
  client generations (ruling HD-19, *Decided*). Class none: stricter than
  one client, never looser than the strictest. Recommendation: keep the
  rule.
- **127 anchor shapes every apt accepts are refused, in four families**
  (raised 2026-10-28, the hardening plan's DN-6 and ruling HD-9, fourteen
  shapes then; recorded as families by its addendum's DN-11 and rulings
  HD-16 and HD-17, Task 9b; counts of 2026-11-05). `aptShapeDeviations`
  (`internal/gpg/aptshape_test.go`) maps each strict deviation — a shape
  every measured apt accepts, the six current images and the five older
  ones, and the cache refuses — to its family, one reason a family, and
  `TestAptShapeDeviations_EveryFamilyHasARow` requires a row in each: **one
  armor block** (6 shapes: the cache reads one block, apt reads every
  one; no surveyed vendor publishes two); **non-canonical armor layout**
  (74: outside layer A's whitelist, `internal/gpg/armor_canon.go` — armor
  lines over 76 characters, blanks or Unicode spaces inside the armor,
  header grammar — or a NUL in the envelope's headers, layer T's T5);
  **non-canonical packet layout** (40: outside layer P's v4 grammar,
  `internal/gpg/sigpacket.go` — bytes left inside a packet, two issuer
  subpackets, a non-minimal MPI, an embedded signature, a critical
  subpacket gpgv 2.2 does not handle); and **unverifiable algorithm** (7:
  a public-key or hash algorithm layer P does not read, SHA-224 and
  SHA3-256 among them). Every one is an edit no surveyed signer makes: of
  the vendor survey's 158 anchors (the verifier entry under *Decided*),
  152 lie inside the grammar, and the six outside it are four binary
  `Release.gpg` files every apt refuses and pkgs.k8s.io v1.30's two
  anchors, whose version 3 packet `debian:13` refuses too; the twelve
  in-tree vendor fixtures (`TestVerifier_AcceptsTheVendorAnchors`) all
  verify.
  Each is refused as `malformed_clearsign` or `malformed_signature` (95
  and 32), and the suite keeps its previous snapshot; no setting lets one
  through (ruling HD-17: widening the grammar is a code change with a
  measured corpus row). DN-6's three go-crypto-decoder deviations and the
  shapes HD-13 and HD-14 had added stopped being strict with the hybrid,
  and Task 9e took out three that gpgv 1.4 refuses
  (`q_good_plus_bigunh_dup`, `v6_i_good_plus_v6unknown`,
  `v6_d_good_plus_v6unknown`). Class R3: availability — the cache refuses
  what every apt accepts, though only an edit no real signer makes.
  Recommendation: keep them; widen the
  grammar for a family only when a real vendor's anchor needs it, with
  that anchor as a vendor fixture.
- **Weak keys (C-3's key half), and the rest of C-1** (raised 2026-09-08,
  the security review's C-3 and C-1; left out of the hardening plan, its
  DN-1; narrowed 2026-10-29 by ruling HD-18 and 2026-11-03 by HD-19).
  C-3's digest half is closed (*Decided*, the verifier entry): S2 never
  counts a SHA-1 signature, so `x3i_sha1_only`, which all six measured apt
  refuse, is refused, and the table test's known-unsafe list
  (`aptShapeKnownUnsafe`) is empty. Its key half stands by design: S2
  counts what the gpgv-based apt count, an RSA-1024, DSA or SHA-1-bound key
  included, where `debian:13`'s sqv or the `APT::Key::Assert-Pubkey-Algo`
  of `ubuntu:24.04` and `26.04` refuse some of them (ruling HD-19: crypto
  policy is not intersected across client generations, or the cache would
  refuse vendors their own older clients read); the signer downgrade guard
  covers the strip of a strong signature beside such a key, not a vendor
  that signs with one alone (the six vendors' entry below). C-1: the `Date`
  rules and `rollback_refused` (spec C5) protect a suite from a replayed
  anchor after its first adoption, but nothing checks `Valid-Until`, and a
  replaying man in the middle can choose a suite's first snapshot (the
  flat-repositories spec, §10 item 5), as it can strip a strong signer
  before the first adoption (the guard's entry below). Class none for the
  key half (the ruled policy: the cache counts what the gpgv-based apt
  count); R2 for C-1 (integrity: a replayed or stripped first snapshot is
  adopted), open since 1.0.x and needing a man in the middle at a suite's
  first adoption. Recommendation: take
  up the spec's §10 item 5 (`Valid-Until` and per-suite identity pinning)
  in a later release; re-measure the key policies (`make e2e-apt-oracle`)
  before each date to come in `internal/gpg/strength.go`'s rules (RSA-2048
  keys on 2030-02-01; the SHA-1 and SHA-224 binding date, 2026-02-01, has
  passed).
- **S1 covers only the keys the cache holds** (raised 2026-10-28, the
  hardening plan's DN-5). S1 refuses an anchor that carries a bad signature
  from a key in the cache's host keyring — any of its keys, a certify-only
  primary or an encryption subkey too, since the hardening plan's Task 9e
  (*Decided*) — and ignores one from a key the
  keyring lacks (gpgv's NO_PUBKEY); a client that trusts such a key still
  refuses the anchor, so the cache adopts what that client refuses.
  `docs/configuration.md` (*Trust and signer pins*) asks the operator to
  install in the cache every key the clients' `signed-by` names. Class
  none: inherent, and the operator's install closes it. Recommendation:
  leave it (inherent: the cache checks the keys it holds).
- **S2's key-ID collision quirk** (raised 2026-10-28, the hardening plan's
  ruling HD-10; unchanged by the hybrid verifier, checked 2026-11-05). S2
  (`verifyAnyTrusted`, `internal/gpg/verifier.go`) verifies a packet whose
  issuer fingerprint is trusted against
  the whole trust set at once, and go-crypto's `VerifyDetachedSignature`
  hashes once and tries each key holding the packet's key ID with that one
  hash, which only the first can pass: a host key that shares the signer's
  key ID and is loaded ahead of it makes S2 refuse a valid anchor
  (`crypto_verify_failed`), as before the hardening plan. S1 tries each
  entity alone
  (`TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature`,
  which notes that S2 still refuses). Class R3: it refuses a valid anchor
  and never adopts a bad one. Recommendation: if it is ever met,
  verify S2's fingerprint path against the single entity that owns the
  issuer fingerprint, with a test.
- **A retired snapshot whose sub-job C chunk fails at every call blocks
  the later ones** (raised 2026-10-28, the hardening plan's DN-7, as
  *Displaced snapshots a periodic tick retired but did not delete wait*;
  narrowed 2026-11-05). The entry's first half, that retired snapshots a
  pass left wait, was wrong in class and premise (the hardening plan's
  final review 2, G2-I2): it assumed a later tick catches up, and under a
  sustained displacement rate none did, so the backlog grew without bound;
  it is decided under *Decided*, issue #14 (the startup pass runs C, and
  the retired run repeats hourly while a backlog lasts). What stays open:
  sub-job C deletes the oldest retired snapshot first
  (`RunRetiredSnapshotGCChunk`, `internal/cache/gc.go`), so one whose chunk
  fails at every call stops the deletion of every later retired snapshot,
  B's displaced ones included, with their rows and the blobs they keep,
  until it is fixed, where a periodic B used to delete them itself whatever
  C did (the hardening plan's final review, G-M1: probed with a trigger
  standing in for a fault confined to one snapshot's rows, the only cause
  the review found; WARN `gc_tick_failed` with `retired snapshot gc
  chunk:` shows it). A failing retired run is not repeated hourly (no
  retry storm); the next periodic tick retries. Class none: disk only,
  since a retired snapshot never serves. Recommendation: leave it; if a
  chunk is seen to fail at every call, let C step past a snapshot whose
  chunk failed for the rest of its pass.
- **Sub-job C's chunk size** (raised 2026-10-28, the hardening plan's
  Task 2; ruling HD-11). Sub-job C deletes 10,000 rows a writer transaction
  (spec C9; `retiredChunkRows`, `internal/cache/gc.go`), and a chunk holds
  every write while it runs: Task 2 measured 120–240 ms a chunk (median
  149–191 ms; no `-race`, a loaded 12-thread laptop, 700,000 `package_hash`
  rows), where the plan had expected under 200 ms and phase F had measured
  about 80 ms. Ruling HD-11 kept 10,000 rows: the aim was that GC never
  holds the writer for seconds, which this meets. Class none: performance.
  Recommendation: re-measure
  on a staging host before deciding; a chunk under 10,000 rows is a change
  to spec C9 (pinned by
  `TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows`).
- **No metric counts a failed GC pass** (raised 2026-10-29, the hardening
  plan's final review, G-M6; unchanged since 1.0.x). A pass that fails at
  every tick never pages: its tick still logs `gc_run_complete` and sets
  `acu_gc_last_run_unixtime`, so the GC alert in the 1.1.0 release notes,
  on that gauge's age, stays quiet, and WARN `gc_tick_failed` is the only
  sign (`docs/log-fields.md`); since the final fix wave's follow-up a
  startup pass in which a pass failed sets the gauge too, so one that fails
  at every start of a daemon that never runs a periodic tick never pages
  either, and the status page's *Last GC* cell, which reads the same
  last-run summary, shows that run with its partial counts where it showed
  "no GC run yet" (the final review's G re-review, RR-M1). Since ruling
  HD-15 the passes after a failed one run (*Decided*,
  below), but the failed pass's own work waits until its fault is fixed.
  Class none: operability. Recommendation: a counter,
  `acu_gc_pass_failures_total{pass}`, and an alert line on it in the
  release notes, in a later release.
- **A vouched `.deb` is stored unchecked when the read after its download
  fails** (raised 2026-10-28, the hardening plan's Task 4 review, Minor 3;
  the posture predates that plan). When the read before a `.deb` miss's
  download found it vouched (one declared hash) and the read after the
  download fails, `runFetch` (`internal/handler/handler.go`) logs WARN
  `package_hash validate lookup failed` and stores and serves the file with
  no hash constraint (`Finalize`), on any host: it fails open, as the hit
  path does, so that a database fault does not refuse every package. The
  hash the first read found is not kept. Class R2, by a deliberate
  fail-open: on a database fault a vouched `.deb` is served without its
  hash check, so that the fault does not refuse every package.
  Recommendation: keep it, and
  finalize with it (`FinalizeExpectingHash`), which refuses only bytes that
  do not match it, so it adds no flood of refusals.
- **What `crypto_verify_failed` still means** (raised 2026-10-29, the
  hardening plan's Task 6 review, Minor 4; the final review's D-M1 and
  D-M6; rewritten 2026-11-05, its Task 9c). The shape refusals that used
  to land here are `malformed_*` since the hybrid verifier (*Decided*): a
  base64 error in the armor (`x4i_b64_invalid_char`,
  `x4d_b64_invalid_char`: layer A's A4), an armor that holds no packet
  (`a_b64_empty_body`: A4, on the bypass too) and a signature over 64 KiB
  (A4). What falls to `ClassifyVerifyErr`'s default now is S2 finding a
  trusted packet and counting none — 64 of the corpus' 904 rows, measured
  2026-11-05 (`PE/task9c/reasons-a7d0d72.tsv`, by
  `PE/task9c/zz_reasons_test.go`, planning evidence, not in the tree; 65
  of 876 then, recounted at 729e666 by the plan's Task 8, where
  `e2_unknownB_good_sha1A` is `no_usable_signature` since gpgv 1.4's
  first-packet rule): a packet sqv does not read (52 rows: a subpacket
  Sequoia's parser refuses, the re-review's A-RC1), a good signature apt
  erases at a later SHA-1 packet by the same key (8: A-RC2), a SHA-1
  packet alone (`x3i_sha1_only`, ruling HD-18), a packet by a known key
  that cannot sign (`us_good_primary_alone`, `us_good_enc_alone`), and an
  expired key's signature alone (`i_expired_only`: "openpgp: key expired",
  in the field the cache's copy of a vendor key whose expiry the vendor
  has since extended); and outside the corpus a revoked key, a key the
  keyring marks (ruling R2), S2's key-ID collision quirk (above) and the
  detached form's empty inputs. The log line's `err` says which; since
  Task 9c the status page's tooltip lists these causes, and
  `ClassifyVerifyErr`'s comment too; for a key the keyring marks, the log
  line names the key and the mark (a revocation, an expiry or a binding
  the cache cannot read), and the operator's action is the vendor's
  current key file (`docs/configuration.md`, *Trust and signer pins*). The
  detached refusals' own text no longer claims an apt verdict: it read
  "Release.gpg is not one ASCII-armored signature block apt accepts" for
  every layer's refusal, the deviations included, and reads "… block in
  the shape the cache accepts" since Task 9c's fix round (the controller's
  ruling R-9c-1; `TestMalformedSentinels_ClaimNoAptVerdict`). Class none:
  reporting only; the verdicts are right. Recommendation: leave the
  reason; if operators meet the expired-key case often, give it a reason
  of its own (`key_expired`), with a `TestClassifyVerifyErr` row.
- **What the hybrid verifier leaves** (raised 2026-10-29, the hardening
  plan's Tasks 6 and 7, as "anchor shapes beyond the corpus are
  unmeasured"; rewritten 2026-11-05, its Task 9c, for the residual risk of
  the approach decision, `PE/approach-decision.md` §6). The table test
  runs every one of the corpus' 904 shapes, none skipped
  (`TestVerifier_FollowsTheAptShapeTable`), against the six current images'
  verdicts and the five older ones', and the cache adopts no shape any
  measured apt refuses but the crypto-policy rows a newest image alone
  refuses (`aptShapeKeyPolicy`, ruling HD-19) and the older images'
  refusals of crypto policy and real signers' layouts (`aptShapeOldExempt`,
  below). What is left, by §6's list: (1) a bug in the port of apt's text
  layer, which `TestAptTextLayer_RefusesExactlyTheSplitRows` holds to
  apt's own refusals on every row; (2) signer-only policy differences
  (dates, key strength, SHA-224 and SHA3 countability), which a vendor
  makes for every client alike (the C-3 entry above); (3) go-crypto and
  gpgv or sqv disagreeing on a known key's valid-looking signature,
  signer-made, out of the cache's hands; (4) gpgv or sqv refusing a
  canonical armor or packet for a reason not yet measured, which the
  oracle's controls and the vendor survey would show as a changed row;
  (5) the handler's `InRelease` sniff (`aptpath.SniffInRelease`), looser
  than apt's splitter, the fail-safe direction (DN-8). And shapes beyond
  the corpus stay unmeasured: until the hybrid every review round found
  more; the final review's adversarial lens measured 586 candidates on the
  hybrid of Task 9b's second fix round and found no gap (its re-review 3).
  One real vendor's shape is outside the corpus (the final review's
  D-M9): pkgs.k8s.io v1.30's `InRelease` and `Release.gpg` carry one
  version 3 signature packet, which gpgv accepts in the five gpgv images
  and `debian:13`'s sqv refuses ("Signature Packet v3 is not considered
  secure since 2021-02-01"); layer P refuses it (`malformed_clearsign`,
  `malformed_signature`: "a version 3 signature (the cache reads version 4
  only)"), as the cache refused it on 1.0.x (`no_usable_signature` there),
  and gpg makes no version 3 signature to build a corpus row from. Class
  R2 for what is unmeasured (a shape the corpus lacks could be adopted
  while some apt refuses it), none for what is measured. Recommendation:
  add each shape a search, a review or a vendor turns up
  to the corpus with its measured row, and then fix the verifier or list
  the shape in its family.
- **A go-crypto upgrade can move the verifier's and the key reader's
  verdicts** (raised 2026-10-29, the hardening plan's rulings HD-13 and
  HD-14; rewritten 2026-11-05, its Task 9c). Since the hybrid verifier
  go-crypto reads no armor and frames no packet: it parses one packet that
  layer P has validated (P5, `packet.Read`) and verifies it (S1 and S2,
  `VerifyDetachedSignature`, and for a key that cannot sign
  `PublicKey.VerifySignature`), so the deviation families no longer
  depend on its decoders. It still reads every key: the key reader hands
  it each key block (`internal/gpg/keyfile.go`, whose AIDEV-NOTEs list what
  v1.5.2 changed and what it skips — v3 signatures, RIPEMD-160, v4
  direct-key expiry), and the downgrade guard reads its self-signature in
  use (`strongSigner`). The bump to v1.5.2 (Task 9b, commit 4fa21be) gave
  identical verdicts on the suite, the corpus and the vendor survey. Class
  none: a process item; an upgrade could move verdicts either way, which
  the gates below would show. Recommendation: at every go-crypto upgrade
  run the table test and the key reader's tests (`go test
  ./internal/gpg`), both oracles (`e2e/apt-oracle/README.md`, *When to run
  it*) and the vendor re-run of
  the hardening plan's Task 8 gate, and re-read the AIDEV-NOTEs that cite
  go-crypto's lines (`grep -n 'go-crypto v1' internal/gpg/*.go`).
- **A copy of a key that go-crypto cannot read is dropped with its marks**
  (raised 2026-11-05, the final review's V lens, its re-review 10:
  V-R10I1; on `main` and 1.0.x too). Class R2, under-strict: the cache
  counts a key every apt that reads its first copy treats as revoked. The
  key
  reader skips a key block that go-crypto reads in neither of its two
  readings, and the loader a file that holds no key go-crypto reads
  (`readKeyBlocks`, `internal/gpg/keyfile.go`; `LoadKeyringWithEmbedded`,
  `internal/gpg/keyring.go`), and the marks of that block or file
  (ruling R2: a revocation or binding gpgv honours) go with it. Alone, that
  copy's key is unknown to the cache, which is harmless; with a second,
  readable copy of the same key in the operator's directories, the cache
  counts the key through it while every apt that reads the first copy
  treats the key as revoked: `kr29_unread_b_signedby_a_too` (copy B holds
  the key's own revocation and a RIPEMD-160 self-certification alone) is
  refused in all six images and adopted by the cache at every commit,
  `main` included (`PE/final-review/V/rr10/`: `mkkr29.py`,
  `meas/kr29.*`, `kr29-cache.txt`; planning evidence, not in the tree).
  Not key-free: it needs two
  copies of one key in the operator's directories, one of them unreadable
  to go-crypto; no real keyring has one (debian-keyring, debian-maintainers
  and the 70 survey files hold no key twice with differing marks: Task 9b's
  fix round 9). Recorded Open by the controller's rule that closed Task 9b
  (only a round's own regressions reopened it). Recommendation: fix it in a
  later release — run `keyBlock.marks` for every key block, read or not,
  and keep a dropped block's or file's marks in the union — with
  `kr29_unread_b_signedby_a_too` as its test.
- **Key files and anchors the cache refuses where every apt accepts them**
  (raised 2026-11-03 to 05, Task 9b's fix rounds 5 to 9 and Task 9e).
  Class R3 for the key-file rows (availability: an unusual operator file
  is refused where apt reads it; ruling R3), R3 for Task 9e's rows that
  every image accepts (the deviations), none for its rows a current image
  refuses. Ruling R3 bounds an S1 or S2 gap that needs a
  malformed or unusual operator key file as availability-class (the
  content is still authenticated by a good signature from a trusted, valid
  key); the rulings of those rounds chose to fail closed, and these rows,
  each `ok` in all six images, are refused by the cache (the final
  review's V re-reviews 5 to 10, `PE/final-review/V/rr5/` to `rr10/`,
  each round's generators, `meas/` and `allkr*.txt`; Task 9b's fix round
  reports 6 to 9, their over-strict tables; planning evidence, not in the
  tree):
  - **legacy (version 2 and 3) signatures, one fail-closed rule** (round
    6): a third party's legacy key or subkey revocation
    (`kr21_tp_v3rev_xin`, `kr25_tp_v3subrev`), a legacy binding older
    than, or as old as, the one kept (`kr25_v3_bind_older`,
    `kr25_v3_bind_tie_before`), a legacy direct-key signature
    (`kr25_v3_directkey`), a legacy back-signature alone
    (`kr25_v3_backsig_alone`);
  - **designated revokers** (round 5; V-R6M1, ruled to stay): a designated
    revocation without the revoker's key in the keyring, which the five
    gpgv-based apt then ignore (`kr19_dr_rev_xout`, `kr21_dr_ctl_xout`), a
    designated subkey revocation (`kr21_dr_subrev_xin`), a designation in a
    direct-key signature that does not verify (`kr21_dr_baddesig_xin`) or
    in a user-ID self-certification (`kr17_dr_keyrev_xin`,
    `kr17_dr_keyrev_xout`), a forged designated revocation
    (`kr20_dr_forged_xin`, `kr20_dr_forged_xout`);
  - **issuers that disagree** (rounds 6 to 8, the issuer principle,
    *Decided*): a verifying self-signature whose issuer subpackets name
    two keys is not the key's own, so it neither supersedes nor extends
    (`kr28_dk_fprmis_noexp`, `kr28_uid_fprmis_newer_noexp`), and
    go-crypto verifies a revocation by the primary key whatever its issuer
    says (`kr22_self_fprother_keyrev`, as on `main`);
  - **the mark union** (round 9): two copies of a key in the keyring
    directories, the one gpgv reads first unmarked
    (`kr28_xcopy_both_a_first`: gpgv reads the first keyblock), and,
    unmeasured, two copies with a past and a future direct-key expiry,
    where gpg's merge takes the newest.
  None touches a real file: debian-keyring (3,209 keys and subkeys),
  debian-maintainers (761) and the 70 survey files (201) count exactly as
  before those rounds (Task 9b's fix round 9). And 322 corpus rows are
  accepted by some older image and refused by the cache (Task 9e, counted
  again at 79e7dd2 by `PE/task9c/split.py` over
  `PE/task9c/reasons-79e7dd2.tsv`, Task 9c's fix round): 127 are the strict
  deviations every image accepts (their entry above), 184 are rows some
  current image refuses, and 11 are rows only an older image refuses,
  which the cache folds in (Task 9e's class (c): T6's
  `i_begin_trailing_ws`, `a_msgbegin_sp_cr` and `q_msgbegin_sp_tab_crcr`;
  S1's every-key rows `t7g5_s_primary`, `us_bad_primary_plus_good`,
  `us_bad_enc_plus_good`, `us_good_plus_bad_enc` and
  `us_d_bad_enc_plus_good`; and P's `q_good_plus_bigunh_dup`,
  `v6_i_good_plus_v6unknown` and `v6_d_good_plus_v6unknown`; each is in
  the 322 because another older image accepts it). By the rule that
  refuses them: layer T (F0 included) 56, layer A 117, layer P 71, S1 15
  and S2 63, where an old image's lenient parser or older crypto reads the
  shape. Strict is safe: the cache stays strict. Recommendation: leave
  them; narrow a rule only if a real key file or anchor meets it, with a
  measured row.
- **Three debian-keyring keys every apt refuses from 2027** (raised
  2026-11-04, Task 9b's fix rounds 8 and 9; exact, not over-strict).
  `440B7638ED88D68D5F351BCFB95B0493F5AB0C47` (from 2027-12-24),
  `D477040C70C2156A5C298549BB7E9101495E6BF7` (2027-01-17) and
  `F75FBFCD771DEB5E9C86050550C3634D3A291CF9` (2027-09-28), Debian
  developers' keys in `/usr/share/keyrings/debian-keyring.gpg`, each carry
  a readable direct-key self-signature with a key expiry, which go-crypto
  v1.5.2 ignores and the key reader honours at the verifier's clock since
  round 8 (commit 60d26f1; `TestVerifier_AReadableDirectKeyExpiryIsHonoured`).
  A direct-key expiry outranks a newer user-ID self-signature in all six
  images (V's `kr28_dk_exp_then_uid_later` and
  `kr28_dk_exp_then_uid_none`: EXPKEYSIG in the five gpgv images, refused
  by sqv), so from those dates every apt, and the cache, refuses an anchor
  by one of these keys unless its owner renews the direct-key signature
  itself (`gpg --quick-set-expire` does). A fourth key,
  `4D14050653A402D73687049D2404C9546E145360`, expired on 2026-04-27 for
  go-crypto too. Class none: the cache agrees with every apt.
  Recommendation: nothing in the cache; a suite signed by
  one of them that turns `crypto_verify_failed` needs the key renewed
  upstream.
- **Six vendors have no strong signer** (raised 2026-11-05, Task 9d; a
  fact, not a defect). ceph (its only self-signature, of 2026-07-03, is
  newer than its signatures, of 2026-03-12), mongodb, newrelic,
  nvidia-cuda, openresty and raspberrypi (SHA-1 self-signatures), both
  forms each: `debian:13`'s sqv refuses all twelve real anchors today
  ("not bound"; ceph's "No binding signature at time …"), `ubuntu:24.04`
  and `26.04` refuse none, and the cache adopts them, since S2 counts what
  the gpgv-based apt count (ruling HD-19). Their suites' current anchors
  have no strong signer, so the downgrade guard stands down for them. The
  survey's 137 anchors the cache accepts were measured in the three
  newest images, and the cache's strong flag equals "all three accept" on
  every one: 125 strong, these 12 not (Task 9d; the vendor re-run of
  2026-11-05 at a7d0d72 gives the same flags, `PE/task9c/strong.txt`;
  mongodb's vendor fixture is
  pinned with none, `TestVerifier_PinsTheVendorAnchorsStrongSigners`).
  `debian:13` clients of these suites fail with or without the cache:
  the vendor's to fix, by binding its key again with SHA-256 (ceph: with
  a self-signature older than its signatures). Class none: the cache
  agrees with the gpgv-based apt, and `debian:13`'s refusal is the
  vendor's to fix. Recommendation: nothing in the cache; the release notes
  can name them.
- **The signer downgrade guard cannot see a strip at a suite's first
  adoption** (raised 2026-11-03, the plan's addendum, HD-19; ruling G6,
  Task 9d). The guard compares a candidate with the suite's current
  anchor, so it stands down when there is none: a suite's first adoption,
  the one after `POST /forget`, and the one after the reaper deletes an
  idle suite, each a first adoption
  (`TestSignerGuard_AdoptsWhatLosesNoStrongSigner`, "first adoption by
  weak"); it also stands down on its own errors, for a current anchor it
  cannot read or verify, as a 1.0.x snapshot's may be (ruling G2,
  `adoption_signer_guard_skipped`). A man in the middle at that moment can
  strip a vendor's strong signature beside a weak one, and the cache
  adopts what `debian:13` clients then refuse, until upstream changes and
  the next adoption brings the strong signer back. Class R2 (integrity,
  for the newest clients): at that moment a strip goes through; open by
  ruling G6. The refusal clears by itself when the vendor signs again with
  a strong key the keyring holds; a vendor that rotated to a new strong
  key the keyring lacks, keeping a weak co-signer, does not clear it (the
  final review 2's F2-I2), and the WARN's `uncounted_signers` then names
  the new key, `not in the trust set`. The recourse for a refusal (ruling
  G4; `adoption_signer_downgrade`'s row in `docs/log-fields.md`;
  `docs/configuration.md`, *Trust and signer pins*), in this order:
  install the vendor's new key, once confirmed out of band (its
  announcement, its site or its key-rotation notice), and restart, which
  keeps the guard on (`TestSignerGuard_NamesTheUncountedKeyOfARotation`).
  Otherwise first confirm out of band that the vendor really dropped its
  strong signer, since the other two recourses adopt the very anchor the
  guard refused, which an on-path strip also produces: the operator sends
  `POST /forget` (and for the suite's `/.` twin and other-scheme copy,
  each a suite of its own), which adopts the candidate as a first adoption
  and is the recourse for a bundled key; or the operator removes the old
  strong key from the keyring, and the guard then stands down for the
  suite until a strong key is installed again, at these costs (where the
  vendor's strong and weak keys share one `.asc` file, it means editing
  the vendor's file) — the cache reads the host's own apt key directories
  (`/etc/apt/trusted.gpg.d`, `/etc/apt/keyrings`, `/usr/share/keyrings`:
  `keyringDirs` in `cmd/apt-cacher-ultra/main.go`), so the host's own apt
  loses the key too, and a file a package owns comes back at its next
  upgrade; the bundled Debian, Ubuntu and Ubuntu Pro keys
  (`internal/gpg/embedded/`) cannot be removed; every other suite that key
  signs stops counting it (`untrusted_signer` there); the keyring is read
  only at start, so it needs a restart; and the refusal keeps its retry
  time across the restart (`rescheduleFailingAdoptions` leaves refusal
  states alone), so the suite adopts at that retry, not at the restart.
  `accept_any_signer` would clear it too, by verifying no signature for
  the host's suites (no anchor has a strong signer under the bypass): not
  a recourse. No override and no configuration key (ruling HD-17).
  Per-client tracking by User-Agent was considered and not taken (the
  plan's addendum). Recommendation: leave it; the per-suite identity
  pinning of the flat-repositories spec's §10 item 5 (the C-3 entry above)
  would close it with the first-snapshot problem.
- **Older clients refuse real vendors' layouts that the cache accepts**
  (raised 2026-11-05, Task 9e, its classes (a) and (b); ruling E4: never
  folded). Measured with the 158 survey anchors in the five older images:
  `ubuntu:16.04`'s apt 1.2 refuses debian-buster-archive's real
  `InRelease` and `Release.gpg` ("Invalid message from method gpgv:
  NO_PUBKEY DCC9EFBF77E11517": one good signature beside two by keys the
  client lacks; corpus row `us_good_plus_two_unknown`; class (b), and a
  16.04 client of a buster archive is no real deployment, the controller's
  ruling) and nginx's CRLF `InRelease` (a CR after the clearsign marker,
  "Clearsigned file isn't valid, got 'NOSPLIT'": `i_crlf_all`,
  `n_i_nginx_crlf`, `a_text_crlf_sig_lf`; (b)); `ubuntu:16.04` and `14.04`
  (gpgv 1.4) refuse syncthing's `InRelease` (no CRC line and base64 that
  needs no padding, read on into the END line, NODATA: `i_crc_missing`,
  `d_crc_missing`; (b), and any CRC-less signer's — ms-*, signal,
  tailscale — whose packets' length lands on a multiple of 3) and yarn's
  two anchors (an EdDSA subkey, which gpgv 1.4 cannot use, as for 14 `pk_`
  rows of EdDSA and ECDSA keys; class (a), crypto policy, not intersected:
  HD-19). `aptShapeOldExempt` lists the 20 corpus rows and
  `aptShapeOldRefusals` pins each class to its measured refusal. Those
  clients fail fetching directly too. For the vendors' own anchors, class
  none: the cache agrees with the current clients, and the older ones
  refuse the vendor directly. But each (b) class is also a key-free edit an
  on-path attacker can make to any suite's anchor (the second final
  review's V2-M1): converting it to CRLF (`i_crlf_all`), stripping the CRC
  of one whose base64 needs no padding (`i_crc_missing`), or appending two
  packets by keys the client lacks, even one unknown packet twice (the
  lens's probe `o3_good_unkB_unkB`, [A good][B][B], refused by
  `ubuntu:16.04`: "Invalid message from method gpgv: NO_PUBKEY
  2628FEC1C3054C2B"); the cache adopts such an anchor and keeps serving it
  to the clients that refuse it, which then fail `apt update` for the
  suite: the stripped CRC breaks 16.04 and 14.04 clients (gpgv 1.4), the
  CRLF conversion and the two unknown-key packets 16.04 clients alone (apt
  1.2; 14.04's apt 1.0 accepts both: lens V2's re-review, V2-R1-M2). That
  residual is class R2 for those clients, accepted by the (b)
  rulings (E3, E4): a fold would refuse nginx, syncthing or
  debian-buster-archive. Recommendation: leave them; a vendor's to fix,
  and the residual stays recorded here.
- **Three S1 cases Task 9e left unmeasured, and a fourth of the liveness
  check** (raised 2026-11-05, the Task 9e review's re-review 1; (4): the
  final review 2's closure check). S1 checks a packet by a known key that cannot
  sign directly (`verifiesDirectly`, *Decided*), which reads no usage,
  expiry or notation: (1) a key whose self-signature has no key-flags
  subpacket goes the direct way too, so a bad packet by it now refuses,
  toward what gpgv 2.2.4 and 1.4 do (unmeasured); (2) an unknown critical
  notation in a good packet by a key that cannot sign is not read, so the
  packet passes, as before Task 9e (the older gpgv unmeasured); (3) a
  packet naming an ElGamal or ECDH subkey can never verify, so S1 always
  refuses an anchor that holds one (strict; no real signer emits one).
  (4) For gpgv 1.4's one checked packet, such a key's liveness (`keyLive`)
  checks revocation, the key's lifetime and the packet's expiry, but not
  whether the primary's self-signature, the subkey's binding or its
  back-signature has itself expired, which go-crypto refuses for a signing
  key: a dead binding passes (the final review 2's V2-CC-M1; vendor-only,
  as it needs the vendor's own packet; unmeasured on gpgv 1.4).
  Class R3 for (3), an attacker's edit refused where the newer apt accept
  it; (1), (2) and (4) unmeasured, each toward the older gpgv; no vendor anchor
  changed. Recommendation:
  measure them in a corpus batch when the generator next makes keys
  (`--usage-keys` makes key U).
- **Two layouts the guard calls not strong where every image accepts
  them** (raised 2026-11-05, Task 9d; ruled accepted). `strongSigner`
  reads go-crypto's self-signature in use (the newest, the primary
  identity's), where `debian:13`'s sqv takes the binding in force at the
  signature's time: a key file that also keeps an older valid
  self-signature made before the packet (`pk_rsa3072_rebound_both`, `ok`
  in all six images; `aptShapeStrongOverStrict`), and a key whose primary
  user ID is SHA-1-bound while another is SHA-256-bound (unmeasured;
  `TestStrongSigner_ReadsGoCryptosSelfSignatureInUse` pins go-crypto's
  choice). The guard then stands down for such a current anchor (it fails
  open), and could refuse such a candidate only after a strong current
  anchor signed later than the candidate. No surveyed vendor has either
  layout. Class none: the guard is defence in depth and fails open here;
  R3 only for that unlikely refusal of an earlier-signed candidate.
  Recommendation: leave it (defence in depth, ruling G2's direction).
- **A shutdown test that failed once under load** (raised 2026-11-03, Task
  9b's fix round 6). `TestServe_GracefulShutdown_FetchMidStream_NoOrphanBlob`
  (`cmd/apt-cacher-ultra/shutdown_orphan_blob_test.go`) took 5.44 s to shut
  down once, against its 5 s bound, on a loaded machine; it passed alone
  3 of 3 times, in a clean race re-run, and 10 of 10 under `-race` at
  light load (the V lens's re-review 7). The release workflow runs
  `-race`, and a teardown flake failed the 1.0.3 release run twice. Class
  none: a test's timing, a release-process risk. **Closed** (2026-11-05,
  the hardening plan's Task 8, the final review 2's D2-M7): measured under
  `-race -count=8` at light load, every run took 0.91–0.96 s, the whole
  test; the regression the bound's comment names (the lifecycle context
  no longer ending the fetch, so the 30 s `TotalTimeout` fires) is caught
  by the test's 15 s `Fatalf`, and the 5 s `Errorf` added only a failure
  under load between 5 and 15 s, while the test's claims (`pool/` and
  `tmp/` empty) do not depend on it. The bound is 12 s now, with that
  reason in its comment.
- **The key reader reads more than some apt reads** (raised 2026-11-01 to
  03, Task 9b's fix rounds 2 and 3: the V lens's V-R2M3 and V-R3M1;
  rulings R1 and R3). By R1 the cache reads every key that any supported
  apt reads from a key file (`internal/gpg/keyfile.go`: apt-key's awk
  reading and apt 3's, unioned), so for an odd operator file its keyring
  holds more than some client's: an armored file named `.gpg`, from which
  apt 2.x's gpgv and apt 3 read no key, or a `.asc` with CRLF line ends,
  from which apt 2.2, 2.4 and 2.6 read none; such a client refuses the
  suite as untrusted with or without the cache. And copies of a key are
  merged (`mergeEntity`): a copy holding a signing subkey with its primary
  expired, beside a renewed copy without the subkey, verifies an anchor by
  the subkey that all six apt refuse (V-R3M1, `kr10_mixed_*`; the
  AIDEV-NOTE in `LoadKeyringWithEmbedded`); and where an older copy read
  first has the primary expired and a later copy renews it, gpgv, which
  reads only the first keyblock holding the key ID, refuses an anchor that
  the cache and sqv accept (`kr10_renew_old_first`; the merge ruling of
  Task 9b's fix round 2, in the plan's ledger `progress.md` beside `PE/`;
  the V lens's re-review 3, `PE/final-review/V-report-rereviews.txt`, its
  kr10 table: "copy1, then
  copy2", refused in the five gpgv images and adopted by the cache, its
  generator in `PE/final-review/V/rr3/`). None is an attacker's
  lever: the first needs the operator's own file, the others the vendor's
  own signature. Class R3: availability for the clients that read less, or
  read the first copy only; each such client fails with or without the
  cache. Recommendation: leave them.
- **A skipped key file shows only in the start's log** (raised 2026-11-01,
  the V lens's re-review 2; parked for Task 9c). A key file the loader
  skips or reads only in part logs WARN once at start
  (`keyring_file_skipped`, `keyring_file_truncated`, or `keyring: parse
  failed; skipping file`), and nothing on the status page or in the
  metrics names it; the suite then shows `untrusted_signer`, and a skipped
  file is also a key S1 does not know. Since Task 9c the `untrusted_signer`
  tooltip points at those lines. Class R3 (a skipped file is an S1 gap
  that needs an operator's unreadable file; ruling R3), otherwise
  operability. Recommendation: list skipped files on the
  status page's keyring panel, or count them in a gauge, in a later
  release.
- **Two reconcile WARNs still repeat** (raised 2026-11-01, the V lens's
  re-review 2; on 1.0.x too). Task 9b's fix round 2 made a failing
  reconcile *decision* warn once per snapshot
  (`acu_adoption_reconcile_failed_total{stage}` counts every failure), but
  the heal stage's `adoption_reconcile_failed`
  and each member's `adoption_reconcile_member_failed` still log WARN at
  every attempt, on every unchanged check of a snapshot that cannot heal.
  Class none: log noise. Recommendation: the same once-per-snapshot memo
  for both, with the counter as the signal, when that code is next
  touched.
- **Three optional items Task 9c left** (raised 2026-11-05, the hardening
  plan's final review, D-M1, D-M5 and D-M12; recorded by Task 9c's fix
  round). (1) The status page's aggregate `gpg_failed` notice
  (`internal/admin/status_template.go`, mirrored in
  `docs/admin-ui-spec.md`) still says "Likely cause: upstream repository
  key changed or the matching archive key isn't loaded", and after an
  upgrade it will show beside waves of `malformed_*` refusals, where it
  misleads: recommendation, a hint that names the shape refusals too
  (hover a row's reason; the `adoption_gpg_failed` line names the rule),
  in a later release. (2) CI runs neither ruff nor mypy on
  `e2e/apt-oracle/gen_shapes.py`, and no scheduled workflow runs the apt
  oracle to see apt drift between releases: recommendation, both in
  `make lint` and a weekly workflow, when CI is next touched. (3)
  `docs/configuration.md`'s strict-mode prose does not say that a refusal
  can come after a whole download (502, `Retry-After: 60`, the request
  line carrying `upstream_status`): recommendation, one sentence. Class
  none: operability and docs.
- **The verifier's maintainability** (raised 2026-11-05, the second final
  review's V2-M3 and V2-M4; not done in Task 8's verifier fix by the
  controller's ruling). (1) `verifyAnyTrusted` (internal/gpg/verifier.go)
  runs two near-copies of one per-packet sequence (the short key ID branch
  and the fingerprint branch: trust check, skip, the keyring's mark,
  verify, count) with seven state variables and a long precedence comment
  for its final switch, so a new S2 rule is made twice and its precedence
  derived again; the smallest simplification is one helper that resolves a
  packet to the entities to verify it against, its key's fingerprint, or
  why it is untrusted, short or ambiguous, then a single skip, mark,
  verify and count path. (2) The verifier's files carry about one comment
  line per code line, most of it review history (V-RM1, A-RC1, 9e-I1,
  HD-14) interleaved with the invariants, so that a stale sentence reads
  as a rule (V2-C2's "2.2 is the strictest supported gpgv" was one): lead
  each function's doc with its invariant in two or three lines and move the
  history into one history file per package that the AIDEV-NOTEs point to,
  shortening the notes, never removing one. Class none: maintainability;
  the table tests and `TestVerifier_RefusesEachRowByItsRule` pin the
  verdicts and reasons a refactor must keep. Recommendation: after 1.1.0,
  as a refactor pinned by the table tests.
- **Two pins main's #17/#18 work removed** (raised 2026-11-06, the merge
  review of main into the hardening branch, M-M5; the removals are main's
  0c6d2c0, deliberate, and are not undone). (1)
  `TestServeHTTP_AMissStampsItsRowAfterItsPoolFile` pinned ruling R-G3's
  order: `runFetch` reads a row's `last_fetched_at` stamp only after its pool
  file is final. The code still keeps the order (the R-G3 AIDEV-NOTEs in
  `internal/handler/handler.go` and at `adoptionStampTx`,
  `internal/cache/adoption.go`), but nothing a client sees depends on it
  any more: `Last-Modified` is upstream's date and validation is by content
  ETag; only the first adoption's historical floor over the anchors'
  `last_fetched_at` reads it. (2) B-33's
  `TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline` pinned that the
  adopter path's `Release.gpg` GET runs under the check deadline
  (`fetchCtx`, `checkDetached` in `internal/freshness/check.go`); main
  rewrote it into the no-adopter
  `TestCheck_LegacyDetachedRefreshDoesNotWaitForSignature`, and the adopter
  path still uses `fetchCtx`, unpinned. Class none: test coverage; neither
  behaviour changed. Recommendation: if R-G3's floor ever matters again (a
  reader that compares `adopted_at` with `last_fetched_at`, or a
  client-visible date from it), bring the first test back; if the check
  deadline ever matters again for the adopter's `Release.gpg` (a stall
  report, or a change to `checkDetached`'s contexts), bring the second
  back.

## Decided

- **An upstream HTTP 204 is an empty file for indexes and metadata only**
  (the maintainer, 2026-11-05; raised by the hardening plan's final review
  2, F2-I1 and D2-M2, then F2-R1-M1). History: PR #22 by Laurent Corbes
  (@lcaflc) — GitLab's Debian registry serves zero-byte index files, such
  as an empty `main/debian-installer/binary-all/Packages`, as 204 — which
  the maintainer implemented on 2026-11-01 (main 2e987d1, this branch
  3a6d5ba), accepting a 204 on every path. On this branch a 204 for a
  `.deb` the snapshots do not vouch for was then cached as an empty file
  and served as a HIT for good, where 1.0.x answered 502 and cached
  nothing; a 204 for an anchor was cached as an empty anchor. The
  narrowing: a 204 is an empty success only for indexes, metadata and
  declared-size-0 downloads — an adoption's `Release` members (declared
  size and hashes decide), an adopted suite's metadata recovery (declared
  hash decides) and a client's index or metadata miss (`proxy.IsMetadata`)
  that is no anchor. A 204 for a `.deb` or any other package file is an
  upstream error (F2-I1, D2-M2), and so is a 204 for an anchor, a path
  `aptpath.ClassifySuiteFile` classifies as `InRelease`, `Release` or
  `Release.gpg`, dists and flat suites alike (F2-R1-M1): a 502 with
  `Retry-After`, nothing cached; hot prefetch counts such a `.deb` as a
  failed warm. Measured (`PE/final-review-2/F2-report.txt`, *Re-review 1*
  (3)): with
  apt 3.0.3 (debian:13), 2.8.3 (ubuntu:24.04) and 2.2.4 (debian:11), an
  empty or 204 `InRelease` or `Release.gpg` is a hard error
  (NOSPLIT/NODATA, exit 100), a 502 only warns (exit 0, the old lists
  kept), and of a 404, 502, 204 and an empty 200, only the 404 makes
  apt fall back to `Release` and `Release.gpg` (no other status was
  measured). A 204 is visible either way: `upstream_status` 204 and
  `acu_fetch_total{outcome="no_content"}` (F2-M3). Do not widen 204
  acceptance to anchors or package files without re-measuring apt and
  asking the maintainer. Do not narrow it below empty indexes without
  asking the maintainer: that breaks PR #22's GitLab case. Residual: an anchor-named path with no
  suite (a host-root flat repository's `/InRelease`, unsupported and
  logged `flat_host_root_unsupported`; a non-canonical path) is
  classified as plain metadata and still accepts a 204. Commit c2ee1c2
  and its anchor follow-up; the decision point is `runFetch`'s
  AIDEV-NOTE (`internal/handler/handler.go`). Pinned by
  `TestServeHTTP_NoContentPackageFileIsAnUpstreamError`,
  `TestServeHTTP_NoContentAnchorIsAnUpstreamError`,
  `TestServeHTTP_NoContentMetadataIsAnEmptySuccess`,
  `TestServeHTTP_NoContentMissThenHit`,
  `TestServeHTTP_NoContentSnapshotMemberRecovery`,
  `TestAdopter_NoContentHTTP` (PR #22's case through adoption),
  `TestRunHotPrefetch_NoContentIsAFailure`,
  `TestFetch_NoContentIsEmptySuccess` and `TestFetch_NoContentOnlyForA204`.

- **Issue #14: GC frees disk again on a busy cache, and the retired backlog
  stays bounded** (the maintainer, 2026-11-05: worked for 1.1.0, all three
  parts of a consult's design; raised 2026-09-28 as GitHub issue #14, and
  2026-11-05 by the hardening plan's final review 2, G2-I1, present since
  1.0.x, and G2-I2, a regression of the hardening plan's DN-7). G2-I1: a
  pass that reached `gc.max_tick_duration` ended the tick with the rotation
  at it, so while the snapshot pass had more work than one budget every
  tick ended there and the blob pass, the only one that frees disk, never
  ran (the reporter's 1.0.2 status page: a tick that deleted 90 displaced
  snapshots in 5 m, flagged degraded, then a full disk that took two
  restarts to clear); at a start a url_path scan longer than the budget did the same
  to every start, its cursor living in memory. G2-I2: since DN-7 a periodic
  B only retires the displaced snapshots and chunked sub-job C, 1.6–2.3×
  slower than main's one-transaction delete, deletes them once a tick and
  once an hour after a start, while the startup pass ran no C (ruling
  PF-15), so the retired backlog grew without bound and a restart no
  longer cleared it. The three parts, each its own commit:
  (1) every pass gets a turn — the tick's budget T splits into deadlines
  with f = T/5, the url-path pass ending by T−2f, the snapshot pass by T−f,
  the blob pass by T (`passDeadlines`, `internal/gc/gc.go`), and a url-path
  or snapshot pass that hits its deadline moves the rotation on and the tick
  goes on, as ruling HD-15 does for a failed pass, at a start and in a tick
  (commit d170dd6); (2) the retired run (`retiredRun`) is re-armed an hour
  later whenever it, or a periodic tick's snapshot pass, stops at its
  deadline with work left, never after a failure or a panic and not when
  `gc.interval` is an hour or less: up to 24 bounded, chunked budgets a day
  while a backlog lasts, with no new key, goroutine or schema change
  (commit 9d0e8ad); (3) ruling PF-15 is re-ruled: the startup pass runs C
  after A ∪ B within its snapshot deadline and its blob pass reclaims their
  blobs, so a start with a retired backlog may wait up to
  `max_tick_duration` before the listeners open, the bound HD-7 accepts
  for B; C deletes every retired snapshot, the trust audit's quarantined
  ones included (spec C9), and none that is not retired, such as a
  displaced one the audit still holds queued (R-H1) (commit 13a999e).
  Measured with the lens's probe P3 (the 2026-11-03 scaled repro: 70,000
  `package_hash` rows a snapshot, 1 MiB members, two suites, four
  adoptions a day, a 3 s budget, 50 expired blobs; serial; four days), on
  an archive of 13a999e with the hourly runs simulated back to back after
  each tick: ticks of 2.5–2.9 s, `deadline_reached=true` and
  `blobs_reaped` 53, 24, 10 and 6; retired 11, 3, 2 and 3 after each tick
  and 0 after its hourly runs (5, 1, 1 and 2 runs); a restart then reaps 5
  blobs, leaving 16 (8 MiB) and no retired snapshot. The lens measured
  `blobs_reaped=0` at every tick on both 7f276ee and main's 276bfa2, the
  retired count growing 11, 14, 18, 21 on 7f276ee (a restart left 57 blobs,
  28 MiB, and the 21 retired) and main leaving 16 blobs after a restart
  that reaped 98. With ticks alone (no hourly runs) 13a999e reaps blobs at
  every tick (53, 2, 2, 4) but the retired count grows (11, 14, 17, 19),
  which the hourly runs bound. Under contention (adoptions during the tick)
  a batch queued behind them can overrun the budget and use the blob
  pass's share (ticks of 4.9 and 6.5 s); the next tick then runs the blob
  pass alone, which reaped 66 and 16, and the url-path and snapshot passes
  wait one more `gc.interval` (the final review 2's G2-RR1-M1: on day 4 of
  that run 8 displaced snapshots waited a day to be retired). A busy cache's runs may now end
  with `deadline_reached=true` while they free disk, a healthy steady
  state (`docs/configuration.md`, `[gc]`, which also gives 1.0.x users the
  interim workaround); the status page's *Deadline reached* badge is warn,
  not crit, with a tooltip saying so (commit d7e9c8a), and the GC vital
  warns on it rather than going crit, so the page reads WATCHING, not
  DEGRADED (the controller's ruling, 2026-11-05; `admin-ui-spec.md`
  §9.1). Part (3) makes the first 1.1.0 start delete the
  snapshots the upgrade's trust audit quarantined, so a way back to 1.0.x
  after a second 1.1.0 start loses their pool files even within the first
  hour, and a suite whose current snapshot was quarantined can then answer
  502 (`Retry-After: 30`) on 1.0.x until 1.0.x re-adopts it, whenever
  upstream has changed since (1.0.x's hash-validated member refetch fails
  closed); the maintainer, 2026-11-05, kept part (3) at that cost, and the
  1.1.0 release notes say so.
  Pinned by `TestPassDeadlines_SplitTheBudget`,
  `TestRunTick_ASnapshotDeadlineDoesNotDeferTheBlobPass`,
  `TestRunTick_AURLPathDeadlineDoesNotDeferTheLaterPasses`,
  `TestRunTick_ASnapshotDeadlineMovesTheRotationToTheBlobPass` (was
  `TestRunTick_SnapshotDeadlineDefersBlobPass`, which pinned the
  starvation), `TestRun_RepeatsTheRetiredRunHourlyWhileABacklogRemains`,
  `TestPeriodicTick_ASnapshotDeadlineArmsTheRetiredRun`,
  `TestRun_AFailedRetiredRunIsNotRepeatedHourly`,
  `TestRun_TheRetiredBacklogStaysBoundedUnderASustainedDisplacementRate`,
  `TestStartupPass_RunsSubJobCWithinItsSnapshotDeadline` and
  `TestStartupPass_DeletesRetiredSnapshotsAndReapsTheirBlobs`. Not done:
  making C's delete cheaper (the consult measured no gain from a larger
  page cache; the rest is schema work).
- **A GC pass that fails at every tick no longer starves the passes after
  it** (the maintainer, 2026-10-28: worked for 1.1.0; raised 2026-10-27, the
  H3 final review R3 #5, and again 2026-10-29, the hardening plan's final
  review, G-I1 and G-M5; unchanged since 1.0.x for the url_path pass and
  sub-jobs A and B). `runTick` (`internal/gc/gc.go`) moved the pass rotation
  on after a panic but not after an error, so a pass that failed at every
  tick (a fault limited to its tables) kept the blob pass from running. Its
  three error paths now go through `passFailed`, which moves the rotation
  past the failed pass, as `skipPanickedPass` does after a panic, unless the
  GC's context has ended (a shutdown stopped the pass: it did not fail);
  each pass that fails logs WARN `gc_tick_failed` as it fails, on the same
  test, and so does the retired run (its row in `docs/log-fields.md`).
  Ruling HD-15: a failed pass no longer ends its tick either — the passes
  after it run under the same deadline, and `runTick` returns the first
  failure — in the startup pass as in a periodic tick. A daemon restarted
  more often than `gc.interval` never runs a periodic tick and begins the
  rotation afresh at every start, so the rotation alone left a pass that
  failed at every start starving the passes after it there (G-I1); such a
  daemon now runs every pass at every start. And a pass that fails at every
  tick is retried at every tick, the others keeping their cadence, where it
  ran every other tick, the passes after it in between (G-M5). Safe: each
  pass re-checks its own predicates. The fix wave's follow-up: every failed
  pass logs its own line, where a tick logged only its first failure, and a
  startup pass in which a pass failed still logs `gc_run_complete` and sets
  its metrics and last-run summary, where it wrote none. Done in the
  hardening plan's Task 1 (commits 38eb75a and 85407b1) and its final fix
  wave (commit d91c8e1 and the fix wave's follow-up), pinned by
  `TestPeriodicTick_AFailingPassDoesNotStarveTheNext`,
  `TestStartupPass_AFailingSnapshotPassLeavesTheBlobPassToRun`,
  `TestPeriodicTick_LogsEveryFailedPass`,
  `TestStartupPass_AFailedPassStillReportsItsWork` and
  `TestPassFailed_MovesPastEachPassUnlessTheContextEnded`.
- **Periodic GC retires displaced snapshots instead of deleting them in one
  transaction** (the maintainer, 2026-10-28: worked for 1.1.0; raised
  2026-10-27, the H3 final review R3 #4; unchanged since 1.0.x).
  `RunSnapshotGCBatch` (`internal/cache/gc.go`) deleted up to
  `gc.snapshot_batch_size` displaced snapshots' rows in one writer
  transaction, which held every write (a miss's store, a flip's commit) for
  seconds on a large suite. Ruling HD-4: in a periodic tick, sub-job B now
  gives the displaced snapshots beyond `gc.keep_displaced` their
  `retired_at` in one `UPDATE`, and sub-job C deletes them, 10,000 rows a
  transaction, normally in the same snapshot pass; every reader that serves,
  vouches or judges history filters `retired_at` or reads current snapshots
  only. The startup pass still deletes them at once (ruling HD-7): it runs
  before the freshness scheduler and the listeners, so it holds up no other
  writer and no request, and a daemon that never lives
  min(1 h, `gc.interval`) still deletes them at every start. A snapshot
  retired so counts twice, in `displaced_reaped` and then in
  `retired_reaped`. Measured in the hardening plan's Task 2 (six displaced
  snapshots of 70,000 `package_hash` rows, 700,000 in the table; no `-race`;
  a loaded 12-thread laptop): a retiring batch took 4.6–8.5 ms, the startup
  pass's delete of the same six 3.6–4.7 s, and each sub-job C chunk
  120–240 ms (median 149–191 ms). The chunk stays 10,000 rows (ruling HD-11:
  the aim was a writer never held for seconds, which this meets; the plan's
  200 ms a chunk was an estimate), and its re-measure is under *Open*. Done
  in that task (commits 68111f9 and 908963f), pinned by
  `TestRunSnapshotGCBatch_RetiresTheDisplacedBeyondKeep`,
  `TestRunSnapshotGCBatch_ARetiredDisplacedSnapshotIsNoLongerRead` and
  `TestPeriodicTick_RetiresAndDeletesTheDisplacedSnapshots`.
- **Adoption stops a member's download at its declared size, and nothing is
  decompressed in transit** (the maintainer, 2026-10-28: worked for 1.1.0;
  raised 2026-09-08, the security review's A-2, VA-3 and VA-4; raised again
  2026-10-27, the H3 final review R6 #3). An unsigned `Release` declares its
  own members' sizes, and the size check ran after the download; with
  `adopt_unsigned = true` (the default) a client that fetches an unsigned
  repository through the cache makes it adopt that repository's members.
  Ruling HD-5: the upstream transport sets `DisableCompression` (Go asked
  every upstream for gzip and decoded it, so a decoded body had no declared
  length and skipped the size check, and a resumed one appended compressed
  bytes to a decoded prefix); `fetch.Target` gains `MaxBytes`, which fails a
  declared length over it before the body is read, and a body that runs past
  it as soon as it does, with `ErrBodyTooLarge` (outcome `body_too_large`,
  never retried); adoption sets it to each member's declared size, one byte
  for a member declared empty (`memberMaxBytes`), on the by-hash probe and
  the plain path, which repair and reconcile share. There is no margin: a
  member of any other size fails its verification anyway (the plain path
  checks the declared size since the hardening plan's final review, F-M6,
  where it checked the response's length, so a chunked body, which
  declares none, was checked by its hash alone). It costs two things (the
  final review's F-M5, measured 2026-10-29 over the security review's 81
  vendors). Nine of them answer an uncompressed member gzip-encoded when
  asked, so those downloads are now full size (hashicorp's
  `main/binary-amd64/Packages`: 2,027,475 bytes, which came gzip-encoded
  and chunked while Go asked for gzip). And an upstream that sends
  `Content-Encoding: gzip` unasked now has its bytes stored as sent, so an
  uncompressed index member served so fails its declared size
  (`member_fetch_failed`, before its hash is read; the final review's
  F-M7), which Go's decoding used to rescue, and its suite stops adopting;
  no measured vendor does
  (no answer to a request without `Accept-Encoding` carried one: the 81
  anchors, and 239 members of 69 vendors), and every supported apt fails
  such a file too ("File has unexpected size", in the six client images).
  The same change fixes what an operator could meet in the field (DN-9;
  the final review's D-M8): an Apache that serves `.gz` files with
  `Content-Encoding: gzip` had them decoded by Go and stored as wrong
  bytes, and they are now stored as sent
  (`TestFetch_StoresAGzipBodyItDidNotAskFor`).
  Ruling HD-12: the long-member test asserts that the cache read
  exactly the declared size and one byte more, that the upstream's endless
  body then fails to write, and that `adoptMember` returns within 15 s, but
  no byte count at the upstream, which loopback buffering varies.
  `docs/configuration.md`'s unsigned threat note now says each file's
  download stops at its declared size. Done in the hardening plan's Task 3
  (commits 391eac2 and c45cd74) and its final fix wave (commit a8a8d20),
  pinned by
  `TestAdoptMember_AMemberLongerThanDeclaredStopsAtItsSize`,
  `TestAdoptMember_APlainPathMemberShortOfItsDeclaredSizeIsRefused` and the
  `TestFetch_MaxBytes…` tests. Downloads with no declared size are under
  *Open*.
- **A vouched `.deb` whose snapshot is retired during its download is
  refused on a strict host** (the maintainer, 2026-10-28: worked for 1.1.0;
  raised 2026-10-27, the H3 final review's R6 re-review; unchanged since
  1.0.x). On a strict host, a `.deb` miss that was vouched before its
  download was served unchecked once if its vouching snapshot was retired,
  or displaced by one that does not list it, while it downloaded. Ruling
  HD-6: `prefetchCheck` reports the path vouched when its read, or
  `prefetchRecheck`'s second look, found it so; when `runFetch`'s read after
  the download then finds no row, strict mode decides there
  (`strictAfterDownload`, `internal/handler/handler.go`), as it already did
  after a failed read before the download: refused (502, `Retry-After: 60`),
  or, with a signed adoption in flight on the host, waited for and retried
  once; a host whose coverage is incomplete passes it through, and a host
  without strict mode serves it as before. A read after the download that
  fails still fails open (WARN `package_hash validate lookup failed`). Done
  in the hardening plan's Task 4 (commits 5c27a97 and 7c3aca0), pinned by
  `TestServeHTTP_StrictMissDecidesWhenVouchingEndsDuringTheDownload`.
- **The verifier refuses the anchor shapes apt rejects: the hybrid
  verifier** (the maintainer, 2026-10-28: worked for 1.1.0; and
  2026-10-29: the hybrid, rulings HD-16 to HD-18, chosen over closer
  emulation of go-crypto's decoders and over shipping the first rules and
  deferring; raised 2026-09-08, the security review's theme one: VC-2,
  C-4, VC-1, and I-1; confirmed 2026-10-27 by the H3 final review R6 #2;
  rewritten 2026-11-05 by the hardening plan's Task 9c). On 1.0.x,
  `VerifyInline` (`internal/gpg/verifier.go`) accepted a valid `InRelease`
  followed by blank lines, a forged CRC line and an extra bad signature
  from the same trusted key, and `VerifyDetached` lines around a
  `Release.gpg`'s armor, a second armor block and a binary signature
  (ruling HD-1: both forms). The rule (ruling HD-2): refuse a shape any
  supported apt refuses, accept one all of them accept, with listed
  exceptions. The hardening plan's Tasks 6 and 7 first enforced it with
  structural rules (F0–F6) and S1 beside go-crypto's lenient decoders;
  its final review found that every round left more (the verifier lens's
  V-C1 to V-C3, among them a 4 GiB allocation before any check, a crash on
  1.0.x too; the adversarial lens's 102 measured gaps in 11 families), and
  the maintainer chose the hybrid (`PE/approach-decision.md`; the plan's
  addendum of 2026-10-29, DN-11). It reads an anchor in four layers, each
  refusal naming its layer and rule ("armor A4: line 7: 80 base64
  characters, the limit is 76"):
  - **T, the text** (`internal/apttext`): apt's own `LineBuffer`,
    `SplitClearSignedFile` and detached check ported exactly, whose
    output — the message M apt hands its verifier and its `Release`
    parser — is what the signatures are checked against, what adoption
    parses and what reconcile reads again (the re-review's V-RI1); on top,
    no NUL (T5), no space or tab after the clearsign marker on line 1 (T6,
    Task 9e: `ubuntu:16.04`'s apt 1.2 refuses it; a CR stays accepted,
    nginx's layout), no END marker in M (F0, narrowed to M: every apt
    accepts one in an armor header, `vm3_*`) and one armor block;
  - **A, the armor** (`internal/gpg/armor_canon.go`): a canonical
    whitelist that gpgv and sqv both read and real signers write, decoded
    by the cache alone (strict base64: `debian:13`'s sqv refuses set pad
    bits, `n_i_b64_pad_bits`);
  - **P, the packets** (`internal/gpg/sigpacket.go`): the cache's own v4
    parser with exact consumption — v3, v5 and v6 refused before go-crypto
    sees them, one issuer key ID (and a fingerprint only if it agrees), no
    embedded signature, bounded areas, minimal MPIs, at most 8 packets —
    then go-crypto's `packet.Read` of each packet's own bytes as framed
    (P5): no packet is written again before it is verified, which closes
    the old Open entry on re-serialization (ruling HD-14's caveat);
  - **S, the signatures**: S1 (`requireKnownSignaturesVerify`; rulings
    HD-3, HD-8): a packet by any key in the host keyring that fails its
    check refuses the anchor (`bad_signature`), as gpgv's BADSIG makes apt
    refuse it, and since Task 9e whatever that key's usage (its own entry,
    below); S2 (`verifyAnyTrusted`; ruling HD-10): one trusted packet that
    verifies and counts. S1 and S2 read the packets gpgv checks: all of
    them when they share one signature type, else the first only (gnupg's
    `proc_tree`; `n_i_unknown00_first`). S2 counts a packet only if its
    hash is SHA-256, SHA-384 or SHA-512 (ruling HD-18: SHA-1 never counts,
    C-3's digest half), sqv reads it (the re-review's A-RC1: a subpacket
    Sequoia's parser refuses makes `debian:13` ignore the packet) and apt
    does not erase it at a later SHA-1 packet by the same key (A-RC2:
    apt's `methods/gpgv.cc`).
  Under `accept_any_signer`'s bypass T, A and P run unchanged
  (`TestVerifier_BypassRefusesEveryMalformedRow`) and no signature is
  verified (DN-8, *Open*). The trust set is resolved first, so an unpinned
  suite under `require_pinned_signer` is `unpinned_suite` whatever its
  anchor (the re-review's V-RM1); an empty keyring's `no_usable_signature`
  comes after T, A and P. No runtime override (ruling HD-17): an anchor
  outside the grammar is refused with its reason, the previous snapshot
  keeps serving, and widening the grammar is a code change with a measured
  corpus row. apt's verdicts are measured on 904 byte-exact shapes in the
  six client images and the five older ones (`internal/gpg/testdata/aptshapes`,
  `e2e/apt-oracle/README.md`; batches fx to fx22), which
  `TestVerifier_FollowsTheAptShapeTable` runs every one of, none skipped,
  with each refused row pinned to the rule that refuses it
  (`TestVerifier_RefusesEachRowByItsRule`), the text layer held to apt's
  own split refusals (`TestAptTextLayer_RefusesExactlyTheSplitRows`) and
  every row's allocation under a mebibyte
  (`TestVerifier_BoundsAllocationOnEveryRow`); twelve real vendors'
  anchors, one per signer layout, verify in CI
  (`TestVerifier_AcceptsTheVendorAnchors`). The exceptions: 127 strict
  deviations in four families and the older images' exemptions (each
  under *Open*); the known-unsafe list is empty
  (`TestAptShapeKnownUnsafe_IsEmpty`: `x3i_sha1_only` is refused). The
  vendor re-run (the security review's survey, 158 anchors of 81 vendors,
  with every survey key; the Task 8 gate's method): on ff15ccf `InRelease`
  ok 68 and `Release.gpg` ok 67 against 68 and 70 before the plan, the
  only new refusals the binary `Release.gpg` of nginx, steam and syncthing,
  each beside an `InRelease` the check prefers; since Task 9b's fix round
  1 `InRelease` ok 69 and `Release.gpg` ok 68 (yarn's key in a later
  armor block, read now), unchanged to a7d0d72
  (`PE/task9c/vendorscan.txt`); no vendor anchor is
  refused `bad_signature`, and pkgs.k8s.io v1.30's version 3 packet is
  refused as on 1.0.x (*Open*, the residual entry). The upgrade's trust
  audit classifies 1.0.x snapshots with its own checks, not the verifier
  (`internal/freshness/legacy_audit.go`), so a 1.0.x snapshot whose anchor
  the new rules refuse keeps serving, and the suite's next adoption is
  the one refused (the final review's D-M8). go-crypto moved to v1.5.2
  (Task 9b's fix round 2, commit 4fa21be: identical verdicts on the suite,
  the corpus and the survey), and the verifier no longer calls its
  clearsign or armor decoders or its packet reader, so I-1's panic cannot
  reach it. Three of the plan's statements are corrected here (the plan
  stays as written): the deviations were fourteen, not eleven, before the
  hybrid made them families; key E expired on 2020-12-31, not in 2021
  (DN-2); and of the survey's 158 anchors 152, not all, lie inside the
  canonical grammar (HD-16; the deviations' entry under *Open*). Done in
  the hardening plan's Tasks 5 to 7 (commits dc6c8ee, 3c54a22, 80af12b,
  b51ee0e, 132ebe6, 8d2900d, fdfa2b0, 40d3614, 276d60d and a900dbd), its
  Task 9a (9efa08d, the corpus to 490 shapes) and its
  Task 9b (7701e7b, 2552ebf, 5d881fb, 5899af8, e819372; fix round 1
  10098d1, 650f356, 72b8359, 3281d6a, a47437e; fix round 2 a89211f,
  4fa21be, abc37e5, b7b5c31, ffbcbf9), with Task 9e's T6 (28c72ea).
- **Format rules hold over every client generation; crypto policy does
  not; the signer downgrade guard** (the maintainer, 2026-11-01: the cache
  supports every distribution with upstream support, Ubuntu 14.04 through
  Ubuntu Pro until May 2029 included, and 2026-11-03: the guard; ruling
  HD-19, the plan's addendum of 2026-11-03; raised by the final review's
  V lens, V-RC2, and its later ASSERT_PUBKEY_ALGO measurements). The
  format rules (layers T, A and P) follow "refuse what any supported apt
  refuses" over the six current images and the five older ones —
  `debian:10`, `ubuntu:20.04`, `18.04`, `16.04` and `14.04`, all of which
  ran — but fold an older image's refusal in only for a shape no real
  signer emits (Task 9e's class (c): T6, and three rows layer P already
  refused); a real signer's layout an old apt refuses is recorded, never
  refused (class (b)), and so is crypto policy (class (a)), *Open*. Crypto
  policy (layer S: algorithms, hashes, key bindings and sizes) is not
  intersected: S2 counts what the gpgv-based apt count, with HD-18, since
  intersecting would refuse real vendors their own clients read (gpgv 1.4
  cannot verify EdDSA, which signs yarn; `debian:13` refuses SHA-1-bound
  keys, which sign openresty). What that leaves — an on-path strip of a
  vendor's strong signature beside a weak one (sublime's real `InRelease`:
  its second packet dropped leaves a SHA-1-bound key's, which every gpgv
  apt accepts and `debian:13` refuses) — the guard closes: a *strong
  signer* is a packet S2 counts whose key `debian:13`'s sqv and the
  `APT::Key::Assert-Pubkey-Algo` of `ubuntu:24.04` and `ubuntu:26.04` all
  accept (`strongSigner`, `internal/gpg/strength.go`, its table settled by
  measurement, batch fx20: Ed25519, ECDSA P-256 and P-384, RSA of 3072
  bits or more, RSA-2048 until 2030-02-01, each bound by SHA-256 or
  better no later than the signature, the primary of a signing subkey
  judged too; DSA, RSA under 2048 bits, P-521 as a signer, brainpool,
  secp256k1, SHA-1 and SHA-224 bindings, and anything unmeasured, Ed448
  included, are not), and the adopter refuses a candidate with no strong
  signer when the suite's current anchor, verified again at that moment
  with the same verifier and keyring, has one (refusal `signer_downgrade`,
  WARN `adoption_signer_downgrade`; after the rollback check and before
  any download; the current snapshot keeps serving, and unlike
  `rollback_refused` the suite's lag shows, since it really is behind
  upstream). The guard runs in every mode, never fires under the bypass,
  which counts no signer (ruling G1), stands down on its own errors (G2:
  INFO `adoption_signer_guard_skipped`) and when there is no current
  snapshot (G6, *Open*), dates its rules by the verifier's clock (G3),
  and has no override (G4, HD-17: the recourse is in the *Open* entry);
  WARN `adoption_signer_guard_unavailable` says once when a verifier that
  reports no signers turns it off. It reads the stored anchor: no schema
  change and no configuration key. Per-client tracking by User-Agent was
  considered and not taken. The survey's 137 accepted anchors were
  measured in the three newest images: the strong flag is "all three
  accept" on every one, 125 strong and 12 not (*Open*). Done in the
  hardening plan's Task 9d (commits ea28539, 3b6f550 and 65ba00a; its fix
  round eb5ea57, e1f6c9f, 5f46774 and 0d7b103) and Task 9e (e190c8c,
  28c72ea and 5176209; its fix round ea1c081, 9165670, 44e18d6 and
  a7d0d72), pinned by `TestVerifier_StrongSignerFollowsTheAptShapeTable`,
  `TestAptShapeKeyPolicy_AreTheNewestImagesRefusals`,
  `TestAptShapeOldExempt_AreOlderImagesRefusals`,
  `TestSignerGuard_RefusesTheStrongSignersStrip` and
  `TestSignerGuard_RefusesTheStripOfEachWeakClass`.
- **S1 checks every known key, whatever its usage** (the hardening plan's
  controller, 2026-11-05, ruling for Task 9e's fix round, from its review's
  9e-I1). gpgv 2.2.4 and 1.4 (`ubuntu:18.04`, `16.04` and `14.04`) check a
  packet against any key of its key ID, a certify-only primary or an
  encryption subkey too, and refuse the anchor on a bad one (BADSIG),
  where later gpgv and sqv skip a key that cannot sign: `t7g5_s_primary`
  and batch fx21's `us_bad_primary_plus_good`, `us_bad_enc_plus_good`,
  `us_good_plus_bad_enc` and `us_d_bad_enc_plus_good`, each beside a good
  packet. S1 is integrity, not crypto policy, so HD-19's rule against
  intersecting layer S does not apply, and no real signer emits a bad
  packet: S1 now covers a packet by any key of the host keyring, a signing
  key through go-crypto's verification of its entity as before, any other
  key directly (`verifiesDirectly`: go-crypto's own path would look the key
  up again by signing usage and fail a good packet every image accepts,
  `us_good_*_plus_good`); S2 is unchanged, so such a packet never counts
  (`us_good_primary_alone`, `us_good_enc_alone`: refused, as every current
  image refuses them). No vendor anchor changes (0 of 171). What it leaves
  unmeasured is under *Open*. Done in commit 44e18d6, pinned by the table
  test's fx21 rows and `TestVerifier_RefusesEachRowByItsRule`.
- **The key reader reads what apt reads; integrity fails closed** (the
  hardening plan's controller, 2026-11-01 to 05, rulings R1 to R3 and the
  issuer principle, for Task 9b's fix rounds 3 to 9; raised by the final
  review's V lens, V-RC1 and its re-reviews' V-R2C1 to V-R9I1). On 1.0.x
  the loader read only an armored key file's first block and kept the
  first copy of a key, dropping a later copy's subkeys, so a key S1 should
  know could be missing and an anchor every apt accepts refused (yarn's
  key, in a later block, was). Ruling R1: the keyring is trusted operator
  input; the cache reads every key any supported apt reads from a key file
  — apt-key's reading and apt 3's, unioned: every block of any type, the
  keys before junk, whole packets (`internal/gpg/keyfile.go`; files of
  regular type only, up to 256 MiB, so `debian-keyring.gpg` loads, 937
  keys) — refuses a file only where every apt fails it, and merges copies
  of a key into one entity (`mergeEntity`); one reader feeds S1 and S2,
  and new WARN events say what it skips (`keyring_file_skipped`,
  `keyring_file_truncated`). Ruling R2, integrity fails closed: S2 never
  counts a key or subkey any supported apt treats as revoked, expired or
  unbound — a revocation, expiry or newer binding by the key itself that
  go-crypto cannot read, verify or apply (a version 2 or 3 signature,
  RIPEMD-160, a v4 direct-key expiry go-crypto ignores) marks that component
  uncountable for S2, S1 still knowing it. The marks are a union: a
  fingerprint carries every mark any component, copy or reading gave it,
  and is not counted while any holds (Task 9b's fix round 9: a choice of
  one mark had let a copy's future expiry hide another copy's revocation).
  The issuer principle, which settled rounds 5 to 8's back and forth over
  where a signature is "the key's own": a decision that *adds* a mark may
  take either issuer reading (the issuer fingerprint or the issuer key
  ID), and a decision that *suppresses* one (a newer self-signature that
  supersedes or extends) needs a self-signature go-crypto verified, every
  issuer subpacket naming the key. Ruling R3: an S1 or S2 gap that needs a
  malformed or unusual operator key file is availability-class — the
  content is still authenticated by a good signature from a trusted,
  valid key — and goes *Open* as Minor; the realistic cases (a truncated
  download, a pasted key, a secret-key export) were fixed; integrity (R2)
  is never deferred. Measured on every round: debian-keyring,
  debian-maintainers and the 70 survey key files count as before but for
  the marks R2 adds (one subkey go-crypto drops anyway, and four
  direct-key expiries, *Open*), and the vendor re-run is unchanged. What
  remains is under *Open* (a copy go-crypto cannot read; the over-strict
  rows; the reader reading more than some apt). Done in Task 9b's fix
  rounds 1 to 9 (commits 3f5afc4; fd12bc9, d1966ea, 7a80473; dc8f058,
  654ed58, e5856a7, 38c89be; eade3f8, 1d2664d, 0dd821b, abe49d3, 8bd8182;
  c43e04f, 40fec4b, 5f7394c, ee95aef; 7be2e70, df4de65, 9e89d14, d5dde1f;
  6a68a62, 74f7a90, c8a8f7f; abd5bac, 60d26f1; 09b50fe, ad653a7), pinned
  by the key reader's tests (`internal/gpg/keyfile_*_test.go`,
  `keyring_*_test.go`), among them
  `TestVerifier_UnreadableRevocationsAndBindingsAreNotCounted`,
  `TestVerifier_NewestKeptIsTheKeysOwnAlone` and
  `TestVerifier_EveryMarkOnAKeyIsKept`.
- **The apt oracle runs before every release, both sets of images** (the
  hardening plan's Task 5, its ruling E5 for Task 9e, and its final
  review's D-M5). `make e2e-apt-oracle` (the six client images) and
  `make e2e-apt-oracle-old` (the five older ones; a separate target, as
  one run would take about three minutes) run before every release, after
  `docker pull` of the images, and after a go-crypto upgrade or a change
  to `internal/gpg`'s armor, packet or signature checks; a difference
  blocks the release until it is recorded and the table test agrees
  (`e2e/apt-oracle/README.md`, *When to run it* and *When apt changes*).
  They are the only check that sees apt drift between releases; the table
  test holds the cache to the recorded verdicts in CI. The per-release
  checklist: [docs/releasing.md](releasing.md).
- **Dependencies updated for 1.1.0** (the maintainer, 2026-10-28; raised
  2026-09-08, the security review's finding H-3): `golang.org/x/net`
  0.58.0, `golang.org/x/text` 0.41.0, `golang.org/x/crypto` 0.55.0,
  `golang.org/x/sys` 0.47.0 and `github.com/cloudflare/circl` 1.6.5,
  each the newest that keeps `go 1.25.0` (the toolchain entry, below).
  `govulncheck` reported 16 vulnerabilities the code reaches before
  (with go1.26.8), none after under go1.27.3; what remains is the
  `govulncheck` entry under *Open*. And `github.com/ProtonMail/go-crypto`
  1.4.1 to 1.5.2 (the hardening plan's controller, 2026-11-01, for its
  Task 9b's fix round 2, commit 4fa21be; still `go 1.25.0`): it fixes the
  `clearsign.Decode` panic (I-1, in 1.5.0) and the parse panic on an empty
  hashed type-4 subpacket (A-RC3, in 1.5.1), and gave identical verdicts
  on the test suite, the apt-shape corpus and the vendor survey; if it is
  ever wrong, that commit reverts alone (the verifier's own refusal of the
  empty type 4 and its `recover` stand on 1.4.1). 1.0.3 carries the same
  bump.
- **Merge to `main` without a pull request; changes collect in
  `Changelog.md`** (the maintainer, 2026-10-28): the flat-repositories
  branch was merged into `main` directly, and `Changelog.md` lists each
  change until 1.1.0-rc1, whose release notes take them in.
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
  `e2e/live/README.md`, its first paragraph. The per-release checklist:
  [docs/releasing.md](releasing.md).
