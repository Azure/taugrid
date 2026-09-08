# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

.PHONY: build install test test-stellar-adx-e2e lint clean

BINARY := taugrid-portal
PKG := ./cmd/taugrid-portal
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
VERSION_PKG := github.com/Azure/taugrid/core/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)
BUILD_FLAGS := -trimpath -mod=readonly -buildvcs=false

# Pinned so a staticcheck release cannot fail CI on an unrelated change. Bump it
# deliberately, in a PR that also fixes whatever the new version finds.
STATICCHECK_VERSION := v0.7.0

build:
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

install:
	go install $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" $(PKG)

test:
	go test ./...


lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read -r _)
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

clean:
	rm -rf bin/
