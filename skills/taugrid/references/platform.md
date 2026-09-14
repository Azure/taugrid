# Platform operations and workspace handoff

Use this reference for platform-owned changes and connection setup. Keep the
user's requested boundary: a preview, status check, or troubleshooting request
does not authorize install/apply/delete or privileged node probes.

## Namespaces and ownership

- `--system-namespace` selects TauGrid system objects, including TauWorkspace.
  Resolution can use the repository connection, `TAU_SYSTEM_NAMESPACE`, or
  the fallback `tau-system`; do not assume the fallback is the installed value.
- Workload `--namespace` is different. For workspace create/adopt it names the
  researcher's target namespace; for run/serve lifecycle commands it selects
  the workload namespace.
- The current v0 workspace creation contract permits one researcher workspace
  per cluster. Do not create another to work around a conflicting handoff.
- Cloud infrastructure, node pools, Azure RBAC, storage, and identity
  federation remain platform-owned. `tau cluster install` does not provision
  an AKS cluster.

## Install and inspect an existing cluster

Inspect the current values contract before preparing a change:

```bash
tau cluster explain-values
tau cluster install --help
tau cluster validate installation --context <context>
```

With installation authorization, use the approved chart version and values:

```bash
tau cluster install --context <context> --version <version> --values <values-file>
```

Install/uninstall are Helm-backed. Do not invent old storage/ServiceAccount
mutation flags or force Helm to take over GitOps-owned releases. Inspect the
actual release ownership and system namespace first.

## Create or adopt a workspace

Native CLI workflows now exist alongside reviewed Helm/GitOps/IaC.

**Create** requests controller-managed workspace objects. The baseline
ClusterQueue must already exist. Preview performs live preflight:

```bash
tau workspace create <workspace> \
  --namespace <workload-namespace> --system-namespace <system-namespace> \
  --principal-name <entra-group-object-id> --context <context>
```

Add `--apply` only after approval. The CLI conditionally creates the
TauWorkspace; the controller reconciles the namespace, RBAC, LocalQueue, and
optional workload-identity ServiceAccount.

Use the real identity-provider subject. Defaulting an Entra Group subject to
a human-readable workspace name permits bootstrap but grants nobody access
until the actual asserted group is configured. `Group`, `User`, and
`ServiceAccount` subjects are supported by the CRD; select the intended
principal explicitly instead of treating ServiceAccount as a test-only kind.
Optional `--service-account` and `--workload-identity-client-id` configure the
Kubernetes ServiceAccount, not cloud federation or Azure role assignments.

**Adopt** hands off existing platform-managed resources:

```bash
tau workspace adopt <workspace> \
  --namespace <workload-namespace> --queue <localqueue> \
  --data-pvc <existing-pvc> --system-namespace <system-namespace> \
  --context <context>
```

Preview checks the namespace, LocalQueue/backing ClusterQueue, and optional
Bound PVC. Use UID/StorageClass/ClusterQueue guards for a pinned adoption.
`--apply` repeats preflight/server dry-run and creates only the TauWorkspace;
compatible existing intent is a no-op, conflicting intent is refused. It does
not create the namespace, queue, RBAC, PVC, Secrets, or Azure resources.
`--data-pvc` defaults to `blob-training`; set the real name, or explicitly use
an empty value only for a handoff that needs no PVC.

`workspace-rbac` reconciles scoped researcher RBAC. `cluster-wide` creates no
researcher RBAC and relies on the caller's existing access; an isolated local
kubeconfig is not an isolation boundary. Do not switch authorization modes to
make a permission failure disappear.

## Connect and hand off a project

The repository contains a non-secret `tau/workspace.connection.yaml` and
checked-in targets. The descriptor declares the workspace, cluster/access
method, authorization mode, system namespace, and minimum compatible binary.
It must not contain credentials or kubeconfig contents.

