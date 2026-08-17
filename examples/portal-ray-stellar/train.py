# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""RayJob driver that lights BOTH portal detail-page links at once.

Goal: submit a single ``tau run --config tau.yaml`` job whose portal detail
page shows both the "Ray dashboard" link AND the "Open in Experiments"
(Stellar) link.

- **Ray dashboard link**: satisfied because this runs as a RayJob, which
  creates a RayCluster with a head Service — exactly what the portal keys the
  Ray dashboard button off of. The link remains briefly during post-completion
  cleanup; it is not durable run history.
- **Stellar link**: satisfied because worker 0 publishes immutable
  ``metrics-history-attempt-0/*.jsonl`` chunks (``_step`` + ``_timestamp`` +
  scalar fields) under ``$TAU_OUTPUT_DIR``. The Tau metrics-offload sidecar
  watches those chunks,
  remote-writes rows to adx-mon/Kusto (``ExperimentMetrics``), and — critically
  — publishes a terminal ``tau/run_status`` marker on shutdown. The portal only
  lights the Stellar link once that terminal marker lands.

The metrics file schema is fixed by the online offloader's validation
(``validateOnlineMetricsChunk`` in ``taugrid-portal/internal/cli/exp_offload.go``):
each line needs a ``_step`` (JSON integer) field and a ``_timestamp`` field that
is a JSON **number** holding positive Unix epoch *seconds* — an RFC3339 string
is rejected with ``_timestamp must be numeric`` and the sidecar then imports
nothing and never publishes the terminal marker. Any other numeric keys become
tracked scalar metrics.

Entrypoint contract: ``engine: ray`` renders a bare ``python3 train.py``
entrypoint. This bounded acceptance driver uses Ray Core tasks so the canonical
CPU Ray image needs no undeclared framework dependency.
"""

import json
import os
import time
from pathlib import Path

import ray
from ray.util.placement_group import placement_group, remove_placement_group
from ray.util.scheduling_strategies import PlacementGroupSchedulingStrategy


def _metrics_dir(output_dir: str | None = None) -> Path:
    """Resolve the directory containing immutable JSONL chunks.

    ``TAU_OUTPUT_DIR`` is injected by Tau and points at the writable /data
    PVC (``storage.output`` in tau.yaml). The offload sidecar watches the
    matching final names and ignores dot-prefixed temporary files.
    """
    root = output_dir or os.environ.get("TAU_OUTPUT_DIR", "/tmp/portal-ray-stellar")
    path = Path(root) / "metrics-history-attempt-0"
    path.mkdir(parents=True, exist_ok=True)
    return path


def metrics_row(step: int, loss: float, accuracy: float) -> dict:
    """Build one online metrics row.

    ``_timestamp`` MUST be a JSON number of positive Unix epoch seconds — the
    online offloader rejects strings (including RFC3339) with
    ``_timestamp must be numeric``. Kept as a standalone function so the
    regression test in taugrid-portal can feed a real generated row through the
    offloader's validation/import path.
    """
    return {
        "_step": step,
        "_timestamp": time.time(),
        "loss": loss,
        "accuracy": accuracy,
    }


def publish_metrics_row(metrics_dir: Path, step: int, loss: float, accuracy: float) -> Path:
    """Publish one closed chunk without exposing a partial object-file write."""
    token = f"{step:06d}-{time.time_ns()}"
    final = metrics_dir / f"chunk-{token}.jsonl"
    temporary = metrics_dir / f".{final.name}.tmp-{os.getpid()}"
    with temporary.open("x", encoding="utf-8") as stream:
        stream.write(json.dumps(metrics_row(step, loss, accuracy)) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, final)
    return final


@ray.remote(num_cpus=1)
def run_worker(worker_index: int, steps: int, output_dir: str) -> dict:
    """Run one bounded task; worker 0 is the only metrics producer."""
    metrics_dir = _metrics_dir(output_dir) if worker_index == 0 else None

    for step in range(steps):
        loss = 1.0 / (step + 1)
        accuracy = 1.0 - loss

        if metrics_dir is not None:
            published = publish_metrics_row(metrics_dir, step, loss, accuracy)
            print(f"published_metrics_chunk={published}", flush=True)

        time.sleep(1)

    return {
        "worker_index": worker_index,
        "node_id": ray.get_runtime_context().get_node_id(),
        "steps": steps,
    }


def main():
    # tau.yaml pins compute.workers: 1 / gpus_per_worker: 0 — a single CPU
    # worker is all this link-lighting demo needs.
    num_workers = int(os.environ.get("TAU_NUM_WORKERS", "1"))
    steps = int(os.environ.get("PORTAL_DEMO_STEPS", "20"))

    ray.init(address="auto")
    print("cluster_resources", ray.cluster_resources(), flush=True)

    group = placement_group(
        [{"CPU": 1} for _ in range(num_workers)],
        strategy="STRICT_SPREAD",
    )
    ray.get(group.ready(), timeout=120)
    try:
        futures = [
            run_worker.options(
                scheduling_strategy=PlacementGroupSchedulingStrategy(
                    placement_group=group,
                    placement_group_bundle_index=worker_index,
                )
            ).remote(
                worker_index,
                steps,
                os.environ.get("TAU_OUTPUT_DIR", "/tmp/portal-ray-stellar"),
            )
            for worker_index in range(num_workers)
        ]
        print("worker_results", json.dumps(ray.get(futures), sort_keys=True), flush=True)
    finally:
        remove_placement_group(group)


if __name__ == "__main__":
    main()
