# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := tau-core-controller
TAG   ?= latest

ACR_REGISTRY ?= aksairuntime.azurecr.io
IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64

SRC := ../../controllers/tau-core
CONTEXT := ../..

.PHONY: build test test-kind-e2e lint generate manifests clean docker-build docker-push

comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

build:
	$(MAKE) -C $(SRC) build

test:
	$(MAKE) -C $(SRC) test

test-kind-e2e:
	$(MAKE) -C $(SRC) test-kind-e2e

lint:
	$(MAKE) -C $(SRC) lint

generate:
	$(MAKE) -C $(SRC) generate

manifests:
	$(MAKE) -C $(SRC) manifests

clean:
	$(MAKE) -C $(SRC) clean

docker-build:
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		$(CONTEXT)

docker-push:
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		--push \
		$(CONTEXT)
