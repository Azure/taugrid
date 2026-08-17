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
  --version 0.1.4 \
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
  that profile.
- `ib_rate_gbps` to set the expected InfiniBand rate. GB200 defaults to 400
  Gbps and GB300 defaults to 800 Gbps.
- `dcgm_health_required` to override whether NPD runs the host `dcgmi` health
  check. By default, the chart requires it only for profiles whose selected
  DCGM scrape target is host-local.

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

Flex A100 uses its built-in `0x800b` value. Flex H200 overrides both the device
names and PKey:

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
the metrics collector scrapes the configured endpoint. The collector does not
currently publish endpoint availability as a dedicated Node condition, so
remediation systems must not treat the absence of `DcgmHealthProblem` as proof
that a remote exporter is reachable.

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
