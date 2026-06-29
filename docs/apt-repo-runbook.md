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
