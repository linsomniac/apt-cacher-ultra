#!/usr/bin/env bash
# e2e/apt-oracle driver (e2e/apt-oracle/README.md): re-measures the real apt
# of each client image on the apt-shape corpus, internal/gpg/testdata/aptshapes.
# For each image it runs e2e/apt-oracle/in-container.sh in a container
# without network (the images in parallel), prints apt's verdicts as one
# table, shape by image, and compares them with the corpus's verdicts.txt:
# any difference is "apt changed" (exit 1). Manual, never in CI: before a
# release, after a go-crypto upgrade, after a change to internal/gpg's armor
# or signature checks.
#
#   E2E_APT_ORACLE_IMAGES  client images, whitespace-separated (default: the
#                          six columns of verdicts.txt, in its order); any of
#                          them, each compared with its own column
#
#   make e2e-apt-oracle
#   bash e2e/apt-oracle/run.sh            # measure and compare
#   bash e2e/apt-oracle/run.sh --record   # measure and rewrite verdicts.txt
#
# Exit status: 0 apt gives the recorded verdicts (or --record wrote them),
# 1 apt changed or a container failed, 2 a usage or setup error.
set -euo pipefail
cd "$(dirname "$0")/../.."

# verdicts.txt's columns, in its order (the hardening plan's DN-2: e2e/flat's
# six client images): --record measures exactly these.
COLUMNS="debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04"
CORPUS=internal/gpg/testdata/aptshapes
VERDICTS=$CORPUS/verdicts.txt

record=0
case "$#:${1-}" in
0:) ;;
1:--record) record=1 ;;
*)
    echo "usage: bash e2e/apt-oracle/run.sh [--record]"
    exit 2
    ;;
esac

