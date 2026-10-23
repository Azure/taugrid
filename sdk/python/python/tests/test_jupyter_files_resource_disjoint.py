# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Companion-file resources must be disjoint from Kubernetes namespace resources.

``files_resource(root, root)`` returned ``"files"`` and ``authorize`` passed it
through ``auth_resource``, producing ``taugrid:files``. A Kubernetes namespace
literally named ``files`` produced the byte-identical resource, so a
resource-keyed Authorizer granting read on that namespace also granted root
companion-file listing and packaging with no filesystem grant.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from types import SimpleNamespace

import pytest

from tau.jupyter import server

NOTEBOOK = "analysis.ipynb"


def _root(tmp_path: Path) -> Path:
    root = tmp_path / "server_root"
    root.mkdir()
    (root / NOTEBOOK).write_text("{}", encoding="utf-8")
    for name in ("sub", "other", "files"):
        (root / name).mkdir()
        (root / name / NOTEBOOK).write_text("{}", encoding="utf-8")
    return root


class _ResourceAuthorizer:
    """Allows exactly the listed resources, like a resource-keyed Authorizer."""

    def __init__(self, allowed=()):
        self.allowed = set(allowed)
        self.calls: list[tuple[str, str]] = []

    def is_authorized(self, handler, user, action, resource) -> bool:
        self.calls.append((action, resource))
        return resource in self.allowed


class _FilesHandler(server._ClientMixin):
    def __init__(self, root: Path, path: str, authorizer) -> None:
        self.settings = {"server_root_dir": str(root)}
        self.arguments = {"path": path}
        self.current_user = "researcher"
        self.authorizer = authorizer
        self.request = SimpleNamespace(method="GET")
        self.response = None

    def get_argument(self, name, default=None):
        return self.arguments.get(name, default)

    def finish(self, value) -> None:
        self.response = value


class _SubmitHandler(server._SubmitMixin):
    current_user = "researcher"

    def __init__(self, root: Path, body, authorizer) -> None:
        import json

        self.settings = {"server_root_dir": str(root)}
        self.request = SimpleNamespace(method="POST", body=json.dumps(body).encode())
        self.authorizer = authorizer
        self.result = None

    def client(self):
        raise AssertionError("authorization must happen before any cluster access")

    def finish(self, value) -> None:
        self.result = value


def _body(*, files=("helpers.py",)) -> dict:
    return {
        "notebook": "{}",
        "namespace": "ray",
        "path": NOTEBOOK,
        "files": list(files),
    }


def test_a_namespace_named_files_does_not_authorize_root_companion_listing(tmp_path):
    root = _root(tmp_path)
    (root / "helpers.py").write_bytes(b"VALUE = 1\n")
    namespace_grant = server._ClientMixin.auth_resource("files")
    listing = _FilesHandler(root, NOTEBOOK, _ResourceAuthorizer({namespace_grant}))

    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(server.FilesHandler.get(listing))

    assert failure.value.status_code == 403
    assert listing.response is None
    assert namespace_grant not in {resource for _, resource in listing.authorizer.calls}


def test_a_namespace_named_files_does_not_authorize_packaging(tmp_path):
    root = _root(tmp_path)
    (root / "helpers.py").write_bytes(b"VALUE = 1\n")
    namespace_grant = server._ClientMixin.auth_resource("files")
    preview = _SubmitHandler(root, _body(), _ResourceAuthorizer({namespace_grant}))

    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(server.PreviewHandler.post(preview))

    assert failure.value.status_code == 403
    assert preview.result is None
    assert namespace_grant not in {resource for _, resource in preview.authorizer.calls}


def test_root_and_namespace_resources_occupy_disjoint_sets(tmp_path):
    root = _root(tmp_path)
    root_resource = server.files_resource(root, root)
    namespace_resources = {
        server._ClientMixin.auth_resource(name)
        for name in ("files", "files:sub", "ray", "taugrid", "notebooks")
    }

    assert server._ClientMixin.auth_resource("files") == "taugrid:files"
    filesystem_resources = {
        server.files_resource(root, directory)
        for directory in (root, root / "sub", root / "other", root / "files")
    }
    assert filesystem_resources.isdisjoint(namespace_resources)
    assert server.files_resource(root, root) == "taugrid:files/"
    assert server.files_resource(root, root / "files") == "taugrid:files/files"
    assert all("/" in resource for resource in filesystem_resources)
    assert all("/" not in resource for resource in namespace_resources)
    assert root_resource not in namespace_resources


def test_root_listing_still_works_with_its_own_grant(tmp_path):
    root = _root(tmp_path)
    (root / "helpers.py").write_bytes(b"VALUE = 1\n")
    grant = server.files_resource(root, root)
    listing = _FilesHandler(root, NOTEBOOK, _ResourceAuthorizer({server._ClientMixin.AUTH_RESOURCE, grant}))

    asyncio.run(server.FilesHandler.get(listing))

    assert ("read", grant) in listing.authorizer.calls
    assert listing.response is not None


def test_denying_one_directory_still_leaves_another_usable(tmp_path):
    root = _root(tmp_path)
    allowed = server.files_resource(root, root / "other")
    authorizer = _ResourceAuthorizer({server._ClientMixin.AUTH_RESOURCE, allowed})

    denied = _FilesHandler(root, "sub/" + NOTEBOOK, authorizer)
    with pytest.raises(server.web.HTTPError):
        asyncio.run(server.FilesHandler.get(denied))

    permitted = _FilesHandler(root, "other/" + NOTEBOOK, authorizer)
    asyncio.run(server.FilesHandler.get(permitted))
    assert permitted.response is not None
