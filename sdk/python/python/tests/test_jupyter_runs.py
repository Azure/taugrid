# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import asyncio
import io
import json
import threading
from types import SimpleNamespace

import pytest

from tau.jupyter import runs
from tau.widgets.kube import ClusterClient


def resource(name="train", kind="Job", uid="run-uid", **status):
    return {
        "kind": kind,
        "metadata": {
            "name": name, "uid": uid, "namespace": "ray",
            "labels": {"tau.azure.com/workload": "train", "kueue.x-k8s.io/queue-name": "gpu"},
            "creationTimestamp": "2026-09-20T00:00:00Z",
        },
        "spec": {}, "status": status,
    }


def owner(kind, uid):
    return {"kind": kind, "uid": uid, "name": "train", "controller": True}


def pod(uid="run-uid", kind="Job", **status):
    return {
        "metadata": {"name": "worker", "uid": "pod-uid", "ownerReferences": [owner(kind, uid)]},
        "spec": {"containers": [{"name": "main"}], "initContainers": [{"name": "init"}]},
        "status": {"phase": "Pending", **status},
    }


class LogBody(io.BytesIO):
    def set_read_timeout(self, timeout):
        assert 0 < timeout <= 5


class FakeApis:
    def __init__(self):
        self.job = resource()
        self.rayjob = resource(kind="RayJob", jobStatus="PENDING")
        self.cluster = resource(kind="RayCluster", uid="cluster-uid")
        self.cluster["metadata"]["ownerReferences"] = [owner("RayJob", "run-uid")]
        self.jobs = [self.job]
        self.rayjobs = [self.rayjob]
        self.workloads = []
        self.pods = [pod()]
        self.text = "hello"
        self.namespaces = [
            {"metadata": {"name": "default"}},
            {"metadata": {"name": "team-a", "labels": {"tau.azure.com/workspace": "team-a"}}},
            {"metadata": {"name": "beta", "labels": {"tau.azure.com/workspace": "beta"}}},
        ]
        self.calls = []
        self.denied = set()

    def check(self, key):
        if key in self.denied:
            raise RuntimeError("forbidden")

    def list_namespaced_job(self, **kwargs):
        self.check("jobs")
        return {"items": self.jobs, "metadata": {}}

    def read_namespaced_job(self, **kwargs):
        return self.job

    def get_namespaced_custom_object(self, **kwargs):
        self.check(kwargs["plural"])
        return self.cluster if kwargs["plural"] == "rayclusters" else self.rayjob

    def list_namespaced_custom_object(self, **kwargs):
        self.check(kwargs["plural"])
        return {"items": self.rayjobs if kwargs["plural"] == "rayjobs" else self.workloads}

    def list_namespaced_pod(self, **kwargs):
        self.check("pods")
        return {"items": self.pods}

    def read_namespaced_pod(self, **kwargs):
        return self.pods[0]

    def read_namespaced_pod_log(self, **kwargs):
        self.calls.append(kwargs)
        return LogBody(self.text.encode("utf-8"))

    def list_namespace(self, **kwargs):
        self.check("namespaces")
        return {"items": self.namespaces, "metadata": {}}

    def client(self):
        return ClusterClient(core=self, custom=self, batch=self)


def phase(status, key):
    return next(item for item in status.phases if item["key"] == key)


def test_lists_both_tau_kinds_filters_queue_and_submitters():
    api = FakeApis()
    unrelated = resource("unrelated")
    unrelated["metadata"]["labels"] = {}
    submitter = resource("submitter")
    submitter["metadata"]["ownerReferences"] = [owner("RayJob", "parent")]
    api.jobs.extend([unrelated, submitter])
    api.rayjob["metadata"]["creationTimestamp"] = "2026-09-21T00:00:00Z"
    result = runs.list_runs(api.client(), namespace="ray", queue="gpu")
    assert [(row["name"], row["kind"]) for row in result["runs"]] == [("train", "RayJob"), ("train", "Job")]
    assert runs.list_runs(api.client(), namespace="ray", queue="other")["runs"] == []
    api.denied.add("jobs")
    result = runs.list_runs(api.client(), namespace="ray")
    assert len(result["runs"]) == 1
    assert result["warnings"]


