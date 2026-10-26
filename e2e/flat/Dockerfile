# e2e/flat: real apt against flat (and dists/) fixture repositories through
# apt-cacher-ultra, in one container (spec §7, "Real-apt end-to-end"): nginx
# serves the upstream fixtures, the daemon runs beside it, and apt clients
# with their own state directories talk to it. e2e/flat/run.sh builds and
# runs one image per client image.
#
# Three stages:
#   build     the daemon, with -tags e2ehooks (the commit-delay seam,
#             cmd/apt-cacher-ultra/e2ehooks.go); never shipped.
#   fixtures  build-fixtures.sh: the repositories, two versions each, and
#             a throwaway signing key generated here (ruling R-H2-1).
#   test      ${CLIENT_IMAGE} with nginx, curl, jq and apt-file; its
#             docker-* apt.conf files are removed, so apt behaves as on a
#             host (no gzip-only indexes, languages, or deleted archives).
#
# Build context is the repo root:
#   docker build -f e2e/flat/Dockerfile --build-arg CLIENT_IMAGE=debian:12 \
#       -t acu-e2e-flat:debian-12 .
ARG CLIENT_IMAGE=debian:12

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags e2ehooks \
    -ldflags "-s -w -X main.Version=0.0.0-e2e-flat" \
    -o /out/apt-cacher-ultra ./cmd/apt-cacher-ultra

FROM debian:bookworm-slim AS fixtures
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        apt-utils dpkg-dev gnupg gzip xz-utils && \
    rm -rf /var/lib/apt/lists/*
COPY e2e/flat/build-fixtures.sh /usr/local/bin/build-fixtures.sh
RUN bash /usr/local/bin/build-fixtures.sh /fixtures

FROM ${CLIENT_IMAGE} AS test
ENV DEBIAN_FRONTEND=noninteractive
# Debian 11's LTS ended on 2026-08-31 and its packages moved to
# archive.debian.org; the image still names deb.debian.org (design note 9).
# bullseye-security stays: the image's own packages (perl-base) come from
# it, so apt-file's perl cannot install without it. Retries: apt 2.2
# (Debian 11) makes none by default.
RUN . /etc/os-release && \
    if [ "$VERSION_CODENAME" = bullseye ]; then \
        printf '%s\n' 'deb http://archive.debian.org/debian bullseye main' \
            'deb http://archive.debian.org/debian-security bullseye-security main' \
            > /etc/apt/sources.list; \
    fi && \
    rm -f /etc/apt/apt.conf.d/docker-* && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        nginx-light curl jq apt-file ca-certificates && \
    rm -rf /var/lib/apt/lists/* /etc/apt/sources.list /etc/apt/sources.list.d/* && \
    mkdir -p /run/acu-e2e /var/log/acu-e2e
COPY --from=build /out/apt-cacher-ultra /usr/local/bin/apt-cacher-ultra
COPY --from=fixtures /fixtures /fixtures
COPY e2e/flat/nginx.conf /etc/acu-e2e/nginx.conf
COPY e2e/flat/lib.sh e2e/flat/test.sh /usr/local/lib/acu-e2e/
CMD ["bash", "/usr/local/lib/acu-e2e/test.sh"]
