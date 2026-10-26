#!/bin/bash
# e2e/flat in-container test (spec §7, "Real-apt end-to-end"): real apt
# clients against flat and dists/ fixture repositories through
# apt-cacher-ultra, with nginx as the upstream, in one container.
# e2e/flat/run.sh builds the image per client image and runs this as its
# CMD. Scenarios run in order and share one cache; e2e/flat/README.md says
# what each one proves.
#
# E2E_IMAGE names the client image in messages. E2E_UNSIGNED_WAIT=1 runs
# the unsigned first-adoption scenario, which waits out the unsigned
# confirmation: 5 to 15 minutes after the first contact (run.sh sets it
# for the oldest and newest images only).
set -euo pipefail
# shellcheck source=e2e/flat/lib.sh
. /usr/local/lib/acu-e2e/lib.sh

# A command that fails outside fail (nginx not binding, dpkg, a publish's
# mv) exits through set -e: still name the scenario, the command and logs.
on_exit() {
    local rc=$? cmd=$BASH_COMMAND
    if ((rc != 0 && FAILED == 0)); then
        say "unexpected exit $rc in scenario $SCENARIO (command: $cmd)"
        show_logs
    fi
}
trap on_exit EXIT
# Ctrl-C on docker run, or docker stop: end the run at the next command
# boundary (bash as PID 1 would ignore these signals without a handler).
trap 'fail "interrupted by a signal"' INT TERM

# The repositories, as apt clients name them (spec §3's shapes; see
# build-fixtures.sh).
SRC_K8S="deb [signed-by=$KEY] http://signed.test/core:/stable:/v1.34/deb/ /"
SRC_K8S_DOT="deb [signed-by=$KEY] http://signed.test/core:/stable:/v1.34/deb ./"
SRC_CUDA="deb [signed-by=$KEY] http://signed.test/cuda/ubuntu2204/x86_64/ /"
SRC_NVIDIA="deb [signed-by=$KEY] http://signed.test/libnvidia-container/stable/deb/amd64 /"
SRC_JENKINS="deb [signed-by=$KEY] http://signed.test/debian-stable binary/"
SRC_SUBLIME="deb [signed-by=$KEY] http://signed.test/ apt/stable/"
SRC_JITSI="deb [signed-by=$KEY] http://signed.test/jitsi stable/"
SRC_JITSI_SRC="deb-src [signed-by=$KEY] http://signed.test/jitsi stable/"
SRC_DISTS="deb [signed-by=$KEY] http://signed.test/debian acu main"
SRC_UNSIGNED404="deb [trusted=yes] http://plain.test/unsigned404/ /"
SRC_UNSIGNED403="deb [trusted=yes] http://plain.test/unsigned403/ /"
SRC_FURY="deb [trusted=yes] http://plain.test/fury/ /"
SRC_GORELEASER="deb [trusted=yes] http://plain.test/goreleaser/ /"

# The suites the daemon tracks for them.
K8S=/core:/stable:/v1.34/deb
CUDA=/cuda/ubuntu2204/x86_64
NVIDIA=/libnvidia-container/stable/deb/amd64
JENKINS=/debian-stable/binary
SUBLIME=/apt/stable
JITSI=/jitsi/stable
DISTS=/debian/dists/acu

write_config() {
    mkdir -p /etc/acu-e2e/keys /var/cache/acu-e2e
    cp /fixtures/keys/acu-e2e.gpg "$KEY"
    # Short timers (ruling R-H2-2): the cooldown, the refresh interval
    # (tick 7.5 s) and the quietest flip barrier; the unsigned confirmation
    # stays max(cooldown, 5m) = 5 minutes, as spec C5 fixes it.
    cat > "$CONFIG" << EOF
[cache]
dir = "/var/cache/acu-e2e"
listen = "127.0.0.1:3142"

[upstream]
allowed_host_regex = ['^(signed|plain)\.test\$']
deny_target_ranges = []

[freshness]
cooldown = "${COOLDOWN}s"
periodic_refresh = "${PERIODIC}s"

[adoption]
enabled = true
require_signature = true
flip_quiet_period = "1s"
keyring_dirs = ["/etc/acu-e2e/keys"]

[integrity]
refuse_unvouched_debs = true

[[trusted_signer]]
match_canonical_host = '^signed\.test\$'
fingerprints = ["$(cat /fixtures/keys/fingerprint)"]

[[mirror]]
prefix = "/m-signed"
upstream = "http://signed.test/"

[log]
level = "info"
format = "text"
EOF
}

