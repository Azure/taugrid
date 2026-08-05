IMAGE := gpu-metrics-collector
TAG   ?= latest

ACR_REGISTRY ?= aksmcrimagescommon.azurecr.io
IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)
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
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--file Dockerfile \
		--push \
		$(SRC)
