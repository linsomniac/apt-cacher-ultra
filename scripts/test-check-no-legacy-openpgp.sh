#!/usr/bin/env bash
# Exercise the guard with real, offline Go dependency graphs.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT
cd "$fixture"
# Keep fixtures independent of a developer's workspace and build flags.
export GOWORK=off GOFLAGS=-buildvcs=false GOPROXY=off
mkdir -p cmd/apt-cacher-ultra internal/helper crypto/{bcrypt,openpgp/packet,openpgputil}
cat > go.mod <<'EOF'
module guard.test

go 1.26.0

require golang.org/x/crypto v0.0.0
replace golang.org/x/crypto => ./crypto
EOF
printf 'module golang.org/x/crypto\n\ngo 1.26.0\n' > crypto/go.mod
for package in bcrypt openpgp openpgp/packet openpgputil; do
  printf 'package %s\n' "${package##*/}" > "crypto/$package/package.go"
done
cat > cmd/apt-cacher-ultra/main.go <<'EOF'
package main
import _ "golang.org/x/crypto/bcrypt"
import _ "golang.org/x/crypto/openpgputil"
func main() {}
EOF
bash "$script_dir/check-no-legacy-openpgp.sh"

expect_rejection() {
  if bash "$script_dir/check-no-legacy-openpgp.sh" > output.log 2>&1; then
    echo "Guard unexpectedly accepted: $1" >&2
    exit 1
  fi
  if ! grep -Fq "$2" output.log; then
    cat output.log >&2
    echo "Guard failed for the wrong reason: $1" >&2
    exit 1
  fi
  echo "PASS: $1"
}

cat > cmd/apt-cacher-ultra/legacy.go <<'EOF'
package main
import _ "golang.org/x/crypto/openpgp"
EOF
expect_rejection 'direct legacy package' 'Forbidden legacy OpenPGP dependency on linux/amd64: golang.org/x/crypto/openpgp'
rm cmd/apt-cacher-ultra/legacy.go

cat > internal/helper/helper.go <<'EOF'
package helper
import _ "golang.org/x/crypto/openpgp/packet"
EOF
cat > cmd/apt-cacher-ultra/legacy_arm64.go <<'EOF'
package main
import _ "guard.test/internal/helper"
EOF
expect_rejection 'ARM64-only transitive subpackage' 'Forbidden legacy OpenPGP dependency on linux/arm64: golang.org/x/crypto/openpgp/packet'
rm cmd/apt-cacher-ultra/legacy_arm64.go

cat > cmd/apt-cacher-ultra/broken.go <<'EOF'
package main
import _ "missing.invalid/package"
EOF
expect_rejection 'dependency lookup failure' 'Cannot inspect production dependencies for linux/amd64.'
