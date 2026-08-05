FROM mcr.microsoft.com/oss/go/microsoft/golang:1.25-cbl-mariner2.0 AS builder
ARG TARGETARCH
RUN tdnf install -y glibc-static && tdnf clean all
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags '-s -w -linkmode external -extldflags "-static"' -o /gpu-metrics-collector ./cmd/collector

FROM mcr.microsoft.com/cbl-mariner/distroless/base:2.0
COPY --from=builder /gpu-metrics-collector /gpu-metrics-collector
USER 65532:65532
ENTRYPOINT ["/gpu-metrics-collector"]