def test_admission_matches_owner_uid_not_name_and_reports_denial():
    api = FakeApis()
    workload = resource("old", conditions=[{"type": "Admitted", "status": "True"}])
    workload["metadata"]["ownerReferences"] = [owner("Job", "old-uid")]
    api.workloads = [workload]
    assert phase(runs.read_run(api.client(), namespace="ray", name="train", kind="Job"), "admission")["state"] == "unknown"
    workload["metadata"]["ownerReferences"] = [owner("Job", "run-uid")]
    assert runs.read_run(api.client(), namespace="ray", name="train", kind="Job").admitted is True
    api.denied.add("workloads")
    status = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert phase(status, "admission")["state"] == "unknown"
    assert status.diagnostics


def test_retry_pod_is_not_terminal_job_and_waiting_reason_is_visible():
    api = FakeApis()
    api.job["status"] = {"failed": 1, "active": 1}
    api.pods = [pod(containerStatuses=[{"name": "main", "state": {"waiting": {"reason": "ImagePullBackOff"}}}])]
    status = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert status.state == "running" and not status.terminal
    assert "ImagePullBackOff" in phase(status, "pods")["detail"]
    api.job["status"]["conditions"] = [{"type": "Complete", "status": "True"}]
    api.pods = []
    status = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert status.terminal
    assert phase(status, "pods")["state"] == "skipped"


@pytest.mark.parametrize("over_limit", [False, True])
def test_incomplete_job_pod_discovery_cannot_select_loss_source(monkeypatch, over_limit):
    api = FakeApis()
    selected = pod()
    selected["metadata"]["uid"] = "pod-uid"
    listing = {"items": [selected] * (runs.LIMIT + 1 if over_limit else 1),
               "metadata": {} if over_limit else {"continue": "more-pods"}}
    monkeypatch.setattr(api, "list_namespaced_pod", lambda **kwargs: listing)
    info = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert info.metric_discovery_error


@pytest.mark.parametrize("retries,expected", [(None, 7), (0, 0)])
def test_kubernetes_retry_override_is_explicit(monkeypatch, retries, expected):
    from kubernetes import client, config
    from tau.widgets.kube import load_client

    configuration = SimpleNamespace(retries=7)
    monkeypatch.setattr(config, "load_incluster_config", lambda: None)
    monkeypatch.setattr(client.Configuration, "get_default_copy", lambda: configuration)
    monkeypatch.setattr(client, "ApiClient", lambda settings: settings)
    for api_name in ("CustomObjectsApi", "CoreV1Api", "BatchV1Api"):
        monkeypatch.setattr(client, api_name, lambda transport: transport)
    connection = load_client(retries=retries)
    assert configuration.retries == expected
    assert connection.core is connection.custom is connection.batch is configuration


def test_cluster_creation_does_not_claim_ready_and_rejects_stale_owners():
    api = FakeApis()
    api.rayjob["status"]["rayClusterName"] = "cluster"
    api.pods = [pod("cluster-uid", "RayCluster"), pod("old-uid", "RayCluster")]
    status = runs.read_run(api.client(), namespace="ray", name="train")
    assert phase(status, "cluster")["state"] == "done"
    assert status.total_pods == 1 and status.ready_pods == 0
    api.cluster["metadata"]["ownerReferences"] = [owner("RayJob", "old-uid")]
    status = runs.read_run(api.client(), namespace="ray", name="train")
    assert status.total_pods == 0
    assert phase(status, "cluster")["state"] == "unknown"


def test_manager_execution_is_remote_not_locally_running():
    api = FakeApis()
    api.job["spec"]["managedBy"] = "kueue.x-k8s.io/multikueue"
    status = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert "remote" in phase(status, "execution")["detail"].lower()
    assert phase(status, "execution")["state"] == "unknown"
    assert status.pods == []


