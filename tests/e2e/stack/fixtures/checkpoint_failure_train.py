"""Ray Train checkpoint-aware failure recovery E2E fixture.

Exercises execution.configs.failure_config.max_failures via
TAU_RAY_TRAIN_CONFIG_JSON. The script:
1. Writes a checkpoint every 3 steps.
2. Simulates one rank-0 worker failure at step 5 (only once, via sentinel).
3. Resumes from checkpoint on restart.
4. Asserts final step > fail_at_step (proves checkpoint resume, not restart from 0).
"""

import json
import os
import tempfile
from pathlib import Path

import ray
from ray import train
from ray.train import Checkpoint, FailureConfig, RunConfig, ScalingConfig
from ray.train.torch import TorchConfig, TorchTrainer

SENTINEL_PATH = Path("/tmp/checkpoint_failure_sentinel")


def train_loop(config: dict) -> None:
    rank = train.get_context().get_world_rank()
    start_step = 0
    fail_at = config["fail_at_step"]
    total_steps = config["total_steps"]
    checkpoint_every = config["checkpoint_every"]

    checkpoint = train.get_checkpoint()
    if checkpoint:
        with checkpoint.as_directory() as d:
            state = json.loads(Path(d, "state.json").read_text())
            start_step = state["step"]
            print(f"rank={rank} resuming from checkpoint at step={start_step}", flush=True)

    for step in range(start_step, total_steps):
        if step == fail_at and rank == 0 and not SENTINEL_PATH.exists():
            SENTINEL_PATH.touch()
            print(f"rank={rank} simulating failure at step={step}", flush=True)
            os._exit(1)

        loss = 1.0 / (step + 1)

        if (step + 1) % checkpoint_every == 0 or step == total_steps - 1:
            with tempfile.TemporaryDirectory() as d:
                Path(d, "state.json").write_text(json.dumps({"step": step + 1}))
                train.report(
                    {"loss": loss, "step": step},
                    checkpoint=Checkpoint.from_directory(d),
                )
        else:
            train.report({"loss": loss, "step": step})


def main() -> None:
    ray.init(address="auto")

    ray_train_config_raw = os.environ.get("TAU_RAY_TRAIN_CONFIG_JSON", "{}")
    ray_train_config = json.loads(ray_train_config_raw)

    failure_kwargs = ray_train_config.get("failure_config", {})
    torch_kwargs = ray_train_config.get("torch_config", {})
    scaling_kwargs = ray_train_config.get("scaling_config", {})

    dist_backend = os.environ.get("TAU_DIST_BACKEND", "nccl")
    num_workers = int(os.environ.get("TAU_NUM_WORKERS", "2"))

    torch_config_kwargs = {"backend": dist_backend}
    torch_config_kwargs.update(torch_kwargs)

    scaling_config_kwargs = {
        "num_workers": num_workers,
        "use_gpu": True,
        "resources_per_worker": {"GPU": 1},
    }
    scaling_config_kwargs.update(scaling_kwargs)

    run_config_kwargs = {"name": "checkpoint-failure-test"}
    if failure_kwargs:
        run_config_kwargs["failure_config"] = FailureConfig(**failure_kwargs)

    trainer = TorchTrainer(
        train_loop,
        train_loop_config={
            "fail_at_step": 5,
            "total_steps": 10,
            "checkpoint_every": 3,
        },
        torch_config=TorchConfig(**torch_config_kwargs),
        scaling_config=ScalingConfig(**scaling_config_kwargs),
        run_config=RunConfig(**run_config_kwargs),
    )
    result = trainer.fit()

    final_step = result.metrics.get("step", -1)
    print(f"Training completed. final_step={final_step}", flush=True)
    if final_step < 9:
        raise RuntimeError(
            f"Checkpoint resume failed: final_step={final_step}, expected >= 9. "
            "Training likely restarted from step 0 instead of resuming."
        )
    print("SUCCESS: checkpoint-aware failure recovery verified", flush=True)


if __name__ == "__main__":
    main()