setup() {
    say "apt $(apt-get --version | head -n 1 | cut -d' ' -f2); fixtures key $(cat /fixtures/keys/fingerprint)"
    echo '127.0.0.1 signed.test plain.test' >> /etc/hosts
    : > "$DAEMON_LOG"
    # The waits are not vacuous: a condition that never holds times out.
    ! poll_quietly 0 false || fail "poll_quietly returned success for a condition that never held"
    write_config
    start_nginx
    start_daemon
}

# Every signed suite adopts in its upstream's form; the unsigned ones wait
# for confirmation; the Release-less one refreshes without a Release.
first_contact() {
    scenario first-contact "every repository through one client: adoption, installs, request lines, strict mode"
    client main "$PROXY" "$SRC_K8S" "$SRC_CUDA" "$SRC_NVIDIA" "$SRC_JENKINS" "$SRC_SUBLIME" \
        "$SRC_JITSI" "$SRC_JITSI_SRC" "$SRC_DISTS" "$SRC_UNSIGNED404" "$SRC_UNSIGNED403" "$SRC_FURY" \
        "$SRC_GORELEASER"
    # The error checks are not vacuous on this apt: apt_update must fail an
    # update of a source the cache refuses (a host outside its allowlist),
    # and one of a signed source checked against a key that did not sign it
    # (apt then prints no "Failed to fetch": only the signature words count).
    client control "$PROXY" "deb [trusted=yes] http://nowhere.test/ /"
    if (apt_update control) > /clients/control/apt_update.out 2>&1; then
        sed 's/^/    | /' /clients/control/update.*.log
        fail "apt_update passed a failed update: the apt error grep is vacuous (above)"
    fi
    client control-sig "$PROXY" "deb [signed-by=/fixtures/keys/other.gpg] http://signed.test/debian acu main"
    if (apt_update control-sig) > /clients/control-sig/apt_update.out 2>&1; then
        sed 's/^/    | /' /clients/control-sig/update.*.log
        fail "apt_update passed a signature failure: the signed-source error list is vacuous (above)"
    fi
    # The same at the host root, which apt names with no slash after the
    # host ("http://signed.test apt/stable/"): the host filter must see it.
    client control-sig-root "$PROXY" "deb [signed-by=/fixtures/keys/other.gpg] http://signed.test/ apt/stable/"
    if (apt_update control-sig-root) > /clients/control-sig-root/apt_update.out 2>&1; then
        sed 's/^/    | /' /clients/control-sig-root/update.*.log
        fail "apt_update passed a host-root signature failure: the signed-host filter misses it (above)"
    fi

    UNSIGNED_FIRST_CONTACT=$SECONDS
    apt_update main -o Debug::Acquire::http=true
    # apt_sent is not vacuous: a line apt never sent does not match.
    if apt_sent_line 'http://signed\.test/jitsi/stable/no-such-file'; then
        fail "apt_sent matched a request line apt never sent"
    fi
    # by-hash uses the strongest hash the Release declares (spec §3).
    apt_sent 'http://signed\.test/jitsi/stable/by-hash/SHA512/[0-9a-f]{128}'
    # A binary Release.gpg is never fetched: apt uses the InRelease
    # (deviation 1).
    if grep -q '^GET http://signed\.test/jitsi/stable/Release\.gpg ' "$APT_LOG"; then
        fail "apt fetched jitsi's Release.gpg; the fixture's InRelease should be used"
    fi

    # Generous bounds: the update's own checks adopt, refresh or refuse
    # every suite within seconds. 30 s is PERIODIC, over three TICKs; 90 s,
    # three PERIODICs, leaves room for seven adoptions two at a time.
    local s
    for s in "$K8S inline" "$CUDA inline" "$NVIDIA inline" "$JENKINS detached" \
        "$SUBLIME inline" "$JITSI inline" "$DISTS inline"; do
        poll 90 "signed.test${s% *} to adopt (${s#* })" adopted_as signed.test "${s% *}" "${s#* }"
    done
    for s in /fury /goreleaser; do
        poll 30 "the Release-less $s to refresh without a Release" log_has "$SCENARIO_MARK" \
            "$(event freshness_check)" "$(suite_kv "$s")" "$(kv form anchorless)" 'result=anchorless_(unchanged|refreshed)'
    done
    for s in /unsigned404 /unsigned403; do
        poll 30 "plain.test$s to wait for its confirmation" log_has "$SCENARIO_MARK" \
            "$(event adoption_refused)" "$(suite_kv "$s")" "$(kv reason unsigned_unconfirmed)"
    done

    # Served from the snapshots now: still clean.
    apt_update main
    apt_install main acu-k8s=1.0 acu-cuda=1.0 acu-epoch=1:1.0 acu-nvidia=1.0 acu-jenkins=1.0 \
        acu-jenkins-direct=1.0 acu-sublime=1.0 acu-jitsi=1.0 acu-dists=1.0 \
        acu-unsigned404=1.0 acu-unsigned403=1.0 acu-fury=1.0 acu-goreleaser=1.0 \
        -- -o Debug::Acquire::http=true
    # The literal ./ of a dpkg-scanpackages Filename and the epoch's %3a,
    # as apt sends them and as the cache sends them upstream (spec C1).
    apt_sent "$(re "http://signed.test$CUDA/./acu-epoch_1%253a1.0_amd64.deb")"
    upstream_got signed.test "$(re "$CUDA/./acu-epoch_1%253a1.0_amd64.deb")"
    apt_sent "$(re "http://signed.test$NVIDIA/./acu-nvidia_1.0_amd64.deb")"
    upstream_got signed.test "$(re "$NVIDIA/./acu-nvidia_1.0_amd64.deb")"
    # Every signed .deb was vouched by its snapshot (spec C6), whatever its
    # Filename's shape.
    local deb
    for deb in "$K8S/amd64/acu-k8s_1.0_amd64.deb" "$CUDA/./acu-cuda_1.0_amd64.deb" \
        "$CUDA/./acu-epoch_1%3a1.0_amd64.deb" "$NVIDIA/./acu-nvidia_1.0_amd64.deb" \
        /debian-stable/binary/acu-jenkins_1.0_all.deb /debian-stable/direct/acu-jenkins-direct_1.0_all.deb \
        /files/acu-sublime_1.0_amd64.deb /jitsi/stable/acu-jitsi_1.0_all.deb \
        /debian/pool/main/a/acu-dists/acu-dists_1.0_amd64.deb; do
        log_has "$SCENARIO_MARK" "$(event request)" "$(kv path "$deb")" "$(kv validated_hash true)" ||
            fail "$deb was not served vouched (validated_hash=true)"
    done

    # Jitsi's Contents-amd64 through apt-file, and its Sources. (Outputs go
    # to files: a grep -q at the end of a pipe can fail it under pipefail.)
    capt main apt-file search /usr/share/acu-e2e/acu-jitsi > /clients/main/apt-file.out || true
    grep -q '^acu-jitsi: /usr/share/acu-e2e/acu-jitsi$' /clients/main/apt-file.out ||
        fail "apt-file did not find acu-jitsi's file in Contents-amd64"
    capt main apt-cache showsrc acu-jitsi > /clients/main/showsrc.out || true
    grep -q '^Package: acu-jitsi$' /clients/main/showsrc.out ||
        fail "apt-cache showsrc found no acu-jitsi source package"

    # Strict mode is on for signed.test: an undeclared .deb is refused...
    local st
    st=$("${CURL[@]}" -sS -o /dev/null -w '%{http_code}' -x "$PROXY" \
        "http://signed.test$K8S/amd64/acu-rogue_1.0_amd64.deb" || true)
    [[ $st == 502 ]] || fail "an undeclared .deb on signed.test answered $st, want 502 (strict mode)"
    log_has "$SCENARIO_MARK" "$(event unvouched_deb_refused)" "$(kv path "$K8S/amd64/acu-rogue_1.0_amd64.deb")" ||
        fail "no unvouched_deb_refused line for the undeclared .deb"
    # ...and pkgs.k8s.io's shape answers 403 for a file it lacks, which
    # the cache passes on. (Not Release: an inline snapshot answers its
    # suite's Release 404 itself, spec §10 item 9.)
    st=$("${CURL[@]}" -sS -o /dev/null -w '%{http_code}' -x "$PROXY" "http://signed.test$K8S/README" || true)
    [[ $st == 403 ]] || fail "a missing file of the k8s shape answered $st through the cache, want 403"
    passed
}

