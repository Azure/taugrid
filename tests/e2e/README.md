# Chart Integration Tests

Integration tests that deploy the vendored Helm charts to a real AKS cluster and verify the operators function correctly.

## Prerequisites

- **Go 1.26.5**
- **kubectl** configured with a kubeconfig pointing to an AKS cluster
- All 3 Helm charts installed on the cluster:
  - Kueue (`kueue-system` namespace)
  - KubeRay Operator (`kuberay-system` namespace)
  - GPU Monitoring (`gpu-monitoring` namespace)

## Running Locally

Tests are gated behind the `AI_RUNTIME_E2E=1` environment variable to prevent accidental execution:

```bash
cd tests/e2e

# Run all tests
AI_RUNTIME_E2E=1 go test -v -timeout 15m ./...

# Run only Kueue tests
AI_RUNTIME_E2E=1 go test -v -timeout 10m ./kueue/

# Run only KubeRay tests
AI_RUNTIME_E2E=1 go test -v -timeout 5m ./kuberay/

# Run warm-cluster GPU smoke e2e tests
AI_RUNTIME_E2E=1 AI_RUNTIME_GPU_E2E=1 \
AI_RUNTIME_GPU_A10_SELECTOR='agentpool=a10' \
AI_RUNTIME_GPU_A100_SELECTOR='agentpool=gpu' \
go test -v -timeout 20m ./managedgpu/

# Assert mixed-cluster collector source selection on managed A100 and GPU Operator H200 nodes
AI_RUNTIME_E2E=1 AI_RUNTIME_GPU_MONITORING_RUNTIME=1 \
AI_RUNTIME_GPU_MONITORING_A100_SELECTOR='<managed A100 selector>' \
AI_RUNTIME_GPU_MONITORING_H200_SELECTOR='<joined H200 selector>' \
go test -v -timeout 20m -run '^TestGPUMonitoringRuntimeOnGPUNode$' ./gpu-monitoring/

# Run the portable scheduler packing check against a multi-node kind cluster
AI_RUNTIME_E2E=1 go test -v -timeout 2m ./scheduler/

# Run the manual 16-GPU Ray Train conformance test against a prepared cluster
AI_RUNTIME_E2E=1 E2E_GPU=1 E2E_LARGE_GPU=1 \
RAY_E2E_IMAGE='<ray image>' \
GPU_NODE_SELECTOR_KEY='<gpu node selector key>' \
GPU_NODE_SELECTOR_VALUE='<gpu node selector value>' \
RAY_SUBMITTER_NODE_SELECTOR_KEY='<cpu node selector key>' \
RAY_SUBMITTER_NODE_SELECTOR_VALUE='<cpu node selector value>' \
NANOGPT_DATASET_URIS='<comma-separated pre-tokenized OpenWebText shard URLs>' \
NANOGPT_DATASET_SHA256S='<comma-separated 64-character hexadecimal SHA256s>' \
NANOGPT_DATASET_TOKEN_COUNTS='<comma-separated positive integer token counts>' \
go test -v -timeout 120m -run '^TestNanoGPTRayTrainLargeGPU$' ./stack/

# Run the Tau Python SDK entrypoint smoke against a prepared stack/flex cluster
AI_RUNTIME_E2E=1 E2E_TAU_PY_ENTRYPOINT=1 \
RAY_E2E_IMAGE='<ray image>' \
go test -v -timeout 30m -run '^TestTauPyEntrypointRayJob$' ./stack/

# Run the Tau Python SDK GPU entrypoint golden e2e on a selected GPU node
AI_RUNTIME_E2E=1 E2E_GPU=1 E2E_TAU_PY_ENTRYPOINT_GPU=1 \
RAY_E2E_IMAGE='<ray image>' \
TORCH_SPEC='torch==2.7.1' \
TORCH_INDEX_URL='https://download.pytorch.org/whl/cu128' \
GPU_NODE_SELECTOR_KEY='<gpu node selector key>' \
GPU_NODE_SELECTOR_VALUE='<gpu node selector value>' \
go test -v -timeout 35m -run '^TestTauPyEntrypointRayJobGPU$' ./stack/
```

