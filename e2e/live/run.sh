#!/usr/bin/env bash
# e2e/live driver: builds e2e/live/Dockerfile and runs the live-upstream
# smoke (e2e/live/README.md). Manual, before 1.1.0 and before later
# releases that change the request path, adoption or vouching; it reaches
# five vendors' live repositories, so it is never run in CI.
#
#   E2E_LIVE_NAME  image and container name (default acu-e2e-live)
#
#   make e2e-live
#   bash e2e/live/run.sh
set -euo pipefail
trap 'echo "[e2e-live] interrupted"; exit 130' INT
trap 'echo "[e2e-live] terminated"; exit 143' TERM
cd "$(dirname "$0")/../.."

name="${E2E_LIVE_NAME:-acu-e2e-live}"
if ! [[ $name =~ ^[a-z0-9][a-z0-9_.-]*$ ]]; then
    echo "[e2e-live] invalid E2E_LIVE_NAME '$name'"
    exit 2
fi

start=$SECONDS
if ! docker build -f e2e/live/Dockerfile -t "$name" .; then
    echo "[e2e-live] FAIL (build)"
    exit 1
fi
built=$((SECONDS - start))
# A container left by an interrupted run would take the name. --init puts
# a real init first, so Ctrl-C and docker stop end the smoke.
docker rm -f "$name" > /dev/null 2>&1 || true
if ! docker run --rm --init --name "$name" "$name"; then
    echo "[e2e-live] FAIL (smoke; build ${built}s, smoke $((SECONDS - start - built))s)"
    exit 1
fi
echo "[e2e-live] PASS (build ${built}s, smoke $((SECONDS - start - built))s)"
