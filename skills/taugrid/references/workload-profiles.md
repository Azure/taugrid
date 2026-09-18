# Workload profiles and offline rendering

The platform declares `spec.workloadProfiles` on the singleton
`TauCluster/cluster`. The controller resolves queues, ResourceFlavors,
topologies, and priority classes into status. The CLI reads this ready status,
not a compiled catalog, local profile directory, or `TopologyPolicy` file.

## Choose an authorized shape

Ask for the platform's profile names and namespace/team/lane contract. For
connected inspection:

```bash
tau cluster profiles export --context <context> --output profiles.snapshot.yaml
tau cluster validate topology --context <context> --profile <profile>
```

Export reads Kubernetes and writes a local snapshot; create the output parent
directory first if necessary. `validate topology` reads the current profile
chain. Omitting `--profile` checks all ready profiles; old `--preset` and
`--cluster-queue` flags are not supported.

Use `policy.profile` in a run config. Omission works only if exactly one ready,
applicable profile remains. Profiles can retain old-looking names such as
`azure.research.training.l`, but that does not make `policy.preset` valid or
guarantee the name exists.

The profile's `workerCount` and `gpusPerWorker` map to:

- Job: `execution.nodes` and `compute.gpus`.
- RayJob: `compute.workers` and `compute.gpus_per_worker`; the control-only head
  is separate.

Explicit counts must match. The profile also owns LocalQueue/ClusterQueue
bindings, mode, placement, and priority classes. CPU/memory/image/storage
settings are still workload-owned. Do not force node selectors, GPU class,
shape, or priority tier around an incompatible profile.

## Distinguish three validation levels

1. `tau run validate --config <file>` checks direct config/dispatch locally.
   It does not look up profiles, prove permission, or reserve quota.
2. Client rendering normally resolves the active workspace and live profile.
   It does not apply the workload, but **is connected** and may require
   interactive trust/authentication. ServiceAccount values may remain
   placeholders.
3. Server dry-run rechecks connected dependencies and sends Kubernetes dry-run
   requests. It does not prove Kueue admission, GPU allocation, image pull,
   storage I/O, or application execution.

## Render offline explicitly

Obtain an exported ready snapshot from the platform owner or export it while
connected. Keep an offline-review copy of the run config next to the source
files and add this fragment (replace values with the snapshot's scope):

```yaml
policy:
  profile: <profile>
  namespace: <workload-namespace>
  team: <team>
  lane: <lane>
  workload_profile_snapshot: profiles.snapshot.yaml
```

```bash
tau run validate --config tau/train.offline.yaml
tau run --config tau/train.offline.yaml --dry-run=client
```

The snapshot path resolves relative to that config. The snapshot branch skips
the repository connection and live TauCluster lookup, so it cannot validate
current workspace output-root policy, RBAC, storage, quota, or capacity.
Never use a synthetic test snapshot to authorize a real deployment.

Snapshots are accepted **only** for client rendering. Remove
`policy.workload_profile_snapshot` when returning to a connected config, then
re-run connected validation. Do not pipe a snapshot-rendered manifest straight
to `kubectl apply` to bypass that check.

## Interpret profile readiness

Resolution rejects:

- Missing or stale `status.workloadProfiles.observedGeneration`.
- `WorkloadProfilesReady` not true for the current generation.
- Missing/mismatched `profileSetHash`.
- A selected profile whose `Ready` condition is not current and true.
- Namespace/team/lane not applicable, or ambiguous selection.

Rendered annotations record generation, profile-set hash, and selected profile
name. They are provenance, not reservations. A green profile does not mean
capacity will still be available when Kueue evaluates the workload.

For `executionTarget: multiKueue`, check current `MultiKueueReady` and the
manager/worker prerequisites. The platform owns remote credentials, queue
isolation, storage reachability, and ServiceAccount/image-pull parity. Do not
construct worker access from a snapshot. Status/log/cleanup operations may
still be needed even when a profile stops authorizing new submissions.
