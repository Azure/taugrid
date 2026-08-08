---
title: Run your first target
weight: 1
description: Validate, submit, observe, and retrieve one repository target
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Prerequisites:

- Tau is installed.
- The repository contains a workspace connection and a checked-in
  [target](../../concepts/glossary/#target) (`tau/train.yaml`, `tau/smoke.yaml`,
  and so on).
- The platform workspace reports Ready.

`tau run` is the config-first entry point. `smoke` and `train` below are not
subcommands — they are an optional positional `TARGET` argument that `tau run`
resolves to `tau/<target>.yaml`. `smoke` is a built-in name that always runs
the onboarding smoke test; any other value must match a checked-in target
file. See the [direct run config reference](../../reference/run-config/) for
the full `tau.yaml` field set. `tau run` automatically discovers the checked-in
workspace connection; `tau workspace connection inspect` is only an optional
offline diagnostic.

Validate before you spend a full submission, then submit:

```bash
cd <research-repository>
tau run smoke
tau run train --dry-run=client
tau run train
```

`--dry-run=client` does not submit the workload, so it is the safe way to catch
a config mistake before it consumes queue capacity. It still performs workspace
connection discovery and readiness verification so the rendered namespace,
LocalQueue, and service account come from current pinned platform policy. Tau
prints the submitted [run](../../concepts/glossary/#run) name; record it for the
rest of this walkthrough.

```bash
tau run status <run-name> --watch
tau run logs <run-name>
tau run get <run-name>
```

`status --watch` renders the startup phase tree (Submitted, Kueue admission,
scheduling, image pull, readiness) until the
[workload](../../concepts/glossary/#workload) is ready, failed, or you
interrupt it. For RayJobs, `logs` streams the Ray
driver's execution output rather than head-pod logs; for batch Jobs it streams
the Job pod logs. `get` fetches durable run results and artifacts once the run
has produced them.

Named `status` and `logs` also work for a batch Job submitted outside Tau. Such
Jobs are hidden from the default Tau-owned list; opt in when investigating a
shared namespace:

```bash
tau run list --include-external
tau run logs <job-name> -c <container> --previous --timestamps
tau run status <job-name> --diagnostic-hints
```

Batch logs also support `--all-containers` and `--prefix`. The diagnostic hints
print correctly scoped `kubectl top`, detailed log, and exec commands; Tau does
not proxy metrics or interactive exec and does not request broader RBAC. For an
external or already deleted Job with no Tau result annotations, retrieve a known
PVC path explicitly:

```bash
tau run get <job-name> --path /data/<job-name>/results --pvc <pvc-name>
```

If you need to stop a run before it finishes — for example, you spot a bad
hyperparameter mid-training — cancel it instead of leaving it to fail on its
own:

```bash
tau run cancel <run-name>
```

`cancel` deletes the underlying RayJob or Job and lets Kueue reclaim the
workload's quota. If a run instead fails on its own, do not resubmit blindly —
see [recovery](../../operations/recovery/) for automatic retry and
`tau run resume` semantics before you retry by hand.

## Next: look at your evidence

A completed (or still-running) run's metrics and artifacts are
[evidence](../../concepts/evidence/). Once you have a run name, discover and
open it in Stellar using the `taugrid-portal` binary:

```bash
taugrid-portal experiment search
taugrid-portal experiment stellar <run-name>
```

`taugrid-portal experiment search` (alias `runs`) lists indexed runs so you can
find the one you just submitted if you did not keep the name.
`taugrid-portal experiment stellar` renders the local Stellar dashboard for that
run as static HTML by default; add `-o tui` for a terminal summary, or use
`taugrid-portal experiment open <run-name>` to serve it and open your browser in
one step.

Do not add namespace, queue, kubeconfig, or cloud credentials to project config
to work around platform readiness failures. Those remain workspace concerns.

When the run has produced a durable checkpoint and your project has a serving
image, continue with [Serve a trained model](../serve-model/).
