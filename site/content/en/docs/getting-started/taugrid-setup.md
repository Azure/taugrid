---
title: TauGrid setup
linkTitle: TauGrid setup
weight: 3
description: Install the MCR TauGrid Helm chart with the Tau CLI and verify the control plane
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

TauGrid setup is a cluster-administrator workflow that begins after the [AKS setup](../aks-setup/) completion gate passes. It uses the Tau CLI as a Helm wrapper to install the released TauGrid distribution from MCR into an existing Kubernetes cluster, then verifies the in-cluster control plane.

This page does not provision AKS, node pools, Azure networking, managed Entra, workload identities, storage accounts, registries, GPU drivers, device plugins, or CSI drivers. Those are Azure/provider responsibilities owned by AKS setup. Workspace resources and the researcher repository are created in the following [workspace setup](../workspace/) phase.

## 1. Verify the administrator workstation and AKS context

Install the [Tau CLI](../install/), Helm 3 or 4, and `kubectl`. Use the normal AKS cluster-user or approved administrator context produced by AKS setup; do not rely on an implicit current context.

```bash
TAU_CONTEXT="<aks-context>"

tau version --short
helm version --short
kubectl --context "$TAU_CONTEXT" version
kubectl --context "$TAU_CONTEXT" get nodes
```

You should see a released Tau version, a Helm 3 or 4 version, a Kubernetes server version of 1.30 or newer, and the required AKS nodes in `Ready` state. Stop here if a command is unavailable, the context points to the wrong cluster, the API is unreachable, or required provider capabilities are not ready.

## 2. Understand what the TauGrid chart installs

The Tau CLI defaults to the versioned public OCI chart at `oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid`. A source checkout and local chart path are not required for the end-user installation path.

The default distribution contains:

| Component | Default | Responsibility |
| --- | --- | --- |
| Kueue | Enabled | Queueing, admission, and quota policy |
| KubeRay operator | Enabled | RayCluster, RayJob, and RayService lifecycle |
| `tau-core-controller` and `TauCluster` | Enabled | TauWorkspace reconciliation and reviewed node-label rules |
| Baseline queue and quota guard | Enabled | A portable `jobqueue` policy and fail-closed quota approval contract |
| GPU monitoring | Enabled with the Tau controller | Profile-specific GPU, interconnect, NVMe, and DCGM health collection; this observes provider-enabled GPUs but does not install a GPU driver or device plugin |
| `taugrid-core` services chart | Included | Portal, Stellar, lifecycle recorder, and image prewarm remain individually disabled until the platform opts in |

Use [cluster install values](../../reference/cluster-install-values/) for the complete values contract. The `components.gpuMonitoring.enabled` key is intentionally unset by default so GPU monitoring follows `components.tauCoreController.enabled`; set it explicitly only when the cluster has another owner for that monitoring stack.

### Controller release artifacts

