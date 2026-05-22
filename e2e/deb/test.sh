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

# ============================================================
# SPEC3 §14.5 phase 4: Phase 3 hot-package prefetch end-to-end.
#
# SPEC3 definition-of-done #5 requires the .deb-install harness to
# exercise at least one second-cycle adoption that successfully
# prefetches a hot deb. The unit/chaos suite already covers the
# wiring; this section proves the same wiring works from the
# packaged binary running under the packaged config schema.
#
# Flow (V1 → V2 → V1 chain so two adoptions actually fire):
#   1. Stop, wipe cache, stage V1 fixture, restart with Phase 3
#      config (hot_packages.window, adoption.hot_prefetch_budget).
#   2. Prime cache by GETting V1 InRelease (cold-cache miss). No
#      adoption fires here — there's no current_snapshot_id yet
#      and no upstream divergence to drive freshness.
#   3. Swap nginx webroot to V2. Drive a second InRelease GET. The
#      cached V1 bytes diverge from upstream V2 → freshness fires
#      → V2 adoption commits. This is the FIRST adoption
#      (prior_snapshot_id was NULL → hot_count=0).
#   4. GET the V2 .deb so url_path.last_requested_at on
#      hello-acu_1.1_amd64.deb is recorded (V1=1.0, V2=1.1 per the
#      Dockerfile's repo-build VER args) — that's the (Package,
#      Arch) signal SPEC3 §7.5.3 Stage 1 mines on the next
#      adoption.
#   5. Swap nginx webroot back to V1. Drive a third InRelease GET.
#      Cached V2 bytes diverge from upstream V1 → V1 adoption
#      fires. prior_snapshot_id = V2's id → hot pair (hello-acu,
#      amd64) is hot → Stage 2 maps it to V1's
#      hello-acu_1.0_amd64.deb → hot prefetch warms it.
#   6. Assert:
#        - adoption_hot_prefetch_complete fires for the V1 snapshot
#          with hot_count >= 1, fetched >= 1, failed=0,
#          mismatched=0;
#        - url_path row for hello-acu_1.0_amd64.deb exists with a
#          non-null blob_hash AND last_fetched_at (proves
#          CommitAdoption wrote the prefetched row in the flip
#          transaction per SPEC3 §7.5.1).
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

# 4a: cold-cache prime with V1 InRelease. The freshness check fires
# only on subsequent hits, so this MISS just establishes a url_path
# row pointing at the V1 bytes; no adoption fires yet.
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

# 4b: swap upstream to V2 so the next freshness check sees changed
# bytes (cached=V1, upstream=V2). Sleep > cooldown.
echo "[deb-test] phase 4: swap nginx webroot to V2 (drives V2 adoption)"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v2/dists /var/www/html/
cp -r /fixture-v2/pool /var/www/html/

echo "[deb-test] phase 4: GET InRelease to trigger V2 adoption (1st adoption)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.inrelease.trigger.v2 \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V2 trigger InRelease returned $status"
    exit 1
fi

echo "[deb-test] phase 4: poll for V2 adoption (1st)"
v2_id=""
for i in $(seq 1 30); do
    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
        2>/dev/null || echo "")
    if [[ -n "$val" ]]; then
        v2_id="$val"
        echo "[deb-test] phase 4 V2 adopted snapshot id=$v2_id (after ${i}s)"
        break
    fi
    sleep 1
done
if [[ -z "$v2_id" ]]; then
    echo "FAIL: phase-4 V2 adoption did not flip current_snapshot_id within 30s"
    tail -100 "$daemon_log" || true
    exit 1
fi

# 4c: hit the V2 .deb so SPEC3 §7.5.3 Stage 1 has a hot pair to
# mine on the next adoption. The (Package=hello-acu, Arch=amd64)
# tuple is recorded with last_requested_at via the Phase 1 fetch
# path (no snapshot prefetch happened in 4b — this was the first
# adoption with prior_snapshot_id=NULL, hot_count=0).
# V2 fixture is version 1.1 (V1 is 1.0); see e2e/deb/Dockerfile
# repo-build args (VER=1.1).
V2_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.1_amd64.deb"
echo "[deb-test] phase 4: GET V2 .deb to record url_path.last_requested_at"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.deb.v2 \
    -w '%{http_code}' \
    --max-time 10 \
    "http://127.0.0.1${V2_DEB_PATH}" || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V2 deb GET returned $status"
    exit 1
fi
v2_lr=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(last_requested_at, '') FROM url_path WHERE path='${V2_DEB_PATH}';" \
    2>/dev/null || echo "")
if [[ -z "$v2_lr" ]]; then
    echo "FAIL: phase-4 V2 .deb url_path.last_requested_at not recorded"
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT path, last_requested_at, request_count FROM url_path;" || true
    exit 1
fi

