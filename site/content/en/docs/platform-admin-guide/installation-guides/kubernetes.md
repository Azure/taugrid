---
title: Getting started on Kubernetes
linkTitle: Install on Kubernetes
weight: 10
description: Install TauGrid on Kubernetes, create a workspace, and run your first workload
url: "/docs/platform-admin-guide/kubernetes/"
aliases:
  - "/docs/getting-started/kubernetes/"
---

TauGrid is a Kubernetes-native workflow layer for AI workloads. It installs on
a Kubernetes cluster your platform provisions and operates. This is the
canonical, provider-neutral installation path.

This page takes a platform operator from an empty workstation to a validated
TauGrid workspace and a completed smoke workload. The portable path works on
Kubernetes 1.30 or newer. Provider-specific identity, networking, storage, GPU
drivers, and cluster lifecycle remain owned by your platform.

## What you will set up

1. Install the Tau CLI and local Kubernetes tools.
2. Select or provision a Kubernetes cluster.
3. Install and validate the TauGrid control plane.
4. Create a workspace for workloads.
5. Submit a smoke workload.

The first four steps require cluster-administrator access. Researchers need
only the Tau CLI, access to the finished workspace, and the repository or
configuration they will run.

## 1. Install the Tau CLI

Install these tools on the operator workstation:

- `kubectl`
- Helm 3 or 4
- Git
- the released `tau` CLI

Install the latest Tau CLI release with standard user permissions:

```bash
curl -fsSL https://github.com/Azure/taugrid/releases/latest/download/install.sh | sh

export PATH="$HOME/.local/bin:$PATH"
tau version --short
kubectl version --client
helm version --short
```

Add `$HOME/.local/bin` to your shell startup file so future terminals can find
`tau`. The installer supports Linux and macOS on amd64 and arm64, verifies the
release checksum, and installs only the CLI.

