# Platform operator reference

Commands in this file need cluster-admin-level access. `tau cluster validate
nodes` creates privileged pods, and cluster installation or TauWorkspace
creation changes platform desired state. These are deliberately not part of the
researcher-facing surface — do not hand them to researchers as a workaround for
a readiness failure.

Tau does **not** provision or delete cloud infrastructure. Create the AKS
cluster, node pools, network, and cloud RBAC with Azure tooling first; Tau then
prepares an *existing* cluster.

## Contents

- [Day 0: cluster install](#day-0-cluster-install)
- [Day 0: workspace declaration](#day-0-workspace-declaration)
- [The handoff to a researcher](#the-handoff-to-a-researcher)
- [Recovering a Degraded workspace](#recovering-a-degraded-workspace)
- [Cluster validation](#cluster-validation)
- [Offboarding](#offboarding)
- [Quota requests](#quota-requests)
- [Monorepo project catalogs](#monorepo-project-catalogs)

## Day 0: cluster install

Fresh clusters install the versioned TauGrid Helm distribution. Existing
ArgoCD-managed clusters keep Kueue, KubeRay, queue policy, and
`tau-core-controller` as independently owned Applications.

```yaml
# taugrid-values.yaml
baselineQueue:
  name: jobqueue
```

```bash
tau cluster install \
  --context <ctx> \
  --version 0.1.0 \
  --values taugrid-values.yaml
```

This is a thin `helm upgrade --install` wrapper. The deprecated `prepare` alias
invokes the same Helm-only path; legacy storage, lane, node-label, and
ServiceAccount mutation flags no longer exist.

## Day 0: workspace declaration

Platform owners declare `TauWorkspace` through reviewed Helm/GitOps/IaC. The
controller may create the target namespace, namespaced RBAC, a LocalQueue, and
an optional workload-identity ServiceAccount. It has no storage API or
`StorageReady` condition.

TauGrid 0.1 storage is separate platform desired state. Create a compatible PVC
in the target namespace through the platform delivery path, then reference it
with `storage.data_pvc` in the run config. Tau never provisions, adopts, owns,
resizes, migrates, or deletes the claim, its StorageClass/PV, CSI configuration,
or backing cloud storage. Blob and Lustre are optional backend examples, not
Tau requirements. Storage lifecycle functionality is a post-0.1 fast-follow
with no promised API shape or date.

### Authorization modes

`authorization.mode` defaults to **`workspace-rbac`**, where
`kubernetesSubject.kind` accepts only `Group` or `User` — `ServiceAccount` is
rejected because it is reserved for controller test fixtures.

The explicit **`cluster-wide`** mode omits principal/subject/role fields and
grants no RBAC, relying on access the caller already has. Be precise when
describing it: in `cluster-wide` mode, workspace selection supplies policy
defaults but is **not** researcher isolation. The AKS credential can access the
cluster, and Kubernetes audit records the shared cluster-user identity rather
than the initiating human. `workspace-rbac` — not `cluster-wide` — is the
actual isolation release gate.

## The handoff to a researcher

Before handing a repository over, confirm:

```bash
tau workspace check <name> --context <ctx>    # must exit 0
```

The researcher receives a repository containing a non-secret
`tau/workspace.connection.yaml` plus at least one target config:

```text
tau/
  workspace.connection.yaml
  smoke.yaml
  train.yaml
```

The descriptor names the exact cluster and workspace contract. It must **never**
contain a credential or kubeconfig. Also supply:

- The TauWorkspace name.
- Proof that `tau workspace check <name>` exits 0.
- Connection instructions when `network.privateCluster` is `true`.

On the researcher's first interactive `tau run`, Tau shows the workspace, AKS
resource ID, context, and authorization mode; asks for approval; uses their
signed-in Azure identity (or interactive browser, or device code with
`TAU_AUTH_MODE=devicecode`); requests normal AKS cluster-user credentials; and
writes a dedicated kubeconfig with mode `0600`.

Their Azure identity must be allowed to list AKS cluster-user credentials.
`kubelogin` is required for `workspace-rbac` and for any returned Entra exec
kubeconfig. Connect to the VPN/private network first when the cluster is
private.

`tau workspace init-repo NAME` scaffolds a Tau-ready Python + uv project
(README, `tau/smoke.yaml`, `tau/train.yaml`, training Dockerfile, lifecycle
scripts, `AGENTS.md`). Templates: `python` (default), `external-github`, `dpr`.
The standalone `tau-gen` binary shares the same renderer.

## Recovering a Degraded workspace

Start from the condition reason and message, not a guessed repair:

```bash
tau workspace status <name> --context <ctx>
tau workspace status <name> --context <ctx> -o json
```

The phase gates on `RBACReady` and `QueueReady` being true, with
`DriftDetected` not true. That is it — the controller's `workspacePhase()`
checks exactly those two conditions.

**Do not trust `Ready` as proof that storage works.** TauGrid 0.1 has no
`StorageReady` condition on `TauWorkspace` or `TauCluster`. A workspace can
report `Ready` with a missing or unbound platform-managed PVC. Verify it
directly before handing off:

```bash
kubectl get pvc blob-training -n <workspace-namespace> --context <ctx>
```

Even a `Bound` PVC is only an existence proof. Nothing write-validates the
volume, so mount-time failures (wrong BlobFuse credentials, read-only mount)
surface on the workload's own pod rather than in workspace status.

| Condition | Gates phase? | Action when False |
|---|---|---|
| `RBACReady` | yes | In `workspace-rbac` mode, correct the declared subject/role and let the controller reconcile namespaced RBAC. In `cluster-wide` mode, repair the pre-existing authorization — the controller grants none. |
| `QueueReady` | yes | Restore the named LocalQueue and its accessible backing ClusterQueue. Do not create a second queue under a different name to bypass the workspace spec. |
| `DriftDetected=True` | yes (→ Degraded) | Fix the dependency or workspace spec named in the message, then let the controller restore its owned objects. |
| `WorkloadIdentityReady` | no | Diagnostic only. Resolve before handing off workloads that use Azure Workload Identity. |

The controller reconciles periodically. After repairing the named dependency,
wait for `tau workspace check <name>` to exit 0. There is no client-side bypass
— the submission gate is enforced server-side, so repairing the dependency is
the only recovery action.

## Cluster validation

```bash
tau cluster validate nodes --gpu-class <class> --min-healthy <N> --timeout 2m
tau cluster validate nodes --selector <label-selector> --min-healthy <N>
tau cluster validate topology --preset azure.research.training.l
tau cluster validate topology --cluster-queue taugrid-cq
```

`validate nodes` runs privileged validation pods on the selected GPU nodes and
checks `nvidia-smi`, NVLink, IB, and ECC. `--timeout` (default `2m`) is
per-pod. A `DEGRADED`/`UNHEALTHY` result names a specific hardware reason — a
node problem, not a Tau or application bug.

`validate topology` without `--preset` validates every ResourceFlavor
referenced by `--cluster-queue` (default `taugrid-cq`). With `--preset` it
validates that single preset's full chain: LocalQueue, ClusterQueue, topology,
priority classes, and ResourceFlavor node match. Zero matching nodes for a
ResourceFlavor means the node pool, instance type, or GPU device plugin does not
match what the preset expects.

## Offboarding

```bash
tau cluster uninstall --context <ctx> --yes
```

This is a thin `helm uninstall` wrapper. The deprecated `offboard` alias invokes
the same command. It removes Helm-owned release resources; platform-owned
StorageClasses, PVCs, PVs, cloud storage, and workspace data remain because
TauGrid never owned them. Remove or migrate active TauWorkspace objects before
uninstalling the controller.

Deleting a TauWorkspace does **not** delete its target namespace. The finalizer
removes the access objects the controller owns — RBAC, controller-created
LocalQueues, workload identity ServiceAccounts — and leaves the namespace and
everything in it, because researcher PVCs and results live there. Delete the
namespace separately once you have confirmed nothing in it is still needed:

```bash
kubectl --context <ctx> delete workspaces.tau.azure.com <name> -n tau-system
kubectl --context <ctx> delete namespace <target-namespace>
```

`kubectl` resolves the CRD as `workspaces`, `workspace`, `tw`, or the fully
qualified `workspaces.tau.azure.com`. Bare `tauworkspace` is **not** accepted —
`kubectl` only matches the kind when the group is given, so
`tauworkspace.tau.azure.com` works while `tauworkspace` reports
`the server doesn't have a resource type`.

## Quota requests

```bash
tau workspace quota request <workspace> --context <ctx>
```

Creates a structured `TauQuotaRequest` (phases: `PendingApproval` →
`Approved`/`Rejected`/`Expired`). The controller's quota mutation mode is
**ReportOnly** — it records the request and never patches a Kueue ClusterQueue.
Actual quota changes remain a platform action.

## Monorepo project catalogs

A single-project repository needs no catalog. A monorepo adds one
`tau.projects.yaml` at the Git worktree root:

```yaml
schema: tau.projects.v1
projects:
  vision:
    path: projects/vision
    connection: projects/vision/tau/workspace.connection.yaml
  language:
    path: projects/language
    connection: projects/language/tau/workspace.connection.yaml
```

Tau selects a project from explicit `--project`, an explicit config path, the
current directory, a unique target name, or the only catalog entry. Ambiguity is
an error rather than an implicit cross-project guess:

```bash
tau run train --project vision
tau run status <run-name> --project vision
```

Repositories that require catalog routing should set
`requirements.minTauVersion: 0.5.0` in their workspace connection descriptor.
