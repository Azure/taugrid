---
title: Connect to an AKS workspace
weight: 1
description: Understand how Azure CLI sign-in, Tau, and kubelogin work together for repository-based AKS access
---

{{< maturity status="ga" reviewed="2026-09-01" >}}

An AKS research repository can contain the cluster's non-secret resource ID and
Microsoft Entra tenant ID in `tau/workspace.connection.yaml`. You do not need a
platform operator's kubeconfig. Tau obtains normal cluster-user credentials for
your identity and stores an isolated kubeconfig outside the repository.

## Before your first connection

Install the required clients:

- the Tau CLI; and
- [`kubelogin`](https://azure.github.io/kubelogin/install.html) 0.1.7 or newer.

Installing [Azure CLI](https://learn.microsoft.com/cli/azure/install-azure-cli)
is recommended but not required. Without a usable Azure CLI session, Tau uses
its browser or device-code fallback.

Your platform team must grant your identity permission to obtain AKS
cluster-user credentials and the Kubernetes or Azure RBAC permissions declared
by the workspace. For a private cluster, follow the repository descriptor's
network instructions first.

Azure CLI sign-in is recommended because Tau can reuse one session for both the
Azure Resource Manager request and Kubernetes authentication:

```bash
az login --tenant <tenant-id>
cd <research-repository>
tau workspace connection
```

The last command guides the complete first-time setup:

1. Tau shows where the repository wants to connect: the descriptor, workspace,
   access method, Kubernetes context, authorization mode, network requirement,
   AKS resource ID, and tenant.
2. Compare those values with the platform handoff. Nothing has been accessed or
   saved yet, and the prompt defaults to decline.
3. Approve the destination to let Tau acquire credentials, verify the workspace,
   and save an isolated local connection.

When Tau prints `Connected`, the repository is ready for `tau run` and
`tau serve`. You do not need a separate login or connection command inside Tau.

The first approval requires an interactive terminal. If `tau run` or `tau serve`
encounters a repository that has not been connected on this machine, it tells
you to run `tau workspace connection`. After approval and verification,
subsequent commands reuse the pinned connection noninteractively.

The AKS resource ID in the repository selects the subscription and cluster, so
you do not need to run `az aks get-credentials` or merge anything into your
normal kubeconfig.

## How sign-in works

| Local state | What Tau does |
| --- | --- |
| `az` has an active session for the descriptor's tenant | Tau verifies that session with an Azure Resource Manager token, fetches AKS cluster-user credentials, and configures `kubelogin` to reuse Azure CLI authentication. |
| Azure CLI is missing or has no usable session | Tau opens browser-based Microsoft Entra sign-in and configures `kubelogin` for interactive authentication. |
| Browser sign-in is unavailable | From an interactive terminal, run `TAU_AUTH_MODE=devicecode tau workspace connection`; Tau and `kubelogin` use device-code authentication after checking for a usable Azure CLI session. |

`az login` authenticates your local Azure CLI. Tau does not read or copy its
tokens into the repository. Tau uses that identity to call the AKS cluster-user
credential API. The returned kubeconfig identifies the Kubernetes API server
and delegates token acquisition to `kubelogin`; it does not grant permissions
beyond those assigned to your Microsoft Entra identity.

Tau writes the selected context to a mode-`0600` kubeconfig in its local config
directory and verifies the TauWorkspace, namespace, queue, and authorization
contract before a Run or Serve command uses it.

## Start again without an active Azure session

Signing out of Azure CLI does not modify the repository:

```bash
az logout
```

An isolated kubeconfig configured for Azure CLI authentication cannot obtain a
new Kubernetes token until you run `az login` again. On a new machine or a
fresh Tau config directory, running `tau workspace connection` without an
active Azure CLI session starts Tau's browser fallback. Use device-code mode
for a terminal without a browser.

## Troubleshooting

Check whether Azure CLI has a session in the required tenant:

```bash
az account show
az login --tenant <tenant-id>
```

- An ARM authorization error means the signed-in identity cannot obtain
  cluster-user credentials for the resource ID. Ask the platform team to check
  the AKS cluster-user role assignment.
- A `kubelogin is required` error means the executable is not installed or is
  not on `PATH`.
- A Kubernetes forbidden error means cloud sign-in succeeded, but the identity
  lacks the workspace's required Kubernetes or Azure RBAC authorization.
- A network timeout for a private cluster means you must establish the required
  VPN or private network path before retrying.
