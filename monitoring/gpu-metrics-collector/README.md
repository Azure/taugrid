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
- **Graceful degradation** — unavailable optional scrape targets are logged and skipped (e.g., no GPU = no DCGM)
- **Required-target availability** — a required target's sustained loss is published as its own Node condition, so a silenced exporter cannot look like a healthy GPU
- **Continuous metric coverage** — required finite samples are counted per physical GPU; missing, partial, stale, and insufficient rate input becomes `Unknown`, not an error-free reading
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
│   ├── availability/
│   │   ├── availability.go        # Required-target reachability → debounced conditions
│   │   └── availability_test.go
│   ├── conditions/
│   │   ├── conditions.go          # Node condition writer with jittered heartbeat
│   │   └── conditions_test.go
│   └── config/
│       ├── config.go              # YAML config loader with validation
│       └── config_test.go
├── Dockerfile                     # Multi-stage: Microsoft Go 1.26.7 → distroless/static
├── Makefile
├── go.mod
└── go.sum
```

## Building

This repository is the single source of truth for the collector: the Go sources
here and the build context in `images/gpu-metrics-collector` are what ships.
There is no second copy of these sources to keep in sync.

Production images are published from merged Azure/TauGrid `main` by external,
approved automation rather than by a workflow in this repository. Merging a
change here therefore does not by itself alter any deployed cluster: a chart
must additionally pin an image digest built from that merged source before new
collector behavior takes effect.

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

Use the chart's `dcgmHealth.source` and `dcgmHealth.exporterUrl` defaults, or
`gpuSkus.<profile>.dcgmHealth` to select a different health source and endpoint
for one GPU monitoring DaemonSet:

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
dcgmHealth:
  source: host-dcgmi
  exporterUrl: http://localhost:19400/metrics
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

For a GPU Operator-backed profile, set its `dcgmHealth.source` to `exporter`
and its `dcgmHealth.exporterUrl` to the node-local Service URL shown above.
Managed profiles can retain the global host-dcgmi settings. Apply the
`ClusterPolicy` locality setting above before enabling Node-condition writes.

### Scrape Target Schema

The collector accepts these fields in the `scrapeTargets` entries of its config.
The bundled `gpu-monitoring` chart owns its DCGM target and fixed availability
condition; configure that chart through `dcgmHealth` rather than overriding its
raw `scrapeTargets`. The schema below also supports standalone collector configs.

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Target name, used in logs and condition messages |
| `url` | string | Absolute `http`/`https` metrics endpoint |
| `required` | bool | Target loss is a node health signal, not just a log line. Requires `availabilityCondition` |
| `availabilityCondition` | string | Node condition type reporting this target's reachability. Requires `required: true` |
| `unavailableFor` | duration | Continuous failure before the condition is set (default `2m`) |
| `availableFor` | duration | Continuous success before the condition is cleared (default `1m`) |

Targets without `required` keep the previous behavior: failures are logged,
skipped, and publish no condition.

### Required-Target Availability

A rule can only report on metrics the collector received. When a required
exporter disappears, every rule reading its metrics evaluates to "not firing",
which is indistinguishable from a healthy node. Required targets close that gap:

- The condition is set only after `unavailableFor` of continuous failure and
  cleared only after `availableFor` of continuous success, so one missed scrape
  cannot flap it.
- Debounce state is persisted with the rest of the collector state, and both
  timers are shifted by the collector's downtime on restore. A restart neither
  re-arms the failure timer from zero nor counts the collector's own downtime as
  continuous scrape failure or continuous recovery.
- The collector publishes a condition only once it has proven that condition's
  state itself, or restored continuity from a snapshot written by a process that
  had. If the snapshot is missing, corrupt, or stale, it stays silent rather
  than reporting `False`, so a restart in the middle of an outage cannot clear a
  `True` condition the API server already holds on the strength of a scrape that
  just failed. Silence leaves the existing condition untouched; the condition is
  re-asserted after `unavailableFor` of proven failure, or cleared after
  `availableFor` of proven success.
- A condition the collector no longer owns (availability disabled, or the
  condition renamed) is published once as `False` on the next start, because
  Kubernetes cannot delete a Node condition and nothing else would clear it. It
  is published exactly once, and never for a condition type a rule now owns:
  results share one patch keyed by condition type, so a repeated stale clear
  would overwrite the live owner's value.
- Messages carry the target name, a sanitized URL, how long the state has held,
  and the underlying connection or HTTP status error. Userinfo, query strings,
  and fragments are stripped from both the URL and the error text, so a
  credentialed endpoint cannot leak into node status.
- The condition reports endpoint reachability only. It is distinct from DCGM
  diagnostic health (NPD's `dcgmi` checks, `DcgmHealthProblem`).
- Each required target owns exactly one condition type. Config load rejects two
  targets that claim the same condition type and rejects a target that claims a
  condition type also owned by a rule.

Validation is scoped to explicit availability and metric-coverage contracts. Configuration shapes that
earlier versions accepted — a target with no name, a target with no URL, a
duplicate target name, or two rules sharing a condition type — still load and
are only logged as warnings. Refusing to start would restart-loop the collector
and freeze every condition it owns, which is worse than the degraded-but-running
behavior it replaces. A rule declaring `minSamples` must have a valid mode,
condition, finite threshold, positive rate window, and valid coverage settings.
Its condition cannot be shared with another rule.

### Continuous Metric Coverage

Exporter reachability is not proof that its CSV enables every required field.
Use `minSamples` to require continuous readings, and `sampleLabel` to count
distinct physical identities rather than duplicate series:

```yaml
rules:
  - name: ecc-dbe-volatile
    metricName: DCGM_FI_DEV_ECC_DBE_VOL_TOTAL
    conditionType: GPUECCDoubleVolatile
    mode: rate
    threshold: 0
    window: 1m
    minSamples: 8
    sampleLabel: UUID
    maxSampleAge: 2m
