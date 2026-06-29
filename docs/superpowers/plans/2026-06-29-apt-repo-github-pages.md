# Apt Repo on GitHub Pages — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically publish a GPG-signed apt repository to `https://linsomniac.github.io/apt-cacher-ultra/` so users can `apt-get install apt-cacher-ultra`.

**Architecture:** A standalone GitHub Actions workflow (`pages.yaml`) rebuilds the repo statelessly on every successful "Release" run (and on manual dispatch): it downloads the newest 5 stable `.deb`s already attached to GitHub Releases, verifies them against each release's `SHA256SUMS`, assembles a `pool/` + `dists/stable/` tree with `apt-ftparchive`, GPG-signs it with a key imported from a secret, smoke-tests the signed repo in a clean container, and deploys the whole tree via GitHub Actions Pages. The build/select/smoke logic lives in three small, independently testable `scripts/`, so the workflow stays thin. The existing `release.yaml` is **not modified**.

**Tech Stack:** Bash, `apt-ftparchive` (apt-utils), `dpkg-deb`, `gnupg`, `jq`, `gh` CLI, `python3` (test HTTP server), Docker (smoke test), GitHub Actions (`actions/checkout@v5`, `actions/upload-pages-artifact@v3`, `actions/deploy-pages@v4`).

## Global Constraints

Every task's requirements implicitly include this section. Values are verbatim from the spec (`docs/superpowers/specs/2026-06-29-apt-repo-github-pages-design.md`).

- **Base URL:** `https://linsomniac.github.io/apt-cacher-ultra/`
- **Suite / component / arch:** single `stable` suite, `main` component, `binary-amd64` only.
- **Retention:** newest **5** stable versions (`KEEP_VERSIONS` default `5`, env-tunable).
- **Channels:** stable only — drafts and `-rc*` prereleases are excluded.
- **Signing key:** dedicated, passphraseless, RSA-4096, no expiry. Stored in secret **`APT_GPG_PRIVATE_KEY`** (ASCII-armored private key). The workflow derives the published public key from it at build time.
- **Published public key filename:** `apt-cacher-ultra.gpg` (binary/dearmored), at the site root.
- **Do NOT modify** `.github/workflows/release.yaml`.
- **Scripts** live in `scripts/`; **tests** live in `e2e/apt-repo/`; the **index template** lives in `packaging/apt-repo/`.
- **Released `.deb` asset name** (input from Releases): `apt-cacher-ultra_<version>_amd64.deb` (finals only, so `<version>` == tag, no tilde).
- **Commits:** conventional-commit style (matches repo). End every commit message with the trailer:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- **Branch:** all work lands on `feat/apt-repo-github-pages` (already created; the spec is committed there).

---

## File Structure

| Path | Responsibility |
|---|---|
| `scripts/select-stable-tags.sh` | Pure transform: `gh release list` JSON (stdin) → newest-N stable tag names (stdout). |
| `scripts/build-apt-repo.sh` | Assemble `pool/` + `dists/stable/` from a dir of `.debs`; sign; export pubkey; render `index.html`. |
| `scripts/smoke-test-apt-repo.sh` | Serve a built site over local HTTP and prove a clean Debian container can verify + install from it. Pre-deploy gate. |
| `packaging/apt-repo/index.html.tmpl` | Landing page template (`@FINGERPRINT@` substituted at build time). |
| `.github/workflows/pages.yaml` | Orchestrates: import key → select → download+verify → build → smoke → deploy. |
| `e2e/apt-repo/test.sh` | Aggregator: runs every `test-*.sh` in the dir. |
| `e2e/apt-repo/test-select-tags.sh` | Unit test for `select-stable-tags.sh`. |
| `e2e/apt-repo/test-build-repo.sh` | Unit test for `build-apt-repo.sh` (unsigned). |
| `e2e/apt-repo/test-build-repo-signed.sh` | Unit test for `build-apt-repo.sh` (signed, ephemeral key). |
| `e2e/apt-repo/fixtures/releases.json` | Fixture for the select-tags test. |
| `Makefile` | Add `apt-repo-test` target. |
| `.gitignore` | Ignore local build outputs (`/site/`, `/debs/`, `/tags.txt`). |
| `docs/apt-repo-runbook.md` | Operator runbook (key gen, secret, Pages enable, rebuild, rotation). |
| `README.md` | Add "Install from the apt repository" subsection. |

---

## Task 1: Release selection script + test scaffolding

**Files:**
- Create: `scripts/select-stable-tags.sh`
- Create: `e2e/apt-repo/fixtures/releases.json`
- Create: `e2e/apt-repo/test-select-tags.sh`
- Create: `e2e/apt-repo/test.sh` (aggregator)
- Modify: `Makefile` (add `apt-repo-test` target)

**Interfaces:**
- Produces: `scripts/select-stable-tags.sh <N>` reads a JSON array of `{tagName,isDraft,isPrerelease}` on **stdin**, writes the newest `N` non-draft, non-prerelease `tagName`s to **stdout**, one per line, ascending `sort -V` order. Exit 2 on a non-integer `N`.
- Produces: `make apt-repo-test` runs `bash e2e/apt-repo/test.sh`, which executes every `e2e/apt-repo/test-*.sh` and exits non-zero if any fail.

- [ ] **Step 1: Write the fixture**

Create `e2e/apt-repo/fixtures/releases.json`:

```json
[
  {"tagName": "0.10.4", "isDraft": false, "isPrerelease": false},
  {"tagName": "0.10.4-rc2", "isDraft": false, "isPrerelease": true},
  {"tagName": "0.9.8", "isDraft": false, "isPrerelease": false},
  {"tagName": "0.10.1", "isDraft": false, "isPrerelease": false},
  {"tagName": "0.10.3", "isDraft": false, "isPrerelease": false},
  {"tagName": "0.10.2", "isDraft": false, "isPrerelease": false},
  {"tagName": "0.11.0-draft", "isDraft": true, "isPrerelease": false},
  {"tagName": "0.9.7", "isDraft": false, "isPrerelease": false}
]
```

