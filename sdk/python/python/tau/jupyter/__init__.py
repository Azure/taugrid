# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""TauGrid JupyterLab extension entry points.

This subpackage is the server half of the TauGrid JupyterLab prebuilt extension.
It registers two things with Jupyter:

* a prebuilt labextension (TypeScript UI bundled in the wheel) via
  _jupyter_labextension_paths, and
* a server extension REST API via _jupyter_server_extension_points.

Importing this module requires the tau[widgets] extra (jupyter_server,
kubernetes, ipywidgets). A plain import tau does not.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any, Dict, List


def _jupyter_labextension_paths() -> List[Dict[str, str]]:
    """Where JupyterLab finds the prebuilt frontend assets."""
    here = Path(__file__).parent.resolve()
    src = here.parent / "labextension"
    return [{"src": str(src), "dest": "taugrid-jupyterlab"}]


def _jupyter_server_extension_points() -> List[Dict[str, str]]:
    return [{"module": "tau.jupyter.server"}]


def _load_jupyter_server_extension(server_app: Any) -> None:
    """Legacy loader kept for jupyter_server < 2 compatibility."""
    from tau.jupyter.server import _load_jupyter_server_extension

    _load_jupyter_server_extension(server_app)


__all__ = [
    "_jupyter_labextension_paths",
    "_jupyter_server_extension_points",
    "_load_jupyter_server_extension",
]
