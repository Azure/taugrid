# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Ray Train nanoGPT-style workload for live 16-GPU stack conformance."""

from __future__ import annotations

import hashlib
import math
import os
import random
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import ray
import torch
import torch.nn as nn
import torch.nn.functional as F
from ray import train
from ray.train import RunConfig, ScalingConfig
from ray.train import torch as ray_train_torch
from ray.train.torch import TorchTrainer


@dataclass(frozen=True)
class GPTConfig:
    block_size: int
    vocab_size: int = 50304
    n_layer: int = 4
    n_head: int = 4
    n_embd: int = 256
    dropout: float = 0.0


class CausalSelfAttention(nn.Module):
    def __init__(self, config: GPTConfig):
        super().__init__()
        if config.n_embd % config.n_head != 0:
            raise ValueError("n_embd must be divisible by n_head")
        self.c_attn = nn.Linear(config.n_embd, 3 * config.n_embd, bias=False)
        self.c_proj = nn.Linear(config.n_embd, config.n_embd, bias=False)
        self.n_head = config.n_head
        self.n_embd = config.n_embd
        self.register_buffer(
            "bias",
            torch.tril(torch.ones(config.block_size, config.block_size)).view(
                1, 1, config.block_size, config.block_size
            ),
            persistent=False,
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        bsz, seq_len, channels = x.size()
        q, k, v = self.c_attn(x).split(self.n_embd, dim=2)
        q = q.view(bsz, seq_len, self.n_head, channels // self.n_head).transpose(1, 2)
        k = k.view(bsz, seq_len, self.n_head, channels // self.n_head).transpose(1, 2)
        v = v.view(bsz, seq_len, self.n_head, channels // self.n_head).transpose(1, 2)
        att = (q @ k.transpose(-2, -1)) * (1.0 / math.sqrt(k.size(-1)))
        att = att.masked_fill(self.bias[:, :, :seq_len, :seq_len] == 0, float("-inf"))
        att = F.softmax(att, dim=-1)
        y = att @ v
        y = y.transpose(1, 2).contiguous().view(bsz, seq_len, channels)
        return self.c_proj(y)


class MLP(nn.Module):
    def __init__(self, config: GPTConfig):
        super().__init__()
        self.c_fc = nn.Linear(config.n_embd, 4 * config.n_embd, bias=False)
        self.c_proj = nn.Linear(4 * config.n_embd, config.n_embd, bias=False)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.c_proj(F.gelu(self.c_fc(x)))


class Block(nn.Module):
    def __init__(self, config: GPTConfig):
        super().__init__()
        self.ln_1 = nn.LayerNorm(config.n_embd)
        self.attn = CausalSelfAttention(config)
        self.ln_2 = nn.LayerNorm(config.n_embd)
        self.mlp = MLP(config)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        x = x + self.attn(self.ln_1(x))
        return x + self.mlp(self.ln_2(x))


class GPT(nn.Module):
    def __init__(self, config: GPTConfig):
        super().__init__()
        self.config = config
        self.transformer = nn.ModuleDict(
            {
                "wte": nn.Embedding(config.vocab_size, config.n_embd),
                "wpe": nn.Embedding(config.block_size, config.n_embd),
                "h": nn.ModuleList([Block(config) for _ in range(config.n_layer)]),
                "ln_f": nn.LayerNorm(config.n_embd),
            }
        )
        self.lm_head = nn.Linear(config.n_embd, config.vocab_size, bias=False)
        self.transformer["wte"].weight = self.lm_head.weight
        self.apply(self._init_weights)

    def _init_weights(self, module: nn.Module) -> None:
        if isinstance(module, nn.Linear):
            torch.nn.init.normal_(module.weight, mean=0.0, std=0.02)
            if module.bias is not None:
                torch.nn.init.zeros_(module.bias)
        elif isinstance(module, nn.Embedding):
            torch.nn.init.normal_(module.weight, mean=0.0, std=0.02)

    def forward(self, idx: torch.Tensor, targets: torch.Tensor) -> tuple[torch.Tensor, torch.Tensor]:
        _, seq_len = idx.size()
        if seq_len > self.config.block_size:
            raise ValueError(f"sequence length {seq_len} exceeds block size {self.config.block_size}")
        pos = torch.arange(0, seq_len, dtype=torch.long, device=idx.device)
        x = self.transformer["wte"](idx) + self.transformer["wpe"](pos)
        for block in self.transformer["h"]:
            x = block(x)
        x = self.transformer["ln_f"](x)
        logits = self.lm_head(x)
        loss = F.cross_entropy(logits.view(-1, logits.size(-1)), targets.view(-1))
        return logits, loss


def csv_values(name: str, required: bool = True) -> list[str]:
    raw = os.environ.get(name, "")
    values = [item.strip() for item in raw.split(",")]
    if required and not values:
        raise RuntimeError(f"{name} must be set to a comma-separated list")
    if required and any(value == "" for value in values):
        raise RuntimeError(f"{name} must not contain empty CSV items")
    return values


def env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "")
    if raw == "":
        return default
    return int(raw)


def safe_uri(uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))


def validate_shard_uri(index: int, uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc or not parsed.path:
        raise RuntimeError(
            f"NANOGPT_DATASET_URIS item {index} must be an http(s) URL with a non-empty shard path"
        )

    basename = parsed.path.rsplit("/", 1)[-1]
    if ".bin" in basename and not basename.endswith(".bin"):
        raise RuntimeError(
            f"NANOGPT_DATASET_URIS item {index} has unexpected characters after .bin; "
            f"only put URI data in NANOGPT_DATASET_URIS, not token counts or SHA256s ({safe_uri(uri)})"
        )
    if not basename.endswith(".bin"):
        raise RuntimeError(f"NANOGPT_DATASET_URIS item {index} must point to a .bin shard ({safe_uri(uri)})")
    return uri


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(8 * 1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def download_shard(uri: str, expected_sha256: str, rank: int) -> Path:
    cache_dir = Path("/tmp/nanogpt-openwebtext")
    cache_dir.mkdir(parents=True, exist_ok=True)
    path = cache_dir / f"rank-{rank}-{hashlib.sha256(uri.encode()).hexdigest()[:16]}.bin"
    if path.exists() and sha256_file(path) == expected_sha256:
        print(f"rank={rank} reusing cached dataset shard {path.name}")
        return path

    tmp_path = path.with_suffix(".tmp")
    for attempt in range(1, 6):
        try:
            print(f"rank={rank} downloading dataset shard attempt={attempt} uri={safe_uri(uri)}")
            with urllib.request.urlopen(uri, timeout=120) as response, tmp_path.open("wb") as out:
                while True:
                    chunk = response.read(8 * 1024 * 1024)
                    if not chunk:
                        break
                    out.write(chunk)
            digest = sha256_file(tmp_path)
            if digest != expected_sha256:
                raise RuntimeError(f"sha256 mismatch: got {digest}, expected {expected_sha256}")
            tmp_path.replace(path)
            return path
        except (OSError, RuntimeError, urllib.error.URLError) as exc:
            tmp_path.unlink(missing_ok=True)
            if attempt == 5:
                raise RuntimeError(
                    f"failed to download dataset shard {safe_uri(uri)} after {attempt} attempts: {type(exc).__name__}"
                ) from exc
            time.sleep((2**attempt) + random.uniform(0, 3))
    raise AssertionError("unreachable")


def parse_dataset_contract() -> tuple[list[str], list[str], list[int]]:
    uris = [
        validate_shard_uri(index, value)
        for index, value in enumerate(csv_values("NANOGPT_DATASET_URIS"), start=1)
    ]
    sha256s = []
    for index, value in enumerate(csv_values("NANOGPT_DATASET_SHA256S"), start=1):
        if not re.fullmatch(r"[0-9a-fA-F]{64}", value):
            raise RuntimeError(
                f"NANOGPT_DATASET_SHA256S item {index} must be a 64-character hexadecimal SHA256 digest"
            )
        sha256s.append(value.lower())

    token_counts = []
    for index, value in enumerate(csv_values("NANOGPT_DATASET_TOKEN_COUNTS"), start=1):
        if not re.fullmatch(r"[1-9][0-9]*", value):
            raise RuntimeError(f"NANOGPT_DATASET_TOKEN_COUNTS item {index} must be a positive integer token count")
        token_counts.append(int(value))

    if len(uris) != len(sha256s):
        raise RuntimeError("NANOGPT_DATASET_URIS and NANOGPT_DATASET_SHA256S must have the same length")
    if len(uris) != len(token_counts):
        raise RuntimeError("NANOGPT_DATASET_URIS and NANOGPT_DATASET_TOKEN_COUNTS must have the same length")
    min_total_tokens = env_int("NANOGPT_MIN_TOTAL_TOKENS", 100_000_000)
    total_tokens = sum(token_counts)
    if total_tokens < min_total_tokens:
        raise RuntimeError(
            f"dataset has {total_tokens} declared tokens, below required minimum {min_total_tokens}"
        )
    return uris, sha256s, token_counts


def get_batch(
    tokens: np.memmap,
    batch_size: int,
    block_size: int,
    vocab_size: int,
    rank: int,
    step: int,
    device: torch.device,
) -> tuple[torch.Tensor, torch.Tensor]:
    max_start = len(tokens) - block_size - 1
    if max_start <= 0:
        raise RuntimeError(f"dataset shard has {len(tokens)} tokens, too small for block_size={block_size}")
    starts = np.random.randint(0, max_start, size=batch_size)
    x = np.stack([tokens[start : start + block_size].astype(np.int64) for start in starts])
    y = np.stack([tokens[start + 1 : start + 1 + block_size].astype(np.int64) for start in starts])
    max_token = max(int(x.max()), int(y.max()))
    if max_token >= vocab_size:
        raise RuntimeError(
            f"rank={rank} step={step} sampled token id {max_token} exceeds vocab_size={vocab_size}; "
            "increase NANOGPT_VOCAB_SIZE or use GPT-2/OpenWebText-compatible token shards"
        )
    return torch.from_numpy(x).to(device), torch.from_numpy(y).to(device)


def train_loop(config: dict) -> None:
    ctx = train.get_context()
    rank = ctx.get_world_rank()
    world_size = ctx.get_world_size()
    expected_world_size = int(config["num_workers"])
    if world_size != expected_world_size:
        raise RuntimeError(f"expected world_size={expected_world_size}, got {world_size}")

    device = ray_train_torch.get_device()
    if device.type != "cuda":
        raise RuntimeError(f"rank={rank} expected CUDA device, got {device}")
    torch.cuda.set_device(device)

    dataset_uris = config["dataset_uris"]
    dataset_sha256s = config["dataset_sha256s"]
    shard_index = rank % len(dataset_uris)
    time.sleep((rank % 8) * 2)
    shard_path = download_shard(dataset_uris[shard_index], dataset_sha256s[shard_index], rank)
    tokens = np.memmap(shard_path, dtype=np.uint16, mode="r")

    block_size = int(config["block_size"])
    batch_size = int(config["batch_size"])
    vocab_size = int(config["vocab_size"])
    steps = int(config["steps"])
    if vocab_size <= 0:
        raise RuntimeError(f"vocab_size must be positive, got {vocab_size}")
    min_needed = block_size + batch_size * steps + 1
    if len(tokens) < min_needed:
        raise RuntimeError(f"rank={rank} shard has {len(tokens)} tokens, below minimum needed {min_needed}")

    np.random.seed(int(config["seed"]) + rank)
    torch.manual_seed(int(config["seed"]) + rank)
    print(
        " ".join(
            [
                f"rank={rank}",
                f"world_size={world_size}",
                "device=cuda",
                f"cuda_visible_devices={os.environ.get('CUDA_VISIBLE_DEVICES', '')}",
                f"gpu_name={torch.cuda.get_device_name(device)}",
                f"shard_index={shard_index}",
                f"tokens={len(tokens)}",
                f"vocab_size={vocab_size}",
            ]
        )
    )

    model = GPT(GPTConfig(block_size=block_size, vocab_size=vocab_size)).to(device)
    model = ray_train_torch.prepare_model(model)
    optimizer = torch.optim.AdamW(model.parameters(), lr=float(config["learning_rate"]), betas=(0.9, 0.95))
    report_every = int(config["report_every"])

    last_loss = math.nan
    for step in range(1, steps + 1):
        xb, yb = get_batch(tokens, batch_size, block_size, vocab_size, rank, step, device)
        _, loss = model(xb, yb)
        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
        optimizer.step()

        last_loss = float(loss.detach().cpu())
        if not math.isfinite(last_loss):
            raise RuntimeError(f"rank={rank} produced non-finite loss at step={step}: {last_loss}")
        if step % report_every == 0 or step == steps:
            print(f"rank={rank} step={step} loss={last_loss:.4f}")
            train.report(
                {
                    "step": step,
                    "loss": last_loss,
                    "loss_is_finite": True,
                    "world_size": world_size,
                }
            )


def final_metrics_from_result(result) -> dict:
    metrics = getattr(result, "metrics", None)
    if metrics:
        return dict(metrics)

    metrics_dataframe = getattr(result, "metrics_dataframe", None)
    if metrics_dataframe is not None and not getattr(metrics_dataframe, "empty", True):
        return dict(metrics_dataframe.iloc[-1].to_dict())

    return {}


def main() -> None:
    dataset_uris, dataset_sha256s, token_counts = parse_dataset_contract()
    workers = env_int("NANOGPT_TRAIN_WORKERS", 16)
    steps = env_int("NANOGPT_TRAIN_STEPS", 2000)
    block_size = env_int("NANOGPT_BLOCK_SIZE", 256)
    batch_size = env_int("NANOGPT_BATCH_SIZE", 2)
    vocab_size = env_int("NANOGPT_VOCAB_SIZE", 65536)
    report_every = env_int("NANOGPT_REPORT_EVERY", 25)

    ray.init()
    resources = ray.cluster_resources()
    available_gpus = int(resources.get("GPU", 0))
    print(f"Cluster resources: {resources}")
    if available_gpus < workers:
        raise RuntimeError(f"expected at least {workers} Ray GPU resources, got {available_gpus}")

    trainer = TorchTrainer(
        train_loop_per_worker=train_loop,
        train_loop_config={
            "dataset_uris": dataset_uris,
            "dataset_sha256s": dataset_sha256s,
            "dataset_token_counts": token_counts,
            "num_workers": workers,
            "steps": steps,
            "block_size": block_size,
            "batch_size": batch_size,
            "vocab_size": vocab_size,
            "report_every": report_every,
            "learning_rate": 3e-4,
            "seed": 1337,
        },
        scaling_config=ScalingConfig(num_workers=workers, use_gpu=True),
        run_config=RunConfig(name="nanogpt-ray-train-large-gpu"),
    )
    result = trainer.fit()
    metrics = final_metrics_from_result(result)
    final_step = int(metrics.get("step", steps))
    world_size = int(metrics.get("world_size", workers))
    loss_metric = metrics.get("loss")
    final_loss = float(loss_metric) if loss_metric is not None else math.nan
    if final_step != steps:
        raise RuntimeError(f"expected final step={steps}, got {final_step}")
    if world_size != workers:
        raise RuntimeError(f"expected world_size={workers}, got {world_size}")
    if loss_metric is not None and not math.isfinite(final_loss):
        raise RuntimeError(f"final loss is not finite: {final_loss}")
    final_loss_text = f"{final_loss:.4f}" if loss_metric is not None else "unreported"

    print(
        f"NANOGPT_RAY_TRAIN_SUCCESS step={final_step} world_size={world_size} "
        f"workers={workers} final_loss={final_loss_text} dataset_tokens={sum(token_counts)}"
    )


if __name__ == "__main__":
    main()
