#!/usr/bin/env bash
# e2e/apt-oracle driver (e2e/apt-oracle/README.md): re-measures the real apt
# of each client image on the apt-shape corpus, internal/gpg/testdata/aptshapes.
# For each image it runs e2e/apt-oracle/in-container.sh in a container
# without network (the images in parallel), prints apt's verdicts as one
# table, shape by image, and compares them, and the error class of each
# refusal, with the corpus's verdicts.txt and classes.txt: any difference is
# "apt changed" (exit 1). Manual, never in CI: before a release, after a
# go-crypto upgrade, after a change to internal/gpg's armor or signature
# checks.
#
#   E2E_APT_ORACLE_IMAGES  client images, whitespace-separated (default: the
#                          six columns of verdicts.txt, in its order, or with
#                          --old the five of verdicts-old.txt); any of them,
#                          each compared with its own column
#
#   make e2e-apt-oracle                         # the six current images
#   make e2e-apt-oracle-old                     # the five older ones
#   bash e2e/apt-oracle/run.sh                  # measure and compare
#   bash e2e/apt-oracle/run.sh --record         # measure and rewrite
#                                               # verdicts.txt and classes.txt
#   bash e2e/apt-oracle/run.sh --old [--record] # the same for the older
#                                               # images: verdicts-old.txt and
#                                               # classes-old.txt
#
# Exit status: 0 apt gives the recorded verdicts and error classes (or
# --record wrote them), 1 apt changed, a container failed or an error class
# and apt's words disagree, 2 a usage or setup error.
set -euo pipefail
cd "$(dirname "$0")/../.."

# verdicts.txt's columns, in its order (the hardening plan's DN-2: e2e/flat's
# six client images): --record measures exactly these. OLD_COLUMNS are
# verdicts-old.txt's (the plan's Task 9e: the older clients that still have
# upstream support, ruling HD-19), which --old --record measures.
COLUMNS="debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04"
OLD_COLUMNS="debian:10 ubuntu:20.04 ubuntu:18.04 ubuntu:16.04 ubuntu:14.04"
CORPUS=internal/gpg/testdata/aptshapes

record=0
old=0
bad=0
for arg in "$@"; do
    case $arg in
    --record) record=$((record + 1)) ;;
    --old) old=$((old + 1)) ;;
    *) bad=1 ;;
    esac
done
if ((bad || record > 1 || old > 1)); then
    echo "usage: bash e2e/apt-oracle/run.sh [--old] [--record]"
    exit 2
fi
if ((old)); then
    COLUMNS=$OLD_COLUMNS
    VERDICTS=$CORPUS/verdicts-old.txt
    CLASSES=$CORPUS/classes-old.txt
    oldflag=" --old"
else
    VERDICTS=$CORPUS/verdicts.txt
    CLASSES=$CORPUS/classes.txt
    oldflag=
