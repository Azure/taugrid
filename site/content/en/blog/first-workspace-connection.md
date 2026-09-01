---
title: From Azure sign-in to your first Tau run
date: 2026-09-01
description: A safer one-command handoff from a cloned research repository to its AKS workspace
draft: false
---

A researcher should not need a platform engineer's kubeconfig to run a workload.
They should not need to understand how Azure Resource Manager credentials become
Kubernetes credentials, either. They should be able to clone a repository,
review where it wants to connect, and get to work.

That is the path behind TauGrid's workspace connection:

```bash
az login --tenant <tenant-id>
cd <research-repository>
tau workspace connection
```

The repository contains non-secret coordinates: an AKS resource ID, Microsoft
Entra tenant, workspace name, Kubernetes context, and expected authorization
mode. It does not contain a token or a shared kubeconfig.

## A deliberate pause before access

The first live connection begins with a review:

```text
First-time workspace connection
This repository has not been connected with Tau on this machine.
Review where Tau will connect:
  Workspace:       research-a
  Access method:   aks
  Context:         aks-research
  Authorization:   workspace-rbac
  Private network: not indicated

Nothing has been accessed or saved yet.

Approve and connect? [y/N]
```

That pause matters because a managed-cloud adapter can authenticate and contact
cloud APIs, while a generic kubeconfig can invoke a standard exec credential
plugin. Tau defaults to no and performs none of those actions before approval.
Automation also fails closed until a researcher completes this review in an
interactive terminal.

After approval, Tau uses the researcher's identity to obtain AKS cluster-user
credentials, normalizes the `kubelogin` configuration, verifies the workspace
contract, and stores an isolated mode-`0600` kubeconfig outside the repository.
The normal kubeconfig is left alone.

## One connection, then normal work

A successful connection ends with the information a researcher needs:

```text
Connected.
Workspace:     research-a
Status:        Ready
Namespace:     research-a
Queue:         jobqueue
Authorization: workspace-rbac
Ready:         tau run can now use this workspace.
```

Later commands reuse the pinned connection. If the repository descriptor changes
its destination or authorization contract, Tau asks for another review instead
of silently following the change.

We exercised the complete path against a live AKS workspace: local-only
validation, fail-closed noninteractive first use, decline without local state,
interactive approval, noninteractive reuse, and `tau run list` through the
isolated connection.

For the current workspace workflow and prerequisites, read
[Run your first workload](../../docs/developer-guide/first-run/).
