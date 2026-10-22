# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY   := gpu-health-checker
MODULE   := github.com/Azure/taugrid/monitoring/gpu-health-checker
GOARCH   ?= arm64
GOFLAGS  := -trimpath
LDFLAGS  := -s -w -X=runtime.godebugDefault=ms_tlsmldsacert=0
CGO      := 1
GOLANGCI_LINT_VERSION := v2.14.0

ifeq ($(GOARCH),arm64)
  export CC := aarch64-linux-gnu-gcc
endif

.PHONY: build clean test lint

build:
	CGO_ENABLED=$(CGO) GOARCH=$(GOARCH) CC=$(CC) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/gpu-health-checker

test:
	CGO_ENABLED=$(CGO) go test ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

clean:
	rm -rf bin/
