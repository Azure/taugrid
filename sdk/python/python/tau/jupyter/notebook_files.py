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

import errno
import os
import stat
from dataclasses import dataclass
from itertools import islice
from pathlib import Path
from typing import Any, Callable, Dict, Iterable, List, Optional

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
_DIRECTORY_OPEN_FLAGS = (
    os.O_RDONLY
    | getattr(os, "O_DIRECTORY", 0)
    | getattr(os, "O_NOFOLLOW", 0)
    | getattr(os, "O_CLOEXEC", 0)
)
_SYMLINK_OPEN_ERRORS = {errno.ELOOP}


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


def _inside(path: Path, directory: Path) -> bool:
    """Whether ``path`` is ``directory`` or lives beneath it.

    A parent test rather than a string prefix, so a sibling whose name merely
    starts with the directory's name (``/root/evil`` beside ``/root/ev``) is
    never accepted as contained. This is the same containment stance
    :func:`notebook_directory` takes for the notebook path itself.
    """
    return path == directory or directory in path.parents


@dataclass(frozen=True)
class _DirectoryIdentity:
    device: int
    inode: int


def _stat_directory(descriptor: Optional[int], path: Path) -> _DirectoryIdentity:
    opened = os.fstat(descriptor) if descriptor is not None else os.lstat(path)
    if not stat.S_ISDIR(opened.st_mode):
        raise FileSelectionError("the notebook directory is no longer a directory", status=409)
    return _DirectoryIdentity(opened.st_dev, opened.st_ino)


@dataclass(frozen=True)
class _OpenedDirectory:
    descriptor: Optional[int]
    path: Path


def _verify_authorized_directory(directory: _OpenedDirectory) -> None:
    try:
        expected = os.lstat(directory.path)
    except OSError as exc:
        raise FileSelectionError(f"the notebook directory was moved or replaced before it could be read: {exc}", status=409) from exc
    if stat.S_ISLNK(expected.st_mode):
        raise FileSelectionError("the notebook directory was replaced by a symlink before it could be read", status=409)
    if not stat.S_ISDIR(expected.st_mode):
        raise FileSelectionError("the notebook directory was replaced by something other than a directory", status=409)
    opened = _stat_directory(directory.descriptor, directory.path)
    if (expected.st_dev, expected.st_ino) != (opened.device, opened.inode):
        raise FileSelectionError("the notebook directory was moved or replaced before it could be read", status=409)


def _open_authorized_directory(path: Path) -> _OpenedDirectory:
    if os.name == "nt":
        _stat_directory(None, path)
        return _OpenedDirectory(None, path)
    try:
        descriptor = os.open(path, _DIRECTORY_OPEN_FLAGS)
    except FileNotFoundError as exc:
        raise FileSelectionError(f"the notebook directory was not found: {exc}", status=409) from exc
    except OSError as exc:
        try:
            current = os.lstat(path)
        except OSError:
            current = None
        if current is not None and stat.S_ISLNK(current.st_mode):
            raise FileSelectionError("the notebook directory was replaced by a symlink before it could be read", status=409) from exc
        raise FileSelectionError(f"could not open the notebook directory: {exc}", status=502) from exc
    directory = _OpenedDirectory(descriptor, path)
    try:
        _verify_authorized_directory(directory)
    except BaseException:
        os.close(descriptor)
        raise
    return directory


_DIRECTORY_BINDING = os.name != "nt" and os.open in os.supports_dir_fd and os.stat in os.supports_dir_fd and hasattr(os, "O_NOFOLLOW")


def _read_bound_file(directory_fd: int, name: str) -> bytes:
    flags = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        descriptor = os.open(name, flags, dir_fd=directory_fd)
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise FileSelectionError(f"{name!r} is a symlink, not a file beside the notebook", status=409) from exc
        if exc.errno == errno.ENOENT:
            raise FileSelectionError(f"{name!r} was not found beside the notebook", status=409) from exc
        raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISREG(opened.st_mode):
            raise FileSelectionError(f"{name!r} is not a regular file beside the notebook", status=409)
        with os.fdopen(os.dup(descriptor), "rb") as handle:
            return handle.read(MAX_FILE_BYTES + 1)
    except FileSelectionError:
        raise
    except OSError as exc:
        raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
    finally:
        os.close(descriptor)


def _is_windows_reparse_point(file_stat: os.stat_result) -> bool:
    return bool(getattr(file_stat, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0))


