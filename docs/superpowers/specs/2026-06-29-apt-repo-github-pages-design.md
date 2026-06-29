# Design: Publish an apt repository to GitHub Pages

- **Date:** 2026-06-29
- **Status:** Approved (design); pending implementation plan
- **Author:** Sean Reifschneider (with Claude)
- **Topic:** CI-published, GPG-signed apt repo at `https://linsomniac.github.io/apt-cacher-ultra/`

## 1. Goal

Let users install and upgrade `apt-cacher-ultra` with native apt tooling:

```sh
sudo apt-get install apt-cacher-ultra
```

backed by a signed Debian repository served from this repo's GitHub Pages
site. Publishing is automated by GitHub Actions and triggered by each
stable release; no manual repo surgery.

Non-goal: replacing the existing GitHub Releases (they remain the source
of truth for artifacts — see §3).

## 2. Decisions (settled)

| Question | Decision | Rationale |
|---|---|---|
| Hosting model | **Rebuild from Releases**, deployed via GitHub Actions Pages | Stateless / idempotent; no `gh-pages` branch; no binary blobs in git; a re-run fully rebuilds and self-heals. Releases already hold every `.deb`. |
| Retention | **Keep latest 5** versions | Bounds site size; still allows a couple of downgrade/pin steps. Tunable via `KEEP_VERSIONS`. |
| Channels | **Stable only** | `-rcN` prereleases are skipped so apt users never auto-upgrade onto an RC. |
| Trigger | **Approach A**: standalone `pages.yaml`, `workflow_run` after "Release" + `workflow_dispatch` | `release: published` does **not** fire for releases created by `GITHUB_TOKEN` (anti-recursion). `workflow_run` fires regardless; `workflow_dispatch` gives a manual rebuild/self-heal button and day-one population. |
| Signing key | **Dedicated, passphraseless, RSA-4096, no expiry** | RSA-4096 = conservative/broad apt compatibility. A sibling passphrase secret adds ~no security (same secret store). Dedicated key = clean rotation/revocation. |
| Suite layout | Single `stable` suite, `main` component, `binary-amd64` | The binary is static (`CGO_ENABLED=0`), so one distro-agnostic suite is correct. |
| Architecture | **amd64 only** | Matches today's release. arm64 is a trivial future add (pure-Go cross-compile). |
| Pre-deploy gate | **Container smoke test** before deploy | Fits the project's verification culture; nothing is published unless a real `apt-get update`/install against the signed tree succeeds. |
| End-user trust | Modern `signed-by` keyring (no `apt-key`) | `apt-key` is deprecated; key published over GitHub TLS. |

## 3. Architecture overview

```
                      release.yaml (existing, unchanged)
  tag 0.10.5  ───────▶  build .deb + binary + SHA256SUMS
                        attach to GitHub Release  (source of truth)
                                   │
                                   │ workflow_run: "Release" completed (success)
                                   ▼
                      pages.yaml (new)                    ── or workflow_dispatch (manual)
   ┌──────────────────────────────────────────────────────────────────────┐
   │ 1. import APT_GPG_PRIVATE_KEY → derive public key + fingerprint        │
   │ 2. gh release list → drop drafts/prereleases → sort -V → newest 5      │
   │ 3. gh release download each: *_amd64.deb + SHA256SUMS → VERIFY hash    │
   │ 4. scripts/build-apt-repo.sh debs/ site/   (pool + indices + sign)     │
   │ 5. smoke test: serve site/, apt-get update+install in debian container │
   │ 6. upload-pages-artifact(site/) → deploy-pages                         │
   └──────────────────────────────────────────────────────────────────────┘
                                   ▼
        https://linsomniac.github.io/apt-cacher-ultra/   (whole site replaced)
```

`release.yaml` is **not modified**; the pages workflow is fully decoupled
and reads Releases as input.

## 4. Published site layout

```
/                          index.html              landing + copy-paste install instructions
/apt-cacher-ultra.gpg      dearmored public signing key (curl target)
/pool/main/a/apt-cacher-ultra/
    apt-cacher-ultra_0.10.4_amd64.deb
    ... (newest 5 finals)
/dists/stable/
    Release                apt-ftparchive release output (Origin/Label/Suite/...)
    Release.gpg            detached ASCII-armored signature over Release
    InRelease              clearsigned Release (preferred by modern apt)
/dists/stable/main/binary-amd64/
    Packages
    Packages.gz
```

Apt fetches `…/dists/stable/InRelease`; `Filename:` entries in `Packages`
are repo-root-relative (`pool/main/a/…`) and resolve against the base URI.

Both `InRelease` (inline/clearsigned) and `Release` + `Release.gpg`
(detached) are emitted for maximum client compatibility.

## 5. Components

### 5.1 `scripts/build-apt-repo.sh` (new)

