---
title: Glossary
weight: 7
description: Canonical TauGrid terms, so overlapping words mean one thing everywhere
aliases:
  - "/docs/concepts/glossary/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

This page is the canonical source for TauGrid terminology. Other concept and
reference pages link to a definition here instead of redefining it. Where a
term below conflicts with older prose elsewhere, this definition wins.

## TauGrid {#tau}

The CLI (and its companion Python SDK) that resolves a project's checked-in
workload contract, applies platform-owned policy, renders a Kubernetes `Job`
or KubeRay `RayJob`, submits it, and gives the researcher one lifecycle
surface for status, logs, results, retry, and resume. TauGrid resolves, renders,
and observes workload lifecycle; Kueue and Kubernetes own scheduling and
orchestration, and platform teams provision cloud infrastructure. See
[What is TauGrid](../../getting-started/what-is-tau/) and
[Architecture](../../getting-started/architecture/).

## Repository / research repository {#repository}

One Git worktree containing a TauGrid-enabled research project, or a monorepo
catalog of several. The repository owns entrypoint code, images, runtime
dependencies, and data/output contracts; TauGrid only resolves and renders
against it.

## Project {#project}

The unit inside a repository that owns one or more [targets](#target) and
exactly one [workspace connection descriptor](#workspace-connection) at a
time. A monorepo can hold multiple projects.

## Target {#target}

A checked-in, named, runnable config -- for example `tau/train.yaml`, or a
[direct run config](#run-config-vs-manifest) passed with
`tau run --config`. "Target" means "which checked-in workload to run." It is
unrelated to [cluster context](#cluster-context); keep the two distinct.

## Cluster context {#cluster-context}

The kubectl context TauGrid operates against, selected with `--context` or the
`TAU_CONTEXT` environment variable. Cluster context answers "which
cluster," never "which checked-in workload." See [target](#target) for that
meaning.

## Workspace / workspace environment {#workspace}

Shorthand for the platform-owned destination and shared defaults a project
resolves against: cluster, namespace, queue, priority, output root, and identity.
This shorthand is safe when the surrounding text describes policy rather than
a specific object. When precision matters, say
[TauWorkspace](#tauworkspace) for the Kubernetes resource, or
[workspace connection descriptor](#workspace-connection) for the client-side
file a repository holds.

## TauWorkspace {#tauworkspace}

The Kubernetes custom resource (`tau.azure.com/v1alpha1`, kind
`TauWorkspace`) that the Tau workspace controller reconciles in the
`tau-system` namespace. It is a **Kubernetes API for onboarding, policy intent, and status**: its spec declares target namespace, Kueue queue, and defaults
for workload identity, output root, and priority; its controller reconciles or
verifies namespace/RBAC, Kueue LocalQueue accessibility, and workload-identity
ServiceAccount configuration, then reports
[status conditions and phase](#status-condition). The spec has no storage field:
durable storage is external platform desired state.

TauWorkspace is a Kubernetes onboarding and policy intent and status API
scoped to namespace, queue, and workload-identity wiring. The platform
operator and Azure/provider tooling own the AKS cluster, network, node
pools, Kueue/KubeRay installation, and cloud RBAC outside TauWorkspace. See
[TauWorkspace reference](../workspace/) and
[Identity and security boundaries](../../platform-admin-guide/identity/).

## Workspace connection descriptor {#workspace-connection}

The non-secret `tau/workspace.connection.yaml` file a platform operator hands to a repository once its TauWorkspace is [Ready](#status-condition). It names the Kubernetes context, access method, TauGrid system namespace, and workspace contract a project resolves against and must never contain a credential or kubeconfig. This file is client-side project configuration only: the TauWorkspace object it describes is reconciled independently by the controller and is unaffected by edits to this file. `cluster.systemNamespace` defaults to `tau-system`.

Checking in the descriptor is explicit repository/platform preconfiguration.
`tau run` discovers it automatically; `tau workspace connection` verifies and
pins the configured access before the first run, while `--offline` validates
only the repository configuration. On first cluster-backed use, TauGrid isolates the
named context from the user's kubeconfig or obtains AKS cluster-user credentials,
verifies the live workspace contract, and records a durable configuration pin
separately from short-lived readiness evidence.
An unchanged pinned connection can refresh Ready, LocalQueue, and authorization
checks noninteractively after the readiness cache expires. Descriptor, trust,
or live workspace-contract drift requires interactive review.

`workspace-rbac` is the API default and what `tau workspace create` writes. In
that mode the controller binds the researcher subject in the workspace
namespace. `cluster-wide` is an explicit opt-out: the workspace supplies only
routing and policy defaults, and the platform grants researcher access
separately, which is how some existing clusters are configured. The
[multiple-workspace lifecycle](../../developer-guide/concepts/workspaces/#multiple-workspaces) is Alpha: v0
activates one workspace and blocks additional workspace objects until the active
workspace is removed. Researcher isolation still requires its negative-access
gate.

## Status condition / Ready {#status-condition}

A `TauWorkspace` reports Kubernetes-style status conditions and an overall
`status.phase` of `Pending`, `Ready`, or `Degraded`. "Ready" currently means
`RBACReady` and `QueueReady` are true and no drift is detected.
`WorkloadIdentityReady` is diagnostic; the overall phase gate currently
excludes it. TauGrid 0.1 has no `StorageReady` condition on `TauWorkspace` or
`TauCluster`, so Ready confirms only the conditions above: a
platform-managed durable PVC may still be missing or unbound, Azure
infrastructure may be older than the current reconcile, and researcher-scoped
isolation needs its own check; see
[Identity and security boundaries](../../platform-admin-guide/identity/).

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
any required or preferred node topology for pod placement. TauGrid resolves
topology intent; Kueue and Kubernetes still own admission and scheduling.
See [Queue, quota, topology, and GPU placement](../../platform-admin-guide/policy-and-placement/).

## Queue / LocalQueue / ClusterQueue {#queue}

Kueue objects, not TauGrid objects. A `LocalQueue` is the tenant-facing entry
point a workload references. A `ClusterQueue` owns shared quota and
fairness across the LocalQueues bound to it. TauGrid resolves which LocalQueue
a run should target; Kueue decides admission. See
[Queue, quota, topology, and GPU placement](../../platform-admin-guide/policy-and-placement/).

## Direct run config vs. managed workflow manifest {#run-config-vs-manifest}

The normal researcher contract is a **direct run config**: a checked-in
`tau run --config` YAML file (`name`, `engine`, `compute`, `storage`, and so
on) that TauGrid validates and renders directly. Its conventional filename is
`tau.yaml`.

A **managed workflow manifest** carries `schema_version: 1` and is normally
generated by the Python SDK for staged train/eval or renderer-level workflows.
It may also be named `tau.yaml`, so the schema, not the filename, identifies
the format. It is machine-generated output rather than a hand-authored
default a researcher writes directly. See
[Configuration resolution](../../developer-guide/concepts/config-resolution/) and the
[run config reference](../run-config/).

## Run {#run}

One execution of a [target](#target) plus its lifecycle handle -- the
object `tau run status`, `tau logs`, `tau run get`,
`tau run cancel`, and `tau run resume` operate on.

## Workload (Job / RayJob) {#workload}

The rendered Kubernetes object TauGrid submits: a `batch/v1` `Job` for
single-pod work, or a KubeRay `RayJob` for multi-node Ray execution. The
workload is what Kueue admits and Kubernetes schedules: a run's rendered
execution artifact, downstream of the run's own lifecycle handle.

## Service / endpoint {#service}

An online lifecycle target rendered by `tau serve` as either a KubeRay
`RayService` or a Kubernetes `Deployment`. A service consumes a project-owned
image and optional durable checkpoint, and it keeps its own lifecycle surface
via `tau serve status` and `tau serve delete`, separate from run lifecycle
commands.

## Experiment {#experiment}

A comparison set over [runs](#run), their metrics, and their artifacts,
scoped by `experiment.project`. Two fields define that identity inside a direct
run config's `experiment` block:

- **name** -- the stable experiment identifier (`experiment.name`).
- **group** -- a named arm of runs within that experiment
  (`experiment.group`).

## Evidence / metrics / artifacts / checkpoints {#evidence}

Files and records saved by a run: metric history, summaries, checkpoints,
model outputs, images, tables, reports, and retry or resume state. TauGrid
keeps the durable copy with the experiment. The TauGrid Portal and optional
ADX/Kusto dashboards provide additional ways to view and compare it. See
[Experiment evidence and artifacts](../../developer-guide/concepts/evidence/).
