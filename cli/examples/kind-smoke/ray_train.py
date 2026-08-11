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
    resources = wait_for_ray_cpus(2)
    print(f"tau kind ray marker={os.environ.get('KIND_RAY_MARKER', '')}")
    print(f"tau kind ray resources={resources}")
    if resources.get("CPU", 0) < 2:
        raise SystemExit(f"expected at least two Ray CPUs, got {resources}")
    workers = [WorkerProbe.remote() for _ in range(2)]
    worker_nodes = ray.get(
        [worker.location.remote() for worker in workers], timeout=120
    )
    print(f"tau kind ray worker nodes={worker_nodes}")
    if len(set(worker_nodes)) != 2:
        raise SystemExit(f"expected tasks on two Ray workers, got {worker_nodes}")
    print("tau kind ray smoke complete")


if __name__ == "__main__":
    main()
