#!/bin/bash
# SPEC §14 step 3 in-container test. Installs the bundled
# apt-cacher-ultra*.deb, validates the package contract (files,
# user, perms, unit syntax), then launches the daemon directly
# (same ExecStart as the systemd unit) and verifies it binds the
# listener and answers HTTP.
#
# We do NOT spin up systemd-as-PID-1 here — that requires either a
# third-party systemd-enabled base image or --privileged + cgroup
# gymnastics that vary across host kernels and CI runners. The
# systemd unit's correctness is checked statically via
# `systemd-analyze verify`. End-to-end "serves traffic via systemd"
# coverage comes from step 4 (production deployment) per §14.

set -euo pipefail

echo "[deb-test] sanity: exactly one .deb in /tmp"
debs=(/tmp/apt-cacher-ultra_*.deb)
if [[ ${#debs[@]} -ne 1 ]]; then
    echo "FAIL: expected 1 .deb in /tmp, got ${#debs[@]}: ${debs[*]:-<none>}"
    exit 1
fi
echo "[deb-test] installing ${debs[0]}"

# `dpkg -i` first to capture exit (apt swallows it). The deb only
# depends on `adduser` which is in the base image, so this should
# succeed first try; the apt-get fallback only kicks in if a future
# dependency change introduces an unmet runtime dep.
dpkg -i "${debs[0]}" || (apt-get update && apt-get install -fy && dpkg -i "${debs[0]}")

echo "[deb-test] verify file layout"
test -x /usr/sbin/apt-cacher-ultra        || { echo "FAIL: /usr/sbin/apt-cacher-ultra missing"; exit 1; }
test -f /etc/apt-cacher-ultra/config.toml || { echo "FAIL: /etc/apt-cacher-ultra/config.toml missing"; exit 1; }
test -f /lib/systemd/system/apt-cacher-ultra.service \
    || { echo "FAIL: /lib/systemd/system/apt-cacher-ultra.service missing"; exit 1; }

echo "[deb-test] verify user/group created by preinstall"
id apt-cacher-ultra >/dev/null              || { echo "FAIL: user not created"; exit 1; }
getent group apt-cacher-ultra >/dev/null    || { echo "FAIL: group not created"; exit 1; }

echo "[deb-test] verify cache dir owner/perms set by postinstall"
mode="$(stat -c '%U:%G %a' /var/cache/apt-cacher-ultra)"
expected="apt-cacher-ultra:apt-cacher-ultra 750"
if [[ "$mode" != "$expected" ]]; then
    echo "FAIL: /var/cache/apt-cacher-ultra perms wrong"
    echo "  want: $expected"
    echo "  got:  $mode"
    exit 1
fi

echo "[deb-test] verify systemd unit syntax"
# `systemd-analyze verify` parses the unit and checks ExecStart
# resolution, dependencies, and known-bad directives. It does NOT
# require systemd-as-PID-1.
systemd-analyze verify --no-pager /lib/systemd/system/apt-cacher-ultra.service

# Daemon log capture: if the binary errors before binding, the
# /dev/tcp wait loop will time out and we want the stderr trail
# in the failure output.
daemon_log=/tmp/daemon.log
echo "[deb-test] launching daemon (same ExecStart as systemd unit)"
runuser -u apt-cacher-ultra -- \
    /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
    >"$daemon_log" 2>&1 &
DAEMON_PID=$!
cleanup() {
    local ec=$?
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    if [[ $ec -ne 0 && -s "$daemon_log" ]]; then
        echo "=== daemon log ==="
        cat "$daemon_log"
    fi
    exit $ec
}
trap cleanup EXIT

echo "[deb-test] wait for :3142 listener"
ready=0
for i in $(seq 1 30); do
    if (echo > /dev/tcp/127.0.0.1/3142) 2>/dev/null; then
        ready=1
        echo "[deb-test] :3142 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ready -eq 0 ]]; then
    echo "FAIL: daemon did not bind :3142 within 30s"
    exit 1
fi

echo "[deb-test] HTTP probe (any well-formed response confirms HTTP stack is up)"
# A bare GET / will not be a valid proxy request (proxy mode wants
# http://upstream/path). The cache should respond with a 4xx — we
# do not assert a specific code, only that the daemon answered HTTP.
status=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:3142/ || echo "000")
echo "[deb-test] / responded HTTP ${status}"
if [[ "$status" == "000" || -z "$status" ]]; then
    echo "FAIL: daemon did not produce an HTTP response"
    exit 1
fi
if [[ "$status" -lt 200 || "$status" -gt 599 ]]; then
    echo "FAIL: unexpected HTTP code: $status"
    exit 1
fi

echo "[deb-test] PASS"
