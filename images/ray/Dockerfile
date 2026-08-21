# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG PYTHON_VERSION

# --- Build GNU Wget 1.x from source ---
# Azure Linux 3 ships wget2 as the "wget" package, but wget2 drops the response
# body on stdout when localhost resolves to both ::1 (IPv6) and 127.0.0.1 (IPv4)
# — which is the default in Kubernetes pods. KubeRay exec-based health probes
# use `wget -q -O- <url> | grep success`, so the wget2 bug causes every probe
# to fail. Build GNU Wget 1.x instead.
FROM mcr.microsoft.com/azurelinux/base/core:3.0 AS wget-builder
ARG WGET_VERSION=1.25.0
RUN tdnf install -y gcc glibc-devel kernel-headers binutils make tar \
        openssl-devel ca-certificates gawk && tdnf clean all
RUN curl -sL -o /tmp/wget.tar.gz \
        "https://ftp.gnu.org/gnu/wget/wget-${WGET_VERSION}.tar.gz" \
    && tar xzf /tmp/wget.tar.gz -C /tmp
WORKDIR /tmp/wget-${WGET_VERSION}
RUN ./configure --with-ssl=openssl --disable-nls --quiet \
    && make -j"$(nproc)" --quiet

# --- Final image ---
FROM mcr.microsoft.com/azurelinux/base/python:${PYTHON_VERSION} AS base

ARG RAY_VERSION
ARG CUDA_VERSION

COPY --from=wget-builder /tmp/wget-*/src/wget /usr/bin/wget

# Build tools needed by flashinfer JIT and vLLM extensions at runtime
# RDMA userspace libraries (rdma-core, libibverbs, librdmacm) enable NCCL to
# use InfiniBand transport on IB-capable nodes (e.g. H200/NDR). Without these,
# multi-node GPU workloads silently fall back to TCP over eth0.
RUN tdnf install -y ca-certificates which gcc gcc-c++ binutils glibc-devel kernel-headers \
        ninja-build python3-devel \
        rdma-core libibverbs librdmacm && tdnf clean all

# --- CUDA compiler toolkit from NVIDIA's RHEL9 RPM repository ---
# vLLM >=0.14 depends on flashinfer which JIT-compiles CUDA kernels at runtime.
# Install only the compiler toolchain needed for JIT (nvcc, ptxas, headers,
# nvrtc, nvvm/libdevice) plus libcurand-devel (provides curand.h required by
# flashinfer's sampling kernels). We intentionally use individual packages
# instead of cuda-libraries-devel, which pulls in all CUDA math library -devel
# packages (cublas, cusolver, cusparse, cufft) that are not needed and would
# exceed disk limits on the arm64 build.
#
# NCCL (libnccl, libnccl-devel) is installed separately from the math library
# suite — it is not part of cuda-libraries-devel. NCCL is required for
# multi-GPU collective communications (all-reduce, broadcast) and is the
# library that actually uses the RDMA/IB transport (libibverbs, librdmacm)
# installed above. NCCL RPMs use a different versioning scheme
# (e.g. libnccl-2.28.9-1+cuda13.0) so we install by name and let tdnf
# resolve the latest version available for the repo.
#
# NVIDIA uses "x86_64" for amd64 and "sbsa" (server base system architecture)
# for aarch64 in their repo paths.
# NVIDIA RPM packages use dashes in version suffixes (e.g. cuda-nvcc-13-0),
# so convert the dot-based CUDA_VERSION (13.0) to dash form for tdnf.
RUN CUDA_PKG_VERSION=$(echo "${CUDA_VERSION}" | tr '.' '-') \
    && NVARCH=$(case "$(uname -m)" in x86_64) echo x86_64;; aarch64) echo sbsa;; *) echo unsupported && exit 1;; esac) \
    && printf '[cuda-rhel9]\nname=cuda-rhel9-%s\nbaseurl=https://developer.download.nvidia.com/compute/cuda/repos/rhel9/%s\nenabled=1\ngpgcheck=0\n' \
        "$NVARCH" "$NVARCH" > /etc/yum.repos.d/cuda-rhel9.repo \
    && tdnf install -y \
        cuda-nvcc-${CUDA_PKG_VERSION} \
        cuda-cudart-devel-${CUDA_PKG_VERSION} \
        cuda-driver-devel-${CUDA_PKG_VERSION} \
        cuda-nvrtc-devel-${CUDA_PKG_VERSION} \
        libnvvm-${CUDA_PKG_VERSION} \
        libcurand-devel-${CUDA_PKG_VERSION} \
        libnccl \
        libnccl-devel \
    && rm -f /etc/yum.repos.d/cuda-rhel9.repo \
    && tdnf clean all

ENV PATH="/usr/local/cuda/bin:${PATH}"
ENV CUDA_HOME="/usr/local/cuda"
# cuda-driver-devel provides a libcuda.so stub under lib64/stubs/ for
# build-time linking. At runtime the real libcuda.so is provided by the
# NVIDIA driver mounted into the container. LIBRARY_PATH lets the linker
# find the stub during flashinfer JIT compilation.
ENV LIBRARY_PATH="/usr/local/cuda/lib64/stubs"

RUN pip install --no-cache-dir \
    "ray[default]==${RAY_VERSION}" \
    "ray[data]==${RAY_VERSION}" \
    "ray[serve]==${RAY_VERSION}"

# Run as non-root for security (nonroot user is built into the base image)
USER nonroot:nonroot

# Ray GCS server, dashboard, client ports
EXPOSE 6379 8265 10001

ARG SOURCE_REVISION=dev
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}"
