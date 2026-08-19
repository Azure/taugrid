# Node Problem Detector Helm Chart

A minimal Helm chart for deploying Node Problem Detector with custom GPU and InfiniBand monitoring scripts.

## Structure

```
charts/gpu-monitoring/
├── Chart.yaml                          # Helm chart metadata
├── values.yaml                         # Default configuration values
├── templates/                          # Kubernetes resource templates
│   ├── _helpers.tpl                    # Template helper functions
│   ├── serviceaccount.yaml             # ServiceAccount resource
│   ├── clusterrole.yaml                # ClusterRole for node and event permissions
│   ├── clusterrolebinding.yaml         # ClusterRoleBinding
│   ├── executable-bundle-secret.yaml   # RBAC-separated script/config bundle
│   └── daemonset.yaml                  # DaemonSet running on GPU nodes
├── configs/                            # Monitor configuration files
│   ├── custom-plugin-monitor.json      # Custom plugin monitor config (GPU/IB checks)
│   ├── system-log-monitor.json         # System log monitor config
│   ├── system-stats-monitor.json       # System stats monitor config
│   └── known-modules.json              # Known kernel modules list
└── scripts/                            # Health check scripts
    ├── check-nvidia-smi.sh
    ├── check-nvidia-device-files.sh
    ├── check-dcgm-health.sh
    ├── check_gpu_count.sh
    ├── check_gpu_nvlink.sh
    ├── check_gpu_xid.sh
    ├── check_gpu_ecc.sh
    ├── check_gpu_ecc_from_sai.sh
    ├── check_ib.sh
    ├── check_ib_pkeys.sh
    ├── check_gpu_vbios.sh
    ├── check_gpu_vbios_consistency.sh
    ├── check_gpu_throttle.sh
    ├── check_gpu_driver.sh
    ├── check_gpu_ecc_remap_pending.sh
    ├── check_gpu_ecc_remap_failure.sh
    ├── check_gpu_nvlink_b200.sh
    ├── check_gpu_xid_always_fail.sh
    ├── check_ib_flaps.sh
    ├── check_nvme_mount.sh
    ├── check_temp_imex.sh

```

## Resources Created

1. **ServiceAccount**: `gpu-monitoring` - Identity for the DaemonSet pods
2. **ClusterRole**: Permissions to read nodes, create events, and update node status
3. **ClusterRoleBinding**: Binds the ClusterRole to the ServiceAccount
4. **Secret**: `gpu-monitoring-gpu-<content-hash>` - Immutable RBAC-separated bundle of all scripts and monitor configs
5. **DaemonSet**: `gpu-monitoring-gpu` - Runs on GPU nodes with custom checks

## Key Features

- **Custom Plugin Monitoring**: GPU health checks (NVIDIA SMI, ECC, XID errors, NVLink, throttling)
- **InfiniBand Checks**: Link status, flapping, PKey validation
- **System Monitoring**: Log and stats monitoring
- **Node Affinity**: Targets GPU worker nodes (excludes system nodes)
- **Privileged Access**: Required for GPU and IB device access
- **Immutable Executable Bundle**: Scripts use mode 0755 and a content-addressed Secret name, so updates roll pods to a new immutable object

## Installation

Install the published OCI chart from Microsoft Container Registry:

```bash
helm upgrade --install gpu-monitoring \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/gpu-monitoring \
  --version 0.1.7 \
  --namespace kube-system \
  --create-namespace
```

The chart monitors GPU health; it does not install the NVIDIA driver or GPU
device plugin. Configure those cluster prerequisites first and review the
chart values for the target GPU SKU before deployment.

To install from a source checkout instead:

```bash
helm upgrade --install gpu-monitoring ./charts/gpu-monitoring \
  --namespace kube-system \
  --create-namespace
```

## Customization

Edit `values.yaml` to customize:
- Image version
- Node affinity/tolerations
- Monitor configurations
- Resource limits

Each `gpuSkus` entry may set:

- `ib_pkey` to the exact PKey expected at `pkeys/0`. The link check validates
  only device identity, `ACTIVE`/`LinkUp`, and rate; the independent PKey check
  validates only this value. Built-in profiles set it explicitly, while custom
  profiles fall back to the global `ibPkey` value.
- `scrapeTargets` to replace the global `metricsCollector.scrapeTargets` for
  that profile. When exporter availability is enabled, the effective list must
  contain exactly one target named `dcgm-exporter`; duplicate optional targets
  remain compatible.
