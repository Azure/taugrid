# TauGrid A100 GPU quickstart

> **Status:** `operator runbook`
> **Intended use:** create a disposable single-A100 AKS cluster, install
> TauGrid, and verify CUDA execution with device and training metrics.
> **Not for:** production clusters, shared clusters not provisioned for this
> purpose, or any cluster not intended for deletion afterward. An A100 node
> bills by the hour.

This example creates an AKS cluster with one **NVIDIA A100 80GB** node, installs
TauGrid, and runs a PyTorch workload that requires GPU execution.

This is the GPU counterpart to [`aks-cpu-quickstart`](../aks-cpu-quickstart).
It uses the same three tools and adds one GPU node pool.

```
./cli/examples/aks-gpu-quickstart/run.sh      # ~25 min, creates billable resources
./cli/examples/aks-gpu-quickstart/cleanup.sh  # deletes everything
```

> **Cost warning.** A `Standard_NC24ads_A100_v4` node bills by the hour. Run
> `cleanup.sh` after collecting the required evidence. `run.sh` prints the
> cleanup command when it finishes.

## Why job completion alone does not verify GPU execution

A workload can exit with status 0 after falling back to CPU. A successful
Kueue state verifies scheduling, not GPU computation. `train.py` uses three
checks that require GPU execution. Each check fails the run if its condition is
not met:

| Gate | Claim | CPU behavior |
|---|---|---|
| 1. Device identity | Compute capability 8.0 and ≥70 GiB VRAM, read inside the Ray training actor | `torch.cuda.is_available()` is `False` without a GPU; a non-A100 GPU reports a different capability |
| 2. Tensor-core throughput | ≥20 TFLOP/s sustained TF32 8192³ matmul | An A100 provides 150–300 TFLOP/s; a server CPU provides less than 5 TFLOP/s. The threshold allows a shared or throttled A100 and rejects CPU fallback. |
| 3. On-device convergence | Every parameter and every batch asserted `.device.type == "cuda"`, and loss must decrease | A GPU that is present but unused passes gates 1 and 2 and fails here |

On a CPU-only machine, `probe_device()` and `train_loop()` raise
`GATE 1 FAILED`.

Evidence is printed to stdout between `=== TAU-GPU-EVIDENCE-BEGIN ===` and
`=== TAU-GPU-EVIDENCE-END ===` as JSON. Capture stdout as durable evidence. In
the default configuration, `/data` is an `emptyDir`, so files written to
`TAU_DURABLE_CHECKPOINTS_DIR` are deleted with the pod. Retrieve the output
with:

```bash
tau run logs tau-aks-gpu-quickstart -n taugrid-default --context tau-gpu-quickstart \
  | sed -n '/TAU-GPU-EVIDENCE-BEGIN/,/TAU-GPU-EVIDENCE-END/p'
```

## Prerequisites

- `az`, `tau`, `kubectl` on `PATH`, and an `az login` session.
- `helm`. `tau cluster install` invokes it internally. Both Helm 3 and Helm 4
  work with current `tau`.
- No chart checkout or registry login. `tau` pulls its pinned TauGrid chart from
  the public MCR OCI registry.
- A100 quota in the target region. See below.

`run.sh` invokes exactly `az`, `tau`, and `kubectl`. It generates no Kubernetes
manifests and calls no other infrastructure tool.

## Choosing a region

A100 quota is specific to each VM family and region. `run.sh` checks quota in
step 0b. Detecting insufficient quota after cluster creation adds approximately
15 minutes and creates a billable resource group.

Check quota in the target region:

```bash
az vm list-usage --location <region> -o table | grep NCADS_A100_v4
```

The default is `swedencentral`. `NCADS_A100_v4` quota is region-specific, so
check where the subscription has capacity and override with
`TAU_QUICKSTART_LOCATION`.

Two quota constraints to check:

- **The family must match the SKU.** `Standard_NDASv4_A100` quota does not admit
  an `NC24ads` node, and `Standard_ND96asr_v4` needs 96 vCPU rather than 24.
- **Other clusters may consume the available quota.** In a shared subscription,
  check regional usage before creating the node pool.

## Choosing a GPU SKU

Default: `Standard_NC24ads_A100_v4`: 24 vCPU, **1× A100 80GB**, the smallest
A100 shape on Azure. This workflow requires one node. An 8-GPU `ND96` node
costs approximately eight times more per hour.

Tau keeps the Ray **head** pod CPU-only on the system pool and gives each
dedicated execution worker `compute.gpus` GPUs. With `workers: 1` in `tau.yaml`
that means exactly one GPU pod, hence one GPU node. Setting `workers: 2`
requests a second A100 and therefore a second GPU node. It
fits a 50 vCPU quota (2 × 24) but doubles the hourly cost. The default uses one
worker.

