#!/usr/bin/env bash
# Exercise the shipped image's isolation, cache, MITM policy and persistent CA.
# Probe scripts expand variables inside the container, not on the host.
# shellcheck disable=SC2016
set -euo pipefail
image="${1:-apt-cacher-ultra:smoke}"
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
container="acu-smoke-${RANDOM}-$$"
volume="${container}-cache"
network="${container}-net"
upstream="${container}-upstream"
probe="${container}-probe"
scratch_dir="$(mktemp -d)"
cleanup() {
  local status=$?
  if [[ $status -ne 0 ]]; then
    docker logs "$container" >&2 || true
    docker logs "$upstream" >&2 || true
  fi
  docker rm -f "$probe" "$container" "$upstream" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf -- "$scratch_dir"
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The network has no external route. The sole private upstream is allowed only
# in the temporary cache-test config, after testing the shipped deny policy.
docker network create --internal "$network" >/dev/null
docker volume create "$volume" >/dev/null
run_cache() {
  local mounts=()
  if [[ $# -gt 0 ]]; then
    mounts=(--mount "type=bind,source=$1,target=/etc/apt-cacher-ultra/config.toml,readonly")
  fi
  docker run -d --name "$container" --network "$network" --network-alias cache \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --health-interval 1s --health-start-period 1s --health-timeout 3s \
    --mount "source=$volume,target=/var/cache/apt-cacher-ultra" \
    "${mounts[@]}" "$image" >/dev/null
}
wait_healthy() {
  local attempt state
  for ((attempt = 0; attempt < 60; attempt++)); do
    state="$(docker inspect --format '{{.State.Running}} {{.State.Health.Status}}' "$container")"
    if [[ "$state" == 'true healthy' ]]; then return; fi
    if [[ "$state" == false* ]]; then break; fi
    sleep 1
  done
  docker logs "$container"
  echo 'Production image did not become healthy.' >&2
  return 1
}
run_probe() {
  docker run --rm --name "$probe" --network "$network" \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --entrypoint /bin/sh "$image" -c "$1"
}

run_cache
wait_healthy
test "$(docker exec "$container" id -u)" = 10001
docker exec "$container" test -s /etc/ssl/certs/ca-certificates.crt

# Require explicit policy refusals, not timeouts or broken DNS/network.
run_probe '
  set -eu
  for target in http://127.0.0.1:6789/healthz http://localhost:6789/healthz http://cache:6789/healthz http://169.254.169.254/; do
    response=$(http_proxy=http://cache:3142 wget -Y on -T 3 -S -O - "$target" 2>&1) || true
    if ! printf "%s\n" "$response" | grep -q "HTTP/1.1 403"; then
      printf "Expected proxy policy refusal for %s:\n%s\n" "$target" "$response" >&2
      exit 1
    fi
  done
  if wget -T 3 -q -O /dev/null http://cache:6789/healthz; then
    echo "Admin is reachable from a neighboring container." >&2
    exit 1
  fi
'
docker rm -f "$container" >/dev/null

# Opt in to exactly one mock mirror for a real miss/hit check, with no internet.
awk '
  /^allowed_host_regex = \[/ { print "allowed_host_regex = [\"^upstream$\"]"; next }
  /^deny_target_ranges = \[/ { print "deny_target_ranges = []"; skip = 1; next }
  skip && /^\]/ { skip = 0; next }
  !skip { print }
' "$repo_root/docker/config.toml" > "$scratch_dir/http.toml"
chmod 0644 "$scratch_dir/http.toml"
mkdir -m 0755 "$scratch_dir/www"
printf 'acu production cache smoke test\n' > "$scratch_dir/www/payload"
chmod 0644 "$scratch_dir/www/payload"
cat > "$scratch_dir/www/serve" <<'EOF'
#!/bin/sh
# Consume request headers before returning the fixed cacheable payload.
while IFS= read -r line && [ "$line" != "$(printf '\r')" ] && [ -n "$line" ]; do :; done
printf 'HTTP/1.1 200 OK\r\nContent-Length: %s\r\nCache-Control: public, max-age=3600\r\nConnection: close\r\n\r\n' "$(wc -c < /www/payload)"
cat /www/payload
EOF
chmod 0755 "$scratch_dir/www/serve"
docker run -d --name "$upstream" --network "$network" --network-alias upstream \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --mount "type=bind,source=$scratch_dir/www,target=/www,readonly" \
  --entrypoint /bin/busybox "$image" nc -lk -p 8080 -e /www/serve >/dev/null
run_cache "$scratch_dir/http.toml"
wait_healthy
run_probe '
  set -eu
  response=$(http_proxy=http://cache:3142 wget -Y on -T 5 -S -O - http://upstream:8080/payload 2>&1) || { printf "%s\n" "$response" >&2; exit 1; }
  printf "%s\n" "$response"
  printf "%s\n" "$response" | grep -qi "X-Cache: MISS"
  printf "%s\n" "$response" | grep -q "acu production cache smoke test"
'
docker stop "$upstream" >/dev/null
run_probe '
  set -eu
  response=$(http_proxy=http://cache:3142 wget -Y on -T 5 -S -O - http://upstream:8080/payload 2>&1) || { printf "%s\n" "$response" >&2; exit 1; }
  printf "%s\n" "$response"
  printf "%s\n" "$response" | grep -qi "X-Cache: HIT"
  printf "%s\n" "$response" | grep -q "acu production cache smoke test"
'
docker rm -f "$container" >/dev/null

# Enabling MITM alone retains the shipped constrained CA policy.
sed '/^\[tls_mitm\]/,$s/^enabled = false$/enabled = true/' \
  "$repo_root/docker/config.toml" > "$scratch_dir/mitm.toml"
chmod 0644 "$scratch_dir/mitm.toml"
run_cache "$scratch_dir/mitm.toml"
wait_healthy
docker exec "$container" apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml > "$scratch_dir/ca-before"
test -s "$scratch_dir/ca-before"
test "$(docker exec "$container" stat -c %a /var/cache/apt-cacher-ultra/ca)" = 700
test "$(docker exec "$container" stat -c %a /var/cache/apt-cacher-ultra/ca/ca.key)" = 600
docker rm -f "$container" >/dev/null
run_cache "$scratch_dir/mitm.toml"
wait_healthy
docker exec "$container" apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml > "$scratch_dir/ca-after"
cmp "$scratch_dir/ca-before" "$scratch_dir/ca-after"
echo 'Production image: isolation, upstream restrictions, HTTP miss/hit, non-root user, TLS roots, and constrained CA persistence passed.'
