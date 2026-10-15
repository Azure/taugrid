# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.26.8-1-azurelinux3.0 AS builder
ENV GOTOOLCHAIN=go1.26.9 \
    MS_GOTOOLCHAIN_ALLOW_NON_LOCAL=1
ARG TARGETARCH
RUN tdnf install -y glibc-static && tdnf clean all
WORKDIR /src/monitoring/gpu-metrics-collector
COPY monitoring/gpu-metrics-collector/go.mod monitoring/gpu-metrics-collector/go.sum ./
RUN go mod download
COPY monitoring/gpu-metrics-collector/ ./
RUN GOEXPERIMENT=none CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags '-s -w -linkmode external -extldflags "-static"' -o /gpu-metrics-collector ./cmd/collector

FROM mcr.microsoft.com/cbl-mariner/distroless/base:2.0 AS runtime
COPY --from=builder /gpu-metrics-collector /gpu-metrics-collector
USER 65532:65532
ENTRYPOINT ["/gpu-metrics-collector"]

FROM runtime AS validation
RUN ["/gpu-metrics-collector", "--help"]

FROM runtime AS final
