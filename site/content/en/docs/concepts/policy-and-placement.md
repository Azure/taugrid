---
title: Queue, quota, topology, and GPU placement
weight: 4
description: How policy intent becomes admitted and scheduled pods
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau resolves workload policy; upstream systems enforce it.

| Stage | Owner | Decision |
|---|---|---|
| Target resolution | Tau | Requested workers, GPUs, placement, priority, and workspace defaults |
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
| `a100-80gb` | NVIDIA A100 with 80 GB memory |
| `h100-95gb` | NVIDIA H100 with 95 GB memory |
| `h200-141gb` | NVIDIA H200 with 141 GB memory |

For a specific class, Tau renders the canonical label as both workload metadata
and a pod node selector. Queue preflight accepts a ResourceFlavor only when
`spec.nodeLabels["tau.azure.com/gpu-class"]` equals the requested class. It does
not parse ResourceFlavor names: `ndm-a100-v4`, `nd-h200-v5`, and
`taugrid-default` are platform identifiers, not researcher API values.

Placement stays in `policy.topology`: `independent`, `single-node-nvlink`,
`multi-node-nccl`, or `elastic-workers`. GPU class values do not encode NVLink,
InfiniBand, NCCL, or same-host placement.

The legacy inputs `a100-nvlink-80gb`, `h100-standalone-95gb`, and
`h200-nvlink-141gb` are accepted for one compatibility window, normalized
before validation/rendering, and produce a CLI deprecation warning. New
configs and platform assets must use canonical values.

## Existing-cluster migration

Update downstream run configs/examples to canonical values, then label both
the GPU nodes and their ResourceFlavors. For the standard Azure flavor names:

```bash
kubectl label nodes -l kueue.azure.com/gpu-series=ndm-a100-v4 \
  tau.azure.com/gpu-class=a100-80gb --overwrite
kubectl label nodes -l kueue.azure.com/gpu-series=nd-h200-v5 \
  tau.azure.com/gpu-class=h200-141gb --overwrite

kubectl patch resourceflavor ndm-a100-v4 --type=merge \
  -p '{"spec":{"nodeLabels":{"tau.azure.com/gpu-class":"a100-80gb"}}}'
kubectl patch resourceflavor nd-h200-v5 --type=merge \
  -p '{"spec":{"nodeLabels":{"tau.azure.com/gpu-class":"h200-141gb"}}}'
```

Use the same pattern for H100 (`h100-95gb`). If the cluster's ResourceFlavor
webhook rejects in-place specification changes, create a replacement flavor
with the canonical node label, update the ClusterQueue to reference it, and
remove the old flavor only after no admitted workloads use it.

Verify the exact contract before submitting specific-class work:

```bash
kubectl get resourceflavor -o \
  custom-columns=NAME:.metadata.name,GPU_CLASS:.spec.nodeLabels.tau\\.azure\\.com/gpu-class
kubectl get nodes -L tau.azure.com/gpu-class
tau cluster validate nodes --gpu-class a100-80gb --min-healthy 1
tau cluster validate nodes --gpu-class h200-141gb --min-healthy 1
```

Fresh installs intentionally leave `taugrid-default` without a GPU class label.
It remains valid for `gpu_class: any` and must not be advertised as any specific
hardware class.

A successful preflight is not a capacity reservation. Capacity can change before
admission or scheduling.