fi
vname=${VERDICTS##*/}

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
        echo "[e2e-apt-oracle] ${images[$i]} is not a column of $vname ($COLUMNS)"
        exit 2
    fi
    fields+=("$field")
done
if ((record)) && [[ "${images[*]}" != "${columns[*]}" ]]; then
    echo "[e2e-apt-oracle] --record measures every column of $vname:" \
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
for f in "$VERDICTS" "$CLASSES"; do
    if ((!record)) && [[ ! -f $f ]]; then
        echo "[e2e-apt-oracle] $f is missing: record it with$oldflag --record"
        exit 2
    fi
done
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

# The generator's round trip (the Task 9e re-review's 9e-R1-M1), before any
# container: gen_shapes.py --from-controls on a copy of the corpus makes no
# key and must exit 0 and leave the copy byte for byte the corpus. A file
# the generator does not know (Task 9e's verdicts-old.txt broke every mode
# so, unseen) or a cut batch drifted from the committed fixtures fails here
# as a setup error. python3 only; the copy goes with $work (cleanup).
if ! command -v python3 > /dev/null; then
    echo "[e2e-apt-oracle] python3 is not installed (the generator's round trip needs it)"
    exit 2
fi
cp -R "$CORPUS" "$work/roundtrip"
if ! python3 -I -B e2e/apt-oracle/gen_shapes.py --from-controls "$work/roundtrip" \
    > "$work/roundtrip.out" 2>&1; then
    echo "[e2e-apt-oracle] FAIL (setup): gen_shapes.py --from-controls fails on a" \
        "copy of $CORPUS; its output's tail:"
    tail -n 5 "$work/roundtrip.out"
    exit 2
fi
if ! diff -r "$CORPUS" "$work/roundtrip" > "$work/roundtrip.diff"; then
    echo "[e2e-apt-oracle] FAIL (setup): gen_shapes.py --from-controls does not" \
        "give $CORPUS back byte for byte; the first differences:"
    head -n 20 "$work/roundtrip.diff"
    exit 2
fi
echo "[e2e-apt-oracle] the generator's round trip (--from-controls on a copy): identical"

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
# The detached shapes' names (classify: only a Release.gpg's refusal by
# apt's own check may say no more than gpgv's NODATA).
for f in ${shape_files[@]+"${shape_files[@]}"}; do
    b=${f##*/}
    case $b in
    *.Release.gpg) echo "${b%.Release.gpg}" ;;
    esac
done > "$work/detached"

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

# classify IMAGE OUT LEGACY: "<shape> <class>" for every line of
# in-container.sh's output OUT, unsorted; fails, naming each, when a class and
# apt's words disagree. LEGACY is 1 for an apt before 1.6 (below).
#
# AIDEV-NOTE: the classes (e2e/apt-oracle/README.md, "Error classes"): "-"
# accepted; split, apt's own checks of the file refused it before its
# verifier ran (in-container.sh's <ran> "n"); with the verifier run ("y"),
# nodata (NODATA), badsig (gpgv's BADSIG, sqv's "Message has been
# manipulated") or other. Each class is checked against apt's wording, so
# that a changed apt cannot shift one silently: a split refusal must print
# one of apt's text-layer messages (gpgv.cc: SplitClearSignedFile's
# "Splitting up", acquire's NOSPLIT, VerifyDetachedSignatureFile's dash,
# binary, unclosed and stray-line messages, or its "got 'NODATA'" for a
# Release.gpg without a BEGIN line — accepted for a detached shape only, the
# names in $work/detached: an InRelease refused before the verifier ran with
# no more than NODATA is a changed apt, the Task 9a review's 9A-M2, but on
# an apt before 1.6, whose gpgv method words SplitClearSignedFile's failure
# so: apt 1.0's and 1.2's, ubuntu:14.04's and 16.04's, Task 9e), and a
# verifier's refusal none of them. Task 9b's layer-T test
# (TestAptTextLayer_RefusesExactlyTheSplitRows) holds the cache's port of
# apt's splitter to the split rows of classes.txt.
classify() {
    awk -v img="$1" -v detached="$work/detached" -v legacy="$3" '
    BEGIN {
        while ((getline name < detached) > 0) isdetached[name] = 1
        close(detached)
    }
    function textlayer(s) {
        return s ~ /Splitting up .* into data and signature failed/ ||
            s ~ /Clearsigned file isn.t valid, got .NOSPLIT./ ||
            s ~ /does not start with a signed message block/ ||
            s ~ /contains lines not belonging to a signature/ ||
            s ~ /contains unexpected line starting with a dash/ ||
            s ~ /is in unsupported binary format/ ||
            s ~ /contains unclosed signatures/
    }
    function fail(why) {
        printf "[e2e-apt-oracle] %s: %s: %s: %s\n", img, $1, why, line > "/dev/stderr"
        bad = 1
    }
    /^#/ { next }
    {
        line = $0
        sub(/^[^ ]+ [^ ]+ [^ ]+ ?/, "", line)
        if ($2 == "0") {
            if ($3 != "-") fail("accepted, yet asked whether apt ran its verifier")
            print $1, "-"
        } else if ($3 == "n") {
            if (!textlayer(line) && !((isdetached[$1] || legacy) && line ~ /got .NODATA./))
                fail("refused before apt ran its verifier, with no text-layer message")
            print $1, "split"
        } else if ($3 == "y") {
            if (textlayer(line))
                fail("refused with a text-layer message after apt ran its verifier")
            if (line ~ /NODATA/) print $1, "nodata"
            else if (line ~ /BADSIG/ || line ~ /Message has been manipulated/) print $1, "badsig"
            else print $1, "other"
        } else {
            fail("refused, then not refused alike when run again (" $3 ")")
            print $1, "?"
        }
    }
    END { exit bad }
    ' "$2"
}

# Per image: the version line, and the verdicts of every shape (ok: apt-get
# update exited 0), checked to name exactly the corpus's shapes; the error
# class of each (classify); apt's first E:/W: line per shape goes to "said"
# ("<image index> <shape> <line>") for the differences.
columns_files=()
class_files=()
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
        NF < 3 || $2 !~ /^[0-9]+$/ || $3 !~ /^[-yn?]$/ { bad = 1; next }
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
    legacy=0
    if [[ ${versions[$i]} =~ ^apt\ ([0-9]+)\.([0-9]+) ]] &&
        ((BASH_REMATCH[1] < 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] < 6))); then
        legacy=1
    fi
    if ! classify "${images[$i]}" "$work/out.$i" "$legacy" | LC_ALL=C sort > "$work/classes.$i"; then
        echo "[e2e-apt-oracle] FAIL: ${images[$i]}: an error class that apt's" \
            "words do not bear out (above); e2e/apt-oracle/README.md, \"Error" \
            "classes\", says what to check"
        exit 1
    fi
    cut -d ' ' -f 2 "$work/verdicts.$i" > "$work/column.$i"
    columns_files+=("$work/column.$i")
    cut -d ' ' -f 2 "$work/classes.$i" > "$work/class_column.$i"
    class_files+=("$work/class_column.$i")
    awk -v k=$((i + 1)) '
        /^#/ { next }
        { line = $0; sub(/^[^ ]+ [^ ]+ [^ ]+ ?/, "", line); print k, $1, line }
    ' "$work/out.$i" >> "$work/said"
