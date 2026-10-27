# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Regression tests for two confirmed security defects on the notebook path.

* DEFECT 1 (``env_secret`` reaches any Secret in the namespace). The renderer
  accepted any caller-chosen ``SECRET:KEY`` string, validated only by a shape
  regex, and emitted a ``secretKeyRef`` into the head and worker containers. The
  notebook executes on the head, so a caller who could ``execute`` a notebook in
  namespace X could name another team's Secret there and read it back from the
  run logs. Kubernetes resolves a ``secretKeyRef`` with the node/kubelet
  identity, not the pod's ServiceAccount RBAC, so the server's namespace check
  never bounded which Secret a notebook could mount.

  The fix confines Secret *names* to an operator-configured allow-list
  (``TAUGRID_ALLOWED_SECRETS``) and fails closed when it is unset or empty. The
  *key* inside an allowed Secret stays caller-chosen: the Secret is the
  privilege boundary, because referencing any key makes the kubelet resolve the
  whole Secret object.

* DEFECT 2 (the listing path resolves the request path twice). ``FilesHandler``
  resolved the request path once to authorize the directory and
  ``list_candidates`` resolved it again to list it. Two independent
  ``Path.resolve()`` calls on a caller-supplied path mean a symlink repoint or a
  parent rename between them makes the directory *authorized* differ from the
  directory *listed*. The fix resolves once and passes the resolved directory
  down, matching the POST-path ``authorize_companion_files`` pattern, and
  ``notebook_directory`` stays the single resolver.

Every refusal test here fails on the unfixed tree; the two negative controls
pass on both to prove the feature still works.
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from tau._render import GPU_EXECUTION_RAY, Profile, UnsupportedShape, render_rayjob
from tau.jupyter import notebook_files, server
from tau.jupyter.notebook_files import list_candidates
from tau.jupyter.submit import SubmitError, build_plan
from tests.test_jupyter_submit import FakeClient, FakeCustom, VALID_NB, cluster_doc

#: The operator-facing contract under test. Spelled out here rather than imported
#: from the implementation: this string is the interface the operator sets, so a
#: rename of the constant must not silently pass the test.
ALLOWED_SECRETS_ENV = "TAUGRID_ALLOWED_SECRETS"


# -- DEFECT 1: caller-chosen Secret names ------------------------------------


def _render(**overrides):
    kwargs = dict(
        name="nb-demo",
        namespace="ray",
        queue="research-gpu",
        profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
        entrypoint="python3 /script/_tau_runner.py --notebook analysis.ipynb",
    )
    kwargs.update(overrides)
    return render_rayjob(**kwargs)


def _container_env(job, role: str):
    spec = job["spec"]["rayClusterSpec"]
    if role == "head":
        pod = spec["headGroupSpec"]["template"]["spec"]
    else:
        pod = spec["workerGroupSpecs"][0]["template"]["spec"]
    return pod["containers"][0].get("env", [])


def _secret_refs(env):
    return [item["valueFrom"]["secretKeyRef"] for item in env if "valueFrom" in item]


def test_render_refuses_a_secret_when_the_operator_allow_list_is_unset(monkeypatch):
    """Fail closed: with no ``TAUGRID_ALLOWED_SECRETS``, no Secret ref may render."""
    monkeypatch.delenv(ALLOWED_SECRETS_ENV, raising=False)

    with pytest.raises(UnsupportedShape) as err:
        _render(env_secret={"X": "not-allowed:password"})

    message = str(err.value)
    assert ALLOWED_SECRETS_ENV in message
    assert "not-allowed" in message
    # The operator action is named, and the caller is told the allow-list is empty
    # rather than being handed a run that quietly dropped the requested Secret.
    assert "unset or empty" in message


def test_render_refuses_a_secret_name_the_operator_did_not_list(monkeypatch):
    """A configured allow-list still refuses a name that is not on it."""
    monkeypatch.setenv(ALLOWED_SECRETS_ENV, "allowed-secret")

    with pytest.raises(UnsupportedShape) as err:
        _render(env_secret={"X": "not-allowed:password"})

    message = str(err.value)
    assert ALLOWED_SECRETS_ENV in message
    assert "not-allowed" in message
    # The refusal shows the operator what *is* permitted, so the fix is findable.
    assert "allowed-secret" in message


