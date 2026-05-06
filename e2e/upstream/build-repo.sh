#!/bin/sh
# build-repo.sh assembles a minimal but valid Debian repository tree
# under $1 (default /out) so an apt client driven through
# apt-cacher-ultra can run `apt-get update && apt-get install hello-acu`
# end-to-end. Intended to run during the upstream image's build stage.
#
# Layout produced:
#   pool/main/h/hello-acu/hello-acu_1.0_amd64.deb
#   dists/noble/main/binary-amd64/Packages
#   dists/noble/main/binary-amd64/Packages.gz
#   dists/noble/Release
#
# When SIGNED=1 (opt-in, default off so existing callers stay
# unchanged): also produce dists/noble/InRelease (clearsigned), and
# emit the ephemeral signing key as $OUT/aculan-test.gpg (binary
# pubkey, droppable into /etc/apt/trusted.gpg.d/) and
# $OUT/aculan-test.fingerprint (40-char hex, suitable for a
# [[trusted_signer]] block). Used by the e2e/deb/ harness to drive
# Phase 2 adoption.
#
# Without SIGNED=1, no GPG signature is generated and the e2e apt
# client uses `[trusted=yes]` in sources.list — the standard apt
# mechanism for unsigned repos in test environments.
set -eu

OUT="${1:-/out}"
SUITE=noble
COMPONENT=main
ARCH=amd64
PKG=hello-acu
# VER is overridable so a caller can build two fixture trees with the
# same signing key but distinct .deb versions (and therefore distinct
# Packages content and Release SHA256 entries). The deb-install
# harness uses this to drive an adoption flip — adoption fires only
# on a CHANGED InRelease, so the fixture must materialize change.
VER="${VER:-1.0}"

mkdir -p "$OUT/dists/$SUITE/$COMPONENT/binary-$ARCH"
mkdir -p "$OUT/pool/$COMPONENT/h/$PKG"

# Build the trivial .deb. dpkg-deb requires a directory layout with
# DEBIAN/control plus the staged file tree.
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$PKG/DEBIAN" "$WORK/$PKG/usr/bin"
cat > "$WORK/$PKG/DEBIAN/control" <<EOF
Package: $PKG
Version: $VER
Architecture: $ARCH
Maintainer: apt-cacher-ultra-e2e <noreply@example.invalid>
Description: trivial test package for apt-cacher-ultra e2e
 This package exists only so the e2e test can drive a real
 apt-get install through the cache.
EOF

cat > "$WORK/$PKG/usr/bin/hello-acu" <<'EOF'
#!/bin/sh
echo "hello from apt-cacher-ultra e2e"
EOF
chmod 755 "$WORK/$PKG/usr/bin/hello-acu"

dpkg-deb --build "$WORK/$PKG" \
    "$OUT/pool/$COMPONENT/h/$PKG/${PKG}_${VER}_${ARCH}.deb"

# Generate Packages indices. apt-ftparchive walks pool/ and emits
# entries with paths relative to cwd, so we must run it from $OUT.
cd "$OUT"
apt-ftparchive packages "pool/$COMPONENT" \
    > "dists/$SUITE/$COMPONENT/binary-$ARCH/Packages"
gzip -kn9 "dists/$SUITE/$COMPONENT/binary-$ARCH/Packages"

# Release file: hashes + metadata for InRelease/Release apt fetches.
cat > /tmp/apt-ftparchive.conf <<EOF
APT::FTPArchive::Release::Origin "apt-cacher-ultra-e2e";
APT::FTPArchive::Release::Label "test";
APT::FTPArchive::Release::Suite "$SUITE";
APT::FTPArchive::Release::Codename "$SUITE";
APT::FTPArchive::Release::Architectures "$ARCH";
APT::FTPArchive::Release::Components "$COMPONENT";
APT::FTPArchive::Release::Description "apt-cacher-ultra e2e fixture";
EOF
apt-ftparchive -c /tmp/apt-ftparchive.conf release "dists/$SUITE" \
    > "dists/$SUITE/Release"

