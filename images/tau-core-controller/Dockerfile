FROM mcr.microsoft.com/oss/go/microsoft/golang:1.25.12 AS builder

RUN apt-get update && apt-get install -y --no-install-recommends gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

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
