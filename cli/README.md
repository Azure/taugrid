# Tau CLI

Tau v0.5 is a repository-first, Kubernetes-native interface for running AI
workloads on AKS. A project checks in workload intent; Tau combines it with
platform-owned workspace policy, renders upstream Kubernetes resources, submits
them through Kueue, and provides one lifecycle surface for the resulting run.

The Go CLI is the Kubernetes executor. The Python SDK is a first-class
code-centric authoring surface: its explicit train/eval handles and separately
submitted serve handles generate managed handoffs or CLI arguments and delegate
platform execution back to the CLI. Direct config remains the default for
repository targets. See the
[`Tau authoring strategy`](../../docs/tau/tau-authoring-strategy.md) before
choosing between direct config and decorators.

## The model

| Concept | Meaning |
| --- | --- |
| **Repository** | One Git worktree. It can contain one Tau project or a versioned `tau.projects.yaml` catalog for a monorepo. |
| **Project** | The unit that owns workload configs, named targets, and a workspace connection. |
| **Workspace** | Platform-owned cluster access and policy defaults such as namespace, queue, priority, and output root. It does not own project code or workload shape. |
| **Target** | A checked-in runnable config such as `tau/train.yaml`. `smoke` is the built-in workspace and cluster readiness target. |
| **Run** | One submitted execution and its lifecycle handle: status, logs, results, resume, and cancel. |
| **Workload** | The resolved execution intent Tau renders as a Kubernetes Job, RayJob, or serving resource. |
| **Experiment** | A comparison set over runs, metrics, and artifacts. Expstore is the local/offline source of truth; ADX/Kusto is a hosted scalar projection. |

The normal flow is:

```text
repository -> project -> target -> run -> Job | RayJob
                          ^
workspace policy ---------|
serve config + policy -> Service -> RayService | Deployment
experiment -> groups and compares runs
```

This split is intentional:

- The **project** owns code, images, runtime dependencies, resource intent,
  datasets, and artifacts.
- The **workspace/platform** owns cluster access, queues, priorities, and shared
  storage defaults.
- **Tau** owns deterministic resolution, validation, rendering, submission, and
  lifecycle handoff.
- **Kubernetes, Kueue, and KubeRay** remain the visible execution substrates;
  Tau does not replace their schedulers or controllers.

## First run

A Tau-enabled repository checks in a non-secret
`tau/workspace.connection.yaml` and one or more run configs:

```text
tau/
  workspace.connection.yaml
  train.yaml
```

See [`examples/kind-smoke`](./examples/kind-smoke/) for the Kind cluster config,
CPU-only lane manifest, and checked-in `tau.yaml`.

