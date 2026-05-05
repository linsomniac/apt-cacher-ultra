.PHONY: build test test-race lint fmt deb e2e clean

GO        ?= go
BIN       := apt-cacher-ultra
BUILD_DIR := build
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.Version=$(VERSION)

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
	VERSION=$(VERSION) nfpm pkg --packager deb --config packaging/nfpm.yaml --target $(BUILD_DIR)/

# SPEC §12.4 docker-compose end-to-end test: real apt against a mock
# upstream, through apt-cacher-ultra. Slower CI lane; needs a working
# `docker` + `docker compose`.
e2e:
	bash e2e/run.sh

clean:
	rm -rf $(BUILD_DIR)
