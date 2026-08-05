---
title: Tau object model
weight: 1
description: How repository, project, workspace, target, run, workload, and experiment relate
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Full definitions for these terms live in the [glossary](../glossary/). This
page only explains how they relate and who owns what.

| Concept | Owns |
|---|---|
| [Repository](../glossary/#repository) | Code and history in one Git worktree |
| [Project](../glossary/#project) | One or more targets and a workspace connection |
| [Workspace](../glossary/#workspace) | Destination policy: cluster, namespace, queue, priority, output root, identity |
| [Target](../glossary/#target) | One checked-in runnable config |
| [Run](../glossary/#run) | One execution plus its lifecycle handle |
| [Workload](../glossary/#workload) | Rendered Job or RayJob execution intent |
| [Service](../glossary/#service) | Online RayService or Deployment lifecycle target |
| [Experiment](../glossary/#experiment) | A comparison set over runs, metrics, and artifacts |

The ownership split is deliberate:

- Projects own code, images, runtime dependencies, resource intent, datasets,
  and artifacts.
- Workspaces own destination policy and shared platform defaults -- not
  project code or workload shape. The
  [TauWorkspace](../glossary/#tauworkspace) Kubernetes resource backs this
  policy; it does not provision the underlying Azure resources.
- Tau owns deterministic resolution and lifecycle handoff.
