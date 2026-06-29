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
# Start from a clean tree so a re-run into a reused out-dir cannot retain
# stale .debs or a previous InRelease (which would be hashed into the new
# Release). CI always uses a fresh dir; this protects local re-runs.
rm -rf "$OUT/pool" "$OUT/dists"
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

echo "build-apt-repo.sh: built $SUITE repo with ${#debs[@]} package file(s) at $OUT"
