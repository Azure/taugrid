# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.26.8-1 AS microsoft-go

ARG MICROSOFT_GO_COMMIT=5e399a544ddb40d8ae13d077b10dc9a91d8b36e1

RUN apt-get update && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /tmp/microsoft-go
RUN git init \
    && git remote add origin https://github.com/microsoft/go.git \
    && git fetch --depth 1 origin "${MICROSOFT_GO_COMMIT}" \
    && git checkout --detach FETCH_HEAD \
    && git submodule update --init --depth 1 \
    && git -C go config user.name "Microsoft Go build" \
    && git -C go config user.email "golang@microsoft.com" \
    && git -C go am /tmp/microsoft-go/patches/[0-9][0-9][0-9][0-9]-*.patch \
    && cd go/src \
    && GOROOT_BOOTSTRAP=/usr/local/go ./make.bash \
    && ../bin/go version | grep -F "go1.26.9" \
    && GOEXPERIMENT=systemcrypto ../bin/go env GOEXPERIMENT | grep -Fx "systemcrypto"

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.26.8-1 AS builder

COPY --from=microsoft-go /tmp/microsoft-go/go /usr/local/microsoft-go
ENV GOROOT=/usr/local/microsoft-go \
    GOTOOLCHAIN=local \
    PATH=/usr/local/microsoft-go/bin:${PATH}

RUN apt-get update && apt-get install -y --no-install-recommends gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src/controllers/tau-core
COPY controllers/tau-core/go.mod controllers/tau-core/go.sum ./
COPY core/go.mod core/go.sum ../../core/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY controllers/tau-core ./
COPY core ../../core

# CGO required for systemcrypto FIPS build; buildx sets TARGETOS/TARGETARCH per platform.
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOEXPERIMENT=systemcrypto \
    go build -buildvcs=false -ldflags '-s -w' -o /tau-core-controller ./cmd/controller

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0

LABEL org.opencontainers.image.description="Tau core controller — reconciles TauCluster, TauWorkspace, and TauQuotaRequest state"

COPY --from=builder /tau-core-controller /tau-core-controller
USER 65532:65532
ENTRYPOINT ["/tau-core-controller"]