- [ ] **Step 2: Write the failing test**

Create `e2e/apt-repo/test-select-tags.sh`:

```bash
#!/usr/bin/env bash
# Unit test for scripts/select-stable-tags.sh.
set -euo pipefail
cd "$(dirname "$0")/../.."

fixture=e2e/apt-repo/fixtures/releases.json

# Newest 5 finals, ascending. Finals present: 0.9.7 0.9.8 0.10.1 0.10.2
# 0.10.3 0.10.4 (six). Drop the oldest -> these five:
expected=$'0.9.8\n0.10.1\n0.10.2\n0.10.3\n0.10.4'
got="$(scripts/select-stable-tags.sh 5 < "$fixture")"
if [ "$got" != "$expected" ]; then
    echo "FAIL newest-5: expected:"; printf '%s\n' "$expected"
    echo "got:"; printf '%s\n' "$got"; exit 1
fi

# Fewer-than-N asks return all finals (6 available).
n_all="$(scripts/select-stable-tags.sh 10 < "$fixture" | grep -c .)"
[ "$n_all" = "6" ] || { echo "FAIL fewer-than-N: expected 6, got $n_all"; exit 1; }

# Non-integer N is rejected.
if scripts/select-stable-tags.sh abc < "$fixture" >/dev/null 2>&1; then
    echo "FAIL: non-integer N should exit non-zero"; exit 1
fi

echo "PASS test-select-tags"
```

Create the aggregator `e2e/apt-repo/test.sh`:

```bash
#!/usr/bin/env bash
# Runs every fast (docker-free) apt-repo unit test in this directory.
set -uo pipefail
cd "$(dirname "$0")"
rc=0
for t in test-*.sh; do
    [ -e "$t" ] || continue
    echo "── $t"
    if ! bash "$t"; then rc=1; fi
done
exit "$rc"
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
chmod +x e2e/apt-repo/test.sh e2e/apt-repo/test-select-tags.sh
bash e2e/apt-repo/test-select-tags.sh
```

Expected: FAIL — `scripts/select-stable-tags.sh: No such file or directory` (script not yet created).

- [ ] **Step 4: Implement the script**

Create `scripts/select-stable-tags.sh`:

```bash
#!/usr/bin/env bash
# Reads `gh release list --json tagName,isDraft,isPrerelease` JSON on
# stdin and writes the newest N stable (non-draft, non-prerelease) tag
# names to stdout, one per line, in ascending version order.
#
#   gh release list --json tagName,isDraft,isPrerelease \
#       | scripts/select-stable-tags.sh 5
#
# Pure transform — no network, no gh — so the newest-N selection logic is
# unit-testable against a fixture.
set -euo pipefail

n="${1:?usage: select-stable-tags.sh <N> (JSON on stdin)}"
case "$n" in
    ''|*[!0-9]*) echo "select-stable-tags.sh: N must be a positive integer, got '$n'" >&2; exit 2 ;;
esac

# Drop drafts and prereleases, emit bare tag names. `sort -V` orders
# Debian-ish versions correctly (0.10.4 above 0.9.8); `tail -n N` keeps the
# newest. Finals carry no '-' suffix, so the version sort is unambiguous.
jq -r '.[] | select(.isDraft | not) | select(.isPrerelease | not) | .tagName' \
    | sort -V \
    | tail -n "$n"
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
chmod +x scripts/select-stable-tags.sh
bash e2e/apt-repo/test-select-tags.sh
```

Expected: `PASS test-select-tags`

- [ ] **Step 6: Add the Makefile target**

In `Makefile`, add `apt-repo-test` to the `.PHONY` line (line 1) and append a target. The `.PHONY` line becomes:

```make
.PHONY: build test test-race lint fmt deb e2e deb-test apt-repo-test clean
```

Append after the `deb-test` target (before `clean:`):

```make
# Fast, docker-free unit tests for the apt-repo publishing scripts
# (scripts/select-stable-tags.sh, scripts/build-apt-repo.sh). The
# containerized signature/install gate lives in
# scripts/smoke-test-apt-repo.sh and runs in CI.
apt-repo-test:
	bash e2e/apt-repo/test.sh
```

- [ ] **Step 7: Run via the Makefile target**

```bash
make apt-repo-test
```

Expected: prints `── test-select-tags.sh` then `PASS test-select-tags`, exit 0.

- [ ] **Step 8: Commit**

```bash
git add scripts/select-stable-tags.sh e2e/apt-repo/ Makefile
git commit -m "feat(apt-repo): newest-N stable release selector + test harness" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Build script — unsigned core

**Files:**
- Create: `scripts/build-apt-repo.sh`
- Create: `e2e/apt-repo/test-build-repo.sh`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `scripts/build-apt-repo.sh <debs-dir> <out-dir>` copies `*.deb` from `<debs-dir>` into `<out-dir>/pool/main/a/apt-cacher-ultra/`, writes `<out-dir>/dists/stable/main/binary-amd64/{Packages,Packages.gz}` and `<out-dir>/dists/stable/Release`. Honors env `SUITE`, `COMPONENT`, `ARCH`, `ORIGIN`, `LABEL`, `DESCRIPTION`, `INDEX_TEMPLATE`, and `GPG_SIGN_KEY` (signing is added in Task 3; absent here = unsigned). Exits 2 if `apt-ftparchive` is missing or `<debs-dir>` has no `.deb`.

- [ ] **Step 1: Write the failing test**

Create `e2e/apt-repo/test-build-repo.sh`:

```bash
#!/usr/bin/env bash
# Unit test for scripts/build-apt-repo.sh (unsigned path).
set -euo pipefail
cd "$(dirname "$0")/../.."

