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

## Priority and preemption

TauGrid installs two workload and pod priority pairs: `taugrid-default` at
1000 and `taugrid-priority` at 1200. Runs select them with
`policy.priority_tier: default|priority`; consumers do not need to create
their own classes. An explicit run tier overrides only the priority pair from
the selected workload profile.

The portable ClusterQueue uses Kueue `BestEffortFIFO`: higher resolved
Workload priority is considered before FIFO order, and equal-priority work is
ordered oldest first. `withinClusterQueue: LowerPriority` allows admitted
lower-priority work to be reclaimed for eligible higher-priority work in the
same ClusterQueue. Both pod classes use `PreemptLowerPriority`, which is a
separate Kubernetes scheduler decision after Kueue admission.

Portal queue views show the resolved Kueue priority class/value and the pod
priority class separately. “Pending admission,” “quota admitted,” and
“running” remain distinct states: quota, ResourceFlavor eligibility,
admission checks, and node availability can still prevent the highest
priority row from running immediately.

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

Placement stays in `policy.topology`: `unconstrained`, `same-host`,
`same-accelerator-domain`, `same-network-domain`, or `same-site`. GPU class
values encode hardware only; NVLink, InfiniBand, NCCL, and same-host placement
are expressed separately through `policy.topology`.

`same-accelerator-domain` requires one `tau.azure.com/accelerator-domain`
subtree. TauGrid assigns deterministic singleton domains unless the provider
publishes an authoritative `net.unbounded-cloud.io/accelerator-domain` value,
so matching GPU models alone never imply that Nodes share an NVL72 island.
Provider-declared domains may span hosts. The placement does not add
anti-affinity or guarantee distinct hosts, and insufficient capacity remains
pending rather than falling back to a network domain or site.

`same-network-domain` requires one shared fabric but does not generally require
one worker per host. Generic smaller workers may co-locate. Direct Job torchrun
workloads with `execution.nodes > 1` request hostname slices of size one from
Kueue TAS and retain required hostname anti-affinity, so admission and
scheduling place their rank pods on distinct Kubernetes hosts within the
selected fabric domain.

Tau emits a warning when this placement is selected. The request does not
independently require `tau.azure.com/infiniband=true`: Nodes without
authoritative shared-fabric metadata receive singleton network domains. A
multi-host workload remains pending when no one domain has sufficient eligible
capacity. Generic workers that all fit on one Node may still run in that Node's
singleton domain; multi-node direct Job torchrun remains pending until distinct
hosts are available. TauGrid never falls back automatically to `same-site` or
`unconstrained`.

## Current cluster contract

TauGrid creates one controller-owned Topology named `taugrid-gpu-topology`
with the levels `tau.azure.com/site`, `tau.azure.com/network-domain`,
`tau.azure.com/accelerator-domain`, and `kubernetes.io/hostname`.

The baseline queue contains the non-TAS CPU flavor `taugrid-default-cpu` with
zero GPU quota. `tau-core-controller` discovers GPU Nodes and creates one
topology-aware ResourceFlavor per distinct `tau.azure.com/gpu-class`. Initial
GPU quota is the summed allocatable `nvidia.com/gpu` capacity for that class;
the controller increases quota when more capacity appears and does not
automatically decrease or prune discovered flavors. Each discovered flavor
declares `sku=gpu:NoSchedule` as an admission taint, keeping CPU-only workloads
out of GPU quota. Node creation and later allocatable GPU updates both trigger
reconciliation, so newly joining pools receive quota as soon as kubelet reports
their devices.

Add custom hardware through `extraNodeLabelRules`:

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

Verify the active contract with:

```bash
kubectl get topology taugrid-gpu-topology -o yaml
kubectl get resourceflavor -o \
  custom-columns=NAME:.metadata.name,GPU_CLASS:.spec.nodeLabels.tau\\.azure\\.com/gpu-class,TOPOLOGY:.spec.topologyName
kubectl get nodes -L tau.azure.com/gpu-class,tau.azure.com/network-domain
tau cluster validate nodes --gpu-class h200-141gb --min-healthy 1
```

For a one-GPU A100 cluster the queue shape is:

```yaml
spec:
  resourceGroups:
    - coveredResources: [cpu, memory, nvidia.com/gpu]
      flavors:
        - name: taugrid-default-cpu
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
