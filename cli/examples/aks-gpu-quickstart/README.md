# TauGrid A100 GPU quickstart

> **Status:** `operator runbook`
> **Intended use:** stand up a disposable single-A100 AKS cluster, install
> TauGrid, and prove a real CUDA PyTorch run end to end with evidence a CPU
> cannot fabricate.
> **Not for:** production clusters, shared clusters you did not provision, or
> any cluster you are not willing to delete afterwards. An A100 node bills by
> the hour.
>
> **Blocked on [#1294] until it merges.** This example uses `runtime.pip`, and
> on `main` that install fails with `EACCES` on the pinned image before any
> launcher runs — so *both* the 1-GPU and multi-GPU paths exit 1. See
> [Known issues](#known-issues-this-example-works-around). Don't spend an A100
> hour on this until #1294 lands, or bake the deps into a custom
> `runtime.image` first.

Creates a brand-new AKS cluster with a single **NVIDIA A100 80GB** node,
installs TauGrid, and runs a PyTorch workload through Tau that **cannot pass on
a CPU**.

This is the GPU counterpart to [`aks-cpu-quickstart`](../aks-cpu-quickstart).
Same shape, same three tools, one extra node pool.

```
./cli/examples/aks-gpu-quickstart/run.sh      # ~25 min, creates billable resources
./cli/examples/aks-gpu-quickstart/cleanup.sh  # deletes everything
```

> **Cost warning.** An `Standard_NC24ads_A100_v4` node bills by the hour and is
> not cheap. Run `cleanup.sh` as soon as you have your evidence. `run.sh` prints
> the cleanup command at the end for exactly this reason.

## Why "Job Complete" is not proof

A workload that silently fell back to CPU also exits 0. Screenshots of a green
Kueue queue prove scheduling, not computation. So `train.py` is written so that
a CPU physically cannot produce its output — three gates, each fatal:

| Gate | Claim | Why a CPU cannot fake it |
|---|---|---|
| 1. Device identity | Compute capability 8.0 and ≥70 GiB VRAM, read **inside the Ray training actor** | `torch.cuda.is_available()` is `False` without a GPU; a non-A100 GPU reports a different capability |
| 2. Tensor-core throughput | ≥20 TFLOP/s sustained TF32 8192³ matmul | An A100 does 150–300 TFLOP/s; a server CPU is well under 5. The floor is deliberately far below A100 and far above CPU, so a shared or throttled GPU still passes but CPU fallback cannot |
| 3. On-device convergence | Every parameter and every batch asserted `.device.type == "cuda"`, and loss must decrease | A GPU that is *present but unused* passes gates 1 and 2 and fails here |

The gates are verified non-vacuous: running `probe_device()` or `train_loop()`
on a CPU-only machine raises `GATE 1 FAILED` rather than passing.

Evidence is printed to stdout between `=== TAU-GPU-EVIDENCE-BEGIN ===` and
`=== TAU-GPU-EVIDENCE-END ===` as JSON. **stdout is the durable copy**: in the
default configuration `/data` is an `emptyDir`, so the file `train.py` also
writes to `TAU_DURABLE_CHECKPOINTS_DIR` dies with the pod. Retrieve it with:

```bash
tau run logs tau-aks-gpu-quickstart -n taugrid-default --context tau-gpu-quickstart \
  | sed -n '/TAU-GPU-EVIDENCE-BEGIN/,/TAU-GPU-EVIDENCE-END/p'
```

## Prerequisites

- `az`, `tau`, `kubectl` on `PATH`, and an `az login` session.
- `helm`. You never invoke it. `tau cluster install` shells out to it
  internally, so it must exist. Both Helm 3 and Helm 4 work with current `tau`.
- A100 quota in your target region — see below.

`run.sh` invokes exactly `az`, `tau`, and `kubectl`. It generates no Kubernetes
manifests and calls no other infrastructure tool.

## Choosing a region

A100 quota is the binding constraint, and it is per-family and per-region.
`run.sh` checks it up front (step 0b) because discovering it *after* the cluster
exists costs ~15 minutes and leaves a billable resource group behind.

Find a region with headroom:

```bash
az vm list-usage --location <region> -o table | grep NCADS_A100_v4
```

The default is `swedencentral`. `NCADS_A100_v4` quota is region-specific, so
check where your subscription has capacity and override with
`TAU_QUICKSTART_LOCATION`.

Two quota traps worth knowing:

- **The family must match the SKU.** `Standard_NDASv4_A100` quota does not admit
  an `NC24ads` node, and `Standard_ND96asr_v4` needs 96 vCPU rather than 24.
- **Quota shown as free may be held by someone else's cluster.** In a shared
  subscription, check what is consuming a region before assuming you can use it.

## Choosing a GPU SKU

Default: `Standard_NC24ads_A100_v4` — 24 vCPU, **1× A100 80GB**, the smallest
A100 shape on Azure. One node is enough to prove the whole platform path, and it
is roughly an eighth of the cost of an 8-GPU `ND96` node.

Tau keeps the Ray **head** pod CPU-only on the system pool and gives each
dedicated execution worker `compute.gpus` GPUs. With `workers: 1` in `tau.yaml`
that means exactly one GPU pod, hence one GPU node. Setting `workers: 2`
requests a second A100 and therefore a second GPU node — it
still fits a 50 vCPU quota (2 × 24) but doubles the hourly cost, so the default
stays at the smallest configuration that proves the platform works.

## Why TauGrid needs no GPU-specific configuration

None. This is the interesting part, and it is worth not breaking.

The stock chart already handles GPUs:

- `charts/taugrid/values.yaml` gives the baseline `jobqueue` ClusterQueue a
  `nvidia.com/gpu` nominal quota.
- Its `taugrid-default` ResourceFlavor selects only `kubernetes.io/os: linux`
  and declares **no tolerations**.

So the A100 node pool must be created **untainted**, which is what `run.sh`
does. A tainted pool would never be admitted by Kueue without editing the chart,
and editing the chart per-cluster is exactly the "every TauGrid install looks
the same" invariant this repo maintains.

### Two AKS GPU gotchas the script handles for you

**1. AKS installs the driver but not the device plugin.** `gpuProfile.driver`
defaults to `Install`, so the host driver is present and healthy — but nothing
advertises `nvidia.com/gpu`, so every GPU pod stays `Pending` forever. `run.sh`
applies `nvidia-device-plugin.yaml` (step 2a), a digest-pinned MCR DaemonSet.
It needs `privileged: true` and a hostPath mount of `/dev`; with only
`NVIDIA_VISIBLE_DEVICES=all` it logs `No devices found. Waiting indefinitely.`
and never registers, which is a silent failure worth knowing about.

**2. `Standard_NC24ads_A100_v4` comes up with MIG enabled.** `az aks nodepool
show` reports `gpuInstanceProfile: null`, so this is not something AKS asked
for — it is the node/VM default. With MIG on and no MIG instances configured,
NVML still sees the card but CUDA cannot create a context. The signature is
distinctive:

- `nvidia-smi -L` → `NVIDIA A100 80GB PCIe` (looks fine)
- `cuInit()` → **100 `CUDA_ERROR_NO_DEVICE`**
- `torch.cuda.device_count()` → `1`, but `torch.cuda.is_available()` → `False`

`nvidia-smi -mig 0` only sets a *pending* state ("in use by another client"),
so step 2c also restarts the node pool VMSS to make the driver apply it. Both
halves are skipped when MIG is already `Disabled`.

Neither condition is diagnosed for you today. `tau cluster validate nodes`
discovers GPU nodes solely by reading allocatable `nvidia.com/gpu`, so both
failures above present as the same unhelpful line:

```
no GPU nodes found
```

Before MIG is disabled the node advertises no `nvidia.com/gpu` at all, so
`validate nodes` cannot see it and cannot tell you why. Check by hand instead —
this is what step 2c automates:

```bash
# MIG mode on the node (Enabled here means CUDA cannot create a context)
kubectl get nodes -l agentpool=a100 \
  -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}'
```

> Teaching `tau cluster validate nodes` to distinguish "no GPU hardware" from
> "GPU hardware present but advertising nothing" — including MIG enabled with no
> instances configured, and DRA `ResourceSlice` nodes — is a worthwhile
> follow-up, but it is not implemented, and this example does not claim it is.

> MIG is worth it only when you have many small single-GPU jobs. For jobs asking
> for one or more whole GPUs — the common case, and what this example does —
> leave MIG off. On A100s, changing MIG mode requires a node restart. MIG *with*
> instances configured is a supported request mode (`requestVia: mig`); only the
> empty state is a fault.

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
is needed — it only needs a recent enough host driver, which the AKS GPU node
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

On a mixed cluster Kueue picks a ResourceFlavor for you, which may not be the
hardware you want. There is no `--gpu-class` or `--queue` flag on `tau run`;
select the flavor in the config instead:

```yaml
policy:
  queue: jobqueue
  node_selector:
    kueue.azure.com/gpu-series: ndm-a100-v4   # from `kubectl get resourceflavor`
```

Match the key/value to the flavor's `spec.nodeLabels`.

Note that `scheduling.node_selector` is **not** a field, and the two config
schemas in this repo fail differently on it — verified both ways:

| Config schema | Used by | Unknown top-level key |
|---|---|---|
| managed workflow (`schema_version: 1`) | this example's `tau.yaml` | **silently ignored** — a `scheduling:` typo fails open and the job lands anywhere |
| direct run config (`engine:`) | [`../aks-cpu-quickstart/stellar-demo/tau.yaml`](../aks-cpu-quickstart/stellar-demo/tau.yaml) | hard error: `field scheduling not found in type runconfig.Config` |

So on the schema this example uses, a misspelled key gives you no warning at
all. Confirm the selector actually rendered:

```bash
tau run --config tau.yaml --namespace taugrid-default --dry-run=client \
  | grep -A2 nodeSelector
```

### Flex nodes have no PyPI egress

Nodes joined via AKS Flex (`kubernetes.azure.com/cluster=flex-*`) had no route
to PyPI in testing: `pip install torch` fails with
`[Errno 101] Network is unreachable`. `runtime.pip` is therefore unusable
there, and this is a distinct failure from the permission problem in "Known
issues" below -- the install never reaches the index at all, so neither the
`--user` fallback nor #1294 helps. Bake dependencies into a custom
`runtime.image` instead. Managed node pools in the same cluster did have
egress, so this is a per-pool property; confirm before relying on
`runtime.pip`.

## What `run.sh` does

| Step | Command | Note |
|---|---|---|
| 0 | `tau run --dry-run=client` | renders the RayJob offline |
| 0b | `az vm list-usage` | fails fast on insufficient quota |
| 1 | `az group create`, `az aks create` | CPU-only system pool |
| 2 | `az aks nodepool add` | the A100 pool, **untainted** |
| 2a | `kubectl apply -f nvidia-device-plugin.yaml` | AKS does **not** install this |
| 2b | `kubectl get nodes` poll | waits for `nvidia.com/gpu` to be *allocatable* |
| 2c | `nvidia-smi -mig 0` + `az vmss restart` | MIG is on by default; CUDA fails until it is off |
| 3 | `tau cluster install`, `tau cluster validate installation` | |
| 4 | `tau workspace create taugrid-default --apply` | NAME is optional; pass it explicitly to keep overrides aligned |
| 5 | `tau run smoke` | connectivity probe |
| 6 | `tau run --config tau.yaml` | the real workload |
| 7 | `tau run status` / `tau run logs` | |

Step 2b matters more than it looks: a GPU node reports `Ready` **before** the
device plugin has registered the device. Submitting in that window leaves the
pod `Pending` on `Insufficient nvidia.com/gpu` with no obvious cause, so the
script waits for the resource to be allocatable rather than for the node to be
Ready.

Steps 4–6 pass no `--workspace` and no `--namespace`. TauGrid v0 activates
exactly one workspace per cluster and defaults its name, so a researcher never
has to know it exists.

## Cleanup

```bash
./cli/examples/aks-gpu-quickstart/cleanup.sh
```

Cancels the workload, deletes the workspace, uninstalls TauGrid, then deletes
the whole resource group — which is the step that actually stops the billing.
Everything is idempotent, so it is safe to re-run after a partial failure.

Set `TAU_QUICKSTART_KEEP_CLUSTER=1` to keep the cluster for another run. The
script warns that the A100 keeps billing.

## Known issues this example works around

- **`runtime.pip` has no install fallback.** The RayJob entrypoint on `main`
  runs a bare `pip install --quiet --no-cache-dir ${PIP_PACKAGES}` with no
  retry. That install fails with `EACCES` and **the job exits 1** — before any
  launcher runs, so the 1-GPU and multi-GPU paths fail identically. Treat a
  `pip`-stage exit 1 as this problem rather than a fault in your script.

  ```
  ERROR: Could not install packages due to an OSError: [Errno 13]
  Permission denied: '/usr/lib/python3.12/site-packages/nvidia'
  ```

  **This is not CUDA-specific.** An earlier revision of this file claimed the
  plain `ray:py3.12-ray2.54.0` image was unaffected and that the denied
  `nvidia` directory was one "only the CUDA image ships". Both claims are
  false. Reproduced directly in the **plain** image:

  ```
  $ docker run --rm --platform linux/amd64 \
      mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0 \
      sh -c 'id; pip install --quiet --no-cache-dir "PyYAML>=6.0" "torch>=2.4.0"'
  uid=65532(nonroot) gid=65532(nonroot)
  ERROR: Could not install packages due to an OSError: [Errno 13]
  Permission denied: '/usr/lib/python3.12/site-packages/nvidia'
  ```

  The mechanism is now verified rather than guessed: the plain image has **no**
  `site-packages/nvidia` directory and does **not** ship `torch`. `torch` pulls
  `nvidia-*` wheels, so pip must *create* that directory inside
  `/usr/lib/python3.12/site-packages`, which is `drwxr-xr-x root root` while
  the runtime uid is 65532. The denial is on creation, not on a pre-existing
  CUDA directory.

  Two corollaries worth knowing when triaging:

  - Installing `PyYAML>=6.0` **alone** succeeds, because it is already present
    in the image, so pip writes nothing. A partial reproduction therefore looks
    green. It is `torch` that fails.
  - Because the cause is `torch` plus a root-owned `site-packages`, this
    affects **any** MCR Ray image, plain or CUDA.

  > **Addressed by [#1294], not yet merged.** That PR lands *both* halves
  > together: a `pip install --user` fallback, and a `PATH` export for
  > `$(python3 -m site --user-base)/bin`. Both are needed, because the fallback
  > alone merely moves the failure — pip then succeeds but its console scripts
  > land off `PATH`, so a multi-GPU run dies with `torchrun: command not found`
  > (exit 127) with torch imported fine, a considerably more confusing symptom
  > than the permission error.
  >
  > | State | Outcome |
  > |---|---|
  > | `main` today | no fallback; `pip install` → `EACCES`, **exit 1** |
  > | `--user` fallback alone | pip succeeds → `torchrun: command not found`, **exit 127** |
  > | #1294 (both halves) | works |
  >
  > Until #1294 merges, bake the dependencies into a custom `runtime.image`
  > rather than relying on `runtime.pip`. On `main` this affects every example
  > that puts `torch` in `runtime.pip` regardless of image variant, including
  > `ray-tune-smoke`, `cpu-multi-interest-ray`, and
  > [`aks-cpu-quickstart`](../aks-cpu-quickstart).

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
[#1294]: https://github.com/Azure/taugrid/pull/1294
