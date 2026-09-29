# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Choose which files ship with a submitted notebook.

The payload is embedded in the workload, so it is bounded: 64 KiB once encoded
and 1 MiB of raw content. This module lets the review tab offer the files sitting
next to the notebook and reads back only the ones the researcher picked, with the
path and size checks that keep an embedded payload safe and small.

Only flat files in the notebook's own directory are offered. Nested trees do not
fit the transport (the envelope carries flat names), and the CLI's answer for a
real project is a working_dir archive or an image, not a bigger env var.
"""

from __future__ import annotations

import os
from itertools import islice
from pathlib import Path
from typing import Any, Dict, Iterable, List

from tau._payload import MAX_DECODED_BYTES

#: Names never worth embedding, mirroring the CLI's project excludes.
SKIP_NAMES = {
    "__pycache__",
    ".ipynb_checkpoints",
    ".git",
    ".venv",
    "venv",
    "node_modules",
    ".DS_Store",
    ".tau",
}
SKIP_SUFFIXES = {".pyc", ".pyo", ".so", ".dll", ".dylib", ".bin", ".png", ".jpg", ".jpeg", ".gif", ".zip", ".tar", ".gz", ".pdf", ".parquet"}
#: Bound on how many candidates the picker will offer.
MAX_CANDIDATES = 200
#: Individual file ceiling for a candidate to be offered or read.
MAX_FILE_BYTES = 256 * 1024


class FileSelectionError(Exception):
    """A user-actionable refusal about which files may ship."""

    def __init__(self, message: str, status: int = 400) -> None:
        super().__init__(message)
        self.status = status


def server_root(value: Any) -> Path:
    """Resolve the Jupyter server root, expanding a ~-relative setting."""
    return Path(os.path.expanduser(str(value or "."))).resolve()


def notebook_directory(root: Path, notebook_path: str) -> Path:
    """Resolve the directory that holds the notebook, inside the server root."""
    if not notebook_path or notebook_path.strip() != notebook_path:
        raise FileSelectionError("notebook path is required")
    expanded = Path(os.path.expanduser(notebook_path))
    candidate = expanded.resolve() if expanded.is_absolute() else (server_root(root) / expanded).resolve()
    directory = candidate.parent
    root_resolved = server_root(root)
    if directory != root_resolved and root_resolved not in directory.parents:
        raise FileSelectionError("notebook path resolves outside the Jupyter server root")
    return directory


def _skip(path: Path) -> bool:
    if path.name in SKIP_NAMES or path.name.startswith("."):
        return True
    return path.suffix.lower() in SKIP_SUFFIXES


def list_candidates(root: Path, notebook_path: str) -> Dict[str, Any]:
    """Flat files beside the notebook that could ship with it."""
    directory = notebook_directory(root, notebook_path)
    notebook_name = Path(notebook_path).name
    rows: List[Dict[str, Any]] = []
    warnings: List[str] = []
    try:
        entries = list(islice(directory.iterdir(), MAX_CANDIDATES + 1))
    except OSError as exc:
        return {"files": [], "readable": False,
                "warnings": [f"Could not read notebook directory. Check the path and file permissions, then refresh files. Details: {exc}"]}
    if len(entries) > MAX_CANDIDATES:
        warnings.append(f"Only the first {MAX_CANDIDATES} directory entries were inspected. Move this notebook and its companion files to a smaller directory if a file is missing.")
    entries = sorted(entries[:MAX_CANDIDATES], key=lambda item: item.name)
    for entry in entries:
        if not entry.is_file() or _skip(entry) or entry.name == notebook_name:
            continue
        try:
            size = entry.stat().st_size
        except OSError:
            continue
        if size > MAX_FILE_BYTES:
            warnings.append(f"{entry.name} is {size} bytes and is too large to embed.")
            continue
        rows.append({"name": entry.name, "size": size})
    return {"files": rows, "warnings": warnings, "budgetBytes": MAX_DECODED_BYTES}


def read_selected(directory: Path, names: Iterable[str]) -> Dict[str, bytes]:
    """Read the chosen files, refusing anything outside the flat, bounded set."""
    selected: Dict[str, bytes] = {}
    total = 0
    for raw in names:
        name = str(raw).strip()
        if not name:
            continue
        if name != Path(name).name or name in (".", "..") or "\\" in name:
            raise FileSelectionError(f"{name!r} is not a flat file name in the notebook directory")
        path = directory / name
        if not path.is_file():
            raise FileSelectionError(f"{name!r} was not found beside the notebook")
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
        if len(data) > MAX_FILE_BYTES:
            raise FileSelectionError(f"{name!r} is {len(data)} bytes, above the {MAX_FILE_BYTES}-byte per-file limit", status=413)
        total += len(data)
        if total > MAX_DECODED_BYTES:
            raise FileSelectionError(
                f"the selected files total {total} bytes, above the {MAX_DECODED_BYTES}-byte payload ceiling; "
                "embed fewer files or bake them into the runtime image",
                status=413,
            )
        selected[name] = data
    return selected


__all__ = [
    "FileSelectionError",
    "server_root",
    "list_candidates",
    "read_selected",
    "notebook_directory",
    "MAX_CANDIDATES",
    "MAX_FILE_BYTES",
    "SKIP_NAMES",
]
