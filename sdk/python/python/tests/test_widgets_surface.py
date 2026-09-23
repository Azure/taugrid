# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the notebook widget surface (status/metrics/session/render)."""

import tempfile
from pathlib import Path

from tau.widgets.metrics import (
    MetricSeries,
    MetricSample,
    loss_delta,
    parse_stdout,
    parse_stdout_line,
    read_metrics_file,
)
from tau.widgets.render import gpu_bar, loss_delta_text, svg_loss_chart
from tau.widgets.session import resolve_notebook_path
from tau.widgets.status import read_run_status


# --- metrics ---


def test_parse_stdout_line_forms():
    assert parse_stdout_line("loss=1.25") == MetricSample(step=0, value=1.25)
    assert parse_stdout_line("step=42 loss=1.25") == MetricSample(step=42, value=1.25)
    assert parse_stdout_line("loss=1e-3") is not None
    assert parse_stdout_line("step=3 loss=1.5 trailing") is None
    assert parse_stdout_line("random log line") is None


def test_stdout_collection_skips_nonmatching():
    series = parse_stdout(["epoch 1", "loss=0.5", "step=2 loss=0.4"])
    assert [s.step for s in series.samples] == [0, 2]


def test_read_metrics_file_skips_malformed():
    with tempfile.TemporaryDirectory() as d:
        p = Path(d) / "metrics.jsonl"
        p.write_text('{"step":1,"loss":0.9}\nnot json\n{"step":2,"loss":"bad"}\n{"step":3,"loss":0.8}\n')
        series = read_metrics_file(p)
        assert [s.step for s in series.samples] == [1, 3]


def test_loss_delta_down():
    series = MetricSeries([MetricSample(0, 0.9230), MetricSample(10, 0.1040)])
    d = loss_delta(series)
    assert d["direction"] == "down"

    text = loss_delta_text(series)
    assert text.startswith("loss down")
    assert "0.1040" in text


def test_loss_delta_text_waiting():
    assert loss_delta_text(MetricSeries()) == "waiting for the first steps"


# --- status ---


class FakeClient:
    def __init__(self, rayjob=None, pods=None):
        self._rayjob = rayjob
        self._pods = pods or []
        self.custom = _CustomAPIFake(self)
        self.core = _CoreAPIFake(self)

    def get_rayjob(self):
        return self._rayjob


class _CustomAPIFake:
    def __init__(self, owner):
        self._owner = owner

    def get_namespaced_custom_object(self, **kwargs):
        if self._owner._rayjob is None:
            raise Exception("not found")
        return self._owner._rayjob


class _CoreAPIFake:
    def __init__(self, owner):
        self._owner = owner

    def list_namespaced_pod(self, namespace, **kwargs):
        return {"items": self._owner._pods}


def test_read_run_status_found():
    client = FakeClient(rayjob={"status": {"jobStatus": "RUNNING"}})
    status = read_run_status(client, namespace="ns", name="r")
    assert status.existing is True
    assert status.state == "running"


def test_read_run_status_complete():
    client = FakeClient(rayjob={"status": {"jobStatus": "SUCCEEDED"}})
    status = read_run_status(client, namespace="ns", name="r")
    assert status.state == "complete"


def test_read_run_status_missing_never_raises():
    client = FakeClient(rayjob=None)
    status = read_run_status(client, namespace="ns", name="missing")
    assert status.existing is False
    assert status.state == "not_submitted"


# --- session ---


def test_resolve_notebook_path_matches_kernel():
    sessions = [
        {"kernel": {"id": "k1"}, "path": "a.ipynb"},
        {"kernel": {"id": "k2"}, "path": "b.ipynb"},
    ]
    assert resolve_notebook_path(sessions, "k2") == "b.ipynb"
    assert resolve_notebook_path(sessions, "nope") is None


def test_resolve_notebook_path_empty_kernel_id():
    assert resolve_notebook_path([], None) is None


# --- render ---


def test_svg_loss_chart_accessible_and_empty_below_two():
    assert svg_loss_chart(MetricSeries()) == ""
    series = MetricSeries([MetricSample(0, 1.0), MetricSample(1, 0.5)])
    svg = svg_loss_chart(series, label="train/loss")
    assert 'role="img"' in svg
    assert "aria-label" in svg
    assert "polyline" in svg


def test_gpu_bar_not_reported():
    assert "not reported" in gpu_bar(None, False)


def test_gpu_bar_numbered():
    assert "%" in gpu_bar(87.3, True)