# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os

import ray


def main() -> None:
    ray.init(address="auto")
    resources = ray.cluster_resources()
    print(f"tau kind ray marker={os.environ.get('KIND_RAY_MARKER', '')}")
    print(f"tau kind ray resources={resources}")
    if resources.get("CPU", 0) < 1:
        raise SystemExit(f"expected Ray CPU resources, got {resources}")
    print("tau kind ray smoke complete")


if __name__ == "__main__":
    main()
