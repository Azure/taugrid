---
title: Provision a GPU-enabled TauGrid AKS environment
linkTitle: Full AKS baseline
weight: 30
description: Build AKS, GPU capacity, TauGrid, and Portal with the repository Terraform root.
---

{{< maturity status="alpha" reviewed="2026-08-24" >}}

Use the repository's [`terraform/aks`](https://github.com/Azure/taugrid/tree/main/terraform/aks)
root to create a GPU-enabled AKS evaluation environment. It provisions a system
pool and a GPU pool, enables OIDC and Azure Workload Identity, and invokes the
supported `tau cluster install` workflow.

Do not separately install Kueue, KubeRay, the Tau controller, or GPU
monitoring with Helm. Terraform runs `tau cluster install`, which installs the
versioned TauGrid distribution that owns those components, the baseline Kueue
queue, and Portal.

## Prerequisites

- an Azure subscription that passes the
  [AKS setup gate](../../getting-started/aks-setup/), has GPU quota for the
  selected region and SKU, and an approved Terraform identity;
- Azure CLI, Terraform 1.9 or later, `kubectl`, Helm, and `tau` on PATH; and
- Azure credentials accepted by the AzureRM Terraform provider with permission
  to provision AKS, networking, storage, and identities.

The default deployment creates one `Standard_NC24ads_A100_v4` node. This node
has one A100 80 GB GPU and is billable. Before applying, verify that the target
region has capacity for the corresponding VM family. Change
`gpu_vm_size`, `gpu_count_per_node`, and `gpu_monitoring_sku_name` together
when selecting another GPU SKU.

The [Terraform README](https://github.com/Azure/taugrid/tree/main/terraform/aks)
is the reference for all supported inputs and troubleshooting.

## Deploy

From a checkout of this repository, create a local parameter file, review the
plan, then apply that exact plan:

Windows uses PowerShell 7 by default. Linux, WSL, and macOS users set
`command_interpreter = ["bash", "-c"]` in `terraform.tfvars` before creating
the plan.

```bash
cd terraform/aks
cp terraform.tfvars.example terraform.tfvars

# Edit terraform.tfvars with the subscription and environment values.
terraform init
terraform plan -out tau-aks.tfplan
terraform apply tau-aks.tfplan
```

Keep `terraform.tfvars`, saved plans, generated kubeconfigs, and Terraform
state out of source control. Use an encrypted remote backend with locking for
any shared environment. The committed `.terraform.lock.hcl` is part of the
reproducible dependency contract and must remain tracked.

By default, Terraform uses `gpu_stack_mode = "self_managed"`: it installs the
NVIDIA device plugin and upstream NVIDIA DCGM exporter. It creates a local,
ignored admin kubeconfig and values file under `terraform/aks/generated/`. For
the default A100 pool, it normalizes MIG mode, restarts the GPU VM scale set,
waits for allocatable GPUs, and then installs TauGrid.

Set `gpu_stack_mode = "aks_managed_preview"` to use AKS Managed GPU Experience.
Before applying, register the feature, wait for `Registered`, and refresh the
AKS resource provider:

```bash
az feature register --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview
az feature show --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview --query properties.state --output tsv
az provider register --namespace Microsoft.ContainerService
az provider show --namespace Microsoft.ContainerService --query registrationState --output tsv
```

GPU cluster autoscaling is not supported in the managed preview. The current
AzureRM provider does not expose `gpuProfile.nvidia.managementMode`, so the tag
is a temporary workaround. If a local installation prerequisite fails after
AKS is created, correct it and rerun `terraform apply`.

## Verify and create a workspace

Fetch an operator kubeconfig:

```bash
terraform output -raw get_credentials_command
```

Run the command printed above. Then verify both TauGrid and GPU capacity:

```bash
tau cluster validate installation --timeout 10m
kubectl get nodes -l accelerator=nvidia
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
```

The default path is an operator sandbox using local administrator credentials.
Create the default workspace, then submit the built-in smoke run:

```bash
tau workspace create taugrid-default --apply
tau run smoke
```

## Optional ADX and lifecycle history

ADX observability is opt-in because it creates additional billable resources.
Set `enable_adx = true` and a globally unique `adx_cluster_name` before the
first apply. This installs adx-mon alongside TauGrid. In self-managed mode it
discovers the upstream DCGM exporter Pod. TauGrid GPU monitoring uses that
exporter's node-local Service in self-managed mode; in AKS managed preview mode
it collects from the GPU node host service.

Portal lifecycle history is a second apply because its target namespace is
created by the TauWorkspace. After creating the workspace above, add these
values to the same local `terraform.tfvars` file and run `terraform apply`
again:

```hcl
enable_lifecycle_recorder = true
workspace_namespace       = "taugrid-default"
```

For a real GPU workload, use [the A100 GPU quickstart](https://github.com/Azure/taugrid/tree/main/examples/aks-gpu-quickstart)
after the GPU allocatable-resource check succeeds. It verifies CUDA execution,
not only scheduling, and can incur additional GPU cost.

## Portal

Terraform installs Portal as `tau-portal` in the `tau-system` namespace. Its
Service is ClusterIP-only and Terraform does not expose it outside the cluster.
An operator can inspect it with:

```bash
kubectl port-forward service/tau-portal 18080:80 --namespace=tau-system
```

This is an operator diagnostic path, not a researcher endpoint. Before giving
researchers browser access, deploy a platform-owned authenticated HTTPS proxy.
The default Portal serves Kubernetes-backed boards. Experiment, cluster-health,
and cost boards require separately configured ADX and Azure Workload Identity.

## Destroy

Destroy the environment when it is no longer needed to stop GPU billing:

```bash
terraform destroy
```

Use the same local parameter file and backend that created the environment.
