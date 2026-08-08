#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Production-shaped PufferLib + CUDA RL workload for Tau."""

from __future__ import annotations

import json
import math
import os
import time
from dataclasses import asdict, dataclass
from pathlib import Path

import gymnasium
import numpy as np
import pufferlib
import pufferlib.vector
import ray
import torch
import torch.nn.functional as F


class MarketMakingEnv(pufferlib.PufferEnv):
    """Vectorized synthetic market-making environment.

    Each agent sees a short market state and chooses short/flat/long. Rewards
    favor matching the next latent return while penalizing inventory and churn.
    The environment is cheap, deterministic by seed, and large enough to stress
    the policy learner with production-like batches.
    """

    def __init__(self, num_agents: int = 512, episode_length: int = 128, seed: int = 0, buf=None):
        self.single_observation_space = gymnasium.spaces.Box(
            low=-10.0,
            high=10.0,
            shape=(8,),
            dtype=np.float32,
        )
        self.single_action_space = gymnasium.spaces.Discrete(3)
        self.num_agents = num_agents
        super().__init__(buf)
        self.episode_length = episode_length
        self.rng = np.random.default_rng(seed)
        self.tick = np.zeros(num_agents, dtype=np.int32)
        self.inventory = np.zeros(num_agents, dtype=np.float32)
        self.signal = np.zeros(num_agents, dtype=np.float32)
        self.volatility = np.ones(num_agents, dtype=np.float32) * 0.02
        self.last_action = np.ones(num_agents, dtype=np.int64)

    def reset(self, seed: int = 0):
        if seed:
            self.rng = np.random.default_rng(seed)
        self.tick[:] = 0
        self.inventory[:] = 0.0
        self.last_action[:] = 1
        self._sample_state()
        return self.observations, []

    def step(self, action):
        raw_action = np.asarray(action).reshape(-1)[: self.num_agents]
        target = raw_action.astype(np.int64) - 1
        oracle = np.where(self.signal > 0.01, 1, np.where(self.signal < -0.01, -1, 0)).astype(np.float32)
        churn = np.abs(raw_action - self.last_action).astype(np.float32)
        realized_return = self.signal + self.rng.normal(0.0, self.volatility, size=self.num_agents)
        pnl = target.astype(np.float32) * realized_return
        alignment = np.where(target.astype(np.float32) == oracle, 1.0, -0.35) * np.abs(self.signal) * 10.0
        inventory_penalty = 0.0025 * np.square(self.inventory + target)
        churn_penalty = 0.001 * churn

        self.rewards[:] = (alignment + pnl - inventory_penalty - churn_penalty).astype(np.float32)
        self.inventory[:] = np.clip(0.92 * self.inventory + target, -4.0, 4.0)
        self.last_action[:] = raw_action
        self.tick += 1

        done = self.tick >= self.episode_length
        self.terminals[:] = done
        self.truncations[:] = False
        infos = [
            {
                "inventory": float(self.inventory[i]),
                "reward": float(self.rewards[i]),
                "signal": float(self.signal[i]),
            }
            for i in range(min(4, self.num_agents))
        ]
        if done.any():
            self.tick[done] = 0
            self.inventory[done] = 0.0
            self.last_action[done] = 1
        self._sample_state()
        return self.observations, self.rewards, self.terminals, self.truncations, infos

    def close(self):
        pass

    def _sample_state(self) -> None:
        regime = self.rng.choice(np.array([-1.0, 0.0, 1.0], dtype=np.float32), size=self.num_agents, p=[0.3, 0.4, 0.3])
        noise = self.rng.normal(0.0, 0.03, size=self.num_agents).astype(np.float32)
        self.signal[:] = 0.035 * regime + noise
        self.volatility[:] = self.rng.uniform(0.01, 0.08, size=self.num_agents).astype(np.float32)
        phase = (self.tick.astype(np.float32) % self.episode_length) / float(self.episode_length)
        self.observations[:, 0] = self.signal
        self.observations[:, 1] = self.volatility
        self.observations[:, 2] = self.inventory / 4.0
        self.observations[:, 3] = phase
        self.observations[:, 4] = np.sin(2.0 * math.pi * phase)
        self.observations[:, 5] = np.cos(2.0 * math.pi * phase)
        self.observations[:, 6] = (self.last_action.astype(np.float32) - 1.0)
        self.observations[:, 7] = self.rng.normal(0.0, 0.01, size=self.num_agents).astype(np.float32)


