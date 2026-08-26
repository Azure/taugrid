---
title: Hand off a workspace
linkTitle: Hand off a workspace
weight: 20
description: The concrete, non-secret artifact and proof that ends platform Day 0
url: "/docs/platform-admin-guide/handoff/"
aliases:
  - "/docs/tasks/platform/handoff/"
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

Complete this checklist after the [TauWorkspace readiness gate](../../reference/workspace/#workspace-readiness-and-recovery) reports `Ready`. A handoff is reproducible only when every step below succeeds from a clean checkout, not from a platform owner's already-primed shell.

## The non-secret connection descriptor

The only artifact a researcher needs is `tau/workspace.connection.yaml`, a non-secret [workspace connection descriptor](../../reference/glossary/#workspace-connection):

| Field | Contains |
| --- | --- |
| `schema` | Fixed value `tau.workspace.connection.v1`. |
| `workspace` | The [TauWorkspace](../../reference/glossary/#tauworkspace) name. |
| `cluster.provider` | `azure` today. |
| `cluster.resourceID` | The AKS cluster's ARM resource ID. |
| `cluster.contextName` | The kubeconfig context name to select. |
| `identity.tenantID` | The Microsoft Entra tenant ID. |
| `authorization.mode` | `cluster-wide` or `workspace-rbac`. |
| `authorization.requiredRole` | Required only in `workspace-rbac` mode; forbidden in `cluster-wide` mode. |
| `requirements.minTauVersion` | The minimum compatible `tau` CLI version. |
| `network.privateCluster` | Whether the AKS API server is private. |
| `network.instructions` | Required only when `privateCluster: true` (for example, VPN steps). |

**It must never contain a credential, kubeconfig, client secret, or Azure access token.** On first cluster-backed use, `tau` obtains normal AKS cluster-user credentials through the caller's Azure identity and writes an isolated mode-`0600` kubeconfig outside the repository.

## Repository placement

Commit `tau/workspace.connection.yaml` at the repository root, alongside the target configs it governs (for example `tau/smoke.yaml`, `tau/train.yaml`). Two ways to produce it:

- Author it directly from the table above.
- Generate it with `tau workspace init-repo <name> --workspace <workspace> --azure-subscription-id <id> --azure-tenant-id <id> --aks-resource-group <group> --aks-cluster <cluster> --image <build-tag>`, which also scaffolds the rest of the repository. The descriptor is generated only when `--workspace` and all four Azure connection fields are present; otherwise the generator prints a reminder that the platform owner still needs to add one.

The generated targets are ready only after the project image is built and pushed, its immutable tag or digest is written back, and config validation succeeds:

```bash
./scripts/configure.sh --image "<registry>/<repository>:<immutable-tag>"
tau run validate --config tau/smoke.yaml
tau run validate --config tau/train.yaml
```

## Optional descriptor-only inspection

```bash
tau workspace connection inspect
```

This discovers and validates the descriptor entirely offline, as an optional
diagnostic; `tau run` discovers the file automatically as part of normal
activation. Before handoff, confirm the workspace it names is still `Ready`:

```bash
tau workspace check <workspace> --context <context>
```

## First smoke or dry-run step

From the same clean checkout, in order:

```bash
tau run train --dry-run=client
tau run smoke
```

The first command validates the local descriptor and config entirely
offline; live-only values remain visible placeholders. `tau run smoke` activates the descriptor, obtains AKS user credentials, verifies and pins the workspace contract, then proves queue admission, scheduling, service-account selection, and pod execution with a bounded non-root job. Validate the workspace PVC mount and external cloud identity or data service with separate readiness checks.

## What "handed off" means

A handoff is done only when all of the following hold, reproduced from a clean checkout:

- `tau workspace check <workspace>` exits `0`.
- `tau run smoke` completes.

Only then send the researcher the repository URL and, if `network.privateCluster` is `true`, the connection instructions. See [Hand off to researchers](../kubernetes/#hand-off-to-researchers) for the matching completion checklist.