```bash
tau workspace connection
tau workspace list -o json
tau workspace status <workspace> --system-namespace <system-namespace> -o json
tau workspace check <workspace> --data-pvc <pvc> \
  --system-namespace <system-namespace> --context <context>
```

Connection is live, not offline. First-time trust review needs an interactive
terminal. For AKS it uses the user's Azure identity and cluster-user access;
for `kubeconfig` it isolates the named context. Private clusters still need
network reachability; Entra exec credentials need `kubelogin`. Do not inspect
or copy the resulting private credential files as part of a routine diagnosis.

`workspace check` exits nonzero unless Ready. Its optional PVC check reports
storage diagnostics, but a missing/unbound PVC can be a **warning** with a
successful workspace exit status. Read the output, and verify required storage
separately:

```bash
kubectl --context <context> -n <workload-namespace> get pvc <pvc>
```

`Bound` does not prove mountability, writability, or application I/O.
Workspace phase requires `RBACReady` and `QueueReady`, with no
`DriftDetected=True`; there is no workspace `StorageReady` condition.
`WorkloadIdentityReady` is diagnostic rather than a phase gate.

For a new local project, `tau workspace init-repo NAME --image <pinned-image>`
scaffolds Python/uv files, targets, and lifecycle scripts. Templates are
`python`, `external-github`, and `dpr`; provide the platform connection values
from `--help` for a usable handoff. It is local generation, not cluster creation.
`tau-gen` shares this renderer. Do not use `--force` over unreviewed local files.

## Profiles, readiness, and node checks

Use [Workload profiles](workload-profiles.md) for catalog/readiness/snapshot
rules. Read-only connected checks:

```bash
tau cluster profiles export --context <context>
tau cluster validate topology --context <context>
tau cluster validate topology --context <context> --profile <profile>
```

Node validation is different: it creates privileged pods and is an authorized
operator action, not a routine read-only check:

```bash
tau cluster validate nodes --context <context> \
  --gpu-class <class> --min-healthy <count> --timeout 2m
```

Use `--selector` for a specific node subset. Interpret NVIDIA/NVLink/IB/ECC
results against the expected hardware; lack of NVLink on a standalone-GPU node
is not automatically a hardware failure.

## Quota

```bash
tau workspace quota show <workspace> --context <context> -o json
tau workspace quota request <workspace> --context <context> \
  --resource <resource> --current <count> --requested <count> \
  --duration 14d --reason <reason>
```

Request prints YAML by default; `--apply` creates the request. Approval is
recorded by the controller in ReportOnly mode; it does not patch ClusterQueue
quota. Even though an alternate mutation-mode value is accepted by the CLI,
do not promise automatic quota increases from that flag. Actual quota changes
remain a reviewed platform action.

## Monorepo routing

Use `tau.projects.yaml` at the Git worktree root only when multiple projects
need routing:

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

Use `tau run train --project vision` and the same project for run lifecycle
commands. For connection setup, pass a directory within the desired project:
`tau workspace connection projects/vision`. Do not invent `--project` on
`workspace connection` or `serve deploy`; run serving from the selected
project directory. Set descriptor version requirements from a verified binary,
not an old skill's hard-coded minimum.

## Offboarding

Only after explicit cleanup authorization, verify exact objects, active
workloads, ownership, and data retention. `tau cluster uninstall --yes` removes
the Helm-owned release, not external storage or cloud infrastructure.

TauWorkspace finalization cleans controller-owned access objects while
preserving the target namespace and researcher data. Adoption preserves
externally owned resources. Do not delete the namespace or PVC as an automatic
follow-up. For a scoped workspace deletion use its actual system namespace and
the canonical resource `workspaces.tau.azure.com`, then verify the result.

For read-only inspection of the native workspace object:

```bash
kubectl --context <context> -n <system-namespace> get workspaces.tau.azure.com <workspace>
```

Do not use bare `tauworkspace` as the kubectl resource name; use the canonical
plural above or a CRD-declared short name.
