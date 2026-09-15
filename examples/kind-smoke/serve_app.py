# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from ray import serve


@serve.deployment(
    num_replicas=2,
    max_replicas_per_node=1,
    ray_actor_options={"num_cpus": 0.1},
)
class Smoke:
    def __call__(self) -> str:
        return "tau kind serve smoke complete"


app = Smoke.bind()
