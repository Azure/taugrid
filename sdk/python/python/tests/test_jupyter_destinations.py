# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline destination discovery: no cluster or server process."""

import asyncio
from types import SimpleNamespace

import pytest

from tau.jupyter import server
from tau.jupyter.submit import build_plan
from tests.test_jupyter_submit import VALID_NB, cluster_doc


@pytest.mark.parametrize("listing", [{"items": []}, {"items": [{}, {}]}, {"items": [{}], "metadata": {"continue": "more"}}])
def test_missing_or_ambiguous_cluster_never_invents_profiles(listing):
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    client.custom.list_cluster_custom_object = lambda **kwargs: listing
    result = list_destinations(client)
    assert result["profiles"] == []
    assert result["defaultQueue"] is None
    assert any("operator" in warning for warning in result["warnings"])


def test_no_visible_namespaces_skips_queue_fanout():
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    client.namespaces = {"items": []}
    result = list_destinations(client)
    assert result["namespace"] is None
    assert result["queues"] == []
    assert not any(call[0] == "queues" for call in client.calls)


@pytest.mark.parametrize("nested", [False, True])
def test_profile_overrides_readiness_and_explicit_queue_match_plan(nested, monkeypatch):
    from tau.jupyter.destinations import list_destinations

    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "runtime:environment")
    client = CatalogClient()
    compute = {"gpusPerWorker": 2, "cpusPerWorker": 7.5, "memoryPerWorker": "24Gi", "image": "runtime:profile"}
    profile = {"name": "gpu", "description": "Two GPU workers", "priorityClass": "research-high"}
    profile.update({"compute": {**compute, "workerReplicas": 2}} if nested else {**compute, "workerCount": 2})
    client.cluster["spec"]["workloadProfiles"] = [profile, {"name": "pending", "status": {"state": "pending"}}]
    client.queues = {"items": [{"metadata": {"name": "alternate"}}]}
    catalog = list_destinations(client, "default")
    plan = build_plan(client=None, notebook_bytes=VALID_NB, namespace="default", cluster=client.cluster, profile="gpu", queue="alternate")
    summary = plan.summary()
    assert len(catalog["profiles"]) == 1
    option = catalog["profiles"][0]
    assert option["workers"] == summary["workers"] == 2
    assert option["gpusPerWorker"] == summary["gpusPerWorker"][0] == 2
    assert option["cpusPerWorker"] == summary["cpusPerWorker"][0] == "7.5"
    assert option["memoryPerWorker"] == summary["memoryPerWorker"][0] == "24Gi"
    assert option["image"] == summary["submitterImage"] == "runtime:profile"
    assert option["priority"] == "research-high"
    assert summary["queue"] == "alternate"
    assert catalog["defaultQueue"] != summary["queue"]
    assert client.calls[-1][1]["namespace"] == "default"


def test_profile_fallbacks_match_build_plan(monkeypatch):
    from tau.jupyter.destinations import list_destinations

    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "runtime:environment")
    client = CatalogClient()
    client.cluster["spec"].pop("workspaceDefaults", None)
    client.cluster["spec"]["workloadProfiles"] = [{"name": "minimal"}]
    catalog = list_destinations(client)
    plan = build_plan(client=None, notebook_bytes=VALID_NB, namespace="research", cluster=client.cluster, profile="minimal")
    assert catalog["defaultQueue"] == plan.queue == "default"
    assert catalog["profiles"][0]["image"] == plan.summary()["submitterImage"] == "runtime:environment"