command -v dpkg-deb >/dev/null || { echo "SKIP test-build-repo: dpkg-deb missing"; exit 0; }
command -v apt-ftparchive >/dev/null || { echo "SKIP test-build-repo: apt-ftparchive missing (install apt-utils)"; exit 0; }

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
debs="$work/debs"; out="$work/site"; mkdir -p "$debs"

make_deb() { # <version>
    local v="$1" d; d="$(mktemp -d)"
    mkdir -p "$d/DEBIAN"
    cat > "$d/DEBIAN/control" <<EOF
Package: apt-cacher-ultra
Version: $v
Architecture: amd64
Maintainer: test <t@example.invalid>
Description: fixture package
EOF
    dpkg-deb --build "$d" "$debs/apt-cacher-ultra_${v}_amd64.deb" >/dev/null
    rm -rf "$d"
}
make_deb 0.10.3
make_deb 0.10.4

scripts/build-apt-repo.sh "$debs" "$out"

pkgs="$out/dists/stable/main/binary-amd64/Packages"
rel="$out/dists/stable/Release"
test -f "$pkgs"       || { echo "FAIL: no Packages";    exit 1; }
test -f "$pkgs.gz"    || { echo "FAIL: no Packages.gz"; exit 1; }
test -f "$rel"        || { echo "FAIL: no Release";     exit 1; }
grep -q '^Suite: stable'        "$rel"  || { echo "FAIL: Release missing 'Suite: stable'"; exit 1; }
grep -q '^Components: main'      "$rel"  || { echo "FAIL: Release missing 'Components: main'"; exit 1; }
grep -q '^Architectures: amd64' "$rel"  || { echo "FAIL: Release missing 'Architectures: amd64'"; exit 1; }
grep -q '^Package: apt-cacher-ultra' "$pkgs" || { echo "FAIL: package not indexed"; exit 1; }
grep -q '^Filename: pool/main/a/apt-cacher-ultra/apt-cacher-ultra_0.10.4_amd64.deb' "$pkgs" \
    || { echo "FAIL: Filename path not repo-root-relative"; exit 1; }
test -f "$out/pool/main/a/apt-cacher-ultra/apt-cacher-ultra_0.10.4_amd64.deb" \
    || { echo "FAIL: .deb not copied into pool"; exit 1; }

echo "PASS test-build-repo (unsigned)"
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
chmod +x e2e/apt-repo/test-build-repo.sh
bash e2e/apt-repo/test-build-repo.sh
```

Expected: FAIL — `scripts/build-apt-repo.sh: No such file or directory`. (If it prints `SKIP`, install tooling: `sudo apt-get install -y apt-utils dpkg`.)

- [ ] **Step 3: Implement the script (unsigned core)**

Create `scripts/build-apt-repo.sh`:

```bash
#!/usr/bin/env bash
# build-apt-repo.sh assembles a Debian repository under <out-dir> from the
# .deb files in <debs-dir>, ready to deploy to GitHub Pages.
#
#   scripts/build-apt-repo.sh <debs-dir> <out-dir>
#
# Layout produced (single distro-agnostic suite; the binary is static):
#   pool/main/a/apt-cacher-ultra/*.deb
#   dists/stable/main/binary-amd64/{Packages,Packages.gz}
#   dists/stable/Release   (+ InRelease, Release.gpg when signing)
#   apt-cacher-ultra.gpg   (binary public key, when signing)
#   index.html             (rendered from $INDEX_TEMPLATE)
#
# Signing is opt-in via GPG_SIGN_KEY=<fingerprint of an already-imported
# passphraseless secret key>. When unset, the repo is built unsigned for
# local testing (apt `[trusted=yes]`): no InRelease/Release.gpg/pubkey are
# produced and index.html renders a placeholder fingerprint.
#
# Modelled on e2e/upstream/build-repo.sh.
set -euo pipefail

DEBS_DIR="${1:?usage: build-apt-repo.sh <debs-dir> <out-dir>}"
OUT="${2:?usage: build-apt-repo.sh <debs-dir> <out-dir>}"

SUITE="${SUITE:-stable}"
COMPONENT="${COMPONENT:-main}"
ARCH="${ARCH:-amd64}"
ORIGIN="${ORIGIN:-apt-cacher-ultra}"
LABEL="${LABEL:-apt-cacher-ultra}"
DESCRIPTION="${DESCRIPTION:-apt-cacher-ultra package repository}"
INDEX_TEMPLATE="${INDEX_TEMPLATE:-packaging/apt-repo/index.html.tmpl}"

command -v apt-ftparchive >/dev/null \
    || { echo "build-apt-repo.sh: apt-ftparchive not found (install apt-utils)" >&2; exit 2; }

POOL="$OUT/pool/$COMPONENT/a/apt-cacher-ultra"
BINDIR="$OUT/dists/$SUITE/$COMPONENT/binary-$ARCH"
mkdir -p "$POOL" "$BINDIR"

