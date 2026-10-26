# shellcheck shell=bash disable=SC2034  # the settings below are test.sh's
# lib.sh: helpers for e2e/flat's test.sh, which sources it. Everything runs
# inside the test container: nginx (the upstream), the daemon and the apt
# clients share it.
#
# AIDEV-NOTE: ruling R-H2-5 — every wait in this harness is poll (a bounded
# poll on an observable: a log line, an HTTP answer, an apt result) with a
# failure message; no bare sleep stands in for a condition. fail, and an
# unexpected exit (test.sh's on_exit), print the scenario, the daemon's log
# tail and nginx's access-log tail (show_logs). Every curl has a time limit
# (CURL), so a hung endpoint cannot stall a poll.

E2E_IMAGE="${E2E_IMAGE:-unknown image}"
DAEMON_LOG=/var/log/acu-e2e/daemon.log
NGINX_LOG=/var/log/acu-e2e/nginx-access.log
CONFIG=/etc/acu-e2e/config.toml
KEY=/etc/acu-e2e/keys/acu-e2e.gpg
PROXY=http://127.0.0.1:3142
ADMIN=http://127.0.0.1:6789
MIRROR=http://127.0.0.1:3142/m-signed
# Every curl of the harness: a hung endpoint fails one attempt after 10 s
# (inside a poll, the poll goes on) instead of blocking the run until the
# CI job's timeout. Each call answers in well under a second here.
CURL=(curl --max-time 10)

# The daemon's timers (config.toml, written by write_config). The scheduler
# ticks every PERIODIC/4 seconds (5 s at least): 7.5 s, rounded up here.
COOLDOWN=2
PERIODIC=30
TICK=8
# The longest a published change may take to reach a client that runs
# apt-get update once a cooldown: one cooldown for its check to fire,
# then the adoption and the flip barrier (phase A's quiet period is 1 s
# after the client's last anchor; measured 2–7 s; design note 5). by_client
# proves the client's check carried it, not the scheduler.
REACH=$((COOLDOWN + 20))

# apt-get update's exit status says nothing about failed items (spec §7):
# its output is grepped instead.
APT_ERRORS='Failed to fetch|Connection failed|timed out|Hash Sum mismatch|Mirror sync in progress'
# A signature failure need not print any of those words: apt keeps the old
# lists and exits 0, so the next update hides it, and it prints "Failed to
# fetch" for some failures only (apt 2.6.1: for a BADSIG Release.gpg, not
# for a missing key). These words catch it either way: "GPG error" (apt
# 2.x), "OpenPGP signature verification failed" (apt 3.x, sqv), "is not
# signed", "error occurred during the signature verification", "Clearsigned
# file isn't valid", and "Conflicting distribution" for a wrong Suite.
# Release-less and unsigned repositories behind a captive portal print the
# same words (design note 11), so these words count only on lines naming
# the signed host or the mirror prefix (design note 17).
# AIDEV-NOTE: apt names a host-root source (`deb http://signed.test/
# apt/stable/`, Sublime's shape) without the slash after the host
# (URI::ArchiveOnly): "http://signed.test apt/stable/ InRelease". So the
# host or prefix is followed by a slash or a space; test.sh's
# control-sig-root fails a filter that misses that form.
APT_SIGNED_HOSTS='//signed\.test[/ ]|/m-signed[/ ]'
APT_SIGNED_ERRORS="GPG error|OpenPGP signature verification failed|is not signed|signature verification|Clearsigned file isn't valid|Conflicting distribution"

SCENARIO=setup
SCENARIO_START=$SECONDS
SCENARIO_MARK=0
DAEMON_PID=""
APT_RUNS=0
TIMINGS=()
FAILED=0
BASELINE=0

say() {
    printf '[e2e-flat %s %4ds] %s\n' "$E2E_IMAGE" "$SECONDS" "$*"
}

fail() {
    FAILED=1
    say "FAIL in scenario '$SCENARIO': $*"
    show_logs
    exit 1
}

# show_logs: the daemon's log tail and nginx's access-log tail (ruling
# R-H2-5), for fail and for test.sh's unexpected-exit trap. A log not yet
# written (nginx's, before setup starts it) prints nothing.
show_logs() {
    echo "=== daemon log tail ($DAEMON_LOG)"
    tail -n 80 "$DAEMON_LOG" 2>/dev/null | sed -E 's/ client_addr=[^ ]+//' || true
    echo "=== nginx access log tail ($NGINX_LOG)"
    tail -n 30 "$NGINX_LOG" 2>/dev/null || true
}