The runtime monitoring test accepts
`AI_RUNTIME_GPU_MONITORING_<FAMILY>_SELECTOR` for `A10`, `A100`, `H100`,
`H200`, `GB200`, and `GB300`. Configure only families present in the target
cluster. Each case reads and probes the DCGM URL from the selected monitoring
pod's actual collector ConfigMap; it does not accept an independent test-only
URL.

Without `AI_RUNTIME_E2E=1`, cluster-backed integration tests are skipped. The
offline unit, fixture, payload, and fake-client tests still run and require no
Kubernetes cluster:

```bash
AI_RUNTIME_E2E=0 go test -count=1 ./...
```

## Test Structure

| Package | Tests | What It Verifies |
|---------|-------|-----------------|
| `kueue/` | 3 | Controller deployed, **gang scheduling** (blocked + admitted + pending) |
| `kuberay/` | 1 | Operator deployed |
| `gpu-monitoring/` | 3 | DaemonSet exists, per-SKU scrape/rules ConfigMap shape, opinionated default alert rule inventory, sidecar wiring |
| `managedgpu/` | 1 | Warm-cluster GPU smoke jobs on selected A10/A100 nodes |
| `scheduler/` | 2 | Kubernetes scheduler honors Tau's GPU bin-packing preferred pod affinity and packs single-device plus 2-4 GPU same-node pods onto already-occupied nodes |
| `stack/` | 7 | Full-stack Kueue → KubeRay → Ray Data **inference** pipeline, GPU variants, **training** SGD loop, Tau Python SDK CPU/GPU entrypoint submit tests, and manual 16-GPU Ray Train nanoGPT conformance |

The Tau Python SDK entrypoint smoke submits a CPU RayJob through
`tau.train(entrypoint=...)` and verifies a staged pure-Python/PyTorch-shaped
module/optimizer loop runs with sibling helper imports, without importing Tau or
writing subprocess wrappers. It defaults to one CPU-only system head plus one
execution worker to keep the flex smoke cheap; set
`TAU_PY_ENTRYPOINT_WORKERS=2` only when the selected `RAY_E2E_IMAGE` already has
`torch` available, or set `TAU_PY_ENTRYPOINT_RUNTIME_PIP` to a JSON list that
installs it for the Ray Train worker path.

The Tau Python SDK GPU entrypoint golden test is gated separately by
`E2E_TAU_PY_ENTRYPOINT_GPU=1` + `E2E_GPU=1`. It installs the configured CUDA
Torch wheel, submits a pure PyTorch file through Tau, requests
`nvidia.com/gpu: 1`, asserts the Ray head lands on the system pool and the
execution worker lands on the selected GPU node, and the user entrypoint fails
unless `torch.cuda.is_available()` is true.

### Gang Scheduling Tests (Kueue)

The Kueue tests include gang scheduling scenarios that validate all-or-nothing pod admission:

**Fixtures** (under `kueue/fixtures/`):
- `kueue-resources.yaml` — ResourceFlavor + ClusterQueue (3 CPU quota) + LocalQueue
- `job-gang-blocked.yaml` — 2 pods × 2 CPU = **4 CPU > 3 quota** → not admitted (insufficient quota)
- `job-gang-fits.yaml` — 2 pods × 1 CPU = **2 CPU ≤ 3 quota** → admitted, both pods running (1 CPU unused)
- `job-gang-pending.yaml` — 2 pods × 1 CPU = **2 CPU ≤ 3 quota but only 1 CPU free** → not admitted (insufficient unused quota)

The 3 CPU quota creates artificial scarcity. Each test asserts on three layers:
1. **Decision** — Kueue Workload condition (`Admitted=True` or `QuotaReserved=False`)
2. **Bookkeeping** — LocalQueue pending/admitted counters
3. **Consequence** — Running pod count (gang = all-or-nothing)

## How CI Runs These

The `.github/workflows/chart-integration-test.yaml` workflow:

1. Runs all clusterless/static e2e tests on every matching change.
2. Creates a fresh AKS cluster only when Kueue, KubeRay, GPU Monitoring, the Ray
   image, or their live e2e helpers and fixtures change.
3. Adds two A10 nodes through the AKS-managed GPU experience and installs the
   Kueue, KubeRay, and GPU Monitoring charts.
4. Runs the live CPU and GPU suites serially with
   `AI_RUNTIME_E2E=1 E2E_GPU=1`.
5. Collects diagnostics when infrastructure fails or is slow, then synchronously
   deletes the resource group.

