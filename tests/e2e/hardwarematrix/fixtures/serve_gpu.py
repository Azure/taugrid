# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import asyncio
import concurrent.futures
import json
import os
import socket
import time
import urllib.request

import ray
from ray import serve


EXPECTED_REPLICAS = int(os.environ["MATRIX_WORKERS"])
REQUEST_TIMEOUT_SECONDS = 180
TORCH_SPEC = os.environ.get("MATRIX_TORCH_SPEC", "torch==2.7.1")
TORCH_INDEX_URL = os.environ.get(
    "MATRIX_TORCH_INDEX_URL", "https://download.pytorch.org/whl/cu128"
)


@serve.deployment(
    num_replicas=EXPECTED_REPLICAS,
    ray_actor_options={
        "num_gpus": 1,
        "runtime_env": {
            "pip": {
                "packages": [TORCH_SPEC],
                "pip_install_options": [
                    "--no-cache-dir",
                    "--index-url",
                    TORCH_INDEX_URL,
                    "--extra-index-url",
                    "https://pypi.org/simple",
                ],
                "pip_check": False,
            }
        },
    },
)
class GPUInference:
    def __init__(self):
        import torch

        if not torch.cuda.is_available():
            raise RuntimeError("expected CUDA in Ray Serve replica")
        self.torch = torch
        self.hostname = socket.gethostname()
        context = serve.get_replica_context()
        self.replica = str(
            getattr(context, "replica_id", getattr(context, "replica_tag", "unknown"))
        )
        self.device_name = torch.cuda.get_device_name(0)
        self.visible_devices = os.environ.get("CUDA_VISIBLE_DEVICES", "")

    async def __call__(self, request):
        await asyncio.sleep(0.05)
        left = self.torch.ones((256, 256), device="cuda")
        right = self.torch.ones((256, 256), device="cuda")
        checksum = float((left @ right).sum().item())
        return {
            "replica": self.replica,
            "hostname": self.hostname,
            "device_name": self.device_name,
            "visible_devices": self.visible_devices,
            "checksum": checksum,
        }


def invoke():
    request = urllib.request.Request("http://127.0.0.1:8000/", method="GET")
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())


ray.init()
serve.run(GPUInference.bind(), route_prefix="/")

deadline = time.time() + REQUEST_TIMEOUT_SECONDS
observed = {}
while time.time() < deadline and len(observed) < EXPECTED_REPLICAS:
    with concurrent.futures.ThreadPoolExecutor(
        max_workers=EXPECTED_REPLICAS * 8
    ) as executor:
        futures = [executor.submit(invoke) for _ in range(EXPECTED_REPLICAS * 8)]
        for future in concurrent.futures.as_completed(futures):
            result = future.result()
            observed[result["replica"]] = result

if len(observed) != EXPECTED_REPLICAS:
    raise RuntimeError(
        f"expected {EXPECTED_REPLICAS} Ray Serve replicas to handle requests, "
        f"observed {len(observed)}: {sorted(observed)}"
    )

print("MATRIX_SERVE_RESULT=" + json.dumps(
    {
        "expected_replicas": EXPECTED_REPLICAS,
        "replicas": sorted(observed.values(), key=lambda item: item["replica"]),
    },
    sort_keys=True,
))
print("SUCCESS: Ray Serve GPU inference completed on every replica")
