# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

IMAGE := nccl-tests
VERSIONS_FILE := $(CURDIR)/versions.json
json_value = $(shell python3 -c 'import functools, json, sys; print(functools.reduce(lambda value, key: value[key], sys.argv[2].split("."), json.load(open(sys.argv[1]))))' "$(VERSIONS_FILE)" "$(1)")

TAG := $(call json_value,imageTag)
SOURCE_REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)
NCCL_TESTS_VERSION := $(call json_value,ncclTests.version)
NCCL_TESTS_COMMIT := $(call json_value,ncclTests.commit)
NCCL_TESTS_SHA256 := $(call json_value,ncclTests.archiveSha256)
RDMA_CORE_VERSION := $(call json_value,rdmaCore)
CUDA_VERSION := $(call json_value,cuda)
OPENMPI_VERSION := $(call json_value,openmpi)
NCCL_VERSION := $(call json_value,nccl)

IMG ?= mcr.microsoft.com/aks/ai-runtime/$(IMAGE):$(TAG)
PLATFORMS := $(call json_value,architecture)

.PHONY: docker-build docker-push print-build-args print-platforms test test-image test-ssh clean

print-build-args:
	@printf '%s\n' \
		"NCCL_TESTS_VERSION=$(NCCL_TESTS_VERSION)" \
		"NCCL_TESTS_COMMIT=$(NCCL_TESTS_COMMIT)" \
		"NCCL_TESTS_SHA256=$(NCCL_TESTS_SHA256)" \
		"RDMA_CORE_VERSION=$(RDMA_CORE_VERSION)" \
		"CUDA_VERSION=$(CUDA_VERSION)" \
		"OPENMPI_VERSION=$(OPENMPI_VERSION)" \
		"NCCL_VERSION=$(NCCL_VERSION)" \
		"SOURCE_REVISION=$(SOURCE_REVISION)"

print-platforms:
	@printf '%s\n' "$(PLATFORMS)"

docker-build:
	docker buildx build \
		--file Dockerfile \
		--build-arg NCCL_TESTS_VERSION=$(NCCL_TESTS_VERSION) \
		--build-arg NCCL_TESTS_COMMIT=$(NCCL_TESTS_COMMIT) \
		--build-arg NCCL_TESTS_SHA256=$(NCCL_TESTS_SHA256) \
		--build-arg RDMA_CORE_VERSION=$(RDMA_CORE_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg OPENMPI_VERSION=$(OPENMPI_VERSION) \
		--build-arg NCCL_VERSION=$(NCCL_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--load \
		../..

docker-push:
	docker buildx build \
		--file Dockerfile \
		--build-arg NCCL_TESTS_VERSION=$(NCCL_TESTS_VERSION) \
		--build-arg NCCL_TESTS_COMMIT=$(NCCL_TESTS_COMMIT) \
		--build-arg NCCL_TESTS_SHA256=$(NCCL_TESTS_SHA256) \
		--build-arg RDMA_CORE_VERSION=$(RDMA_CORE_VERSION) \
		--build-arg CUDA_VERSION=$(CUDA_VERSION) \
		--build-arg OPENMPI_VERSION=$(OPENMPI_VERSION) \
		--build-arg NCCL_VERSION=$(NCCL_VERSION) \
		--build-arg SOURCE_REVISION=$(SOURCE_REVISION) \
		--platform $(PLATFORMS) \
		--tag $(IMG) \
		--push \
		../..

test: test-image test-ssh

test-image:
	docker run --rm --entrypoint bash $(IMG) -lc \
		'test "$$(cat /opt/nccl-tests/VERSION)" = "$(NCCL_TESTS_VERSION)" && \
		 test -x /opt/nccl-tests/build/all_reduce_perf_mpi && \
		 /opt/nccl-tests/build/all_reduce_perf_mpi -h >/dev/null && \
		 mpirun --version | grep -q "Open MPI) $(OPENMPI_VERSION)" && \
		 nvcc --version | grep -q "release 12.4" && \
		 test "$$(dpkg-query -W libnccl2 | cut -f2)" = "$(NCCL_VERSION)" && \
		 test "$$(dpkg-query -W rdma-core | cut -f2)" = "$(RDMA_CORE_VERSION)" && \
		 ldconfig -p | grep -q libibverbs && \
		 compgen -G "/usr/lib/x86_64-linux-gnu/libibverbs/libmlx5-rdmav*.so" >/dev/null'

test-ssh:
	./smoke-ssh.sh "$(IMG)"

clean:
	docker rmi $(IMG) 2>/dev/null || true