## Why TauGrid needs no GPU-specific configuration

TauGrid requires no GPU-specific configuration for this quickstart. The default
chart includes GPU support:

- `charts/taugrid/values.yaml` gives the baseline `jobqueue` ClusterQueue a
  `nvidia.com/gpu` nominal quota.
- Its `taugrid-default` ResourceFlavor selects only `kubernetes.io/os: linux`
  and declares **no tolerations**.

Create the A100 node pool without taints. `run.sh` uses this configuration.
Kueue cannot admit workloads to a tainted pool unless the chart defines the
required toleration.

### Two AKS GPU issues the script handles

**1. AKS installs the driver but not the device plugin.** `gpuProfile.driver`
defaults to `Install`, so the host driver is present and healthy. Nothing
advertises `nvidia.com/gpu`, so GPU pods remain `Pending`. `run.sh`
applies `nvidia-device-plugin.yaml` (step 2a), a digest-pinned MCR DaemonSet.
It needs `privileged: true` and a hostPath mount of `/dev`; with only
`NVIDIA_VISIBLE_DEVICES=all` it logs `No devices found. Waiting indefinitely.`
and never registers. This is a silent failure mode.

**2. `Standard_NC24ads_A100_v4` comes up with MIG enabled.** `az aks nodepool
show` reports `gpuInstanceProfile: null`, so this is not something AKS asked
for. It is the node/VM default. With MIG enabled and no MIG instances
configured, NVML detects the card but CUDA cannot create a context. Check these
results:

- `nvidia-smi -L` → `NVIDIA A100 80GB PCIe`
- `cuInit()` → **100 `CUDA_ERROR_NO_DEVICE`**
- `torch.cuda.device_count()` → `1`, but `torch.cuda.is_available()` → `False`

`nvidia-smi -mig 0` sets a pending state ("in use by another client"). Step 2c
restarts the node pool VMSS so the driver applies the change. The script skips
both operations when MIG is already `Disabled`.

Neither condition is diagnosed automatically. `tau cluster validate nodes`
discovers GPU nodes solely by reading allocatable `nvidia.com/gpu`, so both
failures produce the same output:

```
no GPU nodes found
```

Before MIG is disabled the node advertises no `nvidia.com/gpu` at all, so
`validate nodes` cannot identify the condition. Check the allocatable resource
directly. Step 2c automates this check:

```bash
# MIG mode on the node (Enabled here means CUDA cannot create a context)
kubectl get nodes -l agentpool=a100 \
  -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}'
```

> Distinguishing "no GPU hardware" from "GPU hardware present but advertising
> nothing," including MIG enabled with no instances configured, and DRA
> `ResourceSlice` nodes, would improve `tau cluster validate nodes`. This is
> not implemented today.

> Use MIG only for many small single-GPU jobs. For jobs that request one or
> more whole GPUs, the common case, and what this example does, leave MIG
> off. On A100s, changing MIG mode requires a node restart. MIG with instances
> configured is a supported request mode (`requestVia: mig`); only the empty
> state is a fault.

The only GPU-specific settings live in the researcher's `tau.yaml`:

```yaml
compute:
  gpus: 1
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
  pip: [torch>=2.4.0]
```

The image must be the **`-cuda`** tag. The CPU quickstart's plain
`py3.12-ray2.54.0` ships no CUDA runtime and gate 1 will fail on it. The default
PyPI `torch` wheel bundles its own CUDA runtime libraries, so no extra index URL
is needed. It only needs a recent enough host driver, which the AKS GPU node
image provides.

## Multiple GPUs, and running on an existing shared cluster

`tau.yaml` requests one GPU. `compute.gpus` and `compute.workers` control the
shape, and tau picks the launcher from them:

| `workers` | `gpus` | Launcher | What drives the ranks |
|---|---|---|---|
| 1 | 1 | `python3` | single process |
| 1 | N | `torchrun --standalone --nproc_per_node=N` | one process per GPU, one pod |
| >1 | N | `python3` | the script drives Ray Train across pods |

`train.py` handles the first two directly: it detects `RANK`/`LOCAL_RANK`/
`WORLD_SIZE` (set only by torchrun), pins `cuda:$LOCAL_RANK`, and wraps the
model in `DistributedDataParallel` instead of calling Ray Train. Only rank 0
prints the evidence block, so the output stays readable.

Verified on 8x A100-SXM4-80GB in one pod: ranks 0-7 each bound to their own
GPU, `driver_visible: 8`, 124.1 TFLOP/s, loss 4.1620 -> 0.9972, backend `nccl`.

### Targeting a specific GPU type

On a mixed cluster Kueue picks a ResourceFlavor automatically, which may not be
the intended hardware. There is no `--gpu-class` or `--queue` flag on `tau run`;
select the flavor in the config instead:

