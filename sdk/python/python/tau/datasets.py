# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Dataset helpers for Tau workloads: portable file references and data-loading timing."""

from __future__ import annotations

from collections.abc import Callable, Iterable, Iterator, Mapping
from pathlib import Path, PurePosixPath, PureWindowsPath
from time import perf_counter
from typing import TypeVar

_T = TypeVar("_T")

_MetricsSink = Callable[[Mapping[str, float], int], object]

def dataset_file_reference(root: str | Path, raw_path: str, *, field_name: str = "path") -> tuple[str, Path]:
    """Return a portable dataset-relative path and resolved local path.

    Dataset CSVs and manifests should carry paths relative to the mounted
    dataset root. This helper normalizes those records to POSIX separators while
    rejecting absolute paths or references that resolve outside ``root``.
    """

    rel_input = str(raw_path).strip()
    if not rel_input:
        raise ValueError(f"{field_name} must reference a file under dataset root: {raw_path!r}")

    posix_input = PurePosixPath(rel_input)
    windows_input = PureWindowsPath(rel_input)
    if (
        posix_input.is_absolute()
        or windows_input.is_absolute()
        or bool(windows_input.drive)
        or bool(windows_input.root)
    ):
        raise ValueError(f"{field_name} must be relative to dataset root: {raw_path!r}")

    normalized = PurePosixPath(rel_input.replace("\\", "/"))
    resolved_root = Path(root).expanduser().resolve(strict=False)
    candidate = (resolved_root / Path(*normalized.parts)).resolve(strict=False)
    if not candidate.is_relative_to(resolved_root):
        raise ValueError(f"{field_name} escapes dataset root: {raw_path!r}")

    rel_path = candidate.relative_to(resolved_root).as_posix()
    if rel_path == ".":
        raise ValueError(f"{field_name} must reference a file under dataset root: {raw_path!r}")
    return rel_path, candidate

def timed(
    iterable: Iterable[_T],
    name: str = "data",
    *,
    log: _MetricsSink | None = None,
    start_step: int = 0,
) -> TimedIterator[_T]:
    """Iterate ``iterable`` and log per-step data-loading time to Stellar.

    Each item is one step. For step ``n`` the wrapper logs
    ``{name}/wait_seconds``, the time blocked fetching item ``n``, and
    ``{name}/step_seconds``, the time from requesting item ``n`` to requesting
    item ``n + 1``, so step time includes the wait. The final step of a pass
    also carries ``{name}/epoch_wait_seconds`` and ``{name}/epoch_seconds``.
    Steps are numbered from ``start_step``.

    Metrics go to ``log(metrics, step)`` when given, otherwise to the active
    ``tau.stellar`` run. With neither, the iterable passes through untimed.

    The final row is written when the iterable is exhausted or the returned
    iterator is closed. Closing it also closes the source iterator when it has
    a ``close()`` method, releasing loader workers, files, or streams. A
    ``for`` loop that breaks does not close it, so use
    ``with timed(...) as batches:`` or call ``close()`` after an early exit.
    """

    if not name:
        raise ValueError("timed name is required")
    source = iter(iterable)
    sink = log if log is not None else _active_run_sink()
    if sink is None:
        return TimedIterator(source)
    return TimedIterator(_timed(source, name, sink, int(start_step)), source)

class TimedIterator(Iterator[_T]):
    """Iterator returned by :func:`timed`; closing it writes the final row and closes the source."""

    def __init__(self, iterator: Iterator[_T], source: Iterator[_T] | None = None) -> None:
        self._iterator = iterator
        self._source = source

    def __next__(self) -> _T:
        return next(self._iterator)

    def close(self) -> None:
        try:
            _close(self._iterator)
        finally:
            if self._source is not None:
                _close(self._source)

    def __enter__(self) -> TimedIterator[_T]:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

def _active_run_sink() -> _MetricsSink | None:
    try:
        from tau import stellar
    except ImportError:
        return None
    run = stellar.active_run()
    return None if run is None else run.log

def _close(iterator: object) -> None:
    close = getattr(iterator, "close", None)
    if callable(close):
        close()

def _timed(iterator: Iterator[_T], name: str, sink: _MetricsSink, step: int) -> Iterator[_T]:
    wait_key = f"{name}/wait_seconds"
    step_key = f"{name}/step_seconds"
    started = perf_counter()
    previous_requested: float | None = None
    previous_wait = 0.0
    total_wait = 0.0
    try:
        while True:
            requested = perf_counter()
            try:
                item = next(iterator)
            except StopIteration:
                break
            wait = perf_counter() - requested
            if previous_requested is not None:
                sink({wait_key: previous_wait, step_key: requested - previous_requested}, step)
                step += 1
            previous_requested = requested
            previous_wait = wait
            total_wait += wait
            yield item
    except GeneratorExit:
        requested = perf_counter()
        _log_final_step(sink, name, step, previous_requested, previous_wait, total_wait, started, requested)
        raise
    _log_final_step(sink, name, step, previous_requested, previous_wait, total_wait, started, requested)

def _log_final_step(
    sink: _MetricsSink,
    name: str,
    step: int,
    previous_requested: float | None,
    previous_wait: float,
    total_wait: float,
    started: float,
    ended: float,
) -> None:
    if previous_requested is None:
        return
    sink(
        {
            f"{name}/wait_seconds": previous_wait,
            f"{name}/step_seconds": ended - previous_requested,
            f"{name}/epoch_wait_seconds": total_wait,
            f"{name}/epoch_seconds": ended - started,
        },
        step,
    )

__all__ = ["TimedIterator", "dataset_file_reference", "timed"]
