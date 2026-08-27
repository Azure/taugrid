---
title: Getting started on AKS
linkTitle: Install on AKS
weight: 20
description: Choose a disposable CPU runbook, a reproducible GPU Terraform root, or an existing AKS cluster, then continue the standard install
url: "/docs/platform-admin-guide/aks-setup/"
aliases:
  - "/docs/getting-started/aks-setup/"
---

AKS is one supported way to reach the Kubernetes cluster that the
[Getting started on Kubernetes guide](../kubernetes/) installs onto. This page
compares the three ways to get there and links each one to its source runbook.
Use it after deciding you want AKS specifically; if you already have a
Kubernetes cluster of any kind, go straight back to
[step 2 of Getting started on Kubernetes](../kubernetes/#2-prepare-a-kubernetes-cluster).

## Prerequisites

- An Azure subscription and the `az` CLI, logged in, with permission to create
  resource groups and AKS clusters (or an existing cluster you can already
  reach with `kubectl`).
- `tau`, `kubectl`, and `helm` on `PATH`; see
  [Install the Tau CLI](../kubernetes/#1-install-the-tau-cli). `tau
  cluster install` shells out to `helm` internally.
- For the Terraform path: Terraform 1.9+ and Azure credentials usable by the
  AzureRM provider.
- For the GPU Terraform path: regional quota for the target GPU VM SKU.

## Choose a path

| Path | Use it when | Creates billable resources |
| --- | --- | --- |
| [Guided CPU quickstart](#1-guided-cpu-quickstart-recommended-for-evaluation) | You want a guided, disposable, end-to-end evaluation with scripted cleanup, using only CPU quota | Yes |
| [GPU Terraform root](#2-reproducible-gpu-capable-terraform) | You want reviewable, reproducible AKS infrastructure with GPU node pools that your platform team can adapt and own long-term | Yes |
| [Existing AKS cluster](#3-an-existing-aks-cluster) | Your platform team already runs an AKS cluster that meets the prerequisites | Only if you scale it |

### 1. Guided CPU quickstart (recommended for evaluation)

[`examples/aks-cpu-quickstart`](https://github.com/Azure/taugrid/tree/main/examples/aks-cpu-quickstart)
sequences resource-group creation, a CPU-only AKS cluster, `tau cluster
install`, workspace creation, and a real PyTorch smoke workload, then tears
everything down again. It is the fastest way to see TauGrid work end to end
using only CPU quota.

Read its `README.md` first, then run:

```bash
./examples/aks-cpu-quickstart/run.sh
```

`run.sh` sequences only the commands documented in that README (`az`, `tau`,
`kubectl`), keeping every step visible and reproducible. When you are
done, tear the cluster and resource group down:

```bash
./examples/aks-cpu-quickstart/cleanup.sh
```

This path creates billable Azure resources (roughly $0.60–0.70/hour for the
node pool). Budget for a 25–35 minute round trip and run `cleanup.sh` as soon
as you finish; an idle cluster left running costs about $15/day. Confirm
teardown with `az group show --name taugrid-cpu-quickstart-rg`, which should
report `ResourceGroupNotFound`.

### 2. Reproducible GPU-capable Terraform

[`terraform/aks`](https://github.com/Azure/taugrid/tree/main/terraform/aks)
creates a GPU-enabled AKS environment (a system pool plus a GPU pool) and
then invokes the same supported `tau cluster install` workflow. Use this path
when your platform team wants to review, version, and re-apply the
infrastructure instead of running a one-off script.

```bash
cd terraform/aks
terraform init
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars: subscription_id, resource_group_name, cluster_name
terraform apply
```

The default GPU pool is one billable `Standard_NC24ads_A100_v4` node; keep
`terraform.tfvars` out of source control since it can hold subscription and
naming details. See the [full GPU-enabled AKS example](../../examples/full-cluster/)
for the complete workflow, including GPU stack modes, ADX telemetry, and
Portal. Destroy the environment with `terraform destroy` to stop GPU billing.

### 3. An existing AKS cluster

If your platform team already runs an AKS cluster, fetch its credentials and
use it directly, skipping any provisioning step:

```bash
az aks get-credentials --resource-group <rg> --name <cluster-name>
export TAU_CONTEXT="<kubeconfig-context>"
kubectl --context "$TAU_CONTEXT" get nodes
```

Confirm the cluster meets the
[Kubernetes cluster prerequisites](../kubernetes/#2-prepare-a-kubernetes-cluster)
(Kubernetes 1.30+, GPU drivers and device plugins if workloads need GPUs,
storage classes if workloads need PVCs), then continue with
[TauGrid installation](../kubernetes/#3-install-taugrid).

## GPU software stack models

Whichever path creates or reaches your cluster, workload configs request
standard Kubernetes `nvidia.com/gpu` resources in all three models. Platform
configuration selects the owner of the driver, device plugin, and DCGM health
exporter, plus the `dcgmHealth` source that TauGrid GPU monitoring scrapes:

| Model | Stack owner | TauGrid DCGM source | Autoscaling / support note |
| --- | --- | --- | --- |
| Terraform `gpu_stack_mode = "self_managed"` (default) | This repository's Terraform: a standalone NVIDIA device plugin plus the upstream DCGM exporter; NVIDIA GPU Operator is a separate existing-cluster model | `exporter` at `http://dcgm-exporter.dcgm-exporter.svc:9400/metrics` | Standard GPU autoscaling is supported |
| Terraform `gpu_stack_mode = "aks_managed_preview"` | AKS Managed GPU Experience: the NVIDIA driver, device plugin, and a node-local DCGM exporter at port `19400` | `host-dcgmi` at the default `http://localhost:19400/metrics` | GPU autoscaling is unsupported during the preview; requires the `Microsoft.ContainerService/ManagedGPUExperiencePreview` feature |
| Existing cluster with an externally managed NVIDIA GPU Operator | GPU Operator, per its own `ClusterPolicy` | `exporter` with an explicit non-loopback Service URL, commonly port `9400` | Autoscaling and lifecycle follow the cluster's GPU Operator configuration |

The [GPU Terraform root](#2-reproducible-gpu-capable-terraform) supports the
first two rows through `gpu_stack_mode`; see the
[full GPU-enabled AKS example](../../examples/full-cluster/) for the complete
walkthrough of both. The third row applies when your platform team already
runs GPU Operator on an [existing AKS cluster](#3-an-existing-aks-cluster).
GPU Operator owns that software stack, and TauGrid consumes its DCGM exporter
endpoint. See the GPU
monitoring chart's
[DCGM health sources](https://github.com/Azure/taugrid/blob/main/charts/gpu-monitoring/README.md#dcgm-health-sources)
documentation for the full `dcgmHealth` contract.

To use the AKS Managed GPU Experience preview, register the feature and
confirm it before applying Terraform or creating the GPU pool:

```bash
az feature register --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview
az feature show --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview --query properties.state --output tsv
az provider register --namespace Microsoft.ContainerService
az provider show --namespace Microsoft.ContainerService --query registrationState --output tsv
```

## Validate

Whichever path you chose, confirm the cluster and control plane before
creating a workspace:

```bash
kubectl --context "$TAU_CONTEXT" get nodes
tau cluster validate installation --context "$TAU_CONTEXT"
```

Continue with [Create a workspace](../kubernetes/#4-create-a-workspace)
and [Run a project workload](../kubernetes/#5-run-a-project-workload) in
the [Getting started on Kubernetes guide](../kubernetes/).
