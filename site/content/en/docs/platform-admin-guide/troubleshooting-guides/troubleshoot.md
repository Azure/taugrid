---
title: Troubleshoot a run
linkTitle: Troubleshoot a run
weight: 10
description: Locate the first failed lifecycle transition
url: "/docs/platform-admin-guide/troubleshoot/"
aliases:
  - "/docs/tasks/operator/troubleshoot/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Start with:

```bash
tau run status <run-name>
```

`tau run status` is the canonical lifecycle view for any run -- read its
phases top to bottom and stop at the first one that is not `done`. Then work
through the layers below, in order, until you reach that phase:

1. Repository/connection resolution and cluster access --
   `tau workspace connection`.
2. [TauWorkspace](../../reference/glossary/#tauworkspace) readiness and
   handoff validity -- `tau workspace status <name>`.
3. Client-side config validation and rendering --
   `tau run validate --config tau/train.yaml`.
4. [Queue](../../reference/glossary/#queue) admission and quota -- the Kueue
   admission phase in `tau run status`.
5. Kubernetes scheduling, DRA, image pull, init, and readiness -- the
   remaining phases in `tau run status`.
6. GPU/node/topology health -- `tau cluster validate nodes` /
   `tau cluster validate topology`.
7. Ray/Job runtime progress and durable evidence -- `tau logs <run-name>`
   and `taugrid-portal experiment status <name>`.
8. Recovery handoff -- [Retry and resume](../recovery/).

Confirm the queue admitted the workload (layer 4) before moving to GPU or
node debugging (layer 6). Treat a `Running` pod phase (layer 5) as evidence
of container start alone -- confirm separately that the model or data
process is making progress (layer 7).

The full decision path, with what each command's success and failure prove
and who owns the fix at each layer, is in
[Troubleshooting by lifecycle layer](../troubleshooting/).