Pure, locally runnable; keeps the workflow thin. Mirrors the style of the
existing `e2e/upstream/build-repo.sh`.

- **Usage:** `build-apt-repo.sh <debs-dir> <out-dir>`
- **Env:** `SUITE=stable`, `COMPONENT=main`, `ARCH=amd64`,
  `ORIGIN=apt-cacher-ultra`, `LABEL=apt-cacher-ultra`,
  `GPG_SIGN_KEY=<fingerprint>` (optional — if unset, skip signing for
  local unsigned testing).
- **Behavior:**
  1. Create `pool/$COMPONENT/a/apt-cacher-ultra/` and
     `dists/$SUITE/$COMPONENT/binary-$ARCH/`; copy `*.deb` into the pool.
  2. `apt-ftparchive --arch "$ARCH" packages "pool/$COMPONENT"` →
     `Packages`; `gzip -kf` → `Packages.gz`.
  3. `apt-ftparchive` `release` with
     `-o APT::FTPArchive::Release::{Origin,Label,Suite,Codename,Architectures,Components,Description}`
     → `dists/$SUITE/Release`.
  4. If `GPG_SIGN_KEY` set:
     `gpg --batch --yes --pinentry-mode loopback --local-user "$GPG_SIGN_KEY" --clearsign -o dists/$SUITE/InRelease dists/$SUITE/Release`
     and `… --detach-sign --armor -o dists/$SUITE/Release.gpg …`.
- Run from `<out-dir>` so `apt-ftparchive` emits repo-root-relative paths.

### 5.2 `.github/workflows/pages.yaml` (new)

```yaml
name: Publish apt repo
on:
  workflow_run:
    workflows: ["Release"]
    types: [completed]
  workflow_dispatch:
permissions:
  contents: read
  pages: write
  id-token: write
concurrency:
  group: pages
  cancel-in-progress: false
jobs:
  publish:
    if: >
      github.event_name == 'workflow_dispatch' ||
      github.event.workflow_run.conclusion == 'success'
    runs-on: ubuntu-latest
    timeout-minutes: 20
    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}
    env:
      GH_TOKEN: ${{ github.token }}
      KEEP_VERSIONS: 5
    steps:
      - uses: actions/checkout@v5
      - run: sudo apt-get update && sudo apt-get install -y apt-utils gnupg
      - name: Import signing key + derive public key/fingerprint   # → $GPG_FPR, site/apt-cacher-ultra.gpg
      - name: Select newest N stable releases & download verified .debs
      - name: Build repo                                           # scripts/build-apt-repo.sh debs site
      - name: Smoke test in container                              # gate before deploy
      - uses: actions/upload-pages-artifact@v3
        with: { path: site }
      - id: deploy
        uses: actions/deploy-pages@v4
```

Release selection (step detail):

```sh
mapfile -t TAGS < <(
  gh release list --limit 200 \
    --json tagName,isDraft,isPrerelease \
    --jq '.[] | select(.isDraft|not) | select(.isPrerelease|not) | .tagName' \
  | sort -V | tail -n "${KEEP_VERSIONS}"
)
[ "${#TAGS[@]}" -gt 0 ] || { echo "::error::no stable releases found"; exit 1; }
for tag in "${TAGS[@]}"; do
  gh release download "$tag" -D "debs/$tag" -p '*_amd64.deb' -p 'SHA256SUMS'
  ( cd "debs/$tag" && grep '_amd64\.deb$' SHA256SUMS | sha256sum -c - )
  cp "debs/$tag"/*_amd64.deb debs/
done
```

(`sort -V` orders `0.10.4` above `0.9.8` correctly; finals contain no `-`,
so the prerelease filter + version sort are unambiguous.)

### 5.3 Signing key (operator, run locally — you keep the master)

```sh
cat > keyparams <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 4096
Key-Usage: sign
Name-Real: apt-cacher-ultra apt repository signing key
Name-Email: jafo00@gmail.com
Expire-Date: 0
%commit
EOF
gpg --batch --gen-key keyparams
FPR=$(gpg --with-colons --list-keys jafo00@gmail.com | awk -F: '/^fpr:/{print $10; exit}')
gpg --armor --export-secret-keys "$FPR" > acu-apt-private.asc
gh secret set APT_GPG_PRIVATE_KEY < acu-apt-private.asc
# Back up acu-apt-private.asc offline, then remove working copies.
```

The workflow **derives the published public key from the imported secret**
at build time (`gpg --export "$FPR" > site/apt-cacher-ultra.gpg`), so the
published key is guaranteed to match the signer — no public key is stored
in the repo. The fingerprint is rendered into `index.html` for users who
want to verify.

### 5.4 Landing page `index.html` (templated, built each run)

Static HTML with the install snippet and the key fingerprint substituted
in (placeholder e.g. `@FINGERPRINT@`). Provides both the one-line and
deb822 forms (see §6).

