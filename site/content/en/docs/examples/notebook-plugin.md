---
title: Run a notebook from JupyterLab
linkTitle: Notebook plugin
weight: 15
description: Submit the notebook you are working in, then watch its status, logs and loss curve until it finishes.
---

{{< maturity status="alpha" reviewed="2026-09-23" >}}

Use this example when you want to stay in JupyterLab: submit the notebook you
are already working in, then watch the run through to completion without
opening a terminal or writing a manifest.

The plugin is a JupyterLab 4 prebuilt extension bundled in the `tau` Python
wheel. Notebook 7 loads it too. It talks to your cluster through a companion
Jupyter Server extension, so the notebook and the browser never need cluster
credentials.

## What you get

| Surface | What it shows |
|---|---|
| Left sidebar | A bounded list of Tau-managed Jobs and RayJobs in a namespace, a queue filter, and an exact lookup by kind, namespace and name |
| Run tab | Lifecycle phases, admission, pods, execution, diagnostics, the run identifiers, and the loss curve |
| Logs tab | A bounded snapshot for one pod and container |
| Notebook toolbar | Review this notebook as a job, then confirm the submission |
| About | Whether submission is enabled on this server, and why |

## Prerequisites

- a cluster with Kueue, KubeRay, and the Tau controllers installed;
- a JupyterLab installation that can see the `tau` wheel, installed with the
  `widgets` extra;
- a notebook runtime image that can execute notebooks. The platform AI runtime
  image ships Ray but no notebook executor, so build one and point the server at
  it with `TAUGRID_RUNTIME_IMAGE` (see
  `images/notebook-runtime/Dockerfile`);
- a CPU or GPU workload profile whose queue the namespace can use, plus a
  `LocalQueue` in the run namespace and the namespace label the ClusterQueue
  selects on; and
- `TAUGRID_SUBMISSION_ENABLED=1` on the Jupyter server, because submission is
  gated until an operator certifies a runtime image.

## Install and start

```bash
pip install "tau[widgets]"
jupyter labextension list          # taugrid-jupyterlab should be enabled

TAUGRID_SUBMISSION_ENABLED=1 \
TAUGRID_RUNTIME_IMAGE=taugrid-notebook-runtime:local \
TAUGRID_PORTAL_URL=https://your-portal \
  jupyter lab
```

## Open the runs browser

Run **TauGrid: Open runs** from the command palette, or use the TauGrid icon on
the launcher. The browser docks in the left sidebar.

![TauGrid runs sidebar](/images/notebook/notebook-runs-sidebar.png)

The namespace is a filtered picker rather than a text box: it lists the
namespaces this server can read, marks namespaces with the Tau workspace label, and selects
a labelled namespace on open. The label is a discovery hint, not proof of queue admission readiness. A namespace without that label is still
selectable, but the panel says the run may stay unadmitted. Click a run name to
open its detail, or use the exact lookup when you already know the resource
name. The list refreshes itself when the namespace or queue changes; Refresh is
there when you want it sooner.

## Submit the notebook in front of you

Open the notebook you want to run and click **Submit notebook** on the
notebook toolbar. The review tab resolves the destination from the cluster, so
you can see exactly what will be created before anything is written.

![TauGrid submit review](/images/notebook/notebook-submit-review.png)

Changing the namespace, the run name, the profile or the queue invalidates the
review, so what you confirm is what you reviewed. The server also compares a digest of the complete rendered plan and refuses changed runtime or cluster defaults before writing. Review again after such a refusal.

![TauGrid submit confirmation](/images/notebook/notebook-submit-confirm.png)

## Watch it to completion

Open the run from the sidebar. The detail tab follows the run while it is
active, then stops when it reaches a terminal state.

![TauGrid run detail with the loss curve](/images/notebook/notebook-run-detail-loss.png)

The loss curve plots the run's own `step=N loss=V` observations, read from the
run's log stream by the Jupyter server. It is a bounded window, not the complete
history, and the chart says so: a partial window, a stale reading, or a single
observation is labelled rather than smoothed over.

Open the logs tab when you need the raw output for one pod.

![TauGrid logs tab](/images/notebook/notebook-logs.png)

## Print a loss curve from your own notebook

The panel reads the same convention the command line uses. Print one record per
step, flush it, and the curve appears:

```python
for step in range(steps):
    loss = train_step()
    print(f"step={step} loss={loss:.12g}", flush=True)
```

A valid log window without any finite `step`/`loss` records reports no observed
loss samples and draws no curve. It does not imply zero loss or run failure.

## Compatibility and verification

The native plugin uses the Jupyter Server identity and release gate. The legacy
`tau.widgets` panel/magic/embed API remains available for explicit Python callers,
uses kernel identity, and is not this native interface. Its opt-in loopback proxy
removes upstream frame restrictions and is for trusted local use only.

The existing screenshots illustrate the native plugin's surfaces and remain
unchanged. Offline tests cover request, state, rendering, and evidence contracts;
they do not establish activation in JupyterLab or Notebook 7, cluster admission,
image availability, or notebook execution. Those require a live host and cluster
verification. This documentation update does not perform that verification or
recapture screenshots.

## Related

- `examples/notebook-ray-cpu-demo.ipynb` runs Ray tasks on a CPU profile.
- `examples/notebook-loss-curve-demo.ipynb` prints a deterministic loss curve.
- The design, API surface, metrics contract and evidence rules are in
  `docs/design/notebook-plugin.md`.
