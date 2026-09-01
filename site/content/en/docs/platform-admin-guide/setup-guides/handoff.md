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
| `cluster.contextName` | The Kubernetes context name Tau selects. |
| `cluster.systemNamespace` | The TauGrid system namespace; defaults to `tau-system`. |
| `access.method` | `kubeconfig` for an existing context or `aks` for automatic AKS credential acquisition. |
| `access.aks.resourceID` | Required only for `access.method: aks`; the AKS cluster's ARM resource ID. |
| `access.aks.tenantID` | Required only for `access.method: aks`; the Microsoft Entra tenant ID. |
| `authorization.mode` | `cluster-wide` or `workspace-rbac`. |
| `authorization.requiredRole` | Required only in `workspace-rbac` mode; forbidden in `cluster-wide` mode. |
| `requirements.minTauVersion` | The minimum compatible `tau` CLI version. |
| `network.privateCluster` | Whether the Kubernetes API server requires private network access. |
| `network.instructions` | Required only when `privateCluster: true` (for example, VPN steps). |

**It must never contain a credential, kubeconfig, client secret, or cloud access
token.** With `access.method: kubeconfig`, `tau` loads the normal kubeconfig
rules (including `KUBECONFIG`) and copies only the named context, cluster, and
user into an isolated mode-`0600` kubeconfig outside the repository. Standard
kubeconfig exec credential plugins remain available, so this is the universal
path for conformant Kubernetes platforms including self-managed, bare-metal,
and managed clusters. With `access.method: aks`, the first-class AKS adapter
obtains normal cluster-user credentials through the caller's Azure identity,
normalizes `kubelogin`, and isolates the result instead. Tau's workspace,
Run, and Serve paths consume only that isolated Kubernetes connection; cloud
provider behavior ends at credential acquisition.

A provider-neutral descriptor uses an existing Kubernetes context:

```yaml
schema: tau.workspace.connection.v1
workspace: research
cluster:
  contextName: research-cluster
  systemNamespace: tau-system
access:
  method: kubeconfig
authorization:
  mode: workspace-rbac
  requiredRole: tau-researcher-v1
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
```

An AKS platform can replace only the access block:

```yaml
access:
  method: aks
  aks:
    resourceID: /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerService/managedClusters/<cluster>
    tenantID: <tenant-uuid>
```

For a clean-machine researcher experience, grant the researcher permission to
obtain cluster-user credentials, provide any private-network instructions, and
point them to [Connect to an AKS workspace](../../developer-guide/aks-access/).
They can run `az login --tenant <tenant-id>` once and Tau will reuse that Azure
CLI session for both the ARM credential request and `kubelogin`. If they have no
active Azure CLI session, Tau falls back to browser authentication; headless
users can select device-code fallback with
`TAU_AUTH_MODE=devicecode tau workspace connection`.

## Repository placement

Commit `tau/workspace.connection.yaml` at the repository root, alongside the target configs it governs (for example `tau/smoke.yaml`, `tau/train.yaml`). Two ways to produce it:

- Author it directly from the table above.
- Generate a provider-neutral descriptor with `tau workspace init-repo <name> --workspace <workspace> --kube-context <context> --image <build-tag>`.
- For automatic AKS access, add `--azure-subscription-id <id> --azure-tenant-id <id> --aks-resource-group <group> --aks-cluster <cluster>`. `--kube-context` then defaults to the AKS cluster name.

The generated targets are ready only after the project image is built and pushed, its immutable tag or digest is written back, and config validation succeeds:

```bash
./scripts/configure.sh --image "<registry>/<repository>:<immutable-tag>"
tau run validate --config tau/train.yaml
```

## Verify the repository connection

```bash
tau workspace connection
```

This resolves credentials, contacts Kubernetes, and verifies the descriptor's
workspace, LocalQueue, and authorization contract without submitting a
workload. Use `tau workspace connection --offline` when only local descriptor
validation is appropriate.

The researcher must run the first live `tau workspace connection` from an
interactive terminal. Tau shows the descriptor's non-secret connection identity
and requires explicit trust before it loads credentials, invokes an exec
credential plugin, contacts the cloud or cluster, or writes local connection
state. Noninteractive Run and Serve commands fail closed until that trust
bootstrap succeeds. Later commands reuse the pinned connection, and any
descriptor identity change requires review again.

Before handoff, platform operators can also inspect the named workspace
directly:

```bash
tau workspace check <workspace> --context <context>
```

## First project run

From the same clean checkout, in order:

```bash
tau run validate --config tau/train.yaml
tau run train --dry-run=client
tau run train
```

The first command validates the config entirely offline. In a connected
repository, the client dry-run activates the descriptor, resolves credentials
through its configured access method, verifies and pins the workspace contract, and reads the live
workload-profile catalog without submitting the rendered workload. The final
command submits the project target and exercises its declared image and
resources. Validate the workspace PVC mount and external cloud identity or data
service with separate readiness checks.

## What "handed off" means

A handoff is done only when all of the following hold, reproduced from a clean checkout:

- `tau workspace check <workspace>` exits `0`.
- A checked-in project target completes.

Only then send the researcher the repository URL and, if `network.privateCluster` is `true`, the connection instructions. See [Hand off to researchers](../kubernetes/#hand-off-to-researchers) for the matching completion checklist.
