# SPEC6_5 — Outstanding items

Phase 6.5 is code-complete. SPEC6_5 §15 Definition-of-done items #1–#12,
#14, #15, and #17 are landed. The three items below require running the
daemon against real traffic in a controlled environment and cannot be
completed by code changes.

## #13 — Live exercise (§15 #13)

Run each of the following against a test deployment and record timing
plus observable metric deltas.

### (a) Source-package fetch
`apt-get source bash` against a real debian/ubuntu source repo.

Expected signals:
- `acu_adoption_sources_parsed_total` increments during Release
  adoption.
- Subsequent `.dsc` / source-tarball fetches validate, one
  `acu_serve_hash_validated_total{path_class=source_dsc|source_tarball,outcome=match}`
  increment per file.
- `acu_package_hash_rows_by_kind{kind="source"}` reflects the
  population.

### (b) PDiff update
`apt update` with `Acquire::PDiffs "true"` configured, against a stable
suite that publishes `Packages.diff/Index`.

Expected signals:
- `acu_adoption_pdiff_indexes_parsed_total` increments.
- Per-patch fetches validate as
  `acu_serve_hash_validated_total{path_class=pdiff_patch,outcome=match}`.
- `acu_package_hash_rows_by_kind{kind="pdiff"}` reflects the patch
  rows.

### (c) Multi-arch install
`apt-get install` from an arm64 chroot (or equivalent arm64 client)
against an upstream that publishes `binary-arm64`.

Expected signals:
- `acu_package_hash_rows_by_kind{kind="binary"}` reflects the arm64
  population.
- Per-`.deb` fetches validate as
  `acu_serve_hash_validated_total{path_class=binary_deb,outcome=match}`.
- Status JSON `repo_coverage.by_host[*].by_architecture["arm64"]`
  populates.

### What to capture per sub-step
- wall-clock duration,
- counter deltas at `/metrics`,
- relevant log lines (`adoption_sources_parsed`,
  `adoption_pdiff_parsed`, `serve_hash_validated`).

## #18 — Multi-arch adoption time bound (§15 #18)

Captured *during* #13. Run a multi-arch debian-main adoption
(`amd64 + arm64 + armhf + i386 + source`) and confirm wall-clock
completion is within **2× the Phase 6 single-arch adoption baseline**.

If the ratio is exceeded, document the measured ratio against the
SPEC6_5 §9.1 adoption budget and revisit the §9 disposition. No code
change unless the §9 reassessment demands one.

## #16 — One-week production soak (§15 #16)

After the test-environment exercise completes, deploy the Phase 6.5
build to the production cache and observe for one week:

- `acu_request_total{outcome=…}` rates stay stable (no regression
  against the pre-deploy baseline).
- `acu_serve_hash_validated_total{outcome="mismatch"}` stays at zero.
  A *single* mismatch may be acceptable as a discovered upstream
  irregularity — investigate and document it. A *sustained* mismatch
  rate signals a daemon bug and blocks the soak.
- No goroutine leak / memory drift over the window
  (`acu_runtime_goroutines`, RSS).

## Sign-off

When #13 / #16 / #18 are all green, Phase 6.5 is delivery-complete.
Record the captured measurements (live-exercise durations, soak
outcome, adoption-time ratio) in SPEC6_5 §15 and tag the release.
