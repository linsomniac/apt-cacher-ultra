#!/usr/bin/env bash
# SPEC §14 step 3 driver. For each Ubuntu LTS target listed in
# UBUNTU_VERSIONS (default: "24.04 26.04"), builds a docker image
# that:
#   1. compiles apt-cacher-ultra
#   2. produces a .deb via nfpm (same path as `make deb`)
#   3. installs the .deb on ubuntu:<ver> in a clean image
#   4. validates the package contract and starts the daemon
#
# Override the version set during development:
#   UBUNTU_VERSIONS="24.04" bash e2e/deb/run.sh
#
# Each target is independent — one failing version does not stop
# the others. The exit code is non-zero iff any target failed.

set -euo pipefail
cd "$(dirname "$0")/../.."

versions="${UBUNTU_VERSIONS:-24.04 26.04}"
failures=()

# Validate up front before letting any value reach `docker build`
# or the image tag. Args are quoted everywhere, so this is mostly
# a footgun guard rather than an injection fix — but a stray
# `latest` or `:`-bearing token would produce confusing errors
# downstream, so reject early.
for ver in $versions; do
    if ! [[ "$ver" =~ ^[0-9]{2}\.[0-9]{2}$ ]]; then
        echo "[deb-test] invalid Ubuntu version: '$ver' (expected NN.NN, got '$ver')"
        exit 2
    fi
done

for ver in $versions; do
    echo
    echo "==============================="
    echo "[deb-test] ubuntu:${ver}"
    echo "==============================="

    if ! docker build \
            -f e2e/deb/Dockerfile \
            --build-arg UBUNTU_VERSION="${ver}" \
            --target test \
            -t "aculan-deb-test:${ver}" . ; then
        echo "[deb-test] ubuntu:${ver} FAIL (build)"
        failures+=("${ver}:build")
        continue
    fi

    if ! docker run --rm "aculan-deb-test:${ver}"; then
        echo "[deb-test] ubuntu:${ver} FAIL (test)"
        failures+=("${ver}:test")
        continue
    fi

    echo "[deb-test] ubuntu:${ver} PASS"
done

echo

if (( ${#failures[@]} > 0 )); then
    echo "[deb-test] OVERALL FAIL: ${failures[*]}"
    exit 1
fi

echo "[deb-test] OVERALL PASS (${versions})"
