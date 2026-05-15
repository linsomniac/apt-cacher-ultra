#!/bin/bash
# refresh-embedded-keys.sh
#
# Refresh the canonical archive keys bundled into the binary via
# go:embed (see internal/gpg/embedded.go). These keys provide
# zero-config trust for stock Ubuntu, Debian, and Ubuntu Pro ESM
# repositories — even on hosts where /etc/apt/* is unpopulated.
#
# Run this script when distros rotate a root key (Ubuntu archive
# keys turn over every ~5–10 years; ESM keys more frequently). The
# bundled .gpg files are checked into the repo at
# internal/gpg/embedded/*.gpg and become part of the binary on the
# next build.
#
# Requirements: dpkg-deb, curl, apt-get download (only for
# ubuntu-keyring and the ESM client; Debian's keyring is fetched
# directly from ftp.debian.org).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EMBED_DIR="$REPO_ROOT/internal/gpg/embedded"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Working in $WORK_DIR"
mkdir -p "$EMBED_DIR"

# -- Ubuntu archive keyring -------------------------------------------------
echo "==> Downloading ubuntu-keyring"
( cd "$WORK_DIR" && apt-get download ubuntu-keyring )
( cd "$WORK_DIR" && dpkg-deb -x ubuntu-keyring_*.deb ubuntu-keyring-x )
cp "$WORK_DIR/ubuntu-keyring-x/usr/share/keyrings/ubuntu-archive-keyring.gpg" \
   "$EMBED_DIR/ubuntu-archive-keyring.gpg"
echo "    -> $EMBED_DIR/ubuntu-archive-keyring.gpg"

# -- Debian archive keyring (current) ---------------------------------------
# Fetched directly from the Debian pool because Ubuntu's mirror lags
# (jammy ships the 2021 release; we want the active 2025+ keyring
# which covers bookworm + trixie + bullseye).
echo "==> Locating current debian-archive-keyring"
DEB_URL_BASE="https://ftp.debian.org/debian/pool/main/d/debian-archive-keyring"
LATEST_DEB=$(curl -sf "$DEB_URL_BASE/" \
  | grep -oE 'debian-archive-keyring_[0-9.+a-z]+_all\.deb' \
  | sort -V | tail -1)
echo "    -> $LATEST_DEB"
curl -sf -o "$WORK_DIR/$LATEST_DEB" "$DEB_URL_BASE/$LATEST_DEB"
( cd "$WORK_DIR" && dpkg-deb -x "$LATEST_DEB" debian-keyring-x )
cp "$WORK_DIR/debian-keyring-x/usr/share/keyrings/debian-archive-keyring.gpg" \
   "$EMBED_DIR/debian-archive-keyring.gpg"
echo "    -> $EMBED_DIR/debian-archive-keyring.gpg"

# -- Ubuntu Pro ESM keys ----------------------------------------------------
# The ubuntu-pro-client package ships per-service signing keys
# (apps, infra). We bundle apps + infra; the FIPS / CIS / Anbox /
# realtime / CC-EAL keys are excluded — operators using those
# services should install ubuntu-pro-client locally and the loader
# will pick them up from /usr/share/keyrings/.
echo "==> Downloading ubuntu-pro-client"
( cd "$WORK_DIR" && apt-get download ubuntu-pro-client )
( cd "$WORK_DIR" && dpkg-deb -x ubuntu-pro-client_*.deb ubuntu-pro-client-x )
SRC_DIR="$WORK_DIR/ubuntu-pro-client-x/usr/share/keyrings"
for src in ubuntu-pro-esm-apps.gpg ubuntu-pro-esm-infra.gpg; do
  if [[ -f "$SRC_DIR/$src" ]]; then
    cp "$SRC_DIR/$src" "$EMBED_DIR/${src/.gpg/-keyring.gpg}"
    echo "    -> $EMBED_DIR/${src/.gpg/-keyring.gpg}"
  else
    echo "    !! missing $src in ubuntu-pro-client; skipping"
  fi
done

echo
echo "==> Fingerprints in the refreshed keyring files"
echo "    These are the trust anchors that will be baked into the"
echo "    binary. Compare them against the upstream-published values"
echo "    (apt-cacher-ultra's commit log will record what they were"
echo "    last time the script ran) BEFORE committing."
echo "    Canonical upstream sources to cross-check against:"
echo "      - https://wiki.ubuntu.com/SecurityTeam/GPGKeyTable"
echo "      - https://www.debian.org/CD/verify"
echo "      - https://canonical-ubuntu-pro-client.readthedocs-hosted.com/"
echo
for f in "$EMBED_DIR"/*.gpg; do
  echo "  $(basename "$f")"
  gpg --no-default-keyring --keyring "$f" \
      --list-keys --with-fingerprint --with-colons 2>/dev/null \
    | awk -F: '$1=="pub"{kid=$5} $1=="fpr"{print "    pub  " $10} $1=="uid"{print "    uid  " $10}'
done
echo
echo "==> Done. After verifying the fingerprints above match the"
echo "    upstream-published values, commit and rebuild."