# Phase 2 adoption fixture (opt-in). gpg + an ephemeral keypair are
# generated only when the caller asks for a signed repo. The key is
# disposable — it lives only in this build image and is never
# checkpointed, so a fresh fingerprint is produced on every build.
# That is fine: the [[trusted_signer]] block in the test config is
# generated from the same fingerprint file, so they always match.
if [ "${SIGNED:-0}" = "1" ]; then
    if ! command -v gpg >/dev/null 2>&1; then
        echo "build-repo.sh: SIGNED=1 requested but gpg is not installed" >&2
        exit 2
    fi

    # GNUPGHOME modes:
    #   - Caller supplies it AND it already contains a secret key:
    #     reuse the key. Used by the deb-install harness so V1 and V2
    #     fixture builds share a fingerprint.
    #   - Caller supplies it but it is empty (no secret key): generate
    #     one in place; subsequent invocations with the same path
    #     fall into the reuse branch.
    #   - Caller does not supply it: allocate a fresh tempdir and
    #     scrub it on exit. Single-invocation default.
    if [ -n "${GNUPGHOME:-}" ]; then
        mkdir -p "$GNUPGHOME"
        chmod 700 "$GNUPGHOME"
        export GNUPGHOME
        # Re-install trap to ALSO scrub $WORK (the parent trap is
        # otherwise overwritten silently); $GNUPGHOME is owned by the
        # caller and we leave it alone.
        trap 'rm -rf "$WORK"' EXIT
    else
        GNUPGHOME="$(mktemp -d)"
        chmod 700 "$GNUPGHOME"
        export GNUPGHOME
        # AIDEV-NOTE: trap restores the parent EXIT-trap (rm -rf $WORK)
        # AND scrubs the GNUPGHOME we just created. Without re-installing
        # the trap the keyring tempdir would survive.
        trap 'rm -rf "$WORK" "$GNUPGHOME"' EXIT
    fi

    # quick-generate-key produces a usable signing key in well under a
    # second on modern entropy. rsa3072/sign covers Debian-style
    # InRelease signing without pulling in encryption/auth subkeys we
    # never use. Skip if the caller-provided GNUPGHOME already has a key.
    if ! gpg --list-secret-keys --with-colons 2>/dev/null | grep -q '^sec:'; then
        gpg --batch --pinentry-mode loopback --passphrase '' \
            --quick-generate-key 'apt-cacher-ultra e2e <e2e@example.invalid>' rsa3072 sign 0
    fi

    # First fpr line in --with-colons is the primary key fingerprint
    # (40-char uppercase hex). Used in two places: the InRelease
    # signing call below, and the operator-facing fingerprint file.
    FINGERPRINT="$(gpg --list-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')"
    if [ -z "$FINGERPRINT" ]; then
        echo "build-repo.sh: failed to read generated key fingerprint" >&2
        exit 1
    fi

    # Clearsigned InRelease (the modern apt format: payload + signature
    # in one file). The cache reads InRelease first; Release.gpg
    # detached signing is supported but unused here to keep the
    # fixture minimal.
    gpg --batch --yes --pinentry-mode loopback --passphrase '' \
        --default-key "$FINGERPRINT" \
        --clearsign \
        --output "$OUT/dists/$SUITE/InRelease" \
        "$OUT/dists/$SUITE/Release"

    # Binary pubkey (drop-in for /etc/apt/trusted.gpg.d/), plus the
    # raw fingerprint so the caller can templatize a [[trusted_signer]]
    # block without re-parsing the keyring.
    gpg --batch --yes --output "$OUT/aculan-test.gpg" --export "$FINGERPRINT"
    printf '%s\n' "$FINGERPRINT" > "$OUT/aculan-test.fingerprint"
fi