def test_logs_bound_utf8_and_verify_membership_and_options():
    api = FakeApis()
    api.text = "€" * 30000
    result = runs.read_logs(api.client(), namespace="ray", name="train", kind="Job", pod="worker", container="main", tail=1000, previous=True, timestamps=True)
    assert len(result["text"].encode("utf-8")) <= 65536
    assert "�" not in result["text"]
    assert result["possiblyTruncated"]
    assert api.calls[0]["follow"] is False
    assert api.calls[0]["limit_bytes"] == 65537
    assert api.calls[0]["_preload_content"] is False
    assert api.calls[0]["tail_lines"] == 1000
    assert api.calls[0]["previous"] and api.calls[0]["timestamps"]
    assert api.calls[0]["_request_timeout"]
    for kwargs in ({"pod": "stranger", "container": "main"}, {"pod": "worker", "container": "other"}, {"pod": "worker", "container": "main", "tail": -1}):
        with pytest.raises(runs.ReadError):
            runs.read_logs(api.client(), namespace="ray", name="train", kind="Job", **kwargs)
    assert len(api.calls) == 1
    api.pods[0]["metadata"]["ownerReferences"][0]["uid"] = "old-uid"
    with pytest.raises(runs.ReadError):
        runs.read_logs(api.client(), namespace="ray", name="train", kind="Job", pod="worker", container="main")


def test_portal_configuration_rejects_unsafe_links(monkeypatch):
    for url in ("javascript:alert(1)", "//portal.example", "https://user:pass@portal.example"):
        monkeypatch.setenv("TAUGRID_PORTAL_URL", url)
        assert runs.portal_url() is None
    monkeypatch.setenv("TAUGRID_PORTAL_URL", "https://portal.example/base")
    assert runs.portal_url() == "https://portal.example/base"


def test_real_sdk_model_field_shapes_and_existing_serializer():
    from tau.jupyter.server import _status_dict

    api = FakeApis()
    api.job = SimpleNamespace(metadata=SimpleNamespace(name="train", uid="run-uid", labels={}, annotations={}), spec=SimpleNamespace(), status=SimpleNamespace(active=1, conditions=[]))
    status = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    result = _status_dict(status)
    assert result["existing"] and result["kind"] == "Job"
    assert result["phases"] and result["pods"][0]["containers"] == ["main", "init"]


@pytest.mark.parametrize(("status", "expected"), [
    ({"jobStatus": "RUNNING"}, "running"),
    ({"jobDeploymentStatus": "Running"}, "running"),
    ({"jobDeploymentStatus": "Complete"}, "complete"),
    ({"jobStatus": "SUCCEEDED", "jobDeploymentStatus": "Failed"}, "failed"),
    ({"jobStatus": "STOPPED"}, "failed"),
    ({"jobStatus": "PENDING", "reason": "SubmissionFailed"}, "failed"),
    ({"jobStatus": "PENDING"}, "queued"),
])
def test_ray_execution_uses_authoritative_markers(status, expected):
    api = FakeApis()
    api.rayjob["status"] = status
    result = runs.read_run(api.client(), namespace="ray", name="train")
    assert result.state == expected
    assert result.terminal == (expected in ("failed", "complete"))


def test_batch_failure_takes_precedence_over_completion():
    api = FakeApis()
    api.job["status"]["conditions"] = [
        {"type": "Complete", "status": "True"},
        {"type": "Failed", "status": "True"},
    ]
    assert runs.read_run(api.client(), namespace="ray", name="train", kind="Job").state == "failed"


def test_missing_resources_terminal_teardown_and_partial_reads(monkeypatch):
    api = FakeApis()
    missing = RuntimeError("not found")
    missing.status = 404

    def not_found(**kwargs):
        raise missing

    monkeypatch.setattr(api, "read_namespaced_job", not_found)
    assert not runs.read_run(api.client(), namespace="ray", name="train", kind="Job").existing
    api.rayjob["status"] = {"jobStatus": "SUCCEEDED", "rayClusterName": "cluster"}
    original = api.get_namespaced_custom_object

    def custom_get(**kwargs):
        return not_found() if kwargs["plural"] == "rayclusters" else original(**kwargs)

    monkeypatch.setattr(api, "get_namespaced_custom_object", custom_get)
    result = runs.read_run(api.client(), namespace="ray", name="train")
    assert phase(result, "cluster")["state"] == "skipped"
    assert result.state == "complete"
    api.denied = {"jobs", "rayjobs"}
    listing = runs.list_runs(api.client(), namespace="ray")
    assert not listing["runs"] and len(listing["warnings"]) == 2