Changes confined to static e2e assertions still run the clusterless suite without
provisioning AKS. Charts not installed by this workflow are covered by Helm CI
instead of triggering this fresh-cluster path.

When warm-cluster repo variables are configured (`AKS_FLEX_*`), the same
workflow also runs a long-haul managed GPU validation job against a persistent
AKS Flex cluster using `tests/e2e/managedgpu/harness/run.sh` for both A10 and
A100 targets (nodepool or explicit node selector as configured), then runs
`tests/e2e/managedgpu` against the same selectors.

The `.github/workflows/kueue-ray-large-gpu-conformance.yaml` workflow runs the
large live conformance target:

- `<cluster>` / H200 managed GPU / 16 GPUs
- `<cluster>` / managed A100 GPU node pool / 16 GPUs
- `<cluster>` / H200 Flex-joined eastuseuap / 16 GPUs
- Kueue-admitted KubeRay RayJob using an explicit pre-provisioned namespace and
  LocalQueue (default `taugrid-e2e/jobqueue`)
- Ray Train (`TorchTrainer`) with 16 GPU workers and `use_gpu=true`
- a bounded nanoGPT-style model over fixed pre-tokenized OpenWebText shards

The workflow separates direct worker/platform access from workload access. Direct
platform access performs node-capacity checks and controller diagnostics. The
workload route submits, watches, and cleans only the fixed nanoGPT RayJob in the
pre-provisioned stack namespace; it never applies/deletes
`stack-kueue-resources.yaml` or deletes the namespace. `workload_access_mode`
defaults to `platform-direct`, which explicitly reuses the direct platform
kubeconfig and preserves today's route. `manager` remains fail-closed until a
distinct manager workload kubeconfig/context is materialized with both a
different credential and a different cluster API server from the worker cluster;
the harness never calls `az aks get-credentials` or falls back to the current
kubectl context. Manager submission receives only the manager kubeconfig plus
the validated non-secret worker cluster identity; a separate platform-context
capacity recheck runs immediately before submission so late contention still
skips cleanly. Manager cleanup waits only for the fixed manager-visible RayJob
to disappear and never lists namespace Pods; platform-direct cleanup retains a
label-scoped drain for that RayJob's Pods. Pre-run stale RayJob deletion is
foreground and fail-hard so capacity is trustworthy. Final fixed-RayJob cleanup
is non-waiting and best-effort so an API/finalizer cleanup problem cannot
overwrite a successful conformance result; the next run repeats the fail-hard
reset.

Manual runs default to `target=all` and `workload_profile=conformance`, which
runs 2,000 optimizer steps on all three configured targets. Manual runs can also
select a single target, all H200 targets with `target=h200`, or the `smoke`
profile for a 1-step live-debugging shape. Scheduled runs are hourly
(`0 * * * *`) and run all configured large-GPU targets (`eastus2-h200`,
`flex-managed-a100`, and `flex-h200`) with the conformance profile.

Scheduled runs have an authorization-controlled maintenance gate. Set it only
after explicit coordinator/user approval by configuring both repository
variables:

- `TAU_QUEUE_MAINTENANCE_UNTIL=<YYYY-MM-DDTHH:MM:SSZ>` with a canonical UTC
  expiry no more than four hours in the future.
- `TAU_QUEUE_MAINTENANCE_CHANGE=<approved change or reason reference>` as one
  printable line.

An active valid window skips scheduled runs before Azure login; an expired
window resumes them automatically. Manual `workflow_dispatch` runs ignore the
gate. The gate does not cancel an in-flight run: operators must wait for the
current run to complete and verify zero workload usage before starting a queue
migration. Clear the gate by removing or emptying both
`TAU_QUEUE_MAINTENANCE_UNTIL` and `TAU_QUEUE_MAINTENANCE_CHANGE`. Do not use
the variables as standing configuration.

Required configuration:

| Name | Type | Purpose |
|------|------|---------|
| `AZURE_E2E_CLIENT_ID`, `AZURE_E2E_TENANT_ID`, `AZURE_E2E_SUBSCRIPTION_ID` | secrets | OIDC login for AKS credentials |
| `AKS_AI_RUNTIME_EASTUS2_RESOURCE_GROUP`, `AKS_AI_RUNTIME_FLEX_RESOURCE_GROUP` | vars | Target cluster resource groups; workflow inputs can override |
| `AKS_AI_RUNTIME_EASTUS2_CLUSTER_NAME`, `AKS_AI_RUNTIME_FLEX_CLUSTER_NAME` | vars | Optional cluster-name overrides; defaults are `<cluster>` and `<cluster>` |
| `LARGE_GPU_WORKLOAD_ACCESS_MODE` | var or `workload_access_mode` dispatch input | Workload credential contract: `platform-direct` (default) or `manager`. Manager mode requires a separately materialized credential whose cluster API server differs from the direct worker cluster |
| `LARGE_GPU_WORKLOAD_STACK_NAMESPACE` | var or `workload_stack_namespace` dispatch input | Pre-provisioned workload namespace; defaults to `taugrid-e2e` |
| `LARGE_GPU_WORKLOAD_STACK_QUEUE` | var or `workload_stack_queue` dispatch input | Pre-provisioned default LocalQueue; defaults to `jobqueue` |
| `LARGE_GPU_WORKLOAD_STACK_LARGE_GPU_QUEUE` | var or `workload_stack_large_gpu_queue` dispatch input | Pre-provisioned large-GPU LocalQueue; defaults to `jobqueue` |
| `AKS_AI_RUNTIME_EASTUS2_H200_SELECTOR`, `AKS_AI_RUNTIME_FLEX_MANAGED_A100_SELECTOR`, `AKS_AI_RUNTIME_FLEX_H200_SELECTOR` | vars | Optional `key=value` selectors whose Ready schedulable nodes have at least 16 available `nvidia.com/gpu`; defaults match the current cluster baseline. `AKS_AI_RUNTIME_FLEX_A100_SELECTOR` remains accepted as a compatibility fallback for the managed A100 pool on `<cluster>` |
| `AKS_AI_RUNTIME_EASTUS2_SUBMITTER_SELECTOR`, `AKS_AI_RUNTIME_FLEX_SUBMITTER_SELECTOR` | vars | Optional CPU pool selectors for Ray head and submitter pods; default is `kubernetes.azure.com/mode=system` |
| `NANOGPT_OPENWEBTEXT_URIS` | secret or var | Comma-separated Azure-accessible URLs for fixed pre-tokenized OpenWebText `.bin` shards |
| `NANOGPT_OPENWEBTEXT_SHA256S` | secret or var | Comma-separated SHA256 checksums matching the shard URLs |
| `NANOGPT_OPENWEBTEXT_TOKEN_COUNTS` | var | Comma-separated token counts matching the shard URLs |
| `RAY_NANOGPT_IMAGE` | var | Optional full Ray image override; otherwise the workflow selects the newest Ray/CUDA combo from `images/ray/versions.json` and resolves `mcr.microsoft.com/aks/ai-runtime/ray:<tag>` |
| `AKS_AI_RUNTIME_RAY_IMAGE_REGISTRY`, `AKS_AI_RUNTIME_RAY_IMAGE_REPOSITORY` | vars | Optional registry/repository override for the `versions.json` tag; defaults are `mcr.microsoft.com` and `aks/ai-runtime/ray` |
| `NANOGPT_TRAIN_STEPS`, `NANOGPT_MIN_TOTAL_TOKENS`, `NANOGPT_REPORT_EVERY` | vars | Optional conformance-profile workload controls; defaults are `2000`, `100000000`, and `25` |
| `NANOGPT_SMOKE_TRAIN_STEPS`, `NANOGPT_SMOKE_MIN_TOTAL_TOKENS`, `NANOGPT_SMOKE_REPORT_EVERY` | vars | Optional smoke-profile workload controls; defaults are `1`, `1`, and `1` |
| `NANOGPT_TORCH_SPEC`, `NANOGPT_TORCH_INDEX_URL` | vars | Optional fallback runtimeEnv torch wheel pin; defaults to `torch==2.7.1` from `cu128` |
| `NANOGPT_NCCL_SOCKET_IFNAME`, `AKS_AI_RUNTIME_*_NCCL_IB_DISABLE`, `NANOGPT_NCCL_DEBUG` | vars | Optional NCCL network controls; defaults favor `eth0`, no IB, and `WARN` diagnostics |

Dataset guidance:

