---
title: Examples
linkTitle: Examples
weight: 25
description: Choose a runnable Tau, Kueue, Ray, or AKS example by learning goal and prerequisite cost.
---

{{< maturity status="ga" reviewed="2026-08-11" >}}

Choose an example by the question you want to answer:

| Goal | Example | Interface | Compute | Start here |
|---|---|---|---|---|
| Run repository-first GPU HPO and verify six Tune trials | Ray Tune smoke on AKS | Tau-first | One or more NVIDIA GPUs | [Run GPU Ray Tune HPO](gpu-ray-tune/) |
| See queue admission and borrowing without GPU quota | CPU queueing | Raw KubeRay and Kueue YAML | CPU | [Explore CPU queueing](cpu-queueing/) |
| Build a complete AKS, Kueue, and Ray baseline | Modular cluster deployment | Terraform, Helm, and kubectl | CPU or A100 GPU | [Provision the platform baseline](full-cluster/) |

## Read the interface label

**Tau-first** examples demonstrate the researcher interface this site
recommends. **Raw KubeRay and Kueue** examples expose the Kubernetes objects
beneath Tau and are useful for platform education or debugging. Do not treat
their `kubectl apply` commands as the preferred researcher workflow.

The `anyscale-comparison` directory is intentionally not promoted here. It is a
vendor comparison with known external-resource defaults, not a supported Tau
workflow.
