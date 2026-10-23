#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
GRPO training with compiler-as-teacher reward for COBOL code generation.

Runs under Ray Train: driver.py starts the `trl vllm-serve` server (TP=8) on one
node, then a TorchTrainer whose 8 workers, one per GPU on the other node, each call
main(). Ray Train initializes torch.distributed and sets RANK, LOCAL_RANK and
WORLD_SIZE before main() runs, and the driver sets VLLM_SERVER_URL. Importing this
module has no side effects.
"""

import json
import math
import os
import re
import subprocess
import tempfile
import time
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path

import torch
from datasets import Dataset
from transformers import AutoTokenizer, TrainerCallback
from trl import GRPOConfig, GRPOTrainer

VLLM_PORT = 8000

LOCAL_RANK = 0
GLOBAL_RANK = 0
WORLD_SIZE = 1

MODEL_ID = os.environ.get(
    "MODEL_ID", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt/final"
)
TRAIN_FILE = os.environ.get("TRAIN_FILE", "/data/datasets/cobol/prepared/train.jsonl")
OUTPUT_DIR = os.environ.get(
    "OUTPUT_DIR", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/grpo"
)
NUM_EPOCHS = int(os.environ.get("NUM_EPOCHS", "3"))
LR = float(os.environ.get("LR", "5e-7"))
K_SAMPLES = int(os.environ.get("K_SAMPLES", "8"))
TEMPERATURE = float(os.environ.get("TEMPERATURE", "0.8"))
KL_COEFF = float(os.environ.get("KL_COEFF", "0.05"))
BATCH_SIZE = int(os.environ.get("BATCH_SIZE", "8"))
GRAD_ACCUM = int(os.environ.get("GRAD_ACCUM", "4"))
MAX_PROMPT_LEN = int(os.environ.get("MAX_PROMPT_LEN", "4096"))
MAX_GEN_LEN = int(os.environ.get("MAX_GEN_LEN", "1024"))
SAVE_STEPS = int(os.environ.get("SAVE_STEPS", "50"))
MAX_STEPS = os.environ.get("MAX_STEPS")
HF_HOME = os.environ.get("HF_HOME", "/tmp/hf-cache")

COMPILE_FORMAT_FLAGS = {"free": "-free", "fixed": "-fixed"}


def compile_format(record_fmt):
    fmt = (record_fmt or "free").split("-", 1)[0]
    if fmt not in COMPILE_FORMAT_FLAGS:
        raise ValueError(f"Unknown compile_fmt {record_fmt!r}")
    return fmt


def extract_prompt(text):
    upper = text.upper()
    if "IDENTIFICATION DIVISION" not in upper or "PROCEDURE DIVISION" not in upper:
        return None, None, 0
    proc_pat = re.compile(
        r"^(.*PROCEDURE\s+DIVISION[^\n]*\.?)$", re.IGNORECASE | re.MULTILINE
    )
    m = proc_pat.search(text)
    if not m:
        return None, None, 0
    prompt = text[: m.end()]
    body = text[m.end() :]
    body_lines = [ln for ln in body.split("\n") if ln.strip()]
    return prompt, body, len(body_lines)


def _preflight_compilable(prompt, body, fmt="free"):
    """Check if prompt + reference body compiles cleanly (rc == 0) in its
    source format. Reject programs with missing copybooks — the compiler
    can't validate the procedure body when dependencies are unresolved, so
    these produce all-failure groups regardless of completion quality."""
    full = prompt + body
    rc, _ = _try_compile(full, fmt)
    return rc == 0


def _check_compiler_available():
    try:
        result = subprocess.run(
            ["cobc", "--version"],
            capture_output=True,
            timeout=10,
        )
        if result.returncode != 0:
            raise RuntimeError(f"cobc exited with code {result.returncode}")
    except FileNotFoundError:
        raise RuntimeError("cobc not found — GnuCOBOL must be installed")
    except subprocess.TimeoutExpired:
        raise RuntimeError("cobc --version timed out")


def load_prompts(
    path, min_body=5, max_body=200, preflight=True, max_body_tokens=None, tokenizer=None
):
    _check_compiler_available()

    candidates = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rec = json.loads(line)
            prompt, body, body_len = extract_prompt(rec.get("text", ""))
            if prompt is None or body_len < min_body or body_len > max_body:
                continue
            candidates.append((prompt, body, compile_format(rec.get("compile_fmt"))))

    if max_body_tokens and tokenizer:
        before = len(candidates)
        candidates = [
            (p, b, fmt)
            for p, b, fmt in candidates
            if len(tokenizer.encode(b, add_special_tokens=False)) <= max_body_tokens
        ]
        if GLOBAL_RANK == 0 and len(candidates) < before:
            print(
                f"  Body token filter: {before - len(candidates)} dropped "
                f"(>{max_body_tokens} tokens)",
                flush=True,
            )

    if not preflight:
        return [{"prompt": p, "compile_fmt": fmt} for p, _, fmt in candidates]

    prompts = []
    skipped = 0
    for prompt, body, fmt in candidates:
        if _preflight_compilable(prompt, body, fmt):
            prompts.append({"prompt": prompt, "compile_fmt": fmt})
        else:
            skipped += 1

    if GLOBAL_RANK == 0:
        n_fixed = sum(r["compile_fmt"] == "fixed" for r in prompts)
        print(
            f"  Preflight: {len(prompts)} compilable ({len(prompts) - n_fixed} free, "
            f"{n_fixed} fixed), {skipped} skipped",
            flush=True,
        )
    if not prompts:
        raise RuntimeError(
            f"No compilable prompts found in {path} — check compiler and data"
        )
    return prompts


def _try_compile(cobol_text, fmt="free"):
    flag = COMPILE_FORMAT_FLAGS[compile_format(fmt)]
    with tempfile.NamedTemporaryFile(suffix=".cbl", mode="w", delete=False) as f:
        f.write(cobol_text)
        f.flush()
        src = f.name
    try:
        result = subprocess.run(
            ["cobc", "-fsyntax-only", flag, src],
            capture_output=True,
            timeout=10,
        )
        result = subprocess.CompletedProcess(
            result.args,
            result.returncode,
            result.stdout.decode("utf-8", errors="replace"),
            result.stderr.decode("utf-8", errors="replace"),
        )
        if result.returncode < 0:
            print(
                f"  cobc killed by signal {-result.returncode}; scoring as failed compile",
                flush=True,
            )
            return 1, result.stderr.strip()
        return result.returncode, result.stderr.strip()
    except subprocess.TimeoutExpired:
        return -1, "compilation timeout"
    except FileNotFoundError:
        return -1, "cobc not found"
    finally:
        os.unlink(src)


def _parse_cobc(stderr):
    errors, warnings, copybook = [], [], []
    for line in stderr.strip().split("\n"):
        if not line.strip():
            continue
        ll = line.lower()
        if "error:" in ll:
            if "no such file or directory" in ll:
                copybook.append(line)
            else:
                errors.append(line)
        elif "warning:" in ll:
            warnings.append(line)
    return errors, warnings, copybook


def _compilation_reward(returncode, stderr):
    if returncode == 0:
        errors, warnings, _ = _parse_cobc(stderr)
        return 0.7 if warnings else 1.0
    if returncode < 0:
        raise RuntimeError(f"Compiler infrastructure failure: {stderr}")
    errors, warnings, copybook = _parse_cobc(stderr)
    if not errors and not copybook:
        return -1.0
    if copybook and not errors:
        return 0.0
    n = len(errors)
    return max(-1.0, -0.2 * n)


_STRING_LITERAL_RE = re.compile(r"""(?:"[^"]*"|'[^']*')""")
_END_TERMINATOR_RE = re.compile(r"\bEND-[A-Z]+\b", re.IGNORECASE)
_HYPHENATED_IDENT_RE = re.compile(r"\b[A-Z0-9]+-[A-Z0-9][-A-Z0-9]*\b", re.IGNORECASE)
_INLINE_COMMENT_RE = re.compile(r"""("[^"]*"|'[^']*')|\*>.*$""", re.MULTILINE)


