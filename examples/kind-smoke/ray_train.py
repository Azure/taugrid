# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
import time

import ray


@ray.remote(num_cpus=1)
class WorkerProbe:
    def location(self) -> str:
        return ray.get_runtime_context().get_node_id()


def wait_for_ray_cpus(minimum: int, timeout_seconds: int = 120) -> dict[str, float]:
    deadline = time.monotonic() + timeout_seconds
    resources = ray.cluster_resources()
    while resources.get("CPU", 0) < minimum and time.monotonic() < deadline:
        time.sleep(2)
        resources = ray.cluster_resources()
    return resources


def main() -> None:
    ray.init(address="auto")
    resources = wait_for_ray_cpus(1)
    print(f"tau kind ray marker={os.environ.get('KIND_RAY_MARKER', '')}")
    print(f"tau kind ray resources={resources}")
    if resources.get("CPU", 0) < 1:
        raise SystemExit(f"expected at least one Ray CPU, got {resources}")
    workers = [WorkerProbe.remote()]
    worker_nodes = ray.get(
        [worker.location.remote() for worker in workers], timeout=120
    )
    print(f"tau kind ray worker nodes={worker_nodes}")
    if len(worker_nodes) != 1 or not worker_nodes[0]:
        raise SystemExit(f"expected one Ray worker node, got {worker_nodes}")
    print("tau kind ray smoke complete")


if __name__ == "__main__":
    main()
