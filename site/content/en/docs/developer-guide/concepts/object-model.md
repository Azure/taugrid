---
title: TauGrid object model
weight: 1
description: How repository, project, workspace, target, run, workload, and experiment relate
aliases:
  - "/docs/concepts/object-model/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Full definitions for these terms live in the [glossary](../../../reference/glossary/). This
page only explains how they relate and who owns what.

| Concept | Owns |
|---|---|
| [Repository](../../../reference/glossary/#repository) | Code and history in one Git worktree |
| [Project](../../../reference/glossary/#project) | One or more targets and a workspace connection |
| [Workspace](../../../reference/glossary/#workspace) | Destination policy: cluster, namespace, queue, priority, output root, identity |
| [Target](../../../reference/glossary/#target) | One checked-in runnable config |
| [Run](../../../reference/glossary/#run) | One execution plus its lifecycle handle |
| [Workload](../../../reference/glossary/#workload) | Rendered Job or RayJob execution intent |
| [Service](../../../reference/glossary/#service) | Online RayService or Deployment lifecycle target |
| [Experiment](../../../reference/glossary/#experiment) | A comparison set over runs, metrics, and artifacts |

The ownership split is deliberate:

- Projects own code, images, runtime dependencies, resource intent, datasets,
  and artifacts.
- Workspaces own destination policy and shared platform defaults; projects
  keep owning their own code and workload shape. The
  [TauWorkspace](../../../reference/glossary/#tauworkspace) Kubernetes resource backs this
  policy, while platform teams provision the underlying Azure resources.
- TauGrid owns deterministic resolution and lifecycle handoff.
