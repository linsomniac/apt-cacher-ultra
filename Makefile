.PHONY: build test test-race lint fmt deb e2e deb-test apt-repo-test clean

GO        ?= go
BIN       := apt-cacher-ultra
BUILD_DIR := build
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.Version=$(VERSION)

# Debian policy requires upstream_version to start with a digit, so a
# bare git short-hash like "cc679df" is rejected. Strip leading non-
# digit characters for the .deb build only; binary/main.Version still
# carries the full descriptor for diagnostics. If stripping leaves
# nothing (e.g. an all-hex-letters hash, or an untagged repo where
# `git describe` printed "dev"), fall back to "0".
DEB_VERSION := $(shell echo '$(VERSION)' | sed -E -e 's/^[^0-9]+//' -e 's/^$$/0/')

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BIN) ./cmd/apt-cacher-ultra

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -timeout 5m ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .
	$(GO) mod tidy

deb: build
	VERSION=$(DEB_VERSION) nfpm pkg --packager deb --config packaging/nfpm.yaml --target $(BUILD_DIR)/

# SPEC §12.4 docker-compose end-to-end test: real apt against a mock
# upstream, through apt-cacher-ultra. Slower CI lane; needs a working
# `docker` + `docker compose`.
e2e:
	bash e2e/run.sh

# SPEC §14 step 3 .deb install validation: build the .deb in a
# golang stage, then install on each Ubuntu LTS target and verify
# the package contract. Default matrix is 24.04 + 26.04; override
# with UBUNTU_VERSIONS for development.
deb-test:
	bash e2e/deb/run.sh

# Fast, docker-free unit tests for the apt-repo publishing scripts
# (scripts/select-stable-tags.sh, scripts/build-apt-repo.sh). The
# containerized signature/install gate lives in
# scripts/smoke-test-apt-repo.sh and runs in CI.
apt-repo-test:
	bash e2e/apt-repo/test.sh

clean:
	rm -rf $(BUILD_DIR)
