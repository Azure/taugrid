# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY   := gpu-metrics-collector
GODEBUG_LDFLAG := -X=runtime.godebugDefault=ms_tlsmldsacert=0
GOLANGCI_LINT_VERSION := v2.14.0

.PHONY: build test lint clean

build:
	CGO_ENABLED=0 go build -ldflags "$(GODEBUG_LDFLAG)" -o bin/$(BINARY) ./cmd/collector

test:
	go test -race -count=1 ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

clean:
	rm -rf bin/
