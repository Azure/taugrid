---
title: Share research with a teammate
weight: 2
description: View the same runs, queue state, logs, and charts from two machines
---

{{< maturity status="alpha" reviewed="2026-08-24" >}}

TauGrid clusters make it easy to share research results with your team.
Researchers working from different machines can use the same cluster and
workspace to view and compare the same runs, logs, and charts.

## Before you start

Both researchers need:

- access to the same TauGrid cluster;
- access to the same workspace;
- the same research project; and
- the Portal address or connection steps from the cluster owner.

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

Each researcher needs a separate port-forward on their own machine.

On Researcher 1's machine, Researcher 1 opens a terminal and runs:

```bash
kubectl -n <portal-namespace> \
  port-forward service/<portal-service> 8080:80
```

On Researcher 2's machine, Researcher 2 opens a different terminal and runs
the same command:

```bash
kubectl -n <portal-namespace> \
  port-forward service/<portal-service> 8080:80
```

The cluster owner supplies the Portal namespace and Service name. In our test
cluster, the command was:

```bash
kubectl -n tau-portal-sdk \
  port-forward service/taugrid-portal-sdk 8080:80
```

Each researcher keeps their port-forward terminal open. A working connection
prints:

```text
Forwarding from 127.0.0.1:8080 -> 8080
```

Researcher 1 opens this address in a browser on Researcher 1's machine:

```text
http://127.0.0.1:8080/portal
```

Researcher 2 opens the same address in a browser on Researcher 2's machine:

```text
http://127.0.0.1:8080/portal
```

`127.0.0.1` means the machine where the browser is running. The two addresses
look the same, but they use two different port-forwards:

```text
Researcher 1 browser -> Researcher 1 port-forward -> shared Portal
Researcher 2 browser -> Researcher 2 port-forward -> shared Portal
```

One researcher does not connect through the other researcher's machine. Both
port-forwards connect to the same Portal Service in the cluster.

To share one run, send its full Stellar link. The link has this form:

```text
http://127.0.0.1:8080/stellar?target=<run-name>&project=<project>&workspace=<workspace>&source=kusto
```

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

After both researchers start their own port-forward, each opens this link in a
browser on their own machine:

```text
http://127.0.0.1:8080/stellar?target=tune-smoke&project=ray-tune-demo&workspace=shared-research&source=kusto
```

The names `ray-tune-demo` and `shared-research` are sample names. A team can
replace them with its own project and workspace names.

Both researchers should see the same `tune-smoke` run, the same six trial
settings, and the same `loss` metric.

## Example: compare several runs

This example uses the published
[`examples/portal-ray-stellar`](https://github.com/Azure/taugrid/tree/main/examples/portal-ray-stellar)
files. It puts three runs in one experiment so both researchers can compare
them on one page.

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
| `tau/baseline.yaml` | `ray-baseline` | `baseline` | `/data/ray-metric-study/baseline` | `20` |
| `tau/short-run.yaml` | `ray-short-run` | `short-run` | `/data/ray-metric-study/short-run` | `10` |
| `tau/long-run.yaml` | `ray-long-run` | `long-run` | `/data/ray-metric-study/long-run` | `40` |

For example, the changing part of `tau/baseline.yaml` is:

```yaml
name: ray-baseline

runtime:
  env:
    PORTAL_DEMO_STEPS: "20"

storage:
  output: /data/ray-metric-study/baseline

experiment:
  project: ray-tune-demo
  name: ray-metric-study
  group: baseline
```

Use the matching values from the table in the other two files. Keep the
`metrics` settings from the published example in every file.

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

After both researchers start their own port-forward, each opens this link in a
browser on their own machine:

```text
http://127.0.0.1:8080/stellar?target=ray-metric-study&project=ray-tune-demo&workspace=shared-research&source=kusto
```

The experiment page should show all three runs together. Both researchers can
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
4. Each researcher's own port-forward terminal is still running.
5. Both browser pages were refreshed.

If a chart is still missing, send the run name, project, workspace, and missing
metric name to the cluster owner.
