# apt-cacher-ultra

A robust apt repository cache focused on availability under upstream failure.

Designed as a replacement for `apt-cacher-ng` that keeps cache hits fast and
successful even when upstream Ubuntu/Debian/PPA mirrors are slow, broken, or
under DDoS. See [SPEC.md](SPEC.md) for the Phase 1 specification.

## Status

Phase 1 scaffolding only. Not yet functional.

## Build

```sh
make build           # binary at ./build/apt-cacher-ultra
make test            # unit tests
make lint            # golangci-lint (must be installed)
make deb             # .deb package (nfpm must be installed)
make clean
```

## Quickstart (when implemented)

```sh
sudo apt install ./build/apt-cacher-ultra_*.deb
sudo systemctl enable --now apt-cacher-ultra
```

Point clients at it as a proxy (matches existing apt-cacher-ng deployments):

```
# /etc/apt/apt.conf.d/00aptcacher
Acquire::http::Proxy "http://cache:3142";
```
