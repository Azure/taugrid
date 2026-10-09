# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the notebook widget core (backend + renderer + profile)."""

import json
import tempfile
from pathlib import Path

import pytest

from tau._backend import KubernetesBackend
from tau._notebook_pkg import PayloadTooLarge, package
from tau._profile import (
    NoTauClusterFound,
    ProfileScope,
    ready_profiles,
    resolve,
    resolve_applicability,
)
from tau._render import (
    GPU_EXECUTION_RAY,
    Profile,
    UnsupportedShape,
    refuse,
    render_rayjob,
)
from tests.cluster_fixtures import ready_cluster

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
        gpu_execution=GPU_EXECUTION_RAY,
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


def test_render_refuses_a_gpu_profile_for_a_local_kernel():
    """S011: the notebook runs in the CPU-only head, so a GPU profile must not
    promise the notebook a GPU it cannot have."""
    with pytest.raises(UnsupportedShape) as err:
        render_rayjob(
            name="demo", namespace="ray", queue="research-gpu",
            profile=Profile(name="training-8gpu", workers=2, gpus_per_worker=8),
            entrypoint="python _tau_runner.py",
        )
    message = str(err.value)
    assert "control-only head" in message
    assert "gpu_execution='ray'" in message


def test_render_rejects_an_unknown_gpu_execution_mode():
    with pytest.raises(UnsupportedShape):
        render_rayjob(
            name="demo", namespace="ray", queue="q",
            profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
            entrypoint="cmd", gpu_execution="sometimes",
        )


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


def test_profile_resolution_uses_the_resolved_status_catalog():
    """S033: readiness comes from status.workloadProfiles.profiles.

    A profile declared in spec but not ready in the current generation must not
    be offered, and a spec-only document is not evidence of readiness.
    """
    cluster = ready_cluster(
        [
            {"name": "training-8gpu", "compute": {"workerReplicas": 4, "gpusPerWorker": 8}},
            {"name": "training-1gpu", "compute": {"workerReplicas": 1, "gpusPerWorker": 1}},
            {"name": "broken", "ready": False, "compute": {"workerReplicas": 2, "gpusPerWorker": 2}},
        ],
        default_queue="research-gpu",
    )
    profiles, queue = resolve(cluster, namespace="ray")
    assert queue == "research-gpu"
    names = {p.name for p in profiles}
    assert names == {"training-8gpu", "training-1gpu"}
    eight = next(p for p in profiles if p.name == "training-8gpu")
    assert eight.workers == 4 and eight.gpus_per_worker == 8


def test_spec_only_declaration_is_not_treated_as_ready():
    """The original defect: a spec entry reading a status field inside itself."""
    cluster = {
        "metadata": {"generation": 1},
        "spec": {
            "workspaceDefaults": {"defaultQueue": "q"},
            "workloadProfiles": [
                {"name": "cpu", "status": {"state": "ready"}, "workerCount": 1, "gpusPerWorker": 0},
            ],
        },
    }
    profiles, skipped = ready_profiles(cluster, scope=ProfileScope(namespace="ray"))
    assert profiles == []
    assert any("observedGeneration is missing" in item.reason for item in skipped)


def test_stale_profile_set_is_withheld_entirely():
    """A generation mismatch withholds the set rather than mixing current and stale."""
    cluster = ready_cluster(
        [{"name": "cpu", "compute": {"workerCount": 1, "gpusPerWorker": 0}}],
        generation=2,
        observed_generation=1,
    )
    profiles, skipped = ready_profiles(cluster, scope=ProfileScope(namespace="ray"))
    assert profiles == []
    assert any("stale" in item.reason for item in skipped)


def test_applicability_is_enforced_for_namespace_and_team():
    """S034: an empty applicability list is global; a scoped list must match."""
    cluster = ready_cluster(
        [
            {"name": "global-cpu", "compute": {"workerCount": 1, "gpusPerWorker": 0}},
            {"name": "team-a-gpu", "compute": {"workerCount": 1, "gpusPerWorker": 1},
             "applicability": {"namespaces": ["team-a"], "teams": ["research"]}},
            {"name": "team-b-something", "compute": {"workerCount": 1, "gpusPerWorker": 1},
             "applicability": {"namespaces": ["team-b"]}},
        ],
    )
    allowed, skipped = ready_profiles(
        cluster, scope=ProfileScope(namespace="team-a", team="research")
    )
    assert {p.name for p in allowed} == {"global-cpu", "team-a-gpu"}
    withheld = {item.name: item.reason for item in skipped}
    assert "does not authorize namespace" in withheld["team-b-something"]

    # A dimension the caller cannot know is not filtered: the notebook submits
    # into a known namespace but has no team of its own, and resolving team from
    # the selected profile is what makes the catalog usable.
    unknown_team, _ = ready_profiles(cluster, scope=ProfileScope(namespace="team-a"))
    assert {p.name for p in unknown_team} == {"global-cpu", "team-a-gpu"}

    # An explicitly supplied team that does not match is still enforced.
    wrong_team, skipped_wrong = ready_profiles(
        cluster, scope=ProfileScope(namespace="team-a", team="someone-else")
    )
    assert {p.name for p in wrong_team} == {"global-cpu"}
    assert "does not authorize team" in {i.name: i.reason for i in skipped_wrong}["team-a-gpu"]


def test_render_pins_the_submitter_backoff_limit():
    """KubeRay 1.6 moved the submitter Job retry count to submitterConfig.

    Leaving it unset lets KubeRay default the submitter Job to 2 retries, so a
    deterministic notebook failure is retried and the successful retry reports
    SUCCEEDED, hiding the real failure.
    """
    job = render_rayjob(
        name="demo", namespace="ray", queue="q",
        profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
        entrypoint="cmd",
    )
    assert job["spec"]["submitterConfig"] == {"backoffLimit": 0}


def test_a_dimension_the_caller_cannot_know_is_not_filtered():
    """The notebook knows its namespace but no team, and must still see profiles."""
    cluster = ready_cluster([
        {"name": "gpu", "compute": {"workerCount": 1, "gpusPerWorker": 1},
         "applicability": {"namespaces": ["ws"], "teams": ["research"], "lanes": ["training"]}},
    ])
    known, _ = ready_profiles(cluster, scope=ProfileScope(namespace="ws"))
    assert [p.name for p in known] == ["gpu"]
    denied, skipped = ready_profiles(cluster, scope=ProfileScope(namespace="ws", team="other"))
    assert denied == []
    assert "does not authorize team" in skipped[0].reason


def test_resolve_applicability_mirrors_the_cli():
    cluster = ready_cluster([
        {"name": "one", "compute": {"workerCount": 1, "gpusPerWorker": 0},
         "applicability": {"teams": ["research"], "lanes": ["training"]}},
        {"name": "multi", "compute": {"workerCount": 1, "gpusPerWorker": 0},
         "applicability": {"teams": ["a", "b"]}},
    ])
    assert resolve_applicability(cluster, "one") == ("research", "training")
    with pytest.raises(ValueError, match="multiple teams"):
        resolve_applicability(cluster, "multi")


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
    # The runner executes with /data as its working directory in a real pod;
    # point the override at tmp_path so the test never needs to create /data
    # (which the Linux CI runner cannot do).
    monkeypatch.setenv("TAU_DATA_DIR", str(tmp_path / "data"))
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
