# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Loss/metrics sources for the notebook widget.

Three adapters map to the design's loss-source chain (metrics file -> stdout ->
portal series). Every adapter is a pure function of its inputs or takes an
injectable ``opener``, so the whole surface is testable offline.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional
from urllib.parse import urlencode

_TAG_RE = re.compile(r"^\s*(?:step\s*=\s*(\d+)\s+)?loss\s*=\s*([+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)\s*$")


@dataclass(frozen=True)
class MetricSample:
    step: int
    value: float


@dataclass
class MetricSeries:
    """Loss series for the panel; empty when no samples yet."""

    samples: List[MetricSample] = None  # type: ignore[assignment]

    def __post_init__(self) -> None:
        if self.samples is None:
            object.__setattr__(self, "samples", [])

    @property
    def has_data(self) -> bool:
        return len(self.samples) >= 1

    @property
    def first(self) -> Optional[MetricSample]:
        return self.samples[0] if self.samples else None

    @property
    def last(self) -> Optional[MetricSample]:
        return self.samples[-1] if self.samples else None


def parse_stdout_line(line: str) -> Optional[MetricSample]:
    """Parse a ``loss`` / ``step=.. loss=..`` line, or None if it does not match."""
    match = _TAG_RE.match(line)
    if not match:
        return None
    step_text, loss_text = match.groups()
    step = int(step_text) if step_text is not None else 0
    value = float(loss_text)
    return MetricSample(step=step, value=value)


def read_metrics_file(path: Any) -> MetricSeries:
    """Parse JSONL at ```` path; malformed lines are skipped.

    Each object is ``{"step": int, "loss": float}``; non-numeric entries are
    skipped rather than failing the whole stream.
    """
    series = MetricSeries()
    with open(str(path), "r", encoding="utf-8") as handle:
        for raw in handle:
            raw = raw.strip()
            if not raw:
                continue
            try:
                record = json.loads(raw)
            except json.JSONDecodeError:
                continue
            try:
                step = int(record["step"])
                value = float(record["loss"])
            except (KeyError, TypeError, ValueError):
                continue
            series.samples.append(MetricSample(step=step, value=value))
    return series


def parse_stdout(stream: Any) -> MetricSeries:
    """Collect loss samples from any iterable of lines using the stdout convention."""
    series = MetricSeries()
    for line in stream:
        sample = parse_stdout_line(line)
        if sample is not None:
            series.samples.append(sample)
    return series


class StellarSeriesClient:
    """Reads the portal ``/api/stellar/series`` endpoint through an injectable opener."""

    def __init__(self, base_url: str, opener: Optional[Callable[[str], Any]] = None) -> None:
        self.base_url = base_url.rstrip("/")
        self._opener = opener or _default_urlopen

    def series(self, *, target: str, metric: str = "train/loss", max_points: int = 1000) -> MetricSeries:
        params = urlencode({"target": target, "metric": metric, "max_points": max_points})
        url = f"{self.base_url}/api/stellar/series?{params}"
        body = self._opener(url)
        return _series_from_payload(body)


def _series_from_payload(payload: Any) -> MetricSeries:
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError:
            return MetricSeries()
    chart = (payload or {}).get("chart") or {}
    series = MetricSeries()
    for s in chart.get("series") or []:
        for point in s.get("values") or []:
            try:
                series.samples.append(MetricSample(step=int(point["step"]), value=float(point["value"])))
            except (KeyError, TypeError, ValueError):
                continue
    return series


def _default_urlopen(url: str) -> Any:  # pragma: no cover - exercised only against a real portal
    import urllib.request

    with urllib.request.urlopen(url) as resp:
        return json.loads(resp.read().decode("utf-8"))


def loss_delta(series: MetricSeries) -> Dict[str, Optional[Any]]:
    """Summarize a series the way the panel hero renders it."""
    if not series.has_data:
        return {"direction": None, "percent": None, "first": None, "last": None}
    first = series.first
    last = series.last
    if first is None or last is None or first.value == 0:
        return {"direction": None, "percent": None, "first": first, "last": last}
    change = (last.value - first.value) / abs(first.value)
    return {
        "direction": "down" if change < 0 else ("up" if change > 0 else "flat"),
        "percent": abs(change) * 100,
        "first": first,
        "last": last,
    }


__all__ = ["MetricSeries", "MetricSample", "read_metrics_file", "parse_stdout", "parse_stdout_line", "StellarSeriesClient", "loss_delta"]