# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

REGISTRY         ?= mcr.microsoft.com/aks/ai-runtime/adx-mon
PUBLISH_REGISTRY ?=
TAG              ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
ADX_MON_VERSION  ?= main
COMPONENTS       := collector ingestor alerter operator
SINGLE_PLATFORM_COMPONENTS := ingestor alerter operator
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: docker-build push docker-build-collector docker-push-collector docker-build-ingestor docker-build-alerter docker-build-operator

docker-build-collector:
	docker build -f Dockerfile.collector \
	  --build-arg ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  -t $(REGISTRY)/collector:$(TAG) \
	  .

docker-push-collector:
	@test -n "$(PUBLISH_REGISTRY)" || { echo "PUBLISH_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build -f Dockerfile.collector \
	  --build-arg ADX_MON_VERSION=$(ADX_MON_VERSION) \
	  --platform $(PLATFORMS) \
	  --tag $(PUBLISH_REGISTRY)/collector:$(TAG) \
	  --push \
	  .

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

docker-build: $(addprefix docker-build-,$(COMPONENTS))

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
