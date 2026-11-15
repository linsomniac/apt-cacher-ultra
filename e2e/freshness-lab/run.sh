#!/usr/bin/env bash
# Manual experiment only; deliberately not wired into make test or CI.
set -euo pipefail
if (($# < 2)); then
    echo "Usage: $0 ACU_BINARY NEW_OUTPUT_DIRECTORY [lab options]" >&2
    exit 2
fi
binary=$(realpath "$1")
output=$(realpath -m "$2")
shift 2
test -x "$binary"
if test -e "$output"; then
    echo "Output must be a new directory: $output" >&2
    exit 2
fi
mkdir -p "$output"
lab_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
image=${ACU_FRESHNESS_LAB_IMAGE:-acu-freshness-lab:local}
if [[ ${ACU_FRESHNESS_LAB_SKIP_BUILD:-0} != 1 ]]; then
    docker build -t "$image" "$lab_dir"
fi
# All three participants live inside one disposable network namespace.
# No host ports, Docker socket, real cache, or host apt configuration is mounted.
docker run --rm --init --network none --add-host repo.test:127.0.0.1 \
    -v "$binary:/lab/acu:ro" -v "$lab_dir/lab.py:/lab/lab.py:ro" \
    -v "$output:/results" "$image" "$@"