# PR #8's end-to-end table (plan overview, "Cross-phase interfaces", H): one
# signed flat repository as `deb <uri>/ /` on one client and `deb <uri> ./`
# on another; both report 2.0 within one cooldown of the v2 publish. The
# upstream-stopped half runs in upstream_down.
pr8_table() {
    scenario pr8-table "one flat repository in both source forms; both see 2.0 after the v2 publish"
    client k8s-slash "$PROXY" "$SRC_K8S"
    client k8s-dot "$PROXY" "$SRC_K8S_DOT"
    apt_update k8s-slash
    apt_update k8s-dot -o Debug::Acquire::http=true
    apt_sent "$(re "http://signed.test$K8S/./InRelease")"
    apt_sent "$(re "http://signed.test$K8S/./Packages.gz")"
    poll 60 "the ./ twin suite to adopt" adopted_as signed.test "$K8S/." inline
    apt_install k8s-slash acu-k8s=1.0
    apt_install k8s-dot acu-k8s=1.0

    local start
    start=$(date +%s)
    apt_update k8s-slash
    apt_update k8s-dot
    baseline "$start" signed.test "$K8S" "$K8S/."
    publish k8s v2
    until_candidates "$REACH" k8s-slash k8s-dot -- acu-k8s=2.0
    by_client
    say "both source forms report acu-k8s 2.0 ${POLL_TOOK}s after the publish (cooldown ${COOLDOWN}s)"
    apt_install k8s-slash acu-k8s=2.0
    apt_install k8s-dot acu-k8s=2.0
    passed
}

