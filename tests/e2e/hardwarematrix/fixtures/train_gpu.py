# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
import socket
import tempfile

import ray
from ray import train
from ray.train import ScalingConfig
from ray.train.torch import TorchTrainer


WORKERS = int(os.environ["MATRIX_WORKERS"])


def train_loop():
    import torch
    import torch.distributed as dist
    import torch.nn as nn

    context = train.get_context()
    rank = context.get_world_rank()
    world_size = context.get_world_size()
    if world_size != WORKERS:
        raise RuntimeError(f"expected world size {WORKERS}, got {world_size}")
    if not torch.cuda.is_available():
        raise RuntimeError(f"rank {rank} expected CUDA")

    device = torch.device("cuda")
    torch.manual_seed(1000 + rank)
    model = nn.Linear(32, 8).to(device)
    optimizer = torch.optim.SGD(model.parameters(), lr=0.05)
    inputs = torch.randn(256, 32, device=device)
    targets = torch.zeros(256, 8, device=device)

    first_loss = None
    last_loss = None
    for step in range(8):
        loss = nn.functional.mse_loss(model(inputs), targets)
        optimizer.zero_grad()
        loss.backward()
        optimizer.step()
        reduced = loss.detach().clone()
        dist.all_reduce(reduced)
        reduced /= world_size
        if first_loss is None:
            first_loss = float(reduced.item())
        last_loss = float(reduced.item())

    metadata = {
        "rank": rank,
        "hostname": socket.gethostname(),
        "device_name": torch.cuda.get_device_name(0),
        "visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES", ""),
        "first_loss": first_loss,
        "last_loss": last_loss,
    }
    gathered = [None] * world_size
    dist.all_gather_object(gathered, metadata)
    if last_loss >= first_loss:
        raise RuntimeError(
            f"rank {rank} loss did not decrease: {first_loss} -> {last_loss}"
        )

    checkpoint_bytes = 0
    if rank == 0:
        checkpoint_dir = tempfile.mkdtemp(prefix="taugrid-matrix-checkpoint-")
        checkpoint_path = os.path.join(checkpoint_dir, "model.pt")
        torch.save(model.state_dict(), checkpoint_path)
        checkpoint_bytes = os.path.getsize(checkpoint_path)
        if checkpoint_bytes <= 0:
            raise RuntimeError("checkpoint is empty")
        print("MATRIX_TRAIN_RESULT=" + json.dumps(
            {
                "expected_workers": WORKERS,
                "workers": sorted(gathered, key=lambda item: item["rank"]),
                "checkpoint_bytes": checkpoint_bytes,
            },
            sort_keys=True,
        ))

    train.report(
        {
            "rank": rank,
            "world_size": world_size,
            "loss": last_loss,
            "checkpoint_bytes": checkpoint_bytes,
        },
    )


ray.init()
trainer = TorchTrainer(
    train_loop_per_worker=train_loop,
    scaling_config=ScalingConfig(num_workers=WORKERS, use_gpu=True),
)
result = trainer.fit()
if result.error:
    raise result.error
print("SUCCESS: Ray Train completed on every GPU worker")
