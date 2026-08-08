#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""PyTorch nanoGPT-style Tau/Ray smoke and scale experiment.

The full llm.c discussion #481 target is GPT-2 124M on FineWeb. This script is a
small, config-driven PyTorch/Ray implementation that uses the same causal-LM
training shape and can scale up by editing tau.yaml/env fields.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import queue
import random
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from contextlib import nullcontext
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Iterable, Sequence

import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F


@dataclass
class GPTConfig:
    vocab_size: int = 50304
    block_size: int = 128
    n_layer: int = 4
    n_head: int = 4
    n_embd: int = 256
    dropout: float = 0.0
    use_rmsnorm: bool = False
    activation: str = "gelu"


def make_norm(cfg: GPTConfig) -> nn.Module:
    if cfg.use_rmsnorm:
        return nn.RMSNorm(cfg.n_embd)
    return nn.LayerNorm(cfg.n_embd)


class CausalSelfAttention(nn.Module):
    def __init__(self, cfg: GPTConfig):
        super().__init__()
        assert cfg.n_embd % cfg.n_head == 0
        self.n_head = cfg.n_head
        self.head_dim = cfg.n_embd // cfg.n_head
        self.c_attn = nn.Linear(cfg.n_embd, 3 * cfg.n_embd)
        self.c_proj = nn.Linear(cfg.n_embd, cfg.n_embd)
        self.dropout = cfg.dropout

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        bsz, seq, channels = x.shape
        q, k, v = self.c_attn(x).split(channels, dim=2)
        q = q.view(bsz, seq, self.n_head, self.head_dim).transpose(1, 2)
        k = k.view(bsz, seq, self.n_head, self.head_dim).transpose(1, 2)
        v = v.view(bsz, seq, self.n_head, self.head_dim).transpose(1, 2)
        y = F.scaled_dot_product_attention(q, k, v, is_causal=True, dropout_p=self.dropout if self.training else 0.0)
        y = y.transpose(1, 2).contiguous().view(bsz, seq, channels)
        return self.c_proj(y)


class MLP(nn.Module):
    def __init__(self, cfg: GPTConfig):
        super().__init__()
        self.cfg = cfg
        self.c_fc = nn.Linear(cfg.n_embd, 4 * cfg.n_embd)
        self.c_proj = nn.Linear(4 * cfg.n_embd, cfg.n_embd)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        x = self.c_fc(x)
        if self.cfg.activation == "relu2":
            x = F.relu(x).square()
        else:
            x = F.gelu(x, approximate="tanh")
        return self.c_proj(x)


class Block(nn.Module):
    def __init__(self, cfg: GPTConfig):
        super().__init__()
        self.ln_1 = make_norm(cfg)
        self.attn = CausalSelfAttention(cfg)
        self.ln_2 = make_norm(cfg)
        self.mlp = MLP(cfg)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        x = x + self.attn(self.ln_1(x))
        x = x + self.mlp(self.ln_2(x))
        return x


class GPT(nn.Module):
    def __init__(self, cfg: GPTConfig):
        super().__init__()
        self.cfg = cfg
        self.tok_emb = nn.Embedding(cfg.vocab_size, cfg.n_embd)
        self.pos_emb = nn.Embedding(cfg.block_size, cfg.n_embd)
        self.blocks = nn.ModuleList([Block(cfg) for _ in range(cfg.n_layer)])
        self.ln_f = make_norm(cfg)
        self.lm_head = nn.Linear(cfg.n_embd, cfg.vocab_size, bias=False)
        self.lm_head.weight = self.tok_emb.weight
        self.apply(self._init_weights)

    @staticmethod
    def _init_weights(module: nn.Module) -> None:
        if isinstance(module, nn.Linear):
            nn.init.normal_(module.weight, mean=0.0, std=0.02)
            if module.bias is not None:
                nn.init.zeros_(module.bias)
        elif isinstance(module, nn.Embedding):
            nn.init.normal_(module.weight, mean=0.0, std=0.02)

    def forward(self, idx: torch.Tensor, targets: torch.Tensor | None = None) -> tuple[torch.Tensor, torch.Tensor | None]:
        _, seq = idx.shape
        pos = torch.arange(0, seq, dtype=torch.long, device=idx.device)
        x = self.tok_emb(idx) + self.pos_emb(pos)
        for block in self.blocks:
            x = block(x)
        logits = self.lm_head(self.ln_f(x))
        loss = None
        if targets is not None:
            loss = F.cross_entropy(logits.view(-1, logits.size(-1)), targets.reshape(-1))
        return logits, loss


def env_int(name: str, default: int) -> int:
    return int(os.environ.get(name, str(default)))


def env_float(name: str, default: float) -> float:
    return float(os.environ.get(name, str(default)))


def candidate_token_files(root: Path) -> Iterable[Path]:
    bases = []
    if os.environ.get("NANOGPT_FINEWEB_DIR"):
        bases.append(Path(os.environ["NANOGPT_FINEWEB_DIR"]))
    bases.extend([
        root / "datasets" / "fineweb",
        root / "fineweb",
        Path("/data/datasets/fineweb"),
        Path("/data/fineweb"),
    ])
    for base in bases:
        if base.exists():
            for pattern in ("**/*.bin", "**/*.npy", "**/*.pt", "**/*.txt"):
                yield from sorted(base.glob(pattern))


