# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY   := gpu-metrics-collector

.PHONY: build test lint clean

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/collector

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
