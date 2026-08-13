# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Train and export the actor-critic policy rendered in the TauGrid docs."""

from __future__ import annotations

import argparse
import json
import math
import os
import platform
import time
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np
import torch
import torch.nn.functional as F

INPUT_COUNT = 8
HIDDEN_COUNT = 24
ACTION_COUNT = 3
ACTION_NAMES = ("short", "flat", "long")
INPUT_NAMES = (
    "signal",
    "volatility",
    "inventory",
    "episode_phase",
    "phase_sine",
    "phase_cosine",
    "last_action",
    "micro_noise",
)


@dataclass(frozen=True)
class TrainConfig:
    seed: int
    training_examples: int
    validation_examples: int
    steps: int
    batch_size: int
    learning_rate: float
    value_loss_weight: float


class MarketPolicy(torch.nn.Module):
    """The exact 8 -> 24 -> (3 actor + 1 critic) browser architecture."""

    def __init__(self) -> None:
        super().__init__()
        self.hidden = torch.nn.Linear(INPUT_COUNT, HIDDEN_COUNT)
        self.policy = torch.nn.Linear(HIDDEN_COUNT, ACTION_COUNT)
        self.value = torch.nn.Linear(HIDDEN_COUNT, 1)

    def forward(self, inputs: torch.Tensor) -> tuple[torch.Tensor, torch.Tensor]:
        hidden = F.relu(self.hidden(inputs))
        return self.policy(hidden), self.value(hidden).squeeze(-1)


def env_int(name: str, default: int) -> int:
    return int(os.environ.get(name, str(default)))


def env_float(name: str, default: float) -> float:
    return float(os.environ.get(name, str(default)))


def load_config() -> TrainConfig:
    return TrainConfig(
        seed=env_int("MARKET_POLICY_SEED", 20260626),
        training_examples=env_int("MARKET_POLICY_TRAINING_EXAMPLES", 8000),
        validation_examples=env_int("MARKET_POLICY_VALIDATION_EXAMPLES", 2000),
        steps=env_int("MARKET_POLICY_STEPS", 4000),
        batch_size=env_int("MARKET_POLICY_BATCH_SIZE", 256),
        learning_rate=env_float("MARKET_POLICY_LEARNING_RATE", 0.004),
        value_loss_weight=env_float("MARKET_POLICY_VALUE_LOSS_WEIGHT", 1.0),
    )