# apt ignores an older Date; so does the cache (spec C5, upstream_older).
# apt alone would keep the existing clients at 2.0, so the check that
# counts is the cache's: the snapshots stay and a fresh client gets 2.0.
older_date() {
    scenario older-date "a publish with an older Date is refused; clients keep 2.0"
    local s before
    before=$(snapshot_ids)
    publish k8s v1
    for s in "$K8S" "$K8S/."; do
        poll "$REACH" "signed.test$s to refuse the older Date" refused_older "$s"
    done
    apt_update k8s-slash
    apt_update k8s-dot
    candidates_are k8s-slash acu-k8s=2.0 || fail "k8s-slash went back to $(candidate k8s-slash acu-k8s)"
    candidates_are k8s-dot acu-k8s=2.0 || fail "k8s-dot went back to $(candidate k8s-dot acu-k8s)"
    [[ $(snapshot_ids) == "$before" ]] || fail "the older-Date publish changed a snapshot: $before -> $(snapshot_ids)"
    client k8s-fresh "$PROXY" "$SRC_K8S"
    apt_update k8s-fresh
    candidates_are k8s-fresh acu-k8s=2.0 || fail "a fresh client sees acu-k8s $(candidate k8s-fresh acu-k8s), want 2.0"
    publish k8s v2
    passed
}

# refused_older SUITE: the suite refused an older Date in this scenario;
# a client update triggers its check.
refused_older() {
    apt_update k8s-slash
    apt_update k8s-dot
    log_has "$SCENARIO_MARK" "$(event adoption_refused)" "$(suite_kv "$1")" "$(kv reason upstream_older)"
}

# The snapshots survive a restart: the same snapshot ids, a clean update
# with the same candidates, and anchors served from the snapshots.
restart() {
    scenario restart "state survives a restart"
    local before after cands_before cands_after stopped_at started
    apt_update main
    before=$(snapshot_ids)
    cands_before=$(main_candidates)
    stopped_at=$(date +%s)
    stop_daemon
    start_daemon
    started=$("${CURL[@]}" -fsS "$ADMIN/?format=json" | jq -r '.process.started_unixtime // empty' || true)
    [[ $started =~ ^[0-9]+$ ]] || fail "no process.started_unixtime in the admin status"
    ((started >= stopped_at)) ||
        fail "the daemon was not restarted: process.started_unixtime $started is before the stop at $stopped_at"
    after=$(snapshot_ids)
    [[ $before == "$after" ]] || fail "snapshots changed across the restart: $before -> $after"
    apt_update main
    cands_after=$(main_candidates)
    [[ $cands_before == "$cands_after" ]] || fail "candidates changed across the restart: $cands_before -> $cands_after"
    # An anchor through the proxy: a flip may hold it up to flipgate.HoldMax
    # (20 s), so its limit is 35 s, not CURL's 10.
    curl --max-time 35 -fsS -D /clients/main/k8s-InRelease.headers -o /dev/null -x "$PROXY" "http://signed.test$K8S/InRelease" ||
        fail "the k8s InRelease through the proxy failed after the restart"
    grep -qi '^x-cache-snapshot:' /clients/main/k8s-InRelease.headers ||
        fail "the k8s InRelease is not served from its snapshot after the restart"
    passed
}

