#!/bin/bash
# e2e/apt-oracle/in-container.sh: runs inside a client image, with the
# apt-shape corpus (internal/gpg/testdata/aptshapes) mounted read-only at
# /fx and no network (e2e/apt-oracle/run.sh starts it). For each shape it
# builds a flat file: repository in a temporary directory — the shape's
# file as InRelease, or as Release.gpg beside /fx/<name>.Release (/fx/Release
# when the shape has none of its own), and an empty Packages — and runs
# apt-get update against that repository alone, signed-by=/fx/keys.gpg.
#
# Prints "# apt <version> <verifier> <version>" (gpgv, or debian:13's sqv:
# whichever the image has installed), then one line per shape:
# "<name> <apt-get update's exit status> <apt's first E:/W: line>".
#
# AIDEV-NOTE: no -e, deliberately. apt-get update exits non-zero for every
# shape it refuses, and an assignment takes its command substitution's
# status, so under set -e the first refused shape would end the run: the
# capture is an if, and the grep that picks apt's first error line (it
# exits 1 when apt printed none) has its own || true.
set -uo pipefail
shopt -s nullglob

apt_ver=$(dpkg-query -W -f '${Version}' apt)
verifier=
for pkg in gpgv sqv; do
    if st=$(dpkg-query -W -f '${db:Status-Abbrev} ${Version}' "$pkg" 2> /dev/null) &&
        [[ $st == ii* ]]; then
        verifier+=" $pkg ${st##* }"
    fi
done
echo "# apt $apt_ver${verifier:- (neither gpgv nor sqv installed)}"

work=$(mktemp -d)
repo=$work/repo
list=$work/sources.list
for shape in /fx/*.InRelease /fx/*.Release.gpg; do
    file=${shape##*/}
    rm -rf "$repo" /var/lib/apt/lists/*
    mkdir -p "$repo" /var/lib/apt/lists/partial
    : > "$repo/Packages"
    case $file in
    *.InRelease)
        name=${file%.InRelease}
        cp "$shape" "$repo/InRelease"
        ;;
    *)
        name=${file%.Release.gpg}
        cp "$shape" "$repo/Release.gpg"
        if [[ -f /fx/$name.Release ]]; then
            cp "/fx/$name.Release" "$repo/Release"
        else
            cp /fx/Release "$repo/Release"
        fi
        ;;
    esac
    echo "deb [signed-by=/fx/keys.gpg] file:$repo/ ./" > "$list"
    if out=$(apt-get update -o Dir::Etc::SourceList="$list" \
        -o Dir::Etc::SourceParts=/dev/null -o APT::Sandbox::User=root 2>&1); then
        rc=0
    else
        rc=$?
    fi
    # 300 characters: the repository's path is in the line twice, and the
    # reason comes after it.
    err=$(printf '%s\n' "$out" | { grep -E '^(E|W): ' || true; } | head -n 1 | cut -c 1-300)
    printf '%s %s %s\n' "$name" "$rc" "$err"
done
rm -rf "$work"