```yaml
policy:
  queue: jobqueue
  node_selector:
    kueue.azure.com/gpu-series: ndm-a100-v4   # from `kubectl get resourceflavor`
```

Match the key/value to the flavor's `spec.nodeLabels`.

`scheduling.node_selector` is not a field, and the two config
schemas in this repo fail differently on it. Verified both ways:

| Config schema | Used by | Unknown top-level key |
|---|---|---|
| managed workflow (`schema_version: 1`) | this example's `tau.yaml` | silently ignored: a `scheduling:` typo fails open and the job lands anywhere |
| direct run config (`engine:`) | [`../aks-cpu-quickstart/stellar-demo/tau.yaml`](../aks-cpu-quickstart/stellar-demo/tau.yaml) | hard error: `field scheduling not found in type runconfig.Config` |

The managed workflow schema silently ignores a misspelled key. Verify that the
selector is present in the rendered manifest:

```bash
tau run --config tau.yaml --namespace taugrid-default --dry-run=client \
  | grep -A2 nodeSelector
```

### Flex nodes have no PyPI egress

Nodes joined via AKS Flex (`kubernetes.azure.com/cluster=flex-*`) had no route
to PyPI in testing: `pip install torch` fails with
`[Errno 101] Network is unreachable`. `runtime.pip` is therefore unusable
there. This differs from the permission issue in "Known issues" below because
the installation never reaches the package index. The `--user` fallback and a
writable package directory do not resolve this condition. Include dependencies
in a custom `runtime.image`. Managed node pools in the same cluster had egress.
Verify egress for each node pool before using `runtime.pip`.

## What `run.sh` does

| Step | Command | Note |
|---|---|---|
| 0 | `tau run --dry-run=client` | renders the RayJob offline |
| 0b | `az vm list-usage` | fails fast on insufficient quota |
| 1 | `az group create`, `az aks create` | CPU-only system pool |
| 2 | `az aks nodepool add` | the A100 pool, **untainted** |
| 2a | `kubectl apply -f nvidia-device-plugin.yaml` | AKS does not install this |
| 2b | `kubectl get nodes` poll | waits for `nvidia.com/gpu` to be *allocatable* |
| 2c | `nvidia-smi -mig 0` + `az vmss restart` | MIG is on by default; CUDA fails until it is off |
| 3 | `tau cluster install`, `tau cluster validate installation` | |
| 4 | `tau workspace create taugrid-default --apply` | NAME is optional; pass it explicitly to keep overrides aligned |
| 5 | `tau run smoke` | connectivity probe |
| 6 | `tau run --config tau.yaml` | the real workload |
| 7 | `tau run status` / `tau run logs` | |

Step 2b handles a timing issue. A GPU node reports `Ready` before the device
plugin registers the device. Submitting during this interval leaves the pod
`Pending` with `Insufficient nvidia.com/gpu`. The script waits for the resource
to become allocatable.

Steps 4–6 do not require `--workspace` or `--namespace`. TauGrid v0 creates one
workspace per cluster with a default name.

## Cleanup

```bash
./cli/examples/aks-gpu-quickstart/cleanup.sh
```

The script cancels the workload, deletes the workspace, uninstalls TauGrid, and
deletes the resource group. Resource group deletion stops billing. All cleanup
steps are idempotent and can be rerun after a partial failure.

Set `TAU_QUICKSTART_KEEP_CLUSTER=1` to keep the cluster for another run. The
script warns that the A100 keeps billing.

## Known issues this example works around

- **`runtime.pip` runs under a non-root runtime.** Tau retries a failed system
  install with `pip install --user` and adds the matching user-site `bin`
  directory to `PATH`. This supports `torch` and its console scripts on the
  pinned Ray images. Package-index egress is still a separate prerequisite;
  use an image with dependencies preinstalled when the node cannot reach the
  approved index.

- **[#1288]** On Helm 4, `tau cluster install` and `uninstall` used to fail on
  server-side apply conflicts: AKS's `admissionsenforcer` addon co-owns
  `.webhooks[*].namespaceSelector` on Kueue's webhook configurations. Both verbs
  now send `--force-conflicts` when the installed Helm accepts it, which resolves
  the ownership standoff without changing what AKS put there. `run.sh` still
  passes `--wait=false --atomic=false` and gates on
  `tau cluster validate installation`.
- **[#1275]** A completed Ray job holds its node capacity for the TTL window.
  On a single-GPU cluster a second submission will sit unadmitted until the
  first job's RayCluster is reclaimed.

[#1275]: https://github.com/Azure/taugrid/issues/1275
[#1288]: https://github.com/Azure/taugrid/issues/1288
