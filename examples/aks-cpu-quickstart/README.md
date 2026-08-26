# TauGrid CPU quickstart: from a new AKS cluster to a verified PyTorch run

> **Status:** `operator runbook`
> **Intended use:** create a disposable CPU-only AKS cluster, install TauGrid
> with `tau cluster install`, and verify a real PyTorch run end to end,
> including live Stellar metrics, before trying GPU/storage-specific flows.
> **Not for:** production clusters, GPU/CUDA workloads, or any cluster not
> intended for deletion afterward. This creates billable Azure resources.

This example shows the minimum path from no cluster to a verified result: a
real, CPU-only PyTorch training run submitted through `tau` and observed in
the Ray dashboard. It uses only three tools on the operator-facing path:

- `az`: create the AKS cluster and fetch its credentials (infra only).
- `tau`: install TauGrid onto the cluster and submit/observe the workload.
- `kubectl`: read-only verification and the couple of platform objects Tau's
  install docs don't yet automate (see caveats below).

No Terraform, Helm, `make`, or custom installer scripts are invoked directly.
`tau cluster install` itself shells out to a local `helm` binary internally.
That is a documented prerequisite of the Tau CLI, not something this example
invokes directly. See [Prerequisites](#prerequisites).

## AKS setup versus Kubernetes/TauGrid setup

AKS is the first-class provider path in this runbook. That means the repository
connection contract names an Azure tenant and AKS ARM resource and Tau can
obtain AKS cluster-user credentials. It does not mean Tau provisions AKS.

| Phase | Steps | Tool/API boundary | What it creates or validates |
|---|---|---|---|
| **Local validation** | 0 | Local `tau` process | Renders configs; creates no Azure or Kubernetes resources |
| **AKS setup (Azure/provider)** | 1 | `az` and Azure Resource Manager | Resource group, AKS control plane, node pool, API endpoint, and dedicated kubeconfig |
| **Kubernetes/TauGrid setup** | 2–3 | `tau`/Helm and the Kubernetes API | Kueue, KubeRay, Tau CRDs/controller, queue policy, TauWorkspace, Namespace, RBAC, and LocalQueue |
| **Workload validation (operator context)** | 4–7 | `tau` and the Kubernetes API | Smoke Job, RayJob, logs/status, and optional result retrieval |

The identity and storage split follows the same boundary:

- AKS setup owns managed Entra, AKS credential access, Azure authorization,
  OIDC, managed identities, Azure storage, networking, and provider add-ons.
- Kubernetes/TauGrid setup owns workspace RBAC, ServiceAccounts, queue objects,
  StorageClasses/PVCs, and workload objects that consume those AKS features.

Kind or another conformant Kubernetes cluster can verify the portable controller,
queue, and workload APIs. It does not exercise this runbook's AKS ARM identity,
credential, network, storage, or node-pool path.

Steps 4–7 keep using the operator's explicit `--context`. They
verify Tau's workload path, but they do not verify a repository-first researcher
handoff. For that first-class AKS path, AKS setup must also enable managed
Entra, grant the intended identity access to normal AKS cluster-user
credentials, and make the API reachable from the researcher. Kubernetes/
TauGrid setup must bind that real subject in a Ready workspace and hand over a
matching connection descriptor. The researcher then follows the
[first workload guide](../../site/content/en/docs/platform-admin-guide/kubernetes.md#5-run-the-smoke-workload)
without an operator context override.

This quickstart does not duplicate the training script. It reuses
[`examples/cpu-multi-interest-ray/train.py`](../cpu-multi-interest-ray/train.py)
unmodified: a CPU-only, 5-pod Ray Train PyTorch DDP demo (multi-interest
retrieval model, `arxiv:2506.23060v1`) that prints per-step loss and pairwise
accuracy. Only [`tau.yaml`](./tau.yaml) in this directory differs from the
shared demo's own config, and only in resource sizing (see
[Caveat 1](#caveat-1-head-pod-oomkill-on-a-real-cluster) below for why).

## What this verifies

1. A CPU-only AKS cluster can be created from scratch with `az`.
2. TauGrid (Kueue + KubeRay + Tau CRDs/controller + bootstrap queue) installs
   cleanly onto it with the single supported `tau cluster install` command.
3. A researcher can submit a real PyTorch workload through `tau run` alone,
   with no raw `kubectl apply` of Jobs/RayJobs. The workload trains: loss drops
   from ~3.7 to ~0.3–0.7 across 4 distributed CPU workers, rather than only
   reaching "Pod Running" or "Job Complete" status.
4. The live run and its logs are inspectable through both the Tau CLI
   (`tau run status` / `tau run logs`) and the Ray dashboard.
5. Tau's Stellar experiment-tracking UI deploys through the same
   `tau cluster install` command in local (non-Kusto) mode. Using the
   companion [`stellar-demo/`](./stellar-demo) script, which calls the
   `tau.stellar` SDK directly, Stellar logs and renders live metrics: a
   "Train loss" chart converging 3.76 → 0.72 and a
   `train/pairwise_accuracy` chart oscillating ~0.94–0.98 across 48/48
   steps, plus the run's config, final metrics, and content-addressed
   artifacts (`history.jsonl`, `top_recommendations.json`). This was
   confirmed in the dashboard UI, not only through `curl`/API calls.
   See [Recorded evidence](#recorded-evidence) below.

## Prerequisites

- `az` CLI, logged in, with a subscription that can create AKS clusters.
- `tau`: build locally from this repo (`make install-tau-cli` from the repo
  root) or use a published binary. This example was built/run against the
  Go CLI in `cli/`.
- `kubectl`: for read-only verification and the two objects noted in
  [Caveats](#caveats-known-gaps-in-the-supported-flow).
- **`helm` on `PATH`.** `tau cluster install` shells out to `helm`
  internally to render/apply the TauGrid chart. This is an implementation
  detail of the command, not something invoked directly here. Nothing in this
  example calls `helm` directly; if `tau cluster install` ever stops needing
  it, this note becomes obsolete.

  > **Helm 3 and Helm 4 both work.** Earlier versions of `tau cluster install`
  > called `helm list --all`; Helm v4 removed that flag, so the install failed
  > with a bare `Error: unknown flag: --all`. That is fixed
  > ([#1274](https://github.com/Azure/taugrid/issues/1274)):
  > the release probe now passes the individual state flags, which both major
  > versions accept, so no Helm version pin is required.
  >
  > If running a `tau` build from before that fix and
  > `helm version --short` reports `v4.x`, put a Helm 3 binary ahead of it on
  > `PATH` for the duration of the install:
  >
  > ```bash
  > export PATH="$(brew --prefix helm@3)/bin:$PATH"
  > helm version --short   # expect v3.x
  > ```
- No chart checkout or registry login. The installed `tau` binary pulls its
  pinned TauGrid chart from the public MCR OCI registry.

## Cost and time

This example creates **billable Azure resources**. Budget accordingly and run
[Cleanup](#cleanup) after finishing.

| Phase | Wall-clock | Notes |
| --- | --- | --- |
| `az group create` + `az aks create` | ~6–9 min | Dominated by AKS control-plane provisioning. |
| `tau cluster install` | ~3–5 min | Pulls/installs Kueue, KubeRay, Tau CRDs + controller. |
| Workspace + smoke test | <1 min | |
| First `tau run` (image pull is cold) | ~5–8 min | Subsequent runs are ~2–3 min once the Ray image is cached on the nodes. |
| Cleanup (`tau cluster uninstall` + `az group delete`) | ~8–12 min | `az group delete` dominates; the uninstall itself is ~1–2 min. |
| **Total** | **~25–35 min** | |

Compute cost is 3× `Standard_D4_v5` (4 vCPU / 16 GiB each). At list price in
`eastus2` that is roughly **$0.60–0.70/hour** for the node pool; the AKS free
tier control plane adds nothing. A full run-and-delete cycle is well under a
dollar. **The risk is forgetting to delete it.** An idle cluster left running
is ~$15/day. Confirm with `az group show --name taugrid-cpu-quickstart-rg`
returning `ResourceGroupNotFound` after cleanup.

## Design decisions (safe naming / no collisions)

- Region: `eastus2` (matches the existing `taugrid-*` test clusters).
- Dedicated resource group: `taugrid-cpu-quickstart-rg`. It does not
  collide with any existing RG, and makes final cleanup a single
  `az group delete`.
- Cluster name: `tau-cpu-quickstart`, 3x `Standard_D4_v5` CPU-only system
  nodes, default/current AKS Kubernetes version.
- Workspace/queue: the single canonical TauWorkspace `taugrid-default` in
  namespace `taugrid-default`, created via `tau workspace create taugrid-default`
  (not hand-written YAML), so this cluster matches a stock install.

## Step-by-step

All commands below assume the repo root is the working directory and
`KUBECONFIG` points at a dedicated file for this cluster (never merge into the
default kubeconfig for a disposable quickstart cluster):

```bash
export KUBECONFIG=/tmp/tau-cpu-quickstart.kubeconfig
```

### 0. Validate the configs before spending money (no cluster required)

`--dry-run=client` renders the full RayJob manifest locally without contacting
a cluster, catching config errors before provisioning anything. Run
this first: it takes seconds and needs no Azure resources:

```bash
tau run --config examples/aks-cpu-quickstart/tau.yaml \
  --namespace taugrid-default --dry-run=client

tau run --config examples/aks-cpu-quickstart/stellar-demo/tau.yaml \
  --namespace taugrid-default --dry-run=client
```

Both should print a complete RayJob spec (head + worker pod templates, the
base64 `TAU_PAYLOAD_B64` script payload, Kueue labels, resource requests) and
exit without an error. `--namespace taugrid-default` is only needed for this offline
render, because `--dry-run=client` never contacts a cluster and so cannot look
the namespace up. Every later command omits it: TauGrid v0 activates exactly
one workspace per cluster, and `tau` resolves that workspace (and its
namespace, queue, and output root) automatically. Researchers never need to
know the workspace name. `--workspace` exists only as an operator override.

### 1. AKS setup: create the resource group and CPU-only cluster (`az` only)

```bash
az group create --name taugrid-cpu-quickstart-rg --location eastus2

az aks create \
  --resource-group taugrid-cpu-quickstart-rg \
  --name tau-cpu-quickstart \
  --location eastus2 \
  --node-count 3 \
  --node-vm-size Standard_D4_v5 \
  --generate-ssh-keys

az aks get-credentials \
  --resource-group taugrid-cpu-quickstart-rg \
  --name tau-cpu-quickstart \
  --file "$KUBECONFIG"
```

This abbreviated disposable-cluster command does not enable managed Entra; it
is sufficient only for the operator-context validation below. Do not present
its local AKS credential as a researcher handoff. A real handoff must meet the
managed-Entra AKS gate described above before Kubernetes/TauGrid workspace
setup begins.

### 2. Kubernetes/TauGrid setup: install the control plane (`tau` only)

```bash
tau cluster install \
  --context tau-cpu-quickstart
```

The CLI uses a pinned public MCR chart reference and a compatible chart version.
This command does not require a TauGrid source checkout. Contributors can pass
`--chart ./charts/taugrid` to test repository changes.

This installs Kueue, KubeRay, the Tau CRDs/controller, and the bootstrap
GPU/CPU queue objects. Confirm with the Tau-native check (not `kubectl wait`):

```bash
tau cluster validate installation --context tau-cpu-quickstart
```

Optionally, also enable Stellar (Tau's local experiment dashboard) in the same
install call. See [Enabling Stellar](#enabling-stellar-local-mode) below.

### 3. Kubernetes/TauGrid setup: create the researcher workspace (`tau` only)

```bash
# NAME defaults to taugrid-default. Pass it explicitly so an override remains
# aligned with the status command that follows.
tau workspace create taugrid-default \
  --principal-name quickstart-researcher --context tau-cpu-quickstart --apply

# `tau workspace status` reports; it does not wait. Poll until phase=Ready.
tau workspace status taugrid-default --context tau-cpu-quickstart
```

### 4. Workload validation: connectivity smoke test (`tau` only)

```bash
tau run smoke --context tau-cpu-quickstart
```

### 5. Workload validation: submit the real PyTorch workload (`tau` only)

```bash
tau run --config examples/aks-cpu-quickstart/tau.yaml --context tau-cpu-quickstart
```

### 6. Workload validation: observe it running

```bash
tau run status tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart
tau run logs tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart -f
```

Or watch it live in the Ray dashboard:

```bash
kubectl port-forward -n taugrid-default svc/<raycluster-name>-head-svc 18265:8265
# then open http://localhost:18265/#/jobs
```

(Find `<raycluster-name>` via `kubectl get raycluster -n taugrid-default`.)

**Expected log output**: pip install of `torch`, then Ray Train
starting a 4-worker DDP process group, then repeated lines like:

```
(RayTrainWorker pid=849) rank=2/4 step=0  loss=3.7385 pairwise_acc=0.922
(RayTrainWorker pid=849) rank=2/4 step=12 loss=0.4032 pairwise_acc=0.969
...
SUCC -- Job 'tau-aks-cpu-quickstart-...' succeeded
```

The loss must drop from approximately 3.7 to 0.3–0.7 across all four workers.
The final status must be `succeeded`. A `RUNNING` or `Complete` state without
these metrics is insufficient.

A full captured transcript from two independent runs is saved as durable
evidence in this session's artifact log (see the creator session /
`files/evidence/` for `tau_run_logs_full_transcript.txt` and
`tau_run_logs_second_run_transcript.txt`). See
[Recorded evidence](#recorded-evidence) below for the full researcher-journey
video, which also verifies the Stellar metrics case.

### 7. Workload validation: retrieve artifacts (optional)

```bash
tau run get tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart
```

## Enabling Stellar (local mode)

Stellar is Tau's built-in experiment-tracking UI. Enabling it in local
(non-Kusto) mode is a single additional `--set` flag on the same
`tau cluster install` call. It does not require the separate
`taugrid-portal` binary/image beyond what the chart already references, and
does not require ADX/Kusto:

```bash
# The portal image is published only when a PR changes its trusted inputs.
# A docs-only commit might not have an image. Do not derive this tag from
# `git rev-parse --short HEAD`. The `latest` tag is mutable and must not be used
# on a cluster (AGENTS.md > "Container image sourcing"). The chart only verifies
# that stellar.image.tag is non-empty.
#
# List published tags:
#   curl -fsSL \
#     https://mcr.microsoft.com/v2/aks/ai-runtime/taugrid-portal/tags/list
#
# NOTE: the chart takes a tag only. templates/stellar.yaml renders
# "{{ .repository }}:{{ .tag }}" and has no digest support. Use the release tag
# matching the taugrid-core chart and verify it resolves before installing.
STELLAR_TAG="0.3.1"

# Verify the public image before starting the rollout.
curl --fail --silent --show-error --output /dev/null \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  "https://mcr.microsoft.com/v2/aks/ai-runtime/taugrid-portal/manifests/${STELLAR_TAG}" \
  || { echo "portal tag '$STELLAR_TAG' does not resolve; pick a current one"; exit 1; }

tau cluster install \
  --context tau-cpu-quickstart \
  --set taugrid-core.stellar.enabled=true \
  --set taugrid-core.stellar.source=local \
  --set taugrid-core.stellar.workspace=taugrid-default \
  --set taugrid-core.stellar.expstore.pvcName=tau-stellar-expstore \
  --set taugrid-core.stellar.image.tag="$STELLAR_TAG" \
  --wait --timeout 8m
```

> **Note:** this release tag must be published to MCR before the chart is used.
> If the preflight fails, wait for MCR syndication or choose another published
> chart and matching image release.

See [Caveat 2](#caveat-2-stellar-needs-a-pvc-created-out-of-band) for the one
manual `kubectl` step this currently needs.

The following port-forward is an operator-only local diagnostic for this
admin-installed quickstart. It is not a supported researcher endpoint or
browser-signoff path.

```bash
kubectl port-forward -n tau-system svc/tau-stellar 18080:80
# then open http://localhost:18080/stellar
```

### Verifying live Stellar metrics: the `stellar-demo/` script

The shared `cpu-multi-interest-ray/train.py` reused above only prints to
stdout. It does not call the `tau.stellar` SDK, so Stellar correctly shows
"No experiments found yet" for that run. To verify Stellar's actual job (real
metrics logged by a researcher's training loop, rendered live in the
dashboard), this quickstart ships a second, self-contained script,
[`stellar-demo/train.py`](./stellar-demo/train.py), that calls
`stellar.init(...)` / `run.log(...)` / `run.log_artifact(...)` /
`run.finish(...)` every step, the same SDK calls a real researcher project
would import. Submit it with its own `tau.yaml`
([`stellar-demo/tau.yaml`](./stellar-demo/tau.yaml), which uses the direct
`tau run --config` schema so `run.working_dir` can ship the `tau/` package
containing `stellar.py` alongside the script. See the comments in that file
for why):

```bash
tau run --config examples/aks-cpu-quickstart/stellar-demo/tau.yaml --context tau-cpu-quickstart
```

This produces a real, converging training curve (Train loss 3.76 → 0.72,
`train/pairwise_accuracy` oscillating ~0.94–0.98 over 48 steps) plus a final
`config.json`, `run.json`, `history.jsonl`, and an `artifacts/` folder
(`top_recommendations.json`) written under `/tmp/tau_stellar_demo/<run-id>/`
inside the pod. Use the retrieval procedure in
[Caveat 6](#caveat-6-kubectl-cp-fails-on-the-ray-image-use-kubectl-exec-cat-instead).**

After retrieval, serve the experiment store locally with `taugrid-portal`.
This does not require port-forwarding to the in-cluster Stellar Deployment.
Install `taugrid-portal` from this repository with
`make install-taugrid-portal`:

```bash
taugrid-portal experiment serve --store /path/to/retrieved/tau_stellar_demo --addr 127.0.0.1:8099
# then open http://127.0.0.1:8099/stellar?target=<run-group-name>
```

The "Train loss" and "train/pairwise_accuracy" charts must render per-step data
instead of the Stellar empty state. The evidence details must show the
configuration, final metrics, and both artifacts. See
[Recorded evidence](#recorded-evidence).

## Running a sweep (multiple experiments)

The `stellar-demo/` directory ships five sweep configs so the Stellar dashboard
has more than one experiment to compare:

| Config | Variant | Experiment |
|---|---|---|
| `sweep-lr-baseline.yaml` | `lr-2e-3` | `lr-sweep` |
| `sweep-lr-low.yaml` | `lr-5e-4` | `lr-sweep` |
| `sweep-lr-high.yaml` | `lr-8e-3` | `lr-sweep` |
| `sweep-lr-extreme.yaml` | `lr-5e-2-unstable` | `lr-sweep` |
| `sweep-long-run.yaml` | `steps-96` | `schedule-study` |

Each config sets `DEMO_LR`, `DEMO_STEPS`, `DEMO_VARIANT`, and `DEMO_EXPERIMENT`
env vars that `train.py` reads. (`TAU_*` env keys are reserved by the Tau
contract, hence the `DEMO_*` prefix.)

### Making a run last long enough to watch

The sweep configs above are deliberately tiny. 48 steps is roughly **2.4
seconds** of actual compute, against roughly **4 minutes** of startup (image
pull, then `pip install torch` in the Ray runtime env). That is fine for
producing metrics, but opening the Ray or Kueue dashboards
"while the job runs" almost certainly shows startup, not
training.

`train.py` therefore supports two extra, default-off knobs:

| Env var | Default | Effect |
|---|---|---|
| `DEMO_MAX_SECONDS` | `0` (off) | Stop the training loop after this many seconds of wall clock, regardless of `DEMO_STEPS`. |
| `DEMO_LOG_EVERY` | `0` (off) | Log every N steps instead of the built-in cadence. |

A wall-clock bound is used rather than a step count because the per-step rate
is node-dependent (~7.4 steps/sec on `Standard_D4_v5`, roughly 20x faster on an
Apple-silicon laptop), so a step count that lasts 20 minutes on one machine
lasts 60 seconds on another.

`stellar-demo/demo-live-run.yaml` uses these to produce a ~25 minute run:

```yaml
DEMO_STEPS: "1000000"      # effectively unbounded
DEMO_MAX_SECONDS: "1500"   # the real bound
DEMO_LOG_EVERY: "25"
```

Both knobs default to `0`, so every existing sweep config behaves exactly as
before.

Submit them one at a time:

```bash
for c in sweep-lr-baseline sweep-lr-low sweep-lr-high sweep-lr-extreme sweep-long-run; do
  tau run --config "stellar-demo/$c.yaml" --context "$CLUSTER"
done
```

Observed results on the reference cluster (48 steps unless noted):

| lr | final `train/loss` | final `train/pairwise_accuracy` |
|---|---|---|
| 5e-4 | 0.712 | 0.961 |
| 2e-3 | 0.723 | 0.945 |
| 8e-3 | 0.733 | 0.938 |
| 5e-2 | **1.260** | **0.867** |
| 2e-3, 96 steps | 0.659 | 0.922 |

The `5e-2` variant exists to produce visible separation on the charts: the
task converges within 48 steps at any reasonable learning rate, so only a
deliberately destabilizing learning rate produces a different result.

> **Capacity limit.** On the default 3x `Standard_D4_v5` pool only about two
> sweep jobs fit concurrently, and **a completed RayJob keeps its head and
> worker pods `Running`, still occupying node CPU, until its TTL expires**.
> A job can therefore report `SUCCEEDED` while continuing to consume the
> cluster, so follow-on runs make no progress.
>
> This is because `shutdownAfterJobFinishes` and `ttlSecondsAfterFinished`
> compose: the TTL is the delay before cleanup, not an independent knob.
> Tau now sets a 10-minute TTL, so finished runs release capacity on their own
> shortly after completing. Previously it was 24 hours, which effectively
> wedged small clusters
> ([#1275](https://github.com/Azure/taugrid/issues/1275)).
> To reclaim capacity immediately rather than waiting out the TTL:
>
> ```bash
> kubectl delete rayjob <finished-run> -n taugrid-default --context "$CLUSTER"
> ```
>
> Extract each run's Stellar data before deleting it (see below). Queueing
> behind *running* jobs is normal Kueue backpressure and is what the Kueue
> board shows; queueing behind *finished* jobs is the TTL, not backpressure.

### Pulling Stellar data off a run

Training runs on the Ray **worker** pod, and the Ray image has **no `tar`**
(and `kubectl cp` fails), so copy the files individually:

```bash
W=$(kubectl get pods -n taugrid-default --context "$CLUSTER" -o name \
      | grep "<run>.*worker" | head -1 | cut -d/ -f2)
RUN=$(kubectl exec -n taugrid-default --context "$CLUSTER" "$W" -c ray-worker -- \
      sh -c 'ls /tmp/tau_stellar_demo')
mkdir -p "/tmp/stellar/$RUN/artifacts"
for f in history.jsonl config.json run.json; do
  kubectl exec -n taugrid-default --context "$CLUSTER" "$W" -c ray-worker -- \
    cat "/tmp/tau_stellar_demo/$RUN/$f" > "/tmp/stellar/$RUN/$f"
done
```

Then import each run and assign it to an experiment:

```bash
taugrid-portal experiment init taugrid-cpu-quickstart --store /tmp/store
taugrid-portal experiment track "$RUN" --store /tmp/store \
  --project taugrid-cpu-quickstart --group stellar-demo --config "$d/config.json"
taugrid-portal experiment import jsonl --history "$d/history.jsonl" \
  --store /tmp/store --project taugrid-cpu-quickstart --run "$RUN" --group stellar-demo
taugrid-portal experiment experiments tag-run "$RUN" --store /tmp/store \
  --experiment lr-sweep --name "Learning-rate sweep"
```

> `import jsonl` takes the file via the **`--history` flag**, not as a
> positional argument.

### Experiments, not "questions"

Stellar's hierarchy is **project -> experiment -> run**. The older `question`
axis was folded into `experiment` by the expstore v1->v2 migration
(`portal/internal/expstore/migrate.go`), so:

* `taugrid-portal experiment init` takes `--description`, not `--question`.
* `taugrid-portal experiment track` takes neither `--question` nor
  `--experiment`; use `experiment experiments tag-run` to attach a run to an
  experiment.
* In Python, pass `stellar.init(..., experiment="lr-sweep")`. The
  `question=`/`question_id=` keywords still work as deprecated aliases.

> If `experiment init` rejects `--description` or demands `--question`, this is a
> **stale `taugrid-portal` binary** from before the migration. Build
> from source and use that binary.

## Viewing Ray and Kueue dashboards

Both are reverse-proxied by the TauGrid Portal, which must run **in-cluster**
(the proxy resolves targets via in-cluster DNS, so a local portal against
port-forwards will not work).

Portal is enabled by default in `tau-system`. Enable KueueViz and point the Portal at this example's workspace during install:

```bash
tau cluster install --context "$CLUSTER" \
  --set kueue.enableKueueViz=true \
  --set-string taugrid-core.portal.workloadNamespace=taugrid-default \
  --set-string taugrid-core.portal.kueueviz.namespace=tau-system \
  --set-string taugrid-core.portal.kueueviz.backendService=taugrid-kueue-kueueviz-backend \
  --set-string taugrid-core.portal.kueueviz.frontendService=taugrid-kueue-kueueviz-frontend \
  --set-string kueue.kueueViz.backend.env[0].name=KUEUEVIZ_ALLOWED_ORIGINS \
  --set-string kueue.kueueViz.backend.env[0].value=http://portal.kueueviz.local
```

This setup relies on the following contracts:

* **The umbrella distribution creates Portal in `tau-system` by default** with a dedicated ServiceAccount and read-only Kubernetes RBAC. The standalone `taugrid-core` child chart keeps its own explicit opt-in defaults.
* **`portal.workloadNamespace` scopes the legacy Runs and Ray views** to `taugrid-default` here. The computed Jobs board is separate and remains disabled until the platform configures `portal.jobs.scopeMode` with reviewed scopes and policy.
* **KueueViz Deployments land in the release namespace (`tau-system`)**, named
  `taugrid-kueue-kueueviz-{backend,frontend}`. The portal's kueueviz defaults
  point at `kueue-kueueviz-*` in `kueue-system`, so they must be overridden.
* `portal.source=local|auto` requires a mounted store and the chart `fail`s
  without one. `source=kusto` runs store-less; Kusto-backed boards return 503
  while the Jobs / Ray / Kueue / Fleet boards serve normally.
* The umbrella chart pins the Portal image to its release-aligned tag. Override the tag or digest only when testing an explicitly published artifact.

For an operator-only local diagnostic:

```bash
kubectl port-forward svc/tau-portal -n tau-system --context "$CLUSTER" 8088:80
```

Do not use this port-forward as researcher acceptance. Hosted browser access
requires the platform-owned authenticated HTTPS proxy contract from the
`taugrid-core` chart.

| Board | Path | Shows |
|---|---|---|
| Kueue (Scheduler) | `/portal/jobs` | admitted/pending per lane and queue |
| Kueue (Live) | `/portal/jobs?view=live` | KueueViz over WebSocket |
| Ray | `/portal/ray` | per-cluster dashboards, proxied live |
| Fleet | `/portal/nodes` | node readiness and capacity |

The Ray dashboard is only reachable **while the RayCluster is running**.

### Watching the dashboards during a run

To observe Ray and Kueue during a run, use a run duration set by
`DEMO_MAX_SECONDS` and submit more work than the cluster can admit
simultaneously. On a 3x4-vCPU cluster, submit `demo-live-run.yaml` and three
sweeps:

```bash
tau run --config stellar-demo/demo-live-run.yaml --context "$CLUSTER"
for c in sweep-lr-low sweep-lr-high sweep-lr-extreme; do
  tau run --config "stellar-demo/$c.yaml" --context "$CLUSTER"
done
```

The Kueue Live board then shows a mix of `Admitted` and `Not admitted`
workloads, and the Ray job detail page streams live driver logs.

Stellar does not stream metrics in this example. `train.py` runs with
`sync=False`, so it writes append-only JSONL inside the pod. To inspect an
active run, retrieve and import the file. `kubectl cp` does not work with this
image; use `exec ... -- cat`:

```bash
kubectl exec -n taugrid-default "$POD" --context "$CLUSTER" -- \
  cat /tmp/tau_stellar_demo/<run>/history.jsonl > /tmp/history.jsonl
taugrid-portal experiment import jsonl --store /tmp/store --history /tmp/history.jsonl ...
```

The Ray and Kueue dashboards update during the run. Stellar data requires the
retrieval step above.

## Caveats (known gaps in the supported flow)

### Caveat 1: head pod OOMKill on a real cluster

The shared `cpu-multi-interest-ray` demo's default `compute.memory: 2Gi` head
limit OOMKills on a real AKS cluster once Ray GCS + dashboard + 4 connecting
workers are running (`kubectl describe rayjob ... ->` `HeadPodReady: False,
Reason: OOMKilled`). There is no `tau run` CLI flag to override
`compute.*` fields at submit time (see `docs/tau/tau-run-config.md`), so this
quickstart keeps its own copy of the config
([`tau.yaml`](./tau.yaml)) with `compute.memory: 4Gi` instead of editing the
shared example, which has different resource requirements.

### Caveat 2: Stellar needs a PVC created out of band

As of this repo's current chart, enabling `taugrid-core.stellar.enabled=true`
creates the Stellar Deployment/Service, but does not create the PVC it
mounts (`tau-stellar-expstore`); the pod stays `Pending` until one exists.
Create the missing PVC with `kubectl`:

```bash
kubectl apply -n tau-system -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: tau-stellar-expstore
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: managed-csi
  resources:
    requests:
      storage: 5Gi
EOF
```

### Caveat 3: `stellar-demo/` verifies live metrics; the shared demo does not

Stellar is confirmed live and reachable in this quickstart. Whether it shows
data depends entirely on whether the submitted training script calls the
`tau.stellar` SDK:

- The shared `cpu-multi-interest-ray/train.py` (used in step 5 above)
  only prints to stdout. Stellar correctly shows "No experiments found yet"
  for that run. Treat the Ray dashboard/logs as the source of truth for that
  run's PyTorch evidence.
- The `stellar-demo/train.py` script in this directory (see
  [above](#verifying-live-stellar-metrics-the-stellar-demo-script)) does call
  `tau.stellar`, and its metrics were confirmed rendering live in the actual
  dashboard UI and API. See
  [Recorded evidence](#recorded-evidence).

### Caveat 4: resubmitting a completed RayJob does not restart it

Re-running `tau run --config ... ` against a workload that has already
reached a terminal state (`SUCCEEDED`/`Complete`) makes Tau report `configured`
(an update), but KubeRay does not start a new run: the RayJob stays in
its old terminal state referencing its old (already-torn-down) RayCluster.
To get a fresh run, delete it first and wait for full teardown before
resubmitting:

```bash
tau run cancel tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m
tau run --config examples/aks-cpu-quickstart/tau.yaml --context tau-cpu-quickstart
```

### Caveat 5: log-follow / port-forward timing

`tau run logs -f` started while the job is still `PENDING` (before the
entrypoint starts) exits immediately with no output instead of waiting.
Similarly, `kubectl port-forward` to the head service's dashboard port fails
with "connection refused" if attempted before the head pod's dashboard
container is ready. In both cases, poll `tau run status` /
`kubectl get pod -n taugrid-default` until the job is `Running` (or the head pod is
at least `1/2 Running`) before starting a follower or port-forward.

### Caveat 6: `kubectl cp` fails on the Ray image: use `kubectl exec cat` instead

The `mcr.microsoft.com/aks/ai-runtime/ray` image doesn't ship `tar` on
`PATH`, and `kubectl cp` shells out to `tar` inside the target container to
build the copy stream. Confirmed live against this quickstart's
`stellar-demo` pods:

```
$ kubectl cp taugrid-default/<pod>:/tmp/tau_stellar_demo /tmp/out
error: ... OCI runtime exec failed: exec failed: unable to start container
process: exec: "tar": executable file not found in $PATH
```

Retrieve individual files with `kubectl exec ... cat` instead (needs an
explicit `sh -c` wrapper so the glob expands inside the container, not on
the local shell):

```bash
POD=$(kubectl get pods -n taugrid-default \
  -l ray.io/cluster=<raycluster-name>,ray.io/node-type=worker \
  -o jsonpath='{.items[0].metadata.name}')

for f in run.json config.json history.jsonl artifacts/top_recommendations.json; do
  mkdir -p "/tmp/tau_stellar_demo_local/$(dirname "$f")"
  kubectl exec -n taugrid-default "$POD" -c ray-worker -- sh -c \
    "cat /tmp/tau_stellar_demo/*/$f" > "/tmp/tau_stellar_demo_local/$f"
done
```

## Recorded evidence

Four recordings exist in this session's `files/evidence/` artifact log. Two
are canonical and cover different parts of the demonstration:
**`full_researcher_journey_demo_1080p.webm`** (one run, end to end) and
**`taugrid_multi_experiment_demo_1080p.webm`** (a sweep plus the platform
dashboards). The other two are earlier versions kept only for history.

0. **`taugrid_multi_experiment_demo_1080p.webm`** /
   `..._report.html` (1920×1080, ~1m50s): canonical for the multi-run and
   dashboard results. This is a live-browser capture with no reconstruction: the
   Stellar experiment chooser listing **two** experiments (`lr-sweep`,
   `schedule-study`), the `lr-sweep` dashboard where the deliberately
   unstable `lr-5e-2` arm starts at loss 8.06 and stays visibly worse than
   the other three, a switch into `schedule-study` (96 steps vs 48), then the
   in-cluster Observability Portal: **Fleet** (3/3 nodes, 0 GPUs),
   **Kueue → Scheduler** (2 admitted on `jobqueue`), **Kueue → Live** (the
   upstream KueueViz dashboard reverse-proxied over a live WebSocket:
   1 LocalQueue / 3 Workloads / 1 Completed, with a workload in
   `QuotaReserved`), and **Ray** (3 RayClusters auto-discovered from their
   `-head-svc` Services). Playback runs at a uniform 2× speed, and segments
   spent on empty or not-yet-implemented boards were cut; nothing was
   synthesized or reordered. The Ray Dashboard opens in a new tab, so
   it is not in this recording. See the report for what was verified
   separately. Reproduce it with the [sweep](#running-a-sweep-multiple-experiments)
   and [dashboard](#viewing-ray-and-kueue-dashboards) sections above.
1. **`full_researcher_journey_demo_1080p.webm`** (1920×1080, ~2m7s):
   canonical for the single-run journey. It has seven segments: title, a
   recap of the Copilot prompt that started this engineering session, a
   code review of `stellar-demo/train.py` and `tau.yaml` pulled directly
   from this repo, the `--dry-run=client` validation step
   ([Step 0](#0-validate-the-configs-before-spending-money-no-cluster-required)),
   the cost-and-time breakdown ([Cost and time](#cost-and-time)), an
   animated terminal replay built from unedited `tau` CLI output
   (`tau run --config`, `tau run status --watch`, `tau run logs`),
   fast-forwarded through the Kueue admission/scheduling wait, then a live
   navigation into the still-running Stellar dashboard proxying the
   real experiment store, showing the converging Train loss and
   `train/pairwise_accuracy` charts (48/48 points) and the
   config/artifacts drill-down described above. Every segment carries a
   provenance label (`recreated UI · real prompt text`,
   `real file contents`, `real command output`, `live dashboard`) that
   identifies which parts are reconstructed and which are captured.
   Source page: `full_researcher_journey_demo_1080p_source.html`.
2. **`full_researcher_journey_demo.webm`** / `..._report.html` (1280×720,
   ~2m9s): the previous version. Same content, but only five segments (no
   dry-run or cost segment), and the frame has a grey pillar strip down the
   right edge. Superseded by (1).
3. **`stakeholder_demo_video.webm`** / `stakeholder_demo_report.html`: an
   earlier, simpler walkthrough from before the Stellar SDK integration was
   verified. Superseded by (1) for the Stellar-metrics claim; kept for the
   original Ray-dashboard/`tau run` walkthrough it also covers.

**What is not a live capture:** Segment 1 (the Copilot prompt) is a recreated
UI showing the real prompt text. No computer-use tool was available to
record the actual Copilot conversation. The terminal in segments 3–5 is a
typewriter animation replaying real captured stdout, not a live `tty`
recording. Everything from the Stellar dashboard onward is an unscripted
live browser session against a real, running server.

**Tooling used to produce these recordings:** the demo page itself, the
local HTTP server used to view it, `ffmpeg`/`ffprobe` (for video
inspection), and the browser-automation tool (`shiplight`) used to drive
and record the session are demo/visualization production tooling, run
outside and after the actual infra/platform path. They did not touch
cluster creation, TauGrid installation, or workload submission. Those
steps used only `az`/`tau`/`kubectl` as required. Viewing the retrieved
Stellar experiment store locally used `taugrid-portal experiment serve`,
one of this repo's own two Go binaries (built the same way as `tau`), not
a new external tool.

## Cleanup

Delete resources in this order after capturing the needed evidence: the
workloads and workspace first, then TauGrid, then the whole
resource group (fastest full teardown since everything created by this
example lives in that one dedicated RG):

```bash
tau run cancel tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m
tau run cancel aks-cpu-quickstart-stellar-demo -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m

# The TauWorkspace object lives in the tau-system namespace, not the workload
# namespace it manages. `tau` has no `workspace delete` subcommand yet.
kubectl delete workspace.tau.azure.com taugrid-default -n tau-system

# --yes is required: uninstall refuses to run without it once TauWorkspace
# objects have existed on the cluster. The first phase re-renders the deployed
# chart version to drain the queue policy while Kueue is still running, so its
# finalizers are released rather than stranded.
tau cluster uninstall --context tau-cpu-quickstart --yes

az group delete --name taugrid-cpu-quickstart-rg --yes --no-wait
```

Two things to expect during teardown, both harmless here:

- `tau run cancel` may print `signal: killed` from an internal Kueue-workload
  wait step after it has already deleted the RayJob. Verify the real outcome
  with `kubectl get rayjob -n taugrid-default` (should be empty) rather than trusting
  the exit code alone.
- `tau cluster uninstall` can leave three cluster-scoped Kueue objects
  (`ClusterQueue/jobqueue`, `ResourceFlavor/taugrid-default`,
  `Topology/default-node-topology`) stuck `Terminating` with an orphaned
  `kueue.x-k8s.io/resource-in-use` finalizer, if its first phase could not
  remove them while Kueue was still running, for example when the chart
  reference cannot be resolved. Uninstall reports that when it happens and
  prints the recovery commands. Since the next command destroys the entire
  cluster, it does not matter here, but it would matter when
  uninstalling TauGrid from a cluster intended to remain.

Confirm that the resource group is deleted. Resource group deletion stops
billing:

```bash
az group show --name taugrid-cpu-quickstart-rg   # expect: ResourceGroupNotFound
```

### Scripted run and cleanup

[`run.sh`](./run.sh) and [`cleanup.sh`](./cleanup.sh) execute exactly the
commands documented above, in order, with `set -euo pipefail` and an echo of
each command before it runs. They contain **no logic beyond sequencing** and
invoke only `az`, `tau`, and `kubectl`. They exist to remove copy-paste error,
not to hide any tool. Read them before running them.

```bash
./examples/aks-cpu-quickstart/run.sh          # step 0 through step 6
./examples/aks-cpu-quickstart/cleanup.sh      # full teardown, prompts first
```

Both are idempotent enough to re-run: `run.sh` skips cluster creation if the
cluster already exists, and `cleanup.sh` tolerates already-deleted resources.

### Caveat 7: `runtime.pip` installs under a non-root runtime

The pinned Ray images run as uid 65532 and their system `site-packages` is
root-owned. Tau first attempts the normal install, then retries with
`pip install --user` when the system path is not writable and prepends the
matching user-site `bin` directory to `PATH`. This supports packages such as
`torch` that install console scripts as well as Python modules.

The fallback cannot compensate for a node with no package-index egress. In
that environment, use a platform-approved image with its dependencies already
installed.

## Non-goals

This quickstart intentionally does not cover: GPU/CUDA workloads,
Terraform/Helm-driven (vs. `tau`-driven) installation, production HA/security
hardening, or Kusto-backed Stellar. See the top-level brief this example was
built against for the full non-goals list.
