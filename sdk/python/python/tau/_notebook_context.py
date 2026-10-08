# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Harvest notebook context so a submitted run carries what the notebook needs.

A notebook carries more context than a plain script: the pip installs it runs,
the environment variables it sets, and the modules it imports. The CLI expresses
these as run-config fields (runtime.pip, runtime.env, runtime.env_secret) and
the renderer turns them into runtimeEnvYAML and container env vars. The plugin
reads the same fields out of the notebook so a one-click submit reproduces the
notebook's environment instead of silently dropping it.

Extraction is a pure function of the notebook bytes, so it is offline-testable.
Explicit caller arguments always win over harvested values; the merge happens in
the panel.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Dict, List

_PIP_RE = re.compile(
    r"^\s*[!%]\s*(?:pip3?|python\s+-m\s+pip)\s+install\s+(?P<args>.+)$", re.IGNORECASE
)
_CONDA_RE = re.compile(r"^\s*[!%]\s*conda\s+install\s+(?P<args>.+)$", re.IGNORECASE)
_ENV_MAGIC_RE = re.compile(r"^\s*%\s*env\s+(?P<key>[A-Za-z_]\w*)\s*=\s*(?P<value>.*)$")
_EXPORT_RE = re.compile(r"^\s*!\s*export\s+(?P<key>[A-Za-z_]\w*)=(?P<value>.*)$")
_OS_ENVIRON_RE = re.compile(
    r"os\.environ\[\s*[\"'](?P<key>[A-Za-z_]\w*)[\"']\s*\]\s*=\s*[\"'](?P<value>[^\"']*)[\"']"
)
_IMPORT_RE = re.compile(r"^\s*(?:import|from)\s+(?P<module>[A-Za-z_]\w*)")
_FLAGS = {"-q", "--quiet", "-u", "--upgrade", "--user", "-y", "--yes", "--no-cache-dir"}


@dataclass
class NotebookContext:
    """Environment the notebook declares, ready to map onto run-config fields."""

    pip: List[str] = field(default_factory=list)
    env: Dict[str, str] = field(default_factory=dict)
    imports: List[str] = field(default_factory=list)
    notes: List[str] = field(default_factory=list)

    @property
    def has_data(self) -> bool:
        return bool(self.pip or self.env or self.imports)


def extract_context(notebook_bytes: bytes) -> NotebookContext:
    """Read pip installs, env vars, and imports out of a notebook.

    Malformed notebook JSON yields an empty context rather than raising, so a
    submit can still proceed (the packager validates the notebook separately).
    """
    context = NotebookContext()
    try:
        notebook = json.loads(notebook_bytes.decode("utf-8"))
    except Exception:
        return context

    for cell in notebook.get("cells", []):
        if not isinstance(cell, dict) or cell.get("cell_type") != "code":
            continue
        source = cell.get("source")
        if isinstance(source, list):
            text = "".join(str(part) for part in source)
        else:
            text = str(source or "")
        for line in text.splitlines():
            _scan_line(line, context)
    return context


def _scan_line(line: str, context: NotebookContext) -> None:
    pip = _PIP_RE.match(line)
    if pip:
        _collect_pip(pip.group("args"), context)
        return
    conda = _CONDA_RE.match(line)
    if conda:
        context.notes.append(
            "conda install is not reproduced by the notebook plugin; use pip or a custom image"
        )
        return
    env_magic = _ENV_MAGIC_RE.match(line)
    if env_magic:
        context.env[env_magic.group("key")] = _unquote(env_magic.group("value"))
        return
    export = _EXPORT_RE.match(line)
    if export:
        context.env[export.group("key")] = _unquote(export.group("value"))
        return
    os_environ = _OS_ENVIRON_RE.search(line)
    if os_environ:
        context.env[os_environ.group("key")] = os_environ.group("value")
        return
    imported = _IMPORT_RE.match(line)
    if imported:
        module = imported.group("module")
        if module not in context.imports:
            context.imports.append(module)


def _collect_pip(args: str, context: NotebookContext) -> None:
    tokens = args.split()
    index = 0
    while index < len(tokens):
        token = tokens[index]
        if token in ("-r", "--requirement"):
            context.notes.append(
                "a requirements file is referenced in the notebook; inline the packages or bake them into a custom image"
            )
            index += 2
            continue
        if token.startswith("-"):
            index += 1
            continue
        if token not in _FLAGS and token not in context.pip:
            context.pip.append(token)
        index += 1


def _unquote(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
        return value[1:-1]
    return value


__all__ = ["NotebookContext", "extract_context"]
