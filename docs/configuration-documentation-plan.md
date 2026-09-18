# Configuration documentation plan and review

## Basis and scope

Based on the ideas in [PR #5](https://github.com/linsomniac/apt-cacher-ultra/pull/5),
reviewed at commit `a934246bc26575f5f55a9e8cf4128c1698850d28`. The PR contains
only an example-config change and a new 103-line Go test. This implementation
was written independently on `docs/config-reference` from `a0254e3`.

The implementation preserves effective packaged settings and production
behavior. Documentation is checked against the loader and runtime consumers,
because some older specifications and source comments no longer describe the
actual behavior. Publishing a new site is a separate follow-up decision.

## Review of the proposed Go test

Every added Go line and its reachable helpers were reviewed before executing
any contributed code. No malicious or suspicious behavior was found. There
are no new dependencies, production-code changes, initialization hooks,
network calls, subprocesses, environment reads, or credential access.

The test reads the example TOML, reflects over `Config`, creates a temporary
directory, writes a temporary config, and calls the existing loader. `Load`
parses/defaults/validates; it does not start the daemon. Validation checks
filesystem metadata and creates/removes randomly named writability probes.
For the PR's fixture those probes stay in its temporary directory. It neither
generates a CA nor loads a signing keyring. Go excludes `_test.go` from normal
application builds.

The code is appropriate as a documentation regression check, but its claims
are stronger than its guarantees:

| Finding in the PR | Resolution here |
| --- | --- |
| Reflection skips all arrays of tables, so signer/remap/mirror examples are unchecked. | Extract explicitly delimited commented examples, parse them as TOML, check their fields against the schema, and validate them with the active sample. |
| Unknown TOML keys are ignored because `MetaData.Undecoded()` is never checked. | Reject undecoded keys in the active sample and optional examples. |
| Spot checks assert loader defaults, which cannot prove the documented values were parsed. | Compare the entire effective shipped configuration with a minimal loaded config; require every documented option separately. |
| Global replacement of `/var/cache/apt-cacher-ultra` is brittle if paths or examples change. | Decode structurally, replace only `cache.dir`, preserve omitted keys, and reject external certificate/password/CA-directory paths before validation. |
| Existing config test already loads the packaged file. | Preserve that test; add coverage, profile comparison, and optional-example validation rather than another set of spot checks. |

Documentation findings include the admin listener's mutating `POST /reconcile`,
HTTPS downgrade confidentiality, and the limited regex grammar for generated
CA constraints. The rewrite also corrects existing inaccuracies: MITM is
enabled by default; disabling it does not make apt fall back automatically;
`idle_read_timeout` is not enforced; GC requires **twice** the heartbeat
interval to be less than the blob grace; hot prefetch precedes snapshot
publication; and version-aware retention supersedes older URL-TTL comments.
Independent review also found APT's HTTPS proxy inherits its HTTP proxy setting;
HTTP-only caching examples now explicitly set the HTTPS proxy to `DIRECT`.

## Implementation plan

1. Create a dedicated branch and inventory accepted TOML fields, defaults,
   validation, and runtime consumers. Review the original PR without applying it.
2. Rewrite the packaged example with all active scalar/list settings at their
   existing values and all optional repeated tables as valid commented examples.
   Keep comments concise; explain zero/empty meanings and key constraints.
3. Add [the full reference](configuration.md), covering every option, interactions,
   security-sensitive choices, startup behavior, and practical examples. Link it
   from README and the packaged file; correct related README setup guidance.
4. Add the focused Go regression checks described above, using only existing
   dependencies and temporary filesystem state.
5. Document [subscription-free publishing options](documentation-hosting.md),
   including a combined Pages artifact that preserves existing apt URLs.
6. Run independent ultra-effort reviews, revise findings, and repeat until
   consensus, with a maximum of five review rounds. Run configuration tests,
   relevant mutation checks, repository checks, and link/whitespace checks.

## Validation and review outcome

Three independent ultra-effort subagents reviewed the implementation. Round 1
identified prefetch timing, APT HTTPS proxy inheritance, two broken anchors,
client-snippet permissions, systemd path restrictions, and wording about CA
generation versus reuse. Those findings were resolved. All three reviewers
approved round 2 with no remaining material findings; further rounds were
unnecessary.

The example now covers **69 active settings and 6 fields across 3 optional
table types**, in 211 lines versus the original 379. Its effective configuration
matches the pre-change shipped profile. Production code, dependencies, and
publishing workflows are unchanged.

Validation completed:

- `go test ./internal/config -count=1`: passed.
- `go test -race -timeout 10m ./...`: passed, including existing HTTP integration
  tests. The sandbox denied local sockets on the initial run; the successful
  run used permitted loopback access. Go build cache was redirected to `/tmp`.
- `go vet ./...`: passed.
- `golangci-lint run ./cmd/... ./internal/...`: passed, zero issues.
  Repository-wide lint additionally found two pre-existing unchecked `Close`
  calls in the operator's untracked `debug/dbq/main.go`; that file was not edited.
- Five independent mutations in temporary repository copies all failed as
  intended: remove a scalar option, add an unknown option, remove an optional
  table field, misspell an optional table field, and change a shipped value.
- All 17 local Markdown links/anchors checked successfully; `git diff --check`
  and Go formatting checks passed.

The checks verify option coverage, parseability, validation, and the shipped
profile. They do not prove prose is accurate; the independent source reviews
remain necessary when changing behavior. Docker packaging/end-to-end workflows
were not run for this documentation and test-only change.
