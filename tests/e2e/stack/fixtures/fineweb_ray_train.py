# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Ray Train ~1.7B FineWeb pre-training workload for the 16xH200 InfiniBand
conformance test.

This is the IB/large-model sibling of nanogpt_ray_train.py. It differs in three
ways that matter for the conformance contract:

  1. The model is a real ~1.7B-parameter GPT (default 32 layers / 2048 hidden /
     16 heads / 1024 block), sharded across all 16 H200 GPUs with PyTorch FSDP
     (FULL_SHARD) so parameter and optimizer state are genuinely partitioned.
  2. It writes a real model checkpoint at a configurable interval. The FULL state
     dict is a *collective*: every rank enters the FSDP state-dict context and
     calls state_dict(); only rank 0 receives the gathered weights and writes
     them to a cleanup-excluded hostPath. A dist.barrier() rendezvous follows.
  3. The success contract is "reach + persist the FIRST checkpoint", emitted as a
     single parseable sentinel line the Go test asserts on:
         FINEWEB_FIRST_CHECKPOINT step=<N> path=<...> bytes=<size> params=<count>

The dataset contract mirrors nanoGPT: fixed, sha256-verified pre-tokenized
uint16 shards (sourced from FineWeb sample-10BT), wired via FINEWEB_DATASET_*.
"""

from __future__ import annotations

import functools
import hashlib
import math
import os
import random
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import ray
import torch
import torch.distributed as dist
import torch.nn as nn
import torch.nn.functional as F
from ray import train
from ray.train import RunConfig, ScalingConfig
from ray.train import torch as ray_train_torch
from ray.train.torch import TorchTrainer
from torch.distributed.fsdp import FullyShardedDataParallel as FSDP
from torch.distributed.fsdp import (
    FullStateDictConfig,
    MixedPrecision,
    ShardingStrategy,
    StateDictType,
)
from torch.distributed.fsdp.wrap import transformer_auto_wrap_policy


DEFAULT_FINEWEB_VOCAB_SIZE = 65536


@dataclass(frozen=True)
class GPTConfig:
    block_size: int
    vocab_size: int = DEFAULT_FINEWEB_VOCAB_SIZE
    n_layer: int = 32
    n_head: int = 16
    n_embd: int = 2048
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
        # Weight tying keeps the param count at ~1.7B (vs ~1.8B untied). wte and
        # lm_head both live in the root FSDP unit (Blocks are wrapped separately),
        # so the shared tensor is preserved under use_orig_params=True.
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
    values = [item.strip() for item in raw.split(",") if item.strip()]
    if required and not values:
        raise RuntimeError(f"{name} must be set to a comma-separated list")
    return values


def env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "")
    if raw == "":
        return default
    return int(raw)


def effective_vocab_size(configured_vocab_size: int) -> int:
    if configured_vocab_size <= 0:
        raise RuntimeError(f"vocab_size must be positive, got {configured_vocab_size}")
    return max(configured_vocab_size, DEFAULT_FINEWEB_VOCAB_SIZE)


def safe_uri(uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(8 * 1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def download_shard(uri: str, expected_sha256: str, rank: int) -> Path:
    cache_dir = Path("/tmp/fineweb-shards")
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
    uris = csv_values("FINEWEB_DATASET_URIS")
    sha256s = csv_values("FINEWEB_DATASET_SHA256S")
    token_counts = [int(v) for v in csv_values("FINEWEB_DATASET_TOKEN_COUNTS")]
    if len(uris) != len(sha256s):
        raise RuntimeError("FINEWEB_DATASET_URIS and FINEWEB_DATASET_SHA256S must have the same length")
    if len(uris) != len(token_counts):
        raise RuntimeError("FINEWEB_DATASET_URIS and FINEWEB_DATASET_TOKEN_COUNTS must have the same length")
    min_total_tokens = env_int("FINEWEB_MIN_TOTAL_TOKENS", 10_000_000)
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
            "increase FINEWEB_VOCAB_SIZE or use GPT-2/FineWeb-compatible token shards"
        )
    return torch.from_numpy(x).to(device), torch.from_numpy(y).to(device)


def build_fsdp_model(config: GPTConfig, device: torch.device) -> tuple[FSDP, int]:
    """Construct the GPT on CPU, count its (deduplicated, tie-aware) parameters,
    then wrap it with FSDP FULL_SHARD + bf16 mixed precision, sharding each
    transformer Block as its own unit."""
    model = GPT(config)
    param_count = sum(p.numel() for p in model.parameters())

    auto_wrap_policy = functools.partial(
        transformer_auto_wrap_policy, transformer_layer_cls={Block}
    )
    mixed_precision = MixedPrecision(
        param_dtype=torch.bfloat16,
        reduce_dtype=torch.bfloat16,
        buffer_dtype=torch.bfloat16,
    )
    fsdp_model = FSDP(
        model,
        auto_wrap_policy=auto_wrap_policy,
        mixed_precision=mixed_precision,
        sharding_strategy=ShardingStrategy.FULL_SHARD,
        device_id=device,
        use_orig_params=True,
    )
    return fsdp_model, param_count


def save_first_checkpoint(
    model: FSDP,
    checkpoint_dir: Path,
    step: int,
    param_count: int,
    rank: int,
) -> None:
    """Gather and persist a FULL_STATE_DICT checkpoint.

    The state-dict gather is a collective: ALL ranks must enter the context and
    call state_dict(); only rank 0 receives the materialized weights. Rank 0
    writes them, re-stats the file (bytes>0) before any pod teardown, and emits
    the parseable first-checkpoint sentinel. A barrier rendezvous follows so no
    rank races ahead of the persisted artifact.
    """
    save_policy = FullStateDictConfig(offload_to_cpu=True, rank0_only=True)
    with FSDP.state_dict_type(model, StateDictType.FULL_STATE_DICT, save_policy):
        cpu_state = model.state_dict()

    if rank == 0:
        checkpoint_dir.mkdir(parents=True, exist_ok=True)
        checkpoint_path = checkpoint_dir / f"fineweb-step-{step}.pt"
        tmp_path = checkpoint_path.with_suffix(".pt.tmp")
        torch.save({"step": step, "params": param_count, "model": cpu_state}, tmp_path)
        tmp_path.replace(checkpoint_path)
        size_bytes = checkpoint_path.stat().st_size
        if size_bytes <= 0:
            raise RuntimeError(f"checkpoint {checkpoint_path} persisted with {size_bytes} bytes")
        print(
            f"FINEWEB_FIRST_CHECKPOINT step={step} path={checkpoint_path} "
            f"bytes={size_bytes} params={param_count}",
            flush=True,
        )

    if dist.is_available() and dist.is_initialized():
        dist.barrier()


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
    checkpoint_interval = int(config["checkpoint_interval"])
    checkpoint_dir = Path(config["checkpoint_dir"])
    if checkpoint_interval <= 0:
        raise RuntimeError(f"checkpoint_interval must be positive, got {checkpoint_interval}")
    if steps < checkpoint_interval:
        raise RuntimeError(
            f"steps={steps} must be >= checkpoint_interval={checkpoint_interval} so the first checkpoint is reached"
        )
    min_needed = block_size + batch_size * steps + 1
    if len(tokens) < min_needed:
        raise RuntimeError(f"rank={rank} shard has {len(tokens)} tokens, below minimum needed {min_needed}")

    np.random.seed(int(config["seed"]) + rank)
    torch.manual_seed(int(config["seed"]) + rank)

    model, param_count = build_fsdp_model(
        GPTConfig(
            block_size=block_size,
            vocab_size=vocab_size,
            n_layer=int(config["n_layer"]),
            n_head=int(config["n_head"]),
            n_embd=int(config["n_embd"]),
        ),
        device,
    )
    optimizer = torch.optim.AdamW(model.parameters(), lr=float(config["learning_rate"]), betas=(0.9, 0.95))
    report_every = int(config["report_every"])

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
                f"params={param_count}",
            ]
        ),
        flush=True,
    )

    first_checkpoint_done = False
    last_loss = math.nan
    for step in range(1, steps + 1):
        xb, yb = get_batch(tokens, batch_size, block_size, vocab_size, rank, step, device)
        _, loss = model(xb, yb)
        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        model.clip_grad_norm_(1.0)
        optimizer.step()

        last_loss = float(loss.detach().cpu())
        if not math.isfinite(last_loss):
            raise RuntimeError(f"rank={rank} produced non-finite loss at step={step}: {last_loss}")
        if step % report_every == 0 or step == steps:
            print(f"rank={rank} step={step} loss={last_loss:.4f}", flush=True)
            train.report(
                {
                    "step": step,
                    "loss": last_loss,
                    "loss_is_finite": True,
                    "world_size": world_size,
                    "params": param_count,
                }
            )

        if not first_checkpoint_done and step % checkpoint_interval == 0:
            save_first_checkpoint(model, checkpoint_dir, step, param_count, rank)
            first_checkpoint_done = True

    if not first_checkpoint_done:
        raise RuntimeError(f"rank={rank} finished {steps} steps without writing a checkpoint")


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
    workers = env_int("FINEWEB_TRAIN_WORKERS", 16)
    steps = env_int("FINEWEB_TRAIN_STEPS", 60)
    checkpoint_interval = env_int("FINEWEB_CHECKPOINT_INTERVAL", 50)
    block_size = env_int("FINEWEB_BLOCK_SIZE", 1024)
    batch_size = env_int("FINEWEB_BATCH_SIZE", 1)
    configured_vocab_size = env_int("FINEWEB_VOCAB_SIZE", DEFAULT_FINEWEB_VOCAB_SIZE)
    vocab_size = effective_vocab_size(configured_vocab_size)
    n_layer = env_int("FINEWEB_N_LAYER", 32)
    n_head = env_int("FINEWEB_N_HEAD", 16)
    n_embd = env_int("FINEWEB_N_EMBD", 2048)
    report_every = env_int("FINEWEB_REPORT_EVERY", 10)
    checkpoint_dir = os.environ.get("FINEWEB_CHECKPOINT_DIR", "/mnt/fineweb-ckpt")

    ray.init()
    resources = ray.cluster_resources()
    available_gpus = int(resources.get("GPU", 0))
    print(f"Cluster resources: {resources}")
    if vocab_size != configured_vocab_size:
        print(
            f"FINEWEB_EFFECTIVE_VOCAB_SIZE configured={configured_vocab_size} effective={vocab_size}",
            flush=True,
        )
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
            "checkpoint_interval": checkpoint_interval,
            "checkpoint_dir": checkpoint_dir,
            "block_size": block_size,
            "batch_size": batch_size,
            "vocab_size": vocab_size,
            "n_layer": n_layer,
            "n_head": n_head,
            "n_embd": n_embd,
            "report_every": report_every,
            "learning_rate": 3e-4,
            "seed": 1337,
        },
        scaling_config=ScalingConfig(num_workers=workers, use_gpu=True),
        run_config=RunConfig(name="fineweb-ray-train-16xh200-ib"),
    )
    result = trainer.fit()
    metrics = final_metrics_from_result(result)
    final_step = int(metrics.get("step", steps))
    world_size = int(metrics.get("world_size", workers))
    params = int(metrics.get("params", 0))
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
        f"FINEWEB_RAY_TRAIN_SUCCESS step={final_step} world_size={world_size} "
        f"workers={workers} params={params} final_loss={final_loss_text} "
        f"dataset_tokens={sum(token_counts)}",
        flush=True,
    )


if __name__ == "__main__":
    main()
