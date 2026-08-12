---
title: Connect to a Tau workspace
weight: 5
description: Request access, receive a workspace connection, and prove you can start
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

A platform operator completes infrastructure, queue, storage, identity, and
[workspace](../../concepts/glossary/#workspace) readiness before handing a
repository to a researcher — see
[prerequisites and the readiness gate](../prerequisites/) for the shared
contract both sides check.

## What to request from the platform team

Ask the platform owner for:

- The [TauWorkspace](../../concepts/glossary/#tauworkspace) name.
- Confirmation that `tau workspace check <workspace>` exits `0` (`Ready`).
- A repository containing `tau/workspace.connection.yaml` and at least one
  target config.
- Connection instructions if the cluster's `network.privateCluster` is `true`.

## The repository you receive

```text
tau/
  workspace.connection.yaml
  smoke.yaml
  train.yaml
```

`tau/workspace.connection.yaml` names the exact cluster and workspace
contract; it must not contain a credential or kubeconfig. See the
[handoff checklist](../../tasks/platform/handoff/) for the full field-by-field
breakdown of what it contains and does not contain.

## What proves you can start

From the repository:

```bash
tau run smoke
```

`tau run smoke` automatically discovers the descriptor, configures the local
connection, verifies that the named workspace is `Ready`, and runs the built-in
bounded onboarding job. If it completes, queue admission, scheduling, the
selected service account, and container execution work end to end. It does not
mount the workspace PVC or access an external cloud service.

`tau workspace connection inspect` is an optional offline diagnostic when you
only want to parse and display the descriptor. It is not an activation or
onboarding prerequisite.

## How the first cluster connection works

Tau does not require a checked-in kubeconfig or client secret. The checked-in
descriptor is explicit repository/platform preconfiguration. On the first
cluster-backed `tau run`, Tau:

1. Uses your signed-in Azure CLI identity when available, otherwise interactive
   browser authentication (or device code with `TAU_AUTH_MODE=devicecode`).
2. Requests normal AKS cluster-user credentials, writes a dedicated kubeconfig
   with mode `0600` under your Tau user configuration directory, and verifies
   the exact context, Ready state and observed generation, workspace UID,
   namespace, LocalQueue, service account, and authorization.
3. Pins the verified configuration and proceeds. Unchanged readiness evidence
   refreshes automatically after five minutes.

Your Azure identity must be allowed to list AKS cluster-user credentials.
Noninteractive callers need an already usable noninteractive Azure identity;
Tau does not bypass authentication. `kubelogin` is required in `workspace-rbac`
mode, and in `cluster-wide` mode whenever AKS returns an Entra exec kubeconfig.
Connect to the VPN/private network before the live command when
`network.privateCluster` is `true`.

The dedicated local kubeconfig avoids mutating your main kubeconfig; it is not
a claim of researcher isolation. Tau does not silently accept descriptor,
trust-identity, authorization-mode, cluster, tenant, workspace, UID, namespace,
queue, or service-account drift after state is pinned. Noninteractive commands
fail closed; an interactive caller must review and confirm the change.

`workspace-rbac` is the API default and what `tau workspace create` writes; the
controller binds your subject in the workspace namespace. `cluster-wide`
is an explicit opt-out that grants no researcher access and supplies policy
defaults only; some existing clusters run it. Multi-workspace activation is
future work: v0 activates exactly one workspace per cluster.

Platform engineers should use the
[workspace enablement task](../../tasks/platform/enable-workspace/) and the
[handoff checklist](../../tasks/platform/handoff/).
