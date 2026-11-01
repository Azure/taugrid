# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.27.3-1-azurelinux3.0@sha256:9af70997416d1deb69c2457146d56c74caf22c3a96ab66466647d4677ec5391f AS builder

ENV GOTOOLCHAIN=go1.27.3 \
    MS_GOTOOLCHAIN_ALLOW_NON_LOCAL=1

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY core/go.mod core/go.sum ./core/
COPY cli/go.mod cli/go.sum ./cli/
WORKDIR /app/cli
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

WORKDIR /app
COPY core ./core
COPY cli ./cli

WORKDIR /app/cli
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOEXPERIMENT=none CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X github.com/Azure/taugrid/core/version.Version=${VERSION} -X github.com/Azure/taugrid/core/version.Commit=${COMMIT} -X github.com/Azure/taugrid/core/version.Date=${DATE} -X=runtime.godebugDefault=ms_tlsmldsacert=0" \
    -o /tau ./cmd/tau

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0@sha256:e952865122598043cfb54f0257f016e28e02c21904e172b47dcb2148e095693a

COPY --from=builder /tau /usr/local/bin/tau
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tau"]
