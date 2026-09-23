# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for load-from-cluster + status reading (no cluster, no network).

The whole capability is a pure function of an injectable ClusterClient-shaped
object, so these tests drive it with small fake .custom/.core APIs and assert
on the normalized RunStatus values. The kubernetes package is never imported.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional

import pytest

from tau.widgets.panel import TauGridPanel
from tau.widgets.status import RunStatus, RunSummary, classify, list_runs, read_run_status

QUEUE_LABEL = "kueue.x-k8s.io/queue-name"


class NotFound(Exception):
    """Mimics a Kubernetes ApiException with a 404 status attribute."""

    def __init__(self, message: str = "rayjobs.ray.io \"missing\" not found") -> None:
        super().__init__(message)
        self.status = 404


class FakeCustom:
    """Records custom-object get/list calls; returns a scripted RayJob."""

    def __init__(
        self,
        rayjob: Optional[Dict[str, Any]] = None,
        *,
        get_error: Optional[Exception] = None,
        listing: Optional[Dict[str, Any]] = None,
        list_error: Optional[Exception] = None,
    ) -> None:
        self.rayjob = rayjob
        self.get_error = get_error
        self.listing = listing
        self.list_error = list_error
        self.gets: List[Dict[str, Any]] = []
        self.lists: List[Dict[str, Any]] = []

    def get_namespaced_custom_object(self, **kwargs: Any) -> Dict[str, Any]:
        self.gets.append(kwargs)
        if self.get_error is not None:
            raise self.get_error
        if self.rayjob is None:
            raise NotFound()
        return self.rayjob

    def list_namespaced_custom_object(self, **kwargs: Any) -> Dict[str, Any]:
        self.lists.append(kwargs)
        if self.list_error is not None:
            raise self.list_error
        return self.listing or {"items": []}


class FakeCore:
    """Serves pods per label selector from a selector -> items map."""

    def __init__(self, by_selector: Optional[Dict[str, List[Dict[str, Any]]]] = None) -> None:
        self.by_selector = by_selector or {}
        self.calls: List[Dict[str, Any]] = []

    def list_namespaced_pod(self, namespace: str, label_selector: str = "") -> Dict[str, Any]:
        self.calls.append({"namespace": namespace, "label_selector": label_selector})
        return {"items": self.by_selector.get(label_selector, [])}


class FakeClient:
    def __init__(self, custom: FakeCustom, core: Optional[FakeCore] = None) -> None:
        self.custom = custom
        self.core = core or FakeCore()


def make_rayjob(
    name: str = "run-1",
    *,
    job_status: Optional[str] = "RUNNING",
    ray_cluster: Optional[str] = "run-1-raycluster",
    queue: Optional[str] = None,
    conditions: Optional[List[Dict[str, Any]]] = None,
    job_id: Optional[str] = "job-abc",
) -> Dict[str, Any]:
    labels: Dict[str, str] = {}
    if queue is not None:
        labels[QUEUE_LABEL] = queue
    status: Dict[str, Any] = {}
    if job_status is not None:
        status["jobStatus"] = job_status
    if ray_cluster is not None:
        status["rayClusterName"] = ray_cluster
    if job_id is not None:
        status["jobId"] = job_id
    if conditions is not None:
        status["conditions"] = conditions
    return {"metadata": {"name": name, "labels": labels}, "status": status}


def make_pod(
    name: str,
    *,
    phase: str = "Running",
    node: Optional[str] = "node-a",
    ready_condition: Optional[bool] = None,
    container_ready: Optional[bool] = None,
    restarts: int = 0,
    node_type: str = "worker",
) -> Dict[str, Any]:
    status: Dict[str, Any] = {"phase": phase}
    if ready_condition is not None:
        status["conditions"] = [{"type": "Ready", "status": "True" if ready_condition else "False"}]
    if container_ready is not None:
        status["containerStatuses"] = [{"ready": container_ready, "restartCount": restarts}]
    else:
        status["containerStatuses"] = [{"restartCount": restarts}]
    return {
        "metadata": {"name": name, "labels": {"ray.io/node-type": node_type}},
        "spec": {"nodeName": node},
        "status": status,
    }


# -- load -------------------------------------------------------------------


def test_load_returns_running_status_and_sets_panel_fields():
    job = make_rayjob("run-1", job_status="RUNNING")
    custom = FakeCustom(job)
    core = FakeCore({"ray.io/cluster=run-1-raycluster": [make_pod("run-1-head", container_ready=True)]})
    panel = TauGridPanel(namespace="ray", client=FakeClient(custom, core))

    status = panel.load("run-1")

    assert isinstance(status, RunStatus)
    assert panel.run_name == "run-1"
    assert panel.status is status
    assert status.existing is True
    assert status.state == "running"
    assert status.display_state == "running"
    assert status.ray_cluster_name == "run-1-raycluster"
    assert status.job_id == "job-abc"
    assert custom.gets[0] == {
        "group": "ray.io",
        "version": "v1",
        "namespace": "ray",
        "plural": "rayjobs",
        "name": "run-1",
    }


