# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the notebook widget core (backend + renderer + profile)."""

import json
import tempfile
from pathlib import Path

import pytest

from tau._backend import KubernetesBackend
from tau._notebook_pkg import PayloadTooLarge, package
from tau._profile import NoTauClusterFound, resolve
from tau._render import Profile, UnsupportedShape, refuse, render_rayjob

TAU_KEY = "kueue.x-k8s.io/queue-name"

VALID_NB = json.dumps({
    "nbformat": 4,
    "nbformat_minor": 5,
    "metadata": {"kernelspec": {"language": "python", "name": "python3"}, "language_info": {"name": "python"}},
    "cells": [
        {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [], "execution_count": None, "source": ["print(1)"]},
    ],
}).encode()


class FakeCustomApi:
    """Records create_namespaced_custom_object calls for the backend test."""

    def __init__(self) -> None:
        self.created: list[dict] = []

    def create_namespaced_custom_object(self, **kwargs):
        self.created.append(kwargs)
        return {"metadata": {"name": kwargs.get("body", {}).get("metadata", {}).get("name")}}


def test_import_tau_does_not_pull_kubernetes():
    import sys

    for mod in list(sys.modules):
        if mod.startswith("kubernetes"):
            del sys.modules[mod]
    assert "kubernetes" not in sys.modules


def test_render_rayjob_kueue_contract():
    job = render_rayjob(
        name="demo",
        namespace="ray",
        queue="research-gpu",
        profile=Profile(name="p1", workers=2, gpus_per_worker=8, num_cpus_per_worker=16),
        entrypoint="python _tau_runner.py",
    )
    assert job["apiVersion"] == "ray.io/v1"
    assert job["kind"] == "RayJob"
    assert job["spec"]["suspend"] is True
    assert "managedBy" not in job["spec"]
    assert job["metadata"]["labels"][TAU_KEY] == "research-gpu"
    head = job["spec"]["rayClusterSpec"]["headGroupSpec"]
    assert head["rayStartParams"]["num-gpus"] == "0"
    assert head["rayStartParams"]["num-cpus"] == "0"
    workers = job["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]
    assert workers["replicas"] == 2
    assert workers["rayStartParams"]["num-gpus"] == "8"
    assert workers["template"]["spec"]["containers"][0]["resources"]["requests"]["nvidia.com/gpu"] == "8"


def test_render_rayjob_no_gpu_when_profile_zero():
    job = render_rayjob(
        name="cpu", namespace="ns", queue="q", profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
        entrypoint="cmd",
    )
    wg = job["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]
    req = wg["template"]["spec"]["containers"][0]["resources"]["requests"]
    assert "nvidia.com/gpu" not in req
    assert wg["rayStartParams"]["num-gpus"] == "0"


def test_refuse_unsupported_shapes():
    with pytest.raises(UnsupportedShape):
        refuse(archives=True)
    with pytest.raises(UnsupportedShape):
        refuse(chained=True)
    with pytest.raises(UnsupportedShape):
        refuse(offload=True)
    refuse()  # non-refusal is a no-op


def test_kubernetes_backend_submits_rayjob():
    fake = FakeCustomApi()
    backend = KubernetesBackend(custom_objects=fake)
    manifest = render_rayjob(
        name="x", namespace="ns", queue="q", profile=Profile(name="p", workers=1),
        entrypoint="cmd",
    )
    backend.submit_rayjob(manifest, namespace="ns")
    assert len(fake.created) == 1
    assert fake.created[0]["body"]["kind"] == "RayJob"
    assert fake.created[0]["plural"] == "rayjobs"


def test_profile_resolution():
    cluster = {
        "spec": {
            "workspaceDefaults": {"defaultQueue": "research-gpu"},
            "workloadProfiles": [
                {"name": "training-8gpu", "status": {"state": "ready"},
                 "compute": {"workerReplicas": 4, "gpusPerWorker": 8}},
                {"name": "training-1gpu", "status": {"state": "ready"},
                 "compute": {"workerReplicas": 1, "gpusPerWorker": 1}},
                {"name": "broken", "status": {"state": "notready"},
                 "compute": {"workerReplicas": 2, "gpusPerWorker": 2}},
            ],
        }
    }
    profiles, queue = resolve(cluster)
    assert queue == "research-gpu"
    names = {p.name for p in profiles}
    assert names == {"training-8gpu", "training-1gpu"}
    eight = next(p for p in profiles if p.name == "training-8gpu")
    assert eight.workers == 4 and eight.gpus_per_worker == 8


def test_resolve_no_cluster_raises():
    with pytest.raises(NoTauClusterFound):
        resolve({})


def test_package_is_self_contained_and_capped():
    with tempfile.TemporaryDirectory() as d:
        payload = package(VALID_NB, staging_dir=Path(d))
        assert payload.notebook_path.exists()
        assert payload.runner_path.exists()
        assert "analysis.ipynb" in payload.notebook_path.name
        with pytest.raises(PayloadTooLarge):
            package(VALID_NB, staging_dir=Path(d), input_cap=10)


def test_runner_forwards_iopub_without_starting_kernel(tmp_path, monkeypatch, capsys):
    import runpy
    import sys
    from types import SimpleNamespace

    messages = [
        {"msg_type": "stream", "content": {"name": "stdout", "text": "step=1 loss=0.5\n"}},
        {"msg_type": "stream", "content": {"name": "stderr", "text": "diagnostic\n"}},
        {"msg_type": "display_data", "content": {"data": {"text/plain": "not a stream"}}},
    ]
    handled = []
    written = []

    class FakeExecutor:
        def __init__(self, timeout):
            assert timeout == 600

        def process_message(self, message, cell, index):
            handled.append(message)
            return "preserved"

        def preprocess(self, notebook, resources):
            for message in messages:
                assert self.process_message(message, {}, 0) == "preserved"

    monkeypatch.setitem(sys.modules, "nbconvert", SimpleNamespace(preprocessors=SimpleNamespace(ExecutePreprocessor=FakeExecutor)))
    monkeypatch.setitem(sys.modules, "nbformat", SimpleNamespace(
        read=lambda *args, **kwargs: {}, write=lambda *args, **kwargs: written.append(args)))
    monkeypatch.setattr(sys, "argv", ["_tau_runner.py"])
    payload = package(VALID_NB, staging_dir=tmp_path)
    runner = runpy.run_path(str(payload.runner_path))
    assert runner["main"]() == 0
    output = capsys.readouterr()
    assert "step=1 loss=0.5\n" in output.out
    assert output.err == "diagnostic\n"
    assert "not a stream" not in output.out
    assert handled == messages and len(written) == 1


def test_package_does_not_modify_source():
    import tempfile as _t

    with _t.TemporaryDirectory() as d:
        noted = Path(d) / "src.ipynb"
        original = VALID_NB
        noted.write_bytes(original)
        before = noted.read_bytes()
        staging = Path(d) / "stage"
        package(noted.read_bytes(), staging_dir=staging)
        assert noted.read_bytes() == before


@pytest.mark.parametrize("kwargs", [
    {"workers": 0}, {"workers": True}, {"workers": 1.5},
    {"gpus_per_worker": -1}, {"gpus_per_worker": True},
    {"num_cpus_per_worker": float("nan")}, {"num_cpus_per_worker": 0},
    {"num_cpus_per_worker": True}, {"node_selector": {"pool": 7}},
])
def test_explicit_profile_rejects_invalid_resources(kwargs):
    with pytest.raises(ValueError):
        Profile(name="invalid", **kwargs)
