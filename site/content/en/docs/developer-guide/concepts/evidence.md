---
title: Experiment evidence and artifacts
weight: 5
description: What TauGrid saves from a run, where it goes, and how to retrieve it
aliases:
  - "/docs/concepts/evidence/"
---

{{< maturity status="ga" reviewed="2026-08-25" >}}

Every run should save enough information for you to answer four questions:

- What code and settings produced this result?
- Did the model improve during the run?
- Where are the checkpoint and output files?
- Can I compare, reproduce, or resume this run later?

## What TauGrid saves

| Saved data | Examples | Where it is saved |
|---|---|---|
| Run details | Project, run ID, config, and input references | TauGrid experiment index |
| Metrics over time | Loss, accuracy, throughput, and GPU use | Metric files |
| Metric summary | First and latest step, minimum, maximum, and point count | TauGrid experiment index |
| Output files | Checkpoints, models, images, tables, profiles, and reports | The directory set by `storage.output` |
| Run history | Attempts, retries, resumes, and final status | TauGrid experiment index and Kubernetes workload |

The output files stay on the storage chosen by the platform team. TauGrid keeps
an index so you can find and compare them.

## How metric storage works

TauGrid saves the complete metric history close to the run. This keeps the data
available even when a dashboard or external data service is offline.

1. **The run writes complete metric files.** A file becomes visible only after
   the write finishes. Readers never receive a half-written file.
2. **TauGrid stores the points in a consistent format.** Each run uses the same
   fields for names, steps, times, and values. The saved files use Parquet,
   which works well for large metric histories.
3. **A retry does not create another copy.** TauGrid recognizes a repeated
   write and reuses the saved record. If the content changed, TauGrid reports
   the conflict instead of replacing the old data.
4. **TauGrid builds a short summary.** For each metric, it records the number
   of points, the step range, the minimum and maximum values, and the latest
   valid value. Invalid numbers such as `NaN` and infinity are counted
   separately.
5. **Dashboards can use an optional copy.** Platform teams can send metric
   points to ADX/Kusto for dashboards and searches across many workspaces. The
   files saved with the run remain the source of truth.

Each metric point includes:

| Field | What it tells you |
|---|---|
| `project`, `run_group_id`, `run_id` | Which experiment and run produced the point |
| `metric_name` | What was measured, such as `loss` or `accuracy` |
| `step`, `wall_time` | When the point occurred in training and on the clock |
| `value`, `unit` | The measured number and its unit, when one applies |
| `source`, `split`, `tags` | Where the point came from and useful labels such as dataset split or workspace |

You can review the same metric history in the TauGrid Portal. The saved run data
remains the source of truth.

## Save output files

Set `storage.output` to a directory on the workspace storage:

```yaml
storage:
  data_pvc: blob-training
  output: /data/projects/vision-lab/runs/train-042
```

The run can write directly to that directory. Use staged publishing when output
files should appear only after all writes finish:

```yaml
storage:
  data_pvc: blob-training
  output: /data/projects/vision-lab/runs/train-042
  publish: staged
```

With staged publishing, the run writes files to `TAU_OUTPUT_STAGING_DIR`.
TauGrid checks the files, copies them into the durable output directory, and
marks the set as complete. Commands only return a completed set.

## Retrieve output files

List the files saved by a run:

```bash
tau run get <run-name> -n <workspace-namespace>
```

Fetch one file:

```bash
tau run get <run-name> -n <workspace-namespace> \
  --artifact reports/evaluation.json -o raw
```

Set `storage.checkpoint` when a run produces a model that you plan to serve.
The value points to one file or directory inside the run's output. TauGrid
records its location so this command can find it later:

```bash
tau serve deploy --from-finetune <run-name>
```

## Try a live example

[Run the live experiment evidence example](../../../examples/experiment-evidence/)
to publish loss and accuracy, inspect the saved files with `tau run get`, and
view the metric history in the TauGrid Portal.

See [Observability](../../../platform-admin-guide/observability/) for platform
telemetry, [Prepare ADX/Kusto](../../../platform-admin-guide/prepare-adx-kusto/)
for optional dashboards across many workspaces, and
[Retry and resume](../../../platform-admin-guide/recovery/) for recovery state.
