# Troubleshooting runs and evidence

Diagnose from observed state, not a fixed assumption about the failing layer.
A missing connection, rejected config, queued workload, and vanished RayJob
need different starting points. Stay read-only unless repair is authorized.

## Establish the exact target

```bash
tau version
tau run status <run-name>
tau run status <run-name> -o json
tau run status <run-name> --diagnostic-hints
tau logs <run-name> --tail 100
```

For a known target use `--context <context> --namespace <namespace>`.
In a monorepo, run lifecycle commands accept `--project <project>`.
Root `tau logs` can search locally configured workspace connections;
ambiguity is a reason to select an exact workspace/context, not guess.

Use JSON for agent processing. `--watch` cannot be combined with JSON or
`--diagnostic-hints`; a bounded human-readable watch can use
`--max-iterations`. JSON already includes diagnostic commands, so do not
reconstruct them from guessed labels.

If no workload was created, start with local config validation:

```bash
tau run validate --config tau/train.yaml
```

This is offline. `tau workspace connection`, workspace status, profile export,
and ordinary client rendering are connected. There is no
`tau workspace connection --offline`. For offline-only work inspect the
non-secret descriptor as a file and use a platform-supplied profile snapshot;
do not read the credential cache.

## Find the first blocked startup phase

The status tree includes conditional phases:

```text
Submitted -> Kueue admission -> [MultiKueue placement] -> [RayCluster]
  -> Pod scheduling -> DRA allocation -> Image pull -> Init containers
  -> Container start -> Ready -> [RayJob status]
```

`pending`, `active`, `done`, `warning`, and `skipped` have different meanings.
A skipped DRA phase is normal for device-plugin GPUs. Investigate the first
genuinely blocked/warning phase; do not stop at an expected skipped phase or
assume every active phase is broken.

| Evidence | Next check | Likely owner |
| --- | --- | --- |
| Interactive trust/authentication required | User completes `tau workspace connection` in an interactive terminal | Researcher/access owner |
| Workspace Pending/Degraded or cached UID differs | `tau workspace status`; inspect/reconnect the intended workspace | Platform; user for reconnect |
| No ready/applicable profile or multiple candidates | Current TauCluster generation/hash, scope and profile name | Platform |
| Config validation/render error | [Run config](run-config.md); config-relative paths and profile assertions | Researcher |
| Kueue admission blocked | Queue reason, quota/borrowing/priority | Queue owner |
| Admitted but unscheduled | Node selectors/taints, free resources, topology or DRA | Platform |
| ImagePullBackOff | Image reference, registry identity, node egress | Image/platform owner |
| Init failure | Source staging, PVC mount, identity, init logs | Depends on the failed step |
| Running with no useful progress | Driver/worker logs, application and distributed communication | Researcher/platform |
| Completed with no results | Actual output PVC/path, publication and offload | Researcher/platform |

Admitted means quota was reserved, not that pods were scheduled. Running means
containers started, not that the training loop progresses. A hang can still
involve storage/network/GPU failure; do not declare infrastructure healthy from
pod phase alone.

## Workspace, profiles, and storage

```bash
tau workspace status <workspace> --system-namespace <system-namespace> -o json
tau workspace check <workspace> --data-pvc <pvc>
tau cluster profiles export --context <context>
tau cluster validate topology --context <context> --profile <profile>
```

Workspace Ready gates on RBAC/queue conditions and absence of drift, not on a
`StorageReady` condition. PVC diagnostics may warn without making
`workspace check` fail; read the output. A Bound PVC still does not prove
mounting/writing succeeds. Workload identity readiness is diagnostic.

Inspect the rejected profile's applicability before changing the config.
Counts, queue, placement, and priority settings cannot override the profile.
Connected client dry-run also checks the live workspace output-root boundary;
an offline snapshot render does not.

## Targeted Kubernetes inspection

When Tau has identified the relevant layer, inspect only necessary objects
within the caller's RBAC:

