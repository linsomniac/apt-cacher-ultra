#!/bin/bash
# SPEC §14 step 3 in-container test. Installs the bundled
# apt-cacher-ultra*.deb, validates the package contract (files,
# user, perms, unit syntax), then launches the daemon directly
# (same ExecStart as the systemd unit) and verifies it binds the
# listener and answers HTTP.
#
# Scope: this is package-contract validation + an ExecStart smoke
# test, NOT a "serves traffic via systemd unit" test. Spinning up
# systemd-as-PID-1 here would require either a third-party
# systemd-enabled base image or --privileged + cgroup gymnastics
# that vary across host kernels and CI runners. Consequence: we
# verify the unit file is syntactically correct (`systemd-analyze
# verify`) and that the binary launches under the packaged user
# with the packaged config — but the runtime sandbox directives in
# the unit (ProtectSystem=strict, ReadWritePaths, RestrictNamespaces,
# NoNewPrivileges, etc.) are NOT exercised. That coverage is the
# job of step 4 (production deployment) per SPEC §14.
#
# Phase 5 checks the maintainer scripts' calls to systemd, and the
# install hint, on a fresh install, an upgrade, dpkg's abort-* actions, a
# removal and a reinstall after it, against a stub systemctl (its
# AIDEV-NOTE says how).

set -euo pipefail

# Pre-init so the EXIT trap (and any pre-launch failure) can safely
# read DAEMON_PID under set -u via ${DAEMON_PID:-}.
DAEMON_PID=""

echo "[deb-test] sanity: exactly one .deb in /tmp"
# nullglob — without it, an unmatched glob expands to the literal
# pattern, which silently passes the "exactly one" check and turns
# the missing-deb case into a `dpkg` error one step later.
shopt -s nullglob
debs=(/tmp/apt-cacher-ultra_*.deb)
shopt -u nullglob
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
test -f /usr/share/doc/apt-cacher-ultra/copyright \
    || { echo "FAIL: /usr/share/doc/apt-cacher-ultra/copyright missing"; exit 1; }
grep -q 'CC0-1.0' /usr/share/doc/apt-cacher-ultra/copyright \
    || { echo "FAIL: /usr/share/doc/apt-cacher-ultra/copyright does not declare CC0-1.0"; exit 1; }

echo "[deb-test] verify ownership and exact mode match the package contract"
# Exact-mode assertion (not a bit-mask check). nfpm.yaml now declares
# each shipped path's mode explicitly, so the test asserts equality.
# This catches both 0664 regressions (umask 002 leaking into nfpm
# output) AND special-bit slip-ups like a 4755 setuid binary, neither
# of which a `mode & 0022` mask would flag. The /etc/apt-cacher-ultra
# directory is included — a 0775 or world-writable config dir lets a
# local user swap config.toml despite the file itself being 0644.
declare -A expected_mode=(
    [/usr/sbin/apt-cacher-ultra]="755"
    [/etc/apt-cacher-ultra]="755"
    [/etc/apt-cacher-ultra/config.toml]="644"
    [/lib/systemd/system/apt-cacher-ultra.service]="644"
    [/usr/share/doc/apt-cacher-ultra/copyright]="644"
)
for path in "${!expected_mode[@]}"; do
    info="$(stat -c '%U:%G %a' "$path")"
    owner="${info% *}"
    mode="${info##* }"
    want="${expected_mode[$path]}"
    if [[ "$owner" != "root:root" ]]; then
        echo "FAIL: $path owned by $owner, expected root:root"
        exit 1
    fi
    if [[ "$mode" != "$want" ]]; then
        echo "FAIL: $path mode is $mode, expected $want"
        exit 1
    fi
done

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

# stop_daemon: graceful kill + wait + clear DAEMON_PID + confirm
# :3142 is fully released. Used between phases so the next phase
# can wipe cache and start a fresh daemon without racing the
# previous process. Clearing DAEMON_PID also keeps the EXIT trap
# from re-killing a PID that has already been reaped (which after
# enough fork churn could collide with a different process).
stop_daemon() {
    if [[ -z "${DAEMON_PID:-}" ]]; then
        return 0
    fi
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
    for _ in $(seq 1 5); do
        if ! (echo > /dev/tcp/127.0.0.1/3142) 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    echo "WARN: :3142 still reachable 5s after stop_daemon"
}

# wait_snapshot COLUMN FILE LIMIT: polls once a second, LIMIT times,
# until /dists/noble's current snapshot records FILE's SHA-256 in
# COLUMN (inrelease_hash or release_hash). Sets SNAPSHOT_ID and
# SNAPSHOT_AFTER (seconds) on success; on timeout prints a line naming
# COLUMN, FILE, LIMIT and the last current snapshot seen, and returns 1.
#
# AIDEV-NOTE: eager first adoption (flat-repositories spec C4 Dispatch
# 3) adopts a suite at its first contact, so "some snapshot is current"
# no longer says which upstream version was adopted: every wait names
# the exact anchor bytes it expects.
wait_snapshot() {
    local column=$1 file=$2 limit=$3 want got i
    want="$(sha256sum "$file" | cut -d' ' -f1)"
    SNAPSHOT_ID=""
    # An unreadable FILE would leave want empty, which a snapshot whose
    # COLUMN is NULL (IFNULL → '') would match: refuse it instead.
    if [[ ! "$want" =~ ^[0-9a-f]{64}$ ]]; then
        echo "wait_snapshot: cannot hash $file"
        return 1
    fi
    for i in $(seq 1 "$limit"); do
        got=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
            "SELECT f.current_snapshot_id || '|' || IFNULL(s.${column}, '') FROM suite_freshness f JOIN suite_snapshot s ON s.snapshot_id = f.current_snapshot_id WHERE f.suite_path='/dists/noble' LIMIT 1;" \
            2>/dev/null || echo "")
        if [[ -n "$got" && "${got#*|}" == "$want" ]]; then
            SNAPSHOT_ID="${got%%|*}"
            SNAPSHOT_AFTER=$i
            return 0
        fi
        sleep 1
    done
    echo "wait_snapshot: no current /dists/noble snapshot with $column = sha256($file) = ${want:0:12}.. after $limit polls (last seen: id|$column = ${got:-<none>})"
    return 1
}

