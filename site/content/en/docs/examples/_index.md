---
title: Examples
linkTitle: Examples
weight: 4
description: Choose a runnable TauGrid, Kueue, Ray, or AKS example by learning goal and prerequisite cost.
sidebar_root_for: self
---

Choose an example by the question you want to answer:

| Goal | Example | Interface | Compute | Start here |
|---|---|---|---|---|
| Run repository-first GPU HPO and verify six Tune trials | Ray Tune smoke on AKS | TauGrid-first | One or more NVIDIA GPUs | [Run GPU Ray Tune HPO](gpu-ray-tune/) |
| Publish live loss and accuracy evidence, retrieve durable files, and open Stellar | Experiment evidence | TauGrid-first | One NVIDIA GPU | [Run live experiment evidence](experiment-evidence/) |
| See queue admission and borrowing using CPU-only quota | CPU queueing | Raw KubeRay and Kueue YAML | CPU | [Explore CPU queueing](cpu-queueing/) |
| Build a complete AKS, Kueue, and Ray baseline | Modular cluster deployment | Terraform, Helm, and kubectl | CPU or A100 GPU | [Provision the platform baseline](full-cluster/) |

## Read the interface label

**TauGrid-first** examples demonstrate the researcher interface this site
recommends and remains the preferred researcher workflow. **Raw KubeRay and Kueue** examples expose the Kubernetes objects
beneath TauGrid and are useful for platform education or debugging.

The `anyscale-comparison` directory serves as a vendor-comparison reference
with known external-resource defaults; it lives outside this table and the
supported TauGrid workflow set.