# Whitespace-split the whole value (e2e/deb/run.sh says why read -d '').
IFS=$' \t\n' read -r -d '' -a columns < <(printf '%s\0' "$COLUMNS")
IFS=$' \t\n' read -r -d '' -a images < <(
    printf '%s\0' "${E2E_APT_ORACLE_IMAGES:-$COLUMNS}"
)
if ((${#images[@]} == 0)); then
    echo "[e2e-apt-oracle] E2E_APT_ORACLE_IMAGES is empty; nothing to measure"
    exit 2
fi
# fields[i]: the field of images[i]'s column in a verdicts.txt row (the
# shape's name is field 1).
fields=()
for i in "${!images[@]}"; do
    for ((j = 0; j < i; j++)); do
        if [[ ${images[$j]} == "${images[$i]}" ]]; then
            echo "[e2e-apt-oracle] ${images[$i]} is named twice in E2E_APT_ORACLE_IMAGES"
            exit 2
        fi
    done
    field=
    for j in "${!columns[@]}"; do
        if [[ ${columns[$j]} == "${images[$i]}" ]]; then
            field=$((j + 2))
        fi
    done
    if [[ -z $field ]]; then
        echo "[e2e-apt-oracle] ${images[$i]} is not a column of verdicts.txt ($COLUMNS)"
        exit 2
    fi
    fields+=("$field")
done
if ((record)) && [[ "${images[*]}" != "${columns[*]}" ]]; then
    echo "[e2e-apt-oracle] --record measures every column of verdicts.txt:" \
        "unset E2E_APT_ORACLE_IMAGES"
    exit 2
fi
for f in "$CORPUS/keys.gpg" "$CORPUS/Release"; do
    if [[ ! -f $f ]]; then
        echo "[e2e-apt-oracle] $f is missing: generate the corpus" \
            "(python3 -I e2e/apt-oracle/gen_shapes.py $CORPUS)"
        exit 2
    fi
done
if ((!record)) && [[ ! -f $VERDICTS ]]; then
    echo "[e2e-apt-oracle] $VERDICTS is missing: record it with --record"
    exit 2
fi
for tool in docker timeout; do
    if ! command -v "$tool" > /dev/null; then
        echo "[e2e-apt-oracle] $tool is not installed"
        exit 2
    fi
done

work=$(mktemp -d)
pids=()
# ${a[@]+"${a[@]}"}: an array that may be empty, which set -u rejects on
# bash before 4.4.
cleanup() {
    local p
    for p in ${pids[@]+"${pids[@]}"}; do
        kill "$p" 2> /dev/null || true
    done
    rm -rf "$work"
}
trap cleanup EXIT
trap 'echo "[e2e-apt-oracle] interrupted"; exit 130' INT
trap 'echo "[e2e-apt-oracle] terminated"; exit 143' TERM

# The corpus's shape names, sorted as every list below is (LC_ALL=C).
shopt -s nullglob
shape_files=("$CORPUS"/*.InRelease "$CORPUS"/*.Release.gpg)
shopt -u nullglob
for f in ${shape_files[@]+"${shape_files[@]}"}; do
    b=${f##*/}
    case $b in
    *.InRelease) echo "${b%.InRelease}" ;;
    *) echo "${b%.Release.gpg}" ;;
    esac
done | LC_ALL=C sort > "$work/shapes"
nshapes=$(($(wc -l < "$work/shapes")))
if ((nshapes == 0)); then
    echo "[e2e-apt-oracle] $CORPUS holds no shape"
    exit 2
fi
if [[ -n $(LC_ALL=C uniq -d "$work/shapes") ]]; then
    echo "[e2e-apt-oracle] a shape has both an .InRelease and a .Release.gpg:" \
        "$(LC_ALL=C uniq -d "$work/shapes" | tr '\n' ' ')"
    exit 2
fi

start=$SECONDS
echo "[e2e-apt-oracle] $nshapes shapes x ${#images[@]} images: ${images[*]}"
# --init: a real init as PID 1 passes timeout's and Ctrl-C's signals on, so
# a container that overruns its 600 s ends.
for i in "${!images[@]}"; do
    timeout 600 docker run --rm --init --network none \
        -v "$PWD/$CORPUS:/fx:ro" \
        -v "$PWD/e2e/apt-oracle/in-container.sh:/in-container.sh:ro" \
        "${images[$i]}" bash /in-container.sh > "$work/out.$i" 2> "$work/err.$i" &
    pids+=("$!")
done
failed=0
for i in "${!images[@]}"; do
    if wait "${pids[$i]}"; then
        continue
    else
        rc=$?
    fi
    echo "[e2e-apt-oracle] ${images[$i]}: the container failed (exit $rc;" \
        "124 means it overran 600 s); its stderr's tail:"
    tail -n 20 "$work/err.$i"
    failed=1
done
pids=()
if ((failed)); then
    echo "[e2e-apt-oracle] FAIL (a container failed; nothing was compared)"
    exit 1
fi

# Per image: the version line, and the verdicts of every shape (ok: apt-get
# update exited 0), checked to name exactly the corpus's shapes; apt's first
# E:/W: line per shape goes to "said" ("<image index> <shape> <line>") for
# the differences.
columns_files=()
versions=()
: > "$work/said"
for i in "${!images[@]}"; do
    version=$(grep -m 1 '^# apt ' "$work/out.$i" || true)
    if [[ -z $version ]]; then
        echo "[e2e-apt-oracle] ${images[$i]}: no '# apt' line from in-container.sh"
        tail -n 20 "$work/out.$i" "$work/err.$i"
        exit 1
    fi
    versions+=("${version#\# }")
    if ! awk '
        /^#/ { next }
        NF < 2 || $2 !~ /^[0-9]+$/ { bad = 1; next }
        { print $1, ($2 == "0" ? "ok" : "REJ") }
        END { exit bad }
    ' "$work/out.$i" | LC_ALL=C sort > "$work/verdicts.$i"; then
        echo "[e2e-apt-oracle] ${images[$i]}: a malformed line from in-container.sh"
        tail -n 20 "$work/out.$i"
        exit 1
    fi
    if ! cut -d ' ' -f 1 "$work/verdicts.$i" | cmp -s - "$work/shapes"; then
        echo "[e2e-apt-oracle] ${images[$i]}: its verdicts do not name the" \
            "corpus's shapes, each once:"
        cut -d ' ' -f 1 "$work/verdicts.$i" | diff "$work/shapes" - | head -n 20 || true
        exit 1
    fi
    cut -d ' ' -f 2 "$work/verdicts.$i" > "$work/column.$i"
    columns_files+=("$work/column.$i")
    awk -v k=$((i + 1)) '
        /^#/ { next }
        { line = $0; sub(/^[^ ]+ [^ ]+ ?/, "", line); print k, $1, line }
    ' "$work/out.$i" >> "$work/said"
done
# Rows: the shape's name, then one verdict per image, in images' order.
paste -d ' ' "$work/shapes" "${columns_files[@]}" > "$work/measured"

for i in "${!images[@]}"; do
    printf '[e2e-apt-oracle] %-13s %s\n' "${images[$i]}" "${versions[$i]}"
done
echo
awk -v images="${images[*]}" '
    BEGIN {
        n = split(images, img, " ")
        printf "%-32s", "shape"
        for (k = 1; k < n; k++) printf " %-12s", img[k]
        printf " %s\n", img[n]
    }
    {
        printf "%-32s", $1
        for (k = 2; k < NF; k++) printf " %-12s", $k
        printf " %s\n", $NF
    }
' "$work/measured"
echo

# AIDEV-NOTE: verdicts.txt's format is an interface: the verifier's Go table
# test (the hardening plan's Task 6) reads it too. Lines starting with "#"
# are comments; every other line is "<name>" and one ok/REJ per COLUMNS
# entry, in COLUMNS' order, single spaces between them.
if ((record)); then
    {
        echo "# apt's verdicts on the shapes of internal/gpg/testdata/aptshapes, one"
        echo "# row per shape: ok (apt-get update succeeded) or REJ (it failed), in"
        echo "# each client image. Measured $(date -u '+%Y-%m-%d %H:%M') UTC by"
        echo "# bash e2e/apt-oracle/run.sh --record (e2e/apt-oracle/README.md); the"
        echo "# fixtures and this file are one measurement. The columns, after the"
        echo "# shape's name:"
        for i in "${!images[@]}"; do
            printf '#   %-13s %s\n' "${images[$i]}" "${versions[$i]}"
        done
        cat "$work/measured"
    } > "$work/verdicts.txt"
    mv "$work/verdicts.txt" "$VERDICTS"
    echo "[e2e-apt-oracle] RECORDED $VERDICTS ($nshapes shapes x ${#images[@]}" \
        "images, $((SECONDS - start)) s): review its diff before committing it"
    exit 0
fi

# Compare with verdicts.txt, image by image, and name every difference with
# what apt said.
if ! awk -v fields="${fields[*]}" -v images="${images[*]}" \
    -v width=$((${#columns[@]} + 1)) -v said="$work/said" -v rec="$VERDICTS" '
    BEGIN { n = split(fields, field, " "); split(images, img, " ") }
    FILENAME == said {
        line = $0
        sub(/^[^ ]+ [^ ]+ ?/, "", line)
        what[$1 SUBSEP $2] = line
        next
    }
    FILENAME == rec {
        if (/^#/ || NF == 0) next
        if (NF != width) {
            printf "[e2e-apt-oracle] verdicts.txt: not %d fields: %s\n", width, $0
            bad = 1
            next
        }
        recorded[$1] = $0
        next
    }
    {
        seen[$1] = 1
        if (!($1 in recorded)) {
            printf "[e2e-apt-oracle] %s: measured, but verdicts.txt has no row for it\n", $1
            bad = 1
            next
        }
        split(recorded[$1], r, " ")
        for (k = 1; k <= n; k++)
            if (r[field[k]] != $(k + 1)) {
                printf "[e2e-apt-oracle] apt changed: %s in %s: recorded %s, measured %s\n", $1, img[k], r[field[k]], $(k + 1)
                line = what[k SUBSEP $1]
                printf "    apt said: %s\n", (line == "" ? "(no E: or W: line)" : line)
                bad = 1
            }
    }
    END {
        for (s in recorded)
            if (!(s in seen)) {
                printf "[e2e-apt-oracle] %s: in verdicts.txt, but the corpus has no such shape\n", s
                bad = 1
            }
        exit bad
    }
' "$work/said" "$VERDICTS" "$work/measured"; then
    echo "[e2e-apt-oracle] FAIL: apt changed, or verdicts.txt does not match the" \
        "corpus ($((SECONDS - start)) s); e2e/apt-oracle/README.md, \"When apt" \
        "changes\", says what to update"
    exit 1
fi
echo "[e2e-apt-oracle] PASS: $nshapes shapes x ${#images[@]} images, apt's" \
    "verdicts as recorded in $VERDICTS ($((SECONDS - start)) s)"
