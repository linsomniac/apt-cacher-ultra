#!/usr/bin/env bash
# Runs every fast (docker-free) apt-repo unit test in this directory.
set -uo pipefail
cd "$(dirname "$0")"
rc=0
for t in test-*.sh; do
    [ -e "$t" ] || continue
    echo "── $t"
    if ! bash "$t"; then rc=1; fi
done
exit "$rc"
