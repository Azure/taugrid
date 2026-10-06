# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# AdxMon image build targets
# Build adx-mon component images using MSFT Go + FIPS (GOEXPERIMENT=systemcrypto)
#
# Usage:
#   make docker-build-collector ADX_MON_VERSION=main
#   make docker-push-collector PUBLISH_REGISTRY=<backing-registry>/public/aks/ai-runtime/adx-mon
#   make docker-build           ADX_MON_VERSION=v0.2.0
#   make push PUBLISH_REGISTRY=<backing-registry>/public/aks/ai-runtime/adx-mon TAG=v0.3.0

REGISTRY         ?= mcr.microsoft.com/aks/ai-runtime/adx-mon
PUBLISH_REGISTRY ?=
TAG              ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
ADX_MON_VERSION  ?= main
COMPONENTS       := collector ingestor alerter operator
SINGLE_PLATFORM_COMPONENTS := ingestor alerter operator
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: docker-build push docker-build-collector docker-push-collector docker-build-ingestor docker-build-alerter docker-build-operator

# Build the collector for the developer machine's native platform.
docker-build-collector:
	docker build -f Dockerfile.collector \
	  --build-arg ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  -t $(REGISTRY)/collector:$(TAG) \
	  .

# Build and push one collector manifest containing every requested platform.
docker-push-collector:
	@test -n "$(PUBLISH_REGISTRY)" || { echo "PUBLISH_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build -f Dockerfile.collector \
	  --build-arg ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  --platform $(PLATFORMS) \
	  --tag $(PUBLISH_REGISTRY)/collector:$(TAG) \
	  --push \
	  .

# The other components remain amd64-only.
define build_target
docker-build-$(1):
	docker build -f Dockerfile.$(1) \
	  --build-arg ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  --platform linux/amd64 \
	  -t $(REGISTRY)/$(1):$(TAG) \
	  .
.PHONY: docker-build-$(1)
endef

$(foreach comp,$(SINGLE_PLATFORM_COMPONENTS),$(eval $(call build_target,$(comp))))

# Build all 4 images
docker-build: $(addprefix docker-build-,$(COMPONENTS))

# Push all 4 images. The collector is a multi-platform manifest; the other
# components retain their existing single-platform behavior.
push: $(addprefix docker-build-,$(SINGLE_PLATFORM_COMPONENTS))
	@test -n "$(PUBLISH_REGISTRY)" || { echo "PUBLISH_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	$(MAKE) docker-push-collector \
	  PUBLISH_REGISTRY=$(PUBLISH_REGISTRY) \
	  TAG=$(TAG) \
	  ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  PLATFORMS=$(PLATFORMS)
	@for comp in $(SINGLE_PLATFORM_COMPONENTS); do \
	  echo "Pushing $(PUBLISH_REGISTRY)/$$comp:$(TAG)"; \
	  docker tag $(REGISTRY)/$$comp:$(TAG) $(PUBLISH_REGISTRY)/$$comp:$(TAG); \
	  docker push $(PUBLISH_REGISTRY)/$$comp:$(TAG); \
	done
