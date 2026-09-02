---
title: CLI reference
weight: 1
description: Canonical Tau CLI command families
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The Tau CLI has exactly eight public command roots. Every example in this site uses
one of these roots and its current nested subcommand path:

| Root | Purpose |
|---|---|
| [`tau cluster`](#tau-cluster) | Install/uninstall TauGrid and validate an existing cluster |
| [`tau workspace`](#tau-workspace) | Create/adopt workspaces and inspect readiness, quota, and repo scaffolds |
| [`tau run`](#tau-run) | Config-first submit, lifecycle, and retry/resume for one [run](../glossary/#run) |
| [`tau logs`](#tau-logs) | Find a run through configured Tau workspaces and stream its logs |
| [`tau serve`](#tau-serve) | Render and manage serving deployments |
| [`tau data`](#tau-data) | Inspect dataset and model registries |
| `tau python` | Optionally author decorator-based `@tau.train` / `@tau.eval` workflows; standard `tau.yaml` projects do not need the Python SDK |
| `tau version` | Print the installed CLI version |

Run evidence and the observability portal ship in a separate binary,
`taugrid-portal`. See [`taugrid-portal experiment`](#taugrid-portal-experiment)
below.

The old flat roots have been removed. Pre-v0.5 top-level aliases (ray, model,
dataset, exp, queue, status, cancel, topology, lanes, storage, eval,
shell, submit, and finetune) are gone; their capabilities live under the seven
roots above. Job execution is owned directly by the config-first `tau run` path.

## `tau cluster`

Install the versioned TauGrid distribution on a fresh Kubernetes cluster, or
validate an already-onboarded cluster:

| Subcommand | Purpose |
|---|---|
| `install` | Show the TauGrid plan, run the two-phase Helm install, and wait for control-plane readiness |
| `uninstall` | Drain the release-owned queue policy while Kueue still runs, then run `helm uninstall` for the TauGrid release (requires `--yes`) |
| `validate installation` | Re-run the read-only TauGrid control-plane readiness report |
| `validate nodes` | Run privileged GPU health probes (nvidia-smi, NVLink, IB, ECC) across nodes |
| `validate topology` | Check ResourceFlavor, node label, and IB readiness against ready TauCluster workload profiles |
| `profiles` | Inspect ready TauCluster workload profiles (subcommand: `export`) |
| `explain-values` | Print the TauGrid install values reference (human-readable Markdown) |

`validate installation`, `validate nodes`, and `validate topology` are
platform/operator diagnostics: run them against a cluster you administer, not
from inside a researcher project. Installation validation is read-only. Node
validation creates short-lived privileged probes and remains explicitly
opt-in. Pair these with per-run troubleshooting in
[recovery](../../platform-admin-guide/recovery/) for workload-specific issues.

`install` validates Kubernetes 1.30+, the three controller Deployments,
TauCluster node readiness, the baseline ClusterQueue, and the narrow
fail-closed quota guard before reporting success; provisioning AKS, mutating
nodes, creating PVCs, and configuring Azure identities remain platform-owned
steps outside this command. Readiness runs after Helm, so a failed
readiness report leaves the release installed for inspection; uninstall still
retains CRDs, custom resources, retained namespaces, storage, and workspace
data. Existing ArgoCD-managed clusters should retain independent component
Applications instead of installing the umbrella release.

The default install runs the Tau CLI's bounded, component-aware readiness report
after Helm applies the release, instead of Helm's generic `--wait` or
automatic rollback. Pass `--wait` to add Helm's generic resource watcher as an
earlier gate. Pass `--atomic` to roll back on a Helm failure; rollback also
enables Helm's watcher, and the Tau CLI translates this compatibility flag to Helm 4's
`--rollback-on-failure`. `--timeout` bounds each Helm operation and the Tau CLI's
readiness report.

The compiled chart default is the public
`oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid` OCI reference. The
requested version must be published. If it is unavailable, the Tau CLI reports the
exact chart and version and asks the operator to pass an accessible `--chart`
reference or local path.

`uninstall` runs in two Helm phases. Helm's own uninstall order deletes
Deployments before custom resources, so removing the release in one pass would
take the Kueue controller down while its ClusterQueue, ResourceFlavor, and
Topology still carry the `kueue.x-k8s.io/resource-in-use` finalizer, leaving
nothing able to clear it. The first phase therefore removes the queue policy
while Kueue is still running. Pass `--drain-queue=false` to skip it. In the
stranded-finalizer case, no controller remains to clear the finalizer, so
raising `--timeout` only delays an expiring wait rather than fixing it.

The final uninstall waits for resource removal by default; pass `--wait=false`
to return after Helm submits the deletions, and use `--timeout` to bound either
mode.

The drain re-renders the release, so it needs the same `--chart` reference the
release was installed from. Uninstall compares that reference against the chart
name Helm recorded for the release and skips the drain when they disagree,
rather than applying a different chart's manifests on the way out. When the
reference cannot be resolved, or the drained objects are still present
afterwards (Kueue holds `resource-in-use` until an active run finishes),
uninstall reports which objects may strand and proceeds; teardown still
completes. Draining is only possible through `tau`, because Helm decides the
deletion order of a release's own custom resources; a plain `helm uninstall`
of the TauGrid release still strands them.

On Helm 4, both verbs send `--force-conflicts` (paired with
`--server-side=true`) because AKS's `admissionsenforcer` addon co-owns
`.webhooks[*].namespaceSelector` on Kueue's webhook configurations. Helm 4
defaults to server-side apply, so every upgrade of the release, including the
uninstall drain, requires `--force-conflicts` to avoid failing on those
fields. Forcing settles field ownership only, leaving the AKS-managed selector
values untouched since the chart declares no value for them. Helm 3 skips this
entirely and receives neither flag. Leave the Kueue webhook
configurations in place to avoid this conflict: recreating them can leave
`caBundle` empty until the Kueue controller restarts.

## `tau workspace`

Researcher-facing: `list`, `status <name>`, `check <name>`,
`connection [path]`, `quota show <workspace>`,
`quota request <workspace>`, and `init-repo NAME` (scaffold a TauGrid-ready repo).
Platform-operator: `create [NAME]` (create the single v0 researcher workspace)
and `adopt NAME` (adopt an existing platform-provisioned namespace). Only
`quota request`, `create`, and `adopt` write to the cluster; each previews by
default and needs `--apply` to take effect. Workspace desired state is a native
`TauWorkspace` delivered through Helm/Kustomize/GitOps and reconciled by the
[TauWorkspace](../glossary/#tauworkspace) controller.

`tau workspace create` requires `--principal-name <external-group-or-team>`;
`--subject-name` defaults to that principal when it is omitted.
`tau workspace connection` verifies and pins the current project's configured
workspace access; `tau run` also discovers and configures the connection
automatically.

## `tau run`

`tau run [TARGET] [--config tau.yaml]` is the config-first entry point.
**`TARGET` is an optional positional argument to the `run` root itself, rather
than a subcommand**: `tau run train --dry-run=client` runs the `run` root with
`TARGET=train` and `--dry-run=client`, resolving `tau/train.yaml`. See
[run config](../../reference/run-config/) for the field reference and
[first run](../../developer-guide/first-run/) for the full walkthrough.

| Subcommand | Purpose |
|---|---|
| `validate [name]` | Validate `--config` (or the default `tau.yaml`); `name` overrides the config's `name:`, which is used when it is omitted |
| `schema` | Print the JSON schema for the direct run config |
| `explain-config` | Print the direct run config field reference |
| `list` | List TauGrid-managed Jobs/RayJobs in a namespace |
| `status <job-name>` | Show lifecycle state and startup phases; `--watch` to poll |
| `logs <job-name>` | Compatibility path for `tau logs <job-name>` |
| `get <name>` | Fetch durable run results and artifacts |
| `cancel <job-name>` | Delete the underlying Job/RayJob and free its Kueue quota |
| `resume <name> --config tau.yaml` | Manually restart a failed run from its checkpoint |

There is no `tau run retry` subcommand; automatic retry is driven entirely
by the `resilience.*` fields in your run config. See
[recovery](../../platform-admin-guide/recovery/) for the full retry and resume
contract.

## `tau logs`

`tau logs <run-name>` searches the connected workspace first, then the locally
configured Tau workspace connections. It streams Ray driver logs or batch Job
pod logs without requiring Kubernetes context or namespace flags. Use
`--workspace`, or `--context` and `--namespace`, when the same run name exists
in more than one configured workspace. Use the native `--tail N` option instead
of piping through the shell's `tail`.

## `tau serve`

`deploy <name>`, `status <name>`, `scale <name>`, and `delete <name>` render
and manage a serving deployment from the same run-config-shaped intent as
`tau run`. `scale --kind=deployment` is implemented; RayService scaling must
currently be done by redeploying with `--replicas` or autoscaling flags.
See [serve a trained model](../../developer-guide/serve-model/).

## `tau data`

Groups the dataset and model registries so researchers have one place to
resolve durable inputs:

| Subcommand group | Purpose |
|---|---|
| `dataset list\|show\|ref\|alias\|verify\|register\|rm\|ingest\|status` | The curated dataset registry |
| `model list\|show\|best\|alias set\|alias get\|index rebuild` | Durable model checkpoints and aliases |

## `taugrid-portal experiment`

Run evidence and the Stellar dashboard ship in the `taugrid-portal` binary, not
in `tau`. The command tree is unchanged from when it lived under `tau
experiment`; only the binary name differs.

| Subcommand | Purpose |
|---|---|
| `init NAME` | Initialize an experiment store |
| `track RUN` | Record run metadata, configs, artifact pointers, and metrics |
| `list` | List runs, groups, or experiments (`--kind`) |
| `search` (alias `runs`) | Full-text and filtered run search |
| `status NAME` | Show a run's lifecycle status |
| `stellar NAME` (alias `dashboard`) | Render the local Stellar dashboard (`-o html\|json\|tui`) |
| `serve` | Serve Stellar over HTTP |
| `open NAME` | Serve and open a Stellar dashboard in the default browser |
| `compare NAME`, `plot NAME` | Compare or plot metrics across runs |
| `import jsonl` | Import JSONL scalar history into the store |

`taugrid-portal portal serve` serves the unified observability portal
(Experiments, Jobs/Queue, Cluster Health) from the same binary.

`taugrid-portal experiment` and the hidden compatibility alias `exp` share the
same command tree, so `--help` also surfaces `offload`, `kusto`, `autocapture`,
`capture`, `sql`, `export`, and `observe`, plus a nested `experiments
search|tag-run` group. Those are platform-internal and automation tools (metrics sidecar offload,
query-builder internals); this reference covers only the researcher-facing
commands.

Use built-in help for the exact installed release:

```bash
tau --help
tau run --help
tau logs --help
tau run status --help
```

The detailed current CLI guide is
[`cli/README.md`](https://github.com/Azure/taugrid/blob/main/cli/README.md).