def _strip_inline_comments(text):
    def _repl(m):
        if m.group(1) is not None:
            return m.group(0)
        return ""

    return _INLINE_COMMENT_RE.sub(_repl, text)


_MEANINGFUL_VERBS = re.compile(
    r"\b(?:ACCEPT|ADD|ALLOCATE|ALTER|CALL|CANCEL|CLOSE|COMPUTE|"
    r"DELETE|DISPLAY|DIVIDE|EVALUATE|FREE|GO|IF|INITIALIZE|INSPECT|"
    r"MERGE|MOVE|MULTIPLY|OPEN|PERFORM|READ|RELEASE|RETURN|REWRITE|"
    r"SEARCH|SET|SORT|START|STRING|SUBTRACT|UNSTRING|WRITE)\b",
    re.IGNORECASE,
)

_NOOP_VERBS = re.compile(
    r"\b(?:CONTINUE)\b",
    re.IGNORECASE,
)

_NON_DISPLAY_VERBS = re.compile(
    r"\b(?:ACCEPT|ADD|ALLOCATE|ALTER|CALL|CANCEL|CLOSE|COMPUTE|"
    r"DELETE|DIVIDE|EVALUATE|FREE|GO|IF|INITIALIZE|INSPECT|"
    r"MERGE|MOVE|MULTIPLY|OPEN|PERFORM|READ|RELEASE|RETURN|REWRITE|"
    r"SEARCH|SET|SORT|START|STRING|SUBTRACT|UNSTRING|WRITE)\b",
    re.IGNORECASE,
)