# 4d: swap upstream BACK to V1 so the next freshness check fires
# again (cached InRelease bytes are V2's after 4b adoption; upstream
# is V1's again). This drives the SECOND adoption — the one whose
# hot-set Stage 1 query finds the (hello-acu, amd64) hot pair from
# the V2 snapshot.
echo "[deb-test] phase 4: swap nginx webroot back to V1 (drives V1 adoption + hot prefetch)"
sleep 2
rm -rf /var/www/html/*
cp -r /fixture-v1/dists /var/www/html/
cp -r /fixture-v1/pool /var/www/html/

echo "[deb-test] phase 4: GET InRelease to trigger V1 adoption (2nd adoption + hot prefetch)"
status=$(curl -sS -x http://127.0.0.1:3142 \
    -o /tmp/p4.inrelease.trigger.v1 \
    -w '%{http_code}' \
    --max-time 10 \
    http://127.0.0.1/dists/noble/InRelease || echo "000")
if [[ "$status" != "200" ]]; then
    echo "FAIL: phase-4 V1 trigger InRelease returned $status"
    exit 1
fi

echo "[deb-test] phase 4: poll for V1 adoption (2nd)"
v1_id=""
for i in $(seq 1 30); do
    val=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT IFNULL(current_snapshot_id, '') FROM suite_freshness WHERE suite_path='/dists/noble' LIMIT 1;" \
        2>/dev/null || echo "")
    if [[ -n "$val" && "$val" != "$v2_id" ]]; then
        v1_id="$val"
        echo "[deb-test] phase 4 V1 adopted snapshot id=$v1_id (after ${i}s; prior was $v2_id)"
        break
    fi
    sleep 1
done
if [[ -z "$v1_id" ]]; then
    echo "FAIL: phase-4 V1 adoption did not flip current_snapshot_id within 30s (still v2_id=$v2_id)"
    tail -100 "$daemon_log" || true
    exit 1
fi

# 4e: assert SPEC3 §10.2 adoption_hot_prefetch_complete event fired
# for the V1 snapshot with hot_count >= 1, fetched >= 1, failed=0,
# mismatched=0. This is the "hot pair was warmed" oracle.
echo "[deb-test] phase 4: assert adoption_hot_prefetch_complete fired for snapshot $v1_id"
hot_line=$(grep "msg=adoption_hot_prefetch_complete" "$daemon_log" \
    | grep "snapshot_id=$v1_id" | tail -1)
if [[ -z "$hot_line" ]]; then
    echo "FAIL: no adoption_hot_prefetch_complete log line for snapshot $v1_id"
    echo "=== relevant log tail ==="
    grep -E "hot_prefetch|adoption_" "$daemon_log" | tail -30 || true
    exit 1
fi
echo "[deb-test] phase 4 hot-prefetch line: $hot_line"
# Extract numeric fields. slog text format emits `key=value` tokens
# separated by spaces. Anchor on the bare key= so a longer suffix
# (e.g. `unattempted=`) doesn't match `attempted=`.
hot_count=$(printf '%s\n' "$hot_line" | grep -oE '(^| )hot_count=[0-9]+' | tr -dc '0-9')
fetched=$(printf '%s\n' "$hot_line" | grep -oE '(^| )fetched=[0-9]+' | tr -dc '0-9')
failed=$(printf '%s\n' "$hot_line" | grep -oE '(^| )failed=[0-9]+' | tr -dc '0-9')
mismatched=$(printf '%s\n' "$hot_line" | grep -oE '(^| )mismatched=[0-9]+' | tr -dc '0-9')
if [[ "${hot_count:-0}" -lt 1 ]]; then
    echo "FAIL: phase-4 hot_count=$hot_count, want >= 1 (Stage 1 should mine the V2 hot pair)"
    exit 1
fi
if [[ "${fetched:-0}" -lt 1 ]]; then
    echo "FAIL: phase-4 fetched=$fetched, want >= 1 (the V1 path should warm)"
    exit 1
fi
if [[ "${failed:-0}" -ne 0 ]]; then
    echo "FAIL: phase-4 failed=$failed, want 0"
    exit 1
fi
if [[ "${mismatched:-0}" -ne 0 ]]; then
    echo "FAIL: phase-4 mismatched=$mismatched, want 0"
    exit 1
fi

# 4f: assert the V1 .deb's url_path row exists with a non-null
# blob_hash and last_fetched_at — proof CommitAdoption inserted the
# prefetched row. SPEC3 §7.5.1: the row is inserted in the same
# transaction as the snapshot flip.
V1_DEB_PATH="/pool/main/h/hello-acu/hello-acu_1.0_amd64.deb"
v1_row=$(sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
    "SELECT IFNULL(blob_hash, '') || '|' || IFNULL(last_fetched_at, '') FROM url_path WHERE path='${V1_DEB_PATH}';" \
    2>/dev/null || echo "")
if [[ -z "$v1_row" || "$v1_row" == "|" ]]; then
    echo "FAIL: phase-4 V1 .deb url_path row missing or has null blob_hash"
    sqlite3 -readonly /var/cache/apt-cacher-ultra/cache.db \
        "SELECT path, blob_hash, last_fetched_at, last_requested_at FROM url_path;" || true
    exit 1
fi
v1_blob="${v1_row%%|*}"
v1_lf="${v1_row#*|}"
if [[ -z "$v1_blob" ]]; then
    echo "FAIL: phase-4 V1 .deb url_path.blob_hash is null"
    exit 1
fi
if [[ -z "$v1_lf" ]]; then
    echo "FAIL: phase-4 V1 .deb url_path.last_fetched_at is null (prefetch should set this)"
    exit 1
fi
echo "[deb-test] phase 4 V1 .deb url_path: blob_hash=${v1_blob:0:12}.. last_fetched_at=$v1_lf"

echo "[deb-test] phase 4 (Phase 3 hot-prefetch) PASS"
echo "[deb-test] OVERALL PASS"
