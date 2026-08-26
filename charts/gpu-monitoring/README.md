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
  --namespace tau-system \
  --create-namespace
```

The chart monitors GPU health; it does not install the NVIDIA driver or GPU
device plugin. Configure those cluster prerequisites first and review the
chart values for the target GPU SKU before deployment.

To install from a source checkout instead:

```bash
helm upgrade --install gpu-monitoring ./charts/gpu-monitoring \
  --namespace tau-system \
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
- `dcgmHealth` to override the global `dcgmHealth.source` and/or
  `.exporterUrl` for that profile. See
  [DCGM health sources](#dcgm-health-sources).
- `monitor_config` to select one of the five bundled
  `custom-plugin-monitor*.json` configurations. Custom profiles must name the
  bundled key explicitly; paths and external config names are rejected.
- `ib_rate_gbps` to set the expected InfiniBand rate. GB200 defaults to 400
  Gbps and GB300 defaults to 800 Gbps.
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
    dcgmHealth:
      source: exporter
      exporterUrl: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
```

The A100 profile continues to use the global default
`dcgmHealth.source: host-dcgmi` and `http://localhost:19400/metrics`. Configure
the GPU Operator `ClusterPolicy.spec.dcgmExporter.service.internalTrafficPolicy`
as `Local`; otherwise its Service can return another node's metrics. GB200 and
GB300 share the historical `NVLinkB200Inactive` Node condition name for
compatibility, but the underlying NVLink and IMEX checks accept both models.

`dcgmHealth.source: exporter` disables the host `dcgmi` check; it scrapes the
configured endpoint and the collector publishes its reachability as its own
Node condition, described below — the host diagnostic's `Unknown` status says
nothing about a remote exporter. Whenever the effective source is not
`host-dcgmi`, the chart derives a profile-specific custom-plugin monitor
config that keeps the `DcgmHealthProblem` declaration and its permanent rule,
but removes the matching temporary event rule. The check returns NPD's
Unknown exit status rather than incorrectly publishing `DcgmHealthOk`; the
detailed rollout semantics are documented below.

### Host DCGM health-watch ownership

Profiles whose effective `dcgmHealth.source` is `host-dcgmi` own the
host-engine health-watch lifecycle. Before NPD starts, a privileged init
container enters the host mount namespace through the bundled `dcgmi`
wrapper, enables the DCGM 4.5.2 watch set with `health -s a`, verifies the
full mask with `health -f`, and waits 60 seconds for sampled systems. A
failed set, incomplete mask, or lost watch during warmup leaves the init
container failed so Kubernetes retries it; NPD cannot publish a false healthy
result during that window.
The chart rejects these profiles unless `daemonset.hostPID` and
`daemonset.hostNetwork` are literal `true` values; `exporter` profiles do not
require `hostNetwork` for the host lifecycle but still require `hostPID`
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

`exporter` profiles render no host-watch init or liveness probe. They rely on
the metrics collector's required exporter-availability condition instead of
host `dcgmi`.

If an expected device or its `pkeys/0` file is missing, both the link and PKey
conditions may fire: link health cannot find the expected port, and PKey health
cannot verify partition membership. Consumers should not assume these
conditions are mutually exclusive.

## DCGM health sources

This chart accepts exactly two DCGM health fields, at the global level and
per-profile under `gpuSkus.<profile>.dcgmHealth`:

```yaml
dcgmHealth:
  source: host-dcgmi
  exporterUrl: http://localhost:19400/metrics
```

`source` is exactly one of two values. The chart itself constructs the
`dcgm-exporter` scrape target and owns its Node-condition contract; there is
no configurable `required`, `availabilityCondition`, condition name, or
debounce. Both values require literal `metricsCollector.enabled: true` —
every accepted profile's contract includes the fixed `DcgmExporterUnavailable`
scrape target and every `DCGM_*` collector rule, and the collector is the
only component that renders them. Disabling DCGM health monitoring entirely
means disabling the gpu-monitoring component or chart release itself; there
is no per-profile opt-out.

| `source` | Runs host `dcgmi` | Scrapes exporter | Cross-cloud mapping |
| --- | --- | --- | --- |
| `host-dcgmi` (default) | Yes: init/warm/liveness-repair, `dcgmi health -c` | Yes | AKS's managed host DCGM engine |
| `exporter` | No | Yes | GKE/EKS/GPU Operator DCGM exporter pods (a Service or node-local endpoint) |

This chart is agnostic to how the scraped DCGM engine got onto the node, but
the three GPU stack models this repository documents map to `source` as
follows: the `terraform/aks` root defaults to `gpu_stack_mode =
"self_managed"`, which installs a standalone NVIDIA device plugin plus the
upstream DCGM exporter and configures `source: exporter` against that
exporter's own Service; NVIDIA GPU Operator remains a separate existing-cluster
model. Setting `gpu_stack_mode =
"aks_managed_preview"` instead uses the AKS Managed GPU Experience, which owns
the driver, device plugin, and a node-local DCGM exporter and matches this
chart's own `source: host-dcgmi` default; and an existing cluster with an
externally managed NVIDIA GPU Operator is a third, separate model — GPU
Operator owns the stack per its own `ClusterPolicy`, and this chart consumes
its DCGM exporter with `source: exporter` and an explicit non-loopback Service
URL, commonly port `9400`. See the AKS getting-started guide's GPU software
stack models comparison for the full picture across all three.

`exporterUrl` must be a nonempty, absolute, entirely lowercase `http://` or
`https://` URL with no whitespace, backslashes, userinfo credentials, query
string, or fragment. Authentication belongs in the exporter deployment rather
than in a URL embedded in the collector ConfigMap.
`host-dcgmi` additionally requires a loopback URL (`localhost`, IPv4
`127.0.0.0/8`, or IPv6 `::1`) — this pod runs with `hostNetwork: true`, so
loopback reaches the host-level exporter. `exporter` requires an explicit
non-loopback URL: it must
not silently inherit the global `host-dcgmi` loopback default, because that
would scrape nothing on a Service-backed node. Rendering fails before any
manifest is produced if a profile's effective source is `exporter` but its
effective `exporterUrl` resolves to loopback; set the profile's own
`dcgmHealth.exporterUrl` to its real endpoint instead.

These are global defaults; only `gpuSkus.<profile>.dcgmHealth.source` and
`.exporterUrl` may override them, so a mixed cluster can run managed AKS
nodes (`host-dcgmi`) and GPU Operator nodes (`exporter`) side by side without
touching the rest of each profile.

For both `host-dcgmi` and `exporter`, the collector scrapes the effective
`exporterUrl` and publishes its reachability as a fixed Node condition:

| | |
| --- | --- |
| Condition type | `DcgmExporterUnavailable` (fixed; not configurable) |
| `True` | The configured DCGM endpoint failed every scrape for 2 minutes |
| `False` | The endpoint is reachable, or has not yet failed for the full window |
| Reason | `DcgmExporterUnavailable` when set, `DcgmExporterUnavailableOk` when clear |
| Cleared | After 1 minute of continuous successful scrapes |

The condition message names the target, its URL, how long the state has held,
and the underlying connection or HTTP status error, for example:

```text
scrape target "dcgm-exporter" at http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics unavailable for 2m0s: connection refused
```

Messages are rendered from a sanitized URL: userinfo, query strings, and
fragments are stripped from both the URL and the error text, so a credentialed
or token-bearing endpoint cannot leak into node status.

The 2-minute unavailable / 1-minute available windows exist so a single missed
scrape cannot flap the condition. Debounce state is persisted with the rest of
the collector state and both timers are shifted by the collector's downtime on
restore, so a restart neither re-arms the failure timer from zero nor lets the
collector's own downtime count as continuous failure or continuous recovery.

This condition reports **endpoint reachability only**. It is deliberately
distinct from `DcgmHealthProblem`, which is NPD's host `dcgmi` diagnostic check:
a reachable exporter can still report unhealthy GPUs, and an unreachable
exporter says nothing about the GPUs themselves. Consumers that gate workload
admission should require `DcgmExporterUnavailable=False` in addition to their
existing GPU health conditions.

For `exporter` profiles, the host diagnostic is not applicable. The derived
NPD monitor keeps `DcgmHealthProblem` and its permanent boot rule, but removes
the matching temporary event rule. `check-dcgm-health.sh` exits `2`, which NPD
v0.8.19 maps to `Unknown`. NPD briefly initializes declared conditions to
`False`, then the boot rule transitions both that value and any stale
pre-rollout `False` to `Unknown` without recurring warning events.
`DcgmHealthProblem=Unknown` in this mode is neither a healthy-host assertion
nor an exporter-health signal; `DcgmExporterUnavailable` remains authoritative
for exporter reachability. This follows NPD v0.8.19's
`pkg/custompluginmonitor/plugin/plugin.go` exit-code mapping (0 = OK, 1 =
NonOK, other = Unknown) and
`pkg/custompluginmonitor/custom_plugin_monitor.go` permanent-condition
handling.

### Ownership

Each Node condition has exactly one writer:

- The fixed `DcgmExporterUnavailable` condition is reserved: rendering fails
  if a `metricsCollector.rules` entry or the profile's effective NPD monitor
  conditions claim that name.
- Across collectors, profiles are isolated by instance-type node affinity, so a
  node runs exactly one profile's DaemonSet. Rendering fails if two enabled
  profiles claim the same instance type, which would otherwise let two
  collectors race to publish this condition from different endpoints.

### Fail-closed rendering

Rendering fails, rather than silently dropping the guarantee, when:

- the effective `dcgmHealth` is not a map, `source` is not exactly one of
  `host-dcgmi` or `exporter`, or `exporterUrl` is not a nonempty absolute
  lowercase HTTP(S) URL without whitespace, backslashes, credentials, query
  strings, or fragments — the error text never echoes the URL;
- the global `dcgmHealth` or a profile's `gpuSkus.<profile>.dcgmHealth` is
  present but not a map — for example `null` or a bare string — rather than
  silently falling back to a default or panicking on the merge;
- `source: host-dcgmi` and the effective `exporterUrl` is not loopback;
- `source: exporter` and the effective `exporterUrl` is loopback (including
  silent inheritance of the `host-dcgmi` default);
- `metricsCollector.enabled` is not `true` for any enabled profile, regardless
  of its effective source — there would be no component to render the fixed
  `DcgmExporterUnavailable` scrape target and collector rules;
- a collector rule or effective NPD monitor condition claims the reserved
  `DcgmExporterUnavailable` name.

### Migrating from chart 0.1.6

Chart 0.1.6 exposed a generic scrape-target availability/ownership API:
`metricsCollector.scrapeTargets`, `metricsCollector.dcgmAvailability`,
`gpuSkus.<profile>.scrapeTargets`, `gpuSkus.<profile>.dcgmAvailability`, and
`gpuSkus.<profile>.dcgm_health_required`. Chart 0.1.7 removed all of these in
favor of the constrained `dcgmHealth.source` / `dcgmHealth.exporterUrl`
contract above. Supplying any of the removed keys fails Helm rendering with a
migration message rather than silently ignoring them or falling back to a
default.

### Collector image requirement and release ordering

The `required` / `availabilityCondition` fields the chart renders into the
fixed dcgm-exporter scrape target are read by the collector binary, so this
chart change must not be activated before a collector image that understands
them exists.

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