def append_jsonl(path: Path, row: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(row, sort_keys=True) + "\n")


class AsyncJSONLLogger:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._queue: queue.Queue[dict | None] = queue.Queue()
        self._thread = threading.Thread(target=self._run, name="stellar-jsonl-logger", daemon=True)
        self._thread.start()

    def log(self, row: dict) -> None:
        self._queue.put(dict(row))

    def close(self) -> None:
        self._queue.put(None)
        self._queue.join()
        self._thread.join(timeout=10)

    def __enter__(self) -> "AsyncJSONLLogger":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def _run(self) -> None:
        with self.path.open("a", encoding="utf-8") as handle:
            while True:
                row = self._queue.get()
                try:
                    if row is None:
                        return
                    handle.write(json.dumps(row, sort_keys=True) + "\n")
                    handle.flush()
                finally:
                    self._queue.task_done()


def log_jsonl(sink: Path | AsyncJSONLLogger | None, row: dict) -> None:
    if sink is None:
        return
    if isinstance(sink, AsyncJSONLLogger):
        sink.log(row)
        return
    append_jsonl(sink, row)


def initialize_history(config: dict) -> Path:
    history_path = Path(config["out_dir"]) / "stellar" / "history.jsonl"
    history_path.parent.mkdir(parents=True, exist_ok=True)
    history_path.unlink(missing_ok=True)
    config["history_initialized"] = True
    return history_path


def prepare_fineweb_smoke_tokens(root: Path, target_tokens: int, history_path: Path | AsyncJSONLLogger | None = None) -> Path | None:
    if os.environ.get("NANOGPT_PREPARE_FINEWEB", "0") != "1":
        return None
    out_dir = Path(os.environ.get("NANOGPT_FINEWEB_DIR", "")) if os.environ.get("NANOGPT_FINEWEB_DIR") else root / "datasets" / "fineweb"
    out_path = out_dir / "tau_smoke_tokens.bin"
    if out_path.exists() and out_path.stat().st_size > 0:
        log_jsonl(
            history_path,
            {
                "_step": -1,
                "_timestamp": time.time(),
                "data/stage_status": "reused",
                "data/staged_bytes": out_path.stat().st_size,
                "data/staged_tokens": out_path.stat().st_size // 2,
                "data/target_tokens": target_tokens,
            },
        )
        return out_path
    try:
        import tiktoken
        from datasets import load_dataset

        out_dir.mkdir(parents=True, exist_ok=True)
        tokenizer = tiktoken.get_encoding("gpt2")
        dataset_name = os.environ.get("NANOGPT_FINEWEB_DATASET", "HuggingFaceFW/fineweb-edu")
        dataset_config = os.environ.get("NANOGPT_FINEWEB_CONFIG", "sample-10BT")
        dataset = load_dataset(dataset_name, name=dataset_config, split="train", streaming=True)
        chunk_tokens = env_int("NANOGPT_PREPARE_CHUNK_TOKENS", 1_000_000)
        progress_tokens = env_int("NANOGPT_PREPARE_PROGRESS_TOKENS", 10_000_000)
        tokens: list[int] = []
        written = 0
        next_progress = progress_tokens
        started = time.time()
        tmp = out_path.with_suffix(".tmp")
        tmp.unlink(missing_ok=True)
        handle = tmp.open("wb")
        log_jsonl(
            history_path,
            {
                "_step": -1,
                "_timestamp": started,
                "data/stage_status": "started",
                "data/source": f"{dataset_name}/{dataset_config}",
                "data/target_tokens": target_tokens,
            },
        )
        for row in dataset:
            text = str(row.get("text") or "")
            if not text:
                continue
            tokens.extend(tokenizer.encode_ordinary(text))
            if written + len(tokens) >= target_tokens:
                keep = target_tokens - written
                if keep > 0:
                    np.asarray(tokens[:keep], dtype=np.uint16).tofile(handle)
                    written += keep
                tokens.clear()
                break
            while len(tokens) >= chunk_tokens:
                np.asarray(tokens[:chunk_tokens], dtype=np.uint16).tofile(handle)
                del tokens[:chunk_tokens]
                written += chunk_tokens
                if written >= next_progress:
                    print(f"prepared FineWeb tokens progress={written}", flush=True)
                    elapsed = max(1e-9, time.time() - started)
                    log_jsonl(
                        history_path,
                        {
                            "_step": -1,
                            "_timestamp": time.time(),
                            "data/stage_status": "running",
                            "data/staged_bytes": written * 2,
                            "data/staged_tokens": written,
                            "data/stage_elapsed_seconds": elapsed,
                            "data/stage_tokens_per_second": written / elapsed,
                            "data/target_tokens": target_tokens,
                        },
                    )
                    next_progress += progress_tokens
        if tokens:
            np.asarray(tokens, dtype=np.uint16).tofile(handle)
            written += len(tokens)
            tokens.clear()
        handle.close()
        if written <= 2:
            tmp.unlink(missing_ok=True)
            return None
        tmp.replace(out_path)
        print(f"prepared FineWeb token shard: {out_path} dataset={dataset_name}/{dataset_config} tokens={written}", flush=True)
        elapsed = max(1e-9, time.time() - started)
        log_jsonl(
            history_path,
            {
                "_step": -1,
                "_timestamp": time.time(),
                "data/stage_status": "complete",
                "data/staged_bytes": written * 2,
                "data/staged_tokens": written,
                "data/stage_elapsed_seconds": elapsed,
                "data/stage_tokens_per_second": written / elapsed,
                "data/target_tokens": target_tokens,
            },
        )
        return out_path
    except Exception as exc:
        try:
            handle.close()  # type: ignore[possibly-undefined]
        except Exception:
            pass
        try:
            tmp.unlink(missing_ok=True)  # type: ignore[possibly-undefined]
        except Exception:
            pass
        log_jsonl(
            history_path,
            {
                "_step": -1,
                "_timestamp": time.time(),
                "data/stage_status": "error",
                "data/stage_error": repr(exc),
                "data/target_tokens": target_tokens,
            },
        )
        print(f"warning: failed to prepare FineWeb smoke tokens: {exc}", flush=True)
        return None


