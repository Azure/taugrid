---
title: Troubleshooting by lifecycle layer
linkTitle: Troubleshoot by layer
weight: 20
description: Diagnose the first failed transition instead of guessing
url: "/docs/platform-admin-guide/troubleshooting/"
aliases:
  - "/docs/operations/troubleshooting/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

This is the canonical diagnose-first path for a stuck or failed
[run](../../reference/glossary/#run). Work through the layers in order and
stop at the first incomplete layer. Each layer names its own
[TauGrid](../../reference/glossary/#tau) command first; use raw `kubectl` only
for the deep, operator-only inspection each layer calls out, never as a
substitute for the command.

Start every investigation the same way:

```bash
tau run status <run-name>
```

`tau run status` is the single canonical lifecycle view. It walks the same
ordered phase tree for every run -- Submitted, Kueue admission, (for a RayJob)
RayCluster, pod scheduling, DRA allocation, image pull, init containers,
container start, readiness, and (for a RayJob) RayJob status -- and each phase
reports pending, active, done, warning, or skipped. Read it top to bottom and
treat the first phase that is not `done` as the layer to investigate,
addressing it before any later phase. Add `--watch` to follow it live.

Two things are never proof of training progress by themselves:

- Queue admission (layer 4) means quota was reserved, not that pods are
  running.
- A `Running` pod phase (layer 5) means containers started, not that the
  model or data process is making useful progress (layer 7).

For current GPU runtime evidence, add `--run-profile`:

```bash
tau run status <run-name> --run-profile
```

When NVIDIA `dcgm-exporter` is installed with its Kubernetes pod-resource
labels, the run profile reads the exporter through the Kubernetes API and
reports:

- `gpu_allocation`: DRA `ResourceClaim` allocation identity, when the run uses
  DRA.
- `gpu_devices`: the matching workload pod/container, GPU index, and GPU UUID.
- `gpu_utilization`: current average and maximum
  `DCGM_FI_DEV_GPU_UTIL`.
- `gpu_memory`: current average and maximum `DCGM_FI_DEV_FB_USED`, converted
  from MiB to GiB.
- `gpu_activity`: `active` when matching device utilization is above zero,
  `idle now` when utilization is zero but framebuffer use remains observed, or
  `idle` when both are zero. This is an instantaneous device-level signal, distinct from
  proof of useful model progress.
- `cuda_compute_process`: explicitly unavailable because dcgm-exporter's
  supported scalar metrics expose only aggregate utilization, excluding compute
  PIDs and CUDA contexts, so utilization alone cannot identify a
  process or context.

The command performs only read-only Kubernetes API calls against unmodified,
uninstrumented workloads. It performs at most two targeted discovery requests
(the official dcgm-exporter label and TauGrid's GPU-monitoring label), then at most
one current metrics request per distinct workload node for each profile
refresh. TauGrid's GPU-monitoring DaemonSet proxies the existing node-local DCGM
endpoint rather than starting another collector. `--watch --run-profile` repeats
that bounded snapshot at the selected watch interval.

An active run shows observed values only when samples match both its namespace
and pod names. An allocated-but-idle run shows `0%` as a real observation.
A completed run keeps any allocation identity still present in Kubernetes but
reports live GPU telemetry unavailable; missing samples always report as
unavailable, never as a false zero. Missing exporter pods, Kubernetes enrichment, pod-proxy RBAC, or
required metrics are also reported as unavailable. Listing exporter pods
requires cluster-wide pod read access, and reading metrics requires the
`pods/proxy` subresource; base `tau run status` output still works when those
permissions are absent.

## At a glance

| # | Layer | Primary command | Owner if it fails |
|---|---|---|---|
| 1 | Repository/connection resolution and cluster access | `tau workspace connection` (`--offline` for local configuration only) | Researcher (descriptor) or platform (access) |
| 2 | [TauWorkspace](../../reference/glossary/#tauworkspace) readiness and handoff validity | `tau workspace status <name>` | Platform operator |
| 3 | Client-side config validation and rendering | `tau run validate --config tau/train.yaml` | Researcher |
| 4 | [Queue](../../reference/glossary/#queue) admission and quota | `tau run status <run-name>` (Kueue admission phase) | Platform/queue owner |
| 5 | Kubernetes scheduling, DRA, image pull, init, readiness | `tau run status <run-name>` (remaining phases) | Platform (scheduling/DRA/image) or researcher (crash) |
| 6 | GPU/node/topology health | `tau cluster validate nodes` / `tau cluster validate topology` | Platform/node-pool operator |
| 7 | Ray/Job runtime progress and durable evidence | `tau run logs <run-name>` and `taugrid-portal experiment status <name>` | Researcher (app logic) or platform (evidence pipeline) |
| 8 | Recovery handoff | -- | See [Retry and resume](../recovery/) |

## 1. Repository/connection resolution and cluster access

**Suspected layer:** TauGrid client-side resolution of the
[project](../../reference/glossary/#project)'s
[workspace connection descriptor](../../reference/glossary/#workspace-connection),
and whether the resolved [cluster context](../../reference/glossary/#cluster-context)
is actually reachable.

**Connection check:**

```bash
tau workspace connection
```

**What success proves:** TauGrid found the current project's connection,
resolved credentials, reached Kubernetes, and verified the TauWorkspace,
LocalQueue, and authorization contract. Add `--offline` to prove only local
project and descriptor resolution.

**What failure means:** No descriptor found, more than one candidate found, or
the descriptor failed schema validation. This is a repository configuration
problem rather than a workload problem, and every later layer stays
unreachable until it is fixed.

`tau run` discovers the descriptor automatically, so this command is a
preflight rather than an activation prerequisite. If live connection fails but
`tau workspace connection --offline` succeeds, the problem is credential
resolution, VPN/DNS reachability, Kubernetes availability, or RBAC rather than
descriptor parsing.

**Next owner/action:** Missing or invalid descriptor -- researcher action
required; get a valid descriptor from the platform operator who owns the
[TauWorkspace](../../reference/glossary/#tauworkspace). Reachability or
permission failure on the first live call -- platform action required
(cluster access/RBAC), or a transient network condition if it clears on
retry.

## 2. TauWorkspace readiness and handoff validity

**Suspected layer:** The [TauWorkspace](../../reference/glossary/#tauworkspace)
object's reconciled [status conditions and phase](../../reference/glossary/#status-condition)
-- the platform-owned onboarding contract behind the descriptor from layer 1.

**Primary command:**

```bash
tau workspace status <name>
```

Use `tau workspace check <name>` in scripts; it exits non-zero unless the
workspace is `Ready`.

**What success proves:** `status.phase` is `Ready`: `RBACReady` and
`QueueReady` are true and no drift is detected. The diagnostic
`WorkloadIdentityReady` condition remains important for workloads that use
Azure Workload Identity, but today's overall phase gate excludes it. Storage
is separate platform desired state. Ready proves the core workspace handoff is
valid for submitting workloads; confirming Azure infrastructure freshness and
queue admission for a specific run (layer 4) still require their own checks.

**What Ready leaves unconfirmed:** TauGrid 0.1 has no `StorageReady` condition on
`TauWorkspace` or `TauCluster`, so a `Ready` workspace can still have a missing
or unbound platform-managed PVC. If the symptom is storage-shaped, check the
claim directly instead of trusting the phase:

```bash
kubectl get pvc blob-training -n <namespace> --context <context>
```

**What failure means:** `Pending` or `Degraded` phase means one or more
platform-owned conditions are unmet -- namespace/RBAC, queue accessibility, or
workload identity. This is an
onboarding/policy problem for the platform to fix, not something a researcher can resolve by resubmitting
the run.

**Next owner/action:** Platform action required.
[TauWorkspace](../../reference/glossary/#tauworkspace) is reconciled entirely
by the platform's workspace controller; let it reconcile RBAC, queues, and
storage rather than hand-editing them to work around a `Degraded` condition. See the
[TauWorkspace reference](../../reference/workspace/#workspace-readiness-and-recovery)
for the condition-driven recovery path.

## 3. Client-side config validation and rendering

**Suspected layer:** TauGrid's local schema validation and render step, before
anything is submitted to the cluster.

**Primary command:**

```bash
tau run validate --config tau/train.yaml
```

Add `--dry-run=client` to a normal `tau run` invocation for the same check
inline, or `--dry-run=server` to render and dry-run against the API server
while skipping admission.

**What success proves:** The checked-in
[direct run config](../../reference/glossary/#run-config-vs-manifest) parses,
passes schema validation, and resolves to a renderable
[Workload](../../reference/glossary/#workload) (`Job` or `RayJob`). `tau run
validate` runs entirely offline. In a connected repository, `tau run
--dry-run=client` activates the workspace connection and reads the live
workload-profile catalog without submitting the rendered workload. Use
`--dry-run=server` for API-server validation or submit a checked-in target to
exercise workspace readiness and admission.

**What failure means:** A schema or field error, an ambiguous
[target](../../reference/glossary/#target), or a manifest that looks like an
SDK-generated managed workflow being run through the wrong command. These are
authoring problems in the repository, not cluster or scheduling problems.

**Next owner/action:** Researcher action required. Fix the run config or
target in the repository; nothing here touches the cluster, a queue, or a
node.

## 4. Queue admission and quota

**Suspected layer:** [Kueue](../../reference/glossary/#queue) admission of the
rendered [Workload](../../reference/glossary/#workload).

**Primary command:**

```bash
tau run status <run-name>
```

Read the **Kueue admission** phase line: `N/M admitted queue=<names>`, plus a
`reason=` hint while a workload awaits admission.

**What success proves:** All workloads show `Admitted`. This proves quota was
reserved -- it does **not** prove pods were scheduled or the process is
running; continue to layer 5.

**What failure means:** `0/N admitted` with a quota-related reason means the
[LocalQueue/ClusterQueue](../../reference/glossary/#queue) currently lacks
capacity, including borrowing limits. A reason mentioning preemption or
eviction means Kueue reclaimed capacity for higher-priority work. Confirm this phase reports `done` before moving on to GPU or node
debugging (layer 6).

**Next owner/action:** Platform/queue owner for capacity or priority changes.
For read-only, operator-only deep inspection of the queue objects themselves:

```bash
kubectl get workload -n <namespace>
kubectl describe clusterqueue <cluster-queue-name>
kubectl describe localqueue <local-queue-name> -n <namespace>
```

These `kubectl` commands confirm what `tau run status` already
reported; run `tau run status` first. See
[Queue, quota, topology, and GPU placement](../policy-and-placement/).

## 5. Kubernetes scheduling, DRA, image pull, init, and readiness

**Suspected layer:** Kubernetes pod scheduling, DRA device-claim allocation
(where the workload uses it), image pull, init containers, and container
start -- everything between "admitted" and "ready."

**Primary command:**

```bash
tau run status <run-name> --watch
```

Read the remaining phases in order: **Pod scheduling**, **DRA allocation**,
**Image pull**, **Init containers**, **Container start**, **Ready**.

**What success proves:** Pods were assigned to nodes; any referenced
`ResourceClaim`s were allocated (this phase is `skipped` when a workload
requests GPUs through the device plugin instead of DRA -- that is expected
behavior rather than a failure); the image was pulled; init containers exited `0`; the main
container started and passed readiness.

**What failure means:**
- **Pod scheduling** stuck: node selector/taint mismatch, or admission from
  layer 4 may still be in progress -- re-check layer 4 first.
- **DRA allocation** stuck past roughly 30 seconds: usually no matching GPU
  device; check `ResourceSlice` availability for the pool.
- **Image pull** failing (`ErrImagePull`/`ImagePullBackOff`): verify the image
  name, tag, registry credentials, and node egress.
- **Init containers**/**Container start** failing: the application or its
  init step is crashing before it becomes ready -- an application problem,
  rather than an infrastructure one.

**Next owner/action:** Scheduling and DRA capacity -- platform/node-pool
owner. Image pull -- researcher (wrong pinned tag) or the image's owning
team. Init/container crash -- researcher action required.

Operator-only deep inspection, after `tau run status` has identified the
stuck phase:

```bash
kubectl describe pod <pod-name> -n <namespace>
kubectl get resourceclaim -n <namespace>
kubectl get events -n <namespace> --sort-by=.lastTimestamp
```

{{< maturity status="alpha" >}} If `tau run status` shows only a
**MultiKueue placement** phase and no pod phases progress locally, the
workload dispatched to a worker cluster. Inspect it from the worker context,
or see [Multi-cluster execution](../multicluster/) (Alpha) before
assuming the run is stuck.

## 6. GPU/node/topology health

**Suspected layer:** The physical or virtual health of the assigned GPU
nodes, and whether the live cluster topology still matches the
[topology preset](../../reference/glossary/#topology)'s expected
`ResourceFlavor` chain.

**Primary commands:**

```bash
tau cluster validate nodes --gpu-class <class> --min-healthy <N>
tau cluster validate topology --preset <preset-name>
```

`cluster validate nodes` accepts `--context`, `--gpu-class`, `--selector`
(alternative to `--gpu-class`), `--min-healthy` (fail if fewer than N nodes
are healthy), and `--timeout` (default `2m`, per-pod). It runs privileged
validation pods on the selected GPU nodes and checks `nvidia-smi`, NVLink, IB,
and ECC health.

`cluster validate topology` accepts `--context`, `--preset` (validate one
preset's full chain: LocalQueue, ClusterQueue, topology, priority classes, and
`ResourceFlavor` node match), and `--cluster-queue` (default `tau-cq`,
validate all `ResourceFlavor`s referenced by a `ClusterQueue` when `--preset`
is omitted).

**What success proves:** The nodes backing the workload pass hardware health
checks, and/or the preset's full Kueue-facing chain has matching, `Ready`
nodes.

**What failure means:** A node reported `DEGRADED`/`UNHEALTHY` for a specific
reason (NVLink down, uncorrectable ECC, IB down) -- a hardware/node problem,
rather than a TauGrid or application bug. `cluster validate topology` reporting zero
matching nodes for a `ResourceFlavor` means the node pool, instance type, or
GPU device plugin diverges from what the preset expects.

**Next owner/action:** Platform/node-pool operator. `cluster validate nodes`
requires cluster-admin-level RBAC to create privileged validation pods;
`cluster validate topology` is read-only (`kubectl get` on Kueue objects and
Nodes) but still needs cluster-scoped read access. Both are deliberately
operator-only diagnostics.

## 7. Ray/Job runtime progress and durable evidence

**Suspected layer:** Whether the application process itself is making
progress, and whether that progress is durably recorded as
[evidence](../../reference/glossary/#evidence) independent of pod lifecycle.

**Primary commands:**

```bash
tau run logs <run-name>
taugrid-portal experiment status <name>
```

`tau run logs` fetches the actual Ray Job driver execution log for a
RayJob (not head-pod container logs), or the batch `Job`'s pod logs for a
plain job. `taugrid-portal experiment status`/`taugrid-portal experiment list`
show durable
[experiment](../../reference/glossary/#experiment) records -- metric history,
summaries, and artifacts -- that outlive the pod. `tau run get <run-name>`
fetches a specific durable result artifact (`--artifact NAME`) when the run's
config declared a `storage.output` path.

After KubeRay deletes a terminal RayJob's head pod, read the driver output from
the central log offload by supplying the explicit ADX identity:

```bash
tau run logs <run-name> \
  --kusto-cluster <Logs.ContainerLogs Cluster value> \
  --kusto-endpoint <adx-endpoint> \
  --kusto-database <logs-database>
```

TauGrid requires the exact `Cluster` value instead of guessing from the kube
context, which lacks a stable observability identity.

**What success proves:** The driver log shows expected progress (loss
decreasing, steps advancing, checkpoints written), and that progress is
mirrored into the durable experiment record. Neither queue admission (layer
4) nor a `Running` container (layer 5) proves this -- always check logs and
evidence directly.

**What failure means:** Logs show no progress despite a `Ready`/`Running`
phase: an application-level hang (data loader stall, deadlock), rather than an
infrastructure fault. An empty or missing experiment record despite healthy
logs: the metrics-offload path or checkpoint contract is missing for this
run, rather than a training failure.

**Next owner/action:** No progress in logs -- researcher action required
(application logic). Missing durable evidence with otherwise healthy logs --
platform/TauGrid owner for the metrics-offload or expstore configuration. See
[Experiment evidence and artifacts](../../developer-guide/concepts/evidence/).

## 8. Recovery handoff

Once you have identified the first failed layer above and classified whether
it is transient, apply the matching recovery action from
[Retry and resume](../recovery/). Do not resume or retry before you have
located the first failed layer -- retrying past an unresolved layer 1-3
problem, a `Degraded` [TauWorkspace](../../reference/glossary/#tauworkspace),
or an actual quota/node problem only reproduces the same failure.
