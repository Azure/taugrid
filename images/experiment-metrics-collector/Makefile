# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

BINARY := experiment-metrics-collector
IMAGE  := experiment-metrics-collector
TAG    ?= latest

VERSION ?= $(TAG)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

ACR_REGISTRY ?= aksairuntime.azurecr.io
IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64
SRC := ../../metrics/experiment-metrics-collector
CONTEXT := ../..

.PHONY: build test lint docker-build docker-push clean

comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

build:
	$(MAKE) -C $(SRC) build

test:
	$(MAKE) -C $(SRC) test

lint:
	$(MAKE) -C $(SRC) lint

docker-build:
	docker buildx build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		$(CONTEXT)

docker-push:
	docker buildx build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		--push \
		$(CONTEXT)

clean:
	$(MAKE) -C $(SRC) clean
