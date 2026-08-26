---
title: Developer guide
linkTitle: Developer guide
weight: 2
description: Run, observe, recover, and compare experiments from an existing TauGrid workspace
sidebar_root_for: self
aliases:
  - "/docs/tasks/researcher/"
---

This guide is for developers and researchers who already have access to a
TauGrid workspace. If a platform owner has not yet provisioned one, start
with the [Platform admin guide](../platform-admin-guide/) instead, or ask
your platform team for a workspace connection.

## Get started

- Confirm your workspace connection with your platform team, then
  [run your first target](first-run/).
- [Serve a trained model](serve-model/) once you have a run to promote.

## Concepts

- [Repository, project, workspace, target, and run](concepts/object-model/)
- [Workspaces and multi-workspace lifecycle](concepts/workspaces/)
- [Run and workload lifecycle](concepts/lifecycle/)
- [Configuration resolution](concepts/config-resolution/)
- [Experiment evidence and artifacts](concepts/evidence/)

## Related

- Recover a failed run with
  [retry and resume](../platform-admin-guide/recovery/).
- Look up exact contracts in the [Reference](../reference/) section, such as
  the [CLI reference](../reference/cli/) and
  [run configuration reference](../reference/run-config/).
- Browse runnable [Examples](../examples/).
