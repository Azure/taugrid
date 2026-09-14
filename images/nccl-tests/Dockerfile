# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/azureml/openmpi5.0-cuda12.4-ubuntu22.04:20260908.v1@sha256:0ba9e648604a241c92a45a60056866b5ff02d1d7ddb23478d25a62b41b7b11a0

ARG NCCL_TESTS_VERSION
ARG NCCL_TESTS_COMMIT
ARG NCCL_TESTS_SHA256
ARG RDMA_CORE_VERSION
ARG CUDA_VERSION
ARG OPENMPI_VERSION
ARG NCCL_VERSION
ARG SOURCE_REVISION=dev

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV LD_LIBRARY_PATH="/usr/local/openmpi/lib:/usr/local/openmpi/lib64:${LD_LIBRARY_PATH}"

RUN test -n "${NCCL_TESTS_VERSION}" \
    && test -n "${NCCL_TESTS_COMMIT}" \
    && test -n "${NCCL_TESTS_SHA256}" \
    && test -n "${RDMA_CORE_VERSION}" \
    && test -n "${CUDA_VERSION}" \
    && test -n "${OPENMPI_VERSION}" \
    && test -n "${NCCL_VERSION}" \
    && nvcc --version | grep -F "release ${CUDA_VERSION%.*}" \
    && mpirun --version | grep -F "Open MPI) ${OPENMPI_VERSION}" \
    && test "$(dpkg-query -W -f='${Version}' libnccl2)" = "${NCCL_VERSION}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        "rdma-core=${RDMA_CORE_VERSION}" \
        "ibverbs-providers=${RDMA_CORE_VERSION}" \
        "libibverbs1=${RDMA_CORE_VERSION}" \
        "libibverbs-dev=${RDMA_CORE_VERSION}" \
        "librdmacm1=${RDMA_CORE_VERSION}" \
        "librdmacm-dev=${RDMA_CORE_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt
RUN curl -fsSL \
        "https://github.com/NVIDIA/nccl-tests/archive/${NCCL_TESTS_COMMIT}.tar.gz" \
        -o nccl-tests.tar.gz \
    && echo "${NCCL_TESTS_SHA256}  nccl-tests.tar.gz" | sha256sum -c - \
    && tar -xzf nccl-tests.tar.gz \
    && mv "nccl-tests-${NCCL_TESTS_COMMIT}" nccl-tests \
    && rm nccl-tests.tar.gz \
    && make -C /opt/nccl-tests -j"$(nproc)" \
        MPI=1 \
        NAME_SUFFIX=_mpi \
        MPI_HOME=/usr/local/openmpi \
        CUDA_HOME=/usr/local/cuda \
        NCCL_HOME=/usr/local/cuda \
    && test -x /opt/nccl-tests/build/all_reduce_perf_mpi \
    && install -D -m 0644 /opt/nccl-tests/LICENSE.txt \
        /usr/share/licenses/nccl-tests/LICENSE.txt \
    && printf '%s\n' "${NCCL_TESTS_VERSION}" >/opt/nccl-tests/VERSION

COPY images/nccl-tests/worker-entrypoint.sh /usr/local/bin/nccl-tests-worker

RUN chmod 0755 /usr/local/bin/nccl-tests-worker \
    && mkdir -p /run/sshd \
    && rm -f /etc/ssh/ssh_host_* \
    && printf '%s\n' \
        'PermitRootLogin prohibit-password' \
        'PasswordAuthentication no' \
        'KbdInteractiveAuthentication no' \
        'UsePAM no' \
        'X11Forwarding no' \
        'AllowTcpForwarding no' \
        'GatewayPorts no' \
        'PermitTunnel no' \
        'PidFile /run/sshd/sshd.pid' \
        >/etc/ssh/sshd_config.d/nccl-tests.conf

LABEL org.opencontainers.image.title="TauGrid NCCL/RDMA diagnostic"
LABEL org.opencontainers.image.description="MPI-enabled nccl-tests image for manually gated TauGrid e2e diagnostics"
LABEL org.opencontainers.image.source="https://github.com/Azure/taugrid"
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}"
LABEL org.opencontainers.image.licenses="MIT AND BSD-3-Clause"
LABEL org.opencontainers.image.version="${NCCL_TESTS_VERSION}"

ENTRYPOINT ["/usr/local/bin/nccl-tests-worker"]
