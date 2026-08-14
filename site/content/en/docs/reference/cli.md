---
title: CLI reference
weight: 1
description: Canonical Tau command families
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Tau has exactly seven public command roots. Every example in this site uses
one of these roots and its current nested subcommand path:

| Root | Purpose |
|---|---|
| [`tau cluster`](#tau-cluster) | Install/uninstall TauGrid and validate an existing cluster |
| [`tau workspace`](#tau-workspace) | Create/adopt workspaces and inspect readiness, quota, and repo scaffolds |
| [`tau run`](#tau-run) | Config-first submit, lifecycle, and retry/resume for one [run](../../concepts/glossary/#run) |
| [`tau serve`](#tau-serve) | Render and manage serving deployments |
| [`tau data`](#tau-data) | Inspect dataset and model registries |
| `tau python` | Proxy setup and authoring commands to the Python SDK |
| `tau version` | Print the installed CLI version |

Run evidence and the observability portal ship in a separate binary,
`taugrid-portal`. See [`taugrid-portal experiment`](#taugrid-portal-experiment)
below.

The old flat roots have been removed. Pre-v0.5 top-level aliases — ray, model,
dataset, exp, queue, status, logs, cancel, topology, lanes, storage, eval,
shell, submit, and finetune — are gone; their capabilities live under the seven
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
| `validate topology` | Check ResourceFlavor, node label, and IB readiness against a Kueue ClusterQueue or [topology](../../concepts/glossary/#topology) preset |

`validate installation`, `validate nodes`, and `validate topology` are
platform/operator diagnostics — run them against a cluster you administer, not
from inside a researcher project. Installation validation is read-only. Node
validation creates short-lived privileged probes and remains explicitly
opt-in. These commands do not replace per-run troubleshooting in
[recovery](../../operations/recovery/).

`install` does not provision AKS, mutate nodes, create PVCs, or configure Azure
identities. It validates Kubernetes 1.30+, the three controller Deployments,
TauCluster node readiness, the baseline ClusterQueue, and the narrow fail-closed
quota guard before reporting success. Readiness runs after Helm, so a failed
readiness report leaves the release installed for inspection; uninstall still
retains CRDs, custom resources, retained namespaces, storage, and workspace
data. Existing ArgoCD-managed clusters should retain independent component
Applications instead of installing the umbrella release.

The default install does not pass Helm's generic `--wait` or enable rollback.
Tau instead runs its bounded, component-aware readiness report after Helm
applies the release. Pass `--wait` to add Helm's generic resource watcher as an
earlier gate. Pass `--atomic` to roll back on a Helm failure; rollback also
enables Helm's watcher, and Tau translates this compatibility flag to Helm 4's
`--rollback-on-failure`. `--timeout` bounds each Helm operation and Tau's
readiness report.

The compiled chart default is the public
`oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid` OCI reference. The
requested version must be published. If it is unavailable, Tau reports the
exact chart and version and asks the operator to pass an accessible `--chart`
reference or local path.

`uninstall` runs in two Helm phases. Helm's own uninstall order deletes
Deployments before custom resources, so removing the release in one pass would
take the Kueue controller down while its ClusterQueue, ResourceFlavor, and
Topology still carry the `kueue.x-k8s.io/resource-in-use` finalizer — leaving
nothing able to clear it. The first phase therefore removes the queue policy
while Kueue is still running. Pass `--drain-queue=false` to skip it. Raising
`--timeout` is not a workaround for the stranded-finalizer case: no controller
remains to clear it, so the wait can only expire.

The final uninstall waits for resource removal by default; pass `--wait=false`
to return after Helm submits the deletions, and use `--timeout` to bound either
mode.

The drain re-renders the release, so it needs the same `--chart` reference the
release was installed from. Uninstall compares that reference against the chart
name Helm recorded for the release and skips the drain when they disagree,
rather than applying a different chart's manifests on the way out. When the
reference cannot be resolved, or the drained objects are still present
afterwards — Kueue holds `resource-in-use` until an active run finishes —
uninstall reports which objects may strand and proceeds; teardown still
completes. Draining is only possible through `tau`: Helm gives a chart no
control over the order its own custom resources are deleted, so a plain
`helm uninstall` of the TauGrid release still strands them.

On Helm 4, both verbs send `--force-conflicts` (paired with
`--server-side=true`) because AKS's `admissionsenforcer` addon co-owns
`.webhooks[*].namespaceSelector` on Kueue's webhook configurations. Helm 4
defaults to server-side apply, so without it every upgrade of the release —
including the uninstall drain — fails on those fields. Forcing does not
overwrite the AKS-managed selectors: the chart declares no value for them, so
the conflict is over field ownership rather than content. Helm 3 never sees
this and is sent neither flag. Do not delete the Kueue webhook configurations
to work around this conflict: recreating them can leave `caBundle` empty until
the Kueue controller restarts.

## `tau workspace`

Researcher-facing: `list`, `status <name>`, `check <name>`,
`connection inspect [path]`, `quota show <workspace>`,
`quota request <workspace>`, and `init-repo NAME` (scaffold a Tau-ready repo).
Platform-operator: `create [NAME]` (create the single v0 researcher workspace)
and `adopt NAME` (adopt an existing platform-provisioned namespace). Only
`quota request`, `create`, and `adopt` write to the cluster; each previews by
default and needs `--apply` to take effect. Workspace desired state is a native
`TauWorkspace` delivered through Helm/Kustomize/GitOps and reconciled by the
[TauWorkspace](../../concepts/glossary/#tauworkspace) controller.

`tau workspace create` requires `--principal-name <external-group-or-team>`;
`--subject-name` defaults to that principal when it is omitted.
`connection inspect` is an optional offline descriptor diagnostic; `tau run`
discovers and configures the checked-in connection automatically.

## `tau run`

`tau run [TARGET] [--config tau.yaml]` is the config-first entry point.
**`TARGET` is an optional positional argument to the `run` root itself, not a
subcommand** — `tau run train --dry-run=client` runs the `run` root with
`TARGET=train` and `--dry-run=client`, resolving `tau/train.yaml`. See
[run config](../../reference/run-config/) for the field reference and
[first run](../../tasks/researcher/first-run/) for the full walkthrough.

| Subcommand | Purpose |
|---|---|
| `validate [name]` | Validate `--config` (or the default `tau.yaml`); `name` overrides the config's `name:`, which is used when it is omitted |
| `schema` | Print the JSON schema for the direct run config |
| `explain-config` | Print the direct run config field reference |
| `submit-manifest` | Submit one previously captured direct `batch/v1` Job after verifying its exact-byte SHA-256 and fixed name/namespace |
| `list` | List Tau-managed Jobs/RayJobs in a namespace |
| `status [job-name]` | Show lifecycle state and startup phases; `--watch` to poll |
| `logs <job-name>` | Stream Ray driver logs or the batch Job pod logs |
| `get <name>` | Fetch durable run results and artifacts |
| `cancel <job-name>` | Delete the underlying Job/RayJob and free its Kueue quota |
| `resume <name> --config tau.yaml` | Manually restart a failed run from its checkpoint |

Safety-sensitive direct Jobs can bind admission review to submission without
rerendering:

```bash
tau run --config tau.yaml --dry-run=server \
  --manifest-out checked-job.yaml > admitted-job.yaml

tau run submit-manifest \
  --manifest checked-job.yaml \
  --digest sha256:<digest-printed-by-the-first-command> \
  --name <job-name> \
  --namespace <namespace>
```

The first command resolves cluster-owned queue and topology inputs, renders
once, sends those bytes to `kubectl create --dry-run=server -o yaml -f -`, and
writes the identical original bytes to `--manifest-out` only after admission
succeeds. Standard output is the API server's admitted/defaulted Job YAML;
stderr reports the exact-byte SHA-256 of the saved input. The admitted response
can differ from the input because admission webhooks and API defaulting may
mutate it.

`submit-manifest` does not load a config, profile, or environment and does not
render. It accepts only one Tau-managed `batch/v1 Job`, recomputes the saved
bytes' digest, requires the expected fixed name and namespace, and passes those
unchanged bytes to `kubectl create -f -`. A changed byte, digest, identity,
object kind, extra YAML document, or missing Tau submission identity fails
before kubectl runs. Keep `checked-job.yaml` private: direct manifests can
contain literal environment values.

There is no `tau run retry` subcommand — automatic retry is driven entirely
by the `resilience.*` fields in your run config. See
[recovery](../../operations/recovery/) for the full retry and resume
contract.

## `tau serve`

`deploy [name]`, `status [name]`, `scale [name]`, and `delete [name]` render
and manage a serving deployment from the same run-config-shaped intent as
`tau run`. `scale --kind=deployment` is implemented; RayService scaling must
currently be done by redeploying with `--replicas` or autoscaling flags.
See [serve a trained model](../../tasks/researcher/serve-model/).

## `tau data`

Groups the dataset and model registries so researchers have one place to
resolve durable inputs:

| Subcommand group | Purpose |
|---|---|
| `dataset list\|show\|ref\|alias\|verify\|register\|rm` | The curated dataset registry |
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
search|tag-run` group. Those are platform and agent-internal tooling (metrics
sidecar offload, query-builder internals) — they are not part of the everyday
researcher loop and are intentionally not documented here.

Use built-in help for the exact installed release:

```bash
tau --help
tau run --help
tau run status --help
```

The detailed current CLI guide is
[`cli/README.md`](https://github.com/Azure/taugrid/blob/main/cli/README.md).