done
# Rows: the shape's name, then one verdict (one class) per image, in images'
# order.
paste -d ' ' "$work/shapes" "${columns_files[@]}" > "$work/measured"
paste -d ' ' "$work/shapes" "${class_files[@]}" > "$work/measured_classes"

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

# AIDEV-NOTE: verdicts.txt's and classes.txt's formats are interfaces: the
# verifier's Go table test (the hardening plan's Tasks 6 and 9) reads them
# too. Lines starting with "#" are comments; every other line is "<name>"
# and one field per COLUMNS entry, in COLUMNS' order, single spaces between
# them: ok/REJ in verdicts.txt; in classes.txt, "-" where the verdict is ok,
# else split, nodata, badsig or other. verdicts-old.txt and classes-old.txt
# (--old, OLD_COLUMNS) have the same format, and the table test reads them
# too (Task 9e, ruling E2).
if ((record)); then
    measured_at=$(date -u '+%Y-%m-%d %H:%M')
    {
        if ((old)); then
            echo "# apt's verdicts on the shapes of internal/gpg/testdata/aptshapes in the"
            echo "# older client images (verdicts.txt has the current ones), one row per"
            echo "# shape: ok (apt-get update succeeded and, on apt 1.0, left the"
            echo "# repository authenticated) or REJ (it did not), in each image."
            echo "# Measured $measured_at UTC by bash e2e/apt-oracle/run.sh --old --record"
            echo "# (e2e/apt-oracle/README.md, \"The older images\"); the fixtures, this"
            echo "# file and classes-old.txt are one measurement. The columns, after the"
            echo "# shape's name:"
        else
            echo "# apt's verdicts on the shapes of internal/gpg/testdata/aptshapes, one"
            echo "# row per shape: ok (apt-get update succeeded) or REJ (it failed), in"
            echo "# each client image. Measured $measured_at UTC by"
            echo "# bash e2e/apt-oracle/run.sh --record (e2e/apt-oracle/README.md); the"
            echo "# fixtures, this file and classes.txt are one measurement. The columns,"
            echo "# after the shape's name:"
        fi
        for i in "${!images[@]}"; do
            printf '#   %-13s %s\n' "${images[$i]}" "${versions[$i]}"
        done
        cat "$work/measured"
    } > "$work/verdicts.txt"
    {
        echo "# apt's error class of each refusal in $vname, the same rows and"
        echo "# columns: \"-\" where apt accepted the shape; where it refused it, split"
        echo "# (apt's own checks of the file refused it before its verifier ran),"
        echo "# nodata (the verifier found no signature data: gpgv's NODATA), badsig"
        echo "# (a signature failed its check: gpgv's BADSIG, sqv's \"Message has been"
        echo "# manipulated\") or other (anything else the verifier said). Measured"
        echo "# $measured_at UTC with $vname; e2e/apt-oracle/README.md,"
        echo "# \"Error classes\", says how each is derived."
        cat "$work/measured_classes"
    } > "$work/classes.txt"
    mv "$work/verdicts.txt" "$VERDICTS"
    mv "$work/classes.txt" "$CLASSES"
    echo "[e2e-apt-oracle] RECORDED $VERDICTS and $CLASSES ($nshapes shapes x" \
        "${#images[@]} images, $((SECONDS - start)) s): review their diff before" \
        "committing them"
    exit 0
