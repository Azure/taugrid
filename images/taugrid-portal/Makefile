BINARY := taugrid-portal
IMAGE  := taugrid-portal
TAG    ?= latest

VERSION ?= $(TAG)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

ACR_REGISTRY ?= aksairuntime.azurecr.io
IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build test lint docker-build docker-push clean

# Registry-based BuildKit cache (set CACHE_REPO to enable)
comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

build:
	$(MAKE) -C ../../portal build

test:
	$(MAKE) -C ../../portal test

lint:
	$(MAKE) -C ../../portal lint

docker-build:
	docker buildx build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		$(CACHE_ARGS) \
		--file Dockerfile \
		--push \
		../..

docker-push: docker-build

clean:
	$(MAKE) -C ../../portal clean
