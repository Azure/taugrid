# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE    := ray
PYTHON_VERSION ?= 3.12
RAY_VERSION    ?= 2.58.0
CUDA_VERSION   ?= 13.0
CA_CERTIFICATES_MIN_VERSION ?= 3.0.0-19.azl3
CERTIFI_MIN_VERSION ?= 2026.10.19
GIT_SHA        ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
TAG            ?= py$(PYTHON_VERSION)-ray$(RAY_VERSION)-cuda$(CUDA_VERSION)-$(GIT_SHA)
SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64
ARCH ?=

BUILD_ARGS = \
	--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
	--build-arg RAY_VERSION=$(RAY_VERSION) \
	--build-arg CUDA_VERSION=$(CUDA_VERSION) \
	--build-arg CA_CERTIFICATES_MIN_VERSION=$(CA_CERTIFICATES_MIN_VERSION) \
	--build-arg CERTIFI_MIN_VERSION=$(CERTIFI_MIN_VERSION) \
	--build-arg SOURCE_REVISION=$(SOURCE_REVISION)

.PHONY: all docker-build docker-push docker-push-arch docker-push-manifest test clean

all: docker-build test docker-push

docker-build:
	docker buildx build \
		--pull \
		$(BUILD_ARGS) \
		--tag $(IMG) \
		--load \
		.

comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build \
		--pull \
		$(BUILD_ARGS) \
		--platform $(PLATFORMS) \
		--tag $(PUSH_IMG) \
		$(CACHE_ARGS) \
		--push \
		.

docker-push-arch: CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache-$(ARCH) --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache-$(ARCH)$(comma)mode=max)
docker-push-arch:
	@case "$(ARCH)" in \
		amd64|arm64) ;; \
		*) echo "ARCH must be amd64 or arm64 (got '$(ARCH)')" >&2; exit 1 ;; \
	esac
	@test "$(origin IMG)" = "command line" -o "$(origin IMG)" = "environment" || \
		{ echo "IMG must be set explicitly (e.g. IMG=<registry>/ray:<tag>)" >&2; exit 1; }
	docker buildx build \
		--pull \
		$(BUILD_ARGS) \
		--platform linux/$(ARCH) \
		--tag $(IMG)-$(ARCH) \
		$(CACHE_ARGS) \
		--push \
		.

docker-push-manifest:
	@test "$(origin IMG)" = "command line" -o "$(origin IMG)" = "environment" || \
		{ echo "IMG must be set explicitly (e.g. IMG=<registry>/ray:<tag>)" >&2; exit 1; }
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
	@echo "=== Verifying Python dependency consistency ==="
	@docker run --rm $(IMG) python3 -m pip check
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
	@echo "=== Verifying CA trust configuration ==="
	@docker run --rm -i $(IMG) python3 - \
		"$(CA_CERTIFICATES_MIN_VERSION)" "$(CERTIFI_MIN_VERSION)" < verify_ca_trust.py 2>&1 && \
		echo "PASS: system and Python trust stores exclude the compromised eMudhra/emSign paths" || \
		{ echo "FAIL: CA trust remediation validation failed"; exit 1; }

clean:
	docker rmi $(IMG) 2>/dev/null || true
