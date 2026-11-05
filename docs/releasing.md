# Releasing apt-cacher-ultra

The steps for every release, on the commit you tag. The release workflow
(`.github/workflows/release.yaml`) builds and publishes from the tag and
runs `go test -race ./...` and the test-seam check; it runs none of the
manual checks below, so they gate the tag. A failing step blocks the tag
until its README's procedure says what to do.

1. **CI is green on the commit.** `ci.yaml` runs on pushes to `main` and on
   pull requests: `go vet`, `go test -race`, golangci-lint (twice),
   `make shellcheck`, the e2e suite, e2e/deb, the `e2e-flat` matrix and
   `make apt-repo-test`. A tag on another branch (a hotfix) gets none of
   them from the release workflow: run them, or push the branch through a
   pull request first.
2. **Pull the client images.** `docker run` never refreshes a cached tag, so
   a run without the pull measures the old images again and passes by
   construction. The eleven images and the loop are in
   [e2e/apt-oracle/README.md](../e2e/apt-oracle/README.md), *Running it*.
3. **The apt oracles:** `make e2e-apt-oracle` (the six current client
   images) and `make e2e-apt-oracle-old` (the five older ones). Each must
   pass: the images' apt still gives the verdicts and error classes the
   corpus records. A difference blocks the release until it is recorded
   and the table test agrees (the README's *When apt changes*). Add the
   runs' row to its *Results*.
4. **`make e2e-live`**, for a release that changes the request path,
   adoption or vouching (and before 1.1.0): the live-upstream smoke against
   five real vendors ([e2e/live/README.md](../e2e/live/README.md)). Add its
   row to that file's *Results*.
5. **The vendor anchors.** `go test ./internal/gpg` (step 1) verifies the
   committed vendor fixtures (`internal/gpg/testdata/vendors`) and pins each
   one's strong signer. The wider vendor survey the GC and upstream
   hardening plan re-ran at every verifier change uses anchors kept outside
   the tree, so it is not a release step; re-survey real vendors after a
   go-crypto upgrade or a verifier change, as that plan did.
6. **The release notes** (`docs/release-notes/<version>.md`), brought up to
   date with [Changelog.md](../Changelog.md), then pasted into the GitHub
   release. For a release candidate, replace the version in their links
   (`docs/open-decisions.md`, *Before the 1.1.0 release candidate*, item 7,
   gives the commands, and the unwrapping for GitHub's editor).
7. **Tag** the commit (`MAJOR.MINOR.PATCH`, or `-rcN`; no `v` prefix) and
   check the release workflow's `Verify no e2e test seam` step on the tag.

Why each check exists: `docs/open-decisions.md`, *Decided*: "The apt
oracle runs before every release, both sets of images" and "`make
e2e-live` runs before 1.1.0, and before later releases that change the
request path, adoption or vouching".
