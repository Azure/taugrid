# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""JSONL data loading helpers for Tau examples and jobs."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def read_jsonl_objects(path: str | Path, max_records: int = 0) -> list[dict[str, Any]]:
    """Read JSONL records, requiring each nonblank line to be a JSON object."""

    if max_records < 0:
        raise ValueError("max_records must be non-negative")

    jsonl_path = Path(path)
    records: list[dict[str, Any]] = []
    with jsonl_path.open(encoding="utf-8") as handle:
        for line_no, line in enumerate(handle, 1):
            raw = line.strip()
            if not raw:
                continue
            try:
                record = json.loads(raw)
            except json.JSONDecodeError as exc:
                raise ValueError(f"{jsonl_path}:{line_no} invalid JSON: {exc.msg}") from exc
            if not isinstance(record, dict):
                raise ValueError(f"{jsonl_path}:{line_no} must contain a JSON object")
            records.append(record)
            if max_records and len(records) >= max_records:
                break

    if not records:
        raise ValueError(f"{jsonl_path} contains no records")
    return records


__all__ = ["read_jsonl_objects"]
