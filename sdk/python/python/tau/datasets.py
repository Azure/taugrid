# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Dataset/example file path helpers for Tau workloads."""

from __future__ import annotations

from pathlib import Path, PurePosixPath, PureWindowsPath


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


__all__ = ["dataset_file_reference"]
