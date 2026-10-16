# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import socket

import ray


@ray.remote(num_cpus=1)
class Worker:
    def identity(self) -> dict[str, str]:
        return {
            "hostname": socket.gethostname(),
            "node_id": ray.get_runtime_context().get_node_id(),
        }


def main() -> None:
    ray.init(address="auto")
    workers = [Worker.remote() for _ in range(2)]
    identities = ray.get([worker.identity.remote() for worker in workers])
    node_ids = {identity["node_id"] for identity in identities}
    if len(node_ids) != 2:
        raise RuntimeError(f"expected two Ray worker nodes, got {identities}")
    print(
        "TAUGRID_NIGHTLY_SMOKE_COMPLETE "
        + json.dumps({"workers": identities}, sort_keys=True),
        flush=True,
    )


if __name__ == "__main__":
    main()
