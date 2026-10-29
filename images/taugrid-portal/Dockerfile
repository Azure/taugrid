# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM --platform=$BUILDPLATFORM docker.io/library/node:24.22.1-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS frontend

WORKDIR /app/portal/frontend
COPY portal/frontend/package.json portal/frontend/package-lock.json portal/frontend/.npmrc ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY portal/frontend ./
RUN npm run build

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.27.3-1-azurelinux3.0@sha256:9af70997416d1deb69c2457146d56c74caf22c3a96ab66466647d4677ec5391f AS builder

ENV GOTOOLCHAIN=go1.27.3 \
    MS_GOTOOLCHAIN_ALLOW_NON_LOCAL=1

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY core/go.mod core/go.sum ./core/
COPY portal/go.mod portal/go.sum ./portal/
WORKDIR /app/portal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

WORKDIR /app
COPY core ./core
COPY portal ./portal
COPY --from=frontend /app/portal/internal/portalapi/assets ./portal/internal/portalapi/assets

WORKDIR /app/portal
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOEXPERIMENT=none CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X github.com/Azure/taugrid/core/version.Version=${VERSION} -X github.com/Azure/taugrid/core/version.Commit=${COMMIT} -X github.com/Azure/taugrid/core/version.Date=${DATE} -X=runtime.godebugDefault=ms_tlsmldsacert=0" \
    -o /taugrid-portal ./cmd/taugrid-portal

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0@sha256:e952865122598043cfb54f0257f016e28e02c21904e172b47dcb2148e095693a

COPY --from=builder /taugrid-portal /usr/local/bin/taugrid-portal
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/taugrid-portal"]
