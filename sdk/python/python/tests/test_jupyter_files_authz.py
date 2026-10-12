# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Regression tests for two adversarial-review findings.

* DEFECT A: the notebook file-packaging path validated only the *directory* and
  then followed a symlink entry, so a link in the notebook directory could read
  any file the server identity can see (the pod's projected service-account
  token is the highest-value target) and embed it in the payload.
* DEFECT B: ``FilesHandler`` authorized the constant ``taugrid`` while reading a
  fully request-controlled ``path``, so per-directory policy could not be
  expressed and every directory under the server root shared one grant.

Each test here fails on the unfixed tree: the symlink test leaks bytes, and the
resource test never observes a path-derived resource.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace

import pytest

from tau.jupyter import notebook_files, server
from tau.jupyter.notebook_files import (
    FileSelectionError,
    list_candidates,
    notebook_directory,
    read_selected,
)

NOTEBOOK = "analysis.ipynb"


def _notebook_dir(root: Path, *parts: str) -> Path:
    directory = root.joinpath(*parts)
    directory.mkdir(parents=True, exist_ok=True)
    (directory / NOTEBOOK).write_text("{}", encoding="utf-8")
    return directory


def test_read_selected_refuses_a_symlink_out_of_the_notebook_directory(tmp_path):
    """DEFECT A: a symlink entry must not be read as a file beside the notebook."""
    root = tmp_path / "server_root"
    nb_dir = _notebook_dir(root, "notebooks")
    secret = tmp_path / "outside_secret.txt"
    secret.write_bytes(b"SA-TOKEN-abcdef")
    (nb_dir / "loot").symlink_to(secret)

    rel = "notebooks/" + NOTEBOOK
    offered = [row["name"] for row in list_candidates(root, rel)["files"]]
    assert "loot" not in offered

    with pytest.raises(FileSelectionError):
        read_selected(notebook_directory(root, rel), ["loot"])


def test_read_selected_refuses_a_symlink_that_points_inside_the_directory(tmp_path):
    """The documented choice: no link is a flat file, an inside link included."""
    root = tmp_path / "server_root"
    nb_dir = _notebook_dir(root, "notebooks")
    (nb_dir / "helpers.py").write_bytes(b"VALUE = 41\n")
    (nb_dir / "alias.py").symlink_to(nb_dir / "helpers.py")

    rel = "notebooks/" + NOTEBOOK
    assert "alias.py" not in [row["name"] for row in list_candidates(root, rel)["files"]]
    with pytest.raises(FileSelectionError):
        read_selected(notebook_directory(root, rel), ["alias.py"])
    # The real file is still fine.
    assert read_selected(notebook_directory(root, rel), ["helpers.py"]) == {"helpers.py": b"VALUE = 41\n"}


def test_containment_is_a_parent_test_not_a_string_prefix(tmp_path):
    """A sibling that merely shares a name prefix is outside the directory."""
    ev = tmp_path / "ev"
    evil = tmp_path / "evil"
    ev.mkdir()
    evil.mkdir()
    (evil / "token").write_bytes(b"SA-TOKEN-abcdef")

    assert notebook_files._inside(ev / "x.py", ev) is True
    assert notebook_files._inside(ev, ev) is True
    assert notebook_files._inside(evil / "token", ev) is False


class _RecordingAuthorizer:
    def __init__(self, allow: bool = True) -> None:
        self.calls: list[tuple[str, str]] = []
        self.allow = allow

    def is_authorized(self, handler, user, action, resource) -> bool:
        self.calls.append((action, resource))
        return self.allow


class _FakeFilesHandler(server._ClientMixin):
    """Minimal handler surface ``FilesHandler.get`` consumes."""

    def __init__(self, root: Path, *, path: str | None = None, current_user=object(), authorizer=None) -> None:
        self.settings = {"server_root_dir": str(root)}
        self.arguments = {} if path is None else {"path": path}
        self.current_user = current_user
        self.authorizer = authorizer or _RecordingAuthorizer()
        self.request = SimpleNamespace(method="GET")
        self.response = None

    def get_argument(self, name: str, default=None):
        return self.arguments.get(name, default)

    def finish(self, value) -> None:
        self.response = value


def test_files_handler_authorizes_a_path_derived_resource(tmp_path):
    """DEFECT B: the resource names the directory, so it differs per directory."""
    root = tmp_path / "server_root"
    _notebook_dir(root, "sub")
    _notebook_dir(root, "other")
    (root / "sub" / "helpers.py").write_bytes(b"VALUE = 41\n")

    handler = _FakeFilesHandler(root, path="sub/" + NOTEBOOK)
    asyncio.run(server.FilesHandler.get(handler))
    assert ("read", "taugrid:files:sub") in handler.authorizer.calls

    handler2 = _FakeFilesHandler(root, path="other/" + NOTEBOOK)
    asyncio.run(server.FilesHandler.get(handler2))
    assert ("read", "taugrid:files:other") in handler2.authorizer.calls

    # A per-directory policy can therefore deny one directory without the other.
    denying = _RecordingAuthorizer(allow=False)
    handler3 = _FakeFilesHandler(root, path="sub/" + NOTEBOOK, authorizer=denying)
    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(server.FilesHandler.get(handler3))
    assert failure.value.status_code == 403
    assert handler3.response is None


def test_files_handler_still_refuses_a_path_outside_the_root(tmp_path):
    """Negative control: an outside path must fail and must not be listed."""
    root = tmp_path / "server_root"
    _notebook_dir(root, "notebooks")

    handler = _FakeFilesHandler(root, path="../" + NOTEBOOK)
    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(server.FilesHandler.get(handler))
    assert failure.value.status_code == 400
    assert handler.response is None
    assert not [call for call in handler.authorizer.calls if call[1].startswith("taugrid:files")]
