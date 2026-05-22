# NOTICE

apt-cacher-ultra is released into the public domain under the
[CC0 1.0 Universal](LICENSE) dedication.

The compiled binary additionally bundles third-party public-key
keyring files via `go:embed`. They live in
[`internal/gpg/embedded/`](internal/gpg/embedded/) and are loaded at
startup as trust anchors for adoption-time GPG verification of
upstream `InRelease` and `Release.gpg` files. The bytes are public
trust material — the same files Debian and Canonical publish in
their archive-keyring packages — and are redistributable without
restriction; we document the sources here so operators can
cross-check the bundled fingerprints against the upstream
publishers.

## Bundled keyrings

| File | Source | Upstream package | Notes |
| --- | --- | --- | --- |
| `ubuntu-archive-keyring.gpg` | Canonical Ltd. | [`ubuntu-keyring`](https://packages.ubuntu.com/source/jammy/ubuntu-keyring) | Ubuntu archive automatic signing keys (2012, 2018) + CD Image (2012) |
| `debian-archive-keyring.gpg` | Debian Project | [`debian-archive-keyring`](https://packages.debian.org/source/sid/debian-archive-keyring) | Debian archive + security + stable-release keys for currently-supported releases (bullseye / bookworm / trixie at time of writing) |
| `ubuntu-pro-esm-apps-keyring.gpg` | Canonical Ltd. | [`ubuntu-pro-client`](https://packages.ubuntu.com/source/jammy/ubuntu-advantage-tools) | Ubuntu Pro ESM Apps automatic signing key |
| `ubuntu-pro-esm-infra-keyring.gpg` | Canonical Ltd. | [`ubuntu-pro-client`](https://packages.ubuntu.com/source/jammy/ubuntu-advantage-tools) | Ubuntu Pro ESM Infra automatic signing key |

The bundled fingerprints are visible on the admin status page at
`:6789/?format=json` (top-level `keyring` array) and rendered in the
HTML status page. Operators are responsible for verifying that the
fingerprints they see there match the values their security policy
expects.

## Refreshing

Run [`scripts/refresh-embedded-keys.sh`](scripts/refresh-embedded-keys.sh)
to re-fetch the current versions of each upstream package and replace
the bundled files. The script prints each refreshed key's primary
fingerprint and UID after copying so the human running it can
cross-check against:

- Ubuntu: https://wiki.ubuntu.com/SecurityTeam/GPGKeyTable
- Debian: https://www.debian.org/CD/verify
- Ubuntu Pro: https://canonical-ubuntu-pro-client.readthedocs-hosted.com/

Distros rotate these keys infrequently (multi-year cadence for the
Debian/Ubuntu archive roots; ESM keys somewhat more often). When a
rotation lands upstream, run the refresh script, verify the new
fingerprints, commit, and ship a new apt-cacher-ultra release.

## Opting out

Operators who want the binary to load only operator-staged keys can
omit the bundled set by setting `keyringEmbeddedSources = nil` in
their own build — that hook is exposed at
[`cmd/apt-cacher-ultra/main.go`](cmd/apt-cacher-ultra/main.go) and is
used by the test suite. In normal deployments the bundled set is
treated as a fallback that on-disk keys override on fingerprint
collision.