class CatalogClient:
    def __init__(self):
        self.calls = []
        self.cluster = cluster_doc()
        self.namespaces = {"items": [
            {"metadata": {"name": "default"}},
            {"metadata": {"name": "research", "labels": {"tau.azure.com/workspace": "true"}}},
        ]}
        self.queues = {"items": [{"metadata": {"name": "research-gpu"}}]}
        self.core = SimpleNamespace(list_namespace=self.list_namespaces)
        self.custom = SimpleNamespace(list_cluster_custom_object=self.list_clusters,
                                      list_namespaced_custom_object=self.list_queues)

    def list_namespaces(self, **kwargs):
        self.calls.append(("namespaces", kwargs))
        return self.namespaces

    def list_clusters(self, **kwargs):
        self.calls.append(("clusters", kwargs))
        return {"items": [self.cluster]}

    def list_queues(self, **kwargs):
        self.calls.append(("queues", kwargs))
        return self.queues


def test_catalog_resolves_same_profile_and_queue_as_preview():
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    catalog = list_destinations(client)
    assert catalog["namespace"] == "research"
    assert catalog["namespaces"][0]["tauEnabled"] is True
    assert catalog["defaultQueue"] == "research-gpu"
    assert catalog["queues"] == ["research-gpu"]
    for option in catalog["profiles"]:
        plan = build_plan(client=None, notebook_bytes=VALID_NB, namespace=catalog["namespace"],
                          cluster=client.cluster, profile=option["name"])
        assert option["queue"] == plan.queue
        assert option["workers"] == plan.summary()["workers"]
        assert str(option["cpusPerWorker"]) == plan.summary()["cpusPerWorker"][0]
        assert option["image"] == plan.summary()["submitterImage"]
    assert len(client.calls) == 3
    assert client.calls[-1][1]["namespace"] == "research"
    assert all(call[1]["limit"] <= 500 for call in client.calls)
    assert all(call[1]["_preload_content"] is False for call in client.calls)


def test_catalog_reports_limits_and_does_not_walk_continuations():
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    client.cluster["spec"]["workloadProfiles"] = [
        {"name": f"cpu-{index}", "workerCount": 1} for index in range(205)
    ]
    client.queues["metadata"] = {"continue": "more"}
    result = list_destinations(client, namespace="research")
    assert len(result["profiles"]) == 200
    assert result["limits"] == {"namespaces": 500, "profiles": 200, "queues": 500}
    assert any("profile" in warning.lower() for warning in result["warnings"])
    assert any("queue" in warning.lower() for warning in result["warnings"])
    assert len(client.calls) == 3


def test_missing_default_is_not_invented_as_a_queue():
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    client.queues = {"items": [{"metadata": {"name": "other"}}]}
    result = list_destinations(client)
    assert result["defaultQueue"] == "research-gpu"
    assert result["queues"] == ["other"]
    assert any("default" in warning.lower() for warning in result["warnings"])


def test_unreadable_queues_are_actionable_and_never_fabricated():
    from tau.jupyter.destinations import list_destinations

    client = CatalogClient()
    def forbidden(**kwargs):
        raise RuntimeError("forbidden")
    client.custom.list_namespaced_custom_object = forbidden
    result = list_destinations(client)
    assert result["queues"] == []
    assert any("permission" in warning.lower() for warning in result["warnings"])


def test_unknown_namespace_does_not_trigger_queue_read():
    from tau.jupyter.destinations import list_destinations
    from tau.jupyter.runs import ReadError

    client = CatalogClient()
    with pytest.raises(ReadError):
        list_destinations(client, namespace="not-visible")
    assert not any(name == "queues" for name, _ in client.calls)


def test_endpoint_is_registered_no_store_and_read_only():
    handler_class = getattr(server, "DestinationsHandler", None)
    assert handler_class is not None
    assert any(route.endswith("/destinations") and cls is handler_class
               for route, cls in server._handlers("/user/alice/taugrid/api"))
    class Handler(server._ClientMixin):
        current_user = "researcher"
        settings = {}
        def client(self):
            return CatalogClient()
        def get_argument(self, name, default=None):
            return default
        def set_header(self, name, value):
            assert (name, value) == ("Cache-Control", "no-store")
        def finish(self, value):
            self.result = value
    handler = Handler()
    asyncio.run(handler_class.get(handler))
    assert handler.result["namespace"] == "research"
