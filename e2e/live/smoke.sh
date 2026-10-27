#!/bin/bash
# e2e/live in-container smoke (the flat-repositories overview, "PR #8
# parity", H): real apt clients against live flat repositories through
# apt-cacher-ultra, with adoption enabled and strict mode on. For each
# repository: its suites adopt (adoption_success, in the form its upstream
# publishes), apt-get update is clean under e2e/flat's error grep, one
# package installs with its .deb vouched (validated_hash=true), and no
# .deb is refused but the controls'. e2e/live/run.sh builds the image and
# runs this as its CMD; e2e/live/README.md says what each step proves.
#
# AIDEV-NOTE: a repository that misbehaves through the cache is a finding
# to report, never something to work around here (e2e/flat's ruling
# R-H2-4). The values below change only when a vendor changes its
# repository (a key, a package, a minor version): README, "Maintenance".
set -euo pipefail
# shellcheck source=e2e/flat/lib.sh
. /usr/local/lib/acu-e2e/lib.sh

# lib.sh's settings that differ here: no nginx, and the five live hosts
# as apt names them through the http://HTTPS/// convention (a host-root
# source, Sublime's, has a space after the host: lib.sh's AIDEV-NOTE).
# live_update checks that every source line of every client's update
# matches it, so each host's alternative is proven on every run.
NGINX_LOG=/dev/null
KEYS=/etc/acu-e2e/keys
APT_SIGNED_HOSTS='HTTPS///(pkgs\.k8s\.io|developer\.download\.nvidia\.com|nvidia\.github\.io|pkg\.jenkins\.io|download\.sublimetext\.com)[/ ]'
# The adoptions' bound: the planning run took 221 s, most of it the wait
# for the scheduler's tick (periodic_refresh / 4, 225 s by default) that
# admits the first adoptions a client's check found no slot for.
ADOPT_LIMIT=900

say() {
    printf '[e2e-live %4ds] %s\n' "$SECONDS" "$*"
}

on_exit() {
    local rc=$?
    if ((rc != 0 && FAILED == 0)); then
        say "unexpected exit $rc in step $SCENARIO (command: $BASH_COMMAND)"
        show_logs
    fi
}
trap on_exit EXIT
trap 'fail "interrupted by a signal"' INT TERM

# The repositories. The k8s minors are two pkgs.k8s.io publishes, K8S_A
# the older (installs takes the newest kubectl, which must be K8S_B's, on
# one client and K8S_A's on the other); the keys are the ones each
# vendor's install instructions name (README, "Maintenance").
K8S_A=v1.36
K8S_B=v1.37
K8S_KEY=https://pkgs.k8s.io/core:/stable:/$K8S_B/deb/Release.key
CUDA_KEY=https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/3bf863cc.pub
NVIDIA_KEY=https://nvidia.github.io/libnvidia-container/gpgkey
JENKINS_KEY=https://pkg.jenkins.io/debian-stable/jenkins.io-2026.key
SUBLIME_KEY=https://download.sublimetext.com/sublimehq-pub.gpg

# src KEY REST: a sources line for the cache's http://HTTPS/// convention.
src() { printf 'deb [signed-by=%s/%s.asc] http://HTTPS///%s' "$KEYS" "$1" "$2"; }

# suite_expectations: one line per suite the cache tracks: host, suite
# path, the form it adopts in.
suite_expectations() {
    cat << EOF
pkgs.k8s.io /core:/stable:/$K8S_A/deb inline
pkgs.k8s.io /core:/stable:/$K8S_A/deb/. inline
pkgs.k8s.io /core:/stable:/$K8S_B/deb inline
pkgs.k8s.io /core:/stable:/$K8S_B/deb/. inline
developer.download.nvidia.com /compute/cuda/repos/debian12/x86_64 inline
nvidia.github.io /libnvidia-container/stable/deb/amd64 inline
pkg.jenkins.io /debian-stable/binary detached
download.sublimetext.com /apt/stable inline
EOF
}

write_config() {
    mkdir -p /var/cache/acu-e2e
    # The shipped defaults (packaging/config/config.toml.default), but
    # adoption on, strict mode on, the vendors' keys, and no MITM (the
    # clients use http://HTTPS///).
    cat > "$CONFIG" << EOF
[cache]
dir = "/var/cache/acu-e2e"
listen = "127.0.0.1:3142"

[adoption]
enabled = true
keyring_dirs = ["$KEYS"]

[integrity]
refuse_unvouched_debs = true

[tls_mitm]
enabled = false

[log]
level = "info"
format = "text"
EOF
}

