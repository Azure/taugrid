# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG PYTHON_VERSION=3.12
ARG PYTHON_BASE_DIGEST=sha256:9343005b72a7cc34847dcb8d09bb8a0c145311eff88c151c60d9c4904edaf6e9

FROM mcr.microsoft.com/azurelinux/base/core:3.0.20261022@sha256:bd88359666e3f20296e7fe639a480dfd3c3c8eba299b21f26e4560353e21a656 AS wget-builder
ARG WGET_VERSION=1.25.0
RUN tdnf install -y gcc glibc-devel kernel-headers binutils make tar \
        openssl-devel ca-certificates gawk && tdnf clean all
RUN curl -sL -o /tmp/wget.tar.gz \
        "https://ftp.gnu.org/gnu/wget/wget-${WGET_VERSION}.tar.gz" \
    && tar xzf /tmp/wget.tar.gz -C /tmp
WORKDIR /tmp/wget-${WGET_VERSION}
RUN ./configure --with-ssl=openssl --disable-nls --quiet \
    && make -j"$(nproc)" --quiet

FROM mcr.microsoft.com/azurelinux/base/python:${PYTHON_VERSION}@${PYTHON_BASE_DIGEST} AS base

ARG PYTHON_VERSION
ARG RAY_VERSION
ARG CUDA_VERSION

RUN python3 -c "import platform, sys; actual=platform.python_version(); expected='${PYTHON_VERSION}'; sys.exit(0 if actual == expected or actual.startswith(expected + '.') else f'expected Python {expected}, found {actual}')"

COPY --from=wget-builder /tmp/wget-*/src/wget /usr/bin/wget

RUN tdnf install -y ca-certificates which gcc gcc-c++ binutils glibc-devel kernel-headers \
        ninja-build python3-devel \
        rdma-core libibverbs librdmacm && tdnf clean all

ENV SSL_CERT_FILE=/etc/pki/tls/certs/ca-bundle.crt
ENV CURL_CA_BUNDLE=/etc/pki/tls/certs/ca-bundle.crt
ENV PIP_CERT=/etc/pki/tls/certs/ca-bundle.crt

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
ENV LIBRARY_PATH="/usr/local/cuda/lib64/stubs"

RUN pip install --no-cache-dir \
    "certifi>=2026.10.19" \
    "ray[default]==${RAY_VERSION}" \
    "ray[data]==${RAY_VERSION}" \
    "ray[serve]==${RAY_VERSION}" \
    && case "${RAY_VERSION}" in \
        2.57.0|2.58.0) pip install --no-cache-dir "jinja2==3.1.6" ;; \
        *) : ;; \
    esac \
    && pip check

USER nonroot:nonroot

EXPOSE 6379 8265 10001

ARG SOURCE_REVISION=dev
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}"
