# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY := tau-core-controller

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0

.PHONY: build test test-kind-e2e lint generate manifests clean

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/controller

test:
	go test -race -count=1 ./...

test-kind-e2e:
	./scripts/kind-e2e.sh

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read -r _)

generate:
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: generate
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

clean:
	rm -rf bin/
