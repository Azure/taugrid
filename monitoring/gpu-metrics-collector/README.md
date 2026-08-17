# NPD Metrics Collector

A lightweight, config-driven sidecar that replaces Prometheus + AlertManager in the NPD DaemonSet. It scrapes DCGM exporter and node-exporter Prometheus endpoints, evaluates threshold rules locally, and writes results directly as Kubernetes **Node conditions** — enabling UNO (Unbound Node Operator) to drain or taint nodes without a centralized monitoring stack.

## Architecture

```
┌─────────────────────────────────────── NPD DaemonSet Pod ───────────────────────────────────────┐
│                                                                                                  │
│  ┌──────────────┐   ┌──────────────┐   ┌────────────────────┐                                   │
│  │ DCGM Exporter│   │ Node Exporter│   │ Node Problem       │                                   │
│  │ :19400/:9400 │   │  :9100       │   │ Detector            │                                   │
│  └──────┬───────┘   └──────┬───────┘   └────────────────────┘                                   │
│         │                  │                                                                     │
│         └────────┬─────────┘                                                                     │
│                  ▼                                                                                │
│  ┌──────────────────────────────────┐        ┌──────────────────────┐                            │
│  │   Metrics Collector (this)      │        │  rules.yaml          │                            │
│  │   - Parallel scraper            │◄───────│  (ConfigMap)         │                            │
│  │   - Rule engine (rate/instant)  │        └──────────────────────┘                            │
│  │   - Condition writer            │                                                             │
│  └──────────────┬───────────────────┘                                                            │
│                 │                                                                                 │
└─────────────────┼─────────────────────────────────────────────────────────────────────────────────┘
                  │  Strategic Merge Patch
                  ▼
          ┌───────────────┐         ┌─────────────────┐
          │ Node Object   │────────▶│ UNO Operator    │
          │ .status       │         │ - reads conditions│
          │  .conditions  │         │ - drains/taints   │
          └───────────────┘         └─────────────────┘
```

**Signal path:** Metric exceeds threshold → Collector writes Node condition (`True`) → UNO reads condition → UNO drains/taints node

## Features

- **Config-driven rules** — all rules defined in `values.yaml`, no Go code changes needed to add/remove rules
- **Dual DCGM support** — scrape AKS managed GPU experience on `19400` or a
  node-local GPU Operator Service on `9400`
- **Parallel scraping** — concurrent HTTP fetches to all targets with bounded connection pool
- **Rate and instant modes** — supports cumulative counter rate-of-change and instantaneous threshold checks
- **"For" duration** — conditions must persist for a configurable duration before firing (reduces flaps)
- **Startup jitter** — random delay before first scrape to prevent thundering herd at fleet scale
- **Heartbeat throttling** — patches API server every ~5 minutes when nothing changed (immediate on status change)
- **Per-node jitter offset** — heartbeat cycles are offset randomly so API patches are distributed evenly across the fleet
- **Graceful degradation** — unavailable scrape targets are logged and skipped (e.g., no GPU = no DCGM)
- **Strategic merge patch** — writes only changed conditions; coexists safely with NPD's own conditions

## Scale Characteristics (20K nodes)

| Metric | Value |
|--------|-------|
| Per-collector CPU | ~10m |
| Per-collector memory | ~32Mi |
| Scrape interval | 15s |
| Heartbeat interval (steady state) | ~5min |
| API patches/sec (steady state) | ~67 |
| API patches/sec (status change) | immediate, up to 1,333 |

## Project Structure

```
monitoring/gpu-metrics-collector/
├── cmd/collector/main.go          # Entrypoint: flags, K8s client, scrape loop
├── internal/
│   ├── scraper/
│   │   ├── scraper.go             # Parallel Prometheus text format scraper
│   │   └── scraper_test.go
│   ├── rules/
│   │   ├── rules.go               # Rule engine: rate/instant modes, history, cleanup
│   │   └── rules_test.go
│   ├── conditions/
│   │   └── conditions.go          # Node condition writer with jittered heartbeat
│   └── config/
│       └── config.go              # YAML config loader with validation
├── Dockerfile                     # Multi-stage: Microsoft Go 1.26.6 → distroless/static
├── Makefile
├── go.mod
└── go.sum
```

## Building

```bash
# Local build (native arch)
make build

# Run tests
make test

# Build container for ARM64 (stretch nodes)
docker buildx build --platform linux/arm64 \
  -f images/gpu-metrics-collector/Dockerfile \
  -t gpu-metrics-collector:dev --load .

# Build container for AMD64
docker buildx build --platform linux/amd64 \
  -f images/gpu-metrics-collector/Dockerfile \
  -t gpu-metrics-collector:dev --load .
```

## Configuration

Rules are defined in the Helm `values.yaml` under `metricsCollector.rules` and rendered into a ConfigMap. The collector loads this at startup via `--config`.

### DCGM endpoints

Use `metricsCollector.scrapeTargets` as the default and
`gpuSkus.<profile>.scrapeTargets` to select a different endpoint for one GPU
monitoring DaemonSet:

- AKS managed GPU experience:
  `http://localhost:19400/metrics`
- NVIDIA GPU Operator:
  `http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics`

Configure the GPU Operator `ClusterPolicy` so its Service uses
`spec.internalTrafficPolicy: Local`:

```yaml
spec:
  dcgmExporter:
    service:
      internalTrafficPolicy: Local
```

This is the Kubernetes-native locality guarantee: traffic is routed only to an
exporter endpoint on the collector's node instead of another node's GPU
exporter.

### Helm Values

```yaml
metricsCollector:
  enabled: true
  image:
    repository: mcr.microsoft.com/aks/ai-runtime/gpu-metrics-collector
    tag: 5e606678
  scrapeInterval: "15s"
  resources:
    requests:
      cpu: "10m"
      memory: "32Mi"
    limits:
      cpu: "100m"
      memory: "64Mi"
  scrapeTargets:
    - name: dcgm-exporter
      url: http://localhost:19400/metrics
    - name: node-exporter
      url: http://localhost:9100/metrics
  rules:
    - name: ecc-dbe-retired
      metricName: DCGM_FI_DEV_ECC_DBE_AGG_TOTAL
      conditionType: GPUECCDoubleRetired
      mode: rate
      threshold: 0
      window: 1m
      for: 1m
    # ... more rules
```

For a GPU Operator-backed profile, set its `gpuSkus.<profile>.scrapeTargets` to
the node-local Service URL shown above while managed profiles inherit the global
port-19400 target. Apply the `ClusterPolicy` setting above before enabling
Node-condition writes.

### Rule Schema

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Human-readable rule name |
| `metricName` | string | Prometheus metric name to match |
| `labels` | map | Optional label selectors (e.g., `xid: "48"`) |
| `conditionType` | string | Node condition type to write (e.g., `GPUECCDoubleRetired`) |
| `mode` | `rate` or `instant` | `rate`: fires when increase over `window` > `threshold`. `instant`: fires when current value > `threshold` |
| `threshold` | float | Threshold value |
| `window` | duration | Time window for rate calculation (only for `rate` mode) |
| `for` | duration | Condition must persist this long before firing |

### Default Rules (20)

| Category | Condition | Mode | Action |
|----------|-----------|------|--------|
| **ECC Double-Bit** | `GPUECCDoubleRetired`, `GPUECCDoubleVolatile` | rate 1m | drain |
| **NVLink Errors** | `GPUNVLinkCRCFlitErrors`, `GPUNVLinkCRCDataErrors`, `GPUNVLinkReplayErrors` | rate 1m | drain |
| **XID Errors** | `XIDError48`, `XIDError63`, `XIDError64`, `XIDError79`, `XIDError94`, `XIDError95` | rate 1m | drain |
| **Thermal/Power** | `GPUThermalViolation`, `GPUPowerViolation` | rate 1m | taint |
| **InfiniBand** | `IBLinkDown`, `IBSymbolError` | rate 1m | taint |
| **Correctable ECC** | `GPUECCSingleVolatileRate` (>10/10m), `GPUECCSingleRetired` | rate | taint |
| **PCIe** | `GPUPCIeReplayErrors` (>5/5m) | rate | taint |
| **Grace CPU** | `GraceCPUECCUncorrectable` | instant | drain |
| **Grace CPU** | `GraceCPUECCCorrectableRate` (>10/10m) | rate | taint |

> The "Action" column reflects what UNO does when the condition fires — the collector only writes the condition.

## Deployment

The collector runs as a sidecar in the NPD DaemonSet. Enable it per-SKU via overlay values:

```yaml
# applications/npd/overlays/cx/values-spark.yaml
metricsCollector:
  enabled: true
  image:
    repository: mcr.microsoft.com/aks/ai-runtime/gpu-metrics-collector
    tag: 5e606678
```

### Adding Custom Rules

To add a new rule for a specific SKU, add it to the overlay's `metricsCollector.rules` list. The entire rules list in the overlay replaces the base — so copy the base rules and add your extras.

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | `/etc/gpu-metrics-collector/rules.yaml` | Path to rules config file |
| `--node-name` | `$NODE_NAME` env var | Kubernetes node name |
| `--scrape-interval` | `15s` | How often to scrape metrics |

## Integration with UNO

The collector writes Node conditions. UNO's policy configmap maps condition names to actions:

```yaml
# applications/uno/base/configmap.yaml (excerpt)
rules:
  - name: mc-ecc-dbe-retired
    priority: 10
    conditionName: GPUECCDoubleRetired
    action: drain
    reason: gpu-ecc-dbe-retired
  - name: mc-xid-48
    priority: 30
    conditionName: XIDError48
    action: drain
    reason: xid-error-48
  # ... all 20 collector conditions mapped
```

## Development

```bash
# Run tests with race detector
go test -race -count=1 ./...

# Vet
go vet ./...

# Format
gofmt -w .
```
