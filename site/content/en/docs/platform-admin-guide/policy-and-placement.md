---
title: Queue, quota, topology, and GPU placement
linkTitle: Manage queues and GPUs
weight: 40
description: How policy intent becomes admitted and scheduled pods
aliases:
  - "/docs/concepts/policy-and-placement/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

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
ResourceFlavors describe resource pools. Priority and preemption remain
cluster-owned policy.

## GPU class contract

`policy.gpu_class` is hardware-only and maps exactly to the node label
`tau.azure.com/gpu-class`:

| Researcher value | Meaning |
|---|---|
| `any` | No class selector; any compatible GPU ResourceFlavor may be admitted |
| `a10-4gb`, `a10-8gb`, `a10-12gb`, `a10-24gb` | NVIDIA A10, including Azure fractional GPU sizes |
| `a100-40gb` | NVIDIA A100 with 40 GB memory |
| `a100-80gb` | NVIDIA A100 with 80 GB memory |
| `h100-80gb` | NVIDIA H100 with 80 GB memory |
| `h100-95gb` | NVIDIA H100 with 95 GB memory |
| `h200-141gb` | NVIDIA H200 with 141 GB memory |
| `gb200-192gb` | NVIDIA GB200 with 192 GB memory |
| `gb300-288gb` | NVIDIA GB300 with 288 GB memory |

For a specific class, TauGrid renders the canonical label as both workload metadata
and a pod node selector. Queue preflight accepts a ResourceFlavor only when
`spec.nodeLabels["tau.azure.com/gpu-class"]` equals the requested class,
matching solely on that label: `ndm-a100-v4`, `nd-h200-v5`, and
`taugrid-default` are platform identifiers, distinct from researcher API
values.

Placement stays in `policy.topology`: `independent`, `single-node-nvlink`,
`multi-node-nccl`, or `elastic-workers`. GPU class values encode hardware
only; NVLink, InfiniBand, NCCL, and same-host placement are expressed
separately through `policy.topology`.

The legacy inputs `a100-nvlink-80gb`, `h100-standalone-95gb`, and
`h200-nvlink-141gb` are accepted for one compatibility window, normalized
before validation/rendering, and produce a CLI deprecation warning. New
configs and platform assets must use canonical values.

## Existing-cluster migration

Update downstream run configs/examples to canonical values first. Before
changing live queue or flavor objects, stop submitters, hold admission, and
drain the queue. `HoldAndDrain` evicts admitted/reserving workloads while
leaving already-pending workloads in place; inspect those workloads and
delete or deactivate their owning Job, RayJob, or workflow before waiting for
zero. Keep namespaces and PVCs in place throughout.

```bash
export CLUSTER_QUEUE=jobqueue
kubectl patch clusterqueue "$CLUSTER_QUEUE" --type=merge \
  -p '{"spec":{"stopPolicy":"HoldAndDrain"}}'
# Inspect pending owners and cancel them through their owning controller.
kubectl get workloads -A -o \
  custom-columns=NAMESPACE:.metadata.namespace,WORKLOAD:.metadata.name,OWNER_KIND:.metadata.ownerReferences[0].kind,OWNER:.metadata.ownerReferences[0].name,ADMITTED:.status.conditions[?(@.type==\"Admitted\")].status
kubectl wait --for=jsonpath='{.status.reservingWorkloads}'=0 \
  "clusterqueue/$CLUSTER_QUEUE" --timeout=10m
kubectl wait --for=jsonpath='{.status.admittedWorkloads}'=0 \
  "clusterqueue/$CLUSTER_QUEUE" --timeout=10m
kubectl wait --for=jsonpath='{.status.pendingWorkloads}'=0 \
  "clusterqueue/$CLUSTER_QUEUE" --timeout=10m
```

After the queue is empty, upgrade the Tau controller chart so its reviewed
VM-size catalog reconciles both labels. Add custom hardware through
`extraNodeLabelRules`, which defines those labels directly rather than
relying on labels a node might already carry:

CPU-only clusters and GPU pools scaled to zero remain ready when no catalog
entry currently matches.

```yaml
tau-core-controller:
  tauCluster:
    extraNodeLabelRules:
      - match:
          vmSizes: [Standard_Custom_H200_v5]
        labels:
          kueue.azure.com/gpu-series: custom-h200-v5
          tau.azure.com/gpu-class: h200-141gb
```

Wait for the singleton `TauCluster` to report `NodesReady`, then create
replacement ResourceFlavors with new names rather than patching referenced
objects: Kueue may protect or reject changes to immutable/in-use flavor fields.

Verify the exact contract before submitting specific-class work:

```bash
kubectl get resourceflavor -o \
  custom-columns=NAME:.metadata.name,GPU_CLASS:.spec.nodeLabels.tau\\.azure\\.com/gpu-class
kubectl get nodes -L tau.azure.com/gpu-class
tau cluster validate nodes --gpu-class a100-80gb --min-healthy 1
tau cluster validate nodes --gpu-class h200-141gb --min-healthy 1
```

Fresh installs use a generic CPU flavor, `taugrid-default-cpu`, with zero GPU quota
and a generic GPU flavor, `taugrid-default-gpu`, with CPU, memory, and GPU quota
in the same node-resource group. The GPU flavor remains valid for
`gpu_class: any` and must not be advertised as a specific hardware class.

Keep the sole ResourceFlavor in a resource group that covers CPU, memory,
and GPU free of any GPU class label. Kueue injects that flavor's node labels for every
admitted workload, which pins CPU-only work to GPU nodes. Use a zero-GPU CPU
flavor plus class-labeled GPU flavors in the same group. Upgrade older
single-flavor installs by draining admission and splitting the flavor:

1. Stop new submissions and set `stopPolicy: HoldAndDrain`. Kueue drains
   reserving/admitted workloads, but pending workloads remain pending. Inspect
   them with `kubectl get workloads -A`, then cancel or delete each pending
   workload's owning Job, RayJob, or workflow before waiting for all three
   counts to reach zero. Keep PVCs and namespaces in place.
2. Create a generic non-TAS CPU ResourceFlavor that carries no GPU labels or GPU
   admission taints. Create separate GPU ResourceFlavors with exact
   `tau.azure.com/gpu-class` labels, GPU `nodeTaints`, `topologyName`, and the
   managed resource annotation
   `kueue.x-k8s.io/podset-required-topology=<level>`. Connected TauGrid submission
   copies that platform requirement onto generated GPU pod templates when no
   explicit placement policy is present. CPU-only jobs remain admissible through
   the non-TAS CPU flavor and cannot consume GPU quota.
3. Replace `spec.resourceGroups` with one group covering CPU, memory, and GPU.
   Give the CPU flavor CPU/memory quota and zero GPU quota. Give each GPU flavor
   CPU, memory, and GPU quota. Kueue then assigns one node flavor across every
   resource requested by a pod set.
4. Restore admission with
   `kubectl patch clusterqueue "$CLUSTER_QUEUE" --type=merge -p
   '{"spec":{"stopPolicy":"None"}}'`, submit CPU and GPU smoke targets, and
   remove the old mixed flavor only after it is no longer referenced, keeping
   namespaces, PVCs, and workspace data in place throughout this migration.

For a one-GPU A100 cluster the resulting queue shape is:

```yaml
spec:
  resourceGroups:
    - coveredResources: [cpu, memory, nvidia.com/gpu]
      flavors:
        - name: taugrid-system
          resources:
            - {name: cpu, nominalQuota: "100000"}
            - {name: memory, nominalQuota: 100Ti}
            - {name: nvidia.com/gpu, nominalQuota: "0"}
        - name: taugrid-a100-80gb
          resources:
            - {name: cpu, nominalQuota: "100000"}
            - {name: memory, nominalQuota: 100Ti}
            - {name: nvidia.com/gpu, nominalQuota: "1"}
```

A successful preflight confirms eligibility alone; actual capacity is
reserved only at admission or scheduling, and can still change before then.
