---
title: Glossary
weight: 7
description: Canonical Tau terms, so overlapping words mean one thing everywhere
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

This page is the canonical source for Tau terminology. Other concept and
reference pages link to a definition here instead of redefining it. Where a
term below conflicts with older prose elsewhere, this definition wins.

## Tau {#tau}

The CLI (and its companion Python SDK) that resolves a project's checked-in
workload contract, applies platform-owned policy, renders a Kubernetes `Job`
or KubeRay `RayJob`, submits it, and gives the researcher one lifecycle
surface for status, logs, results, retry, and resume. Tau is not a
scheduler, not a Kubernetes operator, and does not provision cloud
infrastructure. See [What is Tau](../../overview/what-is-tau/) and
[Architecture](../../overview/architecture/).

## Repository / research repository {#repository}

One Git worktree containing a Tau-enabled research project, or a monorepo
catalog of several. The repository owns entrypoint code, images, runtime
dependencies, and data/output contracts; Tau only resolves and renders
against it.

## Project {#project}

The unit inside a repository that owns one or more [targets](#target) and
exactly one [workspace connection descriptor](#workspace-connection) at a
time. A monorepo can hold multiple projects.

## Target {#target}

A checked-in, named, runnable config -- for example `tau/train.yaml`, or a
[direct run config](#run-config-vs-manifest) passed with
`tau run --config`. "Target" means "which checked-in workload to run." It is
unrelated to [cluster context](#cluster-context); do not conflate the two.

## Cluster context {#cluster-context}

The kubectl context Tau operates against, selected with `--context` or the
`TAU_CONTEXT` environment variable. Cluster context answers "which
cluster," never "which checked-in workload." See [target](#target) for that
meaning.

## Workspace / workspace environment {#workspace}

Shorthand for the platform-owned destination and shared defaults a project
resolves against: cluster, namespace, queue, priority, output root, and identity.
This shorthand is safe when the surrounding text is describing policy, not a
specific object. When precision matters, say
[TauWorkspace](#tauworkspace) for the Kubernetes resource, or
[workspace connection descriptor](#workspace-connection) for the client-side
file a repository holds.

## TauWorkspace {#tauworkspace}

The Kubernetes custom resource (`tau.azure.com/v1alpha1`, kind
`TauWorkspace`) that the Tau workspace controller reconciles in the
`tau-system` namespace. It is a **Kubernetes onboarding and policy intent
and status API**: its spec declares target namespace, Kueue queue, and defaults
for workload identity, output root, and priority; its controller reconciles or
verifies namespace/RBAC, Kueue LocalQueue accessibility, and workload-identity
ServiceAccount configuration, then reports
[status conditions and phase](#status-condition). The spec has no storage field:
durable storage is external platform desired state.

TauWorkspace does **not** provision Azure resources, create an AKS cluster,
grant Azure RBAC, or replace Kueue or KubeRay. Cluster, network, node pool,
Kueue/KubeRay installation, and cloud RBAC remain owned by the platform
operator and Azure/provider tooling outside TauWorkspace. See
[TauWorkspace reference](../../reference/workspace/) and
[Identity and security boundaries](../identity/).

## Workspace connection descriptor {#workspace-connection}

The non-secret `tau/workspace.connection.yaml` file a platform operator hands to a repository once its TauWorkspace is [Ready](#status-condition). It names the exact cluster, TauGrid system namespace, and workspace contract a project resolves against and must never contain a credential or kubeconfig. This file is client-side project configuration: it is not the TauWorkspace object, and writing it does not create or modify one. New descriptors always write `cluster.systemNamespace`, which is `tau-system` for a default installation. Descriptors created before that field existed remain compatible and resolve an omitted field to the legacy `tau-platform` namespace.

Checking in the descriptor is explicit repository/platform preconfiguration.
`tau run` discovers it automatically; `tau workspace connection inspect` is an
optional offline diagnostic. On first cluster-backed use, Tau obtains normal
AKS cluster-user credentials, verifies the live workspace contract, and records
a durable configuration pin separately from short-lived readiness evidence.
An unchanged pinned connection can refresh Ready, LocalQueue, and authorization
checks noninteractively after the readiness cache expires. Descriptor, trust,
or live workspace-contract drift requires interactive review.

`workspace-rbac` is the API default and what `tau workspace create` writes. In
that mode the controller binds the researcher subject in the workspace
namespace. `cluster-wide` is an explicit opt-out: the controller grants no
researcher access and the workspace supplies only routing and policy defaults,
which is how some existing clusters are configured. The
[multiple-workspace lifecycle](../workspaces/#multiple-workspaces) is Alpha: v0
activates one workspace and blocks additional workspace objects until the active
workspace is removed. Researcher isolation still requires its negative-access
gate.

## Status condition / Ready {#status-condition}

A `TauWorkspace` reports Kubernetes-style status conditions and an overall
`status.phase` of `Pending`, `Ready`, or `Degraded`. "Ready" currently means
`RBACReady` and `QueueReady` are true and no drift is detected.
`WorkloadIdentityReady` is diagnostic and does not gate the overall phase.
TauGrid 0.1 has no `StorageReady` condition on `TauWorkspace` or `TauCluster`,
so Ready does not prove a platform-managed durable PVC exists or is bound.
Ready also does not mean Azure infrastructure was just created or that
researcher-scoped isolation is active; see
[Identity and security boundaries](../identity/).

## Profile / resource profile {#profile}

The render-time resource contract (name, lane, and spec) a workload builder
consumes to size compute -- GPU, CPU, and memory intent -- for a run. A
profile describes shape only. It is narrower than a
[topology preset](#topology), which additionally decides queue, priority,
and placement routing.

## Topology and placement {#topology}

The platform-owned mapping from a researcher-facing preset (for example
`azure.research.training.l`) to Kueue-facing queue, priority, and topology
metadata: which [queue](#queue) admits the workload, its priority class, and
any required or preferred node topology for pod placement. Tau resolves
topology intent; Kueue and Kubernetes still own admission and scheduling.
See [Queue, quota, topology, and GPU placement](../policy-and-placement/).

## Queue / LocalQueue / ClusterQueue {#queue}

Kueue objects, not Tau objects. A `LocalQueue` is the tenant-facing entry
point a workload references. A `ClusterQueue` owns shared quota and
fairness across the LocalQueues bound to it. Tau resolves which LocalQueue
a run should target; Kueue decides admission. See
[Queue, quota, topology, and GPU placement](../policy-and-placement/).

## Direct run config vs. managed workflow manifest {#run-config-vs-manifest}

The normal researcher contract is a **direct run config**: a checked-in
`tau run --config` YAML file (`name`, `engine`, `compute`, `storage`, and so
on) that Tau validates and renders directly. Its conventional filename is
`tau.yaml`.

A **managed workflow manifest** carries `schema_version: 1` and is normally
generated by the Python SDK for staged train/eval or renderer-level workflows.
It may also be named `tau.yaml`, so the filename does not identify the format;
the schema does. It is not a competing hand-authored default. See
[Configuration resolution](../config-resolution/) and the
[run config reference](../../reference/run-config/).

## Run {#run}

One execution of a [target](#target) plus its lifecycle handle -- the
object `tau run status`, `tau run logs`, `tau run get`,
`tau run cancel`, and `tau run resume` operate on.

## Workload (Job / RayJob) {#workload}

The rendered Kubernetes object Tau submits: a `batch/v1` `Job` for
single-pod work, or a KubeRay `RayJob` for multi-node Ray execution. The
workload is what Kueue admits and Kubernetes schedules. It is downstream of
a run, not the run itself.

## Service / endpoint {#service}

An online lifecycle target rendered by `tau serve` as either a KubeRay
`RayService` or a Kubernetes `Deployment`. A service consumes a project-owned
image and optional durable checkpoint, but it is not a [run](#run) and does not
inherit run lifecycle commands. Use `tau serve status` and `tau serve delete`
for its lifecycle.

## Experiment {#experiment}

A comparison set over [runs](#run), their metrics, and their artifacts,
scoped by `experiment.project`. Two fields define that identity inside a direct
run config's `experiment` block:

- **name** -- the stable experiment identifier (`experiment.name`).
- **group** -- a named arm of runs within that experiment
  (`experiment.group`).

## Evidence / metrics / artifacts / checkpoints {#evidence}

Durable run output that Tau's local-first expstore treats as part of the
workflow contract: metric history and summaries, checkpoints and model
outputs, images/tables/reports, and retry/resume/terminal-state records.
ADX/Kusto and the hosted Stellar UI are optional projections of scalar
metrics for fleet queries -- consumers of evidence, not the only durable
copy. See [Experiment evidence and artifacts](../evidence/).