### 5.5 Docs

- README "Install via apt" section (the §6 snippet).
- `docs/apt-repo-runbook.md`: key generation, secret upload, enabling
  Pages, manual rebuild, and key rotation.

## 6. End-user trust & install (modern apt)

One-line form (published on the landing page and in the README):

```sh
sudo install -d -m0755 /etc/apt/keyrings
curl -fsSL https://linsomniac.github.io/apt-cacher-ultra/apt-cacher-ultra.gpg \
  | sudo tee /etc/apt/keyrings/apt-cacher-ultra.gpg >/dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] https://linsomniac.github.io/apt-cacher-ultra/ stable main' \
  | sudo tee /etc/apt/sources.list.d/apt-cacher-ultra.list
sudo apt-get update && sudo apt-get install apt-cacher-ultra
```

deb822 alternative (`/etc/apt/sources.list.d/apt-cacher-ultra.sources`):

```
Types: deb
URIs: https://linsomniac.github.io/apt-cacher-ultra/
Suites: stable
Components: main
Architectures: amd64
Signed-By: /etc/apt/keyrings/apt-cacher-ultra.gpg
```

The key is fetched over GitHub's valid TLS, so `curl | tee` into a keyring
is reasonable.

## 7. Pre-deploy smoke test (gate)

Before deploying, the runner serves the freshly built `site/` over local
HTTP (`python3 -m http.server`) and, in a `debian:stable` container
(`--network host`, with `curl`/`ca-certificates`/`gnupg` installed inside
it first), exercises the **real signed path**:

1. Fetch `apt-cacher-ultra.gpg` from the local server into a keyring.
2. Write a `signed-by` sources entry pointing at the local server.
3. `apt-get update` (verifies InRelease signature + indices).
4. `apt-get install -y --download-only apt-cacher-ultra` (verifies the
   `.deb` is resolvable and fetchable; `--download-only` avoids running
   postinstall, which expects a real systemd host).

Any failure fails the job; `deploy-pages` does not run.

## 8. One-time operator setup

1. Generate the signing key and `gh secret set APT_GPG_PRIVATE_KEY` (§5.3).
2. Enable Pages with source = GitHub Actions:
   `gh api -X POST repos/linsomniac/apt-cacher-ultra/pages -f build_type=workflow`
   (or repo Settings → Pages → Source: GitHub Actions).
3. Merge `pages.yaml`, `scripts/build-apt-repo.sh`, the `index.html`
   template, and docs.
4. Trigger `workflow_dispatch` to populate the repo from the existing
   0.9.x–0.10.4 Releases (no new release needed for day one).
5. Verify install on a clean container/VM.

## 9. Error handling & edge cases

- **Failed Release run** → guarded out by `conclusion == 'success'`.
- **Zero stable releases** → fail loudly; never deploy an empty repo over a
  populated one.
- **Fewer than N finals** → include all available.
- **Deleted/yanked release** → next `workflow_dispatch` reconciles (rebuild
  drops it from the newest-N set).
- **Re-run of an old Release** → harmless; always converges to newest-N.
- **Hash mismatch on a downloaded `.deb`** → `sha256sum -c` fails the job.
- **Tag strings** are read via `gh`/`jq` into quoted array elements, not
  interpolated into shell unquoted.

## 10. Security considerations

- `APT_GPG_PRIVATE_KEY` is referenced only by the import step's `env`;
  never echoed or written outside the ephemeral runner.
- Downloaded `.deb`s are verified against each Release's `SHA256SUMS`
  before they enter the pool.
- `id-token: write` + `pages: write` scoped to the single publish job;
  default `contents: read` otherwise.
- Dedicated signing key → revoke/rotate without collateral.

## 11. Testing strategy

- **Local:** run `scripts/build-apt-repo.sh` over a couple of `.deb`s
  pulled from current Releases (unsigned), point a local apt at it with
  `[trusted=yes]` to confirm index correctness — mirrors `e2e/deb`.
- **CI gate:** the §7 container smoke test exercises the signed path on
  every run and blocks deploy on failure.
- Optional: a small shell test for the newest-N selection logic.

## 12. Out of scope (YAGNI)

- arm64 (easy future add: cross-compile + `binary-arm64`).
- `Acquire-By-Hash` / `by-hash/` (not needed for a small static repo).
- A separate `testing` suite / RC channel (chose stable-only).
- A `gh-pages` branch (chose Actions Pages deploy).
- A keyring `.deb` package (the published `.gpg` + `signed-by` is enough).

## 13. Deliverables

1. `scripts/build-apt-repo.sh`
2. `.github/workflows/pages.yaml`
3. `index.html` template (with `@FINGERPRINT@` substitution)
4. README "Install via apt" section
5. `docs/apt-repo-runbook.md` (key gen, secret, Pages enable, rebuild, rotation)
