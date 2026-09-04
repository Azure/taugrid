---
title: Provision a GPU-enabled TauGrid AKS environment
linkTitle: Full AKS baseline
weight: 30
description: Build AKS, GPU capacity, TauGrid, and Portal with the repository Terraform root.
---

{{< maturity status="alpha" reviewed="2026-08-26" >}}

Use the repository's [`terraform/aks`](https://github.com/Azure/taugrid/tree/main/terraform/aks)
root to create a GPU-enabled AKS environment. It provisions a system pool and
a GPU pool, enables OIDC and Azure Workload Identity, and invokes the supported
`tau cluster install` command.

`tau cluster install` installs the versioned TauGrid
distribution that owns Kueue, KubeRay, the Tau controller, GPU monitoring, the
baseline Kueue queue, and Portal; skip installing those components
separately with Helm.

## Prerequisites

- an Azure subscription that passes the
  [AKS cluster prerequisites](../../platform-admin-guide/aks-setup/#prerequisites), has GPU quota for the
  selected region and SKU, and an approved Terraform identity;
- Azure CLI, Terraform 1.9 or later, `kubectl`, Helm, and `tau` on PATH; and
- Azure credentials accepted by the AzureRM Terraform provider and permission
  to provision AKS, networking, storage, and identities.

Terraform uses PowerShell 7 by default on every supported host OS. Linux, WSL,
and macOS users can instead enable the Bash interpreter setting in the local
parameter file before creating a plan.

The default deployment creates one `Standard_NC24ads_A100_v4` node. This node
has one A100 80 GB GPU and is billable. Before applying, verify that the target
region has capacity for the corresponding VM family. Change
`gpu_vm_size`, `gpu_count_per_node`, and `gpu_monitoring_sku_name` together
when selecting another GPU SKU.

## Deploy

From a checkout of this repository, create a local parameter file, review the
plan, and apply that exact plan:

```bash
cd terraform/aks
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars: subscription_id, resource_group_name, cluster_name.
terraform init
terraform plan -out=taugrid.tfplan
terraform show -no-color taugrid.tfplan
terraform apply taugrid.tfplan
```

In Windows PowerShell, use `Copy-Item terraform.tfvars.example terraform.tfvars`
instead of `cp`. Keep `terraform.tfvars`, saved plans, generated kubeconfigs,
and Terraform state out of source control. Use an encrypted remote backend with
locking for every shared environment.

The tracked parameter template deliberately enables ADX, lifecycle recording,
and Portal telemetry in addition to the billable GPU node. Set both
`enable_adx` and `enable_lifecycle_recorder` to `false` before creating the
plan for a platform-only deployment.

By default, Terraform uses `gpu_stack_mode = "self_managed"`: it installs a
standalone NVIDIA device plugin plus the upstream NVIDIA DCGM exporter.
NVIDIA GPU Operator remains a separate existing-cluster model, while the
cluster platform retains ownership of the underlying NVIDIA driver. Terraform
also creates a local ignored admin
kubeconfig and values file under `terraform/aks/generated/`. For the default
A100 pool, it then normalizes MIG mode, restarts the GPU VM scale set, and
waits for allocatable GPUs before running:

```bash
tau cluster install --values generated/taugrid-values.yaml --version 0.4.1
```

In this mode, the generated values configure TauGrid GPU monitoring with
`dcgmHealth.source: exporter` and
`exporterUrl: http://dcgm-exporter.dcgm-exporter.svc:9400/metrics`, pointing
at the standalone DCGM exporter's own Service rather than a host-local
endpoint.

Set `gpu_stack_mode = "aks_managed_preview"` to use AKS Managed GPU Experience
instead. This preview mode uses `EnableManagedGPUExperience=true` at GPU pool
creation; AKS then owns the NVIDIA driver, device plugin, and a node-local
DCGM exporter host service at port `19400`. TauGrid GPU monitoring uses
`dcgmHealth.source: host-dcgmi` with the default
`http://localhost:19400/metrics` in this mode. Before applying, register the
feature, wait for `Registered`, and refresh the AKS resource provider:

```bash
az feature register --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview
az feature show --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview --query properties.state --output tsv
az provider register --namespace Microsoft.ContainerService
az provider show --namespace Microsoft.ContainerService --query registrationState --output tsv
```

GPU cluster autoscaling is not supported in the managed preview. The current
AzureRM provider does not expose `gpuProfile.nvidia.managementMode`, so the tag
is a temporary workaround. When AzureRM exposes that field, replace the tag
with the provider setting; retain the feature registration until AKS makes the
feature generally available.

If the local installation step fails after AKS has been created, correct the
local prerequisite, create and review a new plan, and apply that plan.

An existing cluster with an externally managed GPU Operator is a third,
distinct operational model alongside Terraform's standalone and AKS-managed
modes. GPU Operator owns the GPU software stack according to its own
`ClusterPolicy`, and TauGrid consumes the Operator's DCGM
exporter endpoint with `dcgmHealth.source: exporter` and an explicit
non-loopback Service URL (commonly port `9400`). See the
[GPU software stack models comparison](../../platform-admin-guide/aks-setup/#gpu-software-stack-models)
and the GPU monitoring chart's
[DCGM health sources](https://github.com/Azure/taugrid/blob/main/charts/gpu-monitoring/README.md#dcgm-health-sources)
documentation. Regardless of which of these three models owns the stack,
workload configs always request standard Kubernetes `nvidia.com/gpu`
resources unchanged.

For an existing AKS cluster with attached Flex GPU nodes, use the repository's
[pinned GPU Operator Terraform example](https://github.com/Azure/taugrid/tree/main/terraform/aks-flex-gpu-operator).
It keeps the Flex-provided host driver, installs the toolkit, device plugin, and
DCGM exporter, and documents the mixed-cluster ownership checks.

## Verify and provision a workspace

Fetch an operator kubeconfig:

```bash
terraform output -raw get_credentials_command
```

Run the command printed above to fetch the operator kubeconfig.

Verify both TauGrid and GPU capacity:

```bash
tau cluster validate installation --timeout 10m
kubectl get nodes -l accelerator=nvidia
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
```

The default path is an operator sandbox using local administrator credentials.
It creates platform infrastructure only, so a platform owner provisions the
researcher workspace with an explicit Entra group object ID before submitting
workloads:

```bash
tau workspace create taugrid-default \
  --namespace taugrid-default \
  --principal-name <entra-group-object-id> \
  --apply
tau workspace check taugrid-default
```

To provision the Entra-backed workspace in the same apply, set the following
before the first `terraform apply`. Terraform applies a native `TauWorkspace`
after `tau cluster install`; the controller reconciles the workload Namespace,
`jobqueue` LocalQueue, and namespace-scoped researcher RBAC.

```hcl
workspace_namespace = "taugrid-default"

bootstrap_workspace = {
  name                  = "taugrid-default"
  entra_group_object_id = "<entra-group-object-id>"
}
```

When bootstrap is enabled, Terraform configures Portal's computed Jobs board
with an explicit operator scope limited to this workspace namespace and
`jobqueue`. This is still an operator-only ClusterIP diagnostic path, not a
researcher-facing authenticated Portal endpoint.

On a retained cluster, removing `bootstrap_workspace` stops Terraform from
applying the CR but intentionally does not delete an existing workspace or its
workloads. Remove a workspace through the workspace administration workflow
after reviewing the impact.

## ADX and lifecycle history

The Terraform variable defaults are opt-in, but the tracked parameter template
deliberately enables the complete ADX-backed observability path. Both ADX and
lifecycle history are configured in the initial plan and apply:

```hcl
enable_adx                = true
adx_cluster_name          = ""
enable_lifecycle_recorder = true
workspace_namespace       = "taugrid-default"
```

An empty `adx_cluster_name` generates a stable 20-character
`taugrid<13-hex-characters>` candidate from the subscription, resource group,
and AKS cluster names. Set an explicit globally unique name only if Azure
reports that the candidate is in use. Terraform preserves the deployed name on
later applies, including a legacy 15-character automatic name, so upgrading does
not replace the cluster. After apply, inspect the deployed name with
`terraform output -raw adx_cluster_name`. An explicit name is used unchanged for
a new cluster; changing the name of an existing cluster requires an explicit
data-preserving migration.

Terraform installs adx-mon alongside TauGrid. In self-managed mode it
discovers the upstream DCGM exporter Pod. TauGrid GPU monitoring uses that
exporter's node-local Service in self-managed mode; in AKS managed preview mode
it collects from the GPU node host service.

When lifecycle history is enabled, Terraform creates `workspace_namespace`
before installing TauGrid so the chart can render the recorder's namespace
scoped RBAC in the same apply. If `bootstrap_workspace` is configured,
Terraform applies the TauWorkspace after installing TauGrid. Otherwise, the
`tau workspace create` command above adopts the bootstrapped namespace. Both
paths let the controller reconcile its labels, LocalQueue, RBAC, and other
workspace settings.

For a real GPU workload, use
[the A100 GPU quickstart](https://github.com/Azure/taugrid/tree/main/examples/aks-gpu-quickstart)
after the GPU allocatable-resource check succeeds. It verifies CUDA execution,
not only scheduling, and can incur additional GPU cost.

Maintainers can run the isolated
[`Invoke-TauGridAksVerification.ps1`](https://github.com/Azure/taugrid/blob/main/terraform/aks/Invoke-TauGridAksVerification.ps1)
with workspace bootstrap enabled to validate the complete GPU, ADX, Portal, and
lifecycle path. It creates a disposable environment and requires an explicit
Terraform destroy after verification.

## Portal

Terraform uses the TauGrid distribution default and installs Portal as `tau-portal` in the `tau-system` namespace. Its Service is ClusterIP-only, keeping it reachable only from inside the cluster network. An operator can inspect it with:

```bash
kubectl port-forward service/tau-portal 18080:80 --namespace=tau-system
```

This is an operator diagnostic path; researcher access requires a
platform-owned authenticated HTTPS proxy in front of Portal.
The default Portal serves Kubernetes-backed boards. Experiment, cluster-health,
and cost boards require separately configured ADX and Azure Workload Identity.

## Destroy

Destroy the environment when it is no longer needed to stop GPU billing:

```bash
terraform destroy
```
