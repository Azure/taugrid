# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os

import ray


@ray.remote(num_cpus=1)
class WorkerProbe:
    def location(self) -> str:
        return ray.get_runtime_context().get_node_id()


def main() -> None:
    ray.init(address="auto")
    resources = ray.cluster_resources()
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
