#!/bin/bash
# build-fixtures.sh builds the e2e/flat upstream repositories under $1
# (default /fixtures), two versions of each, and the web roots nginx
# serves them from. It runs in the Dockerfile's `fixtures` stage.
#
# Layout produced:
#   keys/acu-e2e.gpg, keys/fingerprint   the throwaway signing key
#   keys/other.gpg                       a second key that signs nothing
#   <repo>/v1, <repo>/v2                 the two versions of a repository
#   <repo>/current -> v1                 the symlink test.sh flips
#   www/signed.test/..., www/plain.test/...
#                                        symlinks to <repo>/current at
#                                        the URL paths of spec §3's shapes
#
# Each repository reproduces one real repository's shape (spec §3, "Real
# flat repositories"); the comment on its builder says which. Packages are
# named acu-<repo> (acu-epoch, acu-jenkins-direct and acu-rogue besides),
# version 1.0 in v1 and 2.0 in v2 (acu-rogue is 1.0 in both), and install
# one file, /usr/share/acu-e2e/<name>, that holds "<name> <version>".
#
# AIDEV-NOTE: ruling R-H2-1 — the signing key is generated here, at image
# build time, and never leaves the image; no key is committed. v1 is dated
# two days before the build that made the fixtures layer and v2 one day
# before, and every file of a version gets its version's mtime, so v1 and
# v2 never share an nginx ETag or Last-Modified even when their sizes
# match. A cached fixtures layer stays valid indefinitely: no fixture
# carries Valid-Until, apt and the daemon refuse only a Date in the future
# (the daemon's other Date rule, upstream_older, compares v2 with v1), and
# the key never expires (`sign 0`). `docker build --no-cache-filter
# fixtures …` regenerates the key and the dates.
set -euo pipefail

