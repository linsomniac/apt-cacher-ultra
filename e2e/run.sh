#!/usr/bin/env bash
# SPEC §12.4 driver. Builds the three test images, runs compose up
# with `--abort-on-container-exit --exit-code-from client`, and
# returns the client's exit code (0 = pass).
#
# On failure dumps cache + upstream + client logs so CI surfaces
# enough signal to diagnose without re-running.
#
# Always tears down the compose stack (volumes too) on exit, so
# repeated local runs start clean.

set -euo pipefail

cd "$(dirname "$0")"

cleanup() {
    local ec=$?
    if [[ $ec -ne 0 ]]; then
        echo
        echo "=== upstream logs ==="
        docker compose logs upstream || true
        echo
        echo "=== cache logs ==="
        docker compose logs cache || true
        echo
        echo "=== client logs ==="
        docker compose logs client || true
    fi
    docker compose down -v --remove-orphans || true
    exit $ec
}
trap cleanup EXIT

echo "[e2e] building images"
docker compose build

echo "[e2e] running e2e (--exit-code-from client)"
# `set +e` so the trap captures the actual exit code rather than the
# shell aborting before cleanup runs.
set +e
docker compose up \
    --abort-on-container-exit \
    --exit-code-from client
ec=$?
set -e

if [[ $ec -ne 0 ]]; then
    echo "[e2e] FAIL (client exit=$ec)"
    exit "$ec"
fi

# Sanity: the second `apt-get update` inside the client must have
# produced at least one cache HIT. Without this assert a regression
# that turned every request into a MISS would still pass — apt would
# happily re-fetch everything every time.
#
# The regex anchors `outcome=hit` against the slog key=value field
# boundary — the value must end at whitespace or end-of-line. This
# rejects `outcome=hit_stale` / `outcome=hit_coalesced` (legal SPEC
# §10 outcomes the cache emits in other paths, neither of which
# proves a fresh cache hit) and is also tighter than `[^a-z_]`,
# which would have allowed hypothetical future values like `hit2`
# or `hit-status` to slip through.
hits="$(docker compose logs cache 2>/dev/null | grep -cE '(^|[[:space:]])outcome=hit([[:space:]]|$)' || true)"
if [[ "$hits" -lt 1 ]]; then
    echo "[e2e] FAIL: expected at least one cache HIT in cache logs; got $hits"
    exit 1
fi

echo "[e2e] PASS (cache HITs observed: $hits)"