def test_load_with_explicit_namespace_override():
    job = make_rayjob("run-2")
    custom = FakeCustom(job)
    panel = TauGridPanel(namespace="ray", client=FakeClient(custom))

    status = panel.load("run-2", namespace="team-a")

    assert panel.namespace == "team-a"
    assert status.namespace == "team-a"
    assert custom.gets[0]["namespace"] == "team-a"


def test_load_missing_run_is_not_submitted():
    custom = FakeCustom(get_error=NotFound())
    panel = TauGridPanel(namespace="ray", client=FakeClient(custom))

    status = panel.load("ghost")

    assert status.existing is False
    assert status.state == "not_submitted"
    assert status.display_state == "not_submitted"
    assert status.diagnostics == []
    assert status.pods == []


def test_load_read_failure_records_diagnostic():
    custom = FakeCustom(get_error=RuntimeError("boom"))
    panel = TauGridPanel(namespace="ray", client=FakeClient(custom))

    status = panel.load("flaky")

    assert status.existing is False
    assert status.state == "unknown"
    assert len(status.diagnostics) == 1
    diag = status.diagnostics[0]
    assert diag.code == "status-read-failed"
    assert diag.severity == "error"
    assert "boom" in diag.message
    assert "flaky" in diag.message


def test_load_without_client_returns_empty_status():
    panel = TauGridPanel(namespace="ray")

    status = panel.load("no-client")

    assert panel.run_name == "no-client"
    assert status.existing is False
    assert status.name == "no-client"
    assert status.namespace == "ray"


# -- status parsing ---------------------------------------------------------


def test_queue_label_extraction():
    job = make_rayjob("run-q", queue="research-gpu")
    status = read_run_status(FakeClient(FakeCustom(job)), namespace="ray", name="run-q")

    assert status.queue == "research-gpu"


def test_admitted_condition_parsing():
    job = make_rayjob(
        "run-adm",
        conditions=[{"type": "Admitted", "status": "True", "message": "admitted by kueue"}],
    )
    status = read_run_status(FakeClient(FakeCustom(job)), namespace="ray", name="run-adm")

    assert status.admitted is True
    assert status.message == "admitted by kueue"


def test_admitted_condition_false():
    job = make_rayjob("run-wait", conditions=[{"type": "Admitted", "status": "False"}])
    status = read_run_status(FakeClient(FakeCustom(job)), namespace="ray", name="run-wait")

    assert status.admitted is False


def test_pods_read_via_ray_cluster_selector_with_ready_and_restarts():
    job = make_rayjob("run-p", ray_cluster="run-p-raycluster")
    core = FakeCore(
        {
            "ray.io/cluster=run-p-raycluster": [
                make_pod("run-p-head", ready_condition=True, restarts=0, node_type="head"),
                make_pod("run-p-worker-0", container_ready=True, restarts=3, node_type="worker"),
            ]
        }
    )
    status = read_run_status(FakeClient(FakeCustom(job), core), namespace="ray", name="run-p")

    selectors = [call["label_selector"] for call in core.calls]
    assert selectors[0] == "ray.io/cluster=run-p-raycluster"
    assert "job-name=run-p" in selectors
    assert [p.name for p in status.pods] == ["run-p-head", "run-p-worker-0"]
    head, worker = status.pods
    assert head.ready is True
    assert head.node == "node-a"
    assert head.ray_node_type == "head"
    assert worker.ready is True
    assert worker.restarts == 3
    assert status.ready_pods == 2
    assert status.total_pods == 2
    assert any(d.code == "pod-restarts" and d.severity == "warn" for d in status.diagnostics)


def test_pod_ready_falls_back_to_container_statuses():
    job = make_rayjob("run-c", ray_cluster="run-c-raycluster")
    core = FakeCore(
        {
            "ray.io/cluster=run-c-raycluster": [
                make_pod("run-c-a", container_ready=True),
                make_pod("run-c-b", container_ready=False),
            ]
        }
    )
    status = read_run_status(FakeClient(FakeCustom(job), core), namespace="ray", name="run-c")

    by_name = {p.name: p for p in status.pods}
    assert by_name["run-c-a"].ready is True
    assert by_name["run-c-b"].ready is False
    assert status.ready_pods == 1


def test_pods_deduped_across_selectors():
    job = make_rayjob("run-d", ray_cluster="run-d-raycluster")
    shared = make_pod("run-d-worker-0", container_ready=True)
    core = FakeCore(
        {
            "ray.io/cluster=run-d-raycluster": [shared, make_pod("run-d-head", ready_condition=True)],
            "job-name=run-d": [shared, make_pod("run-d-worker-1", container_ready=True)],
        }
    )
    status = read_run_status(FakeClient(FakeCustom(job), core), namespace="ray", name="run-d")

    names = [p.name for p in status.pods]
    assert names == ["run-d-head", "run-d-worker-0", "run-d-worker-1"]
    assert len(names) == len(set(names))


# -- list_runs --------------------------------------------------------------


