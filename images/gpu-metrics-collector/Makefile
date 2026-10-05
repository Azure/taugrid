# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := gpu-metrics-collector
TAG   ?= dev

ACR_REGISTRY ?=
IMG ?= $(if $(ACR_REGISTRY),$(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG),$(IMAGE):$(TAG))
PLATFORMS ?= linux/amd64,linux/arm64

# Source lives in the module directory; this directory only packages it.
SRC := ../../monitoring/gpu-metrics-collector
CONTEXT := ../..

.PHONY: build test lint clean docker-build docker-push

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
		--file Dockerfile \
		$(CONTEXT)

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	@test "$(TAG)" != "dev" && test "$(TAG)" != "latest" || { echo "TAG must be an immutable release or source tag, not $(TAG)" >&2; exit 1; }
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--file Dockerfile \
		--push \
		$(CONTEXT)
