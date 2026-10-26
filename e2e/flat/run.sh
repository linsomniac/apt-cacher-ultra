#!/usr/bin/env bash
# e2e/flat driver (spec §7, "Real-apt end-to-end"). For each client image
# in E2E_FLAT_IMAGES, builds e2e/flat/Dockerfile on it and runs the test
# container (nginx, the daemon and apt clients in one container).
#
#   E2E_FLAT_IMAGES           client images, whitespace-separated (default:
#                             the six of spec §7, oldest apt first)
#   E2E_FLAT_UNSIGNED_IMAGES  the images that also run the unsigned
#                             first-adoption scenario, which waits for its
#                             confirmation, 5 to 15 minutes after first
#                             contact (default: the oldest and the newest;
#                             set but empty: none)
#   E2E_FLAT_NAME             image and container name prefix
#                             (default acu-e2e-flat)
#
#   make e2e-flat
#   E2E_FLAT_IMAGES=debian:12 bash e2e/flat/run.sh
#
# Each image is independent: one failing does not stop the others. The
# exit status is non-zero iff any image failed.
set -euo pipefail
# Ctrl-C ends the running container through its own trap, and docker run
# then returns normally: stop here too rather than go on to the next image.
trap 'echo "[e2e-flat] interrupted"; exit 130' INT TERM
cd "$(dirname "$0")/../.."

# Whitespace-split the whole value (a CI block scalar spans lines; see
# e2e/deb/run.sh for why read -d '' is used).
IFS=$' \t\n' read -r -d '' -a images < <(
    printf '%s\0' "${E2E_FLAT_IMAGES:-debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04}"
)
IFS=$' \t\n' read -r -d '' -a unsigned_images < <(
    printf '%s\0' "${E2E_FLAT_UNSIGNED_IMAGES-debian:11 ubuntu:26.04}"
)
name="${E2E_FLAT_NAME:-acu-e2e-flat}"

if ((${#images[@]} == 0)); then
    echo "[e2e-flat] E2E_FLAT_IMAGES is empty; nothing to test"
    exit 2
fi
# ${a[@]+"${a[@]}"}: an array that may be empty, which set -u rejects on
# bash before 4.4 (macOS's /bin/bash is 3.2).
for img in "${images[@]}" ${unsigned_images[@]+"${unsigned_images[@]}"}; do
    if ! [[ $img =~ ^(debian|ubuntu):[0-9]+(\.[0-9]+)?$ ]]; then
        echo "[e2e-flat] unsupported client image '$img' (want debian:N or ubuntu:NN.NN)"
        exit 2
    fi
done
if ! [[ $name =~ ^[a-z0-9][a-z0-9_.-]*$ ]]; then
    echo "[e2e-flat] invalid E2E_FLAT_NAME '$name'"
    exit 2
fi

failures=()
timings=()
for img in "${images[@]}"; do
    tag="${img/:/-}"
    unsigned_wait=0
    for u in ${unsigned_images[@]+"${unsigned_images[@]}"}; do
        [[ $u == "$img" ]] && unsigned_wait=1
    done
    echo
    echo "==============================="
    echo "[e2e-flat] $img (unsigned first adoption: $unsigned_wait)"
    echo "==============================="
    start=$SECONDS
    if ! docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE="$img" \
        -t "$name:$tag" .; then
        echo "[e2e-flat] $img FAIL (build)"
        failures+=("$img:build")
        continue
    fi
    built=$((SECONDS - start))
    # A container left by an interrupted run would take the name. --init
    # puts a real init first, so Ctrl-C and docker stop end the test.
    docker rm -f "$name-$tag" > /dev/null 2>&1 || true
    if ! docker run --rm --init --name "$name-$tag" \
        -e E2E_IMAGE="$img" -e E2E_UNSIGNED_WAIT="$unsigned_wait" "$name:$tag"; then
        echo "[e2e-flat] $img FAIL (test)"
        failures+=("$img:test")
        continue
    fi
    timings+=("$img: build ${built}s, test $((SECONDS - start - built))s")
    echo "[e2e-flat] $img PASS"
done

echo
for t in ${timings[@]+"${timings[@]}"}; do
    echo "[e2e-flat] $t"
done
if ((${#failures[@]} > 0)); then
    echo "[e2e-flat] OVERALL FAIL: ${failures[*]}"
    exit 1
fi
echo "[e2e-flat] OVERALL PASS (${images[*]})"
