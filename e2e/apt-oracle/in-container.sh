#!/bin/bash
# e2e/apt-oracle/in-container.sh: runs inside a client image, with the
# apt-shape corpus (internal/gpg/testdata/aptshapes) mounted read-only at
# /fx and no network (e2e/apt-oracle/run.sh starts it). For each shape it
# builds a flat file: repository in a temporary directory — the shape's
# file as InRelease, or as Release.gpg beside /fx/<name>.Release (/fx/Release
# when the shape has none of its own), and an empty Packages — and runs
# apt-get update against that repository alone, signed-by=/fx/keys.gpg, or
# signed-by=/fx/<name>.keys.asc when the shape has a key file of its own
# (an armored keyring: batch fx16's ka_* rows).
#
# Prints "# apt <version> <verifier> <version>" (gpgv, or debian:13's sqv:
# whichever the image has installed), then one line per shape:
# "<name> <apt-get update's exit status> <ran> <apt's first E:/W: line>".
# <ran> answers, for a refused shape, whether apt ran its verifier (gpgv or
# sqv) on it: "y", or "n" when apt's own checks of the file refused it
# first; "?" when asking gave another exit status; "-" for an accepted
# shape, which is not asked. run.sh derives the error class from <ran> and
# the line (e2e/apt-oracle/README.md, "Error classes").
#
# AIDEV-NOTE: no -e, deliberately. apt-get update exits non-zero for every
# shape it refuses, and an assignment takes its command substitution's
# status, so under set -e the first refused shape would end the run: the
# capture is an if, and the grep that picks apt's first error line (it
# exits 1 when apt printed none) has its own || true.
#
# AIDEV-NOTE: <ran> is apt's control flow, not its wording, which cannot
# tell the two apart where it matters: a Release.gpg without a BEGIN line
# makes apt's own check print gpgv's "got 'NODATA'" (apt-pkg/contrib/
# gpgv.cc, VerifyDetachedSignatureFile). A refused shape is run a second
# time with the gpgv and sqv methods' debug output, which prints
# "Preparing to exec: " (gpgv, apt-key on apt 2.2) or "Executing <path>sqv "
# only once apt's checks (the clearsign sniff, SplitClearSignedFile,
# VerifyDetachedSignatureFile) have passed the file on. The verdict and the
# line come from the first run, made as it always was.
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

# apt-get update on the shape's repository alone, its lists emptied first;
# extra options after it ("$@"), the output on stdout, its status returned.
update() {
    rm -rf /var/lib/apt/lists/*
    mkdir -p /var/lib/apt/lists/partial
    apt-get update -o Dir::Etc::SourceList="$list" \
        -o Dir::Etc::SourceParts=/dev/null -o APT::Sandbox::User=root "$@" 2>&1
}

for shape in /fx/*.InRelease /fx/*.Release.gpg; do
    file=${shape##*/}
    rm -rf "$repo"
    mkdir -p "$repo"
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
    keys=/fx/keys.gpg
    if [[ -f /fx/$name.keys.asc ]]; then
        keys=/fx/$name.keys.asc
    fi
    echo "deb [signed-by=$keys] file:$repo/ ./" > "$list"
    if out=$(update); then
        rc=0
    else
        rc=$?
    fi
    # 300 characters: the repository's path is in the line twice, and the
    # reason comes after it.
    err=$(printf '%s\n' "$out" | { grep -E '^(E|W): ' || true; } | head -n 1 | cut -c 1-300)
    ran=-
    if ((rc != 0)); then
        if dbg=$(update -o Debug::Acquire::gpgv=1 -o Debug::Acquire::sqv=1); then
            drc=0
        else
            drc=$?
        fi
        if ((drc != rc)); then
            ran='?'
        elif grep -qE '^Preparing to exec: |^Executing [^ ]*sqv ' <<< "$dbg"; then
            ran=y
        else
            ran=n
        fi
    fi
    printf '%s %s %s %s\n' "$name" "$rc" "$ran" "$err"
done
rm -rf "$work"
