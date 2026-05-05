.PHONY: build test test-race lint fmt deb clean

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

clean:
	rm -rf $(BUILD_DIR)