def test_render_allows_a_secret_name_on_the_operator_list(monkeypatch):
    """Negative control: an operator-listed Secret still renders on both pods.

    The key is caller-chosen within the allowed Secret, which is the documented
    least-privilege shape: the operator names the Secret, the caller picks the
    key.
    """
    monkeypatch.setenv(ALLOWED_SECRETS_ENV, " allowed-secret , other ")

    job = _render(env_secret={"X": "other:password"})

    expected = [{"name": "other", "key": "password"}]
    assert _secret_refs(_container_env(job, "head")) == expected
    assert _secret_refs(_container_env(job, "worker")) == expected


def test_build_plan_refuses_a_disallowed_secret_with_a_user_actionable_400(monkeypatch):
    """The submit path surfaces the renderer refusal as a 400 naming the fix."""
    monkeypatch.setenv(ALLOWED_SECRETS_ENV, "allowed-secret")
    client = FakeClient(FakeCustom(cluster=cluster_doc()))

    with pytest.raises(SubmitError) as err:
        build_plan(
            client=client,
            notebook_bytes=VALID_NB,
            namespace="ray",
            name="demo",
            gpu_execution="ray",
            env_secret={"X": "not-allowed:password"},
        )

    assert err.value.status == 400
    assert ALLOWED_SECRETS_ENV in str(err.value)
    assert "not-allowed" in str(err.value)


class _AllowAllAuthorizer:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str]] = []

    def is_authorized(self, handler, user, action, resource) -> bool:
        self.calls.append((action, resource))
        return True


class _PreviewStub(server._SubmitMixin):
    """Minimal handler surface ``PreviewHandler.post`` consumes."""

    current_user = "researcher"

    def __init__(self, body, root: Path) -> None:
        self.settings = {"server_root_dir": str(root)}
        self.request = SimpleNamespace(method="POST", body=json.dumps(body).encode())
        self._client = FakeClient(FakeCustom(cluster=cluster_doc()))
        self.authorizer = _AllowAllAuthorizer()
        self.result = None

    def client(self):
        return self._client

    def finish(self, value) -> None:
        self.result = value


def test_preview_endpoint_refuses_a_disallowed_secret_with_a_400(monkeypatch, tmp_path):
    """The real HTTP entry path refuses, rather than rendering the Secret."""
    monkeypatch.setenv(ALLOWED_SECRETS_ENV, "allowed-secret")
    handler = _PreviewStub(
        {
            "notebook": VALID_NB.decode(),
            "namespace": "ray",
            "name": "reviewed",
            "gpuExecution": "ray",
            "env_secret": {"X": "not-allowed:password"},
        },
        tmp_path,
    )

    with pytest.raises(server.web.HTTPError) as failure:
        asyncio.run(server.PreviewHandler.post(handler))

    assert failure.value.status_code == 400
    assert ALLOWED_SECRETS_ENV in str(failure.value)
    assert handler.result is None


# -- DEFECT 2: the listing path resolved the request path twice --------------


class _FilesHandlerStub(server._ClientMixin):
    """Minimal handler surface ``FilesHandler.get`` consumes."""

    def __init__(self, root: Path, path: str) -> None:
        self.settings = {"server_root_dir": str(root)}
        self.arguments = {"path": path}
        self.current_user = object()
        self.authorizer = _AllowAllAuthorizer()
        self.response = None

    def get_argument(self, name: str, default=None):
        return self.arguments.get(name, default)

    def finish(self, value) -> None:
        self.response = value


