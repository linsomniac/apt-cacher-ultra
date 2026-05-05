#!/bin/sh
# build-repo.sh assembles a minimal but valid Debian repository tree
# under $1 (default /out) so an apt client driven through
# apt-cacher-ultra can run `apt-get update && apt-get install hello-acu`
# end-to-end. Intended to run during the upstream image's build stage.
#
# Layout produced:
#   pool/main/h/hello-acu/hello-acu_1.0_amd64.deb
#   dists/noble/main/binary-amd64/Packages
#   dists/noble/main/binary-amd64/Packages.gz
#   dists/noble/Release
#
# No GPG signature is generated. The e2e apt client uses
# `[trusted=yes]` in sources.list, which is the standard apt mechanism
# for unsigned repos in test environments.
set -eu

OUT="${1:-/out}"
SUITE=noble
COMPONENT=main
ARCH=amd64
PKG=hello-acu
VER=1.0

mkdir -p "$OUT/dists/$SUITE/$COMPONENT/binary-$ARCH"
mkdir -p "$OUT/pool/$COMPONENT/h/$PKG"

# Build the trivial .deb. dpkg-deb requires a directory layout with
# DEBIAN/control plus the staged file tree.
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$PKG/DEBIAN" "$WORK/$PKG/usr/bin"
cat > "$WORK/$PKG/DEBIAN/control" <<EOF
Package: $PKG
Version: $VER
Architecture: $ARCH
Maintainer: apt-cacher-ultra-e2e <noreply@example.invalid>
Description: trivial test package for apt-cacher-ultra e2e
 This package exists only so the e2e test can drive a real
 apt-get install through the cache.
EOF

cat > "$WORK/$PKG/usr/bin/hello-acu" <<'EOF'
#!/bin/sh
echo "hello from apt-cacher-ultra e2e"
EOF
chmod 755 "$WORK/$PKG/usr/bin/hello-acu"

dpkg-deb --build "$WORK/$PKG" \
    "$OUT/pool/$COMPONENT/h/$PKG/${PKG}_${VER}_${ARCH}.deb"

# Generate Packages indices. apt-ftparchive walks pool/ and emits
# entries with paths relative to cwd, so we must run it from $OUT.
cd "$OUT"
apt-ftparchive packages "pool/$COMPONENT" \
    > "dists/$SUITE/$COMPONENT/binary-$ARCH/Packages"
gzip -kn9 "dists/$SUITE/$COMPONENT/binary-$ARCH/Packages"

# Release file: hashes + metadata for InRelease/Release apt fetches.
cat > /tmp/apt-ftparchive.conf <<EOF
APT::FTPArchive::Release::Origin "apt-cacher-ultra-e2e";
APT::FTPArchive::Release::Label "test";
APT::FTPArchive::Release::Suite "$SUITE";
APT::FTPArchive::Release::Codename "$SUITE";
APT::FTPArchive::Release::Architectures "$ARCH";
APT::FTPArchive::Release::Components "$COMPONENT";
APT::FTPArchive::Release::Description "apt-cacher-ultra e2e fixture";
EOF
apt-ftparchive -c /tmp/apt-ftparchive.conf release "dists/$SUITE" \
    > "dists/$SUITE/Release"