# scenario NAME DESCRIPTION: starts a scenario; log assertions that pass
# SCENARIO_MARK see only its lines. A marker line in the daemon's log
# (appended like the daemon's own) dates the scenario for a reader.
scenario() {
    SCENARIO=$1
    SCENARIO_START=$SECONDS
    harness_mark "scenario=$1"
    SCENARIO_MARK=$(log_mark)
    say "=== scenario $1: $2"
}

# harness_mark FIELDS: a harness line in the daemon's log, with the time.
harness_mark() {
    echo "level=INFO msg=e2e_harness $* unixtime=$(date +%s)" >> "$DAEMON_LOG"
}

passed() {
    local took=$((SECONDS - SCENARIO_START))
    TIMINGS+=("$SCENARIO ${took}s")
    say "--- scenario $SCENARIO passed (${took}s)"
}

# poll SECONDS WHAT COMMAND...: runs COMMAND once a second until it
# succeeds; fails the scenario after SECONDS. POLL_TOOK is the wait.
poll() {
    local limit=$1 what=$2
    shift 2
    poll_quietly "$limit" "$@" || fail "timed out after ${limit}s waiting for $what"
}

# poll_quietly SECONDS COMMAND...: poll, returning 1 at the deadline
# instead of failing, for a caller that adds its own diagnostics.
poll_quietly() {
    local limit=$1 start=$SECONDS
    shift
    until "$@"; do
        ((SECONDS - start < limit)) || return 1
        sleep 1
    done
    POLL_TOOK=$((SECONDS - start))
}

# --- the daemon's log --------------------------------------------------

log_mark() {
    wc -l < "$DAEMON_LOG" 2>/dev/null || echo 0
}

# re QUOTED-TEXT: TEXT as an extended regex that matches it literally
# (`/` is not special in an ERE, so it stays as it is).
re() {
    printf '%s' "$1" | sed -e 's/[][\.*^$+?(){}|]/\\&/g'
}

# kv KEY VALUE: the regex of one key=value field of a text log line.
kv() {
    printf '(^| )%s=%s( |$)' "$1" "$(re "$2")"
}

# log_lines MARK REGEX...: the lines after line MARK that match every REGEX.
log_lines() {
    local out
    out="$(tail -n +"$(($1 + 1))" "$DAEMON_LOG")"
    shift
    local r
    for r in "$@"; do
        out="$(grep -E -- "$r" <<< "$out" || true)"
    done
    [[ -n $out ]] && printf '%s\n' "$out"
    return 0
}

log_count() {
    log_lines "$@" | grep -c . || true
}

log_has() {
    (($(log_count "$@") > 0))
}

# event NAME: the regex of a log line's msg.
event() {
    kv msg "$1"
}

suite_kv() {
    kv suite_path "$1"
}

# --- processes -----------------------------------------------------------

# The nginx bounds (10 s) are generous: nginx binds and stops in well
# under a second here; nothing in the daemon's timers bears on them.
start_nginx() {
    nginx -c /etc/acu-e2e/nginx.conf
    poll 10 "nginx on :80" "${CURL[@]}" -fsS -o /dev/null -H 'Host: signed.test' \
        http://127.0.0.1/debian/dists/acu/InRelease
}

stop_nginx() {
    nginx -c /etc/acu-e2e/nginx.conf -s stop
    poll 10 "nginx to stop" port_closed 80
}

port_closed() {
    ! (echo > "/dev/tcp/127.0.0.1/$1") 2> /dev/null
}

# start_daemon [NAME=VALUE...]: starts the daemon with the environment
# additions given, and waits for both listeners. 30 s is generous: both
# open within a second or two of the start.
start_daemon() {
    say "starting the daemon${*:+ with $*}"
    env "$@" /usr/local/bin/apt-cacher-ultra -config "$CONFIG" >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
    poll 30 "the daemon's admin listener" "${CURL[@]}" -fs -o /dev/null "$ADMIN/healthz"
    poll 30 "the daemon's proxy listener" bash -c '(echo > /dev/tcp/127.0.0.1/3142) 2> /dev/null'
}

