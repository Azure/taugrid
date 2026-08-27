---
title: Run GPU Ray Tune HPO on AKS
linkTitle: GPU Ray Tune HPO
weight: 20
description: Provision GPU-capable AKS, enable TauGrid, and run the six-trial Ray Tune example through a repository workspace.
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

This guide runs the GPU HPO workflow for
[`examples/ray-tune-smoke`](https://github.com/Azure/taugrid/tree/main/examples/ray-tune-smoke).
It was verified on one `Standard_NV12ads_A10_v5` node. A100 SKUs are also
usable when the selected subscription and region have both sufficient quota
and real-time allocation capacity.

The workflow creates billable Azure resources. Use a dedicated resource group.
Record its owner, collect the required evidence, and delete the resources when
the workflow is complete.

## Ownership and completion gates

Complete these phases in order:

| Phase | Owner | Completion gate |
|---|---|---|
| **AKS setup (Azure/provider)** | Azure platform operator | Managed-Entra/Azure-RBAC AKS is reachable through normal cluster-user credentials; the CPU system pool and GPU pool are `Ready`; the provider GPU driver and device plugin expose a usable device. |
| **TauGrid setup** | Kubernetes/TauGrid platform operator | The checked-in TauGrid chart passes installation and node validation. |
| **Workspace setup** | Kubernetes/TauGrid platform operator | A `TauWorkspace` for the actual researcher subject is `Ready`, and the repository connection descriptor is generated. |
| **Researcher workflow** | Researcher | The repository-first dry-run, optional smoke, six-trial Tune run, logs, result grid, and terminal lifecycle gates all pass with context, namespace, and queue resolved automatically. |

See [Install TauGrid](../../platform-admin-guide/kubernetes/#3-install-taugrid)
for the general provider/platform boundary. Azure platform operators provision
AKS, Azure RBAC, the GPU driver, and the device plugin; TauGrid builds on top of
them.

## 1. Record local state and choose the target

Run from a TauGrid checkout with `az`, `kubectl`, `kubelogin`, Helm, Git, and
the Go version declared in `cli/go.mod`. Install `tau` from this checkout to use
the same version for the CLI, chart, and example:

```bash
make install-tau-cli

TAU_BIN_DIR="$(go env GOBIN)"
test -n "$TAU_BIN_DIR" || TAU_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$TAU_BIN_DIR:$PATH"

command -v tau
tau version --short
tau --help >/dev/null
```

Persist `TAU_BIN_DIR` on `PATH` for later terminals. The
[Tau CLI installation guide](../../platform-admin-guide/kubernetes/#1-install-the-tau-cli) explains upgrades,
the recommended GitHub Release installation, and the optional Python SDK.

Record the original local state and select the target:

```bash
TAUGRID_ROOT="$PWD"

ORIGINAL_SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
ORIGINAL_KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
ORIGINAL_CONTEXT="$(
  KUBECONFIG="$ORIGINAL_KUBECONFIG" kubectl config current-context
)"

TARGET_SUBSCRIPTION_ID="<subscription-id>"
TENANT_ID="<tenant-id>"
LOCATION="westus2"
RESOURCE_GROUP="tau-ray-tune-a10-rg"
AKS_CLUSTER="tau-ray-tune-a10"
GPU_POOL="gpua10"
GPU_SKU="Standard_NV12ads_A10_v5"
GPU_FAMILY="StandardNVADSA10v5Family"
RESEARCHER_UPN="<researcher-upn>"
RESEARCHER_OBJECT_ID="<researcher-object-id>"

WORK_DIR="$(mktemp -d)"
export KUBECONFIG="$WORK_DIR/kubeconfig"
```

Each Azure command below specifies `--subscription
"$TARGET_SUBSCRIPTION_ID"`. Resolve identity object IDs through the approved
identity-management path. Do not export access tokens or create credentials to
bypass Conditional Access.

## 2. Check cost, SKU visibility, quota, and allocation risk

Check the exact SKU and its regional restrictions:

```bash
az vm list-skus \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --location "$LOCATION" \
  --resource-type virtualMachines \
  --size "$GPU_SKU" \
  --all \
  --query "[0].{name:name,locations:locations,restrictions:restrictions}" \
  --output jsonc

az vm list-usage \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --location "$LOCATION" \
  --query "[?name.value=='$GPU_FAMILY'].[name.localizedValue,currentValue,limit]" \
  --output table
```

Do not continue unless the SKU has no blocking restriction and the family has
enough unused vCPU quota for the planned node count. Quota permits capacity
requests; actual allocation still depends on regional, zonal, or
subscription-specific capacity, which can be exhausted.

This workflow requires one `Standard_NV12ads_A10_v5` node, which exposes one
8 GiB A10 partition. A100 shapes cost more and may require a different
regional family quota. Select an A100 only where both quota and a real create
request succeed. Do not scale past the budgeted node count to work around a
capacity failure.

## 3. Provision managed-Entra/Azure-RBAC AKS

Create one small CPU system node and one GPU worker node:

```bash
az group create \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION" \
  --tags taugrid-e2e=true taugrid-disposable=true

az aks create \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_CLUSTER" \
  --location "$LOCATION" \
  --enable-managed-identity \
  --enable-aad \
  --enable-azure-rbac \
  --nodepool-name system \
  --node-count 1 \
  --node-vm-size Standard_D4_v5 \
  --node-osdisk-type Managed \
  --no-ssh-key

az aks nodepool add \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$AKS_CLUSTER" \
  --name "$GPU_POOL" \
  --mode User \
  --node-count 1 \
  --node-vm-size "$GPU_SKU" \
  --node-osdisk-type Managed \
  --labels sku=gpu \
  --node-taints sku=gpu:NoSchedule
```

AKS owns reserved labels such as `accelerator`; do not set them yourself. The
GPU node should receive `accelerator=nvidia` from AKS.

Grant the researcher normal AKS cluster-user credential access unless the platform
has already done so:

```bash
AKS_ID="$(
  az aks show \
    --subscription "$TARGET_SUBSCRIPTION_ID" \
    --resource-group "$RESOURCE_GROUP" \
    --name "$AKS_CLUSTER" \
    --query id -o tsv
)"
MANAGED_RESOURCE_GROUP="$(
  az aks show \
    --subscription "$TARGET_SUBSCRIPTION_ID" \
    --resource-group "$RESOURCE_GROUP" \
    --name "$AKS_CLUSTER" \
    --query nodeResourceGroup -o tsv
)"

az role assignment create \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --assignee-object-id "$RESEARCHER_OBJECT_ID" \
  --assignee-principal-type User \
  --role "Azure Kubernetes Service Cluster User Role" \
  --scope "$AKS_ID"
```

The platform operator still needs an approved AKS administration role to
install cluster components. Do not grant cluster-admin to the researcher. The
TauWorkspace provides the required namespaced lifecycle permissions.

Fetch credentials only into the isolated file and record the exact target
before the first Kubernetes operation:

```bash
az aks get-credentials \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_CLUSTER" \
  --file "$KUBECONFIG" \
  --overwrite-existing

kubectl config current-context
kubectl config view --minify \
  -o jsonpath='{.contexts[0].context.cluster}{"\n"}'
kubectl get nodes -o wide
```

Both nodes must be `Ready`.

## 4. Verify Kubernetes can allocate the GPU

For the default AKS GPU-driver mode, AKS installs the host driver. This
walkthrough installs the checked-in, digest-pinned NVIDIA device plugin:

```bash
kubectl apply \
  -f "$TAUGRID_ROOT/examples/aks-gpu-quickstart/nvidia-device-plugin.yaml"

kubectl -n kube-system rollout status \
  daemonset/nvidia-device-plugin-daemonset \
  --timeout=5m

kubectl get nodes -l "agentpool=$GPU_POOL" \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
```

If the platform selected an AKS mode that skips the host driver, use the
corresponding supported Azure/NVIDIA GPU Operator path instead. Do not combine
two driver managers. In either mode, `nvidia.com/gpu` must be nonzero before
TauGrid submission.

Verify that an ordinary pod can consume the resource and run `nvidia-smi`:

```bash
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: tau-gpu-readiness
spec:
  restartPolicy: Never
  nodeSelector:
    sku: gpu
  tolerations:
    - key: sku
      operator: Equal
      value: gpu
      effect: NoSchedule
  containers:
    - name: probe
      image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
      command: ["/bin/sh", "-c"]
      args:
        - nvidia-smi -L && nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader
      resources:
        requests:
          nvidia.com/gpu: "1"
        limits:
          nvidia.com/gpu: "1"
YAML

kubectl wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  pod/tau-gpu-readiness \
  --timeout=10m
kubectl logs tau-gpu-readiness
kubectl delete pod tau-gpu-readiness
```

Do not proceed unless the log identifies the expected NVIDIA device.

## 5. Install TauGrid and enable the workspace

Install and validate the checked-in chart:

```bash
tau cluster install \
  --chart "$TAUGRID_ROOT/charts/taugrid" \
  --wait \
  --atomic \
  --timeout 15m

tau cluster validate installation
tau cluster validate nodes
```

The required gates are `READY: 8/8 checks passed` and at least one healthy GPU
node.

Create the workspace for the subject asserted by AKS:

```bash
tau workspace create taugrid-default \
  --queue jobqueue \
  --principal-name "$RESEARCHER_UPN" \
  --subject-kind User \
  --subject-name "$RESEARCHER_UPN" \
  --apply

kubectl wait \
  --for=jsonpath='{.status.phase}'=Ready \
  workspace/taugrid-default \
  -n tau-system \
  --timeout=5m
tau workspace check taugrid-default
```

For a group handoff, use `--subject-kind Group` and the group value AKS places
in the token claim, since a display name never appears there.

## 6. Generate the research repository

Generate the non-secret connection descriptor, then copy the example into that
repository:

```bash
RESEARCH_REPO="$WORK_DIR/ray-tune-research"

tau workspace init-repo ray-tune-research \
  --output "$RESEARCH_REPO" \
  --image mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0 \
  --workspace taugrid-default \
  --azure-subscription-id "$TARGET_SUBSCRIPTION_ID" \
  --azure-tenant-id "$TENANT_ID" \
  --aks-resource-group "$RESOURCE_GROUP" \
  --aks-cluster "$AKS_CLUSTER"

cp "$TAUGRID_ROOT/examples/ray-tune-smoke/tau.yaml" "$RESEARCH_REPO/"
cp "$TAUGRID_ROOT/examples/ray-tune-smoke/train.py" "$RESEARCH_REPO/"

git -C "$RESEARCH_REPO" init
git -C "$RESEARCH_REPO" add .

cd "$RESEARCH_REPO"
tau workspace connection
```

The descriptor must contain `authorization.mode: workspace-rbac` and
`requiredRole: tau-researcher-v1`. It contains no kubeconfig, token, or client
secret.

## 7. Run the six-trial sweep using the resolved workspace connection

The example defines three learning rates and two batch sizes:

```text
3 learning rates x 2 batch sizes x 1 sample = 6 trials
```

It permits two concurrent trials, but each trial needs one GPU. With the
one-GPU pool above, Ray keeps the second trial pending and executes the sweep
sequentially. Adding more GPUs can increase concurrency; it is optional
capacity rather than a correctness requirement.

From the generated repository:

```bash
tau run --config tau.yaml --dry-run=client
tau run --config tau.yaml
```

TauGrid resolves the cluster, workspace namespace, and LocalQueue automatically
from `tau/workspace.connection.yaml`, so skip passing `--context`,
`--namespace`, or `--queue`.

Immediately capture the Ray Jobs application stream in one terminal:

```bash
tau run logs tune-smoke -f | tee tune-smoke.log
```

Watch lifecycle state in another:

```bash
tau run status tune-smoke --watch
```

The generated Tune driver stages the researcher source on remote
`TorchTrainer` workers and forwards arbitrary Ray Train metrics and checkpoint
paths into the outer Tune result grid, treating `loss` the same as any other
metric.

## 8. Require every success gate

Do not call the run successful until all of these are true:

| Gate | Expected evidence |
|---|---|
| Search space | The application log says `Number of trials 6`. |
| Metrics | All six configurations reach five training iterations and report their loss series. |
| Best result | The final log prints `Best config` and `Best loss`. For the checked-in deterministic example, the best config is `{'batch_size': 32, 'lr': 0.1}` and the final loss is `5.587555555555555`. |
| RayJob | `tau run status` reports `Complete` / `SUCCEEDED` and `Job finished successfully`. |
| Kueue | The Workload is `Finished`, admitted `true`, reason `Succeeded`. |
| Compute release | `tau run status` reports quota released and no active Ray pods. |
| ClusterQueue | `admittedWorkloads`, `pendingWorkloads`, and `reservingWorkloads` are zero; every flavor usage and reservation total is zero. |

The platform operator can inspect the final queue counters with:

```bash
kubectl get clusterqueue jobqueue \
  -o custom-columns='NAME:.metadata.name,ADMITTED:.status.admittedWorkloads,PENDING:.status.pendingWorkloads,RESERVING:.status.reservingWorkloads'
kubectl get clusterqueue jobqueue -o yaml
```

KubeRay removes the RayCluster pods shortly after completion. Post-run local
logs and `/home/nonroot/ray_results` persist only as long as the head pod
unless central log offload is configured. Keep `tau run logs -f` attached through completion, or configure
the platform's supported central log backend before the run. The checked-in
example prints its result grid and best result to the application stream,
which is the durable record; the head-local result directory is ephemeral
only.

## 9. Clean up and verify restoration

Capture logs and status first, then delete in dependency order:

```bash
kubectl delete rayjob tune-smoke \
  -n taugrid-default \
  --ignore-not-found

# If the optional smoke ran, delete its run with `tau run cancel <smoke-run>`.

kubectl delete workspace taugrid-default \
  -n tau-system \
  --ignore-not-found

# TauWorkspace cleanup intentionally retains its target namespace.
kubectl delete namespace taugrid-default --ignore-not-found

tau cluster uninstall \
  --chart "$TAUGRID_ROOT/charts/taugrid" \
  --yes \
  --wait

kubectl delete daemonset nvidia-device-plugin-daemonset \
  -n kube-system \
  --ignore-not-found

az group delete \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --name "$RESOURCE_GROUP" \
  --yes \
  --no-wait
```

Wait until both the primary and AKS-managed resource groups are gone, then
confirm the GPU-family usage returned to its pre-run value:

```bash
az group exists \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --name "$RESOURCE_GROUP"

az group exists \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --name "$MANAGED_RESOURCE_GROUP"

az vm list-usage \
  --subscription "$TARGET_SUBSCRIPTION_ID" \
  --location "$LOCATION" \
  --query "[?name.value=='$GPU_FAMILY'].[name.localizedValue,currentValue,limit]" \
  --output table
```

Remove the disposable research repository and isolated kubeconfig only after
checking that `WORK_DIR` is the expected temporary directory. If Helm generated
an untracked `charts/taugrid/Chart.lock` or `charts/taugrid/charts/`, inspect
`git status` and remove only those generated dependency artifacts.

Finally restore and verify the original local targets:

```bash
az account set --subscription "$ORIGINAL_SUBSCRIPTION_ID"
export KUBECONFIG="$ORIGINAL_KUBECONFIG"

az account show --query '[name,id]' -o table
test "$(kubectl config current-context)" = "$ORIGINAL_CONTEXT"
```

The cleanup is complete only when no disposable AKS resource group remains,
GPU quota usage has returned, and the original subscription and Kubernetes
context are active.