fetch_keys() {
    mkdir -p "$KEYS"
    local k name url
    for k in "k8s $K8S_KEY" "cuda $CUDA_KEY" "nvidia $NVIDIA_KEY" \
        "jenkins $JENKINS_KEY" "sublime $SUBLIME_KEY"; do
        read -r name url <<< "$k"
        # Directly from the vendor, not through the cache: the keys are the
        # smoke's trust anchors, for the clients and the daemon alike.
        "${CURL[@]}" -fsSL --retry 3 -o "$KEYS/$name.asc" "$url" ||
            fail "could not fetch the $name signing key from $url"
        grep -q 'BEGIN PGP PUBLIC KEY BLOCK' "$KEYS/$name.asc" ||
            fail "the $name signing key from $url is not an armored key"
    done
}

setup() {
    scenario setup "keys, config, the daemon, the clients"
    say "apt $(apt-get --version | head -n 1 | cut -d' ' -f2); started $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    : > "$DAEMON_LOG"
    ! poll_quietly 0 false || fail "poll_quietly returned success for a condition that never held"
    fetch_keys
    write_config
    # shellcheck disable=SC2119 # no NAME=VALUE environment for this daemon
    start_daemon
    client k8s-slash "$PROXY" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_A/deb/ /")" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_B/deb/ /")"
    client k8s-dot "$PROXY" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_A/deb ./")" \
        "$(src k8s "pkgs.k8s.io/core:/stable:/$K8S_B/deb ./")"
    client cuda "$PROXY" "$(src cuda "developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/ /")"
    client nvidia "$PROXY" "$(src nvidia "nvidia.github.io/libnvidia-container/stable/deb/amd64/ /")"
    client jenkins "$PROXY" "$(src jenkins "pkg.jenkins.io/debian-stable binary/")"
    client sublime "$PROXY" "$(src sublime "download.sublimetext.com/ apt/stable/")"
    # The grep's control: a minor that does not exist answers 403 upstream
    # (as pkgs.k8s.io answers any missing file), and apt must print an
    # error that apt_errors catches, or every clean update below proves
    # nothing.
    client control "$PROXY" "$(src k8s "pkgs.k8s.io/core:/stable:/v1.0/deb/ /")"
    # The signed-host filter's host-root control: Sublime's shape (apt
    # names it with a space after the host) at a suite that does not
    # exist, answered 404, which prints only an "E:" line.
    client control-root "$PROXY" "$(src sublime "download.sublimetext.com/ apt/acu-no-such/")"
    passed
}

CLIENTS=(k8s-slash k8s-dot cuda nvidia jenkins sublime)

