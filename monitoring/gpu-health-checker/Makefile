BINARY   := gpu-health-checker
MODULE   := github.com/Azure/taugrid/monitoring/gpu-health-checker
GOARCH   ?= arm64
GOFLAGS  := -trimpath
LDFLAGS  := -s -w
CGO      := 1

# Cross-compile CC selection: use aarch64 cross-compiler when targeting arm64 from amd64.
ifeq ($(GOARCH),arm64)
  export CC := aarch64-linux-gnu-gcc
endif

.PHONY: build clean test lint

build:
	CGO_ENABLED=$(CGO) GOARCH=$(GOARCH) CC=$(CC) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/gpu-health-checker

test:
	CGO_ENABLED=$(CGO) go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
