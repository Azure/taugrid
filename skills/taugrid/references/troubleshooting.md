# Troubleshooting by lifecycle layer

A Tau run crosses independent control planes. Diagnose the **first** transition
that did not complete; every symptom after it is downstream noise.

Start every investigation the same way:

```bash
tau run status <run-name>
```

Then work the layers below in order and stop at the first one that is not
complete. Each layer names its Tau command first; the `kubectl` commands are
operator-only confirmation of what `tau run status` already reported, not
replacements for it.

## Contents

- [Layer index](#layer-index)
- [1. Repository/connection resolution](#1-repositoryconnection-resolution-and-cluster-access)
- [2. TauWorkspace readiness](#2-tauworkspace-readiness)
- [3. Config validation and rendering](#3-config-validation-and-rendering)
- [4. Kueue admission and quota](#4-kueue-admission-and-quota)
- [5. Scheduling, DRA, image pull, init, readiness](#5-scheduling-dra-image-pull-init-and-readiness)
- [6. GPU/node/topology health](#6-gpunodetopology-health)
- [7. Runtime progress and durable evidence](#7-runtime-progress-and-durable-evidence)
- [8. Recovery handoff](#8-recovery-handoff)
- [Symptom index](#symptom-index)

## Layer index

| # | Layer | Primary command | Owner if it fails |
|---|---|---|---|
| 1 | Repo/connection resolution and cluster access | `tau workspace connection inspect` | Researcher (descriptor) / platform (access) |
| 2 | TauWorkspace readiness and handoff validity | `tau workspace status <name>` | Platform operator |
| 3 | Client-side config validation and rendering | `tau run validate --config <path>` | Researcher |
| 4 | Kueue admission and quota | `tau run status <run>` (admission phase) | Platform/queue owner |
| 5 | Scheduling, DRA, image pull, init, readiness | `tau run status <run> --watch` | Platform (infra) or researcher (crash) |
| 6 | GPU/node/topology health | `tau cluster validate nodes` / `... topology` | Node-pool operator |
| 7 | Runtime progress and durable evidence | `tau run logs <run>` + `taugrid-portal experiment status <name>` | Researcher (app) / platform (pipeline) |
| 8 | Recovery handoff | see [8](#8-recovery-handoff) | — |

Layers 1–3 are offline and cost nothing. Run them first when the symptom is
ambiguous.

## The startup phase tree

`tau run status` renders one ordered tree for every run:

```
Submitted → Kueue admission → [MultiKueue placement] → [RayCluster]
  → Pod scheduling → DRA allocation → Image pull → Init containers
  → Container start → Ready → [RayJob status]
```

Bracketed phases appear conditionally: `RayCluster`/`RayJob status` only for
RayJobs, `MultiKueue placement` only when the workload dispatched to a worker
cluster. Each phase reports `pending`, `active`, `done`, `warning`, or
`skipped`.

`skipped` is not failure. A skipped DRA phase is expected when the workload
requests GPUs through the device plugin instead of DRA.

Three distinctions that prevent misdiagnosis:

- **Admitted ≠ scheduled.** Quota was reserved; no pod exists yet.
- **Running ≠ progressing.** Containers started; the loop may be hung.
- **Completed ≠ evidence preserved.** Verify artifacts actually landed.

## 1. Repository/connection resolution and cluster access

```bash
tau workspace connection inspect
```

**Success proves:** exactly one `tau/workspace.connection.yaml` was found for
the current project, it passes schema validation, and Tau can name the cluster
context and workspace it targets. This is offline — it does not call the
cluster.

**Failure means:** no descriptor, more than one candidate, or a schema
violation. This is a repository configuration problem; every later layer is
unreachable until it is fixed.

Resolution and access are separate proofs. The first command that actually
calls the API server — `tau workspace status`, or any `tau run` — is what
proves access. A timeout or 401/403 there while `connection inspect` succeeded
means kubeconfig/VPN/DNS reachability or RBAC, not a bad descriptor.

**Next:** missing/invalid descriptor → get a valid one from the platform owner.
Reachability or permission failure → platform action, or transient network.

## 2. TauWorkspace readiness

```bash
tau workspace status <name>
tau workspace check  <name>      # exits non-zero unless Ready — use in scripts
tau workspace status <name> -o json
```

**Success proves:** `status.phase` is `Ready` — `RBACReady` and `QueueReady`
true, no drift. Ready proves the workspace handoff is valid for submitting; it
does not prove Azure infrastructure was just created, and it does not
substitute for per-run queue admission (layer 4).

**It also does not prove storage works.** TauGrid 0.1 has no `StorageReady`
condition on `TauWorkspace` or `TauCluster`. A `Ready` workspace can still have
a missing or unbound platform-managed PVC, so if the symptom is storage-shaped,
check the PVC directly rather than trusting the phase.

**Failure means:** `Pending` or `Degraded` — a platform-owned condition is
unmet. Resubmitting will not fix it.

`WorkloadIdentityReady` is diagnostic and does not gate the phase, but resolve
it before running workloads that use Azure Workload Identity.

**Next:** platform action. Do not hand-edit RBAC, queues, or storage to work
around a Degraded condition — see [platform.md](platform.md#recovering-a-degraded-workspace).
There is no client-side bypass; the submission gate is enforced server-side.

## 3. Config validation and rendering

```bash
tau run validate --config tau/train.yaml   # schema only
tau run train --dry-run=client             # renders locally; catches preset/GPU errors
tau run train --dry-run=server             # render + API-server dry-run, no admission
```

**Run both.** `validate` is schema-only; preset resolution and GPU-count
arithmetic happen at render time, so a config can validate clean and still fail
to render (missing `policy.preset`, or `processes_per_node` exceeding the
preset's GPU count). Neither step contacts the cluster for
`--dry-run=client`.

**Success proves:** the config parses, passes schema validation, and resolves
to a renderable Job or RayJob.

**Failure means:** a schema/field error, an unresolvable preset, an ambiguous
target, or an SDK-generated managed manifest being run through the direct path.
All authoring problems — nothing here touches a cluster, queue, or node.

If dry-run reports the entrypoint script missing, that is a working-directory
problem, not a config defect — dry-run resolves `entrypoint` on disk relative
to the config.

**Next:** researcher fixes the config. See
[run-config.md](run-config.md#error-message--cause) for the error→cause map.

## 4. Kueue admission and quota

```bash
tau run status <run-name>
```

Read the **Kueue admission** line: `N/M admitted queue=<names>`, plus a
`reason=` hint when not admitted.

**Success proves:** quota was reserved. It does **not** prove pods were
scheduled — continue to layer 5.

**Failure means:** `0/N admitted` with a quota reason → the LocalQueue /
ClusterQueue has no capacity right now, including borrowing limits. A reason
mentioning preemption or eviction → Kueue reclaimed capacity for
higher-priority work.

Do not proceed to GPU or node debugging (layer 6) before this reports `done`.
A workload waiting on quota looks exactly like a workload waiting on nodes if
you skip this line.

**Next:** platform/queue owner for capacity or priority. Operator-only
confirmation:

```bash
kubectl get workload -n <namespace>
kubectl describe clusterqueue <cluster-queue-name>
kubectl describe localqueue  <local-queue-name> -n <namespace>
```

## 5. Scheduling, DRA, image pull, init, and readiness

```bash
tau run status <run-name> --watch
```

Read the remaining phases in order.

| Phase stuck | Likely cause | Owner |
|---|---|---|
| Pod scheduling | Node selector/taint mismatch, or admission (layer 4) not actually complete | Platform / re-check layer 4 |
| DRA allocation (>~30s) | No matching GPU device; check `ResourceSlice` availability | Platform |
| Image pull (`ErrImagePull`/`ImagePullBackOff`) | Wrong image/tag, registry credentials, node egress | Researcher or image owner |
| Init containers | Init step crashing | Researcher |
| Container start | Application crashing before ready | Researcher |

**Next:** operator-only deep inspection, *after* the tree identified the phase:

```bash
kubectl describe pod <pod-name> -n <namespace>
kubectl get resourceclaim -n <namespace>
kubectl get events -n <namespace> --sort-by=.lastTimestamp
```

If the tree shows only a **MultiKueue placement** phase and no local pod phases
progress, the workload dispatched to a worker cluster — inspect it from the
worker context before assuming it is stuck. Confirm that the selected
`multiKueue` profile is Ready and that the current `TauCluster/cluster`
`MultiKueueReady` condition still reports healthy AdmissionCheck, config, and
worker prerequisites.

## 6. GPU/node/topology health

```bash
tau cluster validate nodes --gpu-class <class> --min-healthy <N> --timeout 2m
tau cluster validate topology --preset <preset-name>
tau cluster validate topology --cluster-queue taugrid-cq
```

Both require cluster-admin-level access — `validate nodes` runs privileged
pods. This is operator diagnosis, not a researcher-facing step.

`validate nodes` flags: `--context`, `--gpu-class`, `--selector` (alternative
to `--gpu-class`), `--min-healthy`, `--timeout` (default `2m`, per pod). It
checks `nvidia-smi`, NVLink, IB, and ECC health.

`validate topology` flags: `--context`, `--preset` (one preset's full chain:
LocalQueue, ClusterQueue, topology, priority classes, ResourceFlavor node
match), `--cluster-queue` (default `taugrid-cq`; validates all ResourceFlavors
referenced by a ClusterQueue when `--preset` is omitted).

**Failure means:** a node reported `DEGRADED`/`UNHEALTHY` for a specific reason
(NVLink down, uncorrectable ECC, IB down) — a hardware problem, not a Tau or
application bug. Zero matching nodes for a ResourceFlavor means the node pool,
instance type, or device plugin does not match what the preset expects.

## 7. Runtime progress and durable evidence

```bash
tau run logs <run-name>
taugrid-portal experiment status <name>
tau run get <run-name>
```

`tau run logs` fetches the Ray Job **driver** execution log for a RayJob (not
head-pod container logs), or the batch Job's pod logs.

**Success proves:** the driver log shows real progress (loss decreasing, steps
advancing, checkpoints written) and that progress is mirrored into the durable
experiment record.

**Failure means:**

- No progress despite `Ready`/`Running` → application-level hang (data loader
  stall, deadlock, collective waiting on a dead rank). Not infrastructure.
- Empty experiment record but healthy logs → the metrics-offload path or
  checkpoint contract is not wired for this run. Not a training failure.

**Next:** no progress in logs → researcher (application logic). Missing
evidence with healthy logs → platform, for offload/expstore configuration.

## 8. Recovery handoff

Only after you have located the first failed layer, choose a recovery action.
Retrying past an unresolved layer 1–3 problem, a Degraded workspace, or a real
quota/node problem just reproduces the same failure.

Automatic retry is config (`resilience.max_retries > 0`), not a command — there
is no `tau run retry`. Manual resume:

```bash
tau run resume <run-name> --config tau/train.yaml   # --config required
tau run resume <run-name> --config tau/train.yaml --from <checkpoint-dir>
tau run resume <run-name> --config tau/train.yaml --force   # required after OOMKilled
```

Resume requires a durable checkpoint under `/data`. Node-local scratch does not
survive workload deletion, so there is nothing to resume from.

Failure classification: `OOMKilled`, `Preempted`, `Evicted`, `Completed`,
`Running`, `Unknown`. `Unknown` is never retryable in either path — if Tau
cannot classify the failure, the signal you would need to decide "retry" vs
"fix and resubmit" is missing. Inspect status and logs instead of looping.

## Symptom index

| Symptom | Start at | Most common cause |
|---|---|---|
| "Job is Pending and nothing happens" | Layer 4 | No queue quota right now |
| "Admitted but no pods" | Layer 5 | Node selector/taint mismatch, or no capacity |
| "ImagePullBackOff" | Layer 5 | Wrong tag or missing registry credentials |
| "Workspace not Ready / submission denied" | Layer 2 | Platform-owned condition unmet |
| "Config error on submit" | Layer 3 | Unknown field or engine/field mismatch |
| "Running but loss never moves" | Layer 7 | Application hang; infra is fine |
| "Job disappeared mid-run" | Layer 4 | Preempted by higher-priority work |
| "Dashboard empty but job ran" | Layer 7 | Metrics offload not wired; local expstore still authoritative |
| "Worked yesterday, fails today" | Layer 6 then 4 | Node health regression, or quota consumed by other work |
| "`tau: unknown command`" | — | Stale binary, or a removed pre-v0.5 root; run `tau --help` |
