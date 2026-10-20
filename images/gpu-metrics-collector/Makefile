# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := gpu-metrics-collector
TAG   ?= dev

ACR_REGISTRY ?=
IMG ?= $(if $(ACR_REGISTRY),$(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG),$(IMAGE):$(TAG))
PUSH_IMG ?= $(IMG)
PLATFORMS ?= linux/amd64,linux/arm64

SRC := ../../monitoring/gpu-metrics-collector
CONTEXT := ../..

.PHONY: build test lint clean docker-build docker-push

comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

build:
	$(MAKE) -C $(SRC) build

test:
	$(MAKE) -C $(SRC) test

lint:
	$(MAKE) -C $(SRC) lint

clean:
	$(MAKE) -C $(SRC) clean

docker-build:
	docker buildx build \
		--load \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		$(CONTEXT)

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	@test "$(TAG)" != "dev" && test "$(TAG)" != "latest" || { echo "TAG must be an immutable release or source tag, not $(TAG)" >&2; exit 1; }
	@case "$(PUSH_IMG)" in *:"$(TAG)") ;; *) echo "PUSH_IMG must end with :$(TAG)" >&2; exit 1 ;; esac
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(PUSH_IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		--push \
		$(CONTEXT)
