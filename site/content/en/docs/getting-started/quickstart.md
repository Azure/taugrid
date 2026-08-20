---
title: TauGrid quickstart
linkTitle: Quickstart
weight: 6
description: Create an AKS cluster, install TauGrid, and run a GPU workload
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

Use this TauGrid quickstart to create an AKS cluster in East US, install the released TauGrid distribution from MCR, create a workspace, and run an NVIDIA A100 workload. The setup flow is shown below.

{{< tau-visual base="Tau-Getting-Started-Flow" title="TauGrid quickstart flow" source="svg" >}}

Before you begin, install the Azure CLI, `kubectl`, Helm 3 or 4, and Git; run `az login`; and select a subscription with at least 24 unused `NCADS_A100_v4` vCPUs in East US.

## Step 1: Install the Tau CLI

Install the latest released CLI in your user account and verify that it is available on `PATH`.

```bash
curl -fsSL https://github.com/Azure/taugrid/releases/latest/download/install.sh |
  sh &&
export PATH="$HOME/.local/bin:$PATH" &&
tau version --short
```

For supported platforms, version pinning, upgrades, and the optional Python SDK, see [Install Tau CLI](../install/).

## Step 2: AKS setup

Create a one-node AKS cluster on the smallest A100 80GB VM size and install the NVIDIA device plugin so Kubernetes advertises the GPU.

```bash
az group create --name taugrid-quickstart --location eastus && \
az aks create \
  --resource-group taugrid-quickstart \
  --name taugrid-quickstart \
  --location eastus \
  --node-count 1 \
  --node-vm-size Standard_NC24ads_A100_v4 \
  --generate-ssh-keys && \
az aks get-credentials \
  --resource-group taugrid-quickstart \
  --name taugrid-quickstart \
  --overwrite-existing && \
kubectl apply -f https://raw.githubusercontent.com/Azure/taugrid/main/examples/aks-gpu-quickstart/nvidia-device-plugin.yaml && \
kubectl -n kube-system rollout status daemonset/nvidia-device-plugin-daemonset --timeout=5m
```

For subscription selection, identity, networking, quota, existing-cluster requirements, or a production-ready cluster with Kusto and full monitoring enabled, see [AKS setup](../aks-setup/).

## Step 3: TauGrid setup

Install the release-aligned public MCR chart into the AKS cluster and run the built-in control-plane readiness checks.

```bash
tau cluster install &&
tau cluster validate nodes --min-healthy 1
```

The commands should finish with every enabled installation check passing, including `PASS Portal`, and one healthy GPU node. The default Portal Deployment, Service, ServiceAccount, and read-only Kubernetes RBAC are installed in `tau-system`. For chart values, previews, upgrades, and additional validation, see [TauGrid setup](../taugrid-setup/).

## Step 4: Workspace setup

Create the default workspace, namespace, LocalQueue, and Kubernetes RBAC contract used by this cluster-operator quickstart.

```bash
tau workspace create --apply &&
kubectl wait \
  --for=jsonpath='{.status.phase}'=Ready \
  workspace/taugrid-default \
  --namespace tau-system \
  --timeout=5m &&
tau workspace check taugrid-default
```

The commands should report that the workspace is `Ready`. For researcher access, identity, storage, and repository setup, see [Tau Workspace Setup](../tau-workspace-setup/).

## Step 5: Run a GPU workload

Create a small Tau repository with a one-worker RayJob and a Python GPU probe, then submit it with the same CUDA Ray image used by the AKS GPU example. The remote function reports the accelerator assigned by Ray and returns the output from `nvidia-smi`.

```bash
mkdir taugrid-quickstart &&
cd taugrid-quickstart &&
git init --quiet

cat > tau.yaml <<'EOF'
name: taugrid-quickstart-gpu
entrypoint: gpu_check.py
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
compute:
  gpus_per_worker: 1
  workers: 1
EOF

cat > gpu_check.py <<'EOF'
import subprocess

import ray


@ray.remote(num_gpus=1)
def check_gpu():
    gpu_ids = ray.get_runtime_context().get_accelerator_ids().get("GPU", [])
    nvidia_smi = subprocess.run(
        ["nvidia-smi"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return gpu_ids, nvidia_smi


ray.init()
gpu_ids, nvidia_smi = ray.get(check_gpu.remote())
print(f"Ray GPU IDs: {gpu_ids}")
print(nvidia_smi)
EOF

tau run
```

Check the initial status, stream the application logs until completion, and then inspect the final status.

```bash
tau run status taugrid-quickstart-gpu &&
tau run logs taugrid-quickstart-gpu --follow &&
tau run status taugrid-quickstart-gpu
```

The final status should report `Complete` / `Succeeded`. The logs should contain a non-empty list after `Ray GPU IDs:` (typically `['0']` for this one-GPU workload) and the NVIDIA-SMI table for an NVIDIA A100 GPU.

## Step 6: View the job in the Portal

Confirm that the default Portal is ready, forward its ClusterIP Service to your workstation, and open the job details page at [http://127.0.0.1:8080/portal/runs/taugrid-default/taugrid-quickstart-gpu](http://127.0.0.1:8080/portal/runs/taugrid-default/taugrid-quickstart-gpu).

```bash
kubectl --namespace tau-system rollout status deployment/tau-portal --timeout=5m &&
kubectl --namespace tau-system port-forward service/tau-portal 8080:80
```

The rollout command should report `deployment "tau-portal" successfully rolled out`. The port-forward process should report that it is forwarding `127.0.0.1:8080`, and the page should show the RayJob as `Complete`, with its quota released and compute reusable. This operator-only diagnostic validates the Kubernetes-backed run detail path; Kusto-backed boards, the computed Jobs board, and KueueViz require additional platform configuration.

![TauGrid Portal showing the completed GPU RayJob](../../../images/tau/TauGrid-Quickstart-Job-Status.png)

## Optional Step 7: Clean up

If you no longer need the cluster after exploring the result, delete its resource group to stop the A100 billing.

```bash
az group delete --name taugrid-quickstart --yes --no-wait
```

For more workloads and platform patterns, see [TauGrid examples](../../examples/) and the repository's [`examples/`](https://github.com/Azure/taugrid/tree/main/examples) directory.