def test_list_continuation_and_result_metadata(monkeypatch):
    api = FakeApis()
    monkeypatch.setattr(api, "list_namespaced_job", lambda **kwargs: {
        "items": api.jobs, "metadata": {"continue": "next-page"},
    })
    assert runs.list_runs(api.client(), namespace="ray")["truncated"]
    api.job["metadata"]["annotations"] = {"tau.azure.com/result-path": "folder/results", "tau.azure.com/result-pvc": "outputs"}
    api.job["metadata"]["labels"] = {}
    api.denied = {"pods"}
    result = runs.read_run(api.client(), namespace="ray", name="train", kind="Job")
    assert result.output["path"] == "folder/results" and result.output["pvc"] == "outputs"
    assert phase(result, "admission")["state"] == "skipped"
    assert phase(result, "pods")["state"] == "unknown"
    with pytest.raises(runs.ReadError):
        runs.read_logs(api.client(), namespace="ray", name="train", kind="Job", pod="worker", container="main")
    assert not api.calls


def test_submitter_uid_chain_separates_workers_and_hidden_jobs():
    api = FakeApis()
    submitter = resource(name="driver", uid="job-uid")
    submitter["metadata"]["ownerReferences"] = [owner("RayJob", "run-uid")]
    api.jobs = [submitter]
    api.pods = [pod(uid="job-uid", phase="Succeeded")]
    api.pods[0]["metadata"]["uid"] = "pod-uid"
    info = runs.read_run(api.client(), namespace="ray", name="train", kind="RayJob")
    assert info.total_pods == 0 and info.ready_pods == 0
    assert info.pod_roles == {"worker": "submitter"}
    assert info.metric_sources[0]["podUid"] == "pod-uid"
    assert info.metric_sources[0]["ownerUid"] == "job-uid"
    assert all(row["kind"] != "Job" for row in runs.list_runs(api.client(), namespace="ray")["runs"])
    api.jobs[0]["metadata"]["ownerReferences"][0]["uid"] = "old-run"
    assert not runs.read_run(api.client(), namespace="ray", name="train", kind="RayJob").metric_sources


def test_ambiguous_or_denied_submitters_fail_closed_but_httpmode_still_reads():
    api = FakeApis()
    api.rayjob["spec"]["submissionMode"] = "HTTPMode"
    assert runs.read_run(api.client(), namespace="ray", name="train", kind="RayJob").state == "queued"
    submitter = resource(name="driver", uid="job-uid")
    submitter["metadata"]["ownerReferences"] = [owner("RayJob", "run-uid")]
    api.jobs = [submitter, submitter]
    info = runs.read_run(api.client(), namespace="ray", name="train", kind="RayJob")
    assert info.metric_discovery_error and not info.metric_sources
    api.denied.add("jobs")
    assert runs.read_run(api.client(), namespace="ray", name="train", kind="RayJob").metric_discovery_error