# stop_daemon: SIGTERM, then 40 s for the exit: the daemon's HTTP drain
# budget (shutdownTimeout, 30 s, cmd/apt-cacher-ultra/main.go), its 1 s
# tunnel grace and a margin for the teardown. Every planned stop has
# nothing in flight, and then the daemon exits in about a second.
stop_daemon() {
    [[ -n $DAEMON_PID ]] || return 0
    say "stopping the daemon"
    kill "$DAEMON_PID"
    poll 40 "the daemon to exit" daemon_gone
    wait "$DAEMON_PID" 2> /dev/null || true
    DAEMON_PID=""
}

daemon_gone() {
    ! kill -0 "$DAEMON_PID" 2> /dev/null
}

# publish REPO VERSION: points the upstream's REPO/current at VERSION
# (v1 or v2) with one rename, as a real publish replaces a tree.
publish() {
    ln -sfn "$2" "/fixtures/$1/current.new"
    mv -T "/fixtures/$1/current.new" "/fixtures/$1/current"
    say "published $1 $2"
}

# --- the status JSON ---------------------------------------------------

# suite HOST SUITE-PATH FIELD: one field of the suite's entry in the admin
# status JSON (suites[]); empty when the suite or the field is absent.
suite() {
    "${CURL[@]}" -fsS "$ADMIN/?format=json" |
        jq -r --arg h "$1" --arg p "$2" --arg f "$3" \
            '.suites[] | select(.host == $h and .suite_path == $p) | .[$f] // empty'
}

# adopted_as HOST SUITE-PATH FORM: the suite serves a snapshot of FORM.
adopted_as() {
    [[ -n $(suite "$1" "$2" current_snapshot_id) && $(suite "$1" "$2" anchor_form) == "$3" ]]
}

# metric NAME-AND-LABELS: the value of one sample on /metrics (0 when it
# has none yet).
metric() {
    "${CURL[@]}" -fsS "$ADMIN/metrics" | awk -v m="$1" '$1 == m {v = $2} END {print v + 0}'
}

# baseline START HOST SUITE...: waits (10 s at most) until every suite's
# last successful check is no older than START less a cooldown — the
# checks the client updates since START triggered, or the one whose
# cooldown made them skip theirs — and sets BASELINE to the oldest. 10 s
# is generous: the checks that update fired land within a second or two.
baseline() {
    local start=$1
    shift
    poll 10 "the suites' checks after the client's update" baseline_met "$start" "$@"
}

baseline_met() {
    local start=$1 host=$2 s v min=""
    shift 2
    for s in "$@"; do
        v=$(suite "$host" "$s" last_success_unixtime)
        [[ -n $v ]] || return 1
        if [[ -z $min ]] || ((v < min)); then min=$v; fi
    done
    ((min >= start - COOLDOWN)) || return 1
    BASELINE=$min
}

# by_client: the client saw the publish before the scheduler could have
# checked the suites again — one periodic_refresh after their last
# success, BASELINE — so a client-triggered check carried it (spec C4's
# T1), not the periodic refresh (design note 5).
by_client() {
    local now
    now=$(date +%s)
    ((now < BASELINE + PERIODIC)) ||
        fail "the publish reached the client $((now - BASELINE))s after the suites' last success; the scheduler could have carried it (want under ${PERIODIC}s)"
    say "carried by the client's check: $((now - BASELINE))s after the suites' last success (the scheduler: ${PERIODIC}s)"
}

# --- apt clients ---------------------------------------------------------

# client NAME PROXY SOURCE-LINE...: a fresh apt client: its own sources,
# lists and archives under /clients/NAME (dpkg's database is shared, so
# install checks purge what they install). PROXY is the cache's proxy
# URL, or DIRECT for a mirror-mode client.
client() {
    local name=$1 proxy=$2 c=/clients/$1
    shift 2
    rm -rf "$c"
    mkdir -p "$c/lists/partial" "$c/cache/archives/partial" "$c/sources.list.d"
    printf '%s\n' "$@" > "$c/sources.list"
    cat > "$c/apt.conf" << EOF
Dir::Etc::SourceList "$c/sources.list";
Dir::Etc::SourceParts "$c/sources.list.d";
Dir::State::Lists "$c/lists";
Dir::Cache "$c/cache";
Acquire::http::Proxy "$proxy";
EOF
}

# capt NAME COMMAND...: COMMAND under client NAME's apt configuration.
capt() {
    local name=$1
    shift
    APT_CONFIG=/clients/$name/apt.conf "$@"
}

