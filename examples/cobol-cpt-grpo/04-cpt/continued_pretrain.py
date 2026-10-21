# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
Continued pretraining of Qwen2.5-Coder-32B on COBOL corpus.

Uses HuggingFace Trainer + DeepSpeed ZeRO-3 (config built inline) across 2 nodes x 8 GPUs.
Publishes Stellar-compatible metrics chunks for TauGrid experiment tracking.

Launch: driver.py runs main() in 16 Ray Train workers (TorchTrainer, one GPU each).
Ray Train initializes torch.distributed and sets RANK, LOCAL_RANK, WORLD_SIZE and
MASTER_ADDR/PORT before main() runs; accelerate and DeepSpeed reuse that process group.
Importing this module has no side effects; all env reads happen in main().

MAX_STEPS (optional) stops training after that many optimizer steps for a short check,
through a callback, so the LR schedule and TrainingArguments stay those of the full run.
Unset means a full run.
"""

import functools
import json
import math
import os
import time
from pathlib import Path

import torch
from datasets import Dataset
from transformers import (
    AutoModelForCausalLM,
    AutoTokenizer,
    Trainer,
    TrainerCallback,
    TrainingArguments,
)

_orig_torch_load = torch.load


def _patched_torch_load(*args, **kwargs):
    if "weights_only" not in kwargs:
        kwargs["weights_only"] = False
    return _orig_torch_load(*args, **kwargs)


def _save_file_direct(tensors, filename, metadata=None):
    """Writes a safetensors file under its final name.

    safetensors 0.8 writes to a temp file and renames it before closing it, and blobfuse2
    fails that rename with EIO. Serialize in memory and write the file directly instead,
    as train_grpo.py's _blobfuse_save_safetensors in 06-grpo does.
    """
    import safetensors.torch

    data = safetensors.torch.save(tensors, metadata=metadata)
    with open(filename, "wb") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())


class StellarMetricsCallback(TrainerCallback):
    def __init__(self, metrics_dir):
        self.metrics_dir = Path(metrics_dir)
        self.metrics_dir.mkdir(parents=True, exist_ok=True)

    def _publish(self, step, metrics):
        row = {"_step": step, "_timestamp": time.time()}
        row.update(metrics)
        token = f"{step:08d}-{time.time_ns()}"
        final = self.metrics_dir / f"chunk-{token}.jsonl"
        tmp = self.metrics_dir / f".{final.name}.tmp-{os.getpid()}"
        with tmp.open("x") as f:
            f.write(json.dumps(row) + "\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, final)

    def on_log(self, args, state, control, logs=None, **kwargs):
        if logs and state.global_step > 0:
            metrics = {}
            for k, v in logs.items():
                if isinstance(v, (int, float)) and not math.isnan(v):
                    metrics[k] = v
            if metrics:
                self._publish(state.global_step, metrics)

    def on_evaluate(self, args, state, control, metrics=None, **kwargs):
        if metrics and state.global_step > 0:
            eval_metrics = {}
            for k, v in metrics.items():
                if isinstance(v, (int, float)) and not math.isnan(v):
                    eval_metrics[k] = v
            if eval_metrics:
                self._publish(state.global_step, eval_metrics)


class StopAtStepCallback(TrainerCallback):
    """Stops after max_steps without changing the full-run LR schedule."""

    def __init__(self, max_steps):
        self.max_steps = max_steps

    def on_step_end(self, args, state, control, **kwargs):
        if state.global_step >= self.max_steps:
            control.should_training_stop = True
        return control


def load_jsonl(path):
    records = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                records.append(json.loads(line))
    return records


def tokenize_dataset(records, tokenizer, max_seq_len):
    all_input_ids = []
    for rec in records:
        text = rec.get("text", "")
        if not text.strip():
            continue
        tokens = tokenizer(text, truncation=False, add_special_tokens=False)[
            "input_ids"
        ]
        tokens.append(tokenizer.eos_token_id)
        all_input_ids.extend(tokens)

    total_tokens = len(all_input_ids)
    n_chunks = total_tokens // max_seq_len
    print(f"  Total tokens: {total_tokens:,}", flush=True)
    print(f"  Chunks of {max_seq_len}: {n_chunks:,}", flush=True)
    print(
        f"  Discarded tail: {total_tokens - n_chunks * max_seq_len:,} tokens",
        flush=True,
    )

    chunks = []
    for i in range(n_chunks):
        start = i * max_seq_len
        chunk = all_input_ids[start : start + max_seq_len]
        chunks.append({"input_ids": chunk, "labels": chunk.copy()})

    return Dataset.from_list(chunks)


def main():
    torch.load = _patched_torch_load
    from transformers import modeling_utils

    if not hasattr(modeling_utils, "safe_save_file"):
        raise RuntimeError(
            "transformers.modeling_utils.safe_save_file not found; update _save_file_direct"
        )
    modeling_utils.safe_save_file = _save_file_direct

    MODEL_ID = os.environ.get("MODEL_ID", "Qwen/Qwen2.5-Coder-32B")
    TRAIN_FILE = os.environ.get(
        "TRAIN_FILE", "/data/datasets/cobol/prepared/train.jsonl"
    )
    VAL_FILE = os.environ.get("VAL_FILE", "/data/datasets/cobol/prepared/val.jsonl")
    OUTPUT_DIR = os.environ.get(
        "OUTPUT_DIR", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt"
    )
    EPOCHS = int(os.environ.get("EPOCHS", "3"))
    LR = float(os.environ.get("LR", "2e-5"))
    BATCH_SIZE = int(os.environ.get("BATCH_SIZE", "1"))
    GRAD_ACCUM = int(os.environ.get("GRAD_ACCUM", "16"))
    MAX_SEQ_LEN = int(os.environ.get("MAX_SEQ_LEN", "4096"))
    SAVE_STEPS = int(os.environ.get("SAVE_STEPS", "500"))
    WARMUP_RATIO = float(os.environ.get("WARMUP_RATIO", "0.05"))
    WEIGHT_DECAY = float(os.environ.get("WEIGHT_DECAY", "0.1"))
    HF_HOME = os.environ.get("HF_HOME", "/tmp/hf-cache")
    RESUME_FROM = os.environ.get("TAU_RESUME_FROM", None)
    MAX_STEPS = os.environ.get("MAX_STEPS", "").strip()
    stop_at_step = int(MAX_STEPS) if MAX_STEPS else None

    os.environ["HF_HOME"] = HF_HOME
    os.makedirs(OUTPUT_DIR, exist_ok=True)

    local_rank = int(os.environ.get("LOCAL_RANK", "0"))
    is_main = local_rank == 0
    global_rank = int(os.environ.get("RANK", "0"))
    world_size = int(os.environ.get("WORLD_SIZE", "1"))

    if is_main:
        print("=" * 60, flush=True)
        print("COBOL Continued Pretraining (multi-node)", flush=True)
        print("=" * 60, flush=True)
        print(f"  Model:        {MODEL_ID}", flush=True)
        print(f"  Train file:   {TRAIN_FILE}", flush=True)
        print(f"  Val file:     {VAL_FILE}", flush=True)
        print(f"  Output:       {OUTPUT_DIR}", flush=True)
        print(f"  Epochs:       {EPOCHS}", flush=True)
        print(f"  LR:           {LR}", flush=True)
        print(f"  Batch size:   {BATCH_SIZE} (per device)", flush=True)
        print(f"  Grad accum:   {GRAD_ACCUM}", flush=True)
        print(f"  Max seq len:  {MAX_SEQ_LEN}", flush=True)
        print(f"  Save steps:   {SAVE_STEPS}", flush=True)
        print(f"  Stop at step: {MAX_STEPS or 'None (full run)'}", flush=True)
        print(f"  World size:   {world_size} processes", flush=True)
        print(f"  GPUs/node:    {torch.cuda.device_count()}", flush=True)
        print(f"  Resume from:  {RESUME_FROM or 'None (fresh start)'}", flush=True)
        effective_batch = BATCH_SIZE * GRAD_ACCUM * world_size
        print(
            f"  Effective batch: {effective_batch} sequences = {effective_batch * MAX_SEQ_LEN:,} tokens",
            flush=True,
        )

    if is_main:
        print("\nLoading tokenizer...", flush=True)
    tokenizer = AutoTokenizer.from_pretrained(MODEL_ID, trust_remote_code=True)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token

    if is_main:
        print("\nTokenizing training data...", flush=True)
    train_records = load_jsonl(TRAIN_FILE)
    if is_main:
        print(f"  Loaded {len(train_records)} training records", flush=True)
    train_dataset = tokenize_dataset(train_records, tokenizer, MAX_SEQ_LEN)
    del train_records

    if is_main:
        print("\nTokenizing validation data...", flush=True)
    val_records = load_jsonl(VAL_FILE)
    if is_main:
        print(f"  Loaded {len(val_records)} validation records", flush=True)
    val_dataset = tokenize_dataset(val_records, tokenizer, MAX_SEQ_LEN)
    del val_records

    if is_main:
        print(f"\n  Train: {len(train_dataset)} chunks", flush=True)
        print(f"  Val:   {len(val_dataset)} chunks", flush=True)
        train_tokens = len(train_dataset) * MAX_SEQ_LEN
        print(f"  Train tokens per epoch: {train_tokens:,}", flush=True)
        print(
            f"  Total tokens ({EPOCHS} epochs): {train_tokens * EPOCHS:,}", flush=True
        )

    if is_main:
        print("\nLoading model...", flush=True)
    model = AutoModelForCausalLM.from_pretrained(
        MODEL_ID,
        dtype=torch.bfloat16,
        trust_remote_code=True,
        attn_implementation="sdpa",
        low_cpu_mem_usage=True,
    )
    model.save_pretrained = functools.partial(
        model.save_pretrained, max_shard_size="5GB"
    )

    if is_main:
        trainable = sum(p.numel() for p in model.parameters() if p.requires_grad)
        total = sum(p.numel() for p in model.parameters())
        print(
            f"  Parameters: {total / 1e9:.1f}B total, {trainable / 1e9:.1f}B trainable",
            flush=True,
        )

    resume_checkpoint = None
    if RESUME_FROM and os.path.isdir(RESUME_FROM):
        resume_checkpoint = RESUME_FROM
        if is_main:
            print(f"\n  Resuming from TauGrid checkpoint: {RESUME_FROM}", flush=True)
    else:
        checkpoints = sorted(
            [d for d in Path(OUTPUT_DIR).glob("checkpoint-*") if d.is_dir()],
            key=lambda p: int(p.name.split("-")[-1]),
        )
        if checkpoints:
            resume_checkpoint = str(checkpoints[-1])
            if is_main:
                print(
                    f"\n  Resuming from latest checkpoint: {resume_checkpoint}",
                    flush=True,
                )

    metrics_dir = os.path.join(OUTPUT_DIR, "metrics-history-attempt-0")
    stellar_cb = StellarMetricsCallback(metrics_dir) if global_rank == 0 else None

    ds_config = {
        "bf16": {"enabled": True},
        "zero_optimization": {
            "stage": 3,
            "offload_optimizer": {"device": "none"},
            "offload_param": {"device": "none"},
            "overlap_comm": False,
            "contiguous_gradients": True,
            "reduce_bucket_size": 1e8,
            "stage3_prefetch_bucket_size": 8.4e7,
            "stage3_param_persistence_threshold": 1e6,
            "stage3_gather_16bit_weights_on_model_save": True,
        },
        "gradient_accumulation_steps": "auto",
        "gradient_clipping": 1.0,
        "train_batch_size": "auto",
        "train_micro_batch_size_per_gpu": "auto",
        "wall_clock_breakdown": False,
    }
    ds_config_path = os.path.join(OUTPUT_DIR, "ds_config.json")
    if global_rank == 0:
        with open(ds_config_path, "w") as f:
            json.dump(ds_config, f, indent=2)
    if torch.distributed.is_initialized():
        torch.distributed.barrier()

    training_args = TrainingArguments(
        output_dir=OUTPUT_DIR,
        num_train_epochs=EPOCHS,
        per_device_train_batch_size=BATCH_SIZE,
        per_device_eval_batch_size=BATCH_SIZE,
        gradient_accumulation_steps=GRAD_ACCUM,
        learning_rate=LR,
        lr_scheduler_type="cosine",
        warmup_steps=WARMUP_RATIO,
        weight_decay=WEIGHT_DECAY,
        bf16=True,
        logging_steps=10,
        eval_strategy="steps",
        eval_steps=SAVE_STEPS,
        save_strategy="steps",
        save_steps=SAVE_STEPS,
        save_total_limit=5,
        load_best_model_at_end=False,
        dataloader_num_workers=4,
        dataloader_pin_memory=True,
        gradient_checkpointing=True,
        gradient_checkpointing_kwargs={"use_reentrant": False},
        deepspeed=ds_config_path,
        report_to="none",
        seed=42,
        remove_unused_columns=False,
    )

    stop_cb = StopAtStepCallback(stop_at_step) if stop_at_step is not None else None
    callbacks = [cb for cb in [stellar_cb, stop_cb] if cb is not None]
    trainer = Trainer(
        model=model,
        args=training_args,
        train_dataset=train_dataset,
        eval_dataset=val_dataset,
        callbacks=callbacks,
    )

    if is_main:
        print("\n" + "=" * 60, flush=True)
        print("Starting training...", flush=True)
        print("=" * 60, flush=True)

    trainer.train(resume_from_checkpoint=resume_checkpoint)

    if is_main:
        print("\nRunning final evaluation...", flush=True)
    final_metrics = trainer.evaluate()
    final_eval_loss = final_metrics["eval_loss"]

    best_eval_loss = trainer.state.best_metric
    best_ckpt = trainer.state.best_model_checkpoint
    if is_main:
        print(
            f"  Final eval loss:     {final_eval_loss:.4f} (ppl {math.exp(final_eval_loss):.2f})",
            flush=True,
        )
        if best_eval_loss is not None and best_ckpt:
            print(
                f"  Best checkpoint:     {best_ckpt} (eval loss {best_eval_loss:.4f}, ppl {math.exp(best_eval_loss):.2f})",
                flush=True,
            )
        else:
            print("  No best checkpoint tracked — using final weights", flush=True)

    use_best = best_eval_loss is not None and best_eval_loss < final_eval_loss
    chosen_loss = best_eval_loss if use_best else final_eval_loss

    if use_best:
        if is_main:
            print(f"  Best checkpoint is better — copying from {best_ckpt}", flush=True)
    else:
        if is_main:
            print("  Final weights are best — saving directly", flush=True)

    final_dir = os.path.join(OUTPUT_DIR, "final")
    if use_best:
        if global_rank == 0:
            print(f"\nCopying best checkpoint to {final_dir}...", flush=True)
            import shutil

            if os.path.exists(final_dir):
                shutil.rmtree(final_dir)
            shutil.copytree(
                best_ckpt,
                final_dir,
                ignore=shutil.ignore_patterns(
                    "global_step*", "zero_pp*", "optimizer*", "*.pt"
                ),
            )
            tokenizer.save_pretrained(final_dir)
        if torch.distributed.is_initialized():
            torch.distributed.barrier()
    else:
        if global_rank == 0:
            print("\nSaving final model...", flush=True)
        trainer.save_model(final_dir)
        tokenizer.save_pretrained(final_dir)

    if global_rank == 0:
        print(f"\n  Saved eval loss: {chosen_loss:.4f}", flush=True)
        print(f"  Saved eval perplexity: {math.exp(chosen_loss):.2f}", flush=True)
        print(f"  Model saved to: {final_dir}", flush=True)

        config = {
            "model_id": MODEL_ID,
            "epochs": EPOCHS,
            "lr": LR,
            "batch_size": BATCH_SIZE,
            "grad_accum": GRAD_ACCUM,
            "max_seq_len": MAX_SEQ_LEN,
            "effective_batch_tokens": BATCH_SIZE
            * GRAD_ACCUM
            * world_size
            * MAX_SEQ_LEN,
            "train_chunks": len(train_dataset),
            "val_chunks": len(val_dataset),
            "final_eval_loss": final_eval_loss,
            "best_checkpoint_eval_loss": best_eval_loss,
            "saved_eval_loss": chosen_loss,
            "saved_eval_ppl": math.exp(chosen_loss),
            "used_best_checkpoint": use_best,
            "world_size": world_size,
            "gpus_per_node": torch.cuda.device_count(),
        }
        with open(os.path.join(OUTPUT_DIR, "train-config.json"), "w") as f:
            json.dump(config, f, indent=2)

        print("\n=== Training complete ===", flush=True)


if __name__ == "__main__":
    main()