def test_metrics_rechecks_entire_uid_chain_before_reading(monkeypatch):
    from tau.jupyter.metrics import read_stdout
    import time

    api = FakeApis()
    api.job = resource(name="driver", uid="job-uid")
    api.job["metadata"]["ownerReferences"] = [owner("RayJob", "run-uid")]
    selected = pod(uid="job-uid")
    selected["metadata"]["uid"] = "pod-uid"
    selected["metadata"]["ownerReferences"][0]["name"] = "driver"
    monkeypatch.setattr(api, "read_namespaced_pod", lambda **kwargs: selected, raising=False)
    info = runs.NativeStatus(name="train", namespace="ray", uid="run-uid")
    source = {"type": "stdout", "pod": "worker", "podUid": "pod-uid", "container": "main", "ownerKind": "Job", "ownerUid": "job-uid"}
    assert read_stdout(api.client(), info, source, time.monotonic() + 10) == b"hello"
    api.job["metadata"]["ownerReferences"][0]["uid"] = "replacement"
    with pytest.raises(ValueError, match="controller identity"):
        read_stdout(api.client(), info, source, time.monotonic() + 10)
    assert len(api.calls) == 1
    selected["metadata"]["uid"] = "replacement"
    with pytest.raises(ValueError, match="Pod"):
        read_stdout(api.client(), info, source, time.monotonic() + 10)
    api.rayjob["metadata"]["uid"] = "replacement"
    with pytest.raises(ValueError, match="Workload"):
        read_stdout(api.client(), info, source, time.monotonic() + 10)


def test_status_metrics_are_opt_in_and_terminal_attempt_is_requested(monkeypatch):
    from tau.jupyter import server, metrics
    from tornado.web import HTTPError

    api = FakeApis()
    api.job["status"] = {"conditions": [{"type": "Complete", "status": "True"}]}
    calls = []

    def collect(client, status, force=False):
        calls.append(force)
        return metrics.empty("No evidence")

    monkeypatch.setattr(metrics.collector, "collect", collect)
    handler = FakeHandler(api, namespace="ray", name="train", kind="Job")
    asyncio.run(server.StatusHandler.get(handler))
    assert "metrics" not in handler.response and calls == []
    handler.arguments["includeMetrics"] = "true"
    asyncio.run(server.StatusHandler.get(handler))
    assert handler.response["terminal"] and handler.response["metrics"]["state"] == "unavailable"
    assert calls == [True]
    handler.arguments["includeMetrics"] = "yes"
    with pytest.raises(HTTPError) as caught:
        asyncio.run(server.StatusHandler.get(handler))
    assert caught.value.status_code == 400


class FakeHandler:
    def __init__(self, api, **arguments):
        self.api = api
        self.arguments = arguments
        self.current_user = object()
        self.request = SimpleNamespace(method="POST", body=b"{}")
        self.headers = {}
        self.response = None

    def client(self):
        return self.api.client()

    def get_argument(self, name, default):
        return self.arguments.get(name, default)

    def finish(self, value):
        self.response = value

    def set_header(self, name, value):
        self.headers[name] = value

    def target(self):
        from tau.jupyter.server import _ClientMixin
        return _ClientMixin.target(self)

    async def read(self, reader, **kwargs):
        from tau.jupyter.server import _ClientMixin
        return await _ClientMixin.read(self, reader, **kwargs)

    def json_body(self):
        return json.loads(self.request.body)


def test_offline_handlers_roundtrip_and_threaded_reads(monkeypatch):
    from tau.jupyter import server
    api = FakeApis()
    handler = FakeHandler(api, namespace="ray", name="train", kind="Job", pod="worker", container="main")
    main_thread = threading.get_ident()
    threads = []
    original = api.read_namespaced_job

    def read_job(**kwargs):
        threads.append(threading.get_ident())
        return original(**kwargs)

    monkeypatch.setattr(api, "read_namespaced_job", read_job)
    asyncio.run(server.StatusHandler.get(handler))
    assert handler.response["kind"] == "Job"
    assert threads and all(thread != main_thread for thread in threads)
    asyncio.run(server.RunsHandler.get(handler))
    assert {row["kind"] for row in handler.response["runs"]} == {"Job", "RayJob"}
    asyncio.run(server.LogsHandler.get(handler))
    assert handler.response["text"] == "hello"
    assert handler.headers["Cache-Control"] == "no-store"
    assert api.calls[0]["tail_lines"] == 200
    monkeypatch.setenv("TAUGRID_PORTAL_URL", "https://portal.example/")
    server.CapabilitiesHandler.get(handler)
    assert handler.response["portalUrl"] == "https://portal.example/"
    handler.arguments.pop("kind")
    asyncio.run(server.StatusHandler.get(handler))
    assert handler.response["kind"] == "RayJob"


