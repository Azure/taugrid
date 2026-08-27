---
title: Workload profile migration
linkTitle: Update workload profiles
weight: 70
description: Move placement catalogs into TauCluster and operate profile revisions safely
aliases:
  - "/docs/operations/workload-profiles/"
---

Workload profiles are platform-owned declarations on the singleton
`TauCluster/cluster`. They replace the removed file-based `TopologyPolicy`
catalog. The controller resolves referenced queues and scheduling objects into
status; TauGrid reads only that status as the single source of truth, rather than
falling back to a compiled catalog or a local policy file.

## Migrate the catalog

Translate each supported workload shape into `spec.workloadProfiles`. Keep live
quota, capacity, ResourceFlavor selectors, and topology object names out of the
declaration: those identities are observations in status.

```yaml
apiVersion: tau.azure.com/v1alpha1
kind: TauCluster
metadata:
  name: cluster
spec:
  workloadProfiles:
    - name: research.training.1x
      description: One-GPU training through the shared queue.
      applicability:
        teams: [research]
        lanes: [training]
        namespaces: [research-workloads]
      gpusPerWorker: 1
      workerCount: 1
      mode: fixed
      placement: independent
      defaultLocalQueue: jobqueue
      executionTarget: singleCluster
      priorities:
        workloadPriorityClassName: taugrid-default
        podPriorityClassName: taugrid-default
```

The controller chart's `tauCluster.workloadProfiles` value is the canonical
default catalog. Helm lists replace rather than merge, so a site override must
supply the complete reviewed catalog. The chart synchronization test keeps the
checked-in Helm, Kustomize, and controller sample declarations identical.

Delete old policy ConfigMaps, volume mounts, environment variables, and files
only after every submitter uses a ready TauCluster profile. There is no
compatibility fallback.

## Readiness is fail closed

TauGrid accepts a profile set only when all of the following are true:

1. `status.workloadProfiles.observedGeneration` equals
   `metadata.generation`.
2. the `WorkloadProfilesReady` condition is `True` for that generation;
3. the published `profileSetHash` is non-empty and matches the normalized
   resolved profiles;
4. the selected profile's `Ready` condition is `True` for that generation; and
5. namespace, team, and lane applicability authorize the caller.

Missing, stale, drifted, forbidden, ambiguous, or unready data stops rendering.
TauGrid stamps successful output with
`tau.azure.com/tau-cluster-generation`,
`tau.azure.com/workload-profile-set-hash`, and
`tau.azure.com/workload-profile`. These annotations identify the observed
revision only; confirm quota or capacity availability with a separate check.

## Connected and offline rendering

A normal run, server dry-run, or apply reads the connected cluster:

```bash
tau run --config tau.yaml --dry-run=server
```

Client rendering is still connected unless an explicit snapshot is configured.
Export only a ready revision:

```bash
tau cluster profiles export --context my-cluster \
  --output profiles.snapshot.yaml
```

Then set `policy.workload_profile_snapshot: profiles.snapshot.yaml` and provide
explicit `policy.namespace`, `policy.team`, and `policy.lane`. Snapshot input is
accepted only with `--dry-run=client`; it cannot authorize server dry-run or
apply. Treat snapshots as immutable review artifacts and re-export after a
TauCluster revision.

## MultiKueue readiness and ownership

The `multiKueue` execution target has one deterministic profile contract:

1. the standard distribution installs MultiKueue controller support;
2. `TauCluster.status.conditions[MultiKueueReady]` reports a current, active
   AdmissionCheck, referenced MultiKueueConfig, and active worker;
3. the catalog includes a ready profile with `executionTarget: multiKueue`, a
   dedicated LocalQueue, and the ordinary team, namespace, and lane
   applicability required by the operator; and
4. profile selection resolves that profile explicitly through `policy.profile`
   or implicitly as the unique ready, applicable profile.

Failure of readiness or profile resolution stops dispatch. A MultiKueue profile
uses the same fail-closed applicability and ambiguity rules as every other
execution target. The supported boundary is TauGrid-rendered workloads whose kind
and dependencies are configured on the manager and every eligible worker.
Direct `kubectl apply`, hand-written Workloads, and objects mutated after TauGrid
renders them are outside this contract.

The platform owner owns worker credentials, least-privilege access,
distribution and rotation, queue isolation, namespace and ServiceAccount
parity, image pull identity, storage reachability, and revocation. This
constrained supported capability remains Alpha until release evidence covers
an environment/version matrix, manager-to-worker E2E, negative authorization
and credential tests, operational enablement, credential rotation/revocation,
drain, and rollback.

## Drain and roll back

Before changing queue bindings, profile scope, or execution target:

1. stop new submissions and set affected ClusterQueues to `HoldAndDrain`;
2. cancel pending owners and wait for pending, reserving, and admitted counts to
   reach zero;
3. export the current TauCluster object and ready profile snapshot;
4. apply the new catalog and wait for the new generation and every selected
   profile to become Ready; then run connected server-dry-run checks; and
5. restore admission.

Rollback by draining again, restoring the previous catalog, waiting for its new
generation to become Ready, removing the MultiKueue profile and routing objects,
then revoking unused worker credentials. Running pods stay pinned to their
original placement when a profile changes. Removing profile authorization
blocks new TauGrid submissions only. Continue status, log, cancellation, and
cleanup operations until every manager and worker object is terminal, since
active remote workloads still require active management. Controller status is
an observation rather than a reservation: queues, credentials, storage, nodes,
and capacity can change between readiness, render, admission, and scheduling.
