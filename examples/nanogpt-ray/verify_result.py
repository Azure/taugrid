#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Verify a Tau nanoGPT result bundle."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def read_history(path: Path) -> list[dict]:
    rows = []
    with path.open(encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--metrics", required=True)
    parser.add_argument("--history", required=True)
    parser.add_argument("--require-cuda", action="store_true")
    parser.add_argument("--require-fineweb", action="store_true")
    parser.add_argument("--require-hellaswag", action="store_true")
    parser.add_argument("--min-history-rows", type=int, default=2)
    parser.add_argument("--min-parameters", type=int, default=0)
    parser.add_argument("--min-val-improvement", type=float, default=1e-6)
    parser.add_argument("--min-hellaswag", type=float, default=0.0)
    parser.add_argument("--require-target", action="store_true")
    parser.add_argument("--max-val-loss", type=float)
    args = parser.parse_args()

    metrics = json.loads(Path(args.metrics).read_text(encoding="utf-8"))
    history = read_history(Path(args.history))
    if len(history) < args.min_history_rows:
        raise SystemExit(f"history rows {len(history)} < required {args.min_history_rows}")
    if args.require_cuda and metrics.get("device") != "cuda":
        raise SystemExit(f"expected cuda device, got {metrics.get('device')!r}")
    if args.require_fineweb and "fineweb" not in str(metrics.get("dataset_source", "")).lower():
        raise SystemExit(f"expected FineWeb dataset source, got {metrics.get('dataset_source')!r}")
    if int(metrics.get("parameter_count") or 0) < args.min_parameters:
        raise SystemExit(f"parameter_count {metrics.get('parameter_count')} < required {args.min_parameters}")
    if not metrics.get("has_heldout_validation"):
        raise SystemExit("held-out validation is unavailable")
    initial_val = float(metrics["initial_val_loss"])
    best_val = float(metrics["best_val_loss"])
    if best_val > initial_val - args.min_val_improvement:
        raise SystemExit(f"validation loss did not improve by {args.min_val_improvement}: initial={initial_val} best={best_val}")
    if best_val <= 0:
        raise SystemExit("best_val_loss must be positive")
    max_val_loss = args.max_val_loss
    if max_val_loss is None and args.require_target:
        max_val_loss = float(metrics["target_val_loss"])
    if max_val_loss is not None and best_val > max_val_loss:
        raise SystemExit(f"best_val_loss {best_val} > required {max_val_loss}")
    if args.require_target and not metrics.get("target_val_loss_met"):
        raise SystemExit("target_val_loss_met is false")
    hellaswag_accuracy = metrics.get("hellaswag_accuracy")
    if args.require_hellaswag and hellaswag_accuracy is None:
        raise SystemExit("HellaSwag accuracy missing")
    if hellaswag_accuracy is not None and float(hellaswag_accuracy) < args.min_hellaswag:
        raise SystemExit(f"HellaSwag accuracy {hellaswag_accuracy} < required {args.min_hellaswag}")
    first = history[0]
    last = history[-1]
    summary = {
        "run": metrics.get("run"),
        "dataset_source": metrics.get("dataset_source"),
        "device": metrics.get("device"),
        "parameter_count": metrics.get("parameter_count"),
        "history_rows": len(history),
        "first_train_loss": first.get("train/loss"),
        "last_train_loss": last.get("train/loss"),
        "best_val_loss": metrics.get("best_val_loss"),
        "target_val_loss": metrics.get("target_val_loss"),
        "target_val_loss_met": metrics.get("target_val_loss_met"),
        "hellaswag_accuracy": hellaswag_accuracy,
    }
    print(json.dumps(summary, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
