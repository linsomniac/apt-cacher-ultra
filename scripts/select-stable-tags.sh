#!/usr/bin/env bash
# Reads `gh release list --json tagName,isDraft,isPrerelease` JSON on
# stdin and writes the newest N stable (non-draft, non-prerelease) tag
# names to stdout, one per line, in ascending version order.
#
#   gh release list --json tagName,isDraft,isPrerelease \
#       | scripts/select-stable-tags.sh 5
#
# Pure transform — no network, no gh — so the newest-N selection logic is
# unit-testable against a fixture.
set -euo pipefail

n="${1:?usage: select-stable-tags.sh <N> (JSON on stdin)}"
case "$n" in
    ''|*[!0-9]*) echo "select-stable-tags.sh: N must be a positive integer, got '$n'" >&2; exit 2 ;;
esac

# Drop drafts and prereleases, emit bare tag names. `sort -V` orders
# Debian-ish versions correctly (0.10.4 above 0.9.8); `tail -n N` keeps the
# newest. Finals carry no '-' suffix, so the version sort is unambiguous.
jq -r '.[] | select(.isDraft | not) | select(.isPrerelease | not) | .tagName' \
    | sort -V \
    | tail -n "$n"
