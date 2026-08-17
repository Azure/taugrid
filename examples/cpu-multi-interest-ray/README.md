# CPU multi-interest Ray config

> **Status:** `smoke/debug`
> **Intended use:** CPU-only Ray config demo for the multi-interest retrieval toy
> workload.
> **Not for:** production training or GPU onboarding.

This folder keeps a config-first CPU Ray example beside the GPU Tau examples.
Run it with `tau run --config tau.yaml`. The workload trains a tiny synthetic
multi-interest retrieval model with Ray Train and no GPUs, but it expects an
existing Ray environment where `ray.init(address="auto")` can connect.