@pytest.mark.parametrize("arguments", [{"tail": "bad"}, {"tail": "1001"}, {"previous": "yes"}, {"timestamps": "1"}, {"kind": "Pod"}, {"namespace": "../other"}, {"pod": "unrelated"}])
def test_log_handler_rejects_bad_inputs(arguments):
    from tornado.web import HTTPError
    from tau.jupyter import server
    api = FakeApis()
    options = {"namespace": "ray", "name": "train", "kind": "Job", "pod": "worker", "container": "main", **arguments}
    with pytest.raises(HTTPError) as failure:
        asyncio.run(server.LogsHandler.get(FakeHandler(api, **options)))
    assert failure.value.status_code in (400, 403)
    assert not api.calls


def test_handlers_require_authentication_and_preserve_submit_gate(monkeypatch):
    from tornado.web import HTTPError
    from tau.jupyter import server
    handler = FakeHandler(FakeApis())
    handler.current_user = None
    handlers = server._handlers("/user/example/taugrid/api")
    assert {path.rsplit("/", 1)[-1] for path, _ in handlers} == {"capabilities", "status", "runs", "namespaces", "destinations", "files", "logs", "preview", "submit"}
    for path, handler_type in handlers:
        method = handler_type.post if path.endswith(("preview", "submit")) else handler_type.get
        with pytest.raises(HTTPError) as failure:
            method(handler)
        assert failure.value.status_code == 403
    handler.current_user = object()
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", False)
    with pytest.raises(HTTPError) as failure:
        asyncio.run(server.SubmitHandler.post(handler))
    assert failure.value.status_code == 409
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    with pytest.raises(HTTPError) as failure:
        asyncio.run(server.SubmitHandler.post(handler))
    assert failure.value.status_code == 400

def test_namespace_discovery_flags_tau_ready_and_orders_them_first():
    api = FakeApis()

    result = runs.list_namespaces(api.client())

    # Tau-ready destinations come first so the picker never makes a researcher
    # guess a namespace that cannot host the run.
    assert [row["name"] for row in result["namespaces"]] == ["beta", "team-a", "default"]
    assert [row["tauEnabled"] for row in result["namespaces"]] == [True, True, False]
    assert result["warnings"] == []


def test_namespace_discovery_reports_denial_without_raising():
    api = FakeApis()
    api.denied.add("namespaces")

    result = runs.list_namespaces(api.client())

    assert result["namespaces"] == []
    assert result["warnings"] and "namespace discovery failed" in result["warnings"][0]


def test_namespace_continuation_warns_even_for_short_pages():
    api = FakeApis()
    api.list_namespace = lambda **kwargs: {"items": api.namespaces[:1], "metadata": {"continue": "next"}}
    assert runs.list_namespaces(api.client())["warnings"]


def test_ray_status_uses_explicit_states_not_reason_substrings():
    from tau.widgets.status import classify

    for status, expected in (({}, "queued"), ({"jobStatus": "RUNNING", "reason": "Stopping"}, "running"),
                             ({"jobStatus": "STOPPED"}, "failed"),
                             ({"jobStatus": "SUCCEEDED"}, "complete"),
                             ({"jobStatus": "FUTURE_STATE"}, "unknown")):
        obj = {"status": status}
        assert runs.ray_state(obj) == expected
        assert classify(obj) == expected


@pytest.mark.parametrize("changed", ["pod", "run", "container"])
def test_logs_discard_evidence_when_identity_changes_during_read(changed):
    api = FakeApis()
    original = api.read_namespaced_pod_log

    def replace(**kwargs):
        response = original(**kwargs)
        if changed == "pod":
            api.pods[0]["metadata"]["uid"] = "replacement"
        elif changed == "run":
            api.job["metadata"]["uid"] = "replacement"
        else:
            api.pods[0]["spec"]["containers"] = [{"name": "other"}]
        return response

    api.read_namespaced_pod_log = replace
    with pytest.raises(runs.ReadError):
        runs.read_logs(api.client(), namespace="ray", name="train", kind="Job", pod="worker", container="main")