class ActorCritic(torch.nn.Module):
    def __init__(self, obs_dim: int, hidden_size: int):
        super().__init__()
        self.body = torch.nn.Sequential(
            torch.nn.Linear(obs_dim, hidden_size),
            torch.nn.SiLU(),
            torch.nn.LayerNorm(hidden_size),
            torch.nn.Linear(hidden_size, hidden_size),
            torch.nn.SiLU(),
            torch.nn.LayerNorm(hidden_size),
        )
        self.actor = torch.nn.Linear(hidden_size, 3)
        self.critic = torch.nn.Linear(hidden_size, 1)

    def forward(self, obs: torch.Tensor) -> tuple[torch.Tensor, torch.Tensor]:
        hidden = self.body(obs)
        return self.actor(hidden), self.critic(hidden).squeeze(-1)


@dataclass
class TrainConfig:
    num_envs: int
    num_workers: int
    agents_per_env: int
    updates: int
    hidden_size: int
    seed: int
    episode_length: int
    learning_rate: float
    require_cuda: bool
    aux_loss_coef: float


def env_int(name: str, default: int) -> int:
    return int(os.environ.get(name, str(default)))


def env_float(name: str, default: float) -> float:
    return float(os.environ.get(name, str(default)))


def load_config() -> TrainConfig:
    return TrainConfig(
        num_envs=env_int("PUFFER_NUM_ENVS", 8),
        num_workers=env_int("PUFFER_NUM_WORKERS", 8),
        agents_per_env=env_int("PUFFER_AGENTS_PER_ENV", 512),
        updates=env_int("PUFFER_UPDATES", 800),
        hidden_size=env_int("PUFFER_HIDDEN_SIZE", 512),
        seed=env_int("PUFFER_SEED", 20260626),
        episode_length=env_int("PUFFER_EPISODE_LENGTH", 128),
        learning_rate=env_float("PUFFER_LEARNING_RATE", 3e-4),
        require_cuda=os.environ.get("PUFFER_REQUIRE_CUDA", "1") == "1",
        aux_loss_coef=env_float("PUFFER_AUX_LOSS_COEF", 0.05),
    )