def _strip_comments_and_strings(text):
    text = _strip_inline_comments(text)
    text = _STRING_LITERAL_RE.sub('""', text)
    return text


def _strip_non_code(text):
    text = _strip_comments_and_strings(text)
    text = _END_TERMINATOR_RE.sub("", text)
    text = _HYPHENATED_IDENT_RE.sub("_ID_", text)
    return text


def _count_cobol_verbs(text):
    cleaned = _strip_non_code(text)
    return len(_MEANINGFUL_VERBS.findall(cleaned))


def _count_noop_verbs(text):
    cleaned = _strip_non_code(text)
    return len(_NOOP_VERBS.findall(cleaned))


def _has_non_display_verb(text):
    cleaned = _strip_non_code(text)
    return bool(_NON_DISPLAY_VERBS.search(cleaned))


_COLLAPSE_WS_RE = re.compile(r"[ \t]+")
_COBOL_TOKEN_RE = re.compile(
    r"""("[^"]*"|'[^']*'"""
    r"""|\d+\.\d+"""
    r"""|[^\s.]+"""
    r"""|\.)\s*""",
    re.MULTILINE,
)


def _tokenize_cobol(text):
    text = _strip_inline_comments(text)
    tokens = []
    for m in _COBOL_TOKEN_RE.finditer(text):
        tok = m.group(1)
        if tok.startswith('"') or tok.startswith("'"):
            tokens.append(tok)
        else:
            tokens.append(tok.upper())
    return tokens


def _split_statements(tokens):
    stmts = []
    cur = []
    for tok in tokens:
        cur.append(tok)
        if tok == ".":
            normalized = " ".join(cur)
            if normalized != ".":
                stmts.append(normalized)
            cur = []
    if cur:
        normalized = " ".join(cur)
        stmts.append(normalized)
    return stmts


def _normalize_line(line):
    s = line.strip()
    if not s:
        return None
    parts = _STRING_LITERAL_RE.split(s)
    literals = _STRING_LITERAL_RE.findall(s)
    result = []
    for i, part in enumerate(parts):
        result.append(_COLLAPSE_WS_RE.sub(" ", part).upper())
        if i < len(literals):
            result.append(literals[i])
    return "".join(result)


def _line_repetition_ratio(text):
    text = _strip_inline_comments(text)
    lines = []
    for line in text.split("\n"):
        n = _normalize_line(line)
        if n:
            lines.append(n)
    if len(lines) < 3:
        return 0.0
    from collections import Counter

    counts = Counter(lines)
    return counts.most_common(1)[0][1] / len(lines)


