# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Single-flight status watcher for the notebook panel.

The panel polls a run's status on a fixed delay. Two properties matter and are
enforced here rather than in the panel:

* Single-flight: the next tick is scheduled only after the previous read
  finishes, so a slow API call cannot pile up overlapping requests.
* Generation-tagged: every start/stop bumps a generation counter. A late reply
  from a stopped or rebound watcher is discarded instead of repainting the
  panel.

The scheduler and clock are injectable, so the whole loop is testable offline
with a fake scheduler and a fake status reader (no threads, no network).
"""

from __future__ import annotations

import threading
from typing import Any, Callable, Optional

from tau.widgets.status import RunStatus

#: Default poll interval, matching the design (30s default, 5s minimum).
DEFAULT_INTERVAL = 30.0
MIN_INTERVAL = 5.0


class Scheduler:
    """Minimal scheduler seam: schedule a one-shot callback, cancel it."""

    def schedule(self, delay: float, callback: Callable[[], Any]) -> Any:  # pragma: no cover - seam
        raise NotImplementedError

    def cancel(self, handle: Any) -> None:  # pragma: no cover - seam
        raise NotImplementedError


class ThreadScheduler(Scheduler):
    """Default scheduler backed by a daemon thread timer."""

    def schedule(self, delay: float, callback: Callable[[], Any]) -> Any:
        timer = threading.Timer(delay, callback)
        timer.daemon = True
        timer.start()
        return timer

    def cancel(self, handle: Any) -> None:
        try:
            handle.cancel()
        except Exception:  # pragma: no cover - a fired timer cannot be cancelled
            pass


class StatusWatcher:
    """Poll a run's status on a fixed delay until it reaches a terminal state.

    read_status is a zero-argument callable returning a RunStatus (or None).
    on_update is called with each fresh status on the calling thread; the panel
    uses it to repaint. Both are injected so tests drive the loop directly.
    """

    def __init__(
        self,
        read_status: Callable[[], Optional[RunStatus]],
        *,
        on_update: Optional[Callable[[Optional[RunStatus]], None]] = None,
        interval: float = DEFAULT_INTERVAL,
        min_interval: float = MIN_INTERVAL,
        scheduler: Optional[Scheduler] = None,
    ) -> None:
        self._read = read_status
        self._on_update = on_update
        self.interval = max(float(interval), float(min_interval))
        self._scheduler = scheduler or ThreadScheduler()
        self._generation = 0
        self._handle: Any = None
        self._active = False

    @property
    def active(self) -> bool:
        return self._active

    @property
    def generation(self) -> int:
        return self._generation

    def start(self) -> int:
        """Begin watching. Returns the new generation for correlation."""
        self._generation += 1
        self._active = True
        self._schedule(self._generation)
        return self._generation

    def stop(self) -> None:
        """Stop watching. Invalidates any in-flight reply via the generation."""
        self._generation += 1
        self._active = False
        if self._handle is not None:
            self._scheduler.cancel(self._handle)
            self._handle = None

    def tick(self) -> Optional[RunStatus]:
        """Run one refresh synchronously (used by tests and refresh-now)."""
        return self._tick(self._generation)

    def _schedule(self, generation: int) -> None:
        if not self._active or generation != self._generation:
            return
        self._handle = self._scheduler.schedule(self.interval, lambda: self._tick(generation))

    def _tick(self, generation: int) -> Optional[RunStatus]:
        if not self._active or generation != self._generation:
            return None
        try:
            status = self._read()
        except Exception:  # per-reader failure isolation: keep the loop alive
            status = None
        # A stop/rebind during the read invalidates this reply.
        if generation != self._generation:
            return None
        if self._on_update is not None:
            self._on_update(status)
        if status is not None and status.terminal:
            self._active = False
            return status
        self._schedule(generation)
        return status


__all__ = ["StatusWatcher", "Scheduler", "ThreadScheduler", "DEFAULT_INTERVAL", "MIN_INTERVAL"]
