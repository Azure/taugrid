# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the server-side submit path (tau.jupyter.submit).

No cluster, no network: the Kubernetes client and backend are small fakes, and
the whole chain (package -> resolve -> render -> apply) runs in-process.
"""

from __future__ import annotations

import json
from typing import Any, Dict, List, Optional

import pytest

from tau._payload import decode
from tau.jupyter.submit import (
    SubmitError,
    _safe_name,
    build_plan,
    submit_notebook,
    submit_plan,
)

VALID_NB = json.dumps(
    {
        "nbformat": 4,
        "nbformat_minor": 5,
        "metadata": {
            "kernelspec": {"language": "python", "name": "python3"},
            "language_info": {"name": "python"},
        },
        "cells": [
            {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [{"x": 1}], "execution_count": 3, "source": ["print(1)"]},
        ],
    }
).encode()


def test_review_reports_rendered_resources_and_packaged_file_sizes():
    plan = build_plan(client=None, notebook_bytes=VALID_NB, namespace="research", cluster=cluster_doc(),
                      profile="cpu", extra_files={"helper.py": b"print(1)"})
    summary = plan.summary()
    assert summary["workers"] == 1
    assert summary["cpusPerWorker"] == ["4"]
    assert summary["memoryPerWorker"] == ["8Gi"]
    assert summary["gpusPerWorker"] == [0]
    assert summary["fileSizes"] == {"helper.py": 8}
    assert summary["packagedFileSizes"]["helper.py"] == 8
    assert summary["decodedBytes"] == sum(summary["packagedFileSizes"].values())
    assert summary["decodedBytes"] > summary["preparedBytes"] + 8
    assert summary["shutdownAfterFinish"] is True
    assert summary["payloadBudgetBytes"] == 1048576
    assert summary["encodedBudgetBytes"] == 65536


class NotFound(Exception):
    def __init__(self) -> None:
        super().__init__("rayjobs.ray.io not found")
        self.status = 404


class FakeCustom:
    def __init__(self, cluster: Optional[Dict[str, Any]] = None, existing: bool = False) -> None:
        self.cluster = cluster
        self.existing = existing
        self.created: List[Dict[str, Any]] = []

    def list_cluster_custom_object(self, **kwargs: Any) -> Dict[str, Any]:
        if self.cluster is None:
            return {"items": []}
        return {"items": [self.cluster]}

    def get_namespaced_custom_object(self, **kwargs: Any) -> Dict[str, Any]:
        if self.existing:
            return {"metadata": {"name": kwargs.get("name")}}
        raise NotFound()

    def create_namespaced_custom_object(self, **kwargs: Any) -> Dict[str, Any]:
        if self.existing:
            error = RuntimeError("already exists")
            error.status = 409
            raise error
        self.created.append(kwargs)
        return {"metadata": kwargs["body"]["metadata"]}


class FakeCore:
    pass


class FakeClient:
    def __init__(self, custom: FakeCustom) -> None:
        self.custom = custom
        self.core = FakeCore()


def cluster_doc() -> Dict[str, Any]:
    return {
        "spec": {
            "workspaceDefaults": {"defaultQueue": "research-gpu"},
            "workloadProfiles": [
                {"name": "training-8gpu", "status": {"state": "ready"}, "compute": {"workerReplicas": 2, "gpusPerWorker": 8}},
                {"name": "cpu", "status": {"state": "ready"}, "compute": {"workerReplicas": 1, "gpusPerWorker": 0}},
            ],
        }
    }


def test_build_plan_embeds_a_self_contained_payload():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))

    plan = build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray", name="demo")

    assert plan.name == "demo"
    assert plan.namespace == "ray"
    assert plan.queue == "research-gpu"
    assert plan.profile == "training-8gpu"
    assert plan.manifest["spec"]["submissionMode"] == "K8sJobMode"
    assert plan.manifest["spec"]["ttlSecondsAfterFinished"] == 600
    submitter = plan.manifest["spec"]["submitterPodTemplate"]["spec"]
    assert submitter["restartPolicy"] == "Never"
    assert submitter["containers"][0]["name"] == "submitter"
    # The submitter runs the entrypoint, so it must carry the payload and mounts.
    assert [c["name"] for c in submitter["initContainers"]] == ["tau-payload"]
    assert {v["name"] for v in submitter["volumes"]} >= {"script", "data"}
    mounts = {m["name"]: m["mountPath"] for m in submitter["containers"][0]["volumeMounts"]}
    assert mounts["script"] == "/script"
    assert mounts["data"] == "/data"
    assert plan.summary()["submissionMode"] == "K8sJobMode"
    assert plan.summary()["retentionSeconds"] == 600
    assert plan.digest

    spec = plan.manifest["spec"]
    head = spec["rayClusterSpec"]["headGroupSpec"]["template"]["spec"]
    init = head["initContainers"][0]
    assert init["name"] == "tau-payload"
    env = {e["name"]: e["value"] for e in init["env"]}
    files = decode(env["TAU_PAYLOAD_B64"], env["TAU_PAYLOAD_DIGEST"])
    # the prepared notebook, the runner, and the context manifest all ship
    assert set(files) == {"analysis.ipynb", "_tau_runner.py", "_tau_notebook_context.json"}
    assert plan.manifest["metadata"]["annotations"]["tau.azure.com/payload-digest"] == plan.digest
    assert plan.manifest["metadata"]["labels"]["kueue.x-k8s.io/queue-name"] == "research-gpu"
    # CLI parity: the managed-by label is what makes a run discoverable by list.
    assert plan.manifest["metadata"]["labels"]["tau.azure.com/managed-by"] == "tau"
    # Plugin submissions keep pods for 600s so a finished run can still be read.
    assert spec["ttlSecondsAfterFinished"] == 600
    # Plugin submissions use K8sJobMode so the driver output is a readable pod log.
    assert spec["submissionMode"] == "K8sJobMode"
    # outputs are stripped before embedding
    assert b"\"outputs\":[]" in files["analysis.ipynb"]


def test_build_plan_honours_explicit_profile_and_queue():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))

    plan = build_plan(
        client=client, notebook_bytes=VALID_NB, namespace="ray", profile="cpu", queue="team-q"
    )

    assert plan.profile == "cpu"
    assert plan.queue == "team-q"
    worker = plan.manifest["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]
    assert worker["rayStartParams"]["num-gpus"] == "0"
    assert plan.summary()["gpusPerWorker"] == [0]


def test_build_plan_rejects_unknown_profile():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))
    with pytest.raises(SubmitError) as err:
        build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray", profile="nope")
    assert "not available" in str(err.value)
    assert err.value.status == 400


def test_build_plan_rejects_malformed_notebook():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))
    with pytest.raises(SubmitError) as err:
        build_plan(client=client, notebook_bytes=b"not json", namespace="ray")
    assert "cannot submit" in str(err.value)
    assert err.value.status == 400


def test_build_plan_reports_oversize_as_413():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))
    with pytest.raises(SubmitError) as err:
        build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray", input_cap=10)
    assert err.value.status == 413


def test_build_plan_requires_a_taucluster():
    client = FakeClient(FakeCustom(cluster=None))
    with pytest.raises(SubmitError) as err:
        build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray")
    assert err.value.status == 409
    assert "no TauCluster" in str(err.value)


def test_build_plan_rejects_env_and_secret_collision():
    client = FakeClient(FakeCustom(cluster=cluster_doc()))
    with pytest.raises(SubmitError):
        build_plan(
            client=client,
            notebook_bytes=VALID_NB,
            namespace="ray",
            env={"TOKEN": "literal"},
            env_secret={"TOKEN": "hf:token"},
        )


def test_submit_plan_applies_once_and_returns_handle():
    custom = FakeCustom(cluster=cluster_doc())
    client = FakeClient(custom)
    plan = build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray", name="demo")

    result = submit_plan(client=client, plan=plan)

    assert result.name == "demo" and result.namespace == "ray" and result.kind == "RayJob"
    assert result.digest == plan.digest
    assert len(custom.created) == 1
    assert custom.created[0]["body"]["kind"] == "RayJob"
    assert custom.created[0]["plural"] == "rayjobs"


def test_submit_plan_refuses_to_replace_an_existing_run():
    custom = FakeCustom(cluster=cluster_doc(), existing=True)
    client = FakeClient(custom)
    plan = build_plan(client=client, notebook_bytes=VALID_NB, namespace="ray", name="demo")

    with pytest.raises(SubmitError) as err:
        submit_plan(client=client, plan=plan)

    assert err.value.status == 409
    assert custom.created == []


def test_submit_notebook_end_to_end():
    custom = FakeCustom(cluster=cluster_doc())
    client = FakeClient(custom)

    result = submit_notebook(client=client, notebook_bytes=VALID_NB, namespace="ray", name="demo")

    assert result.name == "demo"
    assert len(custom.created) == 1


def test_safe_name_sanitizes_and_bounds():
    assert _safe_name("my run/../x") == "my-run-x"
    assert len(_safe_name("a" * 200)) <= 63
    assert _safe_name("///") == "notebook-run"


def test_review_is_in_memory_and_plan_digest_covers_resolved_runtime(monkeypatch):
    import tempfile

    def forbid_temp(*args, **kwargs):
        raise AssertionError("preview must not persist notebook bytes")

    monkeypatch.setattr(tempfile, "mkdtemp", forbid_temp)
    client = FakeClient(FakeCustom(cluster=cluster_doc()))
    first = build_plan(client=client, notebook_bytes=VALID_NB)
    same = build_plan(client=client, notebook_bytes=VALID_NB)
    assert first.summary()["planDigest"] == same.summary()["planDigest"]
    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "example:changed")
    changed = build_plan(client=client, notebook_bytes=VALID_NB)
    assert changed.summary()["planDigest"] != first.summary()["planDigest"]


def test_cluster_discovery_rejects_ambiguous_or_incomplete_lists():
    class Many(FakeCustom):
        def list_cluster_custom_object(self, **kwargs):
            assert kwargs["limit"] == 2
            assert kwargs["_preload_content"] is False
            assert kwargs["_request_timeout"]
            return self.listing

    custom = Many()
    for listing in ({"items": [cluster_doc(), cluster_doc()]},
                    {"items": [cluster_doc()], "metadata": {"continue": "more"}}):
        custom.listing = listing
        with pytest.raises(SubmitError, match="ambiguous"):
            build_plan(client=FakeClient(custom), notebook_bytes=VALID_NB)


def test_profile_preserves_flat_scheduling_and_rejects_invalid_counts():
    from tau._profile import workload_profiles

    document = {"spec": {"workloadProfiles": [{"name": "cpu", "workerCount": 2,
        "gpusPerWorker": 0, "cpusPerWorker": 3, "memoryPerWorker": "6Gi",
        "nodeSelector": {"pool": "cpu"}}]}}
    profile = workload_profiles(document)[0]
    assert profile.node_selector == {"pool": "cpu"}
    assert profile.memory_per_worker == "6Gi"
    for count in (0, -1, True, 1.5):
        document["spec"]["workloadProfiles"][0]["workerCount"] = count
        with pytest.raises(ValueError):
            workload_profiles(document)


def test_malformed_notebook_structure_is_user_error():
    for document in ([1], {"nbformat": 4, "cells": [], "metadata": []},
                     {"nbformat": 4, "cells": [{"cell_type": "code", "source": [7]}],
                      "metadata": {"kernelspec": {"language": "python"}}}):
        with pytest.raises(SubmitError) as error:
            build_plan(client=FakeClient(FakeCustom()), notebook_bytes=json.dumps(document).encode())
        assert error.value.status == 400


def test_launcher_filter_never_discards_unrelated_panel_calls():
    from tau._notebook_pkg import prepare_notebook
    document = json.loads(VALID_NB)
    document["cells"][0]["source"] = ["import tau.widgets as tw\n", "model.panel()"]
    prepared, dropped = prepare_notebook(json.dumps(document).encode())
    assert not dropped
    assert "model.panel()" in prepared.decode()


@pytest.mark.parametrize("metadata", [[], None, "python"])
def test_invalid_metadata_is_rejected(metadata):
    document = json.loads(VALID_NB)
    document["metadata"] = metadata
    with pytest.raises(SubmitError) as error:
        build_plan(client=FakeClient(FakeCustom()), notebook_bytes=json.dumps(document).encode())
    assert error.value.status == 400


@pytest.mark.parametrize("source,expected", [
    ("import tau.widgets as tw\ntw.panel(namespace='ray')", True),
    ("from tau.widgets import panel\npanel()", True),
    ("import tau.widgets as tw; important_work()", False),
    ("import tau.widgets as tw\ntw.panel(namespace=side_effect())", False),
    ("%load_ext tau.widgets.ipython\n%taugrid", True),
])
def test_launcher_recognition_requires_only_known_statements(source, expected):
    from tau._notebook_pkg import classify_launcher_cell
    assert classify_launcher_cell({"source": source}) is expected
