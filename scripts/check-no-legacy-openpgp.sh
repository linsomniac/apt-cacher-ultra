#!/usr/bin/env bash
# Run from the repository root. Keep the OpenVEX assessment true for both
# production platforms, including dependencies introduced by other modules.
set -euo pipefail
go_cmd="${GO:-go}"
for arch in amd64 arm64; do
  # Capture first: a failed go list must fail the check, not look like an empty
  # dependency graph. Match the production build's CGO setting and no test tags.
  if ! dependencies="$(CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_cmd" list -mod=readonly -deps ./cmd/apt-cacher-ultra)"; then
    echo "Cannot inspect production dependencies for linux/$arch." >&2
    exit 1
  fi
  while IFS= read -r package; do
    case "$package" in
      golang.org/x/crypto/openpgp|golang.org/x/crypto/openpgp/*)
        echo "Forbidden legacy OpenPGP dependency on linux/$arch: $package" >&2
        echo 'Use github.com/ProtonMail/go-crypto/openpgp; see security/openpgp.vex.json.' >&2
        exit 1
        ;;
    esac
  done <<< "$dependencies"
  echo "linux/$arch: no legacy OpenPGP dependencies."
done
