# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""TauGrid notebook plugin widgets (tau.widgets).

Public surface for the platform-authored template cell:

    import tau.widgets as tg
    tg.panel()

Importing this subpackage requires the tau[widgets] extra (ipywidgets +
kubernetes); a plain import tau does not.
"""

from __future__ import annotations

from tau.widgets.embed import EmbedView, PortalProxy, iframe_html, ray_dashboard_path, run_view_path
from tau.widgets.panel import TauGridPanel, panel
from tau.widgets.status import Diagnostic, PodInfo, RunStatus, RunSummary, list_runs, read_run_status
from tau.widgets.watcher import StatusWatcher

__all__ = [
    "TauGridPanel",
    "panel",
    "RunStatus",
    "RunSummary",
    "PodInfo",
    "Diagnostic",
    "read_run_status",
    "list_runs",
    "StatusWatcher",
    "EmbedView",
    "PortalProxy",
    "iframe_html",
    "run_view_path",
    "ray_dashboard_path",
]