def test_list_runs_maps_listing_to_summaries():
    listing = {
        "items": [
            make_rayjob("alpha", job_status="RUNNING", queue="q-a"),
            make_rayjob("beta", job_status="SUCCEEDED", queue="q-b"),
            make_rayjob("gamma", job_status=None),
        ]
    }
    listing["items"][0]["metadata"]["creationTimestamp"] = "2026-01-01T00:00:00Z"
    custom = FakeCustom(listing=listing)

    rows = list_runs(FakeClient(custom), namespace="ray")

    assert [r.name for r in rows] == ["alpha", "beta", "gamma"]
    assert all(isinstance(r, RunSummary) for r in rows)
    assert rows[0].namespace == "ray"
    assert rows[0].state == "running"
    assert rows[0].queue == "q-a"
    assert rows[0].created == "2026-01-01T00:00:00Z"
    assert rows[1].state == "complete"
    assert rows[2].state == "queued"
    assert custom.lists[0]["plural"] == "rayjobs"


def test_list_runs_passes_label_selector():
    custom = FakeCustom(listing={"items": []})

    list_runs(FakeClient(custom), namespace="ray", label_selector="team=alpha")

    assert custom.lists[0]["label_selector"] == "team=alpha"


def test_list_runs_returns_empty_on_error():
    custom = FakeCustom(list_error=RuntimeError("api down"))

    rows = list_runs(FakeClient(custom), namespace="ray")

    assert rows == []


# -- classify ---------------------------------------------------------------


@pytest.mark.parametrize(
    ("rayjob", "expected"),
    [
        (None, "not_submitted"),
        ({}, "not_submitted"),
        ({"status": {}}, "queued"),
        ({"status": {"jobStatus": "SUCCEEDED"}}, "complete"),
        ({"status": {"jobStatus": "COMPLETED"}}, "complete"),
        ({"status": {"jobStatus": "SUCCESS"}}, "complete"),
        ({"status": {"jobStatus": "FAILED"}}, "failed"),
        ({"status": {"jobStatus": "DEAD"}}, "failed"),
        ({"status": {"jobStatus": "ERROR"}}, "failed"),
        ({"status": {"jobStatus": "PENDING"}}, "queued"),
        ({"status": {"jobStatus": "SUSPENDED"}}, "queued"),
        ({"status": {"jobStatus": "QUEUED"}}, "queued"),
        ({"status": {"jobStatus": "RUNNING"}}, "running"),
        ({"status": {"jobStatus": "running"}}, "running"),
    ],
)
def test_classify_covers_every_branch(rayjob, expected):
    assert classify(rayjob) == expected


def test_terminal_property_tracks_state():
    assert RunStatus(state="complete").terminal is True
    assert RunStatus(state="failed").terminal is True
    assert RunStatus(state="running").terminal is False
    assert RunStatus(state="queued").terminal is False


# -- real-SDK object shapes -------------------------------------------------


class Obj:
    """A stand-in for a kubernetes SDK model: snake_case attributes, no dict."""

    def __init__(self, **kwargs: Any) -> None:
        self.__dict__.update(kwargs)


class ObjCore:
    """A core API that returns SDK-shaped objects, like the real client."""

    def __init__(self, pods: List[Any]) -> None:
        self._pods = pods

    def list_namespaced_pod(self, namespace: str, label_selector: str = "") -> Any:
        return Obj(items=self._pods)


def test_pods_normalized_from_sdk_objects():
    """The real CoreV1Api returns V1Pod objects, not dicts; both must work."""
    job = make_rayjob("run-sdk", ray_cluster="run-sdk-raycluster")
    pod = Obj(
        metadata=Obj(name="run-sdk-head", labels={"ray.io/node-type": "head"}),
        spec=Obj(node_name="gpu-node-9"),
        status=Obj(
            phase="Running",
            conditions=[Obj(type="Ready", status="True")],
            container_statuses=[Obj(restart_count=2, ready=True)],
        ),
    )
    client = FakeClient(FakeCustom(job), ObjCore([pod]))

    status = read_run_status(client, namespace="ray", name="run-sdk")

    assert status.total_pods == 1
    info = status.pods[0]
    assert info.name == "run-sdk-head"
    assert info.node == "gpu-node-9"
    assert info.phase == "Running"
    assert info.ready is True
    assert info.restarts == 2
    assert info.ray_node_type == "head"
    assert status.ready_pods == 1


def test_sdk_object_pod_not_ready_without_ready_condition():
    job = make_rayjob("run-sdk2", ray_cluster="run-sdk2-raycluster")
    pod = Obj(
        metadata=Obj(name="run-sdk2-worker", labels={}),
        spec=Obj(node_name="node-b"),
        status=Obj(phase="Pending", conditions=[], container_statuses=[Obj(restart_count=0, ready=False)]),
    )
    client = FakeClient(FakeCustom(job), ObjCore([pod]))

    status = read_run_status(client, namespace="ray", name="run-sdk2")

    assert status.pods[0].ready is False
    assert status.ready_pods == 0
