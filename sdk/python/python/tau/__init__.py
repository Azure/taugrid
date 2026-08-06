"""tau: Python-first authoring SDK for the Tau Go CLI.

Researchers write a Python file with a `@tau.train(...)`-decorated
function and call `.submit()`. Tau Python SDK synthesizes the manifest,
generates a self-contained cluster wrapper, writes a generated Tau config, and
shells out to the tau Go CLI with `tau run --config`. No YAML touches the
researcher's hands.

The Python package is intentionally its own SDK: it owns decorators, local
execution, config composition, staged entrypoints, and experiment-logging
ergonomics. The Go `tau` CLI remains the canonical executor for Kubernetes,
Kueue, Ray, serving, expstore, and telemetry contracts. Use `tau python ...`
through the Go CLI for normal use, or `python -m tau.cli ...` for local SDK
debugging.

Eval workloads use `@tau.eval(...)` (a separate decorator) which dispatches
to the `rayjob-eval` workload kind: a CPU-only system head, 1 GPU worker for
a Ray actor, and N CPU worker pods for ray.remote fanout tasks.

Training uses `@tau.train`, eval uses `@tau.eval`, and online inference uses
`tau.serve(...)`. Project-owned YAML/TOML can be loaded with
`tau.config(...)` and passed to the decorators so CLI/env stay overrides.
Both pretraining, finetuning, and post-training all map to
`@tau.train` — they're the same Kubernetes shape (gang-scheduled GPU pods
running a torch entrypoint); only the data and the optimizer differ.
`@tau.eval` exists separately because the eval shape is structurally different
(1 GPU + many CPU workers, eval Kueue queue, different priority class).
`tau.serve(...)` delegates online inference lookup/rendering to the Go CLI for
RayService/Deployment serving endpoints.
"""

__version__ = "0.1.3"

from tau.config import (
    SecretRef,
    SecretSource,
    config,
    load_config,
    pvc_mount,
    secret,
    secret_from_env,
    secret_from_file,
    secret_ref,
)
from tau.datasets import dataset_file_reference
from tau.entrypoints import call_staged_function, load_staged_function, load_staged_module
from tau.workloads import Ctx, eval, train
from tau.jsonl import read_jsonl_objects
from tau.serve import ServeHandle, serve
from tau import stellar, workloads

__all__ = [
    "train",
    "eval",
    "serve",
    "Ctx",
    "ServeHandle",
    "SecretRef",
    "SecretSource",
    "call_staged_function",
    "config",
    "dataset_file_reference",
    "load_config",
    "load_staged_function",
    "load_staged_module",
    "pvc_mount",
    "read_jsonl_objects",
    "secret",
    "secret_from_env",
    "secret_from_file",
    "secret_ref",
    "stellar",
    "workloads",
]
