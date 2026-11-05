# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

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
    for item in datasets.timed(_loader(clock, [0.5, 0.25, 0.125]), log=sink, start_step=10):
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

_BROKEN_AT_SECOND_ITEM = {
    "data/wait_seconds": 0.5,
    "data/step_seconds": 2.5,
    "data/epoch_wait_seconds": 1.0,
    "data/epoch_seconds": 5.0,
}

def test_timed_context_manager_logs_final_step_when_loop_breaks(clock):
    sink = _RecordingSink()

    with datasets.timed(_loader(clock, [0.5, 0.5, 0.5]), log=sink) as batches:
        for item in batches:
            clock.advance(2.0)
            if item == 1:
                break

    assert [step for _, step in sink.calls] == [0, 1]
    assert sink.calls[1][0] == pytest.approx(_BROKEN_AT_SECOND_ITEM)

def test_timed_retained_iterator_logs_final_step_on_close(clock):
    sink = _RecordingSink()
    batches = datasets.timed(_loader(clock, [0.5, 0.5, 0.5]), log=sink)

    for item in batches:
        clock.advance(2.0)
        if item == 1:
            break

    assert [step for _, step in sink.calls] == [0]
    batches.close()
    assert [step for _, step in sink.calls] == [0, 1]
    assert sink.calls[1][0] == pytest.approx(_BROKEN_AT_SECOND_ITEM)
    batches.close()
    assert len(sink.calls) == 2

def test_timed_retained_iterator_resumes_after_break(clock):
    sink = _RecordingSink()
    batches = datasets.timed(_loader(clock, [0.5, 0.5, 0.5]), log=sink)

    assert next(batches) == 0
    assert list(batches) == [1, 2]
    assert [step for _, step in sink.calls] == [0, 1, 2]

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

    with datasets.timed(source) as wrapped:
        assert list(wrapped) == [1, 2, 3]

class _ClosableSource:
    def __init__(self) -> None:
        self.closed = False
        self.iterator = self._items()

    def _items(self):
        try:
            yield from range(3)
        finally:
            self.closed = True

def test_timed_close_closes_source_iterator(clock):
    source = _ClosableSource()
    sink = _RecordingSink()

    with datasets.timed(source.iterator, log=sink) as batches:
        for item in batches:
            if item == 1:
                break
        assert not source.closed

    assert source.closed
    assert [step for _, step in sink.calls] == [0, 1]

def test_timed_pass_through_close_closes_source_iterator(no_active_run):
    source = _ClosableSource()

    with datasets.timed(source.iterator) as batches:
        assert next(batches) == 0

    assert source.closed

def test_timed_close_closes_source_when_final_row_fails(clock):
    source = _ClosableSource()

    def failing_sink(metrics, step: int) -> None:
        raise RuntimeError("sink unavailable")

    batches = datasets.timed(source.iterator, log=failing_sink)
    assert next(batches) == 0

    with pytest.raises(RuntimeError, match="sink unavailable"):
        batches.close()
    assert source.closed

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

def test_timed_imports_without_stellar_module(tmp_path):
    shim = tmp_path / "tau"
    shim.mkdir()
    shutil.copy(Path(datasets.__file__), shim / "datasets.py")
    (shim / "__init__.py").write_text("from . import datasets as datasets\n", encoding="utf-8")
    code = f"""
import sys
sys.path.insert(0, {str(tmp_path)!r})
import tau
with tau.datasets.timed([1, 2]) as batches:
    print(list(batches))
"""

    res = subprocess.run([sys.executable, "-S", "-c", code], capture_output=True, text=True, check=False)

    assert res.returncode == 0, res.stderr
    assert res.stdout.strip() == "[1, 2]"
