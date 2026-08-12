# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := gpu-metrics-collector
TAG   ?= latest

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64

# Source lives in the module directory; this directory only packages it.
SRC := ../../monitoring/gpu-metrics-collector

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
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--file Dockerfile \
		$(SRC)

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(PUSH_IMG) \
		--file Dockerfile \
		--push \
		$(SRC)