The controller source and raw Kustomize manifests live in the [`Azure/taugrid`](https://github.com/Azure/taugrid) repository. Versioned runtime artifacts are released separately through MCR: the `mcr.microsoft.com/aks/ai-runtime/tau-core-controller:<version>` image, the standalone controller OCI chart, the TauGrid umbrella OCI chart, and the CRDs packaged by both charts.

Inspect a published version without cloning the repository:

```bash
TAUGRID_VERSION="<published-version>"

helm show chart oci://mcr.microsoft.com/aks/ai-runtime/helm/tau-core-controller --version "$TAUGRID_VERSION"
helm show crds oci://mcr.microsoft.com/aks/ai-runtime/helm/tau-core-controller --version "$TAUGRID_VERSION"
helm show chart oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid --version "$TAUGRID_VERSION"
helm show crds oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid --version "$TAUGRID_VERSION"
```

The chart commands should print matching `version` and `appVersion` fields. Both CRD commands should list `clusters.tau.azure.com`, `quotarequests.tau.azure.com`, and `workspaces.tau.azure.com`.

Use `tau cluster install` and the umbrella chart for a fresh cluster. The standalone chart is only for a platform that deliberately manages the controller as a separate Helm release. Do not install it while an umbrella release has `components.tauCoreController.enabled=true`: both paths render the same cluster-scoped RBAC, controller Deployment, and `TauCluster`, and both packages carry the same CRDs. Disable the umbrella component before assigning those resources to a standalone release.

## 3. Inspect values and preview the release

Print the values reference before creating a cluster-specific values file:

```bash
tau cluster explain-values
```

For a default fresh-cluster installation, preview the released chart without contacting or changing the cluster:

```bash
tau cluster install --context "$TAU_CONTEXT" --dry-run
```

You should see an installation plan whose chart is the MCR OCI reference, followed by `TauGrid render summary (nothing was applied)` and `Rendered the TauGrid manifests offline. The cluster was not read or changed.`

If the platform needs different queue quotas, GPU flavors, component switches, or service configuration, put the complete reviewed configuration in one file and preview that exact file:

```bash
tau cluster install \
  --context "$TAU_CONTEXT" \
  --values taugrid-values.yaml \
  --dry-run
```

`tau cluster install` uses Helm `--reset-values` so the bootstrap phase cannot leak temporary values into the final release. Pass the same complete values file on every upgrade; a later command with only a partial overlay resets omitted settings to chart defaults.

## 4. Install TauGrid from MCR

Install the release-aligned chart with the CLI defaults:

```bash
tau cluster install --context "$TAU_CONTEXT"
```

Or install the previously reviewed platform values:

```bash
tau cluster install \
  --context "$TAU_CONTEXT" \
  --values taugrid-values.yaml
```

The plan should show the MCR chart, the chart version compiled into the Tau CLI release, the `taugrid` Helm release, and the `tau-system` namespace. On a fresh cluster, the CLI performs a control-plane bootstrap Helm pass, applies the baseline queue policy in a second pass, and then runs component-aware readiness checks.

A default successful installation ends with output shaped like:

```text
TauGrid installation validation
  PASS  Kubernetes           ...
  PASS  Kueue                ...
  PASS  KubeRay              ...
  PASS  Tau controller       ...
  PASS  TauCluster           ...
  PASS  Baseline queue       ...
  PASS  Quota guard          ...
READY: 7/7 checks passed

TauGrid is installed and ready as Helm release taugrid in namespace tau-system.
```

Components explicitly disabled in the release are reported as `SKIP` and reduce the required check count. A `FAIL` or `NOT READY` result means TauGrid setup is incomplete even if Helm created resources.

The default command does not enable Helm's generic watcher or rollback. Add `--wait` to run Helm's watcher before Tau's readiness checks, or `--atomic` to roll back a Helm operation that fails. Tau's post-Helm readiness report always runs; a failure in that report leaves a successfully applied release available for inspection.

## 5. Re-run the core control-plane readiness gate

The installation command runs this gate automatically. Re-run it at any time without changing the cluster:

```bash
tau cluster validate installation --context "$TAU_CONTEXT"
```

You should again see `READY` with every enabled core check marked `PASS` or an intentionally disabled component marked `SKIP`. This command validates Kubernetes compatibility, Kueue, KubeRay, the Tau controller, `TauCluster`, the baseline queue, and the quota guard. It does not claim that every optional service or every GPU-monitoring DaemonSet is healthy.

When GPU monitoring is enabled, inspect its profile-specific DaemonSets separately:

```bash
kubectl --context "$TAU_CONTEXT" \
  --namespace tau-system \
  get daemonsets \
  --selector app.kubernetes.io/name=gpu-monitoring
```

You should see the enabled GPU profiles. Profiles with no matching nodes can correctly show `DESIRED=0`; each profile that matches live GPU nodes should eventually have `READY` equal to `DESIRED`.

For a GPU cluster, an administrator can also run active node health probes after the provider driver and device registration are ready:

```bash
tau cluster validate nodes \
  --context "$TAU_CONTEXT" \
  --gpu-class <gpu-class> \
  --min-healthy <expected-node-count>
```

This command creates short-lived privileged Pods on the selected GPU nodes, so run it only with explicit cluster-administrator authorization. A passing result ends with `<healthy>/<selected> nodes healthy` and exits `0`.

## 6. Continue to workspace setup

TauGrid setup is complete when `tau cluster validate installation` exits `0`, required GPU/provider checks pass for the intended workloads, and any explicitly enabled services have their own documented readiness evidence.

Continue with [workspace setup](../workspace/) to create or adopt the TauWorkspace, verify its namespace/RBAC/LocalQueue/ServiceAccount and storage contract, and hand a non-secret Tau-enabled repository to the researcher. Do not treat a healthy cluster control plane as proof that a workspace or its PVC is ready.
