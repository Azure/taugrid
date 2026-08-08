# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
Training test for e2e validation of the Ray-training-via-Kueue claim.

Runs a short distributed SGD loop on synthetic regression data across Ray actors:
  - ActorPoolStrategy distributes a batch of (x, y) across workers
  - Each actor holds a tiny model, computes repeated gradient steps, returns loss
  - The driver aggregates first/last losses and prints a final line we assert on

Intentionally small: the goal is to exercise Kueue admission + KubeRay head/workers
+ Ray Data distribution of a training-shaped workload, not to produce a real model.
Auto-detects GPU for the GPU variant.
"""
import ray
import numpy as np

ray.init()
cluster_resources = ray.cluster_resources()
print(f"Cluster resources: {cluster_resources}")
GPU_AVAILABLE = cluster_resources.get("GPU", 0) > 0
print(f"GPU_AVAILABLE = {GPU_AVAILABLE}")

# Synthetic linear regression data: y = 3x + 2 + noise.
NUM_SAMPLES = 64
rng = np.random.default_rng(seed=42)
xs = rng.uniform(-1.0, 1.0, size=(NUM_SAMPLES, 1)).astype(np.float32)
ys = (3.0 * xs + 2.0 + rng.normal(0, 0.05, size=xs.shape)).astype(np.float32)
samples = [{"x": xs[i], "y": ys[i]} for i in range(NUM_SAMPLES)]
ds = ray.data.from_items(samples)


class Trainer:
    def __init__(self):
        import torch
        import torch.nn as nn

        torch.manual_seed(0)
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        print(f"Device: {self.device}")
        self.model = nn.Linear(1, 1).to(self.device)
        self.opt = torch.optim.SGD(self.model.parameters(), lr=0.05)
        self.loss_fn = nn.MSELoss()
        self.step = 0

    def __call__(self, batch):
        import torch

        x = torch.from_numpy(np.stack(batch["x"])).to(self.device)
        y = torch.from_numpy(np.stack(batch["y"])).to(self.device)

        first_loss = None
        last_loss = None
        for _ in range(INNER_STEPS):
            self.opt.zero_grad()
            pred = self.model(x)
            loss = self.loss_fn(pred, y)
            loss.backward()
            self.opt.step()
            self.step += 1

            loss_val = float(loss.detach().cpu().item())
            if first_loss is None:
                first_loss = loss_val
            last_loss = loss_val
            print(f"step={self.step} loss={loss_val:.6f}")

        batch["first_loss"] = [first_loss] * len(batch["x"])
        batch["last_loss"] = [last_loss] * len(batch["x"])
        batch["loss"] = [last_loss] * len(batch["x"])
        return batch


POOL_SIZE = 1 if GPU_AVAILABLE else 2
BATCH_SIZE = 8
INNER_STEPS = 12

results = ds.map_batches(
    Trainer,
    compute=ray.data.ActorPoolStrategy(size=POOL_SIZE),
    batch_size=BATCH_SIZE,
    **({"num_gpus": 1} if GPU_AVAILABLE else {}),
).take_all()
first_losses = [r["first_loss"] for r in results]
last_losses = [r["last_loss"] for r in results]
mean_first = float(np.mean(first_losses))
mean_last = float(np.mean(last_losses))

# Basic sanity: training should reduce loss within each actor's deterministic SGD loop.
print(f"mean_first_loss={mean_first:.6f} mean_last_loss={mean_last:.6f}")
assert mean_last < mean_first, (
    f"Expected loss to decrease during SGD; got first={mean_first}, last={mean_last}"
)

print("\nTRAINING_COMPLETE: SGD converged during actor training")
