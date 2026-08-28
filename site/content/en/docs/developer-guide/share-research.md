---
title: Share research with a teammate
weight: 2
description: View the same runs, queue state, logs, and charts from two machines
aliases:
  - "/docs/tasks/researcher/share-research/"
---

{{< maturity status="alpha" reviewed="2026-08-24" >}}

Researchers working from different machines can use the same cluster and
workspace to view and compare the same runs, logs, and charts.

## Access model used in this guide

This guide describes the current interim admin-access workflow. The cluster
owner gives each researcher their own cluster-admin kubeconfig. Each researcher
keeps those credentials on their own machine and uses a separate port-forward
to the shared Portal Service.

This workflow does not use workspace-directory browser authentication. That
mode requires an external sign-in proxy that is not part of the current setup.

## Before you start

Both researchers need:

- their own cluster-admin kubeconfig for the same TauGrid cluster;
- access to the same workspace;
- the same research project; and
- the kubeconfig path, context, Portal namespace, and Portal Service name
  supplied by the cluster owner.

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
tau logs <run-name>
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

Each researcher starts a separate port-forward on their own machine. This opens
the current admin Portal view; it does not sign researchers in separately. Use
the kubeconfig, context, namespace, and Service name supplied by the cluster
owner.

On Researcher 1's machine:

```bash
kubectl --kubeconfig <cluster-kubeconfig> \
  --context <cluster-context> \
  --namespace <portal-namespace> \
  port-forward service/<portal-service> 8080:80
```

On Researcher 2's machine, run the same command with that machine's own
cluster credentials:

```bash
kubectl --kubeconfig <cluster-kubeconfig> \
  --context <cluster-context> \
  --namespace <portal-namespace> \
  port-forward service/<portal-service> 8080:80
```

Keep each port-forward terminal open. Each researcher opens this address in a
browser on their own machine:

```text
http://127.0.0.1:8080/portal
```

The addresses look the same, but each one uses the port-forward running on that
researcher's machine. Both port-forwards connect to the same Portal Service in
the cluster.

Researcher 1 opens a run and sends its full Stellar link to Researcher 2. A
shared link has this form:

```text
http://127.0.0.1:8080/stellar?target=<run-name>&project=<project>&workspace=<workspace>
```

Researcher 2 opens the link while their own port-forward is running. The Portal
uses the data source configured for the workspace.

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
tau logs tune-smoke
```

The names `ray-tune-demo` and `shared-research` are sample names. A team can
replace them with its own project and workspace names.

Researcher 1 opens `tune-smoke` through their port-forward and sends the full
browser link to Researcher 2. Researcher 2 opens it through their own
port-forward. Both researchers should see the same run name and state. Use the
next example to publish metrics and compare charts in Stellar.

## Example: compare several runs

This example uses the published
[`examples/portal-ray-stellar`](https://github.com/Azure/taugrid/tree/main/examples/portal-ray-stellar)
files. It puts three runs in one experiment so both researchers can compare
them on one page.

Before starting, ask the cluster owner to confirm that:

- the workspace is Ready and has a writable `blob-training` PVC;
- the workspace has GPU quota and allocatable GPU capacity;
- the Portal has a Kusto query source for the workspace; and
- you have the platform-supplied `taugrid-portal` image pinned by digest.

Each run uses one GPU. One available GPU can run them one at a time; three
available GPUs can run all three at the same time.

Set the image and offloader working directory in the terminal that starts the
runs:

```bash
export TAU_METRICS_OFFLOAD_IMAGE=<platform-supplied-taugrid-portal@sha256:digest>
export TAU_METRICS_OFFLOAD_OUT=/var/run/tau/metrics-offload
```

Copy the example so `train.py` stays beside the Tau config files:

```bash
cp -R examples/portal-ray-stellar ray-metric-study
cp ray-metric-study/tau.yaml ray-metric-study/baseline.yaml
cp ray-metric-study/tau.yaml ray-metric-study/short-run.yaml
cp ray-metric-study/tau.yaml ray-metric-study/long-run.yaml
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
| `ray-metric-study/baseline.yaml` | `ray-baseline` | `baseline` | `/data/projects/shared-research/runs/ray-metric-study/baseline` | `20` |
| `ray-metric-study/short-run.yaml` | `ray-short-run` | `short-run` | `/data/projects/shared-research/runs/ray-metric-study/short-run` | `10` |
| `ray-metric-study/long-run.yaml` | `ray-long-run` | `long-run` | `/data/projects/shared-research/runs/ray-metric-study/long-run` | `40` |

For example, the changing part of `ray-metric-study/baseline.yaml` is:

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
tau run --config ray-metric-study/baseline.yaml
tau run --config ray-metric-study/short-run.yaml
tau run --config ray-metric-study/long-run.yaml
```

Researcher 1 sends Researcher 2 the shared experiment name:

```text
ray-metric-study
```

Researcher 1 opens the experiment through their port-forward and sends its full
link to Researcher 2:

```text
http://127.0.0.1:8080/stellar?target=ray-metric-study&project=ray-tune-demo&workspace=shared-research
```

Researcher 2 opens the link while their own port-forward is running. The
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
4. Each researcher's port-forward is still running.
5. Both browser pages were refreshed.

If a chart is still missing, send the run name, project, workspace, and missing
metric name to the cluster owner.
