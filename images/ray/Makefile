# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE    := ray
PYTHON_VERSION ?= 3.12
RAY_VERSION    ?= 2.58.0
CUDA_VERSION   ?= 13.0
python_base_digest_3.12 := sha256:9343005b72a7cc34847dcb8d09bb8a0c145311eff88c151c60d9c4904edaf6e9
python_base_digest = $(python_base_digest_$(1))
override PYTHON_BASE_DIGEST := $(call python_base_digest,$(PYTHON_VERSION))
ifeq ($(PYTHON_BASE_DIGEST),)
$(error no pinned Azure Linux Python digest for PYTHON_VERSION=$(PYTHON_VERSION))
endif
GIT_SHA        ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
TAG            ?= py$(PYTHON_VERSION)-ray$(RAY_VERSION)-cuda$(CUDA_VERSION)-$(GIT_SHA)
SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
ACR_REGISTRY ?=
PUSH_IMG ?= $(ACR_REGISTRY)/unlisted/aks/ai-runtime/$(IMAGE):$(TAG)

PLATFORMS ?= linux/amd64,linux/arm64
ARCH ?=

.PHONY: all docker-build docker-push docker-push-arch docker-push-manifest test clean

all: docker-build test docker-push

docker-build:
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg PYTHON_BASE_DIGEST=$(PYTHON_BASE_DIGEST) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--tag $(IMG) \
		--load \
		.

comma := ,
CACHE_REPO ?=
CACHE_ARGS := $(if $(CACHE_REPO),--cache-from type=registry$(comma)ref=$(CACHE_REPO):cache --cache-to type=registry$(comma)ref=$(CACHE_REPO):cache$(comma)mode=max)

docker-push:
	@test -n "$(ACR_REGISTRY)" || { echo "ACR_REGISTRY is required for producer-side pushes" >&2; exit 1; }
	docker buildx build \
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg PYTHON_BASE_DIGEST=$(PYTHON_BASE_DIGEST) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
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
		--build-arg PYTHON_VERSION=$(PYTHON_VERSION) \
		--build-arg PYTHON_BASE_DIGEST=$(PYTHON_BASE_DIGEST) \
		--build-arg RAY_VERSION=$(RAY_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
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
	@echo "=== Verifying CA bundle configuration ==="
	@docker run --rm $(IMG) python3 -c "import certifi, os, ssl; p=[os.environ.get('SSL_CERT_FILE'), os.environ.get('CURL_CA_BUNDLE'), os.environ.get('PIP_CERT')]; assert all(p) and len(set(p)) == 1, p; [ssl.create_default_context(cafile=x) for x in (p[0], certifi.where())]; print('OK', p, certifi.where())" 2>&1 && \
		echo "PASS: system and Python CA bundles are configured and loadable" || \
		{ echo "FAIL: CA bundle configuration validation failed"; exit 1; }
	@echo "=== Verifying affected CA roots are absent ==="
	@docker run --rm $(IMG) sh -c 'set -e; \
		for bundle in "$$SSL_CERT_FILE" "$$(python3 -m certifi)"; do \
			subjects=$$(openssl crl2pkcs7 -nocrl -certfile "$$bundle" | openssl pkcs7 -print_certs -noout); \
			if printf "%s\n" "$$subjects" | grep -Eq "AAA Certificate Services|emSign ECC Root CA - G3|emSign Root TLS CA - G3"; then \
				echo "affected CA root present in $$bundle" >&2; exit 1; \
			fi; \
		done' && \
		echo "PASS: system and Python CA bundles exclude the affected roots" || \
		{ echo "FAIL: affected CA root validation failed"; exit 1; }

clean:
	docker rmi $(IMG) 2>/dev/null || true
