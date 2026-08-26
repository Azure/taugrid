---
title: Live experiment evidence
weight: 2
description: Publish scalar history, inspect durable artifacts, and open the same run in Stellar
---

{{< maturity status="alpha" reviewed="2026-08-25" >}}

This example runs the checked-in
[`examples/portal-ray-stellar`](https://github.com/Azure/taugrid/tree/main/examples/portal-ray-stellar)
RayJob. It publishes immutable loss and accuracy chunks to durable storage
while a metrics-offload sidecar projects the same scalar history to ADX/Kusto.

The run demonstrates three independent surfaces:

- `tau run status` and logs for immediate execution.
- `tau run get` for durable files on the workspace PVC.
- Stellar for experiment comparison and scalar visualization.

## Prerequisites

Prepare:

- A Ready TauWorkspace named `taugrid-default`.
- A writable `blob-training` PVC mounted at `/data`.
- One schedulable NVIDIA GPU.
- A digest-pinned `taugrid-portal` image supplied by the platform team.
- Portal and the optional metrics ingestion path configured for the workspace.

The sidecar runs `taugrid-portal experiment offload metrics`, so use the Portal
image rather than the Tau CLI image.

## Inspect the evidence contract

The checked-in run config declares:

```yaml
storage:
  data_pvc: blob-training
  output: /data/projects/taugrid-default/runs/portal-ray-stellar

metrics:
  history:
    - metrics-history-attempt-0/*.jsonl
  offload:
    enabled: true

experiment:
  project: portal-demo
  name: ray-plus-stellar
  group: default
```

The trainer writes one closed JSONL chunk per step. A temporary dotfile is
flushed and atomically renamed, so readers only see complete rows:

```json
{"_step":0,"_timestamp":1760000000.125,"loss":1.0,"accuracy":0.0}
{"_step":1,"_timestamp":1760000001.125,"loss":0.5,"accuracy":0.5}
{"_step":2,"_timestamp":1760000002.125,"loss":0.3333333333,"accuracy":0.6666666667}
```

`_timestamp` is a positive Unix-seconds number. The offloader uses `_step` and
the timestamp to normalize each scalar into the metrics store.

## Render and submit

From the repository root:

```bash
make install-tau-cli
export TAU_METRICS_OFFLOAD_IMAGE=<taugrid-portal-image@sha256:digest>
export TAU_METRICS_OFFLOAD_OUT=/var/run/tau/metrics-offload

tau run --workspace taugrid-default \
  --config examples/portal-ray-stellar/tau.yaml \
  --dry-run=client
```

Confirm that the rendered RayJob includes:

- `tau.azure.com/stellar-experiment-id: ray-plus-stellar`
- `tau.azure.com/stellar-group-value: default`
- The `blob-training` PVC
- A `metrics-offload` sidecar watching the immutable history glob

Submit and follow the run:

```bash
tau run --workspace taugrid-default \
  --config examples/portal-ray-stellar/tau.yaml
tau run status portal-ray-stellar --watch
tau run logs portal-ray-stellar
```

## Inspect durable evidence

List the directory recorded by `storage.output`:

```bash
tau run get portal-ray-stellar -n taugrid-default
```

The listing includes files shaped like:

```text
metrics-history-attempt-0/
  chunk-000000-<timestamp>.jsonl
  chunk-000001-<timestamp>.jsonl
  chunk-000002-<timestamp>.jsonl
```

Copy one listed name and fetch its exact contents:

```bash
tau run get portal-ray-stellar -n taugrid-default \
  --artifact metrics-history-attempt-0/<chunk-name>.jsonl \
  -o raw
```

The durable file remains available through the workspace storage lifecycle.
The offloaded scalar rows become available in Stellar after the terminal
`tau/run_status` marker is ingested.

## Open the live views

Open the Portal run page:

```text
/portal/runs/taugrid-default/<run-name>
```

While the Ray head is Ready, **Ray dashboard** opens runtime state. After the
metrics marker arrives, **Open in Experiments** opens the durable scalar
history. The two links answer different questions and share the same run
identity.

## What this example proves

| Check | Evidence |
|---|---|
| GPU workload ran | `tau run status`, logs, Ray dashboard |
| Loss improved | Immutable JSONL chunks and Stellar series |
| Durable files exist | `tau run get` listing and file retrieval |
| Fleet projection completed | Terminal metrics marker and Stellar link |
| Run identity stayed aligned | Project, experiment, group, and run annotations |

Read [Experiment evidence and artifacts](../../developer-guide/concepts/evidence/)
for the store methodology, normalized schema, summaries, artifact publication,
and ownership model.
