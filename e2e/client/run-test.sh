#!/bin/bash
# Test entrypoint for the e2e client container. Exercises the SPEC §12.4
# acceptance criterion: a clean apt update + apt install through
# apt-cacher-ultra against a mock upstream.
set -euo pipefail

# Make apt non-interactive — interactive prompts inside docker exec
# would deadlock the test.
export DEBIAN_FRONTEND=noninteractive

# Compose `depends_on: service_started` only waits for the cache
# process to exec — not for it to bind :3142. Race the apt-get
# against startup and CI flakes when cache start happens to be slow
# (cold image, contended runner). Block until the listener is
# accepting TCP, then proceed. Bash's /dev/tcp avoids needing nc
# in the image. 60s is generous; cache normally binds in <1s.
echo "[e2e] waiting for cache:3142"
ready=0
for i in $(seq 1 60); do
    if (echo > /dev/tcp/cache/3142) 2>/dev/null; then
        ready=1
        echo "[e2e] cache:3142 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ready -eq 0 ]]; then
    echo "[e2e] FAIL: cache:3142 never came up"
    exit 1
fi

echo "[e2e] apt-get update (drives metadata MISS through cache)"
apt-get update

echo "[e2e] apt-get install hello-acu (drives .deb MISS through cache)"
apt-get install -y --no-install-recommends hello-acu

echo "[e2e] verify hello-acu output"
out="$(hello-acu)"
expected="hello from apt-cacher-ultra e2e"
if [[ "$out" != "$expected" ]]; then
    echo "[e2e] FAIL: hello-acu output mismatch"
    echo "  want: $expected"
    echo "  got:  $out"
    exit 1
fi

echo "[e2e] apt-get update again (should hit cache for unchanged metadata)"
apt-get update

echo "[e2e] PASS"