OUT="${1:-/fixtures}"
WORK="$(mktemp -d)"
cleanup() {
    local h
    for h in "$WORK"/gnupg*; do
        GNUPGHOME=$h gpgconf --kill gpg-agent 2> /dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

export GNUPGHOME="$WORK/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'apt-cacher-ultra e2e-flat throwaway key <e2e-flat@example.invalid>' \
    rsa3072 sign 0
FPR="$(gpg --list-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')"
mkdir -p "$OUT/keys"
gpg --batch --yes --output "$OUT/keys/acu-e2e.gpg" --export "$FPR"
printf '%s\n' "$FPR" > "$OUT/keys/fingerprint"

# A second throwaway key that signs nothing: first-contact checks a signed
# source against it and expects apt's signature error.
mkdir -m 700 "$WORK/gnupg-other"
GNUPGHOME="$WORK/gnupg-other" gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --quick-generate-key 'apt-cacher-ultra e2e-flat unrelated key <e2e-flat-other@example.invalid>' \
    rsa3072 sign 0
GNUPGHOME="$WORK/gnupg-other" gpg --batch --yes --output "$OUT/keys/other.gpg" --export

STAMP_V1="$(date -u -d '-2 days' '+%Y-%m-%d %H:%M:%S')"
STAMP_V2="$(date -u -d '-1 day' '+%Y-%m-%d %H:%M:%S')"
DATE_V1="$(date -u -d "$STAMP_V1 UTC" '+%a, %d %b %Y %H:%M:%S UTC')"
DATE_V2="$(date -u -d "$STAMP_V2 UTC" '+%a, %d %b %Y %H:%M:%S UTC')"
# libnvidia-container's Release Date has stayed at 2018 (spec §3).
DATE_FROZEN='Thu, 01 Mar 2018 12:00:00 UTC'

# mkdeb NAME VERSION ARCH OUT: a one-file binary package.
mkdeb() {
    local name=$1 ver=$2 arch=$3 out=$4
    local d="$WORK/deb/$name-$ver"
    mkdir -p "$d/DEBIAN" "$d/usr/share/acu-e2e" "$(dirname "$out")"
    printf '%s %s\n' "$name" "$ver" > "$d/usr/share/acu-e2e/$name"
    cat > "$d/DEBIAN/control" <<EOF
Package: $name
Version: $ver
Architecture: $arch
Maintainer: apt-cacher-ultra e2e <noreply@example.invalid>
Section: misc
Priority: optional
Description: e2e/flat fixture package
 Installs /usr/share/acu-e2e/$name, which holds its version.
EOF
    dpkg-deb --root-owner-group --build "$d" "$out" >/dev/null
}

# mksrc NAME VERSION OUTDIR: a native source package (.dsc and .tar.xz).
mksrc() {
    local name=$1 ver=$2 out=$3
    local top="$WORK/src/$name-$ver"
    local d="$top/$name-$ver"
    mkdir -p "$d/debian/source"
    echo '3.0 (native)' > "$d/debian/source/format"
    cat > "$d/debian/control" <<EOF
Source: $name
Section: misc
Priority: optional
Maintainer: apt-cacher-ultra e2e <noreply@example.invalid>
Standards-Version: 4.6.2

Package: $name
Architecture: all
Description: e2e/flat fixture source package
 The source of the $name fixture package.
EOF
    cat > "$d/debian/changelog" <<EOF
$name ($ver) unstable; urgency=low

  * e2e/flat fixture.

 -- apt-cacher-ultra e2e <noreply@example.invalid>  Thu, 01 Jan 2026 00:00:00 +0000
EOF
    printf '#!/usr/bin/make -f\n%%:\n\tdh $@\n' > "$d/debian/rules"
    (cd "$top" && dpkg-source --build "$name-$ver" >/dev/null 2>&1)
    mv "$top/${name}_${ver}.dsc" "$top/${name}_${ver}.tar.xz" "$out/"
}

# release DIR DATE [apt-ftparchive options...]: DIR/Release with Date DATE.
release() {
    local dir=$1 date=$2
    shift 2
    (cd "$dir" && apt-ftparchive \
        -o APT::FTPArchive::Release::Origin=acu-e2e \
        -o APT::FTPArchive::Release::Label=acu-e2e \
        "$@" release . > "$WORK/Release")
    sed "s/^Date: .*/Date: $date/" "$WORK/Release" > "$dir/Release"
}

sign_inline() {
    gpg --batch --quiet --yes --default-key "$FPR" --clearsign --output "$1/InRelease" "$1/Release"
}
sign_detached() { # DIR [--armor]
    local dir=$1
    shift
    gpg --batch --quiet --yes --default-key "$FPR" --detach-sign "$@" --output "$dir/Release.gpg" "$dir/Release"
}

# byhash DIR: by-hash copies of every index DIR/Release lists.
byhash() {
    local dir=$1 f
    mkdir -p "$dir/by-hash/SHA256" "$dir/by-hash/SHA512"
    while read -r f; do
        cp "$dir/$f" "$dir/by-hash/SHA256/$(sha256sum "$dir/$f" | cut -d' ' -f1)"
        cp "$dir/$f" "$dir/by-hash/SHA512/$(sha512sum "$dir/$f" | cut -d' ' -f1)"
    done < <(awk '/^SHA256:/ {on=1; next} /^[^ ]/ {on=0} on {print $3}' "$dir/Release")
}

gz() { gzip -9nk "$@"; }

# pkgs.k8s.io core:/stable:/v1.34/deb: `deb <uri>/ /`, Filenames amd64/…,
# InRelease only, no by-hash, 403 for a missing file (nginx.conf).
# acu-rogue is on the upstream but in no Packages: strict mode's refusal.
build_k8s() { # VDIR VERSION DATE
    local d="$OUT/k8s/$1"
    mkdeb acu-k8s "$2" amd64 "$d/amd64/acu-k8s_${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages amd64 > Packages)
    gz "$d/Packages"
    release "$d" "$3"
    sign_inline "$d"
    rm "$d/Release"
    mkdeb acu-rogue 1.0 amd64 "$d/amd64/acu-rogue_1.0_amd64.deb"
}

# NVIDIA CUDA ubuntu2204/x86_64: `deb <uri>/ /`, dpkg-scanpackages Filenames
# ./…, InRelease beside Release and Release.gpg, and an epoch package whose
# file name carries a literal %3a (apt requests it as %253a).
build_cuda() {
    local d="$OUT/cuda/$1"
    mkdeb acu-cuda "$2" amd64 "$d/acu-cuda_${2}_amd64.deb"
    mkdeb acu-epoch "1:$2" amd64 "$d/acu-epoch_1%3a${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$3"
    sign_inline "$d"
    sign_detached "$d" --armor
}

# libnvidia-container stable/deb/amd64: `deb <uri> /`, Filenames ./…,
# InRelease only, a Release text with only a SHA512 block (Packages keeps
# its SHA256), and a Date frozen at 2018 in both versions.
build_nvidia() {
    local d="$OUT/nvidia/$1"
    mkdeb acu-nvidia "$2" amd64 "$d/acu-nvidia_${2}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$DATE_FROZEN" \
        -o APT::FTPArchive::Release::MD5=false \
        -o APT::FTPArchive::Release::SHA1=false \
        -o APT::FTPArchive::Release::SHA256=false
    sign_inline "$d"
    rm "$d/Release"
}

# pkg.jenkins.io debian-stable: `deb <uri> binary/`, detached (Release and
# an armored Release.gpg, no InRelease), Filenames binary/… and direct/…
# relative to the parent of binary/.
build_jenkins() {
    local d="$OUT/jenkins/$1"
    mkdeb acu-jenkins "$2" all "$d/binary/acu-jenkins_${2}_all.deb"
    mkdeb acu-jenkins-direct "$2" all "$d/direct/acu-jenkins-direct_${2}_all.deb"
    (cd "$d" && { apt-ftparchive packages binary; apt-ftparchive packages direct; } > "$WORK/Packages")
    mv "$WORK/Packages" "$d/binary/Packages"
    gz "$d/binary/Packages"
    release "$d/binary" "$3" -o APT::FTPArchive::Release::Suite=binary
    sign_detached "$d/binary" --armor
}

# download.sublimetext.com apt/stable/: `deb <host-root> apt/stable/`,
# Filenames files/… relative to the host root; files/ keeps every build.
build_sublime() {
    local d="$OUT/sublime/$1"
    mkdeb acu-sublime "$2" amd64 "$OUT/sublime/files/acu-sublime_${2}_amd64.deb"
    mkdir -p "$d"
    (cd "$OUT/sublime" && apt-ftparchive packages files > "$WORK/Packages")
    mv "$WORK/Packages" "$d/Packages"
    gz "$d/Packages"
    release "$d" "$3" -o APT::FTPArchive::Release::Suite=apt/stable
    sign_inline "$d"
    sign_detached "$d" --armor
}

# download.jitsi.org stable/: `deb <uri> stable/` and a deb-src line,
# Filenames stable/…, Acquire-By-Hash with SHA256 and SHA512 only,
# Contents-amd64 for apt-file, Sources, and a binary (unarmored)
# Release.gpg beside InRelease and Release. apt (2.2 to 3.2) refuses a
# binary Release.gpg ("unsupported binary format") and uses the InRelease
# (plan deviation 1).
build_jitsi() {
    local d="$OUT/jitsi/$1"
    local s="$d/stable"
    mkdeb acu-jitsi "$2" all "$s/acu-jitsi_${2}_all.deb"
    mksrc acu-jitsi "$2" "$s"
    (cd "$d" && apt-ftparchive packages stable > "$WORK/Packages" \
        && apt-ftparchive sources stable > "$WORK/Sources" \
        && apt-ftparchive contents stable > "$WORK/Contents-amd64")
    mv "$WORK/Packages" "$WORK/Sources" "$WORK/Contents-amd64" "$s/"
    gz "$s/Packages" "$s/Sources" "$s/Contents-amd64"
    xz -k "$s/Packages"
    release "$s" "$3" \
        -o APT::FTPArchive::Release::MD5=false \
        -o APT::FTPArchive::Release::SHA1=false \
        -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
        -o APT::FTPArchive::Release::Suite=stable
    byhash "$s"
    sign_inline "$s"
    sign_detached "$s"
}

# A signed dists/ repository: `deb <uri> acu main`, inline.
build_dists() {
    local d="$OUT/dists/$1"
    mkdeb acu-dists "$2" amd64 "$d/pool/main/a/acu-dists/acu-dists_${2}_amd64.deb"
    mkdir -p "$d/dists/acu/main/binary-amd64"
    (cd "$d" && apt-ftparchive packages pool/main > dists/acu/main/binary-amd64/Packages)
    gz "$d/dists/acu/main/binary-amd64/Packages"
    release "$d/dists/acu" "$3" \
        -o APT::FTPArchive::Release::Suite=acu \
        -o APT::FTPArchive::Release::Codename=acu \
        -o APT::FTPArchive::Release::Components=main \
        -o APT::FTPArchive::Release::Architectures=amd64
    sign_inline "$d/dists/acu"
}

# An unsigned repository: `deb [trusted=yes] <uri>/ /`, Release only; the
# upstream answers 404 (unsigned404) or 403 (unsigned403) for the missing
# InRelease and Release.gpg (nginx.conf).
build_unsigned() { # REPO VDIR VERSION DATE
    local d="$OUT/$1/$2"
    mkdeb "acu-$1" "$3" amd64 "$d/acu-$1_${3}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . > Packages)
    gz "$d/Packages"
    release "$d" "$4"
}

# apt.fury.io (Gemfury): `deb [trusted=yes] <uri>/ /`, Packages and
# Packages.gz, no Release at all, Filenames without ./. repo.goreleaser.com
# is the same shape with ./ Filenames.
build_releaseless() { # REPO VDIR VERSION FILENAME-SED
    local d="$OUT/$1/$2"
    mkdeb "acu-$1" "$3" amd64 "$d/acu-$1_${3}_amd64.deb"
    (cd "$d" && apt-ftparchive packages . | sed "$4" > Packages)
    gz "$d/Packages"
}

for v in 1 2; do
    if [[ $v == 1 ]]; then date=$DATE_V1; else date=$DATE_V2; fi
    build_k8s "v$v" "$v.0" "$date"
    build_cuda "v$v" "$v.0" "$date"
    build_nvidia "v$v" "$v.0"
    build_jenkins "v$v" "$v.0" "$date"
    build_sublime "v$v" "$v.0" "$date"
    build_jitsi "v$v" "$v.0" "$date"
    build_dists "v$v" "$v.0" "$date"
    build_unsigned unsigned404 "v$v" "$v.0" "$date"
    build_unsigned unsigned403 "v$v" "$v.0" "$date"
    build_releaseless fury "v$v" "$v.0" 's|^Filename: \./|Filename: |'
    build_releaseless goreleaser "v$v" "$v.0" ''
done

repos=(k8s cuda nvidia jenkins sublime jitsi dists unsigned404 unsigned403 fury goreleaser)
for r in "${repos[@]}"; do
    find "$OUT/$r/v1" -exec touch -h -d "$STAMP_V1 UTC" {} +
    find "$OUT/$r/v2" -exec touch -h -d "$STAMP_V2 UTC" {} +
    ln -s v1 "$OUT/$r/current"
done
touch -d "$STAMP_V1 UTC" "$OUT/sublime/files/acu-sublime_1.0_amd64.deb"
touch -d "$STAMP_V2 UTC" "$OUT/sublime/files/acu-sublime_2.0_amd64.deb"

# link URL-PATH TARGET: the web root entry for URL-PATH.
link() {
    mkdir -p "$(dirname "$OUT/www/$1")"
    ln -s "$2" "$OUT/www/$1"
}
link 'signed.test/core:/stable:/v1.34/deb' "$OUT/k8s/current"
link signed.test/cuda/ubuntu2204/x86_64 "$OUT/cuda/current"
link signed.test/libnvidia-container/stable/deb/amd64 "$OUT/nvidia/current"
link signed.test/debian-stable "$OUT/jenkins/current"
link signed.test/apt/stable "$OUT/sublime/current"
link signed.test/files "$OUT/sublime/files"
link signed.test/jitsi "$OUT/jitsi/current"
link signed.test/debian "$OUT/dists/current"
link plain.test/unsigned404 "$OUT/unsigned404/current"
link plain.test/unsigned403 "$OUT/unsigned403/current"
link plain.test/fury "$OUT/fury/current"
link plain.test/goreleaser "$OUT/goreleaser/current"

# Shape self-checks: a scenario relying on one of these shapes would
# pass vacuously without it (frozen-date, first-contact's inline
# adoption of a SHA512-only Release). Each check is positive, so a
# missing file or a missing field fails the build instead of passing it.
for vdir in v1 v2; do
    rel="$OUT/nvidia/$vdir/InRelease"
    if [[ ! -f $rel ]]; then
        echo "build-fixtures: libnvidia-container's $vdir InRelease is missing" >&2
        exit 1
    fi
    if ! grep -Fxq "Date: $DATE_FROZEN" "$rel"; then
        echo "build-fixtures: libnvidia-container's $vdir Release lacks \"Date: $DATE_FROZEN\" (want one frozen Date)" >&2
        exit 1
    fi
    if ! grep -q '^SHA512:' "$rel" || grep -qE '^(MD5Sum|SHA1|SHA256):' "$rel"; then
        echo "build-fixtures: libnvidia-container's $vdir Release is not SHA512-only" >&2
        exit 1
    fi
done

chmod -R a+rX "$OUT"
echo "build-fixtures: key $FPR; v1 dated $DATE_V1, v2 dated $DATE_V2"
