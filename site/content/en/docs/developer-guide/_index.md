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

Your platform team should give you access to a research repository containing
the non-secret `tau/workspace.connection.yaml` descriptor. You do not need to
configure or inspect it before running a target: `tau run` discovers it
automatically, obtains the required cluster credentials, and verifies the live
workspace contract. The descriptor selects a Kubernetes context and declares
how Tau should access it. Most clusters use an existing kubeconfig context;
AKS platforms can instead include optional Azure metadata so Tau obtains
cluster-user credentials automatically. These are platform-supplied connection
settings, not values researchers should copy from `kubectl` output or edit
during onboarding. Kubeconfig remains the generic adapter for any conformant
Kubernetes cluster and supports standard exec credential plugins; AKS is the
only managed-cloud adapter with first-class onboarding in this release.

From the repository, run `tau workspace connection` to verify the complete
read-only connection before submitting work. Use `--offline` when only local
repository validation is appropriate. For AKS, see
[Connect to an AKS workspace](aks-access/) for the clean-machine sign-in flow
and the relationship between `az login`, Tau, and `kubelogin`.

The examples in the TauGrid source repository, including
`examples/aks-cpu-quickstart`, `examples/kind-smoke`, and
`examples/ray-tune-smoke`, contain reusable workload configurations but do not
contain a workspace connection. Kubernetes contexts, workspace names, and any
provider-specific access metadata are environment-specific, so TauGrid does not
commit them to the public examples. Copy an example into a Tau-enabled research
repository, or add the descriptor supplied by the platform team, before
submitting it.

Continue with [run your first target](first-run/).

- [Connect to an AKS workspace](aks-access/).
- [Share research with a teammate](share-research/).
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
