# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import json

import pytest

import tau
from tau import datasets, stellar

class _FakeClock:
    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds

class _RecordingSink:
    def __init__(self) -> None:
        self.calls: list[tuple[dict[str, float], int]] = []

    def __call__(self, metrics, step: int) -> None:
        self.calls.append((dict(metrics), step))

def _loader(clock: _FakeClock, waits: list[float]):
    for index, wait in enumerate(waits):
        clock.advance(wait)
        yield index

@pytest.fixture
def clock(monkeypatch) -> _FakeClock:
    fake = _FakeClock()
    monkeypatch.setattr(datasets, "perf_counter", fake)
    return fake

@pytest.fixture
def no_active_run(monkeypatch) -> None:
    monkeypatch.setattr(stellar, "_ACTIVE_RUN", None)

def test_timed_logs_wait_and_step_seconds_per_step(clock):
    sink = _RecordingSink()

    seen = []
    for item in tau.datasets.timed(_loader(clock, [0.5, 0.25, 0.125]), log=sink, start_step=10):
        seen.append(item)
        clock.advance(1.0)

    assert seen == [0, 1, 2]
    assert [step for _, step in sink.calls] == [10, 11, 12]
    assert sink.calls[0][0] == pytest.approx({"data/wait_seconds": 0.5, "data/step_seconds": 1.5})
    assert sink.calls[1][0] == pytest.approx({"data/wait_seconds": 0.25, "data/step_seconds": 1.25})
    assert sink.calls[2][0] == pytest.approx({
        "data/wait_seconds": 0.125,
        "data/step_seconds": 1.125,
        "data/epoch_wait_seconds": 0.875,
        "data/epoch_seconds": 3.875,
    })

def test_timed_namespaces_metrics_by_name(clock):
    sink = _RecordingSink()

    list(datasets.timed(_loader(clock, [0.5]), name="val_data", log=sink))

    assert sink.calls == [(
        {
            "val_data/wait_seconds": 0.5,
            "val_data/step_seconds": 0.5,
            "val_data/epoch_wait_seconds": 0.5,
            "val_data/epoch_seconds": 0.5,
        },
        0,
    )]

def test_timed_logs_final_step_when_loop_breaks(clock):
    sink = _RecordingSink()

    for item in datasets.timed(_loader(clock, [0.5, 0.5, 0.5]), log=sink):
        clock.advance(2.0)
        if item == 1:
            break

    assert [step for _, step in sink.calls] == [0, 1]
    assert sink.calls[1][0] == pytest.approx({
        "data/wait_seconds": 0.5,
        "data/step_seconds": 2.5,
        "data/epoch_wait_seconds": 1.0,
        "data/epoch_seconds": 5.0,
    })

def test_timed_writes_to_active_stellar_run(tmp_path, no_active_run):
    run = stellar.init(project="nanogpt", run="data-wait", dir=tmp_path / "runs")
    try:
        assert list(datasets.timed(range(3))) == [0, 1, 2]
    finally:
        stellar.finish()

    rows = [json.loads(line) for line in run.history_path.read_text().splitlines()]
    assert [row["_step"] for row in rows] == [0, 1, 2]
    assert all(row["data/wait_seconds"] >= 0 and row["data/step_seconds"] >= row["data/wait_seconds"] for row in rows)
    assert "data/epoch_wait_seconds" in rows[-1]
    assert "data/epoch_seconds" in rows[-1]

def test_timed_passes_through_without_stellar_run(monkeypatch, no_active_run):
    def fail_clock() -> float:
        raise AssertionError("pass-through must not read the clock")

    monkeypatch.setattr(datasets, "perf_counter", fail_clock)
    source = iter([1, 2, 3])

    wrapped = datasets.timed(source)

    assert wrapped is source
    assert list(wrapped) == [1, 2, 3]

def test_timed_empty_iterable_logs_nothing(clock):
    sink = _RecordingSink()

    assert list(datasets.timed([], log=sink)) == []
    assert sink.calls == []

def test_timed_propagates_iterator_exceptions(clock):
    sink = _RecordingSink()

    def broken():
        yield "first"
        raise OSError("shard unavailable")

    with pytest.raises(OSError, match="shard unavailable"):
        for _ in datasets.timed(broken(), log=sink):
            clock.advance(1.0)

    assert sink.calls == []

def test_timed_requires_name():
    with pytest.raises(ValueError, match="name is required"):
        datasets.timed([1], name="", log=_RecordingSink())

def test_stellar_active_run_tracks_init_and_finish(tmp_path, no_active_run):
    assert stellar.active_run() is None
    run = stellar.init(project="nanogpt", run="active", dir=tmp_path / "runs")
    assert stellar.active_run() is run
    stellar.finish()
    assert stellar.active_run() is None
