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

setup
first_contact
stop_daemon
say "PASS in ${SECONDS}s: ${TIMINGS[*]}"
