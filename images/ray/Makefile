# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE    := ray
PYTHON_VERSION ?= 3.12
RAY_VERSION    ?= 2.56.0
CUDA_VERSION   ?= 13.0
GIT_SHA        ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
TAG            ?= py$(PYTHON_VERSION)-ray$(RAY_VERSION)-cuda$(CUDA_VERSION)-$(GIT_SHA)

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: all docker-build docker-push test clean

all: docker-build test docker-push

# Build single-arch image and load into local Docker for testing
docker-build:
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--tag $(IMG) \
		--load \
		.

# Registry-based BuildKit cache (set CACHE_REPO to enable)
comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

# Build multi-arch manifest and push directly to registry
docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--platform $(PLATFORMS) \
		--tag $(PUSH_IMG) \
		$(CACHE_ARGS) \
		--push \
		.

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