```

Fewer valid identities, missing identity labels, non-finite values, or expired
explicit Prometheus timestamps produce `Unknown/MetricCoverageUnavailable`.
Untimestamped exporters use the current successful scrape as their observation
time; unchanged counter values alone do not establish staleness. The default
maximum sample age is two minutes, including the maximum tolerated future
clock offset.

Rate rules require two consecutive observations within their window. Missing
series, invalid readings, excessive observation gaps, and collector restarts
break that baseline instead of interpreting the unobserved interval as healthy.
Unknown input resets a pending `for` timer when no known violation remains.
A known threshold violation still takes precedence over incomplete coverage:
missing another GPU must not hide an observed fault.

For a signal split into several metric families, use `metricNames` instead
of `metricName`. Every named family must have valid observations on at least
`minSamples` **of the same identities**. For example, an eight-GPU profile with
18 required NVLink fields needs all 144 GPU/link combinations, not 144 arbitrary
samples or eight samples from link zero. A missing, stale or warming-up link
remains Unknown; a measured fault still takes precedence.

```yaml
rules:
  - name: nvlink-replay
    metricNames:
      - DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_L0
      - DCGM_FI_DEV_NVLINK_REPLAY_ERROR_COUNT_L1
    conditionType: GPUNVLinkReplayErrors
    mode: rate
    threshold: 0
    window: 1m
    for: 1m
    minSamples: 8
    sampleLabel: UUID
```

This abbreviated example covers **two links only**; enumerate every expected
link for the actual topology. A metric set must be nonempty, contain distinct
literal names, and declare both positive `minSamples` and `sampleLabel`.
Thresholds apply independently to each series. With an any-increase threshold
of zero this detects an increase on any link without letting another link's
counter reset cancel it out. Accumulated nonzero counters alone are not a new
rate violation. Older coverage-enabled binaries reject the missing single
`metricName`; deploy a binary supporting metric sets before using this shape.

`minSamples: 0` retains optional/sparse-event behavior. In particular, a missing
`err_code="48"` XID event is not a missing continuous GPU reading. Do not require
one such error event per GPU merely to establish health. Required exporter
availability remains a separate signal, with its existing debounce windows.

The writer preserves `Unknown` through Node patches, status transitions, and
state persistence. Recovery clears obsolete diagnostic messages, while ordinary
heartbeats preserve the server's last transition time. Consumers must not interpret either `Unknown` or a missing
condition as an explicit `False`/healthy verdict. These Node conditions are
distinct from the Portal's metrics-backed row-remapping health summary.

For the chart, see `metricsCollector.requireMetricCoverage` in
[`charts/gpu-monitoring`](../../charts/gpu-monitoring/README.md). It expands
the chart-only `perGpu: true` rule marker into `minSamples` from the profile's
physical `num_gpus`, plus `sampleLabel: UUID`. The default published image is
older than this contract: build and publish the updated collector through the
approved image pipeline, pin its immutable digest, then enable coverage.
Enabling `--require-metric-coverage` on an old image fails startup rather than
silently ignoring the new contract; the updated binary also rejects that flag
when its config contains no coverage rule.

### Rule Schema

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Human-readable rule name |
| `metricName` | string | One Prometheus metric name; mutually exclusive with `metricNames` |
| `metricNames` | string list | Explicit families required on the same identities; requires positive `minSamples` and `sampleLabel` |
| `labels` | map | Optional label selectors (e.g., `err_code: "48"`) |
| `conditionType` | string | Node condition type to write (e.g., `GPUECCDoubleRetired`) |
| `mode` | `rate` or `instant` | `rate`: fires when increase over `window` > `threshold`. `instant`: fires when current value > `threshold` |
| `threshold` | float | Threshold value |
| `window` | duration | Time window for rate calculation (only for `rate` mode) |
| `for` | duration | Condition must persist this long before firing |
| `minSamples` | integer | Required valid samples; zero preserves optional/sparse behavior |
| `sampleLabel` | string | Count distinct nonempty label values instead of raw samples; requires `minSamples` |
| `maxSampleAge` | duration | Maximum explicit sample age and rate-observation gap; default `2m` for coverage rules |

### Default Rules (20)

| Category | Condition | Mode | Action |
|----------|-----------|------|--------|
| **ECC Double-Bit** | `GPUECCDoubleRetired`, `GPUECCDoubleVolatile` | rate 1m | drain |
| **NVLink Errors** | `GPUNVLinkCRCFlitErrors`, `GPUNVLinkCRCDataErrors`, `GPUNVLinkReplayErrors` | rate 1m | drain |
| **XID Errors** | `XIDError48`, `XIDError63`, `XIDError64`, `XIDError79`, `XIDError94`, `XIDError95` | instant | drain |
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
| `--require-metric-coverage` | `false` | Refuse startup unless the config declares at least one coverage rule |

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
