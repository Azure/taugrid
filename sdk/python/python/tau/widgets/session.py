# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Resolve the current notebook path from the Jupyter server.

The kernel resolves the notebook it is running in via the Jupyter Sessions API
(``GET {base}/api/sessions``): the session whose ``kernel.id`` matches this
kernel is authoritative for ``path``. Resolution is a pure function of an
injectable ``opener``/session provider so it can be tested offline.
"""

from __future__ import annotations

import json
from typing import Any, Callable, List, Mapping, Optional


class NotebookNotResolved(Exception):
    """The current notebook could not be resolved from the Jupyter session API."""


def resolve_notebook_path(
    sessions: List[Mapping[str, Any]],
    kernel_id: Optional[str],
) -> Optional[str]:
    """Return the ``path`` for the session matching ``kernel_id``.

    Returns ``None`` (rather than raising) when there is no match, so callers
    can show the manual-path state. Prefers the top-level ``path``; falls back to
    ``notebook.path`` (legacy).
    """
    if not kernel_id:
        return None
    for session in sessions:
        kernel = session.get("kernel") or {}
        if str(kernel.get("id")) != str(kernel_id):
            continue
        path = session.get("path") or (session.get("notebook") or {}).get("path")
        if path:
            return str(path)
    return None


class SessionsApiResolver:
    """Resolve through a real (or fake) Jupyter ``/api/sessions`` endpoint."""

    def __init__(self, base_url: str, opener: Optional[Callable[[str], Any]] = None) -> None:
        self.base_url = base_url.rstrip("/")
        self._opener = opener or _default_sessions_get

    def sessions(self) -> List[Mapping[str, Any]]:
        url = f"{self.base_url}/api/sessions"
        data = self._opener(url)
        if isinstance(data, str):
            data = json.loads(data)
        return data if isinstance(data, list) else []


def _default_sessions_get(url: str) -> Any:  # pragma: no cover - only against a live server
    import urllib.request

    with urllib.request.urlopen(url) as resp:
        return json.loads(resp.read().decode("utf-8"))


__all__ = ["NotebookNotResolved", "resolve_notebook_path", "SessionsApiResolver"]