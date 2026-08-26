---
title: Architecture
weight: 2
description: How TauGrid connects repository intent to Kubernetes execution
aliases:
  - "/docs/overview/architecture/"
---

TauGrid is a CLI, renderer, and local observer. Kueue, KubeRay, and the
Kubernetes scheduler continue to own quota admission, Ray orchestration, and
pod scheduling.

| Layer | Responsibility |
|---|---|
| Research repository | Entrypoint, image, dependencies, resource intent, data, and outputs |
| TauGrid | Resolve, validate, render, submit, observe, and preserve workflow evidence |
| Workspace/platform | Cluster access, namespace, queue, priority, output root, identity, and shared policy |
| Kueue | Quota reservation, admission, priority, and preemption |
| Kubernetes/KubeRay | Pod scheduling and Job/Ray lifecycle |
| Project process | Training, evaluation, inference, and model-specific behavior |

## Code boundaries

- `cli/cmd`: thin Cobra command wiring for the `tau` binary.
- `cli/internal`: reusable capability packages behind `tau`.
- `core`: the shared library module both binaries link.
- `portal`: a separate module and binary, `taugrid-portal`, that hosts
  experiment tracking (Stellar) and the observability portal.
- `sdk/python`: optional Python authoring APIs that delegate execution
  to the Go CLI.
- `controllers/tau-core`: the separately deployed controller. One manager runs
  three reconcilers -- `TauCluster`, `TauWorkspace`, and `TauQuotaRequest`.

The [visual architecture guide](https://github.com/Azure/taugrid/wiki/Tau-Architecture-Contributing-and-Roadmap)
maps the main packages and extension points.
