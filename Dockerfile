# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/apt-cacher-ultra ./cmd/apt-cacher-ultra

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 acu \
    && adduser -D -H -u 10001 -G acu acu \
    && mkdir -p /var/cache/apt-cacher-ultra \
    && chown acu:acu /var/cache/apt-cacher-ultra
COPY --from=build /out/apt-cacher-ultra /usr/local/bin/apt-cacher-ultra
COPY docker/config.toml /etc/apt-cacher-ultra/config.toml
USER 10001:10001
EXPOSE 3142 6789
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:6789/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/apt-cacher-ultra"]
CMD ["-config", "/etc/apt-cacher-ultra/config.toml"]
