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
to HD-14 were made while it was carried out, and the *Decided* entries
of its work say what each decided.

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
4. **`make e2e-apt-oracle`** on the tree you tag
   (`e2e/apt-oracle/README.md`; add its row to that file's *Results*):
   the real apt of the six client images must still give the verdicts
   recorded for the verifier's 168 signature-shape fixtures; about 10
   seconds with the images cached. The GC and upstream hardening plan
   recorded them (2026-10-29).
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
   `gc_retired_run_complete`.
6. **Tag `1.1.0-rc1`** on `main`. The release workflow's
   `Verify no e2e test seam` step runs first on the tag: check it there.
7. **The release notes** (`docs/release-notes/1.1.0.md`): bring them up
   to date with what `Changelog.md` collected since this branch, then
   paste them into the GitHub release. Their links name the `1.1.0` tag;
   for a release
   candidate, paste them with that tag replaced by the candidate's:
   `sed 's|/blob/1\.1\.0/|/blob/1.1.0-rc1/|g' docs/release-notes/1.1.0.md`.
   The file is wrapped at about 72 columns: check the release editor's
   *Preview*, and if GitHub breaks the lines where the file does, paste
   the paragraphs unwrapped instead (lists, tables and headings kept):
   `sed 's|/blob/1\.1\.0/|/blob/1.1.0-rc1/|g' docs/release-notes/1.1.0.md | perl -0777 -pe 's/([^\n|])\n *+(?![-|#\n]|\z)/$1 /g'`
   (for 1.1.0 itself, drop the `sed` and pass the file name to `perl`:
   `perl -0777 -pe '…' docs/release-notes/1.1.0.md`).

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
- **Downloads with no declared size have no byte limit** (raised 2026-10-28,
  the hardening plan's DN-1). Adoption stops a member's download at its
  declared size (*Decided*, below), but a client request's miss, hot
  prefetch's `.deb`s and the handler's re-fetch of a snapshot member whose
  pool file is gone (`runSnapshotMemberFetch`,
  `internal/handler/handler.go`) have no size to stop at: their
  `fetch.Target` sets no `MaxBytes`, and they are bounded only by a
  response's `Content-Length`, which the body must match, and by
  `upstream.total_timeout` (5 minutes by default). A cap needs a size per
  file, which `snapshot_member` and `package_hash` lack: a schema change.
  Recommendation: keep `Content-Length` and `upstream.total_timeout` as
  their bounds; `runSnapshotMemberFetch` could take a cap from the blob
  row's size when that row survives.
- **`accept_any_signer`'s bypass applies no S1** (raised 2026-10-28, the
  hardening plan's DN-8). Under `adoption.accept_any_signer` with no pin
  matched, the verifier checks an anchor's structure (F0–F6) and that it
  holds a parseable signature packet, and verifies no signature, so S1,
  which refuses a bad signature from a key in the host keyring, is not
  applied: it would be the bypass's only signature check (the AIDEV-NOTE in
  `VerifyInline`'s bypass; `docs/configuration.md`, *Trust and signer
  pins*). The cache can then adopt an anchor that a client holding the key
  refuses. Recommendation: leave it, since the bypass makes the clients' own
  verification the trust boundary, as `docs/configuration.md` says.
- **The `Release.gpg` sniff still passes a binary signature** (raised
  2026-10-28, the hardening plan's DN-8). The verifier now refuses a
  `Release.gpg` that does not start with its armor's BEGIN line, a binary
  one included, as every measured apt does, but the sniff the handler and
  the freshness probe share (`aptpath.SniffReleaseGPG`, spec C3;
  `TestSniffReleaseGPG`) still passes a binary signature, or blank lines
  before the armor: such a file still earns its suite a signed-seen marker,
  and the probe still takes a suite without an `InRelease` for a detached
  one, whose adoption the verifier then refuses (`malformed_signature`).
  Both only keep the suite from adopting unsigned, the fail-safe direction,
  and the four vendors in the vendor survey of the hardening plan's security
  review (2026-10-28) that publish a binary `Release.gpg` all publish an
  `InRelease`, which the check prefers. Recommendation: leave it.
- **The cache is stricter than `debian:13`'s apt** (raised 2026-10-28, the
  hardening plan's DN-2 and DN-3). `debian:13`'s apt 3.0.3 verifies with
  Sequoia's `sqv` (the image has no gpgv) and accepts 27 of the corpus' 168
  shapes that every gpgv-based apt refuses, among them a forged CRC line, a
  bad signature from a known key beside a good one and an expired key's
  signature alone (`verdicts.txt`). The cache now refuses all 27
  (ruling HD-2: what any supported apt refuses is refused), so it does not
  adopt such an anchor — the suite keeps its previous snapshot, if it has
  one — where a `debian:13` client fetching directly accepts it: stricter
  than that client, never looser. Recommendation: keep the rule.
- **Fourteen anchor shapes every apt accepts are refused** (raised
  2026-10-28, the hardening plan's DN-6 and ruling HD-9; two more by
  ruling HD-13 and one by HD-14). `aptShapeDeviations`
  (`internal/gpg/aptshape_test.go`) lists them with their reasons: a second
  armor block, which neither decoder reads, so neither S1 nor S2 can check
  it (4 shapes); go-crypto's clearsign decoder, which refuses spaces or a
  second CR after the signature's BEGIN line and an armor header other than
  `Hash` before the signed text (3); an armor with no blank line before its
  body, which F5 refuses too (2); a packet go-crypto cannot parse, of an
  unknown public-key algorithm or with zero bytes inside its declared length
  (3); and two packets with rewritten issuer subpackets, where go-crypto
  verifies by another subpacket than gpgv reads
  (`x3i_goodsig_unhashed_fpr_added`, `t7g7b_33junk_33A`). The reasons record
  that no surveyed vendor publishes a second block or zero bytes inside a
  packet. Recommendation: keep them; revisit one when a vendor's anchor is
  refused for it, or when go-crypto changes (the go-crypto entry below).
- **Weak digests and keys (C-3), and the rest of C-1** (raised 2026-09-08,
  the security review's C-3 and C-1; left out of the hardening plan, its
  DN-1). C-3: the verifier applies no digest or key-size policy
  (`VerifyDetachedSignature` runs with a nil config, so a SHA-1 signature,
  or one by an RSA-1024 key, counts as good where apt does not count it),
  and so adopts `x3i_sha1_only`, an `InRelease` whose only signature uses
  SHA-1, which all six measured apt refuse: the table test's one
  known-unsafe shape (`aptShapeKnownUnsafe`); only the signer can make it.
  C-1: the `Date` rules and `rollback_refused` (spec C5) protect a suite
  from a replayed anchor after its first adoption, but nothing checks
  `Valid-Until`, and a replaying man in the middle can choose a suite's
  first snapshot (the flat-repositories spec, §10 item 5). Recommendation:
  in a later release, refuse what apt refuses for weak digests and keys,
  with a corpus row each, and take up the spec's §10 item 5 (`Valid-Until`
  and per-suite identity pinning).
- **S1 covers only the keys the cache holds** (raised 2026-10-28, the
  hardening plan's DN-5). S1 refuses an anchor that carries a bad signature
  from a key in the cache's host keyring, and ignores one from a key the
  keyring lacks (gpgv's NO_PUBKEY); a client that trusts such a key still
  refuses the anchor, so the cache adopts what that client refuses.
  `docs/configuration.md` (*Trust and signer pins*) asks the operator to
  install in the cache every key the clients' `signed-by` names.
  Recommendation: leave it (inherent: the cache checks the keys it holds).
- **S2's key-ID collision quirk** (raised 2026-10-28, the hardening plan's
  ruling HD-10). S2 (`verifyAnyTrusted`, `internal/gpg/verifier.go`),
  unchanged, verifies a packet whose issuer fingerprint is trusted against
  the whole trust set at once, and go-crypto's `VerifyDetachedSignature`
  hashes once and tries each key holding the packet's key ID with that one
  hash, which only the first can pass: a host key that shares the signer's
  key ID and is loaded ahead of it makes S2 refuse a valid anchor
  (`crypto_verify_failed`), as before the hardening plan. S1 tries each
  entity alone
  (`TestRequireKnownSignaturesVerify_AKeyIDCollisionDoesNotFailAValidSignature`,
  which notes that S2 still refuses). Recommendation: if it is ever met,
  verify S2's fingerprint path against the single entity that owns the
  issuer fingerprint, with a test.
- **Displaced snapshots a periodic tick retired but did not delete wait**
  (raised 2026-10-28, the hardening plan's DN-7). When a periodic tick's
  snapshot pass stops before its sub-job C has deleted what its sub-job B
  retired — at its deadline, at a shutdown, or on a batch or chunk that
  failed or panicked — those snapshots, which have `retired_at`, are no
  longer B's to delete at the next start: they wait for a later periodic
  tick's snapshot pass or the retired run an hour after a start, so a daemon
  that never lives that long keeps them, as it keeps a reaped suite's (the
  H3 final review's R3 #2 residual; `runSnapshotPass`'s AIDEV-NOTE,
  `internal/gc/snapshot.go`). They never serve meanwhile. Recommendation:
  leave it; look again if a daemon that restarts often is seen to keep them.
- **Sub-job C's chunk size** (raised 2026-10-28, the hardening plan's
  Task 2; ruling HD-11). Sub-job C deletes 10,000 rows a writer transaction
  (spec C9; `retiredChunkRows`, `internal/cache/gc.go`), and a chunk holds
  every write while it runs: Task 2 measured 120–240 ms a chunk (median
  149–191 ms; no `-race`, a loaded 12-thread laptop, 700,000 `package_hash`
  rows), where the plan had expected under 200 ms and phase F had measured
  about 80 ms. Ruling HD-11 kept 10,000 rows: the aim was that GC never
  holds the writer for seconds, which this meets. Recommendation: re-measure
  on a staging host before deciding; a chunk under 10,000 rows is a change
  to spec C9 (pinned by
  `TestRunRetiredSnapshotGCChunk_TheChunkIsTenThousandRows`).
- **A vouched `.deb` is stored unchecked when the read after its download
  fails** (raised 2026-10-28, the hardening plan's Task 4 review, Minor 3;
  the posture predates that plan). When the read before a `.deb` miss's
  download found it vouched (one declared hash) and the read after the
  download fails, `runFetch` (`internal/handler/handler.go`) logs WARN
  `package_hash validate lookup failed` and stores and serves the file with
  no hash constraint (`Finalize`), on any host: it fails open, as the hit
  path does, so that a database fault does not refuse every package. The
  hash the first read found is not kept. Recommendation: keep it, and
  finalize with it (`FinalizeExpectingHash`), which refuses only bytes that
  do not match it, so it adds no flood of refusals.
- **Some shape refusals are reported as `crypto_verify_failed`** (raised
  2026-10-29, the hardening plan's Task 6 review, Minor 4). A base64 error
  in a signature's armor body (`x4i_b64_invalid_char`,
  `x4d_b64_invalid_char`) is refused, in both forms, with a plain error
  rather than the `malformed_*` reasons the other shape refusals get, and so
  are `accept_any_signer`'s bypass's refusal of an armor that holds no
  signature packet (`signature body contains no signature packet`) and a
  signature over the verifier's 64 KiB cap. `ClassifyVerifyErr` reports each
  as `crypto_verify_failed`, whose tooltip on the status page ("The
  signature's issuer was trusted but the cryptographic verification itself
  failed.") is wrong for them. Recommendation: wrap the base64 error and the
  empty armor's refusal in `ErrMalformedClearsign` and
  `ErrMalformedSignature`, with a `TestClassifyVerifyErr` row each.
- **S1 and S2 verify each packet as go-crypto writes it again** (raised
  2026-10-29, the hardening plan's ruling HD-14). Both re-serialize a
  signature packet (`sig.Serialize`) to verify it alone, and go-crypto then
  declares the packet's length from each subpacket's length in its shortest
  form, so a legitimately signed packet whose hashed area writes a length in
  more octets than it needs comes out framed wrong and fails where gpgv
  verifies it (the AIDEV-NOTE in `requireKnownSignaturesVerify`). Strict,
  not unsafe: nothing passes that should fail, and no real signer writes
  such lengths. Recommendation: leave it unless a vendor's anchor is refused
  for it.
- **Anchor shapes beyond the corpus are unmeasured** (raised 2026-10-29, the
  hardening plan's Tasks 6 and 7). The cache adopts no anchor a supported
  apt refuses, but for `x3i_sha1_only`, only as far as the corpus' 168
  shapes go (`e2e/apt-oracle/README.md`, *What the oracle checks*). Tasks 6
  and 7 each found shapes beyond the plan's 135 that the cache adopted and
  some apt refused (batches fx8 and fx9, rulings HD-13 and HD-14), and their
  reviews searched no further; the hardening plan's final review searches
  for more that need no key, measured with the oracle. Recommendation: add
  each shape a search, a review or a vendor turns up to the corpus with its
  measured row, and then fix the verifier or list the shape with its reason.
- **A go-crypto upgrade can move the verifier's verdicts** (raised
  2026-10-29, the hardening plan's rulings HD-13 and HD-14). Several of the
  fourteen strict deviations come from go-crypto: its clearsign decoder
  (`i_sigbegin_trailing_ws`, `x7i_sigbegin_double_cr`,
  `xi_cleartext_comment_hdr`; `x3i_armor_no_blank` and `x3d_armor_no_blank`
  stay refused by F5 whatever it does), its packet reader
  (`xi_good_plus_unknownpk_A`, `gi_p3_inner_zero_bytes`,
  `gd_p3_inner_zero_bytes`) and the issuer subpacket it verifies by
  (`x3i_goodsig_unhashed_fpr_added`, `t7g7b_33junk_33A`); and F0–F6 run
  beside its lenient armor readers (`armor_shape.go`'s AIDEV-NOTE). A
  go-crypto that reads these otherwise can turn a deviation into an
  agreement with apt, or a refusal some apt shares into an acceptance.
  Recommendation: at every go-crypto upgrade run the table test
  (`go test ./internal/gpg`) and `make e2e-apt-oracle`
  (`e2e/apt-oracle/README.md`, *When to run it*), and update
  `aptShapeDeviations` from what they show.

## Decided

- **A GC pass that fails at every tick no longer starves the passes after
  it** (the maintainer, 2026-10-28: worked for 1.1.0; raised 2026-10-27, the
  H3 final review R3 #5; unchanged since 1.0.x for the url_path pass and
  sub-jobs A and B). `runTick` (`internal/gc/gc.go`) moved the pass rotation
  on after a panic but not after an error, so a pass that failed at every
  tick (a fault limited to its tables) kept the blob pass from running. Its
  three error returns now go through `passFailed`, which moves the rotation
  past the failed pass, as `skipPanickedPass` does after a panic, unless the
  GC's context has ended (a shutdown stopped the pass: it did not fail);
  `periodicTick` and the retired run log WARN `gc_tick_failed` on the same
  test (its row in `docs/log-fields.md`). A url_path or snapshot pass that
  fails at every tick now runs every other tick, the passes after it in
  between. Done in the hardening plan's Task 1 (commits 38eb75a and
  85407b1), pinned by `TestPeriodicTick_AFailingPassDoesNotStarveTheNext`
  and `TestPassFailed_MovesPastEachPassUnlessTheContextEnded`.
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
  member of any other size fails its verification anyway. Ruling HD-12: the
  long-member test asserts that the cache read exactly the declared size and
  one byte more, that the upstream's endless body then fails to write, and
  that `adoptMember` returns within 15 s, but no byte count at the upstream,
  which loopback buffering varies. `docs/configuration.md`'s unsigned threat
  note now says each file's download stops at its declared size. Done in the
  hardening plan's Task 3 (commits 391eac2 and c45cd74), pinned by
  `TestAdoptMember_AMemberLongerThanDeclaredStopsAtItsSize` and the
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
- **The verifier refuses the anchor shapes apt rejects** (the maintainer,
  2026-10-28: worked for 1.1.0; raised 2026-09-08, the security review's
  theme one: VC-2, C-4, VC-1; confirmed 2026-10-27 by the H3 final review
  R6 #2). `VerifyInline` (`internal/gpg/verifier.go`) accepted a valid
  `InRelease` followed by blank lines, a forged CRC line and an extra bad
  signature from the same trusted key; `VerifyDetached`, lines around a
  `Release.gpg`'s armor, a second armor block and a binary signature
  (ruling HD-1: both forms). The rule (ruling HD-2): refuse a shape any
  supported apt refuses, accept one all of them accept, with listed
  exceptions. apt's verdicts are measured on 168 byte-exact shapes in the
  six client images (apt 2.2 to 3.2; `internal/gpg/testdata/aptshapes`,
  `e2e/apt-oracle/README.md`), which `TestVerifier_FollowsTheAptShapeTable`
  holds the cache to and `make e2e-apt-oracle` re-measures; the plan's
  Appendix A stays as measured on 2026-10-28 (135 shapes), and the README
  and `verdicts.txt` record the 33 since (batches fx8 and fx9). Structure,
  F0–F6 (rulings HD-8 and HD-13; HD-13 reads armor lines as go-crypto trims
  them), in both forms and under `accept_any_signer`'s bypass too: nothing
  after the END line, a CRC line that matches, no `-` line or header without
  `: ` inside the armor, signature packets only, a `Release.gpg` that starts
  with its BEGIN line (new reason `malformed_signature`). S1 (rulings HD-3,
  HD-8 and HD-14; HD-14 reads the issuer as gpgv does), on the trusted path
  only: a signature packet from a key in the host keyring that fails
  verification refuses the anchor (new reason `bad_signature`). S2 is
  unchanged (ruling HD-10); one armor block is read (HD-9). The exceptions,
  each under *Open*: 14 strict deviations (DN-6's eleven, two of HD-13's
  shapes, one of HD-14's) and one known-unsafe shape, `x3i_sha1_only` (C-3).
  Two of the plan's statements are corrected here (the plan stays as
  written): fourteen deviations, not eleven; key E expired on 2020-12-31,
  not in 2021 (DN-2). Done in the hardening plan's Tasks 5 to 7: the corpus
  and oracle (commits dc6c8ee and 3c54a22), F0–F6 (80af12b, b51ee0e and
  132ebe6) and S1 (8d2900d, fdfa2b0, 40d3614, 276d60d and a900dbd).
- **Dependencies updated for 1.1.0** (the maintainer, 2026-10-28; raised
  2026-09-08, the security review's finding H-3): `golang.org/x/net`
  0.58.0, `golang.org/x/text` 0.41.0, `golang.org/x/crypto` 0.55.0,
  `golang.org/x/sys` 0.47.0 and `github.com/cloudflare/circl` 1.6.5,
  each the newest that keeps `go 1.25.0` (the toolchain entry, below).
  `govulncheck` reported 16 vulnerabilities the code reaches before
  (with go1.26.8), none after under go1.27.3; what remains is the
  `govulncheck` entry under *Open*.
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
  `e2e/live/README.md`, its first paragraph.