# dump_adoption_state: the suite and snapshot rows and the daemon log
# tail, for a failed wait.
dump_adoption_state() {
    echo "=== suite_freshness ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
    echo "=== suite_snapshot ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT snapshot_id, suite_path, inrelease_hash, release_hash, release_gpg_hash, adopted_at FROM suite_snapshot;" || true
    echo "=== daemon log tail ==="
    tail -100 "$daemon_log" || true
}

echo "[deb-test] launching daemon (same ExecStart as systemd unit)"
runuser -u apt-cacher-ultra -- \
    /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
    >"$daemon_log" 2>&1 &
DAEMON_PID=$!
cleanup() {
    local ec=$?
    if [[ -n "${DAEMON_PID:-}" ]]; then
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
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

echo "[deb-test] phase 1 (package contract + listener) PASS"

# ============================================================
# SPEC2 §14 step 6: adoption smoke
#
# Restart the daemon under an adoption-mode config and verify the
# cache adopts a snapshot when upstream's signed InRelease changes.
# The fixture provides V1 + V2 trees signed with the same key. The
# first GET of V1 adopts it at once (eager first adoption, flat-
# repositories spec C4 Dispatch 3); swapping to V2 and GETting the
# InRelease again drives a second, change-driven adoption, which this
# phase waits for by V2's exact InRelease bytes.
# ============================================================

echo
echo "[deb-test] phase 2: stop phase-1 daemon"
stop_daemon

# Sanity: the repo-build stage produced both fixtures and a key.
test -d /fixture-v1 || { echo "FAIL: /fixture-v1 missing (repo-build stage broken?)"; exit 1; }
test -d /fixture-v2 || { echo "FAIL: /fixture-v2 missing"; exit 1; }
test -f /fixture-v1/aculan-test.gpg || { echo "FAIL: /fixture-v1/aculan-test.gpg missing"; exit 1; }
test -f /fixture-v1/aculan-test.fingerprint || { echo "FAIL: /fixture-v1/aculan-test.fingerprint missing"; exit 1; }
FP="$(cat /fixture-v1/aculan-test.fingerprint)"
echo "[deb-test] adoption fixture key fingerprint: $FP"

echo "[deb-test] phase 2: install signing pubkey into /etc/apt/trusted.gpg.d/"
install -o root -g root -m 0644 /fixture-v1/aculan-test.gpg /etc/apt/trusted.gpg.d/aculan-test.gpg

echo "[deb-test] phase 2: stage V1 fixture as nginx webroot"
# nginx-light defaults to /var/www/html. Replace its contents with
# V1's apt repo so the cache sees a fresh upstream on first GET.
mkdir -p /var/www/html
rm -rf /var/www/html/*
cp -r /fixture-v1/dists /var/www/html/
cp -r /fixture-v1/pool /var/www/html/

echo "[deb-test] phase 2: start nginx serving fixture"
nginx
ngx_ready=0
for i in $(seq 1 10); do
    if (echo > /dev/tcp/127.0.0.1/80) 2>/dev/null; then
        ngx_ready=1
        echo "[deb-test] nginx :80 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ngx_ready -eq 0 ]]; then
    echo "FAIL: nginx did not bind :80 within 10s"
    exit 1
fi

echo "[deb-test] phase 2: write adoption-mode config"
# Cooldown=1s so the second InRelease GET is not gated by the
# default-60s cooldown. allowed_host_regex must permit 127.0.0.1
# (the only upstream the test reaches); deny_target_ranges must be
# empty because the cache's default blocks loopback (127.0.0.0/8)
# and would refuse to dial nginx.
#
# Backslashes are doubled inside the heredoc — a single `\.` would
# be eaten by bash's heredoc expansion, leaving the regex with a
# literal-but-unintended dot. Doubling produces `^127\.0\.0\.1$` in
# the file, which is what the canonicalizer matches against.
cat > /etc/apt-cacher-ultra/config.toml <<EOF
[cache]
dir = "/var/cache/apt-cacher-ultra"
listen = "0.0.0.0:3142"

[upstream]
allowed_host_regex = ["^127\\\\.0\\\\.0\\\\.1\$"]
deny_target_ranges = []

[freshness]
cooldown = "1s"
periodic_refresh = "1h"

[adoption]
enabled = true
require_signature = true

[[trusted_signer]]
match_canonical_host = "127.0.0.1"
fingerprints = ["$FP"]

[log]
level = "info"
format = "text"
EOF
chmod 0644 /etc/apt-cacher-ultra/config.toml

echo "[deb-test] phase 2: restart daemon under adoption-mode config"
runuser -u apt-cacher-ultra -- \
    /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
    >>"$daemon_log" 2>&1 &
DAEMON_PID=$!
# trap was already installed in phase 1; it reads $DAEMON_PID at exit
# time, so updating the variable is sufficient.

ready=0
for i in $(seq 1 30); do
    if (echo > /dev/tcp/127.0.0.1/3142) 2>/dev/null; then
        ready=1
        echo "[deb-test] phase-2 :3142 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ready -eq 0 ]]; then
    echo "FAIL: phase-2 daemon did not bind :3142 within 30s"
    exit 1
fi

# 2a: prime the cache with V1's InRelease (cache miss → upstream).
# The miss creates the suite, and its first check adopts V1 at once
# (2a'). The url_path row it stores matters only until then: once a
# snapshot is current, the check reconstructs a missing anchor row.
echo "[deb-test] phase 2: GET InRelease via proxy (V1, primes cache)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/inrelease.v1 \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: V1 InRelease via proxy returned $status; expected 200"
    head -c 4096 /tmp/inrelease.v1 || true
    exit 1
fi
echo "[deb-test] V1 InRelease: $(wc -c < /tmp/inrelease.v1) bytes"

# 2a': that miss created the suite and started its first check, which
# adopts V1 at once (eager first adoption). Wait for it, so the V2
# change below is checked against an adopted suite, not raced by the
# first adoption still holding the suite.
echo "[deb-test] phase 2: wait for the eager first adoption of V1"
if ! wait_snapshot inrelease_hash /fixture-v1/dists/noble/InRelease 30; then
    echo "FAIL: V1 was not adopted at first contact within 30s"
    dump_adoption_state
    exit 1
fi
echo "[deb-test] V1 adopted at first contact: snapshot id=$SNAPSHOT_ID (after ${SNAPSHOT_AFTER}s)"

# 2b: swap upstream to V2 (different .deb version → different
# Packages SHA256 → different Release content → different InRelease
# bytes). Sleep > cooldown (1s) so the next request's freshness
# check is not gated.
echo "[deb-test] phase 2: swap nginx webroot to V2"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2/dists /var/www/html/
cp -r /fixture-v2/pool /var/www/html/

# 2c: drive a second InRelease GET. The cache serves it from the V1
# snapshot, fires maybeFireFreshness, the freshness check sees changed
# bytes, and spawns the adoption goroutine.
echo "[deb-test] phase 2: GET InRelease via proxy (snapshot hit → fires freshness check → adoption)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/inrelease.trigger \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: trigger InRelease returned $status; expected 200"
    exit 1
fi

# 2d: poll for the V2 snapshot. Adoption is async (the response in 2c
# came from the V1 snapshot), and the flip barrier first waits for the
# suite's InRelease serves to go quiet (adoption.flip_quiet_period,
# 10 s by default).
echo "[deb-test] phase 2: poll for the V2 snapshot"
if ! wait_snapshot inrelease_hash /fixture-v2/dists/noble/InRelease 60; then
    echo "FAIL: V2 was not adopted within 60s"
    dump_adoption_state
    exit 1
fi
adopted_id="$SNAPSHOT_ID"
echo "[deb-test] adopted V2 snapshot id=$adopted_id (after ${SNAPSHOT_AFTER}s)"

# 2e: a follow-up GET should now serve via snapshot_member resolution
# and set the X-Cache-Snapshot response header (SPEC2 §6.1 / §10.1)
# to the V2 snapshot's id.
echo "[deb-test] phase 2: follow-up GET should expose X-Cache-Snapshot"
hdrs_file=/tmp/inrelease.headers
status=$(curl -sS -x http://127.0.0.1:3142 \
    -D "$hdrs_file" \
    -o /tmp/inrelease.followup \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: follow-up InRelease returned $status"
    cat "$hdrs_file" || true
    exit 1
fi
if ! grep -qi '^x-cache-snapshot:' "$hdrs_file"; then
    echo "FAIL: follow-up response is missing X-Cache-Snapshot header"
    echo "=== response headers ==="
    cat "$hdrs_file"
    exit 1
fi
xcs="$(grep -i '^x-cache-snapshot:' "$hdrs_file" | tr -d '\r')"
echo "[deb-test] follow-up: $xcs"
xcs_id="${xcs#*:}"
xcs_id="${xcs_id//[[:space:]]/}"
if [[ "$xcs_id" != "$adopted_id" ]]; then
    echo "FAIL: follow-up X-Cache-Snapshot is '$xcs_id', want $adopted_id (the V2 snapshot)"
    exit 1
fi

echo "[deb-test] phase 2 (inline adoption smoke) PASS"

# ============================================================
# SPEC2 §7.6.3 phase 3: detached-mode adoption smoke
#
# Re-runs the phase-2 flow with detached fixtures (Release +
# Release.gpg, no InRelease). Validates that a suite whose upstream
# publishes no InRelease is adopted in the detached form — at first
# contact, by the probe tree (InRelease absent → Release →
# Release.gpg: flat-repositories spec C4 Dispatch 3), then again when
# its Release changes — and that the adopted snapshot's release_hash
# is set and inrelease_hash is nil.
#
# A clean cache directory is required: phase 2 already adopted an
# inline snapshot.
# ============================================================

echo
echo "[deb-test] phase 3: stop phase-2 daemon and wipe cache"
stop_daemon
# Wipe cache so the suite starts fresh (no current_snapshot_id, no
# url_path rows). Daemon recreates cache.db on next start.
rm -rf /var/cache/apt-cacher-ultra/* 2>/dev/null || true
chown apt-cacher-ultra:apt-cacher-ultra /var/cache/apt-cacher-ultra

# Sanity: the detached fixture exists and uses the same signing key
# as the inline fixture (same FINGERPRINT — repo-build shares the
# keyring across all four invocations).
test -d /fixture-v1-detached || { echo "FAIL: /fixture-v1-detached missing"; exit 1; }
test -d /fixture-v2-detached || { echo "FAIL: /fixture-v2-detached missing"; exit 1; }
test -f /fixture-v1-detached/dists/noble/Release.gpg \
    || { echo "FAIL: /fixture-v1-detached missing Release.gpg"; exit 1; }
if [ -f /fixture-v1-detached/dists/noble/InRelease ]; then
    echo "FAIL: detached fixture unexpectedly contains InRelease"
    exit 1
fi
FP_DETACHED="$(cat /fixture-v1-detached/aculan-test.fingerprint)"
if [[ "$FP_DETACHED" != "$FP" ]]; then
    echo "FAIL: detached fingerprint $FP_DETACHED != inline fingerprint $FP"
    exit 1
fi

echo "[deb-test] phase 3: stage V1-detached fixture as nginx webroot"
rm -rf /var/www/html/*
cp -r /fixture-v1-detached/dists /var/www/html/
cp -r /fixture-v1-detached/pool /var/www/html/

echo "[deb-test] phase 3: restart daemon (config reused from phase 2)"
runuser -u apt-cacher-ultra -- \
    /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
    >>"$daemon_log" 2>&1 &
DAEMON_PID=$!

ready=0
for i in $(seq 1 30); do
    if (echo > /dev/tcp/127.0.0.1/3142) 2>/dev/null; then
        ready=1
        echo "[deb-test] phase-3 :3142 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ready -eq 0 ]]; then
    echo "FAIL: phase-3 daemon did not bind :3142 within 30s"
    exit 1
fi

# 3a: prime the cache with V1-detached's Release + Release.gpg. We do
# NOT request InRelease — the upstream doesn't serve it and the cache
# would 404. The Release miss creates the suite, whose first check
# adopts V1-detached at once (3a'); the url_path rows matter only until
# then: once a snapshot is current, the check reconstructs a missing
# anchor row.
echo "[deb-test] phase 3: GET Release + Release.gpg via proxy (V1-detached, primes cache)"
for path in /dists/noble/Release /dists/noble/Release.gpg; do
    status=$(curl -sS -x http://127.0.0.1:3142 \
        -o "/tmp/$(basename "$path").v1d" \
        -w '%{http_code}' \
        --max-time 10 \
        "http://127.0.0.1${path}" || echo "000")
    if [[ "$status" != "200" ]]; then
        echo "FAIL: V1-detached $path via proxy returned $status; expected 200"
        exit 1
    fi
done
echo "[deb-test] V1 Release: $(wc -c < /tmp/Release.v1d) bytes; Release.gpg: $(wc -c < /tmp/Release.gpg.v1d) bytes"

# 3a': the Release miss created the suite; its first check adopts
# V1-detached at once (eager first adoption, detached form).
echo "[deb-test] phase 3: wait for the eager first adoption of V1-detached"
if ! wait_snapshot release_hash /fixture-v1-detached/dists/noble/Release 30; then
    echo "FAIL: V1-detached was not adopted at first contact within 30s"
    dump_adoption_state
    exit 1
fi
echo "[deb-test] V1-detached adopted at first contact: snapshot id=$SNAPSHOT_ID (after ${SNAPSHOT_AFTER}s)"

# 3b: swap to V2-detached.
echo "[deb-test] phase 3: swap nginx webroot to V2-detached"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2-detached/dists /var/www/html/
cp -r /fixture-v2-detached/pool /var/www/html/

# 3c: drive a Release GET. The cache serves it from the V1-detached
# snapshot and fires the freshness check: the current snapshot is
# detached, so the check probes InRelease (absent), then GETs Release
# (200, changed) and Release.gpg, and the adoption goroutine runs
# RunDetached, which verifies the detached signature and commits a
# snapshot with release_hash + release_gpg_hash set.
echo "[deb-test] phase 3: GET Release via proxy (snapshot hit → fires freshness → detached adoption)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/Release.trigger \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/Release || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: trigger Release returned $status; expected 200"
    exit 1
fi

# 3d: poll for the V2-detached snapshot. Same window as phase 2.
echo "[deb-test] phase 3: poll for the V2-detached snapshot"
if ! wait_snapshot release_hash /fixture-v2-detached/dists/noble/Release 60; then
    echo "FAIL: V2-detached was not adopted within 60s"
    dump_adoption_state
    exit 1
fi
adopted_id="$SNAPSHOT_ID"
echo "[deb-test] adopted detached snapshot id=$adopted_id (after ${SNAPSHOT_AFTER}s)"

# 3e: assert the snapshot is in detached form — release_hash is set,
# release_gpg_hash is set, inrelease_hash is NULL. This is the
# difference between a successful detached adoption and a confused
# inline-mode adoption that somehow ran against detached fixtures.
echo "[deb-test] phase 3: verify snapshot $adopted_id is detached form"
form_columns=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(inrelease_hash, '<null>') || '|' || IFNULL(release_hash, '<null>') || '|' || IFNULL(release_gpg_hash, '<null>') FROM suite_snapshot WHERE snapshot_id=$adopted_id;" || echo "")
if [[ -z "$form_columns" ]]; then
    echo "FAIL: cannot read snapshot $adopted_id's hashes from suite_snapshot"
    dump_adoption_state
    exit 1
fi
ir_hash="${form_columns%%|*}"
rest="${form_columns#*|}"
r_hash="${rest%%|*}"
rg_hash="${rest#*|}"
if [[ "$ir_hash" != "<null>" ]]; then
    echo "FAIL: detached snapshot has unexpected inrelease_hash=$ir_hash"
    exit 1
fi
if [[ "$r_hash" == "<null>" ]]; then
    echo "FAIL: detached snapshot is missing release_hash"
    exit 1
fi
if [[ "$rg_hash" == "<null>" ]]; then
    echo "FAIL: detached snapshot is missing release_gpg_hash"
    exit 1
fi
echo "[deb-test] phase 3: detached form confirmed (release_hash=${r_hash:0:12}.., release_gpg_hash=${rg_hash:0:12}..)"

# 3f: a follow-up GET should expose X-Cache-Snapshot, naming the
# V2-detached snapshot, just like phase 2.
echo "[deb-test] phase 3: follow-up GET should expose X-Cache-Snapshot"
hdrs_file=/tmp/release.headers
status=$(curl -sS -x http://127.0.0.1:3142 \
    -D "$hdrs_file" \
    -o /tmp/release.followup \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/Release || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: follow-up Release returned $status"
    cat "$hdrs_file" || true
    exit 1
fi
if ! grep -qi '^x-cache-snapshot:' "$hdrs_file"; then
    echo "FAIL: follow-up response is missing X-Cache-Snapshot header"
    cat "$hdrs_file"
    exit 1
fi
xcs="$(grep -i '^x-cache-snapshot:' "$hdrs_file" | tr -d '\r')"
echo "[deb-test] phase 3 follow-up: $xcs"
xcs_id="${xcs#*:}"
xcs_id="${xcs_id//[[:space:]]/}"
if [[ "$xcs_id" != "$adopted_id" ]]; then
    echo "FAIL: phase-3 follow-up X-Cache-Snapshot is '$xcs_id', want $adopted_id (the V2-detached snapshot)"
    exit 1
fi

echo "[deb-test] phase 3 (detached adoption smoke) PASS"

# ============================================================
# SPEC3 §14.5 phase 4: Phase 3 hot-package prefetch end-to-end.
#
# SPEC3 definition-of-done #5 requires the .deb-install harness to
# exercise at least one second-cycle adoption that successfully
# prefetches a hot deb. The unit/chaos suite already covers the
# wiring; this section proves the same wiring works from the
# packaged binary running under the packaged config schema.
#
# Flow (V1 → V2: the first adoption happens at first contact):
#   1. Stop, wipe cache, stage V1 fixture, restart with Phase 3
#      config (hot_packages.window, adoption.hot_prefetch_budget).
#   2. Prime the cache by GETting V1 InRelease (cold-cache miss). The
#      miss creates the suite and its first check adopts V1 at once
#      (eager first adoption, flat-repositories spec C4 Dispatch 3).
#      This is the FIRST adoption (no prior snapshot → hot_count=0).
#   3. GET the V1 .deb so url_path.last_requested_at on
#      hello-acu_1.0_amd64.deb is recorded — the (Package, Arch)
#      signal SPEC3 §7.5.3 Stage 1 mines on the next adoption.
#   4. Swap nginx webroot to V2 (hello-acu 1.1) and GET InRelease.
#      The changed InRelease drives the SECOND adoption, whose prior
#      snapshot is V1's: Stage 1 finds the hot pair (hello-acu,
#      amd64) requested from it, Stage 2 maps it to V2's
#      hello-acu_1.1_amd64.deb, and hot prefetch warms it before the
#      flip.
#   5. Assert:
#        - adoption_hot_prefetch_complete fires for the V2 snapshot
#          with hot_count >= 1, fetched >= 1, failed=0,
#          mismatched=0;
#        - url_path row for hello-acu_1.1_amd64.deb exists with a
#          non-null blob_hash AND last_fetched_at, and no client has
#          requested it (request_count = 0): CommitAdoption wrote
#          the prefetched row in the flip transaction per SPEC3
#          §7.5.1.
#
# AIDEV-NOTE: until eager first adoption this phase ran V1 → V2 → V1
# because the cold prime adopted nothing; now the cold prime is the
# first adoption, and going back to V1 would be refused (spec C5:
# rollback_refused / upstream_older), so the second cycle is V1 → V2.
# ============================================================

echo
echo "[deb-test] phase 4: stop phase-3 daemon and wipe cache"
stop_daemon
rm -rf /var/cache/apt-cacher-ultra/* 2>/dev/null || true
chown apt-cacher-ultra:apt-cacher-ultra /var/cache/apt-cacher-ultra

echo "[deb-test] phase 4: stage V1 (inline) fixture as nginx webroot"
rm -rf /var/www/html/*
cp -r /fixture-v1/dists /var/www/html/
cp -r /fixture-v1/pool /var/www/html/

echo "[deb-test] phase 4: write Phase 3 adoption config (hot_packages + hot_prefetch_budget)"
cat > /etc/apt-cacher-ultra/config.toml <<EOF
[cache]
dir = "/var/cache/apt-cacher-ultra"
listen = "0.0.0.0:3142"

[upstream]
allowed_host_regex = ["^127\\\\.0\\\\.0\\\\.1\$"]
deny_target_ranges = []

[freshness]
cooldown = "1s"
periodic_refresh = "1h"

[adoption]
enabled = true
require_signature = true
hot_prefetch_budget = "60s"

[[trusted_signer]]
match_canonical_host = "127.0.0.1"
fingerprints = ["$FP"]

[hot_packages]
window = "24h"

[log]
level = "info"
format = "text"
EOF
chmod 0644 /etc/apt-cacher-ultra/config.toml

# The daemon log is appended across phases, and phases 2 and 3 each log
# a snapshot_id=2 of their own (each wiped the cache, so ids restart at
# 1): phase 4's log checks read only the lines from here on.
p4_log_from=$(( $(wc -l < "$daemon_log") + 1 ))

echo "[deb-test] phase 4: restart daemon under Phase 3 config"
runuser -u apt-cacher-ultra -- \
    /usr/sbin/apt-cacher-ultra --config /etc/apt-cacher-ultra/config.toml \
    >>"$daemon_log" 2>&1 &
DAEMON_PID=$!

ready=0
for i in $(seq 1 30); do
    if (echo > /dev/tcp/127.0.0.1/3142) 2>/dev/null; then
        ready=1
        echo "[deb-test] phase-4 :3142 reachable after ${i}s"
        break
    fi
    sleep 1
done
if [[ $ready -eq 0 ]]; then
    echo "FAIL: phase-4 daemon did not bind :3142 within 30s"
    exit 1
fi

# 4a: cold-cache prime with V1 InRelease. The miss establishes the
# url_path row and creates the suite; its first check adopts V1.
echo "[deb-test] phase 4: cold prime — GET V1 InRelease via proxy"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.inrelease.v1 \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V1 InRelease via proxy returned $status"
    exit 1
fi

echo "[deb-test] phase 4: wait for V1's first adoption (eager, at first contact)"
if ! wait_snapshot inrelease_hash /fixture-v1/dists/noble/InRelease 30; then
    echo "FAIL: phase-4 V1 was not adopted at first contact within 30s"
    dump_adoption_state
    exit 1
fi
v1_id="$SNAPSHOT_ID"
echo "[deb-test] phase 4 V1 adopted snapshot id=$v1_id (after ${SNAPSHOT_AFTER}s)"

# 4b: hit the V1 .deb so SPEC3 §7.5.3 Stage 1 has a hot pair to mine
# from the V1 snapshot on the next adoption. The (Package=hello-acu,
# Arch=amd64) tuple is recorded with last_requested_at.
V1_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.0_amd64.deb"
echo "[deb-test] phase 4: GET V1 .deb to record url_path.last_requested_at"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.deb.v1 \
    -w '%{http_code}' \
    --max-time 10 \
    "http://127.0.0.1${V1_DEB_PATH}" || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V1 deb GET returned $status"
    exit 1
fi
v1_lr=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(last_requested_at, '') FROM url_path WHERE path='${V1_DEB_PATH}';" \
    2>/dev/null || echo "")
if [[ -z "$v1_lr" ]]; then
    echo "FAIL: phase-4 V1 .deb url_path.last_requested_at not recorded"
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT path, last_requested_at, request_count FROM url_path;" || true
    exit 1
fi

# 4c: swap upstream to V2 so the next freshness check sees changed
# bytes (current snapshot V1, upstream V2). Sleep > cooldown.
echo "[deb-test] phase 4: swap nginx webroot to V2 (drives V2 adoption + hot prefetch)"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2/dists /var/www/html/
cp -r /fixture-v2/pool /var/www/html/

echo "[deb-test] phase 4: GET InRelease to trigger V2 adoption (2nd adoption + hot prefetch)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.inrelease.trigger.v2 \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V2 trigger InRelease returned $status"
    exit 1
fi

# The bound: this flip follows the hot prefetch, which may run for its
# whole hot_prefetch_budget (60 s, above), and the flip barrier's quiet
# period (adoption.flip_quiet_period, 10 s by default, counted from the
# trigger GET: no client sends an anchor during this wait, so none of
# the barrier's longer waits applies), plus 20 s for the check, the
# member fetches and the commit: 90 s. A prefetch that runs out its
# budget still flips within this bound, so 4d prints its line, with
# the failed and unattempted counts, rather than a timeout here.
echo "[deb-test] phase 4: poll for V2 adoption (2nd)"
if ! wait_snapshot inrelease_hash /fixture-v2/dists/noble/InRelease 90; then
    echo "FAIL: phase-4 V2 adoption did not flip the suite within 90s (still v1_id=$v1_id)"
    dump_adoption_state
    exit 1
fi
v2_id="$SNAPSHOT_ID"
echo "[deb-test] phase 4 V2 adopted snapshot id=$v2_id (after ${SNAPSHOT_AFTER}s; prior was $v1_id)"

# 4d: assert SPEC3 §10.2 adoption_hot_prefetch_complete event fired
# for the V2 snapshot with hot_count >= 1, fetched >= 1, failed=0,
# mismatched=0. This is the "hot pair was warmed" oracle.
echo "[deb-test] phase 4: assert adoption_hot_prefetch_complete fired for snapshot $v2_id"
# The line is logged just after the flip wait_snapshot saw, so poll for
# it briefly. Read only phase 4's lines (p4_log_from), and match the id
# whole, so snapshot_id=2 cannot match snapshot_id=20.
hot_line=""
for _ in $(seq 1 10); do
    # `|| true`: with no such line the greps fail the pipeline, and
    # set -e would exit before the FAIL below names what is missing.
    hot_line=$(tail -n +"$p4_log_from" "$daemon_log" \
        | grep "msg=adoption_hot_prefetch_complete" \
        | grep -E "(^| )snapshot_id=${v2_id}( |$)" | tail -1) || true
    [[ -n "$hot_line" ]] && break
    sleep 1
done
if [[ -z "$hot_line" ]]; then
    echo "FAIL: no phase-4 adoption_hot_prefetch_complete log line for snapshot $v2_id within 10s of its flip"
    echo "=== relevant phase-4 log tail ==="
    tail -n +"$p4_log_from" "$daemon_log" | grep -E "hot_prefetch|adoption_" | tail -30 || true
    exit 1
fi
echo "[deb-test] phase 4 hot-prefetch line: $hot_line"
# Extract numeric fields. slog text format emits `key=value` tokens
# separated by spaces. Anchor on the bare key= so a longer suffix
# (e.g. `unattempted=`) doesn't match `attempted=`. A missing field
# fails by name: none of the four defaults to 0.
hot_count="" fetched="" failed="" mismatched=""
for f in hot_count fetched failed mismatched; do
    if [[ ! "$hot_line" =~ (^|\ )$f=([0-9]+)(\ |$) ]]; then
        echo "FAIL: phase-4 hot-prefetch line has no $f= field"
        exit 1
    fi
    printf -v "$f" '%s' "${BASH_REMATCH[2]}"
done
if [[ "$hot_count" -lt 1 ]]; then
    echo "FAIL: phase-4 hot_count=$hot_count, want >= 1 (Stage 1 should mine the V1 hot pair)"
    exit 1
fi
if [[ "$fetched" -lt 1 ]]; then
    echo "FAIL: phase-4 fetched=$fetched, want >= 1 (the V2 path should warm)"
    exit 1
fi
if [[ "$failed" -ne 0 ]]; then
    echo "FAIL: phase-4 failed=$failed, want 0"
    exit 1
fi
if [[ "$mismatched" -ne 0 ]]; then
    echo "FAIL: phase-4 mismatched=$mismatched, want 0"
    exit 1
fi

# 4e: assert the V2 .deb's url_path row exists with a non-null
# blob_hash and last_fetched_at, and that no client requested it —
# proof CommitAdoption inserted the prefetched row. SPEC3 §7.5.1: the
# row is inserted in the same transaction as the snapshot flip.
V2_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.1_amd64.deb"
v2_row=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(blob_hash, '') || '|' || IFNULL(last_fetched_at, '') || '|' || request_count FROM url_path WHERE path='${V2_DEB_PATH}';" \
    2>/dev/null || echo "")
if [[ -z "$v2_row" ]]; then
    echo "FAIL: phase-4 V2 .deb url_path row missing"
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT path, blob_hash, last_fetched_at, last_requested_at, request_count FROM url_path;" || true
    exit 1
fi
IFS='|' read -r v2_blob v2_lf v2_rc <<< "$v2_row"
if [[ -z "$v2_blob" ]]; then
    echo "FAIL: phase-4 V2 .deb url_path.blob_hash is null"
    exit 1
fi
if [[ -z "$v2_lf" ]]; then
    echo "FAIL: phase-4 V2 .deb url_path.last_fetched_at is null (prefetch should set this)"
    exit 1
fi
if [[ "$v2_rc" != "0" ]]; then
    echo "FAIL: phase-4 V2 .deb url_path.request_count=$v2_rc, want 0 (the row should come from the prefetch, not a client)"
    exit 1
fi
echo "[deb-test] phase 4 V2 .deb url_path: blob_hash=${v2_blob:0:12}.. last_fetched_at=$v2_lf request_count=$v2_rc"

echo "[deb-test] phase 4 (Phase 3 hot-prefetch) PASS"

# ============================================================
# Phase 5: the maintainer scripts under systemd (the 2026-09 security
# review's H-2). A fresh install starts nothing and prints the install
# message; an upgrade of the enabled unit asks systemd for exactly one
# try-restart (a running daemon moves to the new binary, a stopped one
# stays stopped), stops nothing first and prints no install message; a
# failed restart does not fail dpkg; policy-rc.d's "no" is honoured;
# abort-upgrade restarts too, a plain abort-remove does not, and neither
# prints the hint; a removal stops and disables the unit, and a reinstall
# after it prints the hint again; without systemd, or with DPKG_ROOT set,
# the hint prints and systemctl is not called.
#
# AIDEV-NOTE: no systemd runs here (see the header), so this phase makes
# the scripts see one: /run/systemd/system exists, and a systemctl stub
# first on PATH records each call (its arguments, one line) and succeeds,
# or fails a try-restart while $stubs/fail-try-restart exists. It keeps
# the unit's enablement as systemd would: enable creates $stubs/enabled,
# disable deletes it, and is-enabled succeeds only while it exists (the
# postinst asks it before printing the install hint). dpkg passes its PATH
# on to the maintainer scripts, and deb-systemd-invoke looks systemctl up
# on it, so the stub sees what systemd would be asked. The image's
# /usr/sbin/policy-rc.d (exit 101: Docker's images forbid service actions)
# is moved aside, except while the policy step checks that it is honoured.
# The abort-*, no-systemd and DPKG_ROOT steps run the installed postinst
# directly, as dpkg would. UPGRADE_FROM names the package to upgrade from
# (by default this tree's, the /tmp one phase 1 installed); the upgrade
# goes to /upgrade's, the same binary packaged at a higher version (the
# Dockerfile's build stage).
# ============================================================

echo
echo "[deb-test] phase 5: stop phase-4 daemon"
stop_daemon

shopt -s nullglob
upgrade_debs=(/upgrade/apt-cacher-ultra_*.deb)
shopt -u nullglob
if [[ ${#upgrade_debs[@]} -ne 1 ]]; then
    echo "FAIL: expected 1 .deb in /upgrade, got ${#upgrade_debs[@]}: ${upgrade_debs[*]:-<none>}"
    exit 1
fi
new_deb=${upgrade_debs[0]}
old_deb=${UPGRADE_FROM:-${debs[0]}}
new_version=$(dpkg-deb -f "$new_deb" Version)
old_version=$(dpkg-deb -f "$old_deb" Version)
if dpkg --compare-versions "$new_version" le "$old_version"; then
    echo "FAIL: phase 5 would upgrade from $old_version to $new_version, which is not newer"
    exit 1
fi

stubs=/acu-stubs
calls=$stubs/calls
mkdir -p "$stubs" /run/systemd/system
cat > "$stubs/systemctl" <<'EOF'
#!/bin/sh
echo "$*" >> /acu-stubs/calls
if [ "$1" = try-restart ] && [ -e /acu-stubs/fail-try-restart ]; then
    echo "systemctl stub: try-restart fails" >&2
    exit 1
fi
case $1 in
    enable) touch /acu-stubs/enabled ;;
    disable) rm -f /acu-stubs/enabled ;;
    is-enabled) [ -e /acu-stubs/enabled ] || exit 1 ;;
esac
exit 0
EOF
chmod 0755 "$stubs/systemctl"
mv /usr/sbin/policy-rc.d /usr/sbin/policy-rc.d.acu

# dpkg_step NAME ARGS...: runs `dpkg ARGS` with the stub first on PATH and
# an empty call record; its output goes to /tmp/p5.NAME.out, and a dpkg
# failure fails the test with that output.
dpkg_step() {
    local name=$1
    shift
    : > "$calls"
    if ! PATH="$stubs:$PATH" dpkg "$@" > "/tmp/p5.$name.out" 2>&1; then
        echo "FAIL: phase-5 $name: dpkg $* failed:"
        cat "/tmp/p5.$name.out"
        exit 1
    fi
}

# postinst_step NAME ARGS...: runs the installed package's postinst with
# ARGS, as dpkg would, otherwise as dpkg_step.
postinst_step() {
    local name=$1
    shift
    : > "$calls"
    if ! PATH="$stubs:$PATH" /var/lib/dpkg/info/apt-cacher-ultra.postinst "$@" > "/tmp/p5.$name.out" 2>&1; then
        echo "FAIL: phase-5 $name: postinst $* failed:"
        cat "/tmp/p5.$name.out"
        exit 1
    fi
}

# expect_count NAME WHAT WANT FILE PATTERN: FILE has WANT lines matching
# the extended regex PATTERN, or the test fails naming WHAT and showing
# the step's output (dpkg's, or the postinst's) and systemctl calls. A
# missing FILE fails too: grep -c prints nothing for it, which a want of 0
# would otherwise pass.
expect_count() {
    local name=$1 what=$2 want=$3 file=$4 pattern=$5 got
    if [[ -f $file ]]; then
        got=$(grep -cE -- "$pattern" "$file" || true)
    else
        got="no file $file, so no"
    fi
    if [[ "$got" != "$want" ]]; then
        echo "FAIL: phase-5 $name: $got $what, want $want"
        echo "=== the step's output ==="
        cat "/tmp/p5.$name.out"
        echo "=== systemctl calls ==="
        cat "$calls"
        exit 1
    fi
}

# Any call that starts or restarts a unit, after systemctl's options.
restarts='^(--[a-z-]+ )*(start|restart|try-restart|reload-or-restart|try-reload-or-restart) '
message='^apt-cacher-ultra installed\.$'

echo "[deb-test] phase 5: purge the phase-1 install"
dpkg_step purge --purge apt-cacher-ultra

echo "[deb-test] phase 5: fresh install of $old_version: no restart, the install message"
dpkg_step install -i "$old_deb"
expect_count install "start or restart calls" 0 "$calls" "$restarts"
expect_count install "install messages" 1 /tmp/p5.install.out "$message"

# The operator follows the hint: from here the stub's is-enabled succeeds.
PATH="$stubs:$PATH" systemctl enable apt-cacher-ultra.service

echo "[deb-test] phase 5: upgrade to $new_version, unit enabled: one try-restart, no stop, no install message"
dpkg_step upgrade -i "$new_deb"
expect_count upgrade "try-restart calls" 1 "$calls" '^try-restart apt-cacher-ultra\.service$'
expect_count upgrade "start or restart calls" 1 "$calls" "$restarts"
expect_count upgrade "stop calls" 0 "$calls" '^(--[a-z-]+ )*stop '
expect_count upgrade "install messages" 0 /tmp/p5.upgrade.out "$message"
installed=$(dpkg-query -W -f='${Version}' apt-cacher-ultra)
if [[ "$installed" != "$new_version" ]]; then
    echo "FAIL: phase-5 upgrade: installed version is $installed, want $new_version"
    exit 1
fi

echo "[deb-test] phase 5: a failed try-restart does not fail dpkg"
touch "$stubs/fail-try-restart"
dpkg_step failed-restart -i "$new_deb"
rm "$stubs/fail-try-restart"
expect_count failed-restart "try-restart calls" 1 "$calls" '^try-restart apt-cacher-ultra\.service$'
status=$(dpkg-query -W -f='${Status}' apt-cacher-ultra)
if [[ "$status" != "install ok installed" ]]; then
    echo "FAIL: phase-5 failed-restart: package status is '$status', want 'install ok installed'"
    exit 1
fi

echo "[deb-test] phase 5: policy-rc.d's 101 is honoured"
mv /usr/sbin/policy-rc.d.acu /usr/sbin/policy-rc.d
dpkg_step policy -i "$new_deb"
mv /usr/sbin/policy-rc.d /usr/sbin/policy-rc.d.acu
expect_count policy "start or restart calls" 0 "$calls" "$restarts"
expect_count policy "policy-rc.d refusals" 1 /tmp/p5.policy.out 'policy-rc\.d returned 101'

# dpkg runs the installed postinst with abort-upgrade NEW-VERSION when an
# upgrade to NEW-VERSION fails, and with a plain abort-remove when a
# removal's prerm fails. debhelper's snippet restarts only when $2 is set,
# and a plain abort-remove has none. Neither prints the install hint.
# The unit is disabled first, so that the abort-* actions' silence comes
# from the hint's configure gate, not from is-enabled.
PATH="$stubs:$PATH" systemctl disable apt-cacher-ultra.service
echo "[deb-test] phase 5: abort-upgrade: one try-restart; a plain abort-remove: none"
postinst_step abort-upgrade abort-upgrade 9.9.9~e2e
expect_count abort-upgrade "try-restart calls" 1 "$calls" '^try-restart apt-cacher-ultra\.service$'
expect_count abort-upgrade "start or restart calls" 1 "$calls" "$restarts"
expect_count abort-upgrade "install messages" 0 /tmp/p5.abort-upgrade.out "$message"
postinst_step abort-remove abort-remove
expect_count abort-remove "start or restart calls" 0 "$calls" "$restarts"
expect_count abort-remove "install messages" 0 /tmp/p5.abort-remove.out "$message"

# The operator's unit is enabled again before the remove, so that the
# reinstall's hint below shows the remove's disable (an enabled unit
# would get none).
PATH="$stubs:$PATH" systemctl enable apt-cacher-ultra.service
echo "[deb-test] phase 5: remove: stop and disable"
dpkg_step remove -r apt-cacher-ultra
expect_count remove "stop calls" 1 "$calls" '^stop apt-cacher-ultra\.service$'
expect_count remove "disable calls" 1 "$calls" '^disable apt-cacher-ultra\.service$'

# A remove keeps the package's configuration (dpkg's config-files state),
# so the reinstall is a configure with $2 set, as on an upgrade; the remove
# disabled the unit, so the hint prints again.
echo "[deb-test] phase 5: reinstall after the remove: the install message"
dpkg_step reinstall -i "$new_deb"
expect_count reinstall "install messages" 1 /tmp/p5.reinstall.out "$message"

# Without systemd the postinst asks it nothing, so the hint prints even
# though the stub would now answer "enabled".
PATH="$stubs:$PATH" systemctl enable apt-cacher-ultra.service
echo "[deb-test] phase 5: no systemd: no systemctl call, the install message"
rmdir /run/systemd/system
postinst_step no-systemd configure "$old_version"
mkdir /run/systemd/system
expect_count no-systemd "systemctl calls" 0 "$calls" '.'
expect_count no-systemd "install messages" 1 /tmp/p5.no-systemd.out "$message"

# DPKG_ROOT set (dpkg --root: another system's root) is the guards' other
# half: the same, with systemd running and the stub still "enabled".
echo "[deb-test] phase 5: DPKG_ROOT set: no systemctl call, the install message"
DPKG_ROOT=/acu-root postinst_step dpkg-root configure "$old_version"
expect_count dpkg-root "systemctl calls" 0 "$calls" '.'
expect_count dpkg-root "install messages" 1 /tmp/p5.dpkg-root.out "$message"

echo "[deb-test] phase 5 (maintainer scripts under systemd) PASS"
echo "[deb-test] OVERALL PASS"