def _read_verified_file(directory: Path, name: str) -> bytes:
    path = directory / name
    try:
        before = os.lstat(path)
    except FileNotFoundError as exc:
        raise FileSelectionError(f"{name!r} was not found beside the notebook", status=409) from exc
    except OSError as exc:
        raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
    if stat.S_ISLNK(before.st_mode) or _is_windows_reparse_point(before):
        raise FileSelectionError(f"{name!r} is a symlink, not a file beside the notebook", status=409)
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        if exc.errno in _SYMLINK_OPEN_ERRORS:
            raise FileSelectionError(f"{name!r} is a symlink, not a file beside the notebook", status=409) from exc
        if exc.errno == errno.ENOENT:
            raise FileSelectionError(f"{name!r} was not found beside the notebook", status=409) from exc
        raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
    try:
        opened = os.fstat(descriptor)
        if not stat.S_ISREG(opened.st_mode) or (before.st_dev, before.st_ino) != (opened.st_dev, opened.st_ino):
            raise FileSelectionError(f"{name!r} was replaced while it was being read", status=409)
        with os.fdopen(os.dup(descriptor), "rb") as handle:
            return handle.read(MAX_FILE_BYTES + 1)
    except FileSelectionError:
        raise
    except OSError as exc:
        raise FileSelectionError(f"could not read {name!r}: {exc}", status=502) from exc
    finally:
        os.close(descriptor)


def _selection_name(raw: Any) -> Optional[str]:
    name = str(raw).strip()
    if not name:
        return None
    if name != Path(name).name or name in (".", "..") or "\\" in name:
        raise FileSelectionError(f"{name!r} is not a flat file name in the notebook directory")
    if _skip(Path(name)):
        raise FileSelectionError(f"{name!r} is not eligible to be embedded")
    return name


def _read_names(directory: _OpenedDirectory, names: Iterable[Any]) -> Dict[str, bytes]:
    selected: Dict[str, bytes] = {}
    total = 0
    for raw in names:
        name = _selection_name(raw)
        if name is None:
            continue
        if _DIRECTORY_BINDING:
            data = _read_bound_file(directory.descriptor, name)
        else:
            _verify_authorized_directory(directory)
            data = _read_verified_file(directory.path, name)
        if len(data) > MAX_FILE_BYTES:
            raise FileSelectionError(f"{name!r} is larger than the {MAX_FILE_BYTES}-byte per-file limit", status=413)
        total += len(data)
        if total > MAX_DECODED_BYTES:
            raise FileSelectionError(
                f"the selected files total {total} bytes, above the {MAX_DECODED_BYTES}-byte payload ceiling; "
                "embed fewer files or bake them into the runtime image",
                status=413,
            )
        selected[name] = data
    return selected


def list_candidates(directory: Path, notebook_path: str) -> Dict[str, Any]:
    """Flat files beside the notebook that could ship with it.

    ``directory`` is the already-resolved notebook directory the caller
    authorized; it is consumed as-is. Resolving ``notebook_path`` again here would
    be the second ``Path.resolve()`` on a caller-supplied path, and a symlink
    repoint or a parent rename landing between the two resolutions would make the
    directory *listed* differ from the directory *authorized*.
    :func:`notebook_directory` stays the single resolver, and every caller
    resolves once and passes the value down - the same shape
    ``_SubmitMixin.selected_files`` uses for the read path. ``notebook_path`` is
    used only to exclude the notebook itself from the listing.
    """
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
        # ``is_file`` follows a link, so a symlink offered here would be a file
        # whose bytes the server identity reads out of whatever it points at -
        # including, for the pod's service account, the projected token. Links
        # are omitted entirely: the picker's contract is flat files in the
        # notebook's own directory, and a link's target can be repointed between
        # this listing and the read the review performs.
        if entry.is_symlink():
            continue
        if not _inside(entry.resolve(), directory) or not entry.is_file() or _skip(entry) or entry.name == notebook_name:
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


def read_selected(
    directory: Path,
    names: Iterable[str],
    *,
    before_read: Optional[Callable[[], None]] = None,
) -> Dict[str, bytes]:
    """Read the chosen files, refusing anything outside the flat, bounded set.

    The directory is opened once and verified before any file is touched, then
    every file is opened relative to that descriptor without following links. A
    directory renamed and replaced by a symlink after authorization is refused
    rather than re-resolved. Platforms without relative opens refuse the swapped
    name, the link, and a replaced file, but cannot bind the read to the opened
    directory; they re-verify the directory and the opened file identity instead.

    ``before_read`` is a test-only hook that runs after the directory is verified
    and before the first file is opened.
    """
    directory_handle = _open_authorized_directory(Path(directory))
    try:
        if before_read is not None:
            before_read()
        return _read_names(directory_handle, names)
    finally:
        if directory_handle.descriptor is not None:
            os.close(directory_handle.descriptor)


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
