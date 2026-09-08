# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.26.7-1 AS builder

RUN apt-get update && apt-get install -y --no-install-recommends gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src/controllers/tau-core
COPY controllers/tau-core/go.mod controllers/tau-core/go.sum ./
COPY core/go.mod core/go.sum ../../core/
RUN go mod download
COPY controllers/tau-core ./
COPY core ../../core

# CGO required for systemcrypto FIPS build; buildx sets TARGETOS/TARGETARCH per platform.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOEXPERIMENT=systemcrypto \
    go build -buildvcs=false -ldflags '-s -w' -o /tau-core-controller ./cmd/controller

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0

LABEL org.opencontainers.image.description="Tau core controller — reconciles TauCluster, TauWorkspace, and TauQuotaRequest state"

COPY --from=builder /tau-core-controller /tau-core-controller
USER 65532:65532
ENTRYPOINT ["/tau-core-controller"]