fi

# compare RECORDED MEASURED KIND: names every difference between a recorded
# file (verdicts.txt, or classes.txt for KIND class) and the measurement,
# image by image, with what apt said; fails when there is one. A cell whose
# verdict changed is reported once, as a verdict.
compare() {
    awk -v fields="${fields[*]}" -v images="${images[*]}" \
        -v width=$((${#columns[@]} + 1)) -v said="$work/said" -v rec="$1" \
        -v file="${1##*/}" -v kind="$3" '
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
            printf "[e2e-apt-oracle] %s: not %d fields: %s\n", file, width, $0
            bad = 1
            next
        }
        recorded[$1] = $0
        next
    }
    {
        seen[$1] = 1
        if (!($1 in recorded)) {
            printf "[e2e-apt-oracle] %s: measured, but %s has no row for it\n", $1, file
            bad = 1
            next
        }
        split(recorded[$1], r, " ")
        for (k = 1; k <= n; k++) {
            was = r[field[k]]
            now = $(k + 1)
            if (was == now || (kind == "class" && (was == "-" || now == "-")))
                continue
            printf "[e2e-apt-oracle] apt changed: %s in %s: recorded %s%s, measured %s\n", $1, img[k], (kind == "class" ? "class " : ""), was, now
            line = what[k SUBSEP $1]
            printf "    apt said: %s\n", (line == "" ? "(no E: or W: line)" : line)
            bad = 1
        }
    }
    END {
        for (s in recorded)
            if (!(s in seen)) {
                printf "[e2e-apt-oracle] %s: in %s, but the corpus has no such shape\n", s, file
                bad = 1
            }
        exit bad
    }
    ' "$work/said" "$1" "$2"
}
same=1
compare "$VERDICTS" "$work/measured" verdict || same=0
compare "$CLASSES" "$work/measured_classes" class || same=0
if ((!same)); then
    echo "[e2e-apt-oracle] FAIL: apt changed, or $vname or ${CLASSES##*/} does" \
        "not match the corpus ($((SECONDS - start)) s); e2e/apt-oracle/README.md," \
        "\"When apt changes\", says what to update"
    exit 1
fi
echo "[e2e-apt-oracle] PASS: $nshapes shapes x ${#images[@]} images, apt's" \
    "verdicts and error classes as recorded in $VERDICTS and $CLASSES" \
    "($((SECONDS - start)) s)"
