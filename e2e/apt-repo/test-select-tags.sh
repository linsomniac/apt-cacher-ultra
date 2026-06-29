#!/usr/bin/env bash
# Unit test for scripts/select-stable-tags.sh.
set -euo pipefail
cd "$(dirname "$0")/../.."

fixture=e2e/apt-repo/fixtures/releases.json

# Newest 5 finals, ascending. Finals present: 0.9.7 0.9.8 0.10.1 0.10.2
# 0.10.3 0.10.4 (six). Drop the oldest -> these five:
expected=$'0.9.8\n0.10.1\n0.10.2\n0.10.3\n0.10.4'
got="$(scripts/select-stable-tags.sh 5 < "$fixture")"
if [ "$got" != "$expected" ]; then
    echo "FAIL newest-5: expected:"; printf '%s\n' "$expected"
    echo "got:"; printf '%s\n' "$got"; exit 1
fi

# Fewer-than-N asks return all finals (6 available).
n_all="$(scripts/select-stable-tags.sh 10 < "$fixture" | grep -c .)"
[ "$n_all" = "6" ] || { echo "FAIL fewer-than-N: expected 6, got $n_all"; exit 1; }

# Non-integer N is rejected with the documented exit code 2.
scripts/select-stable-tags.sh abc < "$fixture" >/dev/null 2>&1 || rc=$?
[ "${rc:-0}" = "2" ] || { echo "FAIL: non-integer N should exit 2, got ${rc:-0}"; exit 1; }

echo "PASS test-select-tags"
