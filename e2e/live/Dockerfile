# e2e/live: the live-upstream smoke (the flat-repositories overview, "PR #8
# parity", H): real apt clients against the live flat repositories
# pkgs.k8s.io, NVIDIA CUDA, libnvidia-container, pkg.jenkins.io and
# download.sublimetext.com through apt-cacher-ultra, in one container.
# Manual, before 1.1.0 and before later releases that change the request
# path, adoption or vouching; never in CI (it depends on five vendors).
# e2e/live/run.sh builds and runs it; e2e/live/README.md says what it
# checks.
#
# Two stages:
#   build  the daemon exactly as `make build` builds it (no build tags:
#          the binary a release ships).
#   test   debian:12 with curl and jq, the dependencies the smoke's
#          packages need from Debian (installed here, directly, so every
#          apt run of the smoke goes through the cache alone), and
#          e2e/flat's lib.sh beside smoke.sh.
#
# Build context is the repo root:
#   docker build -f e2e/live/Dockerfile -t acu-e2e-live .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.Version=0.0.0-e2e-live" \
    -o /out/apt-cacher-ultra ./cmd/apt-cacher-ultra

# debian:12 because the CUDA repository is per distribution (debian12).
# The second list is what jenkins and sublime-merge depend on beyond the
# base image; when a vendor adds a dependency, the install's apt error
# names it, and it goes here.
FROM debian:12 AS test
ENV DEBIAN_FRONTEND=noninteractive
RUN rm -f /etc/apt/apt.conf.d/docker-* && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        curl jq ca-certificates \
        adduser net-tools sysvinit-utils init-system-helpers \
        libgtk-3-0 libcurl4 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    mkdir -p /run/acu-e2e /var/log/acu-e2e
COPY --from=build /out/apt-cacher-ultra /usr/local/bin/apt-cacher-ultra
COPY e2e/flat/lib.sh e2e/live/smoke.sh /usr/local/lib/acu-e2e/
CMD ["bash", "/usr/local/lib/acu-e2e/smoke.sh"]