# live_update NAME: apt_update on client NAME, then every source line apt
# printed (Hit, Get, Ign, Err) must match APT_SIGNED_HOSTS, and there must
# be one: the two controls prove the filter's two forms on two hosts, this
# proves each host's alternative, which a typo would otherwise break
# unseen (a signature error on that host would pass the error grep).
live_update() {
    local lines missed hosts
    apt_update "$1"
    lines=$(grep -E -- '^(Hit|Get|Ign|Err):[0-9]+ http://HTTPS///' "$APT_LOG" || true)
    [[ -n $lines ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "client $1's update printed no Hit, Get, Ign or Err line for an http://HTTPS/// source (above)"
    }
    missed=$(grep -vE -- "$APT_SIGNED_HOSTS" <<< "$lines" || true)
    [[ -z $missed ]] || {
        printf '%s\n' "$missed" | sed 's/^/    | /'
        hosts=$(grep -oE 'HTTPS///[^/ ]+' <<< "$missed" | sed 's|^HTTPS///||' | sort -u | paste -sd ' ' - || true)
        fail "APT_SIGNED_HOSTS misses client $1's source lines for $hosts (above): a signature error there would pass unseen"
    }
}

first_contact() {
    scenario first-contact "every client's first apt-get update, clean"
    local c
    for c in "${CLIENTS[@]}"; do
        live_update "$c"
        say "$c: first update clean"
    done
    # The control rests on pkgs.k8s.io answering 403 for a minor it does
    # not publish (the README's "Maintenance"; the plan's pre-flight probes
    # v1.0): apt prints "Failed to fetch", which APT_ERRORS catches.
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/control/update.$APT_RUNS.log
    capt control apt-get update > "$APT_LOG" 2>&1 || true
    [[ -n $(apt_errors "$APT_LOG") ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "the control's update (minor v1.0, which pkgs.k8s.io answers with 403) printed no error apt_errors catches"
    }
    # A signature error (NO_PUBKEY, a bad signature) prints only "GPG
    # error" and "is not signed" lines, which only APT_SIGNED_HOSTS sees:
    # so the control's error lines (the 403's "is not signed"; an "E:"
    # line, were it a 404) must match this smoke's APT_SIGNED_HOSTS too.
    [[ -n $(grep -E -- "$APT_SIGNED_HOSTS" "$APT_LOG" | grep -E -- "$APT_SIGNED_ERRORS" || true) ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "APT_SIGNED_HOSTS missed the control's error lines (above; minor v1.0, which pkgs.k8s.io answers with 403): a signature error on these hosts would pass unseen"
    }
    say "control: the error grep catches the missing minor's error"
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/control-root/update.$APT_RUNS.log
    capt control-root apt-get update > "$APT_LOG" 2>&1 || true
    [[ -n $(grep -E -- "$APT_SIGNED_HOSTS" "$APT_LOG" | grep -E -- "$APT_SIGNED_ERRORS" || true) ]] || {
        sed 's/^/    | /' "$APT_LOG"
        fail "APT_SIGNED_HOSTS missed the host-root control's error (above): an error on Sublime's source would pass unseen"
    }
    say "control-root: the signed-host filter catches a host-root source's error"
    passed
}

# all_adopted: every suite serves a snapshot of its form and logged
# adoption_success. One status fetch per poll, not two per suite (each
# fetch logs an admin_request line).
all_adopted() {
    local json host path form
    json=$("${CURL[@]}" -fsS "$ADMIN/?format=json") || return 1
    while read -r host path form; do
        jq -e --arg h "$host" --arg p "$path" --arg f "$form" \
            '.suites[] | select(.host == $h and .suite_path == $p and .current_snapshot_id != null and .anchor_form == $f)' \
            <<< "$json" > /dev/null || return 1
        log_has 0 "$(event adoption_success)" "$(kv canonical_host "$host")" "$(suite_kv "$path")" ||
            return 1
    done < <(suite_expectations)
}

adoptions() {
    scenario adoptions "every suite adopts, in its upstream's form"
    poll_quietly "$ADOPT_LIMIT" all_adopted || {
        say "the suites' status entries:"
        # A diagnostic: a daemon that does not answer must not end the run
        # (set -e) before fail names the failure.
        "${CURL[@]}" -fsS "$ADMIN/?format=json" | jq -c '.suites[] |
            {host, suite_path, anchor_form, current_snapshot_id, adoption_fail_reason, check_fail_count}' || true
        fail "not every suite adopted in its form within ${ADOPT_LIMIT}s"
    }
    say "all $(suite_expectations | wc -l) suites adopted, ${POLL_TOOK}s after first contact"
    local host path form id
    while read -r host path form; do
        id=$(suite "$host" "$path" current_snapshot_id)
        say "  $host$path: $form, snapshot $id, $(log_lines 0 "$(event adoption_success)" \
            "$(suite_kv "$path")" | tail -n 1 | grep -oE 'package_hash_count=[0-9]+')"
    done < <(suite_expectations)
    passed
}

updates() {
    scenario updates "every client's apt-get update from the snapshots, clean"
    local c
    for c in "${CLIENTS[@]}"; do
        live_update "$c"
        say "$c: update clean"
    done
    passed
}

# strict_controls: on each host, a .deb no Packages declares is refused
# (502, unvouched_deb_refused) before any upstream request: strict mode is
# on there, so the installs below are vouched under it and whole_run's
# zero-refusals check is not vacuous.
STRICT_CONTROLS=(
    "pkgs.k8s.io/core:/stable:/$K8S_B/deb/amd64/acu-live-control_1.0_amd64.deb"
    developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/acu-live-control_1.0_amd64.deb
    nvidia.github.io/libnvidia-container/stable/deb/amd64/acu-live-control_1.0_amd64.deb
    pkg.jenkins.io/debian-stable/binary/acu-live-control_1.0_all.deb
    download.sublimetext.com/files/acu-live-control_1.0_amd64.deb
)

# show_coverage HOST: HOST's package_coverage_incomplete lines (each names
# the suite, the snapshot and the reason) from the whole daemon log, or
# that there are none. Coverage is logged when a suite adopts, minutes
# before the strict step, so the log tail a failure prints no longer holds
# it. The README's "A failure: the cache, or the vendor?" says which
# reasons are the vendor's.
show_coverage() {
    local lines
    lines=$(log_lines 0 "$(event package_coverage_incomplete)" "$(kv canonical_host "$1")")
    if [[ -n $lines ]]; then
        say "the daemon's package_coverage_incomplete lines for $1 (the whole log):"
        printf '%s\n' "$lines" | sed 's/^/    | /'
    else
        say "no package_coverage_incomplete line for $1 in the whole daemon log"
    fi
}

strict_controls() {
    scenario strict "strict mode refuses an undeclared .deb on every host"
    local u code
    for u in "${STRICT_CONTROLS[@]}"; do
        code=$("${CURL[@]}" -sS --path-as-is -o /dev/null -w '%{http_code}' \
            -x "$PROXY" "http://HTTPS///$u" || true)
        [[ $code == 502 ]] || {
            show_coverage "${u%%/*}"
            fail "http://HTTPS///$u answered $code, want 502 (strict mode off for its host? the coverage lines above)"
        }
        log_has 0 "$(event unvouched_deb_refused)" "$(kv path "/${u#*/}")" ||
            fail "no unvouched_deb_refused line for /${u#*/}"
        say "refused, as strict mode should: ${u%%/*}"
    done
    passed
}

# live_install NAME PACKAGE [VERSION]: installs PACKAGE (VERSION, or the
# candidate) on client NAME through the cache, requires dpkg to report it
# installed at that version and the daemon to have served its .deb vouched
# (validated_hash=true, package_name=PACKAGE: not a dependency's), then
# purges it.
live_install() {
    local name=$1 pkg=$2 ver=${3:-} mark spec got
    [[ -n $ver ]] || ver=$(candidate "$name" "$pkg")
    [[ -n $ver && $ver != "(none)" ]] || fail "client $name has no candidate for $pkg"
    spec="$pkg=$ver"
    mark=$(log_mark)
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/install.$APT_RUNS.log
    if ! capt "$name" apt-get install -y --no-install-recommends "$spec" > "$APT_LOG" 2>&1 ||
        [[ -n $(apt_errors "$APT_LOG") ]]; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get install $spec on client $name failed (above)"
    fi
    got=$(dpkg-query -W -f='${Status} ${Version}' "$pkg" 2> /dev/null || true)
    [[ $got == "install ok installed $ver" ]] ||
        fail "client $name: dpkg reports '$got' for $pkg, want 'install ok installed $ver'"
    log_has "$mark" "$(event request)" '\.deb( |$)' 'validated_hash=true' "$(kv package_name "$pkg")" || {
        log_lines "$mark" "$(event request)" '\.deb' | sed 's/^/    | /'
        fail "client $name: the daemon served no vouched .deb (validated_hash=true, package_name=$pkg) for $spec"
    }
    say "$name: installed $spec, its .deb vouched ($(log_lines "$mark" "$(event request)" '\.deb' \
        'validated_hash=true' "$(kv package_name "$pkg")" | grep -oE ' path=[^ ]+' | head -n 1 | sed 's/^ //'))"
    dpkg --purge "$pkg" > /dev/null 2>&1 || fail "dpkg --purge $pkg failed"
}

installs() {
    scenario installs "one package per repository, its .deb vouched"
    local older
    live_install k8s-slash kubectl
    # The other source form installs from the other minor. awk reads all
    # of madison's output: an early exit could end apt-cache with SIGPIPE,
    # which pipefail would see (e2e/flat's design note 18).
    older=$(capt k8s-dot apt-cache madison kubectl |
        awk -v m="${K8S_A#v}." '!v && $3 ~ "^" m {v = $3} END {print v}')
    [[ -n $older ]] || fail "client k8s-dot lists no kubectl of $K8S_A"
    live_install k8s-dot kubectl "$older"
    live_install cuda cuda-toolkit-12-config-common
    live_install nvidia nvidia-container-toolkit-base
    live_install jenkins jenkins
    live_install sublime sublime-merge
    passed
}

# no_panics [WHEN]: fails on any panic or hash-conflict line in the whole
# daemon log, printing the first 20 (the head may end the pipe early: a
# diagnostic, so || true).
no_panics() {
    [[ -z $(panics_in_log) ]] || {
        panics_in_log | head -n 20 | sed 's/^/    | /' || true
        fail "the daemon logged a panic or a hash conflict${1:+ $1} (above)"
    }
}

whole_run() {
    scenario whole-run "no refusal but the controls', no panic"
    local refused
    refused=$(log_lines 0 "$(event unvouched_deb_refused)" | grep -v 'acu-live-control_' || true)
    [[ -z $refused ]] || {
        printf '%s\n' "$refused" | sed 's/^/    | /'
        fail "unvouched_deb_refused for a package the smoke installed (above)"
    }
    no_panics
    stop_daemon "the end"
    # A panic recovered while the daemon shut down is logged after the scan
    # above (stop_daemon catches only an unrecovered one, by exit status).
    no_panics "while it stopped"
    passed
}

setup
first_contact
adoptions
updates
strict_controls
installs
whole_run
say "timings: ${TIMINGS[*]}"
say "PASS: live smoke, finished $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
