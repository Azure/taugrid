# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pure HTML/SVG fragments the panel renders (no ipywidgets dependency).

Keeping these as pure functions means the visual rules are unit-testable offline
(golden text / aria attributes / numeric labels) without a notebook frontend.
"""

from __future__ import annotations

import html
from typing import Optional

from tau.widgets.metrics import MetricSeries, loss_delta
from tau.widgets.status import RunStatus


def loss_delta_text(series: MetricSeries) -> str:
    """The panel's one-line loss headline: words + numbers above the chart.

    ``loss down 88.4% 0.9230 -> 0.1040``, ``waiting for the first steps`` when
    fewer than two points, and a warn-flavored ``loss up`` when it rises.
    """
    if not series.has_data:
        return "waiting for the first steps"
    delta = loss_delta(series)
    first, last = series.first, series.last
    if first is None or last is None or delta["percent"] is None:
        return "waiting for the first steps"
    direction = delta["direction"]
    if direction == "up":
        return f"loss up {delta['percent']:.1f}% (warn) {first.value:.4f} -> {last.value:.4f}"
    if direction == "down":
        return f"loss down {delta['percent']:.1f}% {first.value:.4f} -> {last.value:.4f}"
    return "loss flat"


def svg_loss_chart(
    series: MetricSeries,
    *,
    width: int = 300,
    height: int = 80,
    label: str = "train/loss",
) -> str:
    """A minimal, dependency-free inline SVG line chart with an accessible label.

    Returns ``""`` when the series has fewer than two points so the panel can
    show the waiting state instead of a meaningless line.
    """
    if len(series.samples) < 2:
        return ""
    xs = [s.step for s in series.samples]
    ys = [s.value for s in series.samples]
    x0, x1, y0, y1 = min(xs), max(xs), min(ys), max(ys)
    span_x = (x1 - x0) or 1
    span_y = (y1 - y0) or 1.0

    points: list[str] = []
    for i, s in enumerate(series.samples):
        px = 2 + (s.step - x0) / span_x * (width - 4)
        py = height - 2 - (s.value - y0) / span_y * (height - 4)
        points.append(f"{px:.1f},{py:.1f}")

    polyline = " ".join(points)
    return (
        f"<svg width=\"{width}\" height=\"{height}\" role=\"img\" "
        f"aria-label=\"{html.escape(label)} series with {len(series.samples)} points\">"
        f"<polyline fill=\"none\" stroke=\"#2563eb\" stroke-width=\"1.5\" "
        f"points=\"{polyline}\"/></svg>"
    )


def gpu_bar(percent: Optional[float], observed: bool, width: int = 20) -> str:
    """One per-device GPU utilization bar; renders ``not reported`` when unknown.

    The numeric percentage is printed so color is never the only signal.
    """
    if not observed or percent is None:
        return "<span class=\"tif not-reported\">not reported</span>"
    fill = max(0, min(1000, int(percent * 10)))  # percent in tenths
    frac = fill / 10.0
    n = int(round(width * frac))
    n = max(0, min(width, n))
    bar = "\u2588" * n
    return f"<code>{html.escape(bar)}</code> {percent:.1f}%"


_STATE_LABELS = {
    "queued": "Pending (not yet admitted)",
    "running": "Running",
    "failed": "Failed",
    "complete": "Complete",
    "not_submitted": "Not submitted",
    "unknown": "Unknown",
}


def state_badge(state: Optional[str]) -> str:
    """A text badge for the run state; text carries the meaning, not color."""
    key = (state or "unknown").lower()
    label = _STATE_LABELS.get(key, key.replace("_", " ").title())
    return f"<span class=\"tg-badge tg-{html.escape(key)}\">{html.escape(label)}</span>"


def status_header_html(status: "RunStatus") -> str:
    """The panel's one-line run header built from a normalized RunStatus.

    Includes run identity, the state badge, queue, pod readiness, the Ray
    cluster name, and any message. All externally supplied text is escaped.
    """
    if not status.existing:
        return (
            "<div class=\"tg-status\">"
            f"{state_badge(status.state)} run {html.escape(status.namespace)}/{html.escape(status.name)} not found"
            "</div>"
        )

    parts = [
        f"<code>{html.escape(status.namespace)}/{html.escape(status.name)}</code>",
        state_badge(status.state),
    ]
    if status.queue:
        parts.append(f"queue <code>{html.escape(status.queue)}</code>")
    if status.total_pods:
        parts.append(f"{status.ready_pods}/{status.total_pods} pods ready")
    if status.ray_cluster_name:
        parts.append(f"ray cluster <code>{html.escape(status.ray_cluster_name)}</code>")
    if status.job_id:
        parts.append(f"job <code>{html.escape(status.job_id)}</code>")
    if status.message:
        parts.append(html.escape(status.message))

    return "<div class=\"tg-status\">" + " &middot; ".join(parts) + "</div>"


def diagnostics_html(status: "RunStatus") -> str:
    """Render diagnostics as a list; empty string when there are none."""
    if not status.diagnostics:
        return ""
    rows = []
    for item in status.diagnostics:
        suggestion = f" <span class=\"tg-suggestion\">{html.escape(item.suggestion)}</span>" if item.suggestion else ""
        rows.append(
            f"<li class=\"tg-diagnostic tg-{html.escape(item.severity)}\">"
            f"<code>{html.escape(item.code)}</code> {html.escape(item.message)}{suggestion}</li>"
        )
    return "<ul class=\"tg-diagnostics\">" + "".join(rows) + "</ul>"


__all__ = [
    "loss_delta_text",
    "svg_loss_chart",
    "gpu_bar",
    "state_badge",
    "status_header_html",
    "diagnostics_html",
]