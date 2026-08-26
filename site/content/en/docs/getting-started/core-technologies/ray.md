---
title: What is Ray?
linkTitle: What is Ray?
weight: 1
description: How Ray runs distributed AI workloads in TauGrid
---

[Ray](https://www.ray.io/) is an open-source framework for running Python work
across processes and machines. Teams use it for distributed training,
hyperparameter tuning, data processing, and model serving.

[KubeRay](https://ray-project.github.io/kuberay/) brings Ray to Kubernetes. Its
operator manages resources such as `RayJob` and `RayService`, creates the Ray
head and worker pods, and follows the Ray workload lifecycle.

## How TauGrid uses Ray

TauGrid turns a repository target into either a Kubernetes `Job` or a KubeRay
`RayJob`. A Ray workload follows this path:

1. `tau run` validates the target and creates a `RayJob`.
2. Kueue waits for the required quota.
3. Kubernetes places the Ray head and worker pods.
4. Ray coordinates the application across those pods.
5. TauGrid reports status, streams driver logs, and retrieves saved outputs.

Ray owns application execution and communication between workers. TauGrid owns
the submission and run experience around it.

## When Ray is useful

Use Ray when a workload needs multiple workers, Ray Train, Ray Tune, or Ray
Serve. A simple single-container task can run as a standard Kubernetes `Job`.

The Ray dashboard shows live cluster and task state while the Ray head is
running. Stellar serves a different purpose: it shows saved experiment metrics
and comparisons after the workload produces evidence.

Try [GPU Ray Tune HPO](../../../examples/gpu-ray-tune/) for a complete example.