shopt -s nullglob
debs=("$DEBS_DIR"/*.deb)
shopt -u nullglob
[ "${#debs[@]}" -gt 0 ] || { echo "build-apt-repo.sh: no .deb files in $DEBS_DIR" >&2; exit 2; }
cp "${debs[@]}" "$POOL/"

# Packages index. Run from $OUT so Filename: paths are repo-root-relative.
( cd "$OUT" && apt-ftparchive --arch "$ARCH" packages "pool/$COMPONENT" \
    > "dists/$SUITE/$COMPONENT/binary-$ARCH/Packages" )
# -n: deterministic (no name/timestamp in the gzip header).
gzip -kfn9 "$BINDIR/Packages"

# Release file: metadata + index hashes.
conf="$(mktemp)"
trap 'rm -f "$conf"' EXIT
cat > "$conf" <<EOF
APT::FTPArchive::Release::Origin "$ORIGIN";
APT::FTPArchive::Release::Label "$LABEL";
APT::FTPArchive::Release::Suite "$SUITE";
APT::FTPArchive::Release::Codename "$SUITE";
APT::FTPArchive::Release::Architectures "$ARCH";
APT::FTPArchive::Release::Components "$COMPONENT";
APT::FTPArchive::Release::Description "$DESCRIPTION";
EOF
( cd "$OUT" && apt-ftparchive -c "$conf" release "dists/$SUITE" > "dists/$SUITE/Release" )

# --- Signing + pubkey + index rendering are added in Task 3 ---

echo "build-apt-repo.sh: built $SUITE repo with ${#debs[@]} package file(s) at $OUT"
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
chmod +x scripts/build-apt-repo.sh
bash e2e/apt-repo/test-build-repo.sh
```

Expected: `PASS test-build-repo (unsigned)`

- [ ] **Step 5: Ignore local build outputs**

Append to `.gitignore`:

```gitignore
/site/
/debs/
/tags.txt
```

- [ ] **Step 6: Commit**

```bash
git add scripts/build-apt-repo.sh e2e/apt-repo/test-build-repo.sh .gitignore
git commit -m "feat(apt-repo): assemble pool + dists with apt-ftparchive" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Build script — signing, public key, landing page

**Files:**
- Modify: `scripts/build-apt-repo.sh` (replace the `--- Signing ... ---` marker)
- Create: `packaging/apt-repo/index.html.tmpl`
- Create: `e2e/apt-repo/test-build-repo-signed.sh`

**Interfaces:**
- Consumes: `scripts/build-apt-repo.sh` from Task 2.
- Produces: when `GPG_SIGN_KEY=<fpr>` is set, `build-apt-repo.sh` additionally writes `<out>/dists/stable/InRelease` (clearsigned), `<out>/dists/stable/Release.gpg` (detached, armored), `<out>/apt-cacher-ultra.gpg` (binary public key), and `<out>/index.html` (template with `@FINGERPRINT@` → the fingerprint). Unsigned builds render `@FINGERPRINT@` → `UNSIGNED`.

- [ ] **Step 1: Create the landing page template**

Create `packaging/apt-repo/index.html.tmpl`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>apt-cacher-ultra apt repository</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, sans-serif; max-width: 52rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  pre { background: rgba(127,127,127,.12); padding: 1rem; border-radius: 6px; overflow-x: auto; }
  h1 { margin-bottom: .25rem; }
  .fpr { word-break: break-all; }
  a { color: inherit; }
</style>
</head>
<body>
<h1>apt-cacher-ultra</h1>
<p>APT package repository for
  <a href="https://github.com/linsomniac/apt-cacher-ultra">apt-cacher-ultra</a>
  — a robust apt cache focused on availability under upstream failure.</p>

<h2>Install</h2>
<pre><code>sudo install -d -m0755 /etc/apt/keyrings
curl -fsSL https://linsomniac.github.io/apt-cacher-ultra/apt-cacher-ultra.gpg \
  | sudo tee /etc/apt/keyrings/apt-cacher-ultra.gpg &gt;/dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] https://linsomniac.github.io/apt-cacher-ultra/ stable main' \
  | sudo tee /etc/apt/sources.list.d/apt-cacher-ultra.list
sudo apt-get update
sudo apt-get install apt-cacher-ultra</code></pre>

<h2>deb822 alternative</h2>
<p>Save as <code>/etc/apt/sources.list.d/apt-cacher-ultra.sources</code>:</p>
<pre><code>Types: deb
URIs: https://linsomniac.github.io/apt-cacher-ultra/
Suites: stable
Components: main
Architectures: amd64
Signed-By: /etc/apt/keyrings/apt-cacher-ultra.gpg</code></pre>

<h2>Signing key</h2>
<p class="fpr">This repository is signed by GPG key fingerprint:<br>
  <code>@FINGERPRINT@</code></p>
<p>Only <code>amd64</code> is published. The repository keeps the most recent
  stable releases; prereleases are not published here.</p>
</body>
</html>
```

- [ ] **Step 2: Write the failing signed test**

Create `e2e/apt-repo/test-build-repo-signed.sh`:

```bash
#!/usr/bin/env bash
# Unit test for scripts/build-apt-repo.sh (signed path). Generates an
# ephemeral, disposable signing key (like e2e/upstream/build-repo.sh).
set -euo pipefail
cd "$(dirname "$0")/../.."

command -v dpkg-deb >/dev/null       || { echo "SKIP signed: dpkg-deb missing"; exit 0; }
command -v apt-ftparchive >/dev/null || { echo "SKIP signed: apt-ftparchive missing"; exit 0; }
command -v gpg >/dev/null            || { echo "SKIP signed: gpg missing"; exit 0; }

work="$(mktemp -d)"
export GNUPGHOME="$work/gnupg"; mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"
trap 'rm -rf "$work"' EXIT
debs="$work/debs"; out="$work/site"; mkdir -p "$debs"

d="$(mktemp -d)"; mkdir -p "$d/DEBIAN"
cat > "$d/DEBIAN/control" <<EOF
Package: apt-cacher-ultra
Version: 0.10.4
Architecture: amd64
Maintainer: test <t@example.invalid>
Description: fixture package
EOF
dpkg-deb --build "$d" "$debs/apt-cacher-ultra_0.10.4_amd64.deb" >/dev/null; rm -rf "$d"

gpg --batch --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'acu test <test@example.invalid>' rsa3072 sign 0
fpr="$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/{print $10; exit}')"
[ -n "$fpr" ] || { echo "FAIL: could not create ephemeral key"; exit 1; }

GPG_SIGN_KEY="$fpr" scripts/build-apt-repo.sh "$debs" "$out"

inrel="$out/dists/stable/InRelease"
test -f "$inrel"                       || { echo "FAIL: no InRelease"; exit 1; }
head -1 "$inrel" | grep -q 'BEGIN PGP SIGNED MESSAGE' \
                                       || { echo "FAIL: InRelease not clearsigned"; exit 1; }
test -f "$out/dists/stable/Release.gpg" || { echo "FAIL: no Release.gpg"; exit 1; }
test -f "$out/apt-cacher-ultra.gpg"     || { echo "FAIL: no exported public key"; exit 1; }
gpg --verify "$inrel" >/dev/null 2>&1   || { echo "FAIL: InRelease signature does not verify"; exit 1; }
grep -q "$fpr" "$out/index.html"        || { echo "FAIL: index.html missing fingerprint"; exit 1; }

echo "PASS test-build-repo (signed)"
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
chmod +x e2e/apt-repo/test-build-repo-signed.sh
bash e2e/apt-repo/test-build-repo-signed.sh
```

Expected: FAIL — `no InRelease` (signing not yet implemented; the build runs but produces no signature/pubkey/index).

- [ ] **Step 4: Implement signing + pubkey + index rendering**

In `scripts/build-apt-repo.sh`, replace the line:

```bash
# --- Signing + pubkey + index rendering are added in Task 3 ---
```

with:

```bash
# Signing (opt-in). The key is a passphraseless secret already imported
# into the gpg keyring; --passphrase '' + loopback keeps gpg non-interactive.
FINGERPRINT="UNSIGNED"
if [ -n "${GPG_SIGN_KEY:-}" ]; then
    FINGERPRINT="$GPG_SIGN_KEY"
    gpg --batch --yes --pinentry-mode loopback --passphrase '' \
        --default-key "$GPG_SIGN_KEY" --clearsign \
        --output "$OUT/dists/$SUITE/InRelease" "$OUT/dists/$SUITE/Release"
    gpg --batch --yes --pinentry-mode loopback --passphrase '' \
        --default-key "$GPG_SIGN_KEY" --detach-sign --armor \
        --output "$OUT/dists/$SUITE/Release.gpg" "$OUT/dists/$SUITE/Release"
    # Binary (dearmored) public key for /etc/apt/keyrings/.
    gpg --batch --yes --output "$OUT/apt-cacher-ultra.gpg" --export "$GPG_SIGN_KEY"
fi

# Landing page (fingerprint substituted; '|' delimiter avoids clashing
# with the hex fingerprint).
if [ -f "$INDEX_TEMPLATE" ]; then
    sed "s|@FINGERPRINT@|$FINGERPRINT|g" "$INDEX_TEMPLATE" > "$OUT/index.html"
fi
```

- [ ] **Step 5: Run the signed test to verify it passes**

```bash
bash e2e/apt-repo/test-build-repo-signed.sh
```

Expected: `PASS test-build-repo (signed)`

- [ ] **Step 6: Run the full fast suite (no regressions)**

```bash
make apt-repo-test
```

Expected: three `──` lines; `PASS test-select-tags`, `PASS test-build-repo (unsigned)`, `PASS test-build-repo (signed)`; exit 0.

- [ ] **Step 7: Commit**

```bash
git add scripts/build-apt-repo.sh packaging/apt-repo/index.html.tmpl e2e/apt-repo/test-build-repo-signed.sh
git commit -m "feat(apt-repo): GPG-sign repo, export public key, render landing page" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Pre-deploy container smoke test

**Files:**
- Create: `scripts/smoke-test-apt-repo.sh`

**Interfaces:**
- Consumes: a signed site dir from `build-apt-repo.sh` (must contain `apt-cacher-ultra.gpg` and `dists/stable/InRelease`).
- Produces: `scripts/smoke-test-apt-repo.sh <site-dir>` serves the dir over local HTTP (env `PORT`, default `8089`) and runs a clean `debian:stable` container (env `IMAGE`) that fetches the key, configures a `signed-by` source, `apt-get update`s (verifying the signature), and `apt-get install --download-only apt-cacher-ultra`. Exit 0 on success, non-zero on any failure. Exits 2 if docker/python3 are missing or the site is unsigned.

- [ ] **Step 1: Implement the smoke-test script**

Create `scripts/smoke-test-apt-repo.sh`:

```bash
#!/usr/bin/env bash
# Serve a freshly built apt repo over local HTTP and prove a clean Debian
# container can verify its signature and install from it. Used as the
# pre-deploy gate in pages.yaml and runnable locally. Requires docker +
# python3 (both preinstalled on GitHub's ubuntu-latest runners).
#
#   scripts/smoke-test-apt-repo.sh <site-dir>
set -euo pipefail

SITE="${1:?usage: smoke-test-apt-repo.sh <site-dir>}"
PORT="${PORT:-8089}"
IMAGE="${IMAGE:-debian:stable}"

command -v docker  >/dev/null || { echo "smoke-test: docker not found"  >&2; exit 2; }
command -v python3 >/dev/null || { echo "smoke-test: python3 not found" >&2; exit 2; }
test -f "$SITE/apt-cacher-ultra.gpg" \
    || { echo "smoke-test: $SITE is unsigned (no apt-cacher-ultra.gpg)" >&2; exit 2; }

python3 -m http.server "$PORT" --directory "$SITE" >/dev/null 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT

# Wait until the InRelease file is actually served.
ok=0
for _ in $(seq 1 50); do
    if curl -fsS "http://localhost:$PORT/dists/stable/InRelease" -o /dev/null 2>/dev/null; then
        ok=1; break
    fi
    sleep 0.2
done
[ "$ok" = 1 ] || { echo "smoke-test: local server never came up on :$PORT" >&2; exit 1; }

# --network host so the container reaches the runner's localhost server.
docker run --rm --network host -e BASE="http://localhost:$PORT" "$IMAGE" bash -c '
    set -euo pipefail
    apt-get update -qq
    apt-get install -y -qq curl ca-certificates gnupg >/dev/null
    install -d -m0755 /etc/apt/keyrings
    curl -fsSL "$BASE/apt-cacher-ultra.gpg" -o /etc/apt/keyrings/apt-cacher-ultra.gpg
    echo "deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] $BASE stable main" \
        > /etc/apt/sources.list.d/apt-cacher-ultra.list
    apt-get update
    # --download-only: prove the package is trusted, resolvable and
    # fetchable without running the postinstall (which expects systemd).
    apt-get install -y --download-only apt-cacher-ultra
    echo "smoke-test: container verified signature and resolved apt-cacher-ultra"
'
echo "smoke-test: OK"
```

- [ ] **Step 2: Build a signed fixture site to test against**

```bash
chmod +x scripts/smoke-test-apt-repo.sh
export GNUPGHOME="$(mktemp -d)"; chmod 700 "$GNUPGHOME"
gpg --batch --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'acu smoke <smoke@example.invalid>' rsa3072 sign 0
FPR="$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/{print $10; exit}')"
SMOKE="$(mktemp -d)"; mkdir -p "$SMOKE/debs"
D="$(mktemp -d)"; mkdir -p "$D/DEBIAN"
printf 'Package: apt-cacher-ultra\nVersion: 0.10.4\nArchitecture: amd64\nMaintainer: t <t@x.invalid>\nDescription: fixture\n' > "$D/DEBIAN/control"
dpkg-deb --build "$D" "$SMOKE/debs/apt-cacher-ultra_0.10.4_amd64.deb" >/dev/null
GPG_SIGN_KEY="$FPR" scripts/build-apt-repo.sh "$SMOKE/debs" "$SMOKE/site"
```

- [ ] **Step 3: Verify the gate PASSES on a good repo**

```bash
scripts/smoke-test-apt-repo.sh "$SMOKE/site"; echo "exit=$?"
```

Expected: ends with `smoke-test: OK` and `exit=0`. (Requires a working `docker`; on a host without docker the script prints `smoke-test: docker not found` and exits 2 — run this step where docker is available, e.g. CI or the dev box.)

- [ ] **Step 4: Verify the gate FAILS on a tampered repo**

```bash
# Corrupt the signed payload so the signature no longer matches.
printf '\nTAMPERED\n' >> "$SMOKE/site/dists/stable/InRelease"
scripts/smoke-test-apt-repo.sh "$SMOKE/site"; echo "exit=$?"
```

Expected: non-zero `exit=` (apt rejects the bad signature / the package is untrusted, the container exits non-zero, the script propagates it). This proves the gate actually gates.

- [ ] **Step 5: Clean up the fixtures**

```bash
rm -rf "$SMOKE" "$GNUPGHOME"; unset GNUPGHOME SMOKE FPR D
```

- [ ] **Step 6: Commit**

```bash
git add scripts/smoke-test-apt-repo.sh
git commit -m "feat(apt-repo): containerized pre-deploy signature/install smoke test" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Publish workflow

**Files:**
- Create: `.github/workflows/pages.yaml`

**Interfaces:**
- Consumes: `scripts/select-stable-tags.sh`, `scripts/build-apt-repo.sh`, `scripts/smoke-test-apt-repo.sh`, secret `APT_GPG_PRIVATE_KEY`.
- Produces: a GitHub Pages deployment of the built site. No code depends on this task.

- [ ] **Step 1: Author the workflow**

Create `.github/workflows/pages.yaml`:

```yaml
name: Publish apt repo

# release.yaml creates the GitHub Release with GITHUB_TOKEN, which by
# design does NOT emit a release:published event (anti-recursion). So we
# chain off the Release workflow's completion via workflow_run, and also
# expose a manual button for rebuilds and day-one population. An RC tag
# also triggers Release (and therefore this) but the rebuild only ever
# republishes the current stable set, so it is an idempotent no-op.
on:
  workflow_run:
    workflows: ["Release"]
    types: [completed]
  workflow_dispatch:

permissions:
  contents: read   # gh release list/download
  pages: write     # deploy-pages
  id-token: write  # deploy-pages OIDC

# Never run two Pages deploys at once; queue rather than cancel an
# in-flight publish.
concurrency:
  group: pages
  cancel-in-progress: false

jobs:
  publish:
    # Skip when the triggering Release run did not succeed. Manual
    # dispatch always proceeds.
    if: >-
      github.event_name == 'workflow_dispatch' ||
      github.event.workflow_run.conclusion == 'success'
    runs-on: ubuntu-latest
    timeout-minutes: 20
    environment:
      name: github-pages
      url: ${{ steps.deploy.outputs.page_url }}
    env:
      GH_TOKEN: ${{ github.token }}
      KEEP_VERSIONS: '5'
    steps:
      - uses: actions/checkout@v5

      - name: Install apt repo tooling
        run: sudo apt-get update && sudo apt-get install -y apt-utils gnupg

      - name: Import signing key
        env:
          APT_GPG_PRIVATE_KEY: ${{ secrets.APT_GPG_PRIVATE_KEY }}
        run: |
          set -euo pipefail
          if [ -z "${APT_GPG_PRIVATE_KEY:-}" ]; then
            echo "::error::APT_GPG_PRIVATE_KEY secret is not set"; exit 1
          fi
          printf '%s\n' "$APT_GPG_PRIVATE_KEY" | gpg --batch --import
          fpr="$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/{print $10; exit}')"
          [ -n "$fpr" ] || { echo "::error::no secret key after import"; exit 1; }
          echo "GPG_FPR=$fpr" >> "$GITHUB_ENV"

      - name: Select newest stable releases and download verified .debs
        run: |
          set -euo pipefail
          gh release list --limit 200 --json tagName,isDraft,isPrerelease \
            | scripts/select-stable-tags.sh "$KEEP_VERSIONS" > tags.txt
          if [ ! -s tags.txt ]; then
            echo "::error::no stable releases found to publish"; exit 1
          fi
          echo "Publishing these tags:"; cat tags.txt
          mkdir -p debs
          while read -r tag; do
            [ -n "$tag" ] || continue
            dir="debs/$tag"; mkdir -p "$dir"
            gh release download "$tag" -D "$dir" -p '*_amd64.deb' -p 'SHA256SUMS'
            ( cd "$dir" && grep '_amd64\.deb$' SHA256SUMS | sha256sum -c - )
            cp "$dir"/*_amd64.deb debs/
          done < tags.txt

      - name: Build signed apt repo
        run: GPG_SIGN_KEY="$GPG_FPR" scripts/build-apt-repo.sh debs site

      - name: Smoke test (signed install in a clean container)
        run: scripts/smoke-test-apt-repo.sh site

      - uses: actions/upload-pages-artifact@v3
        with:
          path: site

      - name: Deploy to GitHub Pages
        id: deploy
        uses: actions/deploy-pages@v4
```

- [ ] **Step 2: Lint the workflow YAML**

```bash
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/pages.yaml")); print("yaml ok")'
# If actionlint is installed, also run it (catches expression/context errors):
command -v actionlint >/dev/null && actionlint .github/workflows/pages.yaml || echo "actionlint not installed (optional)"
```

Expected: `yaml ok`; if present, `actionlint` reports no errors.

- [ ] **Step 3: Sanity-check the scripts the workflow calls exist and are executable**

```bash
ls -l scripts/select-stable-tags.sh scripts/build-apt-repo.sh scripts/smoke-test-apt-repo.sh
```

Expected: all three present with the executable bit set.

> **Note:** the workflow cannot be fully exercised until it is on the default branch and the `APT_GPG_PRIVATE_KEY` secret + Pages are configured (see "Manual operator steps"). Its constituent scripts are already covered by Tasks 1–4; first end-to-end proof is the manual `workflow_dispatch` in the operator steps.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/pages.yaml
git commit -m "feat(apt-repo): publish workflow (rebuild from Releases -> Pages)" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Documentation — runbook + README

**Files:**
- Create: `docs/apt-repo-runbook.md`
- Modify: `README.md` (new subsection under `## Quickstart`, before `### As Deb Package:` at line 61)

**Interfaces:** none (docs only).

- [ ] **Step 1: Write the operator runbook**

Create `docs/apt-repo-runbook.md`:

````markdown
# apt repository (GitHub Pages) — operator runbook

The repo at <https://linsomniac.github.io/apt-cacher-ultra/> is rebuilt and
deployed by `.github/workflows/pages.yaml`. It is **stateless**: every run
re-derives the newest 5 stable releases from GitHub Releases, so re-running
always converges and self-heals. See the design spec at
`docs/superpowers/specs/2026-06-29-apt-repo-github-pages-design.md`.

## One-time setup

### 1. Generate the signing key (do this locally; keep the master)

Dedicated, passphraseless, RSA-4096, no expiry — used only for this repo.

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
echo "fingerprint: $FPR"
gpg --armor --export-secret-keys "$FPR" > acu-apt-private.asc
```

Back up `acu-apt-private.asc` somewhere offline (e.g. a password manager),
then remove the working copy when done.

### 2. Upload the private key as a repo secret

```sh
gh secret set APT_GPG_PRIVATE_KEY < acu-apt-private.asc
rm -f keyparams acu-apt-private.asc
```

The workflow imports this key and derives the published public key
(`apt-cacher-ultra.gpg`) from it, so the served key always matches the signer.

### 3. Enable GitHub Pages with the Actions source

```sh
gh api -X POST repos/linsomniac/apt-cacher-ultra/pages -f build_type=workflow
```

(Or repo Settings → Pages → Source: **GitHub Actions**. If it already exists,
use `gh api -X PUT .../pages -f build_type=workflow`.)

### 4. Publish for the first time

Once `pages.yaml` is on the default branch, trigger a manual run — this
populates the repo from the existing Releases without cutting a new one:

```sh
gh workflow run "Publish apt repo"
gh run watch "$(gh run list --workflow 'Publish apt repo' --limit 1 --json databaseId --jq '.[0].databaseId')"
```

### 5. Verify on a clean machine

```sh
sudo install -d -m0755 /etc/apt/keyrings
curl -fsSL https://linsomniac.github.io/apt-cacher-ultra/apt-cacher-ultra.gpg \
  | sudo tee /etc/apt/keyrings/apt-cacher-ultra.gpg >/dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] https://linsomniac.github.io/apt-cacher-ultra/ stable main' \
  | sudo tee /etc/apt/sources.list.d/apt-cacher-ultra.list
sudo apt-get update && sudo apt-get install apt-cacher-ultra
```

## Routine operation

- **New stable release:** push the version tag as usual. `release.yaml` builds
  and attaches the `.deb`; on its success `pages.yaml` runs via `workflow_run`
  and republishes the newest 5 stable versions. Nothing else to do.
- **Manual rebuild / self-heal:** `gh workflow run "Publish apt repo"`. Safe to
  run any time; it is idempotent.
- **Changed retention:** edit `KEEP_VERSIONS` in `.github/workflows/pages.yaml`.
- **Yanked a release:** delete the GitHub Release, then run a manual rebuild;
  the dropped version falls out of the newest-5 set.

## Key rotation

1. Generate a new key (step 1) and `gh secret set APT_GPG_PRIVATE_KEY` with it.
2. Manual rebuild (`gh workflow run "Publish apt repo"`) — the repo is re-signed
   and the new public key is published.
3. Users re-fetch the key (step 5). Until they do, `apt-get update` will report a
   signature error, so announce rotations.

## Troubleshooting

- **`deploy-pages` fails with a Pages error:** Pages isn't enabled with the
  Actions source — redo step 3.
- **`no secret key after import`:** `APT_GPG_PRIVATE_KEY` is missing/!armored —
  redo step 2 with `gpg --armor --export-secret-keys`.
- **`no stable releases found`:** every release is a draft/prerelease; publish a
  final release first.
- **Smoke test fails:** inspect the failed step's container log; a signature
  error means the imported key and the published key disagree (shouldn't happen
  since the pubkey is derived from the secret) — check the import step.
````

- [ ] **Step 2: Add the README install subsection**

In `README.md`, insert immediately after line 60 (the blank line under `## Quickstart`) and before `### As Deb Package:`:

```markdown
### Install from the apt repository (recommended):

```sh
sudo install -d -m0755 /etc/apt/keyrings
curl -fsSL https://linsomniac.github.io/apt-cacher-ultra/apt-cacher-ultra.gpg \
  | sudo tee /etc/apt/keyrings/apt-cacher-ultra.gpg >/dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] https://linsomniac.github.io/apt-cacher-ultra/ stable main' \
  | sudo tee /etc/apt/sources.list.d/apt-cacher-ultra.list
sudo apt-get update
sudo apt-get install apt-cacher-ultra
#  EDIT: /etc/apt-cacher-ultra/config.toml
sudo systemctl enable --now apt-cacher-ultra
```

Prebuilt `amd64` packages are published to
<https://linsomniac.github.io/apt-cacher-ultra/> (newest stable releases).

```

- [ ] **Step 3: Verify the docs render and are internally consistent**

```bash
# No leftover placeholders in the new docs.
! grep -RnE '@FINGERPRINT@|TODO|TBD' docs/apt-repo-runbook.md
# README mentions the new repo URL and the secret name matches the workflow.
grep -q 'linsomniac.github.io/apt-cacher-ultra' README.md && echo "README ok"
grep -q 'APT_GPG_PRIVATE_KEY' docs/apt-repo-runbook.md \
  && grep -q 'APT_GPG_PRIVATE_KEY' .github/workflows/pages.yaml && echo "secret name consistent"
```

Expected: `README ok` and `secret name consistent`; the `grep` for placeholders prints nothing and returns success.

- [ ] **Step 4: Commit**

```bash
git add docs/apt-repo-runbook.md README.md
git commit -m "docs(apt-repo): operator runbook + README install instructions" \
  -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Manual operator steps (human, after the branch is merged)

These cannot be done by the implementing engineer — they need the maintainer's
key material and repo admin, and the workflow must be on the default branch.
They are documented in full in `docs/apt-repo-runbook.md`:

1. Generate the RSA-4096 passphraseless signing key locally; note the fingerprint.
2. `gh secret set APT_GPG_PRIVATE_KEY < acu-apt-private.asc`.
3. Enable Pages: `gh api -X POST repos/linsomniac/apt-cacher-ultra/pages -f build_type=workflow`.
4. Merge `feat/apt-repo-github-pages` to `main`.
5. `gh workflow run "Publish apt repo"` to populate from existing Releases.
6. Verify `apt-get install apt-cacher-ultra` on a clean container/VM.

---

## Self-Review

**Spec coverage** (against `2026-06-29-apt-repo-github-pages-design.md`):

- §2 rebuild-from-Releases, keep-5, stable-only, Approach A → Task 5 workflow + Task 1 selector. ✓
- §4 site layout (pool/dists/InRelease/Release.gpg/pubkey/index) → Tasks 2–3. ✓
- §5.1 `build-apt-repo.sh` → Tasks 2–3. ✓
- §5.2 `pages.yaml` (triggers, perms, concurrency, select+verify) → Task 5. ✓
- §5.3 key import + public-key derivation → Task 5 import step + Task 3 export. ✓
- §5.4 templated `index.html` → Task 3. ✓
- §5.5 README + runbook → Task 6. ✓
- §6 modern `signed-by` install → Task 3 template, Task 6 README/runbook. ✓
- §7 container smoke test → Task 4 + Task 5 step. ✓
- §8 one-time setup → "Manual operator steps" + runbook. ✓
- §9 edge cases (zero finals fail, hash verify, fewer-than-N) → Task 5 guard, Task 1 test. ✓
- §10 security (scoped secret, SHA256 verify, scoped perms) → Task 5. ✓
- §11 testing (local unsigned, CI smoke, selection unit test) → Tasks 1–4. ✓
- §12 out-of-scope honored (amd64 only, single suite, no gh-pages, no by-hash). ✓

**Placeholder scan:** `@FINGERPRINT@` is the one intentional template token, substituted in Task 3 and asserted in `test-build-repo-signed.sh`. No TBD/TODO/"handle errors"/"similar to" left in steps. ✓

**Type/name consistency:** secret `APT_GPG_PRIVATE_KEY`, env `GPG_SIGN_KEY`/`GPG_FPR`/`KEEP_VERSIONS`, pubkey `apt-cacher-ultra.gpg`, suite `stable`, pool path `pool/main/a/apt-cacher-ultra/`, and the `Filename:` assertion all match across Tasks 1–6 and the workflow. The `.deb` asset glob `*_amd64.deb` matches the release.yaml naming confirmed at `release.yaml:118,141`. ✓

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-29-apt-repo-github-pages.md`. Two execution options:

1. **Subagent-Driven (recommended)** — a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
