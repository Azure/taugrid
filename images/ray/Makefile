# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE    := ray
PYTHON_VERSION ?= 3.12
RAY_VERSION    ?= 2.56.0
CUDA_VERSION   ?= 13.0
GIT_SHA        ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
TAG            ?= py$(PYTHON_VERSION)-ray$(RAY_VERSION)-cuda$(CUDA_VERSION)-$(GIT_SHA)
# Stamped into the image as the org.opencontainers.image.revision label.
# Full SHA (not GIT_SHA's short form) so consumers can compare it directly
# against `git rev-parse`/`git log --format=%H` output byte-for-byte.
SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64
# Single architecture for docker-push-arch (amd64 or arm64). Unused by the other
# targets — docker-build/docker-push still build for the host arch / $(PLATFORMS).
ARCH ?=

.PHONY: all docker-build docker-push docker-push-arch docker-push-manifest test clean

all: docker-build test docker-push

# Build single-arch image and load into local Docker for testing
docker-build:
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--tag $(IMG) \
		--load \
		.

# Registry-based BuildKit cache (set CACHE_REPO to enable)
comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

# Build multi-arch manifest and push directly to registry. Requires an emulated
# (QEMU) builder for whichever platform isn't native to the calling host — see
# docker-push-arch/docker-push-manifest below for the native, no-emulation split.
docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--platform $(PLATFORMS) \
		--tag $(PUSH_IMG) \
		$(CACHE_ARGS) \
		--push \
		.

# Natively build and push a SINGLE architecture (no QEMU emulation) — intended
# for CI running each arch on its own native runner in parallel. Pushes to
# "$(IMG)-$(ARCH)"; combine the two resulting tags into one multi-arch manifest
# at the canonical $(IMG) tag with docker-push-manifest below. Unlike
# docker-push, this takes the fully-qualified destination tag via IMG (not
# ACR_REGISTRY/PUSH_IMG), matching how docker-build/test already use IMG.
docker-push-arch:
	@case "$(ARCH)" in \
		amd64|arm64) ;; \
		*) echo "ARCH must be amd64 or arm64 (got '$(ARCH)')" >&2; exit 1 ;; \
	esac
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--platform linux/$(ARCH) \
		--tag $(IMG)-$(ARCH) \
		$(CACHE_ARGS) \
		--push \
		.

# Stitch the per-arch images already pushed by docker-push-arch (amd64 + arm64)
# into one multi-arch manifest list at the canonical $(IMG) tag. Pure registry
# metadata operation — no build, no emulation, can run on any runner/arch.
docker-push-manifest:
	docker buildx imagetools create \
		--tag $(IMG) \
		$(IMG)-amd64 \
		$(IMG)-arm64

test:
	@echo "=== Verifying Python version ==="
	@actual_python=$$(docker run --rm $(IMG) python3 --version 2>&1 | awk '{print $$2}'); \
	if echo "$$actual_python" | grep -q "^$(PYTHON_VERSION)"; then \
		echo "PASS: Python version $$actual_python matches expected $(PYTHON_VERSION)"; \
	else \
		echo "FAIL: Python version $$actual_python does not match expected $(PYTHON_VERSION)"; \
		exit 1; \
	fi
	@echo "=== Verifying Ray version ==="
	@actual_ray=$$(docker run --rm $(IMG) python3 -c "import ray; print(ray.__version__)" 2>&1); \
	if [ "$$actual_ray" = "$(RAY_VERSION)" ]; then \
		echo "PASS: Ray version $$actual_ray matches expected $(RAY_VERSION)"; \
	else \
		echo "FAIL: Ray version $$actual_ray does not match expected $(RAY_VERSION)"; \
		exit 1; \
	fi
	@echo "=== Verifying ray[default] ==="
	@docker run --rm $(IMG) python3 -c "import ray.dashboard" 2>&1 && \
		echo "PASS: ray[default] - dashboard importable" || \
		{ echo "FAIL: ray[default] - could not import ray.dashboard"; exit 1; }
	@echo "=== Verifying ray[data] ==="
	@docker run --rm $(IMG) python3 -c "import ray.data" 2>&1 && \
		echo "PASS: ray[data] - ray.data importable" || \
		{ echo "FAIL: ray[data] - could not import ray.data"; exit 1; }
	@echo "=== Verifying ray[serve] ==="
	@docker run --rm $(IMG) python3 -c "import ray.serve" 2>&1 && \
		echo "PASS: ray[serve] - ray.serve importable" || \
		{ echo "FAIL: ray[serve] - could not import ray.serve"; exit 1; }
	@echo "=== Verifying wget is GNU Wget 1.x (not wget2) ==="
	@docker run --rm $(IMG) wget --version 2>&1 | grep -q "^GNU Wget " && \
		echo "PASS: GNU Wget 1.x is installed" || \
		{ echo "FAIL: expected GNU Wget 1.x, not wget2 (wget2 breaks KubeRay health probes)"; exit 1; }
	@echo "=== Verifying RDMA userspace libraries ==="
	@docker run --rm $(IMG) python3 -c \
		"import ctypes.util; missing=[l for l in ('ibverbs','rdmacm','mlx5') if not ctypes.util.find_library(l)]; exit(1) if missing else None" 2>&1 && \
		echo "PASS: RDMA userspace libraries (ibverbs, rdmacm, mlx5) are loadable" || \
		{ echo "FAIL: RDMA userspace libraries not found — NCCL will fall back to TCP on IB nodes"; exit 1; }
	@echo "=== Verifying NCCL ==="
	@docker run --rm $(IMG) python3 -c "import ctypes.util; exit(0 if ctypes.util.find_library('nccl') else 1)" 2>&1 && \
		echo "PASS: NCCL (libnccl) is loadable" || \
		{ echo "FAIL: NCCL not found — multi-GPU collective communications will not work"; exit 1; }

clean:
	docker rmi $(IMG) 2>/dev/null || true
