# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Minimal GPU training job for stack e2e.

Exercises the training path of the advertised feature set:
  - Ray task with num_gpus=1 lands on a worker with nvidia.com/gpu=1
  - PyTorch SGD loop actually runs on the device
  - Asserts loss decreases (optimizer is actually stepping)

Kept intentionally tiny — this validates the admission + scheduling +
device-allocation path, not model quality. Runtime target: under 30s
inside a Ray worker.
"""
import ray


@ray.remote(num_gpus=1)
def train_once():
    import torch
    import torch.nn as nn

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    print(f"Device: {device}")
    if device.type != "cuda":
        raise RuntimeError("FAIL: expected CUDA device, got CPU — nvidia.com/gpu not allocated")

    print(f"Device name: {torch.cuda.get_device_name(0)}")

    torch.manual_seed(0)
    n, d = 2048, 16
    x = torch.randn(n, d, device=device)
    true_w = torch.randn(d, 1, device=device)
    y = x @ true_w + 0.01 * torch.randn(n, 1, device=device)

    model = nn.Linear(d, 1).to(device)
    opt = torch.optim.SGD(model.parameters(), lr=0.05)
    loss_fn = nn.MSELoss()

    losses = []
    for epoch in range(50):
        pred = model(x)
        loss = loss_fn(pred, y)
        opt.zero_grad()
        loss.backward()
        opt.step()
        losses.append(loss.item())
        if epoch % 10 == 0:
            print(f"epoch={epoch} loss={loss.item():.4f}")

    first, last = losses[0], losses[-1]
    print(f"first_loss={first:.4f} last_loss={last:.4f}")
    if last >= first:
        raise RuntimeError(f"FAIL: loss did not decrease ({first:.4f} -> {last:.4f})")

    return {"device": str(device), "first_loss": first, "last_loss": last}


ray.init()
cluster_resources = ray.cluster_resources()
print(f"Cluster resources: {cluster_resources}")
if cluster_resources.get("GPU", 0) < 1:
    raise SystemExit("FAIL: expected at least one Ray GPU resource")

result = ray.get(train_once.remote())
print(f"Training result: {result}")
print("SUCCESS: GPU training loss decreased")