For researcher-facing, workspace-first examples, see
[`examples/`](./examples/). See
[`docs/tau/tau-project-onboarding.md`](../../docs/tau/tau-project-onboarding.md#examples-repo-handoff-contract)
for the exact handoff contract — a project cannot be promoted to that repo
until it meets that contract, and handoff there is currently **blocked** on
live `workspace-rbac` negative tests and H200/storage signoff.

## Day 0: platform-owned workspace desired state

Fresh clusters install the TauGrid distribution with `tau cluster install`,
which prints the Helm plan, performs the two-phase install, and fails unless
the required control plane passes its readiness report. Re-run the same
read-only report with `tau cluster validate installation`. Existing managed
clusters install the same components through their ArgoCD Applications. In
either case, a platform owner then declares a `TauWorkspace`; the controller
reconciles its namespace metadata, RBAC, LocalQueue, and optional
workload-identity ServiceAccount.

TauGrid v0 activates exactly one `TauWorkspace`. A non-GitOps platform owner can
create it directly through the preview-first admin workflow:

```bash
tau workspace create research \
  --context <admin-context> \
  --principal-name research-team

# Review the preflight result and rendered TauWorkspace, then:
tau workspace create research \
  --context <admin-context> \
  --principal-name research-team \
  --apply
```

The command requires the portable `jobqueue` ClusterQueue installed by
`tau cluster install`, server-dry-runs, and conditionally creates only the
native `TauWorkspace`. The controller creates the target Namespace, binds the
researcher identity to `tau-researcher-v1`, and creates a `jobqueue` LocalQueue
backed by that ClusterQueue. A matching workspace is a no-op; a different or
terminating workspace is refused. The controller also blocks extra workspace
objects from activating if they bypass the CLI. A reviewed manifest delivered
through Helm, Kustomize, or ArgoCD remains supported.

StorageClasses, durable PVCs, Azure identities, federation, and Azure role
assignments remain platform-owned. `--service-account` plus
`--workload-identity-client-id` asks the controller to reconcile only the
Kubernetes ServiceAccount.

For an existing, non-GitOps Namespace and LocalQueue, use the native-CR handoff
instead:

```bash
tau workspace adopt vision \
  --namespace vision \
  --queue jobqueue \
  --data-pvc blob-training \
  --storage-class azureblob-fuse-premium \
  --cluster-queue shared-gpu-cq \
  --priority default

# Review the preflight result and rendered TauWorkspace, then repeat with --apply:
tau workspace adopt vision \
  --namespace vision \
  --queue jobqueue \
  --data-pvc blob-training \
  --storage-class azureblob-fuse-premium \
  --cluster-queue shared-gpu-cq \
  --priority default \
  --apply
```

Preview is the default and performs only live reads. `--apply` server-side
dry-runs and conditionally creates one `TauWorkspace`; a compatible existing
workspace is a no-op and conflicting intent is refused. The command requires
the Namespace, LocalQueue, referenced ClusterQueue, and optional Bound PVC to
already exist; it does not create cluster-scoped infrastructure, storage,
queues, RBAC, Secrets, or Azure resources. Operators can pin the exact
Namespace, LocalQueue, and PVC identities with `--namespace-uid`, `--queue-uid`,
and `--pvc-uid`. This is an operator handoff requiring permissions to read those
resources and create `TauWorkspace` objects, not researcher self-service.

See
[`applications/tau-core-controller/README.md`](../applications/tau-core-controller/README.md)
and
[`docs/design/researcher-onboarding-tau-enabled-clusters.md`](../../docs/design/researcher-onboarding-tau-enabled-clusters.md)
for controller ownership and why `workspace-rbac` — not `cluster-wide` — is
the actual isolation release gate.

## Researcher first run (Day 2)

This is the Day 2 researcher workflow, entered only after a platform operator
has completed Day 0 onboarding above and the workspace controller reports the
workspace `Ready`.

A Tau-enabled research repository checks in a non-secret
`tau/workspace.connection.yaml`. After Tau is installed, the researcher path
is:

```bash
git clone <research-repository>
cd <research-repository>
tau run smoke
tau run train
```

`tau run smoke` automatically discovers the checked-in connection, obtains
normal AKS credentials, writes them to an isolated Tau kubeconfig, verifies the
live workspace contract, pins local configuration state, and submits a bounded
CPU smoke through Kueue. `tau workspace connection inspect` is an optional
offline diagnostic, not a first-run prerequisite.
`tau run train` resolves `tau/train.yaml` and submits the declared workload.
Neither command requires a pre-existing kube context, `TAU_CONTEXT`, namespace,
queue, or explicit `--config`.

During the current single-workspace phase, `cluster-wide` is the normal platform
shape and supplies policy defaults, but workspace selection is not an
authorization boundary: the AKS credential can access the cluster, and
Kubernetes audit records the shared cluster-user identity rather than the
initiating human. Future multi-workspace isolation uses `workspace-rbac`.

Use the run lifecycle after submission:

```bash
tau run status <run-name> --watch
tau run logs <run-name>
tau run get <run-name>  # Job-backed run with persisted results
tau run resume <run-name> --config tau/train.yaml
tau run cancel <run-name>
```

For copyable workspace-first repositories, run `tau workspace init-repo`, which
scaffolds a Tau-ready project layout. The examples under this directory are
primarily local fixtures and compatibility coverage.

## Monorepos

Single-project repositories need no catalog. A monorepo can add one
`tau.projects.yaml` at its Git worktree root:

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

## Public command surface

`tau --help` is the source of truth for the visible command tree:

| Command | Responsibility |
| --- | --- |
| `tau cluster` | Install/uninstall the TauGrid Helm distribution and validate an existing cluster. Tau does not provision AKS infrastructure. |
| `tau workspace` | Inspect workspace connections and readiness, request quota, and scaffold repositories. |
| `tau run` | Resolve a project target; submit, inspect, resume, or cancel the run, and fetch persisted results for Job-backed runs. |
| `tau serve` | Deploy and operate online model endpoints. |
| `tau data` | Manage dataset and model registries. |
| `tau python` | Invoke Python SDK helpers while keeping Go Tau as the executor. |
| `tau version` | Print build and version information. |

Experiment tracking (Stellar) and the observability Portal are no longer part
of this binary. They ship as `taugrid-portal experiment` and `taugrid-portal
portal` from `portal`.

Tau v0.5 retains some hidden pre-v0.5 root commands so existing scripts do not
break immediately. They are compatibility routes, not the surface for new docs
or automation. New work should use the grouped commands above; removing a
compatibility route requires an explicit migration decision and release note.

## Stable boundaries

- `tau.yaml`, `tau/<target>.yaml`, `tau.projects.yaml`, and
  `tau/workspace.connection.yaml` are separate contracts. Do not merge project
  workload intent with platform connection policy.
- `tau run --dry-run=client` is offline rendering. Server dry-run and live
  submission may perform cluster reads and validation.
- Workloads default to namespace `ray` only after explicit config, workspace,
  preset, and context resolution.
- Ray Train remains the default distributed training API. Tau owns
  policy-aware RayJob submission, not trainer setup.
- Local expstore remains authoritative for run packets, artifacts, checkpoints,
  and recovery state. ADX/Kusto is a downstream analytics projection only, not
  the source of truth, and telemetry is not the only copy of non-scalar run
  state.
- Secret values belong in Kubernetes Secret or Key Vault references, never in
  checked-in configs, ConfigMaps, annotations, logs, metrics, or screenshots.

## Documentation map

Each contract has one detailed owner; this README intentionally does not repeat
their flag, schema, profiler, telemetry, or artifact reference material.

| Need | Source of truth |
| --- | --- |
| Install or upgrade Tau | [`../site/content/en/docs/getting-started/install.md`](../site/content/en/docs/getting-started/install.md) and [`releases/v0.1.3.md`](releases/v0.1.3.md) |
| Project/workspace ownership and onboarding levels | [`../../docs/tau/tau-project-onboarding.md`](../../docs/tau/tau-project-onboarding.md) |
| Direct config versus Python decorators | [`../../docs/tau/tau-authoring-strategy.md`](../../docs/tau/tau-authoring-strategy.md) |
| Direct `tau run` config schema | [`../../docs/tau/tau-run-config.md`](../../docs/tau/tau-run-config.md) |
| Public, compatibility, and internal API inventory | [`../../docs/tau/tau-api-inventory-object-model.md`](../../docs/tau/tau-api-inventory-object-model.md) |
| Python SDK and Stellar HTTP boundary | [`../../docs/tau/tau-sdk-http-api-contract.md`](../../docs/tau/tau-sdk-http-api-contract.md) |
| Telemetry names and migration policy | [`../../docs/tau/tau-telemetry-schema-versioning.md`](../../docs/tau/tau-telemetry-schema-versioning.md) |
| Contributor review bar | [`SDK_GUIDE.md`](SDK_GUIDE.md) |
| Python authoring APIs | [`../sdk/python/README.md`](../sdk/python/README.md) |
| Historical workload redesign rationale | [`../../docs/design/tau-workload-submission-scheduling.md`](../../docs/design/tau-workload-submission-scheduling.md) |

## Development

```bash
make install
make test
tau --help
```

For a disposable Kind end-to-end that exercises Tau submission through Kueue:

```bash
make test-kind-e2e
```

See [`examples/kind-smoke`](./examples/kind-smoke/) for the checked-in local
fixture.
