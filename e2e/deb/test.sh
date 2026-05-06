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
# Adoption only fires on changed InRelease bytes (freshness.go:455);
# the fixture provides V1 + V2 trees signed with the same key, and
# we drive a swap between two metadata GETs.
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
# This populates url_path so the freshness check has a baseline to
# compare against.
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

# 2b: swap upstream to V2 (different .deb version → different
# Packages SHA256 → different Release content → different InRelease
# bytes). Sleep > cooldown (1s) so the next request's freshness
# check is not gated.
echo "[deb-test] phase 2: swap nginx webroot to V2"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2/dists /var/www/html/
cp -r /fixture-v2/pool /var/www/html/

# 2c: drive a second InRelease GET. The cache hits its url_path row
# from 2a, fires maybeFireFreshness, the freshness check sees changed
# bytes, and spawns the adoption goroutine.
echo "[deb-test] phase 2: GET InRelease via proxy (cache hit → fires freshness check → adoption)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/inrelease.trigger \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: trigger InRelease returned $status; expected 200"
    exit 1
fi

# 2d: poll suite_freshness for current_snapshot_id. Adoption is
# async; the response in 2c was served from the V1 cache (it doesn't
# wait for adoption to commit).
echo "[deb-test] phase 2: poll suite_freshness for current_snapshot_id"
adopted=0
adopted_id=""
for i in $(seq 1 30); do
    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
        2>/dev/null || echo "")
    if [[ -n "$val" ]]; then
        adopted=1
        adopted_id="$val"
        echo "[deb-test] adopted snapshot id=$val (after ${i}s)"
        break
    fi
    sleep 1
done
if [[ $adopted -eq 0 ]]; then
    echo "FAIL: no snapshot adopted within 60s"
    echo "=== suite_freshness ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
    echo "=== suite_snapshot ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT snapshot_id, canonical_host, suite_path, adopted_at FROM suite_snapshot;" || true
    echo "=== daemon log tail ==="
    tail -100 "$daemon_log" || true
    exit 1
fi

# 2e: a follow-up GET should now serve via snapshot_member resolution
# and set the X-Cache-Snapshot response header (SPEC2 §6.1 / §10.1).
echo "[deb-test] phase 2: follow-up GET should expose X-Cache-Snapshot"
hdrs_file=/tmp/inrelease.headers
status=$(curl -sS -x http://127.0.0.1:3142 \
    -D "$hdrs_file" \
    -o /tmp/inrelease.followup \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease)
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

echo "[deb-test] phase 2 (inline adoption smoke) PASS"

# ============================================================
# SPEC2 §7.6.3 phase 3: detached-mode adoption smoke
#
# Re-runs the phase-2 flow with detached fixtures (Release +
# Release.gpg, no InRelease). Validates that the freshness checker
# falls back from the inline path to detached when no InRelease
# url_path row exists, and that adoption produces a snapshot whose
# release_hash is set and inrelease_hash is nil.
#
# A clean cache directory is required: phase 2 already adopted an
# inline snapshot. After the wipe there is no InRelease url_path
# row alongside the seeded Release rows, so the inline checker
# takes the missing-InRelease-url_path branch (freshness.go
# checkLockedInline → fallback=true) and the freshness goroutine
# runs RunDetached. The companion 404 fallback branch (cached
# InRelease url_path row + 404 from upstream + nil current
# snapshot) is covered by unit tests in freshness_test.go; the
# e2e harness only needs to confirm one fallback flavor end-to-end
# because both branches converge on the same RunDetached call.
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
# would 404. Both Release and Release.gpg url_path rows are required
# for the detached fallback path; without them, the freshness checker
# skips with a "no cached Release url_path" debug log.
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

# 3b: swap to V2-detached.
echo "[deb-test] phase 3: swap nginx webroot to V2-detached"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2-detached/dists /var/www/html/
cp -r /fixture-v2-detached/pool /var/www/html/

# 3c: drive a Release GET. Cache hits its url_path row, fires the
# freshness check. detectForm sees no current_snapshot_id, defaults
# to inline. checkLockedInline tries to look up InRelease url_path,
# finds none, sees Release rows exist → returns (nil, true) /
# fallback. checkLockedDetached fetches Release (200, changed),
# fetches Release.gpg, builds an adoptionRequest with
# form=detached. The Check goroutine routes to RunDetached, which
# verifies the detached signature and commits a snapshot with
# release_hash + release_gpg_hash set.
echo "[deb-test] phase 3: GET Release via proxy (cache hit → fires freshness → detached fallback → adoption)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/Release.trigger \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/Release || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: trigger Release returned $status; expected 200"
    exit 1
fi

# 3d: poll for adoption. Same poll window as phase 2.
echo "[deb-test] phase 3: poll suite_freshness for current_snapshot_id"
adopted=0
adopted_id=""
for i in $(seq 1 30); do
    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
        2>/dev/null || echo "")
    if [[ -n "$val" ]]; then
        adopted=1
        adopted_id="$val"
        echo "[deb-test] adopted detached snapshot id=$val (after ${i}s)"
        break
    fi
    sleep 1
done
if [[ $adopted -eq 0 ]]; then
    echo "FAIL: no detached snapshot adopted within 30s"
    echo "=== suite_freshness ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT canonical_scheme, canonical_host, suite_path, current_snapshot_id, inrelease_change_seen_at FROM suite_freshness;" || true
    echo "=== suite_snapshot ==="
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT snapshot_id, suite_path, inrelease_hash, release_hash, release_gpg_hash, adopted_at FROM suite_snapshot;" || true
    echo "=== daemon log tail ==="
    tail -100 "$daemon_log" || true
    exit 1
fi

# 3e: assert the snapshot is in detached form — release_hash is set,
# release_gpg_hash is set, inrelease_hash is NULL. This is the
# difference between a successful detached adoption and a confused
# inline-mode adoption that somehow ran against detached fixtures.
echo "[deb-test] phase 3: verify snapshot $adopted_id is detached form"
form_columns=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(inrelease_hash, '<null>') || '|' || IFNULL(release_hash, '<null>') || '|' || IFNULL(release_gpg_hash, '<null>') FROM suite_snapshot WHERE snapshot_id=$adopted_id;")
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

# 3f: a follow-up GET should expose X-Cache-Snapshot just like phase 2.
echo "[deb-test] phase 3: follow-up GET should expose X-Cache-Snapshot"
hdrs_file=/tmp/release.headers
status=$(curl -sS -x http://127.0.0.1:3142 \
    -D "$hdrs_file" \
    -o /tmp/release.followup \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/Release)
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

echo "[deb-test] phase 3 (detached adoption smoke) PASS"
echo "[deb-test] OVERALL PASS"
