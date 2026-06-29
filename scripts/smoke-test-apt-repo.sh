#!/usr/bin/env bash
# Serve a freshly built apt repo over local HTTP and prove a clean Debian
# container can verify its signature and install from it. Used as the
# pre-deploy gate in pages.yaml and runnable locally. Requires docker +
# python3 (both preinstalled on GitHub's ubuntu-latest runners).
#
#   scripts/smoke-test-apt-repo.sh <site-dir>
set -euo pipefail

SITE="${1:?usage: smoke-test-apt-repo.sh <site-dir>}"
PORT="${PORT:-8089}"
IMAGE="${IMAGE:-debian:stable}"

command -v docker  >/dev/null || { echo "smoke-test: docker not found"  >&2; exit 2; }
command -v python3 >/dev/null || { echo "smoke-test: python3 not found" >&2; exit 2; }
test -f "$SITE/apt-cacher-ultra.gpg" \
    || { echo "smoke-test: $SITE is unsigned (no apt-cacher-ultra.gpg)" >&2; exit 2; }

python3 -m http.server "$PORT" --directory "$SITE" >/dev/null 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT

# Wait until the InRelease file is actually served.
ok=0
for _ in $(seq 1 50); do
    if curl -fsS "http://localhost:$PORT/dists/stable/InRelease" -o /dev/null 2>/dev/null; then
        ok=1; break
    fi
    sleep 0.2
done
[ "$ok" = 1 ] || { echo "smoke-test: local server never came up on :$PORT" >&2; exit 1; }

# --network host so the container reaches the runner's localhost server.
docker run --rm --network host -e BASE="http://localhost:$PORT" "$IMAGE" bash -c '
    set -euo pipefail
    apt-get update -qq
    apt-get install -y -qq curl ca-certificates gnupg >/dev/null
    install -d -m0755 /etc/apt/keyrings
    curl -fsSL "$BASE/apt-cacher-ultra.gpg" -o /etc/apt/keyrings/apt-cacher-ultra.gpg
    echo "deb [signed-by=/etc/apt/keyrings/apt-cacher-ultra.gpg] $BASE stable main" \
        > /etc/apt/sources.list.d/apt-cacher-ultra.list
    apt-get update
    # --download-only: prove the package is trusted, resolvable and
    # fetchable without running the postinstall (which expects systemd).
    apt-get install -y --download-only apt-cacher-ultra
    echo "smoke-test: container verified signature and resolved apt-cacher-ultra"
'
echo "smoke-test: OK"
