# SPEC §14 step 3: validate that the .deb installs cleanly on the
# supported Ubuntu LTS targets and that the systemd unit's ExecStart
# binding does in fact bring up a working listener.
#
# Two stages:
#   1. `build`  — compile apt-cacher-ultra and run nfpm to produce a
#                 .deb under /out/. Same path the Makefile takes.
#   2. `test`   — install on ubuntu:${UBUNTU_VERSION} in a clean
#                 image, validate the package contract (files,
#                 user, perms, unit syntax), and start the daemon
#                 the way the unit would.
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

FROM ubuntu:${UBUNTU_VERSION} AS test
ENV DEBIAN_FRONTEND=noninteractive
# adduser is in the deb's depends and ships with ubuntu:* base.
# systemd brings systemd-analyze (used to lint the unit file).
# curl + ca-certificates serve the HTTP probe.
# util-linux (already present) provides runuser for the unprivileged
# daemon launch.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        adduser \
        systemd \
        curl \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/*.deb /tmp/
COPY e2e/deb/test.sh /test.sh
RUN chmod +x /test.sh
CMD ["/test.sh"]
