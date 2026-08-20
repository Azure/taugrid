---
title: Multi-cluster execution
weight: 4
description: Alpha dispatch behind explicit Beta installation and runtime gates
---

{{< maturity status="alpha" reviewed="2026-08-20" >}}

End-to-end constrained worker dispatch remains **Alpha**. The distribution and
controller runtime gates are Beta-quality safety boundaries: they prevent an
installer from activating MultiKueue accidentally, but they do not promote
tenant authorization, workload dispatch, or arbitrary worker compatibility.

## Supported manager environment

| Component | Supported contract |
|---|---|
| TauGrid | Chart and images `0.3.x` from the same release |
| Kubernetes | `1.30` or newer, matching the TauGrid chart constraint |
| Kueue | TauGrid-pinned AKS chart and controller `0.18.2` |
| KubeRay | TauGrid-pinned operator `1.6.2` for RayJob dispatch |
| Platform | AKS manager and workers with operator-managed identities, networking, storage, GPU drivers, and compatible CRDs |

Use the same Kubernetes, Kueue, KubeRay, and workload CRD minor versions on the
manager and every worker. Other combinations are not covered by this gate.

## Platform-owner enablement

Default `taugrid`, `taugrid-core`, and `tau-core-controller` installs do not
activate MultiKueue. They omit MultiKueue CRDs and specific RBAC, explicitly
disable the pinned Kueue controller feature, give the Tau controller no
MultiKueue prerequisite permissions, and create no AdmissionChecks,
MultiKueueConfigs, MultiKueueClusters, worker credentials, or queue/profile
wiring.

Enable only the manager-side install/runtime boundary with the reviewed values:

```bash
helm upgrade --install taugrid \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid \
  --version 0.3.0 \
  --namespace tau-system --create-namespace \
  --values values-multikueue-beta.yaml \
  --wait --atomic
```

The file supplies both independent approvals:

```yaml
global:
  betaFeatures: [multikueue]
  betaRiskAcknowledgements: [multikueue]
kueue:
  aksExtension:
    enableMultiKueue: true
```

Setting only one approval fails Helm rendering. Raw Kueue feature gates,
aliases, subchart values, or custom MultiKueue queue/profile values also fail
without both approvals. Direct component installs enforce the same lists. The
Tau controller binary separately defaults `--beta-feature-gates` to empty, so
editing `TauCluster.spec` cannot activate support in a binary installed without
`--beta-feature-gates=multikueue`.

The install values intentionally leave the operator gate disabled. After the
manager prerequisites below are configured, set it independently:

```yaml
tau-core-controller:
  tauCluster:
    features:
      multiKueue: Beta
```

The acknowledged installation enables controllers and read-only prerequisite
checks only. It does not create worker identities or routing resources. Before
exposing a Beta profile, the platform owner must:

1. Provision a least-privilege worker kubeconfig Secret and protect it from
   tenants. TauGrid never creates or copies worker credentials.
2. Create explicit MultiKueueCluster and MultiKueueConfig objects.
3. Create a dedicated AdmissionCheck whose `spec.controllerName` is exactly
   `kueue.x-k8s.io/multikueue`, and wait for it and at least one referenced
   worker to report `Active=True`.
4. Create a dedicated, non-global Beta queue/profile. The default `jobqueue`
   remains single-cluster and must never receive a MultiKueue AdmissionCheck.
5. Verify workload dependencies on every eligible worker before authorizing
   tenants.

Readiness is based on manager-visible prerequisites, not enablement alone: the
controller identity, active AdmissionCheck, referenced MultiKueueConfig, and an
active worker must all resolve. AdmissionCheck names are not authoritative.

## Tenant and workload boundary

The four ordered gates are:

1. distribution install approval and controller activation;
2. controller runtime capability plus `TauCluster.spec.features.multiKueue=Beta`
   and `MultiKueueReady=True`;
3. a ready `multiKueueBeta` profile backed by a dedicated queue and explicit
   team/namespace applicability; and
4. researcher acknowledgement through `execution.beta_features: [multikueue]`
   or `--acknowledge-beta-feature multikueue`.

See [workload profile migration](../workload-profiles/#multikueue-gates-and-ownership)
for the profile contract. MultiKueue profiles are never implicit defaults.
Client dry-run, server dry-run, and apply enforce the same gate. Direct
`kubectl` submissions and objects mutated after Tau renders them are
operator-controlled, out-of-contract paths; TauGrid does not install a
name-based CEL policy or admission webhook.

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
| Helm fails naming the two global values | One approval is missing, or custom MultiKueue values attempted to bypass the gate. Use the reviewed values file. |
| `MultiKueueReady=False` with `RuntimeDisabled` | Reinstall the Tau controller through the acknowledged path. No AdmissionCheck reads occur in this state. |
| `MultiKueueReady=False` with `OperatorDisabled` | Set the operator gate to Beta only after manager prerequisites exist. |
| `MultiKueueReady=False` with `PrerequisitesNotReady` | Inspect the named AdmissionCheck, config, worker, credentials, and Kueue controller status. |
| Placement remains pending or retries | Check worker connectivity, quota, dependency parity, image pulls, and worker events. |
| Manager has no local pods | This is normal after remote dispatch. Continue manager-side status, logs, cancellation, and cleanup. |

## Drain, disable, and rollback

1. Remove tenant authorization and stop new submissions to every dedicated Beta
   profile/queue. Do not repoint the default queue.
2. Hold and drain the dedicated ClusterQueues and explicitly cancel unwanted
   pending owners.
3. Wait for every manager Workload to become terminal and for remote cleanup and
   finalizers to complete. Verify worker Jobs/RayJobs and pods directly.
4. Remove dedicated AdmissionCheck/profile/queue wiring, set the TauCluster
   operator gate to Disabled, then upgrade with default values to disable both
   controller runtime gates and remove MultiKueue-specific RBAC.
5. Revoke and delete worker credentials only after no active or retained remote
   workload needs them.

Disabling controllers or abandoning credentials while remote workloads are
active can orphan expensive pods, leave finalizers stuck, lose centralized log
resolution, and prevent manager-side cancellation. Default-off gates block new
use; they do not disable status, logs, cancellation, resume, or cleanup for
already-existing MultiKueue workloads.

## Promotion blockers

MultiKueue remains Alpha until released evidence covers manager-to-worker E2E
across this environment matrix, negative authorization and credential tests,
credential rotation and revocation, and repeatable enablement, drain, and
rollback exercises.

Promotion is blocked on a supported environment/version matrix, released
manager-to-worker E2E coverage, negative authorization and credential tests,
credential rotation/revocation evidence, and repeatable enablement, drain, and
rollback exercises.