def _stmt_repetition_ratio(text):
    tokens = _tokenize_cobol(text)
    stmts = _split_statements(tokens)
    if len(stmts) < 3:
        return 0.0
    from collections import Counter

    counts = Counter(stmts)
    return counts.most_common(1)[0][1] / len(stmts)


def _repeated_statement_ratio(text):
    return max(_line_repetition_ratio(text), _stmt_repetition_ratio(text))


def _structural_reward(completion, max_gen_tokens, completion_n_tokens):
    verb_count = _count_cobol_verbs(completion)
    noop_count = _count_noop_verbs(completion)
    if verb_count < 3:
        return -0.50
    if verb_count + noop_count > 0 and noop_count / (verb_count + noop_count) > 0.5:
        return -0.30
    if not _has_non_display_verb(completion):
        return -0.20
    if _repeated_statement_ratio(completion) > 0.5:
        return -0.20

    bonus = 0.0

    code = _strip_comments_and_strings(completion)
    para_def_re = re.compile(r"^[ ]{7}([A-Z0-9][A-Z0-9-]*)\.\s*$", re.MULTILINE)
    defined = {m.group(1).upper() for m in para_def_re.finditer(code)}
    perform_re = re.compile(r"PERFORM[ \t]+([A-Z0-9][A-Z0-9-]+)", re.IGNORECASE)
    skip = {"UNTIL", "VARYING", "TIMES", "TEST", "WITH"}
    referenced, dangling = set(), set()
    for m in perform_re.finditer(code):
        t = m.group(1).upper()
        if t in skip:
            continue
        referenced.add(t)
        if t not in defined:
            dangling.add(t)
    if referenced and not dangling:
        bonus += 0.15

    if max_gen_tokens > 0 and completion_n_tokens >= max_gen_tokens - 5:
        bonus -= 0.15

    return bonus


def _score_one(args):
    prompt, completion, max_gen_tokens, completion_n_tokens, fmt = args
    full = prompt + completion
    rc, stderr = _try_compile(full, fmt)
    r_compile = _compilation_reward(rc, stderr)
    r_struct = _structural_reward(completion, max_gen_tokens, completion_n_tokens)
    errors, warnings, copybook = _parse_cobc(stderr)
    verb_count = _count_cobol_verbs(completion)
    noop_count = _count_noop_verbs(completion)
    return {
        "reward": r_compile + r_struct,
        "compiled": rc == 0,
        "copybook_only": bool(copybook) and not bool(errors),
        "n_errors": len(errors),
        "verb_count": verb_count,
        "noop_count": noop_count,
        "trivial": verb_count < 3,
    }


_reward_stats = {}
_reward_tokenizer = None