For a pinned version, upgrades, source builds, or the optional Python SDK, see
the [Tau CLI release assets](https://github.com/Azure/taugrid/releases) and the
[repository installation instructions](https://github.com/Azure/taugrid#install-tau).

## 2. Prepare a Kubernetes cluster

TauGrid needs a reachable Kubernetes 1.30+ cluster and a kubeconfig context
with cluster-administrator access for installation. Start with an existing
cluster if you have one:

```bash
export TAU_CONTEXT="<kubeconfig-context>"

kubectl --context "$TAU_CONTEXT" version
kubectl --context "$TAU_CONTEXT" get nodes
```

Confirm that the server is Kubernetes 1.30 or newer and that the nodes required
for your first workload are `Ready`.

Before TauGrid is installed, the cluster owner is responsible for:

- API-server network access and authentication;
- worker node pools and autoscaling;
- container registry access;
- GPU drivers and device plugins when workloads request GPUs;
- CSI drivers, StorageClasses, and persistent volumes when workloads use
  storage; and
- cloud identities and permissions when workloads call provider services.

TauGrid consumes these capabilities through Kubernetes APIs. See
[Architecture](../../getting-started/architecture/) for component ownership.

## 3. Install TauGrid

TauGrid installs a version-aligned OCI chart from Microsoft Container Registry.
The released Tau CLI is the supported installation entry point.

First preview the exact release as a dry run:

```bash
tau cluster explain-values
tau cluster install --context "$TAU_CONTEXT" --dry-run
```

The dry run should end with `TauGrid render summary (nothing was applied)`.
Review the rendered plan before continuing.

Install TauGrid:

```bash
tau cluster install --context "$TAU_CONTEXT"
```

The default distribution installs Kueue, KubeRay, the Tau controller and CRDs,
a portable baseline queue, GPU monitoring profiles, and the TauGrid Portal.
The chart excludes provider GPU drivers and storage drivers.

If the platform needs different queue quotas, GPU labels, or component
settings, keep the complete reviewed configuration in one values file:

```bash
tau cluster install \
  --context "$TAU_CONTEXT" \
  --values taugrid-values.yaml \
  --dry-run

tau cluster install \
  --context "$TAU_CONTEXT" \
  --values taugrid-values.yaml
```

Pass the same complete values file on later upgrades because installation
resets omitted settings to chart defaults. The
[cluster install values reference](../../reference/cluster-install-values/)
documents every supported setting.

Re-run the control-plane readiness gate:

```bash
tau cluster validate installation --context "$TAU_CONTEXT"
```

Continue only when the command exits successfully and every enabled core check
is `PASS`. Only a Helm release that passes this gate is ready for a
workspace.

## 4. Create a workspace

TauGrid v0 supports one active workspace per cluster. A workspace reconciles
the workload Namespace, LocalQueue, ServiceAccount selection, and researcher
RBAC. Platform teams provision cloud storage, a PVC, and cloud identity
resources separately.

For a single-operator evaluation, create the default workspace:

```bash
export TAU_WORKSPACE="taugrid-default"
export TAU_SYSTEM_NAMESPACE="tau-system"

tau workspace create "$TAU_WORKSPACE" \
  --context "$TAU_CONTEXT" \
  --apply
```

When `--principal-name` is omitted, TauGrid creates an intentionally inert
researcher group binding. The cluster administrator can still complete this
guide, but no researcher receives access. For a shared environment, create the
workspace with the real Kubernetes subject from the start instead:

```bash
tau workspace create "$TAU_WORKSPACE" \
  --context "$TAU_CONTEXT" \
  --principal-name "<kubernetes-group-or-user>" \
  --subject-kind Group \
  --apply
```

Use `--subject-kind User` for an individual user or `ServiceAccount` for an
automation identity. The value must match the subject presented to Kubernetes
by your provider's authentication layer.

Wait for reconciliation and inspect the result:

```bash
kubectl --context "$TAU_CONTEXT" wait \
  --for=jsonpath='{.status.phase}'=Ready \
  "workspaces.tau.azure.com/$TAU_WORKSPACE" \
  --namespace "$TAU_SYSTEM_NAMESPACE" \
  --timeout=5m

tau workspace status "$TAU_WORKSPACE" --context "$TAU_CONTEXT"
tau workspace check "$TAU_WORKSPACE" --context "$TAU_CONTEXT"
```

The workspace is ready when `tau workspace check` exits successfully,
`RBACReady=True`, `QueueReady=True`, and no drift is reported.

If the first project needs persistent storage, provision the provider's
StorageClass and PVC separately, then include the claim in the workspace check:

```bash
tau workspace check "$TAU_WORKSPACE" \
  --context "$TAU_CONTEXT" \
  --data-pvc "<pvc-name>"
```

A bound claim proves provisioning; application-level read/write access still
needs a separate check. Test the mount with the same ServiceAccount and
security settings the workload will use before handing the workspace to a
researcher.

## 5. Run the smoke workload

The built-in smoke run verifies workspace discovery, queue admission,
scheduling, ServiceAccount selection, and container execution:

```bash
tau run smoke --context "$TAU_CONTEXT"
```

A successful run exits with status `Succeeded`, validating workspace
discovery, queue admission, scheduling, and container execution. Validate a
project image, GPU, persistent volume, or external cloud service with your
first real project workload.

For a larger provider-neutral CPU workload, clone the repository and run the
[distributed CPU Ray example](https://github.com/Azure/taugrid/tree/main/examples/cpu-multi-interest-ray):

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid

tau run \
  --context "$TAU_CONTEXT" \
  --config examples/cpu-multi-interest-ray/tau.yaml
```

## Hand off to researchers

Before a shared workspace is ready for researchers:

1. Bind the workspace to the real Kubernetes user, group, or ServiceAccount.
2. Build and publish the project image with an immutable tag or digest.
3. Provision and test any required PVC and workload identity.
4. Generate or configure the research repository, keeping kubeconfigs,
   tokens, client secrets, and registry credentials out of it.
5. Prove `tau run smoke` from a clean researcher checkout and identity.

Use the platform admin guide to
[enable a workspace](../enable-workspace/) and
[hand it off](../handoff/). The AKS handoff path can additionally
generate a non-secret connection descriptor that acquires managed-Entra
cluster-user credentials; other Kubernetes platforms can supply a kubeconfig
through their normal access process.

## Next steps

- Run the [TauGrid examples](../../examples/) for training, tuning, and serving.
- Configure deliberate quotas and profiles with
  [cluster install values](../../reference/cluster-install-values/).
- Add provider integrations only when workloads need them; use the
  [platform admin guide](../) for identity, storage, and observability.
- Use the [platform admin guide](../) for production
  identity, storage, observability, and lifecycle ownership.
- Need an AKS cluster to run this guide against? See
  [Getting started on AKS](../aks-setup/).