def csv_env(name: str) -> list[str]:
    return [item.strip() for item in os.environ.get(name, "").split(",") if item.strip()]


def safe_uri(uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(8 * 1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def download_token_shard(uri: str, expected_sha256: str | None, rank: int, kind: str) -> Path:
    cache_dir = Path(os.environ.get("NANOGPT_SHARD_CACHE_DIR", "/tmp/nanogpt-shards"))
    cache_dir.mkdir(parents=True, exist_ok=True)
    path = cache_dir / f"{kind}-rank-{rank}-{hashlib.sha256(uri.encode()).hexdigest()[:16]}.bin"
    if path.exists() and (not expected_sha256 or sha256_file(path) == expected_sha256):
        print(f"rank={rank} reusing cached {kind} shard {path.name}", flush=True)
        return path
    tmp_path = path.with_suffix(".tmp")
    for attempt in range(1, 6):
        try:
            print(f"rank={rank} downloading {kind} shard attempt={attempt} uri={safe_uri(uri)}", flush=True)
            with urllib.request.urlopen(uri, timeout=120) as response, tmp_path.open("wb") as out:
                while True:
                    chunk = response.read(8 * 1024 * 1024)
                    if not chunk:
                        break
                    out.write(chunk)
            if expected_sha256:
                digest = sha256_file(tmp_path)
                if digest.lower() != expected_sha256.lower():
                    raise RuntimeError(f"sha256 mismatch: got {digest}, expected {expected_sha256}")
            tmp_path.replace(path)
            return path
        except (OSError, RuntimeError, urllib.error.URLError) as exc:
            tmp_path.unlink(missing_ok=True)
            if attempt == 5:
                raise RuntimeError(f"failed to download {kind} shard {safe_uri(uri)} after {attempt} attempts") from exc
            time.sleep((2**attempt) + random.uniform(0, 3))
    raise AssertionError("unreachable")


def load_uri_tokens(rank: int, *, kind: str) -> tuple[np.memmap | None, str]:
    prefix = "NANOGPT_VAL_DATASET" if kind == "validation" else "NANOGPT_DATASET"
    uris = csv_env(prefix + "_URIS")
    if not uris:
        return None, ""
    sha256s = csv_env(prefix + "_SHA256S")
    if sha256s and len(sha256s) != len(uris):
        raise RuntimeError(f"{prefix}_SHA256S must have the same length as {prefix}_URIS")
    index = rank % len(uris)
    shard = download_token_shard(uris[index], sha256s[index] if sha256s else None, rank, kind)
    return np.memmap(shard, dtype=np.uint16, mode="r"), safe_uri(uris[index])


def load_tokens(data_root: Path, vocab_size: int, fallback_tokens: int, *, attempted_prepare: bool = False, rank: int = 0) -> tuple[torch.Tensor | np.memmap, str]:
    uri_tokens, uri_source = load_uri_tokens(rank, kind="train")
    if uri_tokens is not None:
        return uri_tokens, uri_source
    for path in candidate_token_files(data_root):
        try:
            if path.suffix == ".pt":
                raw = torch.load(path, map_location="cpu")
                tokens = raw if isinstance(raw, torch.Tensor) else torch.tensor(raw)
            elif path.suffix == ".npy":
                tokens = np.load(path, mmap_mode="r")
            elif path.suffix == ".bin":
                # llm.c/nanoGPT token dumps are commonly uint16 token ids.
                tokens = np.memmap(path, dtype=np.uint16, mode="r")
            else:
                data = path.read_bytes()
                tokens = torch.tensor(list(data), dtype=torch.long)
            if token_count(tokens) > 2:
                return tokens, str(path)
        except Exception as exc:  # keep looking; one stale shard should not kill discovery
            print(f"warning: failed to load token file {path}: {exc}", flush=True)
    prepared = None if attempted_prepare else prepare_fineweb_smoke_tokens(data_root, fallback_tokens)
    if prepared is not None:
        return load_tokens(data_root, vocab_size, fallback_tokens, attempted_prepare=True)
    text = (
        "Tau nanoGPT fallback corpus. FineWeb token shards were not found under "
        "/data/datasets/fineweb, so this run only proves the training path. "
    )
    encoded = torch.tensor(list(text.encode("utf-8")), dtype=torch.long) % vocab_size
    repeat = max(1, math.ceil(fallback_tokens / encoded.numel()))
    return encoded.repeat(repeat)[:fallback_tokens], "fallback:synthetic-byte-corpus"


def load_validation_tokens(data_root: Path, vocab_size: int, fallback_tokens: int, rank: int) -> tuple[torch.Tensor | np.memmap | None, str]:
    uri_tokens, uri_source = load_uri_tokens(rank, kind="validation")
    if uri_tokens is not None:
        return uri_tokens, uri_source
    return None, ""


def load_hellaswag_examples(limit: int) -> tuple[list[dict], str]:
    if limit <= 0:
        return [], "disabled"
    try:
        import tiktoken
        from datasets import load_dataset

        tokenizer = tiktoken.get_encoding("gpt2")
        dataset = load_dataset("Rowan/hellaswag", split="validation", streaming=True)
        examples = []
        for row in dataset:
            ctx = str(row.get("ctx") or (str(row.get("ctx_a") or "") + " " + str(row.get("ctx_b") or ""))).strip()
            endings = list(row.get("endings") or [])
            if not ctx or len(endings) != 4:
                continue
            label = int(row.get("label"))
            examples.append(
                {
                    "context": tokenizer.encode_ordinary(ctx),
                    "endings": [tokenizer.encode_ordinary(str(ending)) for ending in endings],
                    "label": label,
                }
            )
            if len(examples) >= limit:
                break
        return examples, "Rowan/hellaswag:validation"
    except Exception as exc:
        print(f"warning: failed to load HellaSwag examples: {exc}", flush=True)
        return [], f"unavailable:{exc}"


def token_count(tokens: torch.Tensor | np.ndarray | np.memmap) -> int:
    if isinstance(tokens, torch.Tensor):
        return int(tokens.numel())
    return int(tokens.size)


def token_window(tokens: torch.Tensor | np.ndarray | np.memmap, start: int, stop: int) -> torch.Tensor:
    if isinstance(tokens, torch.Tensor):
        return tokens[start:stop].long()
    return torch.from_numpy(np.asarray(tokens[start:stop], dtype=np.int64))


def sample_batch(
    tokens: torch.Tensor | np.ndarray | np.memmap,
    batch_size: int,
    block_size: int,
    device: torch.device,
    *,
    token_start: int = 0,
    token_stop: int | None = None,
    vocab_size: int = 50304,
) -> tuple[torch.Tensor, torch.Tensor]:
    if token_stop is None:
        token_stop = token_count(tokens)
    max_start = token_stop - token_start - block_size - 1
    if max_start <= 0:
        raise RuntimeError(f"token range has {token_stop - token_start} tokens, too small for block_size={block_size}")
    starts = torch.randint(0, max_start, (batch_size,))
    x = torch.stack([token_window(tokens, token_start + int(i), token_start + int(i) + block_size) for i in starts]).to(device) % vocab_size
    y = torch.stack([token_window(tokens, token_start + int(i) + 1, token_start + int(i) + 1 + block_size) for i in starts]).to(device) % vocab_size
    return x, y


def eval_batch(
    tokens: torch.Tensor | np.ndarray | np.memmap,
    batch_size: int,
    block_size: int,
    device: torch.device,
    batch_index: int,
    total_batches: int,
    *,
    token_start: int = 0,
    token_stop: int | None = None,
    vocab_size: int = 50304,
) -> tuple[torch.Tensor, torch.Tensor]:
    if token_stop is None:
        token_stop = token_count(tokens)
    max_start = token_stop - token_start - block_size - 1
    if max_start <= 0:
        raise RuntimeError(f"token range has {token_stop - token_start} tokens, too small for block_size={block_size}")
    total = max(1, total_batches * batch_size)
    offset = batch_index * batch_size
    starts = []
    for item in range(batch_size):
        ordinal = offset + item
        if total == 1:
            rel = 0
        else:
            rel = round((max_start - 1) * ordinal / (total - 1))
        starts.append(int(rel))
    x = torch.stack([token_window(tokens, token_start + i, token_start + i + block_size) for i in starts]).to(device) % vocab_size
    y = torch.stack([token_window(tokens, token_start + i + 1, token_start + i + 1 + block_size) for i in starts]).to(device) % vocab_size
    return x, y


def gpt_config(model: nn.Module) -> GPTConfig:
    inner = model
    for _ in range(4):
        if hasattr(inner, "module"):
            inner = inner.module
            continue
        if hasattr(inner, "_orig_mod"):
            inner = inner._orig_mod
            continue
        break
    return inner.cfg


def maybe_autocast(device: torch.device, enabled: bool):
    if enabled and device.type == "cuda":
        return torch.autocast(device_type="cuda", dtype=torch.bfloat16)
    return nullcontext()


@torch.no_grad()
def estimate_loss(
    model: GPT,
    tokens: torch.Tensor | np.ndarray | np.memmap,
    batch_size: int,
    eval_batches: int,
    device: torch.device,
    *,
    token_start: int = 0,
    token_stop: int | None = None,
    use_bf16: bool = False,
    mode: str = "random",
) -> float:
    cfg = gpt_config(model)
    model.eval()
    losses = []
    for batch_index in range(eval_batches):
        if mode == "stride":
            x, y = eval_batch(
                tokens,
                batch_size,
                cfg.block_size,
                device,
                batch_index,
                eval_batches,
                token_start=token_start,
                token_stop=token_stop,
                vocab_size=cfg.vocab_size,
            )
        else:
            x, y = sample_batch(tokens, batch_size, cfg.block_size, device, token_start=token_start, token_stop=token_stop, vocab_size=cfg.vocab_size)
        with maybe_autocast(device, use_bf16):
            _, loss = model(x, y)
        assert loss is not None
        losses.append(float(loss.item()))
    model.train()
    return sum(losses) / len(losses)


@torch.no_grad()
def estimate_hellaswag(model: GPT, examples: list[dict], device: torch.device, *, use_bf16: bool = False) -> float | None:
    if not examples:
        return None
    cfg = gpt_config(model)
    model.eval()
    correct = 0
    for example in examples:
        scores = []
        for ending in example["endings"]:
            ending = ending or [0]
            max_tokens = cfg.block_size + 1
            if len(ending) >= max_tokens:
                sequence = ending[-max_tokens:]
                target_start = 0
            else:
                context_budget = max_tokens - len(ending)
                context = example["context"][-context_budget:]
                sequence = context + ending
                target_start = max(0, len(context) - 1)
            idx = torch.tensor(sequence[:-1], dtype=torch.long, device=device).unsqueeze(0) % cfg.vocab_size
            targets = torch.tensor(sequence[1:], dtype=torch.long, device=device).unsqueeze(0) % cfg.vocab_size
            with maybe_autocast(device, use_bf16):
                logits, _ = model(idx)
            losses = F.cross_entropy(logits.view(-1, logits.size(-1)), targets.reshape(-1), reduction="none")
            scores.append(float(losses[target_start:].mean().item()))
        prediction = min(range(len(scores)), key=scores.__getitem__)
        correct += int(prediction == example["label"])
    model.train()
    return correct / len(examples)


def configure_optimizer(model: nn.Module, lr: float, weight_decay: float) -> torch.optim.Optimizer:
    decay = []
    no_decay = []
    for _, param in model.named_parameters():
        if not param.requires_grad:
            continue
        if param.dim() >= 2:
            decay.append(param)
        else:
            no_decay.append(param)
    return torch.optim.AdamW(
        [
            {"params": decay, "weight_decay": weight_decay},
            {"params": no_decay, "weight_decay": 0.0},
        ],
        lr=lr,
        betas=(0.9, 0.95),
        eps=1e-8,
    )


def learning_rate(step: int, total_steps: int, base_lr: float, min_lr: float, warmup_steps: int, cooldown_frac: float) -> float:
    if warmup_steps > 0 and step < warmup_steps:
        return base_lr * float(step + 1) / float(warmup_steps)
    if total_steps <= warmup_steps:
        return base_lr
    cooldown_frac = min(1.0, max(0.0, cooldown_frac))
    cooldown_start = max(warmup_steps, int(total_steps * (1.0 - cooldown_frac)))
    if cooldown_frac == 0 or step < cooldown_start:
        return base_lr
    progress = min(1.0, float(step - cooldown_start) / float(max(1, total_steps - cooldown_start)))
    return min_lr + 0.5 * (base_lr - min_lr) * (1.0 + math.cos(math.pi * progress))


def safe_perplexity(loss: float) -> float:
    if math.isnan(loss):
        return float("nan")
    return math.exp(min(50.0, loss))


def should_stop_at_target(config: dict, best_val: float, device: torch.device) -> bool:
    consecutive = int(config.get("target_consecutive_evals", 1))
    hits = int(config.get("target_eval_hits", 0))
    if not config["stop_at_target"] or best_val == float("inf") or best_val > float(config["target_val_loss"]) or hits < consecutive:
        local_stop = 0
    else:
        local_stop = 1
    if torch.distributed.is_available() and torch.distributed.is_initialized():
        flag = torch.tensor([local_stop], dtype=torch.int32, device=device)
        torch.distributed.all_reduce(flag, op=torch.distributed.ReduceOp.MAX)
        return bool(flag.item())
    return bool(local_stop)


def train_worker(config: dict) -> dict:
    if config.get("local_smoke"):
        ray_train = None
        prepare_model = lambda model: model
        rank = 0
        world = 1
    else:
        from ray import train as ray_train
        from ray.train.torch import prepare_model

        ctx = ray_train.get_context()
        rank = ctx.get_world_rank()
        world = ctx.get_world_size()
    seed = int(config["seed"]) + rank
    random.seed(seed)
    torch.manual_seed(seed)
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    history_path = Path(config["out_dir"]) / "stellar" / "history.jsonl"
    if config.get("local_smoke") and os.environ.get("NANOGPT_PREPARE_FINEWEB", "0") == "1" and rank == 0:
        prepare_fineweb_smoke_tokens(Path(config["data_root"]), int(config["fallback_tokens"]), history_path)

    gpt_cfg = GPTConfig(**config["model"])
    tokens, dataset_source = load_tokens(Path(config["data_root"]), gpt_cfg.vocab_size, int(config["fallback_tokens"]), attempted_prepare=True, rank=rank)
    val_tokens, validation_source = load_validation_tokens(Path(config["data_root"]), gpt_cfg.vocab_size, int(config["fallback_tokens"]), rank)
    split = int(token_count(tokens) * float(config["val_fraction"]))
    train_start = 0
    train_stop = split
    if val_tokens is None:
        train_stop = split
        val_start = split
        val_stop = token_count(tokens)
        has_val = token_count(tokens) - split > gpt_cfg.block_size + 1
        validation_source = f"heldout-split:{dataset_source}" if has_val else ""
        if has_val:
            val_tokens = tokens
    else:
        train_stop = token_count(tokens)
        val_start = 0
        val_stop = token_count(val_tokens)
        has_val = token_count(val_tokens) > gpt_cfg.block_size + 1
    if not has_val:
        val_tokens = None

    raw_model = GPT(gpt_cfg).to(device)
    param_count = sum(p.numel() for p in raw_model.parameters())
    model = prepare_model(raw_model)
    if config["use_compile"]:
        if not hasattr(torch, "compile"):
            raise RuntimeError("NANOGPT_USE_COMPILE=1 requires torch.compile")
        model = torch.compile(model, dynamic=False)
    if config.get("ddp_probe"):
        if not config.get("local_smoke"):
            import torch.distributed as dist

            if dist.is_available() and dist.is_initialized():
                probe = torch.tensor([rank + 1], dtype=torch.float32, device=device)
                dist.all_reduce(probe)
                expected = world * (world + 1) / 2
                if float(probe.item()) != expected:
                    raise RuntimeError(f"DDP probe sum={probe.item()} expected={expected}")
        if rank == 0:
            out_dir = Path(config["out_dir"])
            out_dir.mkdir(parents=True, exist_ok=True)
            (out_dir / "ddp_probe.json").write_text(
                json.dumps(
                    {
                        "run": config["run_name"],
                        "world_size": world,
                        "device": str(device),
                        "parameter_count": param_count,
                        "ddp_probe": "ok",
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
        return {"ddp_probe": 1.0}
    optimizer = configure_optimizer(model, float(config["lr"]), float(config["weight_decay"]))
    hellaswag_examples, hellaswag_source = load_hellaswag_examples(int(config["hellaswag_examples"])) if rank == 0 else ([], "rank-nonzero")
    history_path.parent.mkdir(parents=True, exist_ok=True)
    if rank == 0 and not config.get("history_initialized"):
        history_path.unlink(missing_ok=True)
    best_val = float("inf")
    final_train = float("nan")
    final_val = float("nan")
    initial_train = float("nan")
    initial_val = float("nan")
    started = time.time()
    steps = int(config["steps"])
    batch_size = int(config["batch_size"])
    grad_accum = max(1, int(config["grad_accum_steps"]))
    tokens_per_step = batch_size * gpt_cfg.block_size * grad_accum * world
    stopped_at_step = None
    target_eval_hits = 0
    for step in range(steps):
        lr = learning_rate(step, steps, float(config["lr"]), float(config["min_lr"]), int(config["warmup_steps"]), float(config["cooldown_frac"]))
        for group in optimizer.param_groups:
            group["lr"] = lr
        optimizer.zero_grad(set_to_none=True)
        losses = []
        for _ in range(grad_accum):
            x, y = sample_batch(tokens, batch_size, gpt_cfg.block_size, device, token_start=train_start, token_stop=train_stop, vocab_size=gpt_cfg.vocab_size)
            with maybe_autocast(device, bool(config["use_bf16"])):
                _, loss = model(x, y)
            assert loss is not None
            losses.append(float(loss.item()))
            (loss / grad_accum).backward()
        torch.nn.utils.clip_grad_norm_(model.parameters(), float(config["grad_clip"]))
        optimizer.step()
        final_train = sum(losses) / len(losses)
        if step == 0:
            initial_train = final_train
        if step % int(config["eval_every"]) == 0 or step == steps - 1:
            if val_tokens is not None:
                final_val = estimate_loss(
                    model,
                    val_tokens,
                    batch_size,
                    int(config["eval_batches"]),
                    device,
                    token_start=val_start,
                    token_stop=val_stop,
                    use_bf16=bool(config["use_bf16"]),
                    mode=str(config["eval_mode"]),
                )
                if step == 0:
                    initial_val = final_val
                best_val = min(best_val, final_val)
                if final_val <= float(config["target_val_loss"]):
                    target_eval_hits += 1
                else:
                    target_eval_hits = 0
                config["target_eval_hits"] = target_eval_hits
            if rank == 0:
                tokens_seen = (step + 1) * tokens_per_step
                elapsed_seconds = max(1e-9, time.time() - started)
                row = {
                    "_step": step,
                    "_timestamp": time.time(),
                    "train/lr": lr,
                    "train/loss": final_train,
                    "train/entropy_loss": final_train,
                    "train/perplexity": safe_perplexity(final_train),
                    "train/tokens": tokens_seen,
                    "train/tokens_per_second": tokens_seen / elapsed_seconds,
                    "system/world_size": world,
                    "system/cuda_available": 1 if torch.cuda.is_available() else 0,
                    "system/has_heldout_validation": 1 if val_tokens is not None else 0,
                    "system/use_bf16": 1 if config["use_bf16"] else 0,
                    "system/use_compile": 1 if config["use_compile"] else 0,
                }
                if val_tokens is not None:
                    row["val/loss"] = final_val
                    row["val/entropy_loss"] = final_val
                    row["val/perplexity"] = safe_perplexity(final_val)
                    row["val/best_loss"] = best_val
                    row["val/best_entropy_loss"] = best_val
                    row["val/target_eval_hits"] = target_eval_hits
                    row["val/target_consecutive_evals"] = int(config["target_consecutive_evals"])
                    row["val/eval_batches"] = int(config["eval_batches"])
                    row["val/eval_mode_stride"] = 1 if config["eval_mode"] == "stride" else 0
                with history_path.open("a", encoding="utf-8") as handle:
                    handle.write(json.dumps(row, sort_keys=True) + "\n")
            if ray_train is not None:
                report = {"train_loss": final_train}
                if val_tokens is not None:
                    report.update({"val_loss": final_val, "best_val_loss": best_val})
                ray_train.report(report)
            if should_stop_at_target(config, best_val, device):
                stopped_at_step = step
                break

    if rank == 0:
        hellaswag_accuracy = estimate_hellaswag(model, hellaswag_examples, device, use_bf16=bool(config["use_bf16"]))
        if hellaswag_accuracy is not None:
            with history_path.open("a", encoding="utf-8") as handle:
                handle.write(
                    json.dumps(
                        {
                            "_step": steps,
                            "_timestamp": time.time(),
                            "eval/hellaswag_accuracy": hellaswag_accuracy,
                            "eval/hellaswag_examples": len(hellaswag_examples),
                        },
                        sort_keys=True,
                    )
                    + "\n"
                )
        out_dir = Path(config["out_dir"])
        ckpt = out_dir / "checkpoints" / "last.pt"
        ckpt.parent.mkdir(parents=True, exist_ok=True)
        torch.save({"model": model.state_dict(), "config": asdict(gpt_cfg)}, ckpt)
        metrics = {
            "schema_version": 1,
            "run": config["run_name"],
            "dataset_source": dataset_source,
            "validation_source": validation_source,
            "model": asdict(gpt_cfg),
            "parameter_count": param_count,
            "world_size": world,
            "device": str(device),
            "steps": steps,
            "steps_completed": (stopped_at_step + 1) if stopped_at_step is not None else steps,
            "stopped_at_step": stopped_at_step,
            "eval_batches": int(config["eval_batches"]),
            "eval_mode": str(config["eval_mode"]),
            "target_consecutive_evals": int(config["target_consecutive_evals"]),
            "target_eval_hits": target_eval_hits,
            "grad_accum_steps": grad_accum,
            "use_compile": config["use_compile"],
            "use_bf16": config["use_bf16"],
            "cooldown_frac": config["cooldown_frac"],
            "tokens_per_step": tokens_per_step,
            "tokens_seen": tokens_per_step * ((stopped_at_step + 1) if stopped_at_step is not None else steps),
            "initial_train_loss": initial_train,
            "initial_val_loss": None if math.isnan(initial_val) else initial_val,
            "train_loss": final_train,
            "train_entropy_loss": final_train,
            "train_perplexity": safe_perplexity(final_train),
            "val_loss": None if math.isnan(final_val) else final_val,
            "val_entropy_loss": None if math.isnan(final_val) else final_val,
            "val_perplexity": None if math.isnan(final_val) else safe_perplexity(final_val),
            "best_val_loss": None if best_val == float("inf") else best_val,
            "best_val_entropy_loss": None if best_val == float("inf") else best_val,
            "train_loss_decreased": bool(final_train < initial_train),
            "val_loss_decreased": bool(not math.isnan(final_val) and final_val < initial_val),
            "has_heldout_validation": val_tokens is not None,
            "hellaswag_accuracy": hellaswag_accuracy,
            "hellaswag_examples": len(hellaswag_examples),
            "hellaswag_source": hellaswag_source,
            "elapsed_seconds": time.time() - started,
            "stellar_history": str(history_path),
            "checkpoint": str(ckpt),
            "llmc_discussion_481_target": "GPT-2 124M, FineWeb 10B tokens, validation loss + HellaSwag",
            "target_val_loss": config["target_val_loss"],
            "target_val_loss_met": bool(best_val != float("inf") and best_val <= float(config["target_val_loss"])),
            "stop_at_target": config["stop_at_target"],
            "fineweb_dataset": os.environ.get("NANOGPT_FINEWEB_DATASET", "HuggingFaceFW/fineweb-edu"),
            "fineweb_config": os.environ.get("NANOGPT_FINEWEB_CONFIG", "sample-10BT"),
        }
        (out_dir / "metrics.json").write_text(json.dumps(metrics, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return {"train_loss": final_train, "val_loss": final_val, "best_val_loss": best_val}


def build_config(args: argparse.Namespace) -> dict:
    run_name = args.run_name or os.environ.get("TAU_RUN_NAME") or os.environ.get("JOB_NAME") or "nanogpt-fineweb-ray"
    out_dir = Path(args.out_dir or os.environ.get("TAU_OUTPUT_DIR") or f"/data/checkpoints/workflows/{run_name}")
    data_root = Path(args.data_root or os.environ.get("TAU_DATA_DIR") or "/data")
    activation = os.environ.get("NANOGPT_ACTIVATION", "gelu").strip().lower()
    if activation not in {"gelu", "relu2"}:
        raise ValueError(f"NANOGPT_ACTIVATION must be gelu or relu2, got {activation!r}")
    eval_mode = os.environ.get("NANOGPT_EVAL_MODE", "random").strip().lower()
    if eval_mode not in {"random", "stride"}:
        raise ValueError(f"NANOGPT_EVAL_MODE must be random or stride, got {eval_mode!r}")
    return {
        "run_name": run_name,
        "data_root": str(data_root),
        "out_dir": str(out_dir),
        "fallback_tokens": env_int("NANOGPT_FALLBACK_TOKENS", 200_000),
        "val_fraction": env_float("NANOGPT_VAL_FRACTION", 0.9),
        "target_val_loss": env_float("NANOGPT_TARGET_VAL_LOSS", 3.28),
        "stop_at_target": os.environ.get("NANOGPT_STOP_AT_TARGET", "0") == "1",
        "steps": args.steps or env_int("NANOGPT_STEPS", 40),
        "batch_size": args.batch_size or env_int("NANOGPT_BATCH_SIZE", 8),
        "eval_every": args.eval_every or env_int("NANOGPT_EVAL_EVERY", 10),
        "eval_batches": args.eval_batches or env_int("NANOGPT_EVAL_BATCHES", 4),
        "eval_mode": eval_mode,
        "target_consecutive_evals": env_int("NANOGPT_TARGET_CONSECUTIVE_EVALS", 1),
        "target_eval_hits": 0,
        "lr": env_float("NANOGPT_LR", 3e-4),
        "min_lr": env_float("NANOGPT_MIN_LR", 3e-5),
        "warmup_steps": env_int("NANOGPT_WARMUP_STEPS", 5),
        "cooldown_frac": env_float("NANOGPT_COOLDOWN_FRAC", 1.0),
        "grad_accum_steps": env_int("NANOGPT_GRAD_ACCUM_STEPS", 1),
        "weight_decay": env_float("NANOGPT_WEIGHT_DECAY", 0.1),
        "grad_clip": env_float("NANOGPT_GRAD_CLIP", 1.0),
        "hellaswag_examples": env_int("NANOGPT_HELLASWAG_EXAMPLES", 0),
        "seed": env_int("NANOGPT_SEED", 1337),
        "ddp_probe": os.environ.get("NANOGPT_DDP_PROBE", "0") == "1",
        "use_compile": os.environ.get("NANOGPT_USE_COMPILE", "0") == "1",
        "use_bf16": os.environ.get("NANOGPT_USE_BF16", "0") == "1",
        "history_initialized": False,
        "model": {
            "vocab_size": env_int("NANOGPT_VOCAB_SIZE", 50304),
            "block_size": env_int("NANOGPT_BLOCK_SIZE", 128),
            "n_layer": env_int("NANOGPT_N_LAYER", 4),
            "n_head": env_int("NANOGPT_N_HEAD", 4),
            "n_embd": env_int("NANOGPT_N_EMBD", 256),
            "dropout": env_float("NANOGPT_DROPOUT", 0.0),
            "use_rmsnorm": os.environ.get("NANOGPT_USE_RMSNORM", "0") == "1",
            "activation": activation,
        },
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--local-smoke", action="store_true", help="run without Ray for local syntax/smoke validation")
    parser.add_argument("--run-name")
    parser.add_argument("--data-root")
    parser.add_argument("--out-dir")
    parser.add_argument("--steps", type=int)
    parser.add_argument("--batch-size", type=int)
    parser.add_argument("--eval-every", type=int)
    parser.add_argument("--eval-batches", type=int)
    args = parser.parse_args()
    config = build_config(args)
    if args.local_smoke:
        config["local_smoke"] = True
        initialize_history(config)
        train_worker(config)
        return
    import ray
    from ray.train import ScalingConfig
    from ray.train.torch import TorchTrainer

    history_path = initialize_history(config)
    if os.environ.get("NANOGPT_PREPARE_FINEWEB", "0") == "1":
        with AsyncJSONLLogger(history_path) as history_logger:
            prepare_fineweb_smoke_tokens(Path(config["data_root"]), int(config["fallback_tokens"]), history_logger)
    ray.init(address="auto", ignore_reinit_error=True)
    workers = env_int("NANOGPT_RAY_WORKERS", 1)
    trainer = TorchTrainer(
        train_worker,
        train_loop_config=config,
        scaling_config=ScalingConfig(num_workers=workers, use_gpu=torch.cuda.is_available()),
    )
    result = trainer.fit()
    print(result.metrics)


if __name__ == "__main__":
    main()
