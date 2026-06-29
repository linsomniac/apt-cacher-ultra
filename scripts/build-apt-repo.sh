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
