.PHONY: build test test-race lint shellcheck fmt deb e2e e2e-flat e2e-live deb-test apt-repo-test clean

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

# Keep the distributed binary independent of the build host's libc/loader.
# Scope this to the build: the race-enabled test target needs cgo.
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BIN) ./cmd/apt-cacher-ultra

test:
	$(GO) test ./...

# CI's timeout. -timeout bounds each package's test binary, and
# internal/freshness alone takes close to 5 minutes under -race on 4 CPUs.
test-race:
	$(GO) test -race -timeout 10m ./...

# CI's lint job: golangci-lint, also over the e2ehooks build (e2e/flat's
# test seam, cmd/apt-cacher-ultra/e2ehooks.go, compiles only with that tag),
# and ShellCheck.
lint: shellcheck
	golangci-lint run ./...
	golangci-lint run --build-tags e2ehooks ./cmd/...

# ShellCheck over the e2e harnesses and the package's maintainer scripts,
# from its pinned image (needs docker): ShellCheck versions differ in what
# they flag (0.9.0, GitHub's ubuntu-latest, reports an SC2015 that 0.11.0
# does not), so CI's lint job and every checkout run this one version.
SHELLCHECK_IMAGE := koalaman/shellcheck:v0.11.0
SHELL_SCRIPTS    := e2e/flat/*.sh e2e/live/*.sh e2e/deb/*.sh packaging/scripts/*.sh

shellcheck:
	docker run --rm -v "$(CURDIR):/mnt:ro" -w /mnt $(SHELLCHECK_IMAGE) -x $(SHELL_SCRIPTS)

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

# Spec §7 real-apt end-to-end test of flat repositories: nginx, the daemon
# and apt clients in one container per client image (e2e/flat/README.md).
# Default images: debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13
# ubuntu:26.04, about 2.5 minutes each plus the build; debian:11 and
# ubuntu:26.04 also wait for the unsigned confirmation, which lands 5 to 15
# minutes after first contact (about 6 to 7 minutes each in all, about 14
# when a third retry is needed). Override with E2E_FLAT_IMAGES (and
# E2E_FLAT_UNSIGNED_IMAGES); give each checkout its own E2E_FLAT_NAME (the
# image and container name prefix) so concurrent runs do not collide.
e2e-flat:
	bash e2e/flat/run.sh

# The manual live-upstream smoke (e2e/live/README.md): a release-built
# daemon and real apt clients against pkgs.k8s.io, NVIDIA CUDA,
# libnvidia-container, pkg.jenkins.io and download.sublimetext.com, with
# adoption and strict mode on; about 5 minutes. It needs network access to
# those five vendors, so it is run by hand before 1.1.0 and before later
# releases that change the request path, adoption or vouching, never in CI.
# E2E_LIVE_NAME names its image and container (default acu-e2e-live); give
# each checkout its own E2E_LIVE_NAME so concurrent runs do not collide (a
# run removes a container of its name first, a running one too).
e2e-live:
	bash e2e/live/run.sh

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
