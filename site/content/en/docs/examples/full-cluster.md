---
title: Provision a GPU-enabled TauGrid AKS environment
linkTitle: Full AKS baseline
weight: 30
description: Build AKS, GPU capacity, TauGrid, and Portal with the repository Terraform root.
---

{{< maturity status="alpha" reviewed="2026-08-17" >}}

Use the repository's [`terraform/aks`](https://github.com/Azure/taugrid/tree/main/terraform/aks)
root to create a GPU-enabled AKS environment. It provisions a system pool and
a GPU pool, enables OIDC and Azure Workload Identity, installs the NVIDIA
device plugin, and invokes the supported `tau cluster install` command.

Do not separately install Kueue, KubeRay, the Tau controller, or GPU
monitoring with Helm. `tau cluster install` installs the versioned TauGrid
distribution that owns those components, the baseline Kueue queue, and Portal.

## Prerequisites

- an Azure subscription that passes the
  [AKS setup gate](../../getting-started/aks-setup/), has GPU quota for the
  selected region and SKU, and an approved Terraform identity;
- Azure CLI, Terraform 1.6 or later, kubectl, Helm, and local Python
  dependencies;
- Azure credentials accepted by the AzureRM Terraform provider and permission
  to provision AKS, networking, storage, and identities; and
- `tau` and PowerShell 7 on PATH. Linux and macOS users can configure the
  Terraform command interpreter to use Bash.

The default deployment creates one `Standard_NC24ads_A100_v4` node. This node
has one A100 80 GB GPU and is billable. Before applying, verify that the target
region has capacity for the corresponding VM family. Change
`gpu_vm_size`, `gpu_count_per_node`, and `gpu_monitoring_sku_name` together
when selecting another GPU SKU.

## Deploy

From a checkout of this repository:

```bash
cd terraform/aks
terraform init
terraform apply -var="subscription_id=<your-subscription-id>"
```

Terraform creates a local ignored admin kubeconfig and values file under
`terraform/aks/generated/`. Its final installation step applies the NVIDIA
device plugin, then runs:

```bash
tau cluster install --values generated/taugrid-values.yaml --version 0.3.0
```

If the local installation step fails after AKS has been created, correct the
local prerequisite and rerun `terraform apply`. Terraform replaces that step
when its cluster, generated values, or TauGrid version changes.

## Verify and create a workspace

Fetch an operator kubeconfig:

```bash
az aks get-credentials \
  --resource-group "$(terraform output -raw resource_group_name)" \
  --name "$(terraform output -raw cluster_name)" \
  --overwrite-existing
```

Verify both TauGrid and GPU capacity:

```bash
tau cluster validate installation --timeout 10m
kubectl get nodes -l accelerator=nvidia
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
```

Create the default workspace with an actual Entra group object ID, then submit
the built-in smoke run:

```bash
tau workspace create taugrid-default \
  --principal-name <entra-group-object-id> \
  --apply
tau run smoke
```

## Optional ADX and lifecycle history

ADX observability is opt-in because it creates additional billable resources.
Set `enable_adx = true` and a globally unique `adx_cluster_name` before the
first apply. This installs adx-mon and DCGM exporter telemetry alongside
TauGrid.

Portal lifecycle history is a second apply: its target namespace is created by
the TauWorkspace. After creating the workspace above, add these values to the
same local `terraform.tfvars` file and run `terraform apply` again:

```hcl
enable_lifecycle_recorder           = true
lifecycle_recorder_target_namespace = "taugrid-default"
```

For a real GPU workload, use
[the A100 GPU quickstart](https://github.com/Azure/taugrid/tree/main/examples/aks-gpu-quickstart)
after the GPU allocatable-resource check succeeds. It verifies CUDA execution,
not only scheduling, and can incur additional GPU cost.

## Portal

Terraform enables Portal as `tau-portal` in the `tau` namespace. Its Service
is ClusterIP-only by design. An operator can inspect it with:

```bash
kubectl port-forward service/tau-portal 18080:80 --namespace=tau
```

This is an operator diagnostic path, not a researcher endpoint. Before giving
researchers browser access, deploy a platform-owned authenticated HTTPS proxy,
then set the Portal access values to describe that reviewed endpoint. The
default Portal serves Kubernetes-backed boards. Experiment, cluster-health, and
cost boards require separately configured ADX and Azure Workload Identity.

## Destroy

Destroy the environment when it is no longer needed to stop GPU billing:

```bash
terraform destroy -var="subscription_id=<your-subscription-id>"
```
