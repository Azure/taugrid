---
title: Capability maturity
weight: 3
description: Alpha, Beta, GA, Deprecated, and Planned Tau capabilities
---

TauGrid uses Kubernetes-style feature stages. A stage applies to a capability,
not to the entire release.

| Stage | Availability and support | Recommended use |
|---|---|---|
| **Alpha** | Early implementation with limited validation. It may require explicit configuration. APIs and behavior can change or be removed without a deprecation period. | Testing and feedback only. |
| **Beta** | Tested in supported environments. Behavior is expected to remain available, but details can change before GA. | Evaluation and selected workloads that can accept change. |
| **GA** | Supported capability covered by release validation, compatibility requirements, and the deprecation policy. | Production use within the documented platform matrix. |
| **Deprecated** | Still available during a documented migration period. A replacement and removal target must be provided. | Existing users should migrate. Do not start new use. |
| **Planned** | Design or roadmap work without an available user contract. Planned is not a feature stage. | Do not depend on it. |

Feature-state banners appear only when a page's primary subject is a capability.
Navigation, overview, and policy pages do not have a feature stage. A page can
include an inline banner for a lower-maturity subsection.

## Promotion requirements

- **Alpha to Beta:** end-to-end validation, documented enablement and rollback,
  a supported environment matrix, and no unresolved security or authorization
  contract.
- **Beta to GA:** production validation, compatibility tests, operational
  diagnostics, upgrade and recovery coverage, and complete user documentation.
- **GA to Deprecated:** a replacement or removal reason, migration instructions,
  and a documented removal target.

## Current capability stages

### GA

- Repository and project resolution.
- Config-first Job and RayJob submission.
- Workspace connection and readiness checks.
- Status, logs, results, cancellation, retry, and resume.
- Local experiment evidence, artifacts, and profiling.
- Cluster and topology validation.

### Beta

No capabilities are currently labeled Beta.

### Alpha

- Dataset registry and selected production data-plane integrations.
- Multiple-workspace lifecycle. One workspace is active; additional workspace
  objects remain blocked until the active workspace is removed.
- MultiKueue preflight and constrained worker dispatch.
- Hosted Stellar and selected provider-specific rollout surfaces.

### Deprecated

No capabilities are currently labeled Deprecated.

### Planned

- Researcher isolation pending negative-access release gates.
- A general released model-serving quickstart.
- Automatic dataset replication.
- Unrestricted any-worker multi-cluster routing.

A design document or partial implementation does not change a capability's
feature stage.
