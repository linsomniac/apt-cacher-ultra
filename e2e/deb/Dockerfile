# SPEC §14 step 3 + SPEC2 §14 step 6: validate that the .deb installs
# cleanly on supported Ubuntu LTS targets, that the systemd unit's
# ExecStart binding brings up a working listener, AND that the daemon
# successfully adopts at least one snapshot when configured with
# adoption.enabled = true.
#
# Three stages:
#   1. `build`     — compile apt-cacher-ultra and run nfpm to produce
#                    a .deb under /out/.
#   2. `repo-build`— assemble a GPG-signed Debian repo fixture (one
#                    suite, one .deb) and emit the signing key.
#                    Output goes to /repo/ alongside an
#                    aculan-test.gpg pubkey and an
#                    aculan-test.fingerprint scalar.
#   3. `test`      — install on ubuntu:${UBUNTU_VERSION}, validate the
#                    package contract, run the daemon to bind :3142,
#                    THEN restart it under an adoption-mode config,
#                    drive a freshness fetch through the proxy, and
#                    assert a snapshot was adopted.
#
# The test stage does not run systemd-as-PID-1 (that requires a
# privileged container + cgroup mounts that vary across kernels and
# CI runners). The unit file is validated statically with
# `systemd-analyze verify`; full systemd-managed coverage is the
# job of step 4 (production deployment).
#
# Build context must be the repo root:
#   docker build -f e2e/deb/Dockerfile \
#       --build-arg UBUNTU_VERSION=24.04 \
#       --target test \
#       -t aculan-deb-test:24.04 .

# Pre-FROM ARGs — Docker only consumes ARGs in FROM substitution if
# they are declared before any FROM in the file. These get
# re-declared inside their consuming stage if also needed in RUN.
ARG UBUNTU_VERSION=24.04
ARG VERSION=0.0.0-e2e

FROM golang:1.25-bookworm AS build
ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /src/build/apt-cacher-ultra \
    ./cmd/apt-cacher-ultra
# nfpm v2 — pinned for reproducibility, bump deliberately.
RUN GOBIN=/usr/local/bin go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.40.0
RUN mkdir -p /out && VERSION=${VERSION} nfpm pkg \
    --packager deb \
    --config packaging/nfpm.yaml \
    --target /out/apt-cacher-ultra_${VERSION}_amd64.deb

# repo-build: produce the signed-fixture inputs that the Phase 2
# adoption smoke test consumes. Kept separate from `build` because
# it pulls a different toolchain (apt-utils, dpkg-dev, gpg) that the
# Go build stage does not need.
#
# Adoption fires on a CHANGED InRelease (freshness.go: result=changed).
# A static fixture would never trigger adoption — the freshness check
# just records "unchanged" forever. So this stage builds two fixture
# trees, V1 (hello-acu_1.0) and V2 (hello-acu_1.1), signed with the
# SAME ephemeral key. test.sh stages V1, primes the cache, swaps to
# V2, and the second InRelease GET drives the adoption flip.
FROM debian:stable-slim AS repo-build
# `gnupg` (the metapackage) is required rather than just `gpg`,
# because the slim image doesn't ship gpg-agent and gpg --quick-
# generate-key delegates to the agent for key generation.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        apt-utils dpkg-dev gzip gnupg && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /work
COPY e2e/upstream/build-repo.sh /work/build-repo.sh
RUN chmod +x /work/build-repo.sh
# GNUPGHOME persists across the two invocations; build-repo.sh's
# reuse branch picks up the key planted on the first call. The first
# call also populates aculan-test.{gpg,fingerprint} in /repo-v1; the
# second call regenerates them in /repo-v2 — same content because the
# key is unchanged.
ENV GNUPGHOME=/keyring
RUN mkdir -p /keyring && chmod 700 /keyring && \
    SIGNED=1 VER=1.0 /work/build-repo.sh /repo-v1 && \
    SIGNED=1 VER=1.1 /work/build-repo.sh /repo-v2

FROM ubuntu:${UBUNTU_VERSION} AS test
ENV DEBIAN_FRONTEND=noninteractive
# adduser is in the deb's depends and ships with ubuntu:* base.
# systemd brings systemd-analyze (used to lint the unit file).
# curl + ca-certificates serve the HTTP probe.
# util-linux (already present) provides runuser for the unprivileged
# daemon launch.
# nginx-light serves the signed fixture for the adoption smoke
# (nginx-light avoids pulling in xslt / mail modules / geoip2
# we never use).
# sqlite3 lets the test probe the cache DB to assert
# current_snapshot_id was set after adoption.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        adduser \
        systemd \
        curl \
        ca-certificates \
        nginx-light \
        sqlite3 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/*.deb /tmp/
# Fixture trees (V1 + V2) + signing key + fingerprint. The pubkey is
# dropped at /etc/apt/trusted.gpg.d/ in test.sh (not here) so the test
# script owns the install/uninstall cycle around the adoption smoke.
# Both fixture trees carry the same aculan-test.{gpg,fingerprint};
# test.sh reads them from /fixture-v1.
COPY --from=repo-build /repo-v1 /fixture-v1
COPY --from=repo-build /repo-v2 /fixture-v2
COPY e2e/deb/test.sh /test.sh
RUN chmod +x /test.sh
CMD ["/test.sh"]
