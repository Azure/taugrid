# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Packaging glue for the TauGrid JupyterLab extension.

PEP 621 metadata lives in pyproject.toml. This shim only adds the data files a
prebuilt JupyterLab extension needs at install time, which cannot be expressed
declaratively because the bundler emits content-hashed asset names:

* the prebuilt labextension under share/jupyter/labextensions/taugrid-jupyterlab
  so JupyterLab discovers it without a manual 'labextension develop' step, and
* an etc/jupyter/jupyter_server_config.d entry that enables the tau.jupyter
  server extension on install (Jupyter Server reads enabled extensions from the
  environment config dir, not from share/jupyter).

Without these, 'pip install tau[widgets]' installs the Python code but neither
JupyterLab nor Jupyter Server registers the extension.
"""

from __future__ import annotations

import json
from pathlib import Path

from setuptools import setup

HERE = Path(__file__).parent
LABEXT_NAME = "taugrid-jupyterlab"
LABEXT_REL = "tau/labextension"
CONFIG_SRC_REL = "share/jupyter/jupyter_server_config.d/taugrid.json"

manifest_path = HERE / LABEXT_REL / "package.json"
try:
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    entry = manifest["jupyterlab"]["_build"]["load"].replace("\\", "/")
    if not entry.startswith("static/") or not (HERE / LABEXT_REL / entry).is_file():
        raise ValueError("missing frontend entrypoint")
except (OSError, ValueError, KeyError, TypeError, AttributeError) as exc:
    raise RuntimeError("Build the prebuilt notebook frontend with npm run build before packaging") from exc

data_files: list[tuple[str, list[str]]] = []

static_dir = HERE / LABEXT_REL / "static"
if (HERE / LABEXT_REL / "package.json").is_file():
    data_files.append((f"share/jupyter/labextensions/{LABEXT_NAME}", [f"{LABEXT_REL}/package.json"]))
    if static_dir.is_dir():
        static_rel = [
            f"{LABEXT_REL}/static/{p.name}" for p in sorted(static_dir.glob("*")) if p.is_file()
        ]
        if static_rel:
            data_files.append((f"share/jupyter/labextensions/{LABEXT_NAME}/static", static_rel))

# Auto-enable the server extension so the /taugrid/api routes exist after install.
config_path = HERE / CONFIG_SRC_REL
config_path.parent.mkdir(parents=True, exist_ok=True)
config_path.write_text(
    json.dumps({"ServerApp": {"jpserver_extensions": {"tau.jupyter": True}}}, indent=2) + "\n",
    encoding="utf-8",
)
data_files.append(("etc/jupyter/jupyter_server_config.d", [CONFIG_SRC_REL]))

setup(data_files=data_files)
