---
title: Researcher quickstart
linkTitle: Quickstart
weight: 6
description: Run smoke, training, status, logs, and results
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

This is the **researcher workflow**, not cluster setup. It creates no Azure
resources and installs no Kubernetes controllers. It starts only after:

- **[AKS setup](../aks-setup/)** has produced a reachable managed-Entra
  AKS cluster and normal cluster-user credential path;
- **[TauGrid setup](../taugrid-setup/)** has installed and validated Kueue,
  KubeRay when required, the Tau controllers, and cluster-level policy; and
- **[Workspace setup](../workspace/)** has produced a Ready workspace, and a
  platform operator has handed over a Tau-enabled repository.

```bash
git clone <research-repository>
cd <research-repository>

tau run smoke
tau run train
```

On first use, `tau run smoke` uses TauGrid's first-class AKS connection path.
It discovers the repository's checked-in connection descriptor, obtains normal
AKS user credentials with the caller's Azure identity, verifies the live
workspace contract, and stores a dedicated local kubeconfig plus a durable
configuration pin. This path works without a TTY when the caller already has
usable noninteractive Azure authentication and kubelogin state. It then
submits a bounded Kubernetes workload using the workspace namespace, queue,
and service account. The dedicated kubeconfig is not a researcher-isolation
boundary, and the smoke does not verify access to external data services.
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
