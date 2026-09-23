# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY := experiment-metrics-collector
PKG := ./cmd/experiment-metrics-collector
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
VERSION_PKG := github.com/Azure/taugrid/core/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)
BUILD_FLAGS := -trimpath -mod=readonly -buildvcs=false
STATICCHECK_VERSION := v0.7.0

.PHONY: build install test lint clean

build:
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

install:
	CGO_ENABLED=0 go install $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" $(PKG)

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read -r _)
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

clean:
	rm -rf bin/