The large-GPU Ray Train fixture does not mount a PVC. Each worker downloads one
fixed pre-tokenized shard from `NANOGPT_OPENWEBTEXT_URIS`, validates its SHA256,
and memory-maps it as `uint16` tokens. Tokenized `.bin` object size is therefore
`token_count * 2` bytes: 100,000,000 tokens is about 190.7 MiB, and 1,000,000,000
tokens is about 1.86 GiB. Keep shard URLs immutable and put SAS-bearing URLs in a
secret, not a repo variable. Each URI entry must be an `http` or `https` URL whose
path points directly at a `.bin` shard; token counts and SHA256s must stay in
their matching metadata lists, not appended to the URI path. Each SHA must be a
64-character hexadecimal digest, and each token count must be a positive integer.
The workflow also issues a `HEAD` request for every shard URL during preflight so
bad SAS tokens or nonexistent blob names fail before GPUs are scheduled.

Recommended dataset sources:

| Dataset source | Good use | Typical tokenized size | Notes |
|----------------|----------|------------------------|-------|
| nanoGPT/OpenWebText validation shard | Smoke/debug only | ~4.4M GPT-2 tokens, ~8.5 MiB | Useful with the smoke profile and `NANOGPT_SMOKE_MIN_TOTAL_TOKENS=1`; too small for the default conformance floor |
| OpenWebText, GPT-2-tokenized | Default conformance | Full train split is roughly 9B tokens, ~16.8 GiB | Closest match to the workload name; prefer sharded objects instead of one large blob |
| WikiText-103, GPT-2-tokenized | Minimal small-corpus conformance | Around the 100M-token class, ~190-250 MiB after tokenization | Small enough to manage easily; verify the final GPT-2 token count before setting `NANOGPT_OPENWEBTEXT_TOKEN_COUNTS` |
| FineWeb or FineWeb-Edu 10BT sample, GPT-2-tokenized | Soak/large-download validation | 10B tokens, ~18.6 GiB | Good when intentionally exercising larger data movement; overkill for hourly conformance |

Recommended shard shapes:

| Profile | Suggested object layout | Approximate dataset bytes | Notes |
|---------|-------------------------|---------------------------|-------|
| Smoke | 1 shard of at least a few million tokens | 10-20 MiB | Keeps manual H200 smoke/debug runs cheap; set the smoke minimum token override if using a tiny validation shard |
| Minimal conformance | 1 shard with >=100M tokens | >=190.7 MiB | Passes the default token floor, but all 16 workers download the same object |
| Balanced conformance | 16 shards x 50M tokens | ~1.49 GiB total, ~95.4 MiB per worker | Preferred default shape because `rank % len(uris)` gives each worker its own shard |
| Soak | 16 shards x 250M-500M tokens | ~7.45-14.9 GiB total | Use when the goal includes stressing dataset fetch and local cache behavior |

The large-GPU workflow fails before submitting the RayJob if selector parsing,
dataset metadata, explicit access contracts, AKS credentials, the pre-provisioned
namespace/queue, or available GPU capacity are invalid. Success means
the submitter logs contain `NANOGPT_RAY_TRAIN_SUCCESS step=<configured steps>
world_size=16`.
This is admission/scheduling/runtime conformance only; it is not a throughput,
NCCL, or model-quality benchmark.

The PR-gated chart integration workflow triggers on PRs touching `charts/**`,
`tests/e2e/**`, or the workflow file itself.

## Adding New Tests

1. **Create a test file** in the appropriate package (or create a new package)
2. **Use `e2e.NewTestContext(t, ctx)`** — provides K8s clients and helpers
3. **Add fixtures** under `<package>/fixtures/` for any YAML resources
4. **Use helpers** from `k8s.go`:
   - `tc.ApplyFixture(t, name)` — apply fixture YAML and register cleanup
   - `tc.WaitForDeploymentAvailable()` — poll deployment condition
   - `tc.WaitForDaemonSet()` — poll daemonset existence
   - `tc.WaitForWorkloadAdmitted()` / `tc.WaitForWorkloadQuotaNotReserved()` — Kueue workload conditions
   - `tc.GetLocalQueueCounts()` — Kueue LocalQueue pending/admitted counters
   - `tc.WaitForRunningJobPods()` — poll running pod count for a Job
5. **Clean up** resources in `t.Cleanup()` to avoid test pollution
