# TauGrid CPU quickstart — brand-new AKS cluster to a proven PyTorch run

> **Status:** `operator runbook`
> **Intended use:** stand up a disposable CPU-only AKS cluster, install TauGrid
> with `tau cluster install`, and prove a real PyTorch run end to end — including
> live Stellar metrics — before trying GPU/storage-specific flows.
> **Not for:** production clusters, GPU/CUDA workloads, or any cluster you are
> not willing to delete afterwards. This creates billable Azure resources.
>
> **Blocked on [#1294] (not merged).** `tau.yaml` puts `torch` in
> `runtime.pip`, and on `main` that install fails with `EACCES` before
> `train.py` runs. See [Caveat 7](#caveat-7-runtimepip-cannot-install-torch-yet).
> Every other step below is unaffected and was proven live.

This example is the minimum path from **zero** (no cluster) to **evidence** (a
real, CPU-only PyTorch training run submitted through `tau` and observed in
the Ray dashboard) using only three tools on the operator-facing path:

- `az` — create the AKS cluster and fetch its credentials (infra only).
- `tau` — install TauGrid onto the cluster and submit/observe the workload.
- `kubectl` — read-only verification and the couple of platform objects Tau's
  install docs don't yet automate (see caveats below).

No Terraform, Helm, `make`, or custom installer scripts are invoked directly.
`tau cluster install` itself shells out to a local `helm` binary internally —
that is a documented prerequisite of the Tau CLI, not something this example
invokes directly. See [Prerequisites](#prerequisites).

This quickstart does **not** duplicate the training script. It reuses
[`cli/examples/cpu-multi-interest-ray/train.py`](../cpu-multi-interest-ray/train.py)
unmodified — a CPU-only, 5-pod Ray Train PyTorch DDP demo (multi-interest
retrieval model, `arxiv:2506.23060v1`) that prints per-step loss and pairwise
accuracy. Only [`tau.yaml`](./tau.yaml) in this directory differs from the
shared demo's own config, and only in resource sizing (see
[Caveat 1](#caveat-1-head-pod-oomkill-on-a-real-cluster) below for why).

## What this proves

1. A brand-new CPU-only AKS cluster can be created from scratch with `az`.
2. TauGrid (Kueue + KubeRay + Tau CRDs/controller + bootstrap queue) installs
   cleanly onto it with the single supported `tau cluster install` command.
3. A researcher can submit a real PyTorch workload through `tau run` alone —
   no raw `kubectl apply` of Jobs/RayJobs — and it actually trains: loss drops
   from ~3.7 to ~0.3–0.7 across 4 distributed CPU workers, not just "Pod
   Running" or "Job Complete".
4. The live run and its logs are inspectable through both the Tau CLI
   (`tau run status` / `tau run logs`) and the Ray dashboard.
5. Tau's Stellar experiment-tracking UI can be deployed via the same
   `tau cluster install` command in local (non-Kusto) mode, and — using the
   companion [`stellar-demo/`](./stellar-demo) script that calls the
   `tau.stellar` SDK directly — **genuinely logs and renders live metrics**:
   a real "Train loss" chart converging 3.76 → 0.72 and a real
   `train/pairwise_accuracy` chart oscillating ~0.94–0.98 across 48/48 real
   steps, plus the run's config, final metrics, and content-addressed
   artifacts (`history.jsonl`, `top_recommendations.json`), all visually
   confirmed rendering in the actual dashboard UI (not just via `curl`/API).
   See [Recorded evidence](#recorded-evidence) below.

## Prerequisites

- `az` CLI, logged in, with a subscription that can create AKS clusters.
- `tau` — build locally from this repo (`make install-tau-cli` from the repo
  root) or use a published binary. This example was built/run against the
  Go CLI in `cli/`.
- `kubectl` — for read-only verification and the two objects noted in
  [Caveats](#caveats-known-gaps-in-the-supported-flow).
- **`helm` on `PATH`.** `tau cluster install` shells out to `helm`
  internally to render/apply the TauGrid chart — this is an implementation
  detail of the command, not something you invoke yourself. Nothing in this
  example calls `helm` directly; if `tau cluster install` ever stops needing
  it, this note becomes obsolete.

  > **Helm 3 and Helm 4 both work.** Earlier versions of `tau cluster install`
  > called `helm list --all`; Helm v4 removed that flag, so the install failed
  > with a bare `Error: unknown flag: --all`. That is fixed
  > ([#1274](https://github.com/Azure/taugrid/issues/1274)) —
  > the release probe now passes the individual state flags, which both major
  > versions accept, so no Helm version pin is required.
  >
  > If you are running a `tau` build from **before** that fix and
  > `helm version --short` reports `v4.x`, put a Helm 3 binary ahead of it on
  > `PATH` for the duration of the install:
  >
  > ```bash
  > export PATH="$(brew --prefix helm@3)/bin:$PATH"
  > helm version --short   # expect v3.x
  > ```
- A local checkout of this repo, so `tau cluster install --chart ./charts/taugrid`
  can install from source without needing OCI registry / ACR auth.

## Cost and time

This example creates **billable Azure resources**. Budget accordingly and run
[Cleanup](#cleanup) when you are done.

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
dollar. **The risk is forgetting to delete it** — an idle cluster left running
is ~$15/day. Confirm with `az group show --name taugrid-cpu-quickstart-rg`
returning `ResourceGroupNotFound` after cleanup.

## Design decisions (safe naming / no collisions)

- Region: `eastus2` (matches the existing `taugrid-*` test clusters
  and the `aksairuntime` ACR, all in the same region/subscription).
- Dedicated resource group: `taugrid-cpu-quickstart-rg` — does not
  collide with any existing RG, and makes final cleanup a single
  `az group delete`.
- Cluster name: `tau-cpu-quickstart`, 3x `Standard_D4_v5` CPU-only system
  nodes, default/current AKS Kubernetes version.
- Workspace/queue: the single canonical TauWorkspace `taugrid-default` in
  namespace `taugrid-default`, created via `tau workspace create taugrid-default`
  (not hand-written YAML), so this cluster matches a stock install.

## Step-by-step

All commands below assume you're in the repo root and `KUBECONFIG` points at
a dedicated file for this cluster (never merge into your default kubeconfig
for a throwaway quickstart cluster):

```bash
export KUBECONFIG=/tmp/tau-cpu-quickstart.kubeconfig
```

### 0. Validate the configs before spending money (no cluster required)

`--dry-run=client` renders the full RayJob manifest locally without contacting
a cluster, so you can catch config errors before provisioning anything. Run
this first — it takes seconds and needs no Azure resources:

```bash
tau run --config cli/examples/aks-cpu-quickstart/tau.yaml \
  --namespace taugrid-default --dry-run=client

tau run --config cli/examples/aks-cpu-quickstart/stellar-demo/tau.yaml \
  --namespace taugrid-default --dry-run=client
```

Both should print a complete RayJob spec (head + worker pod templates, the
base64 `TAU_PAYLOAD_B64` script payload, Kueue labels, resource requests) and
exit without an error. `--namespace taugrid-default` is only needed for this offline
render, because `--dry-run=client` never contacts a cluster and so cannot look
the namespace up. Every later command omits it: TauGrid v0 activates exactly
one workspace per cluster, and `tau` resolves that workspace (and its
namespace, queue, and output root) automatically. Researchers never need to
know the workspace name — `--workspace` exists only as an operator override.

### 1. Create the resource group + CPU-only AKS cluster (`az` only)

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

### 2. Install TauGrid (`tau` only — Helm is internal to this command)

```bash
tau cluster install \
  --chart ./charts/taugrid \
  --context tau-cpu-quickstart
```

`--chart` here is a local chart directory, so Helm reads the version from its
`Chart.yaml` and `--version` is silently ignored. Pin `--version` only when you
point `--chart` at the OCI registry default.

This installs Kueue, KubeRay, the Tau CRDs/controller, and the bootstrap
GPU/CPU queue objects. Confirm with the Tau-native check (not `kubectl wait`):

```bash
tau cluster validate installation --context tau-cpu-quickstart
```

Optionally, also enable Stellar (Tau's local experiment dashboard) in the same
install call — see [Enabling Stellar](#enabling-stellar-local-mode) below.

### 3. Create the researcher workspace (`tau` only)

```bash
# NAME defaults to taugrid-default. Pass it explicitly so an override remains
# aligned with the status command that follows.
tau workspace create taugrid-default \
  --principal-name quickstart-researcher --context tau-cpu-quickstart --apply

# `tau workspace status` reports; it does not wait. Poll until phase=Ready.
tau workspace status taugrid-default --context tau-cpu-quickstart
```

### 4. Connectivity smoke test (`tau` only)

```bash
tau run smoke --context tau-cpu-quickstart
```

### 5. Submit the real PyTorch workload (`tau` only — no raw `kubectl apply`)

```bash
tau run --config cli/examples/aks-cpu-quickstart/tau.yaml --context tau-cpu-quickstart
```

### 6. Observe it running

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

**What you should see in the logs**: pip install of `torch`, then Ray Train
starting a 4-worker DDP process group, then repeated lines like:

```
(RayTrainWorker pid=849) rank=2/4 step=0  loss=3.7385 pairwise_acc=0.922
(RayTrainWorker pid=849) rank=2/4 step=12 loss=0.4032 pairwise_acc=0.969
...
SUCC -- Job 'tau-aks-cpu-quickstart-...' succeeded
```

Loss dropping from ~3.7 to ~0.3–0.7 across all 4 workers, plus a final
`succeeded` status, is the real-computation proof this example is meant to
demonstrate — not just `RUNNING`/`Complete` in isolation.

A full captured transcript from two independent runs is saved as durable
evidence in this session's artifact log (see the creator session /
`files/evidence/` for `tau_run_logs_full_transcript.txt` and
`tau_run_logs_second_run_transcript.txt`). See
[Recorded evidence](#recorded-evidence) below for the full researcher-journey
video, which additionally proves the Stellar metrics case.

### 7. Retrieve artifacts (optional)

```bash
tau run get tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart
```

## Enabling Stellar (local mode)

Stellar is Tau's built-in experiment-tracking UI. Enabling it in local
(non-Kusto) mode is a single additional `--set` flag on the same
`tau cluster install` call — it does not require the separate
`taugrid-portal` binary/image beyond what the chart already references, and
does not require ADX/Kusto:

```bash
# The portal image is NOT built for every commit — it is published only when a
# PR touches its trusted inputs. So `git rev-parse --short HEAD` is the wrong
# source for this tag: on a docs-only checkout it names a commit that was never
# built, and the install fails with ImagePullBackOff long after `helm` reports
# success. `latest` is a mutable tag and must not be used on a cluster
# (AGENTS.md > "Container image sourcing"). The chart only enforces that
# stellar.image.tag is non-empty, so it will not catch either mistake.
#
# Discover a tag that actually exists:
#   az acr repository show-tags --name aksairuntime \
#     --repository unlisted/aks/ai-runtime/taugrid-portal \
#     --orderby time_desc --top 20 -o tsv
#
# NOTE: the chart takes a tag only. templates/stellar.yaml renders
# "{{ .repository }}:{{ .tag }}" and has no digest support, so a digest cannot
# be pinned here even though AGENTS.md prefers one. Use a short-SHA tag, which
# is immutable in practice, and verify it resolves before installing.
STELLAR_TAG="098cdf4a"

# Preflight: fail here, not 5 minutes into a rollout.
az acr manifest show-metadata \
  "<staging-image-repository>:${STELLAR_TAG}" \
  --query digest -o tsv \
  || { echo "portal tag '$STELLAR_TAG' does not resolve; pick a current one"; exit 1; }

tau cluster install \
  --chart ./charts/taugrid \
  --context tau-cpu-quickstart \
  --set taugrid-core.namespaces.create=true \
  --set taugrid-core.stellar.enabled=true \
  --set taugrid-core.stellar.source=local \
  --set taugrid-core.stellar.workspace=taugrid-default \
  --set taugrid-core.stellar.expstore.pvcName=tau-stellar-expstore \
  --set taugrid-core.stellar.image.tag="$STELLAR_TAG" \
  --wait --timeout 8m
```

> **Note:** this registry is private and holds mostly PR-scoped, prunable tags.
> `098cdf4a` resolved to
> `sha256:c538d417629b10bc97d1a3db80925afada3784f7769befa919a78177c299b0cb`
> when this example was written. If the preflight fails, use the discovery
> command above to choose a current tag.

See [Caveat 2](#caveat-2-stellar-needs-a-pvc-created-out-of-band) for the one
manual `kubectl` step this currently needs.

The following port-forward is an operator-only local diagnostic for this
admin-installed quickstart. It is not a supported researcher endpoint or
browser-signoff path.

```bash
kubectl port-forward -n tau svc/tau-stellar 18080:80
# then open http://localhost:18080/stellar
```

### Proving real Stellar metrics: the `stellar-demo/` script

The shared `cpu-multi-interest-ray/train.py` reused above only prints to
stdout — it does not call the `tau.stellar` SDK, so Stellar correctly shows
"No experiments found yet" for that run. To prove Stellar's actual job (real
metrics logged by a researcher's training loop, rendered live in the
dashboard), this quickstart ships a second, self-contained script,
[`stellar-demo/train.py`](./stellar-demo/train.py), that calls
`stellar.init(...)` / `run.log(...)` / `run.log_artifact(...)` /
`run.finish(...)` every step — the same SDK calls a real researcher project
would import. Submit it with its own `tau.yaml`
([`stellar-demo/tau.yaml`](./stellar-demo/tau.yaml), which uses the direct
`tau run --config` schema so `run.working_dir` can ship the `tau/` package
containing `stellar.py` alongside the script — see the comments in that file
for why):

```bash
tau run --config cli/examples/aks-cpu-quickstart/stellar-demo/tau.yaml --context tau-cpu-quickstart
```

This produces a real, converging training curve (Train loss 3.76 → 0.72,
`train/pairwise_accuracy` oscillating ~0.94–0.98 over 48 steps) plus a final
`config.json`, `run.json`, `history.jsonl`, and an `artifacts/` folder
(`top_recommendations.json`) written under `/tmp/tau_stellar_demo/<run-id>/`
inside the pod. **Retrieving those files needs one gotcha-avoidance step —
see [Caveat 6](#caveat-6-kubectl-cp-fails-on-the-ray-image-use-kubectl-exec-cat-instead).**

Once retrieved locally, viewing them live in Stellar's UI does **not** require
port-forwarding into the cluster's in-cluster Stellar Deployment — the same
experiment store directory can be served locally with the Tau Python SDK's
companion Go binary, `taugrid-portal` (built locally via
`make install-taugrid-portal`, same as `tau`, from this repo — **not** a new
infra tool, just the second of the two Go binaries this repo ships):

```bash
taugrid-portal experiment serve --store /path/to/retrieved/tau_stellar_demo --addr 127.0.0.1:8099
# then open http://127.0.0.1:8099/stellar?target=<run-group-name>
```

This was visually confirmed end-to-end in a real browser: the "Train loss"
and "train/pairwise_accuracy" charts render genuine per-step data (not the
Stellar empty-state fallback), and the run's evidence-details drill-down
shows the real config, final metrics, and both artifacts. See
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

The sweep configs above are deliberately tiny — 48 steps is roughly **2.4
seconds** of actual compute, against roughly **4 minutes** of startup (image
pull, then `pip install torch` in the Ray runtime env). That is fine for
producing metrics, but it means that if you open the Ray or Kueue dashboards
"while the job runs" you are almost certainly looking at *startup*, not
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

The `5e-2` variant exists specifically so the sweep tells a real story: the
task converges within 48 steps at any sane learning rate, so only a
deliberately destabilizing LR produces visible separation on the charts.

> **Capacity gotcha.** On the default 3x `Standard_D4_v5` pool only about two
> sweep jobs fit concurrently, and **a completed RayJob keeps its head and
> worker pods `Running` — still occupying node CPU — until its TTL expires**.
> A job can therefore report `SUCCEEDED` while continuing to consume the
> cluster, so follow-on runs make no progress.
>
> This is because `shutdownAfterJobFinishes` and `ttlSecondsAfterFinished`
> compose: the TTL is the *delay before* cleanup, not an independent knob.
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
> Extract each run's Stellar data *before* deleting it (see below). Queueing
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

* `taugrid-portal experiment init` takes `--description`, **not** `--question`.
* `taugrid-portal experiment track` takes neither `--question` nor
  `--experiment`; use `experiment experiments tag-run` to attach a run to an
  experiment.
* In Python, pass `stellar.init(..., experiment="lr-sweep")`. The
  `question=`/`question_id=` keywords still work as deprecated aliases.

> If `experiment init` rejects `--description` or demands `--question`, you are
> running a **stale `taugrid-portal` binary** from before the migration. Build
> from source and use that binary.

## Viewing Ray and Kueue dashboards

Both are reverse-proxied by the TauGrid Portal, which must run **in-cluster**
(the proxy resolves targets via in-cluster DNS, so a local portal against
port-forwards will not work).

Enable the portal and KueueViz during install:

```bash
# The portal namespace is NOT created by the chart. Create it first,
# or the install fails with `namespaces "tau" not found` (and, with
# --atomic, silently rolls the whole release back).
kubectl create namespace tau --context "$CLUSTER"

tau cluster install --context "$CLUSTER" \
  --set kueue.enableKueueViz=true \
  --set taugrid-core.portal.enabled=true \
  --set taugrid-core.portal.source=kusto \
  --set-string taugrid-core.portal.image.tag=<immutable-tag> \
  --set taugrid-core.portal.serviceAccount.create=true \
  --set taugrid-core.portal.rbac.create=true \
  --set-string taugrid-core.portal.jobs.namespace=taugrid-default \
  --set-string taugrid-core.portal.kueueviz.namespace=tau-system \
  --set-string taugrid-core.portal.kueueviz.backendService=taugrid-kueue-kueueviz-backend \
  --set-string taugrid-core.portal.kueueviz.frontendService=taugrid-kueue-kueueviz-frontend \
  --set-string kueue.kueueViz.backend.env[0].name=KUEUEVIZ_ALLOWED_ORIGINS \
  --set-string kueue.kueueViz.backend.env[0].value=http://portal.kueueviz.local
```

Notes — most of these defaults are wrong for this setup, and each one fails
in a way that looks like a bug but is not:

* **Namespace `tau` must pre-exist.** Nothing in the chart creates it.
* **`portal.rbac.create` and `portal.serviceAccount.create` both default to
  `false`.** Without them the portal runs as `system:serviceaccount:tau:default`
  and the Jobs / Ray / Nodes boards return `503` with an RBAC error
  (`cannot list resource "services" at the cluster scope`). The chart's
  `values.yaml` documents this; it is expected behaviour, not a failure.
* **`portal.jobs.namespace` defaults to `ray`.** Point it at the workspace
  namespace you actually submit into (`taugrid-default` here) or the Jobs board is
  empty.
* **KueueViz Deployments land in the release namespace (`tau-system`)**, named
  `taugrid-kueue-kueueviz-{backend,frontend}`. The portal's kueueviz defaults
  point at `kueue-kueueviz-*` in `kueue-system`, so they must be overridden.
* `portal.source=local|auto` requires a mounted store and the chart `fail`s
  without one. `source=kusto` runs store-less; Kusto-backed boards return 503
  while the Jobs / Ray / Kueue / Fleet boards serve normally.
* The portal image tag must be pinned to an immutable tag or digest.

For an operator-only local diagnostic:

```bash
kubectl port-forward svc/tau-portal -n tau --context "$CLUSTER" 8088:80
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

To see anything meaningful on Ray or Kueue mid-run you need (a) a run that
trains for long enough (see `DEMO_MAX_SECONDS` above) and (b) more submitted
work than the cluster can admit at once, so Kueue actually has something to
schedule. On a 3x4-vCPU cluster, submitting `demo-live-run.yaml` plus three
sweeps produces genuine contention:

```bash
tau run --config stellar-demo/demo-live-run.yaml --context "$CLUSTER"
for c in sweep-lr-low sweep-lr-high sweep-lr-extreme; do
  tau run --config "stellar-demo/$c.yaml" --context "$CLUSTER"
done
```

The Kueue Live board then shows a real mix of `Admitted` and `Not admitted`
workloads, and the Ray job detail page streams live driver logs.

Note that Stellar is **not** live in this example: `train.py` runs with
`sync=False`, so metrics are written as append-only JSONL *inside the pod*. To
inspect an in-flight run you must pull and re-import them (`kubectl cp` does not
work here — use `exec ... -- cat`):

```bash
kubectl exec -n taugrid-default "$POD" --context "$CLUSTER" -- \
  cat /tmp/tau_stellar_demo/<run>/history.jsonl > /tmp/history.jsonl
taugrid-portal experiment import jsonl --store /tmp/store --history /tmp/history.jsonl ...
```

That is real data, but it is a *pull*, not a stream. Ray and Kueue are
genuinely live.

## Caveats (known gaps in the supported flow)

### Caveat 1: head pod OOMKill on a real cluster

The shared `cpu-multi-interest-ray` demo's default `compute.memory: 2Gi` head
limit OOMKills on a real AKS cluster once Ray GCS + dashboard + 4 connecting
workers are running (`kubectl describe rayjob ... ->` `HeadPodReady: False,
Reason: OOMKilled`). There is no `tau run` CLI flag to override
`compute.*` fields at submit time (see `docs/tau/tau-run-config.md`), so this
quickstart keeps its own copy of the config
([`tau.yaml`](./tau.yaml)) with `compute.memory: 4Gi` instead of editing the
shared example (which is used elsewhere and shouldn't have its defaults
changed for this one cluster's needs).

### Caveat 2: Stellar needs a PVC created out of band

As of this repo's current chart, enabling `taugrid-core.stellar.enabled=true`
creates the Stellar Deployment/Service, but does **not** create the PVC it
mounts (`tau-stellar-expstore`) — the pod stays `Pending` until one exists.
This is the one place this example needs a plain `kubectl` object creation
(not a workaround for a missing `tau`/`helm` feature elsewhere, just a chart
gap):

```bash
kubectl apply -n tau -f - <<'EOF'
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

### Caveat 3: `stellar-demo/` proves live metrics; the shared demo does not

Stellar is confirmed live and reachable in this quickstart. Whether it shows
data depends entirely on whether the submitted training script calls the
`tau.stellar` SDK:

- The **shared** `cpu-multi-interest-ray/train.py` (used in step 5 above)
  only prints to stdout — Stellar correctly shows "No experiments found yet"
  for that run. Treat the Ray dashboard/logs as the source of truth for that
  run's PyTorch evidence.
- The **`stellar-demo/train.py`** script in this directory (see
  [above](#proving-real-stellar-metrics-the-stellar-demo-script)) does call
  `tau.stellar`, and its metrics were confirmed rendering live in the actual
  dashboard UI, not just via API — see
  [Recorded evidence](#recorded-evidence).

### Caveat 4: resubmitting a completed RayJob does not restart it

If you re-run `tau run --config ... ` against a workload that has already
reached a terminal state (`SUCCEEDED`/`Complete`), Tau reports `configured`
(an update), but KubeRay does **not** start a new run — the RayJob stays in
its old terminal state referencing its old (already-torn-down) RayCluster.
To get a fresh run, delete it first and wait for full teardown before
resubmitting:

```bash
tau run cancel tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m
tau run --config cli/examples/aks-cpu-quickstart/tau.yaml --context tau-cpu-quickstart
```

### Caveat 5: log-follow / port-forward timing

`tau run logs -f` started while the job is still `PENDING` (before the
entrypoint starts) exits immediately with no output instead of waiting.
Similarly, `kubectl port-forward` to the head service's dashboard port fails
with "connection refused" if attempted before the head pod's dashboard
container is ready. In both cases, poll `tau run status` /
`kubectl get pod -n taugrid-default` until the job is `Running` (or the head pod is
at least `1/2 Running`) before starting a follower or port-forward.

### Caveat 6: `kubectl cp` fails on the Ray image — use `kubectl exec cat` instead

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
your local shell):

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
are canonical and cover different halves of the story:
**`full_researcher_journey_demo_1080p.webm`** (one run, end to end) and
**`taugrid_multi_experiment_demo_1080p.webm`** (a sweep plus the platform
dashboards). The other two are earlier cuts kept only for history.

0. **`taugrid_multi_experiment_demo_1080p.webm`** /
   `..._report.html` (1920×1080, ~1m50s) — *canonical for the multi-run and
   dashboard story*. A pure live-browser capture, no reconstruction: the
   Stellar experiment chooser listing **two** experiments (`lr-sweep`,
   `schedule-study`), the `lr-sweep` dashboard where the deliberately
   unstable `lr-5e-2` arm starts at loss 8.06 and stays visibly worse than
   the other three, a switch into `schedule-study` (96 steps vs 48), then the
   in-cluster Observability Portal: **Fleet** (3/3 nodes, 0 GPUs),
   **Kueue → Scheduler** (2 admitted on `jobqueue`), **Kueue → Live** (the
   upstream KueueViz dashboard reverse-proxied over a live WebSocket —
   1 LocalQueue / 3 Workloads / 1 Completed, with a workload in
   `QuotaReserved`), and **Ray** (3 RayClusters auto-discovered from their
   `-head-svc` Services). Playback is a uniform 2× speed-up and segments
   spent on empty or not-yet-implemented boards were cut; nothing was
   synthesised or re-ordered. The Ray Dashboard itself opens in a new tab so
   it is *not* in this recording — see the report for what was verified
   separately. Reproduce it with the [sweep](#running-a-sweep-multiple-experiments)
   and [dashboard](#viewing-ray-and-kueue-dashboards) sections above.
1. **`full_researcher_journey_demo_1080p.webm`** (1920×1080, ~2m7s) —
   *canonical for the single-run journey*. Seven acts: title, a recap of the Copilot prompt that
   started this engineering session, a real code review of
   `stellar-demo/train.py` and `tau.yaml` pulled directly from this repo,
   the **free `--dry-run=client` validation step** ([Step 0](#0-validate-the-configs-before-spending-money-no-cluster-required)),
   the **cost-and-time breakdown** ([Cost and time](#cost-and-time)), an
   animated terminal replay built from 100% real, unedited `tau` CLI output
   (`tau run --config`, `tau run status --watch`, `tau run logs`),
   fast-forwarded through the Kueue admission/scheduling wait, then a live
   auto-navigation into the still-running Stellar dashboard proxying the
   real experiment store — showing the genuine converging Train loss and
   `train/pairwise_accuracy` charts (48/48 real points) and the real
   config/artifacts drill-down described above. Every act carries an
   on-screen provenance badge (`recreated UI · real prompt text`,
   `real file contents`, `real command output`, `live dashboard`) so a
   viewer can tell reconstruction from capture without reading these notes.
   Source page: `full_researcher_journey_demo_1080p_source.html`.
2. **`full_researcher_journey_demo.webm`** / `..._report.html` (1280×720,
   ~2m9s) — the previous cut. Same story, but only five acts (no dry-run or
   cost act), and the frame has a grey pillar strip down the right edge.
   Superseded by (1).
3. **`stakeholder_demo_video.webm`** / `stakeholder_demo_report.html` — an
   earlier, simpler walkthrough from before the Stellar SDK integration was
   proven; superseded by (1) for the Stellar-metrics claim, kept for the
   original Ray-dashboard/`tau run` walkthrough it also covers.

**What is *not* live capture, stated plainly**: Act 1 (the Copilot prompt)
is a recreated UI showing the real prompt text — no computer-use tool was
available to record the actual Copilot conversation. The terminal in acts
3–5 is a typewriter animation replaying real captured stdout, not a live
`tty` recording. Everything from the Stellar dashboard onward is an
unscripted live browser session against a real, running server.

**Transparency note on tooling used to produce these recordings**: the demo
page itself, the local HTTP server used to view it, `ffmpeg`/`ffprobe` (for
video inspection), and the browser-automation tool (`shiplight`) used to
drive and record the session are all **demo/visualization production
tooling**, run entirely outside and after the actual infra/platform path.
They never touched cluster creation, TauGrid installation, or workload
submission — those steps used only `az`/`tau`/`kubectl` as required. Viewing
the retrieved Stellar experiment store locally used `taugrid-portal
experiment serve`, one of this repo's own two Go binaries (built the same
way as `tau`), not a new external tool.

## Cleanup

Delete resources in this order once you've captured whatever evidence you
need — the workloads and workspace first, then TauGrid, then the whole
resource group (fastest full teardown since everything created by this
example lives in that one dedicated RG):

```bash
tau run cancel tau-aks-cpu-quickstart -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m
tau run cancel aks-cpu-quickstart-stellar-demo -n taugrid-default --context tau-cpu-quickstart --teardown-timeout 3m

# The TauWorkspace object lives in the tau-platform namespace, not the workload
# namespace it manages. `tau` has no `workspace delete` subcommand yet.
kubectl delete workspace.tau.azure.com taugrid-default -n tau-platform

# --yes is required: uninstall refuses to run without it once TauWorkspace
# objects have existed on the cluster. --chart must match the install: the
# first phase re-renders the release to drain the queue policy while Kueue is
# still running, so its finalizers are released rather than stranded.
tau cluster uninstall --context tau-cpu-quickstart --chart ./charts/taugrid --yes

az group delete --name taugrid-cpu-quickstart-rg --yes --no-wait
```

Two things to expect during teardown, both harmless here:

- `tau run cancel` may print `signal: killed` from an internal Kueue-workload
  wait step *after* it has already deleted the RayJob. Verify the real outcome
  with `kubectl get rayjob -n taugrid-default` (should be empty) rather than trusting
  the exit code alone.
- `tau cluster uninstall` can leave three cluster-scoped Kueue objects
  (`ClusterQueue/jobqueue`, `ResourceFlavor/taugrid-default`,
  `Topology/default-node-topology`) stuck `Terminating` with an orphaned
  `kueue.x-k8s.io/resource-in-use` finalizer, if its first phase could not
  remove them while Kueue was still running — for example when the chart
  reference cannot be resolved. Uninstall reports that when it happens and
  prints the recovery commands. Since the next command destroys the entire
  cluster, it does not matter here — but it *would* matter if you were
  uninstalling TauGrid from a cluster you intended to keep.

Confirm the resource group is really gone (this is the step that actually stops
the billing):

```bash
az group show --name taugrid-cpu-quickstart-rg   # expect: ResourceGroupNotFound
```

### Scripted run and cleanup

[`run.sh`](./run.sh) and [`cleanup.sh`](./cleanup.sh) execute exactly the
commands documented above, in order, with `set -euo pipefail` and an echo of
each command before it runs. They contain **no logic beyond sequencing** and
invoke only `az`, `tau`, and `kubectl` — they exist to remove copy-paste error,
not to hide any tool. Read them before running them.

```bash
./cli/examples/aks-cpu-quickstart/run.sh          # step 0 through step 6
./cli/examples/aks-cpu-quickstart/cleanup.sh      # full teardown, prompts first
```

Both are idempotent enough to re-run: `run.sh` skips cluster creation if the
cluster already exists, and `cleanup.sh` tolerates already-deleted resources.

### Caveat 7: `runtime.pip` cannot install `torch` yet

`tau.yaml` lists `torch>=2.4.0` under `runtime.pip`. On `main` the RayJob
entrypoint runs a bare `pip install --quiet --no-cache-dir ${PIP_PACKAGES}`
with no fallback, and that install fails before `train.py` is reached:

```
$ docker run --rm --platform linux/amd64 \
    mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0 \
    sh -c 'id; pip install --quiet --no-cache-dir "PyYAML>=6.0" "torch>=2.4.0"'
uid=65532(nonroot) gid=65532(nonroot)
ERROR: Could not install packages due to an OSError: [Errno 13]
Permission denied: '/usr/lib/python3.12/site-packages/nvidia'
```

The image does not ship `torch`, and `torch` pulls `nvidia-*` wheels, so pip
must **create** `site-packages/nvidia` inside a `drwxr-xr-x root root`
directory while running as uid 65532. Despite the path name this is not
CUDA-specific — it reproduces on the plain CPU image shown above.

Note that `PyYAML>=6.0` on its own succeeds, because it is already present and
pip writes nothing. A partial reproduction therefore looks green; `torch` is
the package that fails.

[#1294] fixes this with a `pip install --user` fallback plus the matching
`PATH` export. Until it merges, bake `torch` into a custom `runtime.image` and
drop it from `runtime.pip`.

This blocks the `stellar-demo/` sweep configs too — all six list
`torch>=2.4.0`, and `stellar-demo/train.py` imports `torch` directly.

[#1294]: https://github.com/Azure/taugrid/pull/1294

## Non-goals

This quickstart intentionally does not cover: GPU/CUDA workloads,
Terraform/Helm-driven (vs. `tau`-driven) installation, production HA/security
hardening, or Kusto-backed Stellar. See the top-level brief this example was
built against for the full non-goals list.
