# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.26.8-1-azurelinux3.0 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY core/go.mod core/go.sum ./core/
COPY metrics/experiment-metrics-collector/go.mod metrics/experiment-metrics-collector/go.sum ./metrics/experiment-metrics-collector/
WORKDIR /app/metrics/experiment-metrics-collector
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

WORKDIR /app
COPY core ./core
COPY metrics/experiment-metrics-collector ./metrics/experiment-metrics-collector

WORKDIR /app/metrics/experiment-metrics-collector
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOEXPERIMENT=none CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X github.com/Azure/taugrid/core/version.Version=${VERSION} -X github.com/Azure/taugrid/core/version.Commit=${COMMIT} -X github.com/Azure/taugrid/core/version.Date=${DATE}" \
    -o /taugrid-metrics-collector ./cmd/taugrid-metrics-collector

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0

COPY --from=builder /taugrid-metrics-collector /usr/local/bin/taugrid-metrics-collector
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/taugrid-metrics-collector"]
