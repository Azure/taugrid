---
title: Share research with a teammate
weight: 2
description: View the same runs, queue state, logs, and charts from two machines
---

{{< maturity status="alpha" reviewed="2026-08-24" >}}

TauGrid clusters make it easy to share research results with your team after
the platform owner configures workspace access and an authenticated Portal.
Researchers working from different machines can use the same cluster and
workspace to view and compare the same runs, logs, and charts.

## Before you start

Both researchers need:

- access to the same TauGrid cluster;
- access to the same workspace;
- the same research project; and
- access to the authenticated Portal address supplied by the cluster owner.

Do not send kubeconfig files, tokens, passwords, or secrets to each other.

## Share a run

Researcher 1 starts a run:

```bash
tau run train
```

Tau prints the run name. Researcher 1 sends that run name to Researcher 2.

Both researchers can now check the run:

```bash
tau run status <run-name>
tau run logs <run-name>
```

Use `--watch` to keep the status open while the run starts:

```bash
tau run status <run-name> --watch
```

Both researchers should see the same queue state, start time, Pods, and final
result.

## See shared and waiting runs

List Tau runs in the workspace:

```bash
tau run list
```

If the team also uses Jobs created outside Tau, include them:

```bash
tau run list --include-external
```

Open one waiting run with:

```bash
tau run status <run-name> --diagnostic-hints
```

Both researchers should see the same reason when a run is waiting for space in
the queue.

## Share a Portal view

The cluster owner must provide a platform-managed HTTPS Portal address with
sign-in enabled. Direct `kubectl port-forward` access is an operator diagnostic,
not a researcher access path.

Each researcher opens the Portal address from their own machine and signs in
with an account that has access to the workspace:

```text
https://<portal-address>/portal
```

Researcher 1 opens a run and sends its full browser link to Researcher 2. A
Stellar link has this form:

```text
https://<portal-address>/stellar?target=<run-name>&project=<project>&workspace=<workspace>
```

The Portal selects the data source configured for that workspace. Researchers
do not need to add a `source` setting to the link.

Both researchers should check:

- the run name;
- the project name;
- the run state;
- the metric names;
- the number of chart points; and
- the run group.

If these match, both researchers are reading the same shared result.

## Example: share the published Ray Tune run

This example uses the published
[GPU Ray Tune HPO walkthrough](../../examples/gpu-ray-tune/) and its
[`examples/ray-tune-smoke`](https://github.com/Azure/taugrid/tree/main/examples/ray-tune-smoke)
files.

Researcher 1 follows the walkthrough and starts the run:

```bash
tau run --config tau.yaml --dry-run=client
tau run --config tau.yaml
```

The checked-in example names the run `tune-smoke`. Researcher 1 sends these
sample values to Researcher 2:

```text
Run: tune-smoke
Project: ray-tune-demo
Workspace: shared-research
```

Researcher 2 checks the same run from their own machine:

```bash
tau run status tune-smoke
tau run logs tune-smoke
```

The names `ray-tune-demo` and `shared-research` are sample names. A team can
replace them with its own project and workspace names.

Researcher 1 opens `tune-smoke` in the authenticated Portal and sends the full
browser link to Researcher 2. Both researchers should see the same run name and
state. Use the next example to publish metrics and compare charts in Stellar.

## Example: compare several runs

This example uses the published
[`examples/portal-ray-stellar`](https://github.com/Azure/taugrid/tree/main/examples/portal-ray-stellar)
files. It puts three runs in one experiment so both researchers can compare
them on one page.

Before starting, ask the cluster owner to confirm that:

- the workspace is Ready and has a writable `blob-training` PVC;
- the Portal has a Kusto query source for the workspace; and
- you have the platform-supplied `taugrid-portal` image pinned by digest.

Set the image and offloader working directory in the terminal that starts the
runs:

```bash
export TAU_METRICS_OFFLOAD_IMAGE=<platform-supplied-taugrid-portal@sha256:digest>
export TAU_METRICS_OFFLOAD_OUT=/var/run/tau/metrics-offload
```

Create three Tau config files from the example:

```text
tau/baseline.yaml
tau/short-run.yaml
tau/long-run.yaml
```

Keep the same project and experiment name in all three files:

```yaml
experiment:
  project: ray-tune-demo
  name: ray-metric-study
```

Give each file its own run name, group, output folder, and step count:

| Config | Run name | Group | Output folder | Steps |
|---|---|---|---|---|
| `tau/baseline.yaml` | `ray-baseline` | `baseline` | `/data/projects/shared-research/runs/ray-metric-study/baseline` | `20` |
| `tau/short-run.yaml` | `ray-short-run` | `short-run` | `/data/projects/shared-research/runs/ray-metric-study/short-run` | `10` |
| `tau/long-run.yaml` | `ray-long-run` | `long-run` | `/data/projects/shared-research/runs/ray-metric-study/long-run` | `40` |

For example, the changing part of `tau/baseline.yaml` is:

```yaml
name: ray-baseline

runtime:
  env:
    PORTAL_DEMO_STEPS: "20"

storage:
  data_pvc: blob-training
  output: /data/projects/shared-research/runs/ray-metric-study/baseline

policy:
  workspace: shared-research
  namespace: shared-research

experiment:
  project: ray-tune-demo
  name: ray-metric-study
  group: baseline
```

Use the matching values from the table in the other two files. Keep the
`metrics` settings from the published example in every file. If the workspace
uses a different Kubernetes namespace or PVC name, use the values supplied by
the cluster owner.

Researcher 1 starts the three runs:

```bash
tau run --config tau/baseline.yaml
tau run --config tau/short-run.yaml
tau run --config tau/long-run.yaml
```

Researcher 1 sends Researcher 2 the shared experiment name:

```text
ray-metric-study
```

Researcher 1 opens the experiment in the authenticated Portal and sends its full
link to Researcher 2. The link has this form:

```text
https://<portal-address>/stellar?target=ray-metric-study&project=ray-tune-demo&workspace=shared-research
```

Each researcher opens the link from their own machine and signs in. The
experiment page should show all three runs together. Both researchers can
compare the `loss` and `accuracy` charts for `baseline`, `short-run`, and
`long-run`.

## What is shared

Researchers in the same workspace can see:

- run names and state;
- queue and waiting state;
- Job and Pod state;
- logs they are allowed to read; and
- metrics sent to the shared metrics store.

## If the views do not match

Check these items in order:

1. Both researchers are using the same cluster.
2. Both researchers are using the same workspace.
3. Both researchers opened the same run name and project.
4. Both researchers signed in to the authenticated Portal.
5. Both browser pages were refreshed.

If a chart is still missing, send the run name, project, workspace, and missing
metric name to the cluster owner.