def test_files_handler_lists_the_directory_it_authorized(tmp_path, monkeypatch):
    """The directory listed is the directory authorized, resolved once.

    ``notebook_directory`` is stubbed to yield directory A on the first call and
    directory B on any later call - the shape of a symlink repoint or a parent
    rename landing between two independent resolutions. On the unfixed tree the
    handler authorizes A (the first call) and ``list_candidates`` then resolves
    again and lists B; the fixed tree resolves once and lists A.
    """
    root = tmp_path / "server_root"
    dir_a = root / "a"
    dir_b = root / "b"
    for directory in (dir_a, dir_b):
        directory.mkdir(parents=True)
        (directory / "analysis.ipynb").write_text("{}", encoding="utf-8")
    (dir_a / "from-a.py").write_bytes(b"a\n")
    (dir_b / "from-b.py").write_bytes(b"b\n")

    resolutions: list[str] = []

    def diverging(_root, notebook_path):
        resolutions.append(notebook_path)
        return dir_a if len(resolutions) == 1 else dir_b

    # Two independent resolutions live in two modules: the handler's import and
    # the one ``list_candidates`` performed. Patching both is exactly the
    # two-call shape the defect has.
    monkeypatch.setattr(server, "notebook_directory", diverging)
    monkeypatch.setattr(notebook_files, "notebook_directory", diverging)

    handler = _FilesHandlerStub(root, "a/analysis.ipynb")
    asyncio.run(server.FilesHandler.get(handler))

    assert ("read", "taugrid:files/a") in handler.authorizer.calls
    names = [row["name"] for row in handler.response["files"]]
    assert names == ["from-a.py"]
    assert resolutions == ["a/analysis.ipynb"]


def test_list_candidates_does_not_re_resolve_the_directory(tmp_path, monkeypatch):
    """``list_candidates`` consumes the resolved directory; it never resolves."""

    def forbidden(*_args, **_kwargs):
        raise AssertionError("list_candidates must not re-resolve the request path")

    monkeypatch.setattr(notebook_files, "notebook_directory", forbidden)

    directory = tmp_path / "nb"
    directory.mkdir()
    (directory / "helpers.py").write_bytes(b"VALUE = 41\n")

    result = list_candidates(directory, "analysis.ipynb")

    assert [row["name"] for row in result["files"]] == ["helpers.py"]


def _pod(job, role: str):
    spec = job["spec"]["rayClusterSpec"]
    if role == "head":
        return spec["headGroupSpec"]["template"]["spec"]
    return spec["workerGroupSpecs"][0]["template"]["spec"]


def _tolerated_taints(job, role: str):
    return {
        (t["key"], t["effect"])
        for t in _pod(job, role).get("tolerations") or []
    }


def test_gpu_worker_tolerates_the_gpu_node_taints():
    """A GPU worker must tolerate the taints the CLI Job renderer tolerates.

    TauGrid's GPU ResourceFlavors declare ``sku=gpu:NoSchedule``; a worker pod
    without the toleration is unschedulable on the default GPU pool.
    """
    job = _render(
        profile=Profile(name="gpu", workers=1, gpus_per_worker=1),
        gpu_execution=GPU_EXECUTION_RAY,
    )

    assert {
        ("sku", "NoSchedule"),
        ("nvidia.com/gpu", "NoSchedule"),
    } <= _tolerated_taints(job, "worker")


def test_cpu_worker_does_not_tolerate_gpu_taints():
    """Negative control: a CPU profile must not claim GPU-node tolerance."""
    job = _render(profile=Profile(name="cpu", workers=1, gpus_per_worker=0))

    assert _tolerated_taints(job, "worker") == set()
    assert _tolerated_taints(job, "head") == set()


def test_gpu_tolerations_cover_the_head_the_pool_places_there():
    """The head requests no GPU, but the profile's selector still places it on the GPU pool.

    Tolerations and the selector have to agree: pinning the head to a tainted
    node without the matching toleration leaves the control pod unschedulable,
    so the RayJob never starts and no worker is ever created.
    """
    job = _render(
        profile=Profile(name="gpu", workers=1, gpus_per_worker=2),
        gpu_execution=GPU_EXECUTION_RAY,
    )

    assert _tolerated_taints(job, "worker") == _tolerated_taints(job, "head")
    assert _tolerated_taints(job, "head")