def create_examples(
    rng: np.random.Generator, count: int
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    regimes = rng.choice(
        np.array([-1.0, 0.0, 1.0], dtype=np.float32),
        size=count,
        p=(0.3, 0.4, 0.3),
    )
    signals = 0.035 * regimes + rng.normal(0.0, 0.03, size=count)
    volatility = rng.uniform(0.01, 0.08, size=count)
    inventory = rng.uniform(-4.0, 4.0, size=count)
    phase = rng.uniform(0.0, 1.0, size=count)
    last_action = rng.choice(
        np.array([-1.0, 0.0, 1.0], dtype=np.float32),
        size=count,
    )
    micro_noise = rng.normal(0.0, 0.01, size=count)

    actions = np.where(signals > 0.01, 2, np.where(signals < -0.01, 0, 1))
    positions = actions - 1
    churn = np.abs(positions - last_action)
    expected_pnl = positions * signals
    alignment = np.abs(signals) * 10.0
    inventory_penalty = 0.0025 * np.square(inventory + positions)
    churn_penalty = 0.001 * churn
    values = alignment + expected_pnl - inventory_penalty - churn_penalty

    observations = np.column_stack(
        (
            signals,
            volatility,
            inventory / 4.0,
            phase,
            np.sin(2.0 * math.pi * phase),
            np.cos(2.0 * math.pi * phase),
            last_action,
            micro_noise,
        )
    )
    return (
        observations.astype(np.float32),
        actions.astype(np.int64),
        values.astype(np.float32),
    )


def evaluate(
    model: MarketPolicy,
    observations: torch.Tensor,
    actions: torch.Tensor,
    values: torch.Tensor,
) -> dict[str, float]:
    model.eval()
    with torch.no_grad():
        logits, predictions = model(observations)
        accuracy = (logits.argmax(dim=1) == actions).float().mean()
        rmse = torch.sqrt(F.mse_loss(predictions, values))
    model.train()
    return {
        "policyAccuracy": float(accuracy.cpu()),
        "valueRmse": float(rmse.cpu()),
    }


def round_values(values: torch.Tensor) -> list[float]:
    return [round(float(value), 7) for value in values.detach().cpu().reshape(-1)]


def export_artifact(
    model: MarketPolicy,
    config: TrainConfig,
    metrics: dict[str, float],
    output_path: Path,
    device: torch.device,
) -> None:
    gpu_name = torch.cuda.get_device_name(0) if device.type == "cuda" else ""
    artifact = {
        "format": "taugrid-market-policy",
        "version": 1,
        "purpose": (
            "Educational actor-critic trained by cli/examples/market-policy/train.py "
            "for a synthetic market-making task. Not financial advice."
        ),
        "architecture": {
            "actionCount": ACTION_COUNT,
            "hiddenCount": HIDDEN_COUNT,
            "inputCount": INPUT_COUNT,
            "activation": "relu",
            "policyHead": "softmax",
            "valueHead": "linear",
        },
        "inputs": list(INPUT_NAMES),
        "actions": list(ACTION_NAMES),
        "training": {
            "batchSize": config.batch_size,
            "device": str(device),
            "exampleCount": config.training_examples,
            "gpuName": gpu_name,
            "seed": config.seed,
            "steps": config.steps,
            "teacher": "signal-threshold policy with a one-step reward target",
            "validationExampleCount": config.validation_examples,
        },
        "metrics": metrics,
        "weights": {
            "hiddenBias": round_values(model.hidden.bias),
            "inputWeights": round_values(model.hidden.weight),
            "policyBias": round_values(model.policy.bias),
            "policyWeights": round_values(model.policy.weight),
            "valueBias": round_values(model.value.bias),
            "valueWeights": round_values(model.value.weight),
        },
    }
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(
        json.dumps(artifact, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def default_output_path() -> Path:
    explicit = os.environ.get("MARKET_POLICY_OUTPUT")
    if explicit:
        return Path(explicit)
    for key in ("TAU_DURABLE_CHECKPOINTS_DIR", "TAU_CHECKPOINTS_DIR"):
        directory = os.environ.get(key)
        if directory:
            return Path(directory) / "market-policy" / "tau-market-policy.json"
    return Path("/data/checkpoints/market-policy/tau-market-policy.json")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=None)
    parser.add_argument(
        "--device",
        choices=("cpu", "cuda"),
        default=None,
        help="Training device. Defaults to CUDA unless MARKET_POLICY_REQUIRE_CUDA=0.",
    )
    parser.add_argument("--manifest", default=None)
    parser.add_argument("--smoke-pairs", type=int, default=0)
    return parser.parse_args()


def train_and_export(
    config: TrainConfig,
    output_path: Path,
    device_name: str,
) -> dict[str, object]:
    if device_name == "cuda" and not torch.cuda.is_available():
        raise SystemExit(
            "CUDA is required inside the Ray worker. Verify the RayJob requested "
            "a GPU and was admitted to the H200 resource flavor."
        )

    device = torch.device(device_name)
    torch.manual_seed(config.seed)
    np.random.seed(config.seed)
    if device.type == "cuda":
        torch.cuda.manual_seed_all(config.seed)
        torch.set_float32_matmul_precision("high")

    rng = np.random.default_rng(config.seed)
    train_arrays = create_examples(rng, config.training_examples)
    validation_arrays = create_examples(rng, config.validation_examples)
    train_observations = torch.from_numpy(train_arrays[0]).to(device)
    train_actions = torch.from_numpy(train_arrays[1]).to(device)
    train_values = torch.from_numpy(train_arrays[2]).to(device)
    validation_observations = torch.from_numpy(validation_arrays[0]).to(device)
    validation_actions = torch.from_numpy(validation_arrays[1]).to(device)
    validation_values = torch.from_numpy(validation_arrays[2]).to(device)

    model = MarketPolicy().to(device)
    optimizer = torch.optim.Adam(model.parameters(), lr=config.learning_rate)
    batch_rng = torch.Generator(device="cpu").manual_seed(config.seed + 1)
    started = time.perf_counter()

    for step in range(1, config.steps + 1):
        indices = torch.randint(
            config.training_examples,
            (config.batch_size,),
            generator=batch_rng,
        ).to(device)
        logits, predictions = model(train_observations[indices])
        policy_loss = F.cross_entropy(logits, train_actions[indices])
        value_loss = F.mse_loss(predictions, train_values[indices])
        loss = policy_loss + config.value_loss_weight * value_loss

        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        optimizer.step()

        if step == 1 or step % 500 == 0 or step == config.steps:
            metrics = evaluate(
                model,
                validation_observations,
                validation_actions,
                validation_values,
            )
            print(
                json.dumps(
                    {
                        "step": step,
                        "loss": round(float(loss.detach().cpu()), 6),
                        **metrics,
                    },
                    sort_keys=True,
                ),
                flush=True,
            )

    if device.type == "cuda":
        torch.cuda.synchronize()
    elapsed_seconds = time.perf_counter() - started
    metrics = evaluate(
        model,
        validation_observations,
        validation_actions,
        validation_values,
    )
    if metrics["policyAccuracy"] < 0.98 or metrics["valueRmse"] > 0.04:
        raise SystemExit(f"Training missed its quality gate: {json.dumps(metrics)}")

    export_artifact(model, config, metrics, output_path, device)
    return {
        "cudaVersion": torch.version.cuda,
        "device": str(device),
        "elapsedSeconds": round(elapsed_seconds, 3),
        "gpuName": torch.cuda.get_device_name(0) if device.type == "cuda" else "",
        "metrics": metrics,
        "node": platform.node(),
        "output": str(output_path),
        "parameters": sum(parameter.numel() for parameter in model.parameters()),
        "status": "succeeded",
        "torchVersion": torch.__version__,
    }


def train_with_ray(config: TrainConfig, output_path: Path) -> dict[str, object]:
    import ray

    ray.init(address="auto", ignore_reinit_error=True)

    @ray.remote(num_cpus=2, num_gpus=1)
    def run_on_gpu(
        config_values: dict[str, object],
        output: str,
    ) -> dict[str, object]:
        return train_and_export(
            TrainConfig(**config_values),
            Path(output),
            device_name="cuda",
        )

    try:
        summary = ray.get(run_on_gpu.remote(asdict(config), str(output_path)))
        summary["rayClusterResources"] = dict(ray.cluster_resources())
        summary["rayVersion"] = ray.__version__
        return summary
    finally:
        ray.shutdown()


def main() -> int:
    args = parse_args()
    config = load_config()
    output_path = args.output or default_output_path()
    use_ray = os.environ.get("MARKET_POLICY_USE_RAY", "0") == "1"
    require_cuda = os.environ.get("MARKET_POLICY_REQUIRE_CUDA", "1") == "1"

    if use_ray:
        if args.device == "cpu":
            raise SystemExit("Ray training requires CUDA; --device cpu is local-only.")
        summary = train_with_ray(config, output_path)
    else:
        if require_cuda and args.device == "cpu":
            raise SystemExit(
                "CPU training requires MARKET_POLICY_REQUIRE_CUDA=0."
            )
        summary = train_and_export(
            config,
            output_path,
            device_name=args.device or ("cuda" if require_cuda else "cpu"),
        )

    print(json.dumps(summary, sort_keys=True), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
