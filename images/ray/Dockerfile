# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG PYTHON_VERSION
ARG CA_CERTIFICATES_MIN_VERSION=3.0.0-19.azl3
ARG CERTIFI_MIN_VERSION=2026.10.19

FROM mcr.microsoft.com/azurelinux/base/core:3.0 AS wget-builder
ARG CA_CERTIFICATES_MIN_VERSION
ARG WGET_VERSION=1.25.0
RUN tdnf install -y "ca-certificates>=${CA_CERTIFICATES_MIN_VERSION}" \
        gcc glibc-devel kernel-headers binutils make tar openssl-devel gawk \
    && tdnf clean all
RUN curl -sL -o /tmp/wget.tar.gz \
        "https://ftp.gnu.org/gnu/wget/wget-${WGET_VERSION}.tar.gz" \
    && tar xzf /tmp/wget.tar.gz -C /tmp
WORKDIR /tmp/wget-${WGET_VERSION}
RUN ./configure --with-ssl=openssl --disable-nls --quiet \
    && make -j"$(nproc)" --quiet

FROM mcr.microsoft.com/azurelinux/base/python:${PYTHON_VERSION} AS base

ARG RAY_VERSION
ARG CUDA_VERSION
ARG CA_CERTIFICATES_MIN_VERSION
ARG CERTIFI_MIN_VERSION

COPY --from=wget-builder /tmp/wget-*/src/wget /usr/bin/wget

RUN tdnf install -y "ca-certificates>=${CA_CERTIFICATES_MIN_VERSION}" \
        which gcc gcc-c++ binutils glibc-devel kernel-headers \
        ninja-build python3-devel \
        rdma-core libibverbs librdmacm \
    && tdnf clean all

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
    "certifi>=${CERTIFI_MIN_VERSION}" \
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