# apt_update NAME [apt-get options...]: apt-get update on client NAME;
# fails when its output holds an apt error (apt_errors). The output is
# kept in APT_LOG.
apt_update() {
    local name=$1
    shift
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/update.$APT_RUNS.log
    capt "$name" apt-get update "$@" > "$APT_LOG" 2>&1 || true
    if [[ -n $(apt_errors "$APT_LOG") ]]; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get update on client $name printed an error (above)"
    fi
}

# apt_errors FILE: the error lines of apt output FILE — those matching
# APT_ERRORS, and signature errors naming the signed host or the mirror
# prefix. Every grep reads all its input: no early exit for pipefail to
# see.
apt_errors() {
    local errs
    errs=$({
        grep -E -- "$APT_ERRORS" "$1"
        grep -E -- "$APT_SIGNED_HOSTS" "$1" | grep -E -- "$APT_SIGNED_ERRORS"
    } || true)
    printf '%s' "$errs"
}

# candidate NAME PACKAGE: the version client NAME would install.
candidate() {
    capt "$1" apt-cache policy "$2" | awk '/Candidate:/ {print $2}'
}

# candidates_are NAME PACKAGE=VERSION...: every candidate is as given.
candidates_are() {
    local name=$1 spec
    shift
    for spec in "$@"; do
        [[ $(candidate "$name" "${spec%%=*}") == "${spec#*=}" ]] || return 1
    done
}

# apt_install NAME PACKAGE=VERSION... [-- apt-get options...]: installs the
# packages on client NAME, checks the installed files' versions, then
# purges them. The output is kept in APT_LOG.
apt_install() {
    local name=$1 specs=() opts=() spec
    shift
    while (($#)); do
        if [[ $1 == -- ]]; then
            shift
            opts=("$@")
            break
        fi
        specs+=("$1")
        shift
    done
    APT_RUNS=$((APT_RUNS + 1))
    APT_LOG=/clients/$name/install.$APT_RUNS.log
    if ! capt "$name" apt-get install -y --no-install-recommends "${opts[@]}" "${specs[@]}" \
        > "$APT_LOG" 2>&1 || [[ -n $(apt_errors "$APT_LOG") ]]; then
        sed 's/^/    | /' "$APT_LOG"
        fail "apt-get install ${specs[*]} on client $name failed (above)"
    fi
    local pkgs=()
    for spec in "${specs[@]}"; do
        local pkg=${spec%%=*} ver=${spec#*=}
        pkgs+=("$pkg")
        [[ $(cat "/usr/share/acu-e2e/$pkg") == "$pkg $ver" ]] ||
            fail "client $name installed $(cat "/usr/share/acu-e2e/$pkg" 2> /dev/null), want $pkg $ver"
    done
    dpkg --purge "${pkgs[@]}" > /dev/null
}

# until_candidates LIMIT NAME... -- PACKAGE=VERSION...: runs apt-get update
# on every client NAME once a cooldown until each reports the versions;
# every update must be clean. POLL_TOOK is the wait.
until_candidates() {
    local limit=$1 names=() specs=() start=$SECONDS n
    shift
    while [[ $1 != -- ]]; do
        names+=("$1")
        shift
    done
    shift
    specs=("$@")
    while :; do
        local all=1
        for n in "${names[@]}"; do
            apt_update "$n"
            candidates_are "$n" "${specs[@]}" || all=0
        done
        ((all)) && break
        if ((SECONDS - start >= limit)); then
            fail "clients ${names[*]} did not report ${specs[*]} within ${limit}s"
        fi
        sleep "$COOLDOWN"
    done
    POLL_TOOK=$((SECONDS - start))
}

# --- request lines -------------------------------------------------------

# apt_sent_line REGEX: client debug output (Debug::Acquire::http) in
# APT_LOG holds a request line apt sent that matches REGEX exactly. apt
# prints the header lines as sent, CRLF included. grep reads the file
# itself: no pipe, so grep -q's early exit cannot fail anything under
# pipefail.
apt_sent_line() {
    grep -Eq -- "^GET $1 HTTP/1\\.1"$'\r'"?\$" "$APT_LOG"
}

# apt_sent REGEX: apt_sent_line, or fail.
apt_sent() {
    apt_sent_line "$1" || fail "apt did not send GET $1 (Debug::Acquire::http output in $APT_LOG)"
}

# upstream_got HOST PATH-REGEX: nginx logged that request line from the
# cache.
upstream_got() {
    grep -Eq -- "^$(re "$1") \"GET $2 HTTP/1\\.1\" " "$NGINX_LOG" ||
        fail "the upstream never got GET $2 for $1"
}
