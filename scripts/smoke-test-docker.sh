#!/usr/bin/env bash
# Validate the production image, HTTP and MITM settings, and persistent MITM CA.
set -euo pipefail
image="${1:-apt-cacher-ultra:smoke}"
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
container="acu-smoke-${RANDOM}-$$"
volume="${container}-cache"
ca_before="$(mktemp)"
ca_after="$(mktemp)"
mitm_config="$(mktemp)"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
  rm -f "$ca_before" "$ca_after" "$mitm_config"
}
trap cleanup EXIT
# Exercise the documented opt-in settings from the single shipped config.
sed -e '/^\[tls_mitm\]/,$s/^enabled = false$/enabled = true/' \
    -e '/^\[tls_mitm\]/,$s/^# allowed_host_regex =/allowed_host_regex =/' \
    -e '/^\[tls_mitm\]/,$s/^# allow_unconstrained_ca =/allow_unconstrained_ca =/' \
    "$repo_root/docker/config.toml" > "$mitm_config"
chmod 0644 "$mitm_config"
wait_healthy() {
  for attempt in {1..60}; do
    if docker exec "$container" wget -q -O /dev/null http://127.0.0.1:6789/healthz; then
      return
    fi
    sleep 1
  done
  docker logs "$container"
  return 1
}
docker volume create "$volume" >/dev/null
docker run -d --name "$container" --mount "source=$volume,target=/var/cache/apt-cacher-ultra" "$image" >/dev/null
wait_healthy
test "$(docker exec "$container" id -u)" = 10001
docker exec "$container" test -s /etc/ssl/certs/ca-certificates.crt
docker rm -f "$container" >/dev/null
docker run -d --name "$container" \
  --mount "source=$volume,target=/var/cache/apt-cacher-ultra" \
  --mount "type=bind,source=$mitm_config,target=/etc/apt-cacher-ultra/config.toml,readonly" \
  "$image" >/dev/null
wait_healthy
docker exec "$container" apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml > "$ca_before"
test -s "$ca_before"
docker restart "$container" >/dev/null
wait_healthy
docker exec "$container" apt-cacher-ultra ca print -config /etc/apt-cacher-ultra/config.toml > "$ca_after"
cmp "$ca_before" "$ca_after"
echo 'Production image: HTTP/MITM startup, non-root user, TLS roots, and CA persistence passed.'
