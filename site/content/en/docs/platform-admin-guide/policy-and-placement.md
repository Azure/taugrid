---
title: Queue, quota, topology, and GPU placement
linkTitle: Manage queues and GPUs
weight: 40
description: How policy intent becomes admitted and scheduled pods
aliases:
  - "/docs/concepts/policy-and-placement/"
---

{{< maturity status="ga" reviewed="2026-09-23" >}}

TauGrid resolves the selected ready `TauCluster` workload profile; upstream systems
enforce the rendered queue, priority, resource, and placement contract. The
controller's resolved profile status is authoritative and stale status fails
closed. See [workload profile migration](../workload-profiles/).

| Stage | Owner | Decision |
|---|---|---|
| Target resolution | TauGrid | Requested workers, GPUs, placement, priority, and workspace defaults |
| Queue admission | Kueue | Whether shared quota may be consumed |
| Pod scheduling | Kubernetes | Which nodes satisfy resources, selectors, taints, and topology |
| Device allocation | Device plugin or DRA | Which concrete GPUs are assigned |
| Node scaling | Cluster infrastructure | Whether matching node capacity can appear |

LocalQueues are tenant-facing entry points. ClusterQueues own quota and fairness.
KEC-managed ResourceFlavors describe AKS resource pools. Priority and preemption
remain cluster-owned policy.

## Priority and preemption

TauGrid installs two workload and pod priority pairs: `taugrid-default` at
1000 and `taugrid-priority` at 1200. Runs select them with
`policy.priority_tier: default|priority`; consumers do not need to create
their own classes. An explicit run tier overrides only the priority pair from
the selected workload profile.

The baseline ClusterQueue uses Kueue `BestEffortFIFO`: higher resolved
Workload priority is considered before FIFO order, and equal-priority work is
ordered oldest first. `withinClusterQueue: LowerPriority` allows admitted
lower-priority work to be reclaimed for eligible higher-priority work in the
same ClusterQueue. Both pod classes use `PreemptLowerPriority`, which is a
separate Kubernetes scheduler decision after Kueue admission.

Portal queue views show the resolved Kueue priority class/value and the pod
priority class separately. "Pending admission," "quota admitted," and
"running" remain distinct states: quota, ResourceFlavor eligibility,
admission checks, and node availability can still prevent the highest
priority row from running immediately.

## GPU class contract

`policy.gpu_class` selects the hardware model represented by KEC's
`kubernetes.azure.com/sku-gpu-name` node and ResourceFlavor label:

| Researcher value | KEC selector |
|---|---|
| `any` | No class selector; any compatible GPU ResourceFlavor may be admitted |
| `a10` | `kubernetes.azure.com/sku-gpu-name=A10` |
| `a100` | `kubernetes.azure.com/sku-gpu-name=A100` |
| `h100` | `kubernetes.azure.com/sku-gpu-name=H100` |
| `h200` | `kubernetes.azure.com/sku-gpu-name=H200` |
| `gb200` | `kubernetes.azure.com/sku-gpu-name=GB200` |
| `gb300` | `kubernetes.azure.com/sku-gpu-name=GB300` |

TauGrid records the normalized lower-case value in workload metadata and
renders the exact upper-case KEC node selector for specific classes. Queue
preflight accepts only ResourceFlavors carrying the matching KEC label.
ResourceFlavor names such as `aks-h200-ndisr-v5` remain platform identifiers;
Tau never infers hardware from a flavor name.

KEC's taxonomy identifies the GPU model, not every memory partition or VM SKU.
Legacy memory-specific values such as `a100-80gb`, `h100-95gb`, and
`h200-141gb` are accepted temporarily and normalize to `a100`, `h100`, and
`h200` with a deprecation warning. A10 fractional-memory values similarly
normalize to `a10`.

Placement stays in `policy.topology`: `independent`, `single-node-nvlink`,
`multi-node-nccl`, or `elastic-workers`. GPU class values encode hardware
only; NVLink, InfiniBand, NCCL, same-host placement, and AKS SKU series remain
separate concerns.

## KEC-managed scheduling objects

KEC is the sole owner of AKS node classification, Topology, and
ResourceFlavors. TauGrid's baseline ClusterQueue references:

- `aks-cpu` for CPU and memory quota.
- Operator-selected KEC GPU flavors under `baselineQueue.gpu.flavors`.
- `aks-default` indirectly through each KEC-managed flavor's `topologyName`.

TauGrid does not patch KEC-managed scheduling objects or add competing node
labels. Installation readiness verifies that user nodes are classified,
`aks-default` and `aks-cpu` are converged, every selected GPU flavor exists,
and the baseline ClusterQueue is active.

To add GPU quota, first inspect the flavors KEC derived from live inventory:

```bash
kubectl get resourceflavors \
  -l app.kubernetes.io/managed-by=aks-managed-kueue-extension \
  -o custom-columns=NAME:.metadata.name,GPU:.spec.nodeLabels.kubernetes\\.azure\\.com/sku-gpu-name,SERIES:.spec.nodeLabels.kubernetes\\.azure\\.com/sku-series,TOPOLOGY:.spec.topologyName
```

Then reference the exact generated name:

```yaml
baselineQueue:
  gpu:
    flavors:
      - name: aks-h200-ndisr-v5
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "8"
```

CPU, memory, and GPU share one Kueue resource group. `aks-cpu` receives zero
GPU quota whenever GPU flavors are present; each selected GPU flavor receives
the configured CPU, memory, and GPU quotas so Kueue assigns one compatible
node flavor across the pod set's resources.

When changing selected flavors on a live cluster, hold and drain the
ClusterQueue before replacing references:

```bash
kubectl patch clusterqueue jobqueue --type=merge \
  -p '{"spec":{"stopPolicy":"HoldAndDrain"}}'
kubectl get workloads -A
kubectl wait --for=jsonpath='{.status.reservingWorkloads}'=0 \
  clusterqueue/jobqueue --timeout=10m
kubectl wait --for=jsonpath='{.status.admittedWorkloads}'=0 \
  clusterqueue/jobqueue --timeout=10m
```

Cancel pending workload owners, upgrade the Helm values, verify installation
readiness, and restore admission:

```bash
tau cluster validate installation
kubectl patch clusterqueue jobqueue --type=merge \
  -p '{"spec":{"stopPolicy":"None"}}'
```
