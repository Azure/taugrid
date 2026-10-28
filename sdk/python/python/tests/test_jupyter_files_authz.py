# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Regression tests for adversarial-review findings on the file-packaging path.

* DEFECT A: the notebook file-packaging path validated only the *directory* and
  then followed a symlink entry, so a link in the notebook directory could read
  any file the server identity can see (the pod's projected service-account
  token is the highest-value target) and embed it in the payload.
* DEFECT B: ``FilesHandler`` authorized the constant ``taugrid`` while reading a
  fully request-controlled ``path``, so per-directory policy could not be
  expressed and every directory under the server root shared one grant.
* DEFECT C (reviewer comment 4247133422, ``server.py:365``): the two POST paths
  resolved and read the same companion files as ``FilesHandler`` while
  authorizing only the workload namespace. A caller allowed to preview/submit
  into a namespace but denied a directory could name that directory's files
  directly, have them packaged into the run payload, and read them back from the
  run output.

Each test here fails on the unfixed tree: the symlink test leaks bytes, the
resource test never observes a path-derived resource, and the POST-path tests
observe a companion file read under a namespace-only grant.
"""

from __future__ import annotations

import asyncio
import json
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
from tests.test_jupyter_submit import FakeClient, FakeCustom, VALID_NB, cluster_doc

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
    offered = [row["name"] for row in list_candidates(notebook_directory(root, rel), rel)["files"]]
    assert "loot" not in offered

    with pytest.raises(FileSelectionError):
        read_selected(root, notebook_directory(root, rel), ["loot"])


def test_read_selected_refuses_a_symlink_that_points_inside_the_directory(tmp_path):
    """The documented choice: no link is a flat file, an inside link included."""
    root = tmp_path / "server_root"
    nb_dir = _notebook_dir(root, "notebooks")
    (nb_dir / "helpers.py").write_bytes(b"VALUE = 41\n")
    (nb_dir / "alias.py").symlink_to(nb_dir / "helpers.py")

    rel = "notebooks/" + NOTEBOOK
    assert "alias.py" not in [row["name"] for row in list_candidates(notebook_directory(root, rel), rel)["files"]]
    with pytest.raises(FileSelectionError):
        read_selected(root, notebook_directory(root, rel), ["alias.py"])
    # The real file is still fine.
    assert read_selected(root, notebook_directory(root, rel), ["helpers.py"]) == {"helpers.py": b"VALUE = 41\n"}


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
    assert ("read", "taugrid:files/sub") in handler.authorizer.calls

    handler2 = _FakeFilesHandler(root, path="other/" + NOTEBOOK)
    asyncio.run(server.FilesHandler.get(handler2))
    assert ("read", "taugrid:files/other") in handler2.authorizer.calls

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
    assert not [call for call in handler.authorizer.calls if call[1].startswith("taugrid:files/")]


# -- DEFECT C: the POST paths must authorize the directory they read ---------
#
# Reviewer comment 4247133422 (server.py:365). ``FilesHandler`` authorizes the
# companion directory as ``taugrid:files:<dir>``; the preview and submit POST
# paths resolved and read the SAME files while authorizing only the workload
# namespace. The tests below allow the namespace and deny the directory, which is
# exactly the policy the reviewer asked for, and assert no read happens.

FILES_RESOURCE = "taugrid:files/notebooks"


class _NamespaceOnlyAuthorizer:
    """Allows the named resources; denies every companion directory by default.

    ``allowed=("taugrid:ray",)`` models a caller who keeps namespace operations
    but is denied ``taugrid:files:notebooks``.
    """

    def __init__(self, allowed=("taugrid:ray",)) -> None:
        self.allowed = set(allowed)
        self.calls: list[tuple[str, str]] = []

    def is_authorized(self, handler, user, action, resource) -> bool:
        self.calls.append((action, resource))
        return resource in self.allowed


class _SubmitHandler(server._SubmitMixin):
    """Minimal handler surface ``PreviewHandler``/``SubmitHandler`` consume."""

    current_user = "researcher"

    def __init__(self, body, root, authorizer=None) -> None:
        self.settings = {"server_root_dir": str(root)}
        self.request = SimpleNamespace(method="POST", body=json.dumps(body).encode())
        self._client = FakeClient(FakeCustom(cluster=cluster_doc()))
        self.authorizer = authorizer or _NamespaceOnlyAuthorizer()
        self.result = None

    def client(self):
        return self._client

    def finish(self, value) -> None:
        self.result = value


def _submit_body(*, files=("helpers.py",), **overrides):
    body = {
        "notebook": VALID_NB.decode(),
        "namespace": "ray",
        "name": "reviewed",
        "gpuExecution": "ray",
        "path": "notebooks/" + NOTEBOOK,
        "files": list(files),
    }
    body.update(overrides)
    return body


def _server_root_with_companion(tmp_path: Path) -> Path:
    root = tmp_path / "server_root"
    _notebook_dir(root, "notebooks")
    (root / "notebooks" / "helpers.py").write_bytes(b"VALUE = 41\n")
    return root


@pytest.fixture
def read_spy(monkeypatch):
    """Record every reader call so a refused request can prove it never read."""
    calls = []
    real = server.read_selected

    def recording(root, directory, names):
        calls.append((str(directory), list(names)))
        return real(root, directory, names)

    monkeypatch.setattr(server, "read_selected", recording)
    return calls


@pytest.mark.parametrize("endpoint", ["preview", "submit"])
def test_post_paths_refuse_a_denied_companion_directory_without_reading(
    tmp_path, read_spy, monkeypatch, endpoint
):
    """DEFECT C: allow the workload namespace, deny the directory -> no read.

    The reviewer's negative test: the caller keeps namespace operations
    (``taugrid:ray``) and is denied only ``taugrid:files:notebooks``. The request
    names a real companion file, so on the unfixed tree the file is read and
    packaged; the fixed tree must refuse before reading.
    """
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    root = _server_root_with_companion(tmp_path)
    handler = _SubmitHandler(_submit_body(confirm=True, planDigest="0" * 64), root)
    handler.authorizer = _NamespaceOnlyAuthorizer()

    call = server.PreviewHandler.post if endpoint == "preview" else server.SubmitHandler.post
    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(call(handler))

    assert failure.value.status_code == 403
    expected_action = "read" if endpoint == "preview" else "execute"
    assert (expected_action, "taugrid:ray") in handler.authorizer.calls
    # The directory is part of the decision, using the same resource vocabulary
    # ``FilesHandler`` already publishes.
    assert ("read", FILES_RESOURCE) in handler.authorizer.calls
    # The decisive assertion: a refused request must not read any file.
    assert read_spy == []
    assert handler.result is None


@pytest.mark.parametrize("endpoint", ["preview", "submit"])
def test_post_paths_allow_a_caller_granted_both_namespace_and_directory(
    tmp_path, read_spy, monkeypatch, endpoint
):
    """Negative control: granting both does not break the feature."""
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    root = _server_root_with_companion(tmp_path)
    granted = _NamespaceOnlyAuthorizer(allowed={"taugrid:ray", FILES_RESOURCE})
    size = len(b"VALUE = 41\n")

    review = _SubmitHandler(_submit_body(), root, authorizer=granted)
    asyncio.run(server.PreviewHandler.post(review))
    assert review.result["plan"]["fileSizes"] == {"helpers.py": size}
    assert read_spy  # the companion file really was read and packaged

    if endpoint == "preview":
        return

    digest = review.result["plan"]["planDigest"]
    attempt = _SubmitHandler(
        _submit_body(confirm=True, planDigest=digest), root, authorizer=granted
    )
    asyncio.run(server.SubmitHandler.post(attempt))
    assert attempt.result["submitted"] is True


def test_post_paths_authorize_the_same_resource_files_handler_does(tmp_path, monkeypatch):
    """One vocabulary: the POST paths and ``FilesHandler`` name the same directory."""
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    root = _server_root_with_companion(tmp_path)

    listing = _FakeFilesHandler(root, path="notebooks/" + NOTEBOOK)
    asyncio.run(server.FilesHandler.get(listing))

    submit = _SubmitHandler(_submit_body(confirm=True, planDigest="0" * 64), root)
    submit.authorizer = _NamespaceOnlyAuthorizer(allowed={"taugrid:ray", FILES_RESOURCE})
    asyncio.run(server.PreviewHandler.post(submit))

    assert ("read", FILES_RESOURCE) in listing.authorizer.calls
    assert ("read", FILES_RESOURCE) in submit.authorizer.calls
