# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

PYTHON_VERSION ?= 3.12
RAY_VERSION ?= 2.58.0
CUDA_VERSION ?= 13.0

ifneq ($(PYTHON_VERSION)/$(RAY_VERSION)/$(CUDA_VERSION),3.12/2.58.0/13.0)
$(error ray-torch requires PYTHON_VERSION=3.12 RAY_VERSION=2.58.0 CUDA_VERSION=13.0)
endif

GIT_SHA ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
TAG ?= py$(PYTHON_VERSION)-ray$(RAY_VERSION)-cuda$(CUDA_VERSION)-torch2.7.1-cu128-$(GIT_SHA)
SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)
IMG ?= mcr.microsoft.com/aks/ai-runtime/ray:$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/ray:$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64
ARCH ?=
CACHE_REPO ?=
comma := ,
BUILD_ARGS = --file Dockerfile --build-arg SOURCE_REVISION=$(SOURCE_REVISION)
CACHE_ARGS = $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

.PHONY: all docker-build docker-push docker-push-arch docker-push-manifest test test-contract test-torch test-gpu clean

all: docker-build test docker-push

docker-build:
	docker buildx build $(BUILD_ARGS) --tag $(IMG) --load ../..

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build $(BUILD_ARGS) --platform $(PLATFORMS) --tag $(PUSH_IMG) $(CACHE_ARGS) --push ../..

docker-push-arch: CACHE_ARGS = $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache-$(ARCH) --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache-$(ARCH)$(comma)mode=max)
docker-push-arch:
	@case "$(ARCH)" in amd64|arm64) ;; *) echo "ARCH must be amd64 or arm64" >&2; exit 1 ;; esac
	@test "$(origin IMG)" = "command line" -o "$(origin IMG)" = "environment" || \
		{ echo "IMG must be set explicitly" >&2; exit 1; }
	docker buildx build $(BUILD_ARGS) --platform linux/$(ARCH) --tag $(IMG)-$(ARCH) $(CACHE_ARGS) --push ../..

docker-push-manifest:
	@test "$(origin IMG)" = "command line" -o "$(origin IMG)" = "environment" || \
		{ echo "IMG must be set explicitly" >&2; exit 1; }
	docker buildx imagetools create --tag $(IMG) $(IMG)-amd64 $(IMG)-arm64

test: test-contract test-torch
	$(MAKE) -C ../ray test PYTHON_VERSION=$(PYTHON_VERSION) RAY_VERSION=$(RAY_VERSION) CUDA_VERSION=$(CUDA_VERSION) IMG=$(IMG)

test-contract:
	python3 -m unittest discover -s ../../scripts/images/ray-torch -p 'test_contract.py'

test-torch:
	docker run --rm --network none --cpus=1 --memory=1g $(IMG) python3 /opt/taugrid/ray-torch/check_image.py
	bash ../../scripts/images/ray-torch/test_image.sh $(IMG)

test-gpu:
	docker run --rm --gpus all $(IMG) python3 /opt/taugrid/ray-torch/check_image.py --gpu

clean:
	docker rmi $(IMG)
