---
title: Researcher quickstart
linkTitle: Quickstart
weight: 6
description: Run smoke, training, status, logs, and results
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

This path starts after a platform operator has provided a Ready workspace and a
Tau-enabled repository.

```bash
git clone <research-repository>
cd <research-repository>

tau run smoke
tau run train
```

On first use, `tau run smoke` automatically discovers the repository's checked-in
connection descriptor, obtains normal AKS user credentials with the caller's
Azure identity, verifies the live workspace contract, and stores a dedicated
local kubeconfig plus a durable configuration pin. The path works without a TTY
when the caller already has usable noninteractive Azure authentication. It then
submits a bounded workload using the workspace namespace, queue, and service
account. The dedicated kubeconfig is not a researcher-isolation boundary and
the smoke does not prove access to external data services.
`tau run train` resolves the checked-in target and submits its Job or RayJob.

Observe and control the submitted run:

```bash
tau run status <run-name> --watch
tau run logs <run-name>
tau run get <run-name>
tau run cancel <run-name>
```

Use client dry-run before a new workload shape:

```bash
tau run train --dry-run=client
```

The project owns its entrypoint, runtime, model code, data contract, and output
semantics. Tau owns deterministic resolution, platform policy handoff,
rendering, submission, and lifecycle commands.
