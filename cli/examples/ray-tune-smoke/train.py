# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Minimal training function for Ray Tune HPO smoke test.

Tau generates the Tuner + TorchTrainer wrapper — this file only defines the
training loop. The function name ``train_func`` is the convention Tau looks
for when ``execution.launcher: ray-tune``.
"""

import ray.train


def train_func(config):
    """One-epoch dummy training loop that reports a loss derived from config."""
    lr = config.get("lr", 0.01)
    batch_size = config.get("batch_size", 32)

    for step in range(5):
        loss = (0.1 + lr * step / 5) ** (-1) + batch_size * 0.001
        ray.train.report({"loss": loss, "step": step})