```bash
kubectl --context <context> -n <namespace> get workloads
kubectl --context <context> -n <namespace> describe localqueue <queue>
kubectl --context <context> describe clusterqueue <clusterqueue>
kubectl --context <context> -n <namespace> describe pod <pod>
kubectl --context <context> -n <namespace> get events --sort-by=.lastTimestamp
```

Cluster-scoped queue/node information may require a platform operator. Do not
dump Secret objects or credential files. `tau cluster validate nodes` creates
privileged pods and needs separate authorization; it is not a read-only
equivalent of topology inspection.

For MultiKueue, manager-side absence of pods can mean execution moved to a
worker. Use the placement phase and platform-provided worker context. Check
current `MultiKueueReady`; do not manually create another worker copy or alter
credentials to recover status.

## Live and historical logs

`tau logs` / `tau run logs` read RayJob driver execution logs through Ray's
dashboard API while local worker access is available. For batch Jobs, use
`--container`, `--previous`, `--all-containers`, `--timestamps`, or `--prefix`
as needed. These container-specific flags are not a Ray driver-log contract.

After terminal local RayJob pods are cleaned up, Tau can read ADX-offloaded
logs using `tau-log-connection` in the selected cluster's system namespace.
Override `--kusto-endpoint`, `--kusto-database`, and `--kusto-cluster` only
with platform-supplied metadata. Here `--kusto-cluster` means the telemetry
source `Cluster`, **not** the Azure ADX resource name.

Manager-side MultiKueue RayJob logs use central ADX ContainerLogs after the
worker is known. Missing ADX configuration or failed offload does not justify
claiming logs were preserved. Expired Kubernetes objects also do not prove
preemption; inspect retention and durable lifecycle history.

## Experiments, datasets, and artifacts

Use the separate portal binary and identify the intended store:

```bash
taugrid-portal experiment --store <store-path> search
taugrid-portal experiment --store <store-path> status <name>
taugrid-portal experiment --store <store-path> stellar <name> -o json
tau run get <run-name>
```

Local expstore, ADX metrics, ADX logs, and lifecycle history are distinct.
An empty dashboard may mean wrong scope/store, no published metric chunks,
missing identity, or a broken projection. Do not assert that a local packet
exists until you have checked it. `metrics.offload` rejects multi-node direct
Indexed Jobs; use the supported single-pod Job/RayJob path.

Dataset lookup and verification:

```bash
tau data dataset list -o json
tau data dataset show <dataset>@<version>
tau data dataset ref <dataset>@<version>
tau data dataset verify <dataset>@<version>
tau data model list -o json
tau data model show <model>/<run>
tau data model best <model>
```

Registry backend determines whether these read local files, PVC data, or Azure
storage; byte verification can be expensive. Dataset `ingest` transfers bytes,
`alias set` mutates pointers, and removal/index rebuild change state. Do not
run them as passive diagnostics. `tau run get` retrieves the recorded output;
storage helpers may need pod access, so it is not an offline metadata check.

## Retry and resume

First fix the actual cause. Automatic retry is config-driven and disabled by
default; it permits `Preempted`/`Evicted`, with `OOMKilled` opt-in.
Manual resume requires a failed existing workload and its config:

```bash
tau run resume <run-name> --config tau/train.yaml \
  --from /data/projects/<workspace>/runs/<run-name>/checkpoints \
  --dry-run=client
```

This preview still reads live state but does not delete the original workload.
The normal command validates a replacement before deleting/resubmitting.
Use the actual durable checkpoint directory; a generic legacy default is not
proof the checkpoint exists. The application must read `TAU_RESUME_FROM`.

After OOM, raise the relevant resources or change the workload before
`--force`. `Unknown`, active, and successfully completed workloads are not
resume candidates. Metrics-enabled resume preserves its session's output
path/PVC. MultiKueue recovery waits for manager-side cleanup/finalizer proof;
do not short-circuit that wait by editing finalizers or submitting duplicates.
