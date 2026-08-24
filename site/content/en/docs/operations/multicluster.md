---
title: Multi-cluster execution
weight: 4
description: Deterministic dispatch to preconfigured MultiKueue workers
---

{{< maturity status="alpha" reviewed="2026-08-20" >}}

TauGrid supports deterministic dispatch from a manager to preconfigured
MultiKueue workers in the environment below. The capability remains **Alpha**
because its released environment and operational evidence are still narrow.
It does not provide arbitrary worker discovery or unrestricted cross-cloud
routing.

## Supported manager environment

| Component | Supported contract |
|---|---|
| TauGrid | Chart and images `0.3.x` from the same release |
| Kubernetes | `1.30` or newer, matching the TauGrid chart constraint |
| Kueue | TauGrid-pinned AKS chart and controller `0.18.2` |
| KubeRay | TauGrid-pinned operator `1.6.2` for RayJob dispatch |
| Platform | AKS manager and workers with operator-managed identities, networking, storage, GPU drivers, and compatible CRDs |

Use the same Kubernetes, Kueue, KubeRay, and workload CRD minor versions on the
manager and every worker. Other combinations are outside the supported
contract.

## Platform-owner configuration

The standard `taugrid` installation enables the pinned Kueue MultiKueue
controller capability and gives the Tau controller read-only access to its
prerequisites:

```bash
helm upgrade --install taugrid \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid \
  --version 0.3.1 \
  --namespace tau-system --create-namespace \
  --wait --atomic
```

There is no separate values file or approval list. Installation supplies
controller support; it does not create worker identities or routing resources.
Before publishing a MultiKueue profile, the platform owner must:

1. Provision a least-privilege worker kubeconfig Secret and protect it from
   tenants. TauGrid never creates or copies worker credentials.
2. Create explicit MultiKueueCluster and MultiKueueConfig objects.
3. Create a dedicated AdmissionCheck whose `spec.controllerName` is exactly
   `kueue.x-k8s.io/multikueue`, and wait for it and at least one referenced
   worker to report `Active=True`.
4. Create a dedicated, non-global MultiKueue queue/profile. The default `jobqueue`
   remains single-cluster and must never receive a MultiKueue AdmissionCheck.
5. Verify workload dependencies on every eligible worker before authorizing
   tenants.

`TauCluster.status.conditions[MultiKueueReady]` reports actual manager-visible
prerequisite health: a correctly controlled and active AdmissionCheck, its
referenced MultiKueueConfig, and at least one active MultiKueueCluster must all
resolve. AdmissionCheck names and installed controller support alone are not
readiness.

## Tenant and workload boundary

Dispatch requires all of the following:

1. the standard distribution's MultiKueue controller support;
2. a current `MultiKueueReady=True` condition proving manager prerequisites;
3. a ready, applicable `multiKueue` profile backed by a dedicated queue; and
4. explicit selection through `policy.profile` or unambiguous implicit
   selection as the unique ready, applicable profile.

See [workload profile migration](../workload-profiles/#multikueue-readiness-and-ownership)
for the profile contract. Client dry-run, server dry-run, and apply use the same
profile selection rules. Direct `kubectl` submissions and objects mutated after
Tau renders them are operator-controlled, out-of-contract paths; TauGrid does
not install a name-based CEL policy or admission webhook.

## Worker dependency and security contract

Every eligible worker needs compatible Kubernetes, Kueue, KubeRay, and workload
CRDs plus matching queue, ServiceAccount, Secret, StorageClass, PVC, image,
dataset, checkpoint, GPU, topology, network, and storage capabilities. Grant
worker credentials only the verbs and namespaces Kueue needs, rotate them
through the platform identity system, and never expose them in researcher
namespaces.

Automatic dataset replication is Planned. Running pods and node-local scratch
do not migrate. Do not remove cluster pins from stateful workloads until common
storage and identity contracts are activated and verified on every worker.

## Failure modes

| Symptom | Meaning and action |
|---|---|
| `MultiKueueReady=False` with `PrerequisitesNotReady` | Inspect the named AdmissionCheck, config, worker, credentials, and Kueue controller status. |
| MultiKueue profile is not Ready | Inspect its `ExecutionReady` condition and verify that every referenced ClusterQueue uses the ready AdmissionCheck. |
| Placement remains pending or retries | Check worker connectivity, quota, dependency parity, image pulls, and worker events. |
| Manager has no local pods | This is normal after remote dispatch. Continue manager-side status, logs, cancellation, and cleanup. |

## Drain and rollback

1. Remove tenant authorization and stop new submissions to every dedicated
   profile/queue. Do not repoint the default queue.
2. Hold and drain the dedicated ClusterQueues and explicitly cancel unwanted
   pending owners.
3. Wait for every manager Workload to become terminal and for remote cleanup and
   finalizers to complete. Verify worker Jobs/RayJobs and pods directly.
4. Remove the MultiKueue profiles, then remove dedicated
   AdmissionCheck/queue wiring after no workload references it.
5. Revoke and delete worker credentials only after no active or retained remote
   workload needs them.

Disabling controllers or abandoning credentials while remote workloads are
active can orphan expensive pods, leave finalizers stuck, lose centralized log
resolution, and prevent manager-side cancellation. Removing profile
authorization blocks new Tau submissions; it does not disable status, logs,
cancellation, resume, or cleanup for already-existing MultiKueue workloads.

## Promotion blockers

MultiKueue remains Alpha until released evidence covers manager-to-worker E2E
across this environment matrix, negative authorization and credential tests,
credential rotation and revocation, and repeatable enablement, drain, and
rollback exercises.

Promotion is blocked on a supported environment/version matrix, released
manager-to-worker E2E coverage, negative authorization and credential tests,
credential rotation/revocation evidence, and repeatable enablement, drain, and
rollback exercises.
