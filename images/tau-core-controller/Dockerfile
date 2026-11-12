# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.27.4-1@sha256:8cbcec825ea146632d907d4e35d3f4914fafcb26b62f0b3eb7b37f33fe9c4e1f AS microsoft-go

ARG MICROSOFT_GO_COMMIT=38d00402d139f69e0f5533f297788cd4df60898a

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
    && ../bin/go version | grep -F "go1.27.4"

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.27.4-1@sha256:8cbcec825ea146632d907d4e35d3f4914fafcb26b62f0b3eb7b37f33fe9c4e1f AS builder

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

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -ldflags '-s -w -X=runtime.godebugDefault=ms_tlsmldsacert=0' -o /tau-core-controller ./cmd/controller

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0@sha256:6b85a86416a29cdcfd404e9fbd9c5ef49cc0612882cecc702535dcac39c9446c

LABEL org.opencontainers.image.description="Tau core controller — reconciles TauCluster, TauWorkspace, and TauQuotaRequest state"

COPY --from=builder /tau-core-controller /tau-core-controller
USER 65532:65532
ENTRYPOINT ["/tau-core-controller"]
