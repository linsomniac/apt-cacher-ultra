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

d="$(mktemp -d -p "$work")"; mkdir -p "$d/DEBIAN"
cat > "$d/DEBIAN/control" <<EOF
Package: apt-cacher-ultra
Version: 0.10.4
Architecture: amd64
Maintainer: test <t@example.invalid>
Description: fixture package
EOF
dpkg-deb --root-owner-group --build "$d" "$debs/apt-cacher-ultra_0.10.4_amd64.deb" >/dev/null; rm -rf "$d"

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
if head -c 64 "$out/apt-cacher-ultra.gpg" | grep -q 'BEGIN PGP'; then
    echo "FAIL: exported public key is ASCII-armored, expected binary"; exit 1
fi
gpg --verify "$inrel" >/dev/null 2>&1   || { echo "FAIL: InRelease signature does not verify"; exit 1; }
grep -q "$fpr" "$out/index.html"        || { echo "FAIL: index.html missing fingerprint"; exit 1; }

echo "PASS test-build-repo (signed)"
