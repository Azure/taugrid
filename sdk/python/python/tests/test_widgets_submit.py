# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the panel's submit integration (notebook -> Kueue RayJob).

The constrained renderer's refusal surface (UnsupportedShape for archives,
chaining, offload, profiler) is covered in test_widgets_core.py; these tests
cover the panel's submit chain: package -> resolve -> render -> apply.
"""

import json
import tempfile
from pathlib import Path

import pytest

from tau._backend import KubernetesBackend
from tau._render import TAU_QUEUE_LABEL, Profile
from tau.widgets.kube import ClusterClient
from tau.widgets.panel import TauGridPanel

NB_BYTES = json.dumps({
    "nbformat": 4,
    "nbformat_minor": 5,
    "metadata": {"kernelspec": {"language": "python", "name": "python3"}, "language_info": {"name": "python"}},
    "cells": [
        {"cell_type": "markdown", "id": "m0", "metadata": {}, "source": ["# demo"]},
        {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [], "execution_count": None, "source": ["print(\"hi\")"]},
    ],
}).encode()


class FakeCustomApi:
    """Records create + cluster-list calls for the submit chain."""

    def __init__(self, cluster=None) -> None:
        self.created: list[dict] = []
        self._cluster = cluster

    def create_namespaced_custom_object(self, **kwargs):
        self.created.append(kwargs)
        return {"metadata": {"name": kwargs["body"]["metadata"]["name"]}}

    def list_cluster_custom_object(self, **kwargs):
        return {"items": [self._cluster] if self._cluster else []}


def _cluster_doc() -> dict:
    return {
        "spec": {
            "workspaceDefaults": {"defaultQueue": "research-gpu"},
            "workloadProfiles": [
                {"name": "training-8gpu", "status": {"state": "ready"},
                 "compute": {"workerReplicas": 2, "gpusPerWorker": 8}},
            ],
        }
    }


def test_submit_full_chain_preserves_kueue_contract():
    fake = FakeCustomApi(cluster=_cluster_doc())
    panel = TauGridPanel(namespace="ray", run_name="demo",
                         client=ClusterClient(custom=fake, core=None))
    with tempfile.TemporaryDirectory() as d:
        handle = panel.submit(notebook_bytes=NB_BYTES, staging_dir=Path(d))
        assert handle.name == "demo" and handle.namespace == "ray"
        assert handle.kind == "RayJob"
        # The panel switched into run view with the staged payload and manifest.
        assert panel.staged is not None and panel.staged.notebook_path.exists()
        assert panel.manifest["spec"] == fake.created[0]["body"]["spec"]

    assert len(fake.created) == 1
    body = fake.created[0]["body"]
    assert body["apiVersion"] == "ray.io/v1" and body["kind"] == "RayJob"
    assert body["spec"]["suspend"] is True
    assert "managedBy" not in body["spec"]
    assert body["metadata"]["labels"][TAU_QUEUE_LABEL] == "research-gpu"
    head = body["spec"]["rayClusterSpec"]["headGroupSpec"]
    assert head["rayStartParams"]["num-gpus"] == "0"
    assert head["rayStartParams"]["num-cpus"] == "0"


def test_submit_resolves_profile_and_queue_from_cluster():
    fake = FakeCustomApi(cluster=_cluster_doc())
    panel = TauGridPanel(namespace="ray", run_name="demo",
                         client=ClusterClient(custom=fake, core=None))
    with tempfile.TemporaryDirectory() as d:
        panel.submit(notebook_bytes=NB_BYTES, staging_dir=Path(d))
    body = fake.created[0]["body"]
    wg = body["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]
    assert wg["replicas"] == 2
    assert wg["rayStartParams"]["num-gpus"] == "8"
    assert wg["template"]["spec"]["containers"][0]["resources"]["requests"]["nvidia.com/gpu"] == "8"


def test_submit_with_explicit_profile_and_queue():
    fake = FakeCustomApi()
    panel = TauGridPanel(namespace="ray", run_name="demo")
    with tempfile.TemporaryDirectory() as d:
        handle = panel.submit(
            notebook_bytes=NB_BYTES,
            profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
            queue="cpu-queue",
            staging_dir=Path(d),
            backend=KubernetesBackend(custom_objects=fake),
        )
        assert handle.name == "demo"
    body = fake.created[0]["body"]
    assert body["metadata"]["labels"][TAU_QUEUE_LABEL] == "cpu-queue"
    wg = body["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]
    assert wg["rayStartParams"]["num-gpus"] == "0"


def test_submit_requires_notebook_when_unresolved():
    fake = FakeCustomApi()
    panel = TauGridPanel(namespace="ray", run_name="",
                         client=ClusterClient(custom=fake, core=None))
    with pytest.raises(ValueError):
        panel.submit(notebook_bytes=None, cluster=_cluster_doc())


def test_submit_packages_payload_without_modifying_source():
    fake = FakeCustomApi(cluster=_cluster_doc())
    panel = TauGridPanel(namespace="ray", run_name="demo",
                         client=ClusterClient(custom=fake, core=None))
    with tempfile.TemporaryDirectory() as d:
        src = Path(d) / "src.ipynb"
        original = NB_BYTES
        src.write_bytes(original)
        panel.submit(notebook=str(src), staging_dir=Path(d) / "stage")
        assert src.read_bytes() == original


def test_submit_stages_runner_and_self_contained_payload():
    fake = FakeCustomApi(cluster=_cluster_doc())
    panel = TauGridPanel(namespace="ray", run_name="demo",
                         client=ClusterClient(custom=fake, core=None))
    with tempfile.TemporaryDirectory() as d:
        panel.submit(notebook_bytes=NB_BYTES, staging_dir=Path(d))
        staged_dir = panel.staged.notebook_path.parent
        names = {p.name for p in staged_dir.iterdir()}
        assert "analysis.ipynb" in names
        assert "_tau_runner.py" in names
        # The rendered entrypoint points at the staged runner.
        assert "_tau_runner.py" in panel.manifest["spec"]["entrypoint"]
