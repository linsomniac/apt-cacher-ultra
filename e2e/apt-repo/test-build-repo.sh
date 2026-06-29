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
