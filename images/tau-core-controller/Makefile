# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := tau-core-controller
TAG   ?= latest

ACR_REGISTRY ?= aksairuntime.azurecr.io
IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64

# Source lives in the module directory; this directory only packages it.
SRC := ../../controllers/tau-core

.PHONY: build test test-kind-e2e lint generate manifests clean docker-build docker-push

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
		--file Dockerfile \
		$(SRC)

docker-push:
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--file Dockerfile \
		--push \
		$(SRC)