def write_json(path: Path, payload: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def append_jsonl(path: Path, payload: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(payload, sort_keys=True) + "\n")


def make_vecenv(config: TrainConfig):
    backend = pufferlib.vector.Multiprocessing if config.num_workers > 1 else pufferlib.vector.Serial
    return pufferlib.vector.make(
        MarketMakingEnv,
        num_envs=config.num_envs,
        num_workers=config.num_workers,
        batch_size=1,
        backend=backend,
        seed=config.seed,
        env_kwargs={
            "num_agents": config.agents_per_env,
            "episode_length": config.episode_length,
        },
    )


def main() -> None:
    config = load_config()
    if config.require_cuda and not torch.cuda.is_available():
        raise RuntimeError("CUDA is required for this PufferLib GPU RL example")

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    torch.manual_seed(config.seed)
    np.random.seed(config.seed)
    if device.type == "cuda":
        torch.set_float32_matmul_precision("high")

    ray.init(address=os.environ.get("RAY_ADDRESS", "auto"))
    vecenv = make_vecenv(config)
    out_dir = Path(os.environ.get("TAU_DATA_DIR", "/data")) / "examples" / "pufferlib-gpu-rl"
    history_path = out_dir / "history.jsonl"
    history_path.unlink(missing_ok=True)

    observations, _ = vecenv.reset()
    obs_dim = int(observations.shape[-1])
    model = ActorCritic(obs_dim, config.hidden_size).to(device)
    optimizer = torch.optim.AdamW(model.parameters(), lr=config.learning_rate)

    reward_ema = 0.0
    reward_first = None
    accuracy_ema = 0.0
    accuracy_first = None
    started = time.time()
    total_steps = 0
    last_log = started
    last_steps = 0

    try:
        for update in range(1, config.updates + 1):
            obs_tensor = torch.as_tensor(observations, dtype=torch.float32, device=device)
            logits, values = model(obs_tensor)
            dist = torch.distributions.Categorical(logits=logits)
            actions = dist.sample()
            target_actions = torch.where(
                obs_tensor[:, 0] > 0.01,
                torch.full_like(actions, 2),
                torch.where(obs_tensor[:, 0] < -0.01, torch.zeros_like(actions), torch.ones_like(actions)),
            )
            action_accuracy = (actions == target_actions).float().mean()
            next_obs, rewards, terminals, truncations, infos = vecenv.step(actions.detach().cpu().numpy())

            reward_tensor = torch.as_tensor(rewards, dtype=torch.float32, device=device)
            advantage = reward_tensor - values.detach()
            policy_loss = -(dist.log_prob(actions) * advantage).mean()
            value_loss = torch.nn.functional.mse_loss(values, reward_tensor)
            entropy = dist.entropy().mean()
            aux_loss = F.cross_entropy(logits, target_actions)
            loss = policy_loss + 0.5 * value_loss - 0.01 * entropy + config.aux_loss_coef * aux_loss

            optimizer.zero_grad(set_to_none=True)
            loss.backward()
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            optimizer.step()
            if device.type == "cuda":
                torch.cuda.synchronize()

            batch_reward = float(np.mean(rewards))
            if reward_first is None:
                reward_first = batch_reward
                reward_ema = batch_reward
            else:
                reward_ema = 0.98 * reward_ema + 0.02 * batch_reward
            batch_accuracy = float(action_accuracy.detach().cpu())
            if accuracy_first is None:
                accuracy_first = batch_accuracy
                accuracy_ema = batch_accuracy
            else:
                accuracy_ema = 0.98 * accuracy_ema + 0.02 * batch_accuracy
            total_steps += int(np.asarray(rewards).size)
            observations = next_obs

            if update == 1 or update % max(1, config.updates // 20) == 0 or update == config.updates:
                now = time.time()
                sps_window = (total_steps - last_steps) / max(now - last_log, 1e-9)
                last_log = now
                last_steps = total_steps
                row = {
                    "update": update,
                    "reward_mean": batch_reward,
                    "reward_ema": reward_ema,
                    "policy_loss": float(policy_loss.detach().cpu()),
                    "value_loss": float(value_loss.detach().cpu()),
                    "aux_loss": float(aux_loss.detach().cpu()),
                    "entropy": float(entropy.detach().cpu()),
                    "action_accuracy": batch_accuracy,
                    "action_accuracy_ema": accuracy_ema,
                    "steps": total_steps,
                    "steps_per_second_window": sps_window,
                }
                append_jsonl(history_path, row)
                print(json.dumps(row, sort_keys=True), flush=True)
    finally:
        vecenv.close()

    elapsed = time.time() - started
    checkpoint = out_dir / "policy.pt"
    checkpoint.parent.mkdir(parents=True, exist_ok=True)
    torch.save({"model": model.state_dict(), "config": asdict(config)}, checkpoint)
    summary = {
        "status": "succeeded",
        "config": asdict(config),
        "device": str(device),
        "gpu_name": torch.cuda.get_device_name(0) if device.type == "cuda" else "",
        "pufferlib_version": getattr(pufferlib, "__version__", "unknown"),
        "total_steps": total_steps,
        "elapsed_seconds": elapsed,
        "steps_per_second": total_steps / max(elapsed, 1e-9),
        "reward_first": reward_first,
        "reward_final_ema": reward_ema,
        "reward_delta": None if reward_first is None else reward_ema - reward_first,
        "action_accuracy_first": accuracy_first,
        "action_accuracy_final_ema": accuracy_ema,
        "action_accuracy_delta": None if accuracy_first is None else accuracy_ema - accuracy_first,
        "max_cuda_memory_allocated": torch.cuda.max_memory_allocated() if device.type == "cuda" else 0,
        "checkpoint": str(checkpoint),
    }
    write_json(out_dir / "summary.json", summary)
    print(json.dumps(summary, sort_keys=True), flush=True)


if __name__ == "__main__":
    main()
