# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the single-flight status watcher (no threads, no network).

The scheduler is injected, so the whole poll loop is driven synchronously by
invoking the recorded callback. These tests pin the single-flight contract:
one tick scheduled at a time, reschedule only after the read returns, a
terminal status stops the loop, and a late callback after stop/restart is
discarded via the generation tag.
"""

from __future__ import annotations

from typing import Any, Callable, List, Optional, Tuple

from tau.widgets.status import RunStatus
from tau.widgets.watcher import DEFAULT_INTERVAL, MIN_INTERVAL, StatusWatcher


class FakeScheduler:
    """Records schedule/cancel calls and lets a test fire the recorded tick."""

    def __init__(self) -> None:
        self.scheduled: List[Tuple[float, Callable[[], Any], str]] = []
        self.cancelled: List[str] = []
        self._counter = 0

    def schedule(self, delay: float, callback: Callable[[], Any]) -> str:
        self._counter += 1
        handle = f"handle-{self._counter}"
        self.scheduled.append((delay, callback, handle))
        return handle

    def cancel(self, handle: Any) -> None:
        self.cancelled.append(handle)

    # -- test helpers --
    @property
    def pending(self) -> Optional[Tuple[float, Callable[[], Any], str]]:
        return self.scheduled[-1] if self.scheduled else None

    def fire(self, index: int = -1) -> Any:
        """Invoke a recorded callback manually (default: the latest one)."""
        _delay, callback, _handle = self.scheduled[index]
        return callback()


def running_status(name: str = "run-1") -> RunStatus:
    return RunStatus(existing=True, name=name, namespace="ray", state="running", display_state="running")


def terminal_status(name: str = "run-1", state: str = "complete") -> RunStatus:
    return RunStatus(existing=True, name=name, namespace="ray", state=state, display_state=state)


def test_start_schedules_exactly_one_tick_at_clamped_interval():
    scheduler = FakeScheduler()
    watcher = StatusWatcher(lambda: running_status(), interval=1.0, min_interval=5.0, scheduler=scheduler)

    generation = watcher.start()

    assert watcher.active is True
    assert watcher.interval == 5.0
    assert generation == watcher.generation
    assert len(scheduler.scheduled) == 1
    delay, _callback, _handle = scheduler.scheduled[0]
    assert delay == 5.0


def test_non_terminal_status_reschedules():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []
    watcher = StatusWatcher(
        lambda: running_status(), on_update=updates.append, interval=30.0, scheduler=scheduler
    )
    watcher.start()
    assert len(scheduler.scheduled) == 1

    result = scheduler.fire()

    assert result is not None and result.state == "running"
    assert watcher.active is True
    assert len(updates) == 1 and updates[0] is not None and updates[0].state == "running"
    assert len(scheduler.scheduled) == 2
    assert scheduler.scheduled[1][0] == 30.0


def test_terminal_status_stops_loop():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []
    watcher = StatusWatcher(
        lambda: terminal_status(state="complete"),
        on_update=updates.append,
        interval=30.0,
        scheduler=scheduler,
    )
    watcher.start()

    result = scheduler.fire()

    assert result is not None and result.state == "complete"
    assert watcher.active is False
    assert len(updates) == 1
    assert len(scheduler.scheduled) == 1  # no reschedule after terminal
    assert scheduler.cancelled == []


def test_failed_status_is_also_terminal():
    scheduler = FakeScheduler()
    watcher = StatusWatcher(lambda: terminal_status(state="failed"), interval=30.0, scheduler=scheduler)
    watcher.start()

    result = scheduler.fire()

    assert result is not None and result.terminal is True
    assert watcher.active is False
    assert len(scheduler.scheduled) == 1


def test_read_exception_does_not_kill_loop():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []

    def boom() -> RunStatus:
        raise RuntimeError("api down")

    watcher = StatusWatcher(boom, on_update=updates.append, interval=30.0, scheduler=scheduler)
    watcher.start()

    result = scheduler.fire()

    assert result is None
    assert watcher.active is True
    assert updates == [None]
    assert len(scheduler.scheduled) == 2  # loop stays alive


def test_stop_cancels_pending_tick_and_discards_late_callback():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []
    watcher = StatusWatcher(
        lambda: running_status(), on_update=updates.append, interval=30.0, scheduler=scheduler
    )
    watcher.start()
    _delay, callback, handle = scheduler.scheduled[0]

    watcher.stop()

    assert watcher.active is False
    assert scheduler.cancelled == [handle]
    # A callback that already fired before the cancel lands must be discarded.
    callback()
    assert updates == []


def test_generation_changes_on_restart_and_old_reply_discarded():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []
    watcher = StatusWatcher(
        lambda: running_status(), on_update=updates.append, interval=30.0, scheduler=scheduler
    )

    first = watcher.start()
    _delay1, stale_callback, _handle1 = scheduler.scheduled[0]
    watcher.stop()
    second = watcher.start()

    assert first == 1
    assert watcher.generation == 3
    assert second == 3
    assert second > first

    # The stale generation's callback must not read or repaint.
    stale_callback()
    assert updates == []
    assert watcher.active is True
    assert len(scheduler.scheduled) == 2  # only the restart's tick is pending


def test_interval_below_min_interval_is_clamped():
    scheduler = FakeScheduler()
    watcher = StatusWatcher(lambda: running_status(), interval=0.5, min_interval=MIN_INTERVAL, scheduler=scheduler)

    assert watcher.interval == MIN_INTERVAL

    watcher.start()
    assert scheduler.scheduled[0][0] == MIN_INTERVAL


def test_default_interval_is_thirty_seconds():
    scheduler = FakeScheduler()
    watcher = StatusWatcher(lambda: running_status(), scheduler=scheduler)

    assert watcher.interval == DEFAULT_INTERVAL == 30.0


def test_tick_runs_synchronously_at_current_generation():
    scheduler = FakeScheduler()
    updates: List[Optional[RunStatus]] = []
    watcher = StatusWatcher(
        lambda: running_status("manual"), on_update=updates.append, interval=30.0, scheduler=scheduler
    )
    watcher.start()

    result = watcher.tick()

    assert result is not None and result.name == "manual"
    assert updates and updates[-1] is result
    assert scheduler.cancelled == []