- `monitor_config` to select one of the five bundled
  `custom-plugin-monitor*.json` configurations. Custom profiles must name the
  bundled key explicitly; paths and external config names are rejected.
- `ib_rate_gbps` to set the expected InfiniBand rate. GB200 defaults to 400
  Gbps and GB300 defaults to 800 Gbps.
- `dcgm_health_required: false` to disable the inferred host `dcgmi` lifecycle.
  The chart enables it only for profiles whose selected DCGM scrape target is
  host-local and rejects `true` for a remote Service target.
- `dcgmAvailability` to override the DCGM scrape-target availability contract
  (condition type and debounce windows) for that profile. See
  [DCGM scrape-target availability](#dcgm-scrape-target-availability).
- `create_imex_channel_expected` to validate the NVIDIA driver's
  `CreateImexChannel0` parameter against the profile's contract. Quote `"0"` or
  `"1"` to require that exact value and fail if the parameter is absent. When
  omitted, the driver-loaded check retains its legacy behavior: an exposed
  parameter must be `1`, while a driver that does not expose the parameter
  passes. The built-in managed A100 profile expects `"0"` based on observed
  driver state. GB200 and GB300 also execute this check but omit the field, so
  they retain the legacy contract; the remaining built-in monitor
  configurations do not execute this check.

Use `enabledGpuSkus` to render only the profiles present in a deployment. An
empty list preserves the default behavior and renders all profiles.

East US 2 H200 deployments can select only the built-in East profile:

```yaml
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    ib_pkey: "0x8001"
```

The A100 profile preserves the chart 0.1.3 `0x8003` expectation. A proposed
Flex A100 value of `0x800b` is intentionally not built in because it lacks a
captured sysfs observation or authoritative fabric source.

Flex H200 overrides both the device names and PKey:

```yaml
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    ib_devices: "mlx5_ib0:1 mlx5_ib1:1 mlx5_ib2:1 mlx5_ib3:1 mlx5_ib4:1 mlx5_ib5:1 mlx5_ib6:1 mlx5_ib7:1"
    ib_pkey: "0xffff"
```

VBIOS expectations remain independently overridable through
`gpuSkus.<profile>.vbios_versions`; only add firmware versions that have been
verified on the target fleet. The built-in H200 profile expects the verified
Hopper firmware `96.00.BC.00.02`.

`GPUVbiosMismatch` and `GPUVbiosInconsistent` are likewise independent
conditions. `check_gpu_vbios.sh` asserts that every observed VBIOS version is on
the profile's allow-list, so `GPUVbiosMismatch` means fleet configuration drift;
`check_gpu_vbios_consistency.sh` asserts that all GPUs on the node run the same
version, so `GPUVbiosInconsistent` means a genuine hardware or provisioning
fault. A node whose GPUs disagree but whose versions are all allow-listed
reports only `GPUVbiosInconsistent`. The consistency check needs no allow-list
and therefore runs even when `VBIOS_VERSIONS` is unset.

For a mixed cluster with managed A100 nodes and GPU Operator H200 nodes:

```yaml
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
      - name: node-exporter
        url: http://localhost:9100/metrics
```

The A100 profile continues to use the global
`http://localhost:19400/metrics` target. Configure the GPU Operator
`ClusterPolicy.spec.dcgmExporter.service.internalTrafficPolicy` as `Local`;
otherwise its Service can return another node's metrics. GB200 and GB300 share
the historical `NVLinkB200Inactive` Node condition name for compatibility, but
the underlying NVLink and IMEX checks accept both models.

For non-host-local DCGM targets, the chart disables the host `dcgmi` check and
the metrics collector scrapes the configured endpoint. Endpoint reachability is
published as its own Node condition, described below; the host diagnostic's
`Unknown` status says nothing about a remote exporter.
Whenever effective `NPD_DCGM_REQUIRED=0`, the chart derives a profile-specific
custom-plugin monitor config that keeps the `DcgmHealthProblem` declaration and
its permanent rule, but removes the matching temporary event rule. The check
returns NPD's Unknown exit status rather than incorrectly publishing
`DcgmHealthOk`; the detailed rollout semantics are documented below.

### Host DCGM health-watch ownership

Profiles whose effective `NPD_DCGM_REQUIRED` value is `1` own the host-engine
health-watch lifecycle. Before NPD starts, a privileged init container enters
the host mount namespace through the bundled `dcgmi` wrapper, enables the DCGM
4.5.2 watch set with `health -s a`, verifies the full mask with `health -f`, and
waits 60 seconds for sampled systems. A failed set, incomplete mask, or lost
watch during warmup leaves the init container failed so Kubernetes retries it;
NPD cannot publish a false healthy result during that window.
The chart rejects these profiles unless `daemonset.hostPID` and
`daemonset.hostNetwork` are literal `true` values; remote Service-only profiles
do not require `hostNetwork` for the host lifecycle but still require `hostPID`
because the other host wrappers enter PID 1's mount namespace.

NPD evaluates the configured watches with `health -c`. The script parses
`Overall Health` because DCGM returns process status zero for Healthy, Warning,
and Failure. It accepts exactly one well-formed DCGM table row whose label is
`Overall Health` and status is Healthy, then verifies the
complete watch mask with `health -f` in the same invocation before returning
healthy; duplicates, conflicts, Warning, Failure, empty, unknown, and partial
mask results all fail closed. Initialization performs the same full-mask
verification after its 60-second warmup. A read-only liveness probe also
verifies the full watch mask with `health -f`.
`nv-hostengine` restarts discard watches, so loss of any required watch restarts
the NPD container. Its startup wrapper detects the missing mask and runs the same
set, verify, and warmup script before starting NPD again. On ordinary pod
startup, the wrapper sees the init container's complete mask and does not set it
twice. The temporary and permanent NPD rules may evaluate concurrently, but
neither mutates watch state.

Profiles using a non-loopback DCGM Service have effective
`NPD_DCGM_REQUIRED=0`: they render no host-watch init or liveness probe and rely
on the metrics collector's required exporter-availability condition instead of
host `dcgmi`.

If an expected device or its `pkeys/0` file is missing, both the link and PKey
conditions may fire: link health cannot find the expected port, and PKey health
cannot verify partition membership. Consumers should not assume these
conditions are mutually exclusive.

## DCGM scrape-target availability

Losing the DCGM exporter silences every DCGM rule, and a silenced rule looks
exactly like a healthy GPU. The collector therefore treats the `dcgm-exporter`
scrape target as **required** and publishes its reachability as a dedicated Node
condition:

| | |
| --- | --- |
| Condition type | `DcgmExporterUnavailable` (`metricsCollector.dcgmAvailability.condition`) |
| `True` | The configured DCGM endpoint failed every scrape for `unavailableFor` (default `2m`) |
| `False` | The endpoint is reachable, or has not yet failed for the full window |
| Reason | `DcgmExporterUnavailable` when set, `DcgmExporterUnavailableOk` when clear |
| Cleared | After `availableFor` (default `1m`) of continuous successful scrapes |

The condition message names the target, its URL, how long the state has held,
and the underlying connection or HTTP status error, for example:

```text
scrape target "dcgm-exporter" at http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics unavailable for 2m0s: connection refused
```

Messages are rendered from a sanitized URL: userinfo, query strings, and
fragments are stripped from both the URL and the error text, so a credentialed
or token-bearing endpoint cannot leak into node status.

The windows exist so a single missed scrape cannot flap the condition. Debounce
state is persisted with the rest of the collector state and both timers are
shifted by the collector's downtime on restore, so a restart neither re-arms the
failure timer from zero nor lets the collector's own downtime count as
continuous failure or continuous recovery.

This condition reports **endpoint reachability only**. It is deliberately
distinct from `DcgmHealthProblem`, which is NPD's host `dcgmi` diagnostic check:
a reachable exporter can still report unhealthy GPUs, and an unreachable
exporter says nothing about the GPUs themselves. Consumers that gate workload
admission should require `DcgmExporterUnavailable=False` in addition to their
existing GPU health conditions.

For a remote exporter, or when a profile explicitly sets
`dcgm_health_required: false`, the host diagnostic is not applicable. The
derived NPD monitor keeps `DcgmHealthProblem` and its permanent boot rule, but
removes the matching temporary event rule. `check-dcgm-health.sh` exits `2`,
which NPD v0.8.19 maps to `Unknown`. NPD briefly initializes declared conditions
to `False`, then the boot rule transitions both that value and any stale
pre-rollout `False` to `Unknown` without recurring warning events.
`DcgmHealthProblem=Unknown` in this mode is neither a healthy-host assertion nor
an exporter-health signal; `DcgmExporterUnavailable` remains authoritative for
exporter reachability. This follows NPD v0.8.19's
`pkg/custompluginmonitor/plugin/plugin.go` exit-code mapping (0 = OK, 1 = NonOK,
other = Unknown) and `pkg/custompluginmonitor/custom_plugin_monitor.go`
permanent-condition handling.

It works unchanged for both supported endpoint shapes — the managed host
exporter on `localhost:19400` and a GPU Operator Service on `:9400` — because a
per-profile `scrapeTargets` override inherits the contract:

```yaml
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
      - name: node-exporter
        url: http://localhost:9100/metrics
    # Optional: widen the windows for this profile only.
    dcgmAvailability:
      unavailableFor: 5m
```

Optional targets (`node-exporter`, `node-problem-detector`) keep their previous
behavior: their loss is logged and they publish no condition.

### Ownership

Each Node condition has exactly one writer:

- Within a collector, config load rejects two scrape targets that claim the same
  condition type, and rejects a scrape target that claims a condition type also
  owned by a rule.
- Across collectors, profiles are isolated by instance-type node affinity, so a
  node runs exactly one profile's DaemonSet. Rendering fails if two enabled
  profiles claim the same instance type, which would otherwise let two
  collectors race to publish this condition from different endpoints.

### Fail-closed rendering

Rendering fails, rather than silently dropping the guarantee, when:

- `metricsCollector.dcgmAvailability.enabled` is true but the effective scrape
  targets contain no `dcgm-exporter` entry;
- `condition` is not an alphanumeric condition type, or `unavailableFor` /
  `availableFor` is not a quoted duration such as `"2m"`;
- a scrape target sets `availabilityCondition` without `required: true`, or is
  `required` without an `availabilityCondition`.

Set `metricsCollector.dcgmAvailability.enabled: false` to opt a deployment out
of the contract entirely. Kubernetes cannot delete a Node condition, so on the
next start the collector publishes one explicit `False` for a condition it no
longer owns — including a renamed `condition` — rather than stranding a node as
unhealthy. That relies on the persisted collector state, which is discarded
after 30 minutes, so opt out or rename by rolling the DaemonSet rather than by
leaving nodes without a collector.

### Collector image requirement and release ordering

The `required` / `availabilityCondition` fields are read by the collector
binary, so this chart change must not be activated before a collector image that
understands them exists.

Azure/TauGrid is the single source of truth for the collector: its Go sources
live in `monitoring/gpu-metrics-collector` and its build context in
`images/gpu-metrics-collector`, both in this repository. Publication to MCR is
external automation: an ADO pipeline in
`azure-management-and-platforms/aks-ai-runtime`
(`.pipelines/publish-gpu-metrics-collector.yml`) checks out Azure/TauGrid
main directly and builds this repository's `images/gpu-metrics-collector`.
Publication is restricted to merged main so a chart cannot activate collector
behavior from an unmerged or pre-rebase source commit. That repository holds no
copy of the collector source, so there is no second source tree to keep in sync.

Release ordering:

1. Merge the base branch's chart release (`gpu-monitoring` 0.1.4) first.
2. Merge the collector source change to TauGrid main.
3. Let the merged-main publisher build and publish that source.
4. Verify the image is live on public MCR and resolve it to an immutable digest.
5. Set `metricsCollector.image.digest` to that digest — digest only, never a
   floating tag and never `latest`.
6. Merge and publish `gpu-monitoring` 0.1.5.
7. Only then let consumers pin the new chart and gate on
   `DcgmExporterUnavailable`.

For chart `0.1.7`, steps 2–5 completed with TauGrid collector PR #140 and the
merged-main publisher. Public source tag `9632b704bca1`, built from TauGrid main
commit `9632b704bca18298efffb9f3024971f9fad23477`, resolves to the multi-architecture
OCI index digest
`sha256:233aba6519e39d9a65069ba168a4cef0aed45a3ebe3080f14d3d0b98dd1076ef`.
The merged PR #140 source commit is an ancestor of that published source.

Chart `0.1.7` pins that immutable collector image and changes the gpu-monitoring
chart's host DCGM watch lifecycle and health-check contract. Publish
`gpu-monitoring` `0.1.7` directly after merge; it requires no TauGrid umbrella
chart version change.

## Security boundary

The NPD pod is privileged and uses `hostPID`, so its executable bundle must be
protected as code. The chart stores that bundle in an immutable,
content-addressed Secret rather than a ConfigMap; the chart ServiceAccount has
no permission to create, update, or delete Secrets. Do not grant untrusted
principals Secret mutation, DaemonSet mutation, or pod-deletion rights in this
namespace. Deployments that delegate those permissions require an external
admission policy or an image-baked bundle.

## Source Files

All configs and scripts are sourced from:
- Configs: `charts/gpu-monitoring/configs/`
- Scripts: `charts/gpu-monitoring/scripts/`