def reward_fn(prompts, completions, compile_fmt=None, **kwargs):
    """Compute rewards for a batch of (prompt, completion) pairs, compiling
    each in the format recorded for its prompt."""
    if _reward_tokenizer is not None:
        n_tokens = [
            len(_reward_tokenizer.encode(c, add_special_tokens=False))
            for c in completions
        ]
    else:
        n_tokens = [len(c) // 4 for c in completions]
    fmts = compile_fmt if compile_fmt is not None else ["free"] * len(prompts)
    args = [
        (p, c, MAX_GEN_LEN, nt, fmt)
        for p, c, nt, fmt in zip(prompts, completions, n_tokens, fmts, strict=True)
    ]
    with ProcessPoolExecutor(max_workers=min(8, len(args))) as pool:
        results = list(pool.map(_score_one, args))

    rewards = [r["reward"] for r in results]

    _reward_stats["compile_rate"] = sum(r["compiled"] for r in results) / len(results)
    _reward_stats["copybook_only_rate"] = sum(
        r["copybook_only"] for r in results
    ) / len(results)
    _reward_stats["trivial_rate"] = sum(r["trivial"] for r in results) / len(results)
    _reward_stats["mean_verb_count"] = sum(r["verb_count"] for r in results) / len(
        results
    )
    _reward_stats["mean_noop_count"] = sum(r["noop_count"] for r in results) / len(
        results
    )
    _reward_stats["mean_errors"] = sum(r["n_errors"] for r in results) / len(results)

    return rewards


class TauMetricsPublisher:
    def __init__(self, output_dir):
        self.metrics_dir = Path(output_dir) / "metrics-history-attempt-0"
        self.metrics_dir.mkdir(parents=True, exist_ok=True)

    def publish(self, step, metrics):
        row = {"_step": step, "_timestamp": time.time()}
        for k, v in metrics.items():
            if isinstance(v, (int, float)) and not math.isnan(v):
                row[k] = v
        token = f"{step:08d}-{time.time_ns()}"
        final = self.metrics_dir / f"chunk-{token}.jsonl"
        tmp = self.metrics_dir / f".{final.name}.tmp-{os.getpid()}"
        with tmp.open("x", encoding="utf-8") as f:
            f.write(json.dumps(row) + "\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, final)


def _aggregate_reward_stats():
    return dict(_reward_stats)


def _any_rank(flag):
    dist = torch.distributed
    if not (dist.is_available() and dist.is_initialized()):
        return flag
    if dist.get_backend() == "nccl":
        device = torch.device("cuda", torch.cuda.current_device())
    else:
        device = torch.device("cpu")
    t = torch.tensor([1.0 if flag else 0.0], device=device)
    dist.all_reduce(t, op=dist.ReduceOp.MAX)
    return bool(t.item())


class TauGRPOMetricsCallback(TrainerCallback):
    """Installed on every rank. Rank 0 publishes metrics; every rank evaluates
    the alarms and the stop decision is shared across ranks, so all ranks
    leave the training loop together."""

    def __init__(self, publisher=None):
        self.publisher = publisher
        self.kl_alarm_count = 0
        self.short_alarm_count = 0

    def on_log(self, args, state, control, logs=None, **kwargs):
        if state.global_step <= 0:
            return

        agg = _aggregate_reward_stats()
        stop = self._check_alarms(logs, agg) if logs else False

        if self.publisher is not None and logs:
            metrics = {}
            for k, v in logs.items():
                if isinstance(v, (int, float)) and not math.isnan(v):
                    metrics[k] = v

            if agg:
                metrics["reward/compile_rate"] = agg.get("compile_rate", 0)
                metrics["reward/copybook_only_rate"] = agg.get("copybook_only_rate", 0)
                metrics["reward/trivial_rate"] = agg.get("trivial_rate", 0)
                metrics["gen/verb_count"] = agg.get("mean_verb_count", 0)
                metrics["gen/noop_count"] = agg.get("mean_noop_count", 0)
                metrics["gen/mean_errors"] = agg.get("mean_errors", 0)

            if metrics:
                self.publisher.publish(state.global_step, metrics)

        if _any_rank(stop):
            control.should_training_stop = True

    def _check_alarms(self, logs, agg):
        stop = False
        kl = logs.get("kl", 0)
        if kl > 5.0:
            self.kl_alarm_count += 1
            if self.kl_alarm_count >= 10:
                print(
                    f"ALARM: KL divergence > 5.0 for 10 steps ({kl:.2f}) on rank "
                    f"{GLOBAL_RANK}. Stopping.",
                    flush=True,
                )
                stop = True
        else:
            self.kl_alarm_count = 0

        mean_verbs = agg.get("mean_verb_count", 100)
        if mean_verbs < 3:
            self.short_alarm_count += 1
            if self.short_alarm_count >= 20:
                print(
                    f"ALARM: Mean verb count < 3 for 20 steps on rank {GLOBAL_RANK}. "
                    "Reward hacking. Stopping.",
                    flush=True,
                )
                stop = True
        else:
            self.short_alarm_count = 0
        return stop


class MaxStepsCallback(TrainerCallback):
    """Stops training once global_step reaches max_steps.

    GRPOConfig is left unchanged, so the run keeps the full-run LR schedule (warmup and
    cosine decay over all epochs) and its first steps track a full run. Give a short run
    its own OUTPUT_DIR: its checkpoints would otherwise be auto-resumed by a later full
    run.
    """

    def __init__(self, max_steps):
        self.max_steps = max_steps

    def on_step_end(self, args, state, control, **kwargs):
        if state.global_step >= self.max_steps:
            control.should_training_stop = True


def _parse_size_bytes(size_str):
    size_str = size_str.strip().upper()
    units = {"TB": 1024**4, "GB": 1024**3, "MB": 1024**2, "KB": 1024, "B": 1}
    for suffix, mult in units.items():
        if size_str.endswith(suffix):
            return int(float(size_str[: -len(suffix)]) * mult)
    return int(size_str)


def _blobfuse_save_safetensors(tensors, filepath, metadata=None):
    """safetensors.torch.save_file() uses Rust serialize_file() which is
    completely broken on blobfuse2 FUSE mounts (I/O error on any file size).
    Workaround: serialize to bytes in memory, then plain Python write()."""
    from safetensors.torch import save as _st_save

    raw = _st_save(tensors, metadata=metadata)
    with open(filepath, "wb") as f:
        f.write(raw)
        f.flush()
        os.fsync(f.fileno())
    return len(raw)


def _blobfuse_save_model(state_dict, output_dir, max_shard_size="5GB"):
    """Save sharded safetensors to blobfuse2, bypassing broken Rust serialize_file."""
    max_bytes = _parse_size_bytes(max_shard_size)

    shards, current, current_sz = [], {}, 0
    for key in sorted(state_dict.keys()):
        t = state_dict[key]
        tsz = t.numel() * t.element_size()
        if current_sz + tsz > max_bytes and current:
            shards.append(current)
            current, current_sz = {}, 0
        current[key] = t
        current_sz += tsz
    if current:
        shards.append(current)

    if len(shards) <= 1:
        path = os.path.join(output_dir, "model.safetensors")
        nbytes = _blobfuse_save_safetensors(shards[0] if shards else {}, path)
        print(
            f"  [blobfuse-save] model.safetensors ({nbytes / 1e9:.2f} GB)", flush=True
        )
    else:
        weight_map, total_size = {}, 0
        for i, shard in enumerate(shards, 1):
            fname = f"model-{i:05d}-of-{len(shards):05d}.safetensors"
            nbytes = _blobfuse_save_safetensors(shard, os.path.join(output_dir, fname))
            for key in shard:
                weight_map[key] = fname
            total_size += sum(t.numel() * t.element_size() for t in shard.values())
            print(
                f"  [blobfuse-save] {fname} ({nbytes / 1e9:.2f} GB) [{i}/{len(shards)}]",
                flush=True,
            )
        index = {"metadata": {"total_size": total_size}, "weight_map": weight_map}
        with open(os.path.join(output_dir, "model.safetensors.index.json"), "w") as f:
            json.dump(index, f, indent=2)


def _patch_trainer_save_shard_size(trainer, max_shard_size="5GB"):
    """Monkey-patch Trainer._save to bypass safetensors Rust serialize_file
    (broken on blobfuse2) and use in-memory serialize + Python write instead."""

    def _patched_save(output_dir=None, state_dict=None):
        output_dir = output_dir if output_dir is not None else trainer.args.output_dir
        os.makedirs(output_dir, exist_ok=True)

        if state_dict is not None:
            _blobfuse_save_model(state_dict, output_dir, max_shard_size=max_shard_size)
        else:
            unwrapped = trainer.accelerator.unwrap_model(
                trainer.model, keep_torch_compile=False
            )
            _blobfuse_save_model(
                unwrapped.state_dict(), output_dir, max_shard_size=max_shard_size
            )

        unwrapped = trainer.accelerator.unwrap_model(
            trainer.model, keep_torch_compile=False
        )
        if hasattr(unwrapped, "config"):
            unwrapped.config.save_pretrained(output_dir)

        if trainer.processing_class is not None:
            trainer.processing_class.save_pretrained(output_dir)

        torch.save(trainer.args, os.path.join(output_dir, "training_args.bin"))

    trainer._save = _patched_save


def main():
    global LOCAL_RANK, GLOBAL_RANK, WORLD_SIZE
    LOCAL_RANK = int(os.environ.get("LOCAL_RANK", "0"))
    GLOBAL_RANK = int(os.environ.get("RANK", "0"))
    WORLD_SIZE = int(os.environ.get("WORLD_SIZE", "1"))

    os.environ["HF_HOME"] = HF_HOME
    os.makedirs(OUTPUT_DIR, exist_ok=True)

    is_main = GLOBAL_RANK == 0

    if is_main:
        print("=" * 60, flush=True)
        print("COBOL GRPO Training (compiler-as-teacher)", flush=True)
        print("=" * 60, flush=True)
        print(f"  Model:        {MODEL_ID}", flush=True)
        print(f"  Train file:   {TRAIN_FILE}", flush=True)
        print(f"  Output:       {OUTPUT_DIR}", flush=True)
        print(f"  Epochs:       {NUM_EPOCHS}", flush=True)
        print(f"  LR:           {LR}", flush=True)
        print(f"  K samples:    {K_SAMPLES}", flush=True)
        print(f"  Temperature:  {TEMPERATURE}", flush=True)
        print(f"  KL coeff:     {KL_COEFF}", flush=True)
        print(f"  Batch size:   {BATCH_SIZE} (per device)", flush=True)
        print(f"  Grad accum:   {GRAD_ACCUM}", flush=True)
        print(f"  GPUs:         {torch.cuda.device_count()}", flush=True)
        print(f"  World size:   {WORLD_SIZE}", flush=True)
        if MAX_STEPS:
            print(f"  Max steps:    {MAX_STEPS}", flush=True)

    global _reward_tokenizer
    if is_main:
        print("\nLoading tokenizer...", flush=True)
    tokenizer = AutoTokenizer.from_pretrained(MODEL_ID)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token
    _reward_tokenizer = tokenizer

    if is_main:
        print("\nExtracting prompts from training data...", flush=True)
    prompts = load_prompts(
        TRAIN_FILE,
        max_body_tokens=MAX_GEN_LEN,
        tokenizer=tokenizer,
    )
    if is_main:
        print(f"  Extracted {len(prompts)} prompts", flush=True)

    orig_count = len(prompts)
    prompts = [
        r for r in prompts if len(tokenizer.encode(r["prompt"])) <= MAX_PROMPT_LEN
    ]
    if is_main and len(prompts) < orig_count:
        print(
            f"  Filtered {orig_count - len(prompts)} prompts exceeding {MAX_PROMPT_LEN} tokens",
            flush=True,
        )

    dataset = Dataset.from_list(prompts)
    if is_main:
        print(f"  Dataset size: {len(dataset)}", flush=True)

    ds_config = {
        "bf16": {"enabled": True},
        "zero_optimization": {
            "stage": 3,
            "offload_optimizer": {"device": "cpu", "pin_memory": True},
            "offload_param": {"device": "none"},
            "overlap_comm": True,
            "contiguous_gradients": True,
            "reduce_bucket_size": 5e8,
            "stage3_prefetch_bucket_size": 5e8,
            "stage3_param_persistence_threshold": 1e6,
            "stage3_gather_16bit_weights_on_model_save": True,
        },
        "gradient_accumulation_steps": "auto",
        "gradient_clipping": 1.0,
        "train_batch_size": "auto",
        "train_micro_batch_size_per_gpu": "auto",
        "wall_clock_breakdown": False,
    }
    ds_config_path = os.path.join(OUTPUT_DIR, "ds_config_rl.json")
    if GLOBAL_RANK == 0:
        with open(ds_config_path, "w") as f:
            json.dump(ds_config, f, indent=2)
    if torch.distributed.is_initialized():
        torch.distributed.barrier()

    vllm_base_url = os.environ.get("VLLM_SERVER_URL", f"http://localhost:{VLLM_PORT}")
    if is_main:
        print(f"  vLLM server: {vllm_base_url}", flush=True)

    grpo_kwargs = dict(
        output_dir=OUTPUT_DIR,
        num_train_epochs=NUM_EPOCHS,
        per_device_train_batch_size=BATCH_SIZE,
        gradient_accumulation_steps=GRAD_ACCUM,
        learning_rate=LR,
        max_completion_length=MAX_GEN_LEN,
        num_generations=K_SAMPLES,
        temperature=TEMPERATURE,
        top_p=0.95,
        beta=KL_COEFF,
        logging_steps=1,
        save_steps=SAVE_STEPS,
        save_total_limit=3,
        gradient_checkpointing=True,
        report_to="none",
        seed=42,
    )
    import inspect

    _sig = inspect.signature(GRPOConfig)
    _optional = {
        "lr_scheduler_type": "cosine",
        "warmup_ratio": 0.1,
        "bf16": True,
        "max_prompt_length": MAX_PROMPT_LEN,
        "gradient_checkpointing_kwargs": {"use_reentrant": False},
        "deepspeed": ds_config_path,
        "save_strategy": "steps",
        "use_vllm": True,
        "vllm_server_base_url": vllm_base_url,
        "vllm_mode": "server",
    }
    for k, v in _optional.items():
        if k in _sig.parameters:
            grpo_kwargs[k] = v
        elif is_main:
            print(f"  Skipping unsupported GRPOConfig param: {k}", flush=True)
    training_args = GRPOConfig(**grpo_kwargs)

    publisher = TauMetricsPublisher(OUTPUT_DIR) if GLOBAL_RANK == 0 else None
    metrics_cb = TauGRPOMetricsCallback(publisher)
    max_steps_cb = MaxStepsCallback(int(MAX_STEPS)) if MAX_STEPS else None
    callbacks = [cb for cb in [metrics_cb, max_steps_cb] if cb is not None]

    if is_main:
        print("\nInitializing GRPOTrainer...", flush=True)

    trainer = GRPOTrainer(
        model=MODEL_ID,
        reward_funcs=reward_fn,
        args=training_args,
        train_dataset=dataset,
        processing_class=tokenizer,
        callbacks=callbacks,
    )

    _patch_trainer_save_shard_size(trainer, max_shard_size="5GB")

    resume_ckpt = None
    ckpts = sorted(
        [d for d in Path(OUTPUT_DIR).glob("checkpoint-*") if d.is_dir()],
        key=lambda p: int(p.name.split("-")[-1]),
    )
    if ckpts:
        resume_ckpt = str(ckpts[-1])
        if is_main:
            print(f"\n  Resuming from {resume_ckpt}", flush=True)

    if is_main:
        print("\n" + "=" * 60, flush=True)
        if resume_ckpt:
            print(
                f"Resuming GRPO training from {Path(resume_ckpt).name}...", flush=True
            )
        else:
            print("Starting GRPO training from scratch...", flush=True)
        print("=" * 60, flush=True)

    trainer.train(resume_from_checkpoint=resume_ckpt)

    if is_main:
        print("\nSaving final model...", flush=True)
    final_dir = os.path.join(OUTPUT_DIR, "final")
    trainer.save_model(final_dir)
    tokenizer.save_pretrained(final_dir)

    if GLOBAL_RANK == 0:
        config = {
            "model_id": MODEL_ID,
            "algorithm": "GRPO",
            "num_epochs": NUM_EPOCHS,
            "lr": LR,
            "k_samples": K_SAMPLES,
            "temperature": TEMPERATURE,
            "kl_coeff": KL_COEFF,
            "batch_size": BATCH_SIZE,
            "grad_accum": GRAD_ACCUM,
            "max_prompt_len": MAX_PROMPT_LEN,
            "max_gen_len": MAX_GEN_LEN,
            "num_prompts": len(prompts),
            "world_size": WORLD_SIZE,
            "gpus_per_node": torch.cuda.device_count(),
        }
        with open(os.path.join(final_dir, "train-config.json"), "w") as f:
            json.dump(config, f, indent=2)
        print("\n=== GRPO training complete ===", flush=True)


if __name__ == "__main__":
    main()