# snapshot_ids: every signed suite's current snapshot id, one line.
snapshot_ids() {
    local s
    for s in "$K8S" "$K8S/." "$CUDA" "$NVIDIA" "$JENKINS" "$SUBLIME" "$JITSI" "$DISTS"; do
        printf '%s=%s ' "$s" "$(suite signed.test "$s" current_snapshot_id)"
    done
}

# main_candidates: the main client's candidate of every fixture package.
main_candidates() {
    local p
    for p in acu-k8s acu-cuda acu-epoch acu-nvidia acu-jenkins acu-jenkins-direct acu-sublime \
        acu-jitsi acu-dists acu-unsigned404 acu-unsigned403 acu-fury acu-goreleaser; do
        printf '%s=%s ' "$p" "$(candidate main "$p")"
    done
}

# PR #8's table, second half: with the upstream stopped, two fresh clients
# (one per source form) install 2.0 from the snapshots. Meanwhile the
# checks their updates trigger fail, and the status page shows the failing
# checks' backoff (backoff_shown).
#
# AIDEV-NOTE: upstream_down stays the last scenario before whole_run.
# Every suite checked while nginx is down (by a client's update or by the
# scheduler's tick) fails, and is still in its backoff after start_nginx:
# 2 minutes after one failure, doubling (freshness.CheckBackoff). So a
# scenario after it that waits for the scheduler's check (a bound of
# periodic_refresh + a tick + 10 s, as the anchorless carry-forward's)
# would time out. Design note 7 records the same cost for the unsigned
# suite.
upstream_down() {
    scenario upstream-down "upstream stopped: fresh clients of both forms install 2.0 from the snapshots"
    stop_nginx
    local upstream_lines
    upstream_lines=$(wc -l < "$NGINX_LOG")
    client fresh-slash "$PROXY" "$SRC_K8S"
    client fresh-dot "$PROXY" "$SRC_K8S_DOT"
    apt_update fresh-slash
    apt_update fresh-dot
    apt_install fresh-slash acu-k8s=2.0
    apt_install fresh-dot acu-k8s=2.0
    # Nothing came from the upstream: these clients were served from the
    # snapshots alone.
    (($(wc -l < "$NGINX_LOG") == upstream_lines)) || fail "the upstream answered during upstream-down"
    # The overview's H1 carry (parked item 12): main.go wires
    # admin.Config.CheckBackoff, without which a failing suite's row shows
    # only "checks failing ×k". A client-triggered check fails within a
    # cooldown of an update; the scheduler's own check of the suite comes
    # within periodic_refresh and a tick of its last success at the latest.
    poll $((PERIODIC + TICK + 10)) "the status page to show a failing check's backoff" backoff_shown
    say "the status page shows $K8S's failing checks with their backoff ${POLL_TOOK}s into the poll"
    start_nginx
    passed
}

# backoff_shown: a client update, whose check of $K8S fails while nginx is
# stopped (a refused connection ends a check unknown: spec C4's failed
# check), then $K8S's check_fail_count is positive and its row on the
# status page (the HTML at /, one <tr> per suite) shows "backoff ends in".
# internal/admin's annotateSuiteEntries writes that text only for a suite
# with a failing count, a last_check_at and a backoff the scheduler keeps
# (unscheduled is false): an adopted suite qualifies unless an
# anchor_absent or unsigned_disabled record paces it, and $K8S's only
# possible record is older-date's upstream_older.
backoff_shown() {
    local n
    apt_update fresh-slash
    n=$(suite signed.test "$K8S" check_fail_count)
    ((${n:-0} > 0)) || return 1
    "${CURL[@]}" -fsS -o /clients/fresh-slash/status.html "$ADMIN/" || return 1
    [[ -n $(awk -v RS='</tr>' -v p="$K8S" '(index($0, ">" p "<") || index($0, ">" p " ")) && index($0, "backoff ends in")' \
        /clients/fresh-slash/status.html) ]]
}

setup
first_contact
pr8_table
older_date
restart
upstream_down
stop_daemon
say "PASS in ${SECONDS}s: ${TIMINGS[*]}"
