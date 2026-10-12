# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline destination discovery: no cluster or server process."""

import asyncio
from types import SimpleNamespace

import pytest
from jupyter_server.auth import AllowAllAuthorizer
from tornado.web import HTTPError

from tau.jupyter import server
from tau.jupyter.submit import build_plan
from tests.test_jupyter_submit import VALID_NB, cluster_doc, team_cluster
from tests.cluster_fixtures import ready_cluster


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
def test_profile_overrides_readiness_and_resolved_queue_matches_plan(nested, monkeypatch):
    from tau.jupyter.destinations import list_destinations

    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "runtime:environment")
    client = CatalogClient()
    compute = {"gpusPerWorker": 2, "cpusPerWorker": 7.5, "memoryPerWorker": "24Gi", "image": "runtime:profile"}
    profile = {"name": "gpu", "description": "Two GPU workers", "priorityClass": "research-high"}
    profile.update({"compute": {**compute, "workerReplicas": 2}} if nested else {**compute, "workerCount": 2})
    # "pending" is declared but not ready, so the resolved catalog withholds it.
    client.cluster = ready_cluster(
        [profile, {"name": "pending", "ready": False}], default_queue="research-gpu"
    )
    client.queues = {"items": [{"metadata": {"name": "alternate"}}]}
    catalog = list_destinations(client, "default")
    plan = build_plan(client=None, notebook_bytes=VALID_NB, namespace="default", cluster=client.cluster,
                      profile="gpu", gpu_execution="ray")
    summary = plan.summary()
    assert len(catalog["profiles"]) == 1
    option = catalog["profiles"][0]
    assert option["workers"] == summary["workers"] == 2
    assert option["gpusPerWorker"] == summary["gpusPerWorker"][0] == 2
    assert option["cpusPerWorker"] == summary["cpusPerWorker"][0] == "7.5"
    assert option["memoryPerWorker"] == summary["memoryPerWorker"][0] == "24Gi"
    assert option["image"] == summary["submitterImage"] == "runtime:profile"
    assert option["priority"] == "research-high"
    # The notebook's own kernel never receives a GPU, so the row says so and
    # flags that GPU work must be dispatched through Ray.
    assert option["notebookKernelGpu"] is False
    assert option["requiresRayDispatch"] is True
    # The catalog's per-profile row and the plan resolve the same queue; the
    # profile owns the queue, so a visible but non-resolved LocalQueue ("alternate")
    # is not selectable as an override.
    assert option["queue"] == summary["queue"] == "research-gpu"
    assert catalog["defaultQueue"] == summary["queue"]
    assert client.calls[-1][1]["namespace"] == "default"


def test_profile_fallbacks_match_build_plan(monkeypatch):
    from tau.jupyter.destinations import list_destinations

    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "runtime:environment")
    client = CatalogClient()
    # No workspaceDefaults: the queue falls back to the constant default.
    client.cluster = ready_cluster([{"name": "minimal"}])
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
        plan = build_plan(
            client=None, notebook_bytes=VALID_NB, namespace=catalog["namespace"],
            cluster=client.cluster, profile=option["name"],
            gpu_execution="ray" if option["gpusPerWorker"] else "local",
        )
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
    client.cluster = ready_cluster(
        [{"name": f"cpu-{index}", "workerCount": 1} for index in range(205)]
    )
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
        authorizer = _RecordingAllowAll()
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
    # The success path authorizes the coarse grant for the namespace listing and
    # then the resolved namespace before the catalog's profiles and queues.
    assert ("read", "taugrid:research") in handler.authorizer.calls


class _RecordingAllowAll:
    """Allows everything and records each (action, resource) decision."""

    def __init__(self):
        self.calls = []

    def is_authorized(self, handler, user, action, resource):
        self.calls.append((action, resource))
        return True


class _NamespaceDenyingAuthorizer:
    """Authenticates, grants the coarse resource, denies one namespaced resource."""

    def __init__(self, denied):
        self.denied = denied
        self.calls = []

    def is_authorized(self, handler, user, action, resource):
        self.calls.append((action, resource))
        return resource != self.denied


def test_omitted_namespace_is_authorized_as_the_resolved_namespace():
    """A request that omits ``namespace`` must authorize ``taugrid:<resolved>``.

    When the request names no namespace, discovery reads the first visible
    namespace's profiles and LocalQueues, so the coarse ``taugrid`` grant alone
    must not reach that read. This Authorizer grants everything except the
    resolved namespace: the request is refused before any queue is listed, which
    is only true if the resolved namespace was authorized.
    """
    client = CatalogClient()

    class Handler(server._ClientMixin):
        current_user = "researcher"
        settings = {}
        authorizer = _NamespaceDenyingAuthorizer("taugrid:research")

        def client(self):
            return client

        def get_argument(self, name, default=None):
            return default

        def set_header(self, name, value):
            raise AssertionError("a denied request must not reach the response")

        def finish(self, value):
            raise AssertionError("a denied request must not finish")

    handler = Handler()
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.DestinationsHandler.get(handler))
    assert error.value.status_code == 403
    assert ("read", "taugrid:research") in handler.authorizer.calls
    assert not any(name == "queues" for name, _ in client.calls)


# -- C1: listing must fail closed without a trusted team ---------------------
def test_team_scoped_profile_is_not_listed_without_a_trusted_team(monkeypatch):
    """The picker must not offer capacity the caller's trusted team cannot claim."""
    monkeypatch.delenv(server.TEAM_ENV, raising=False)
    client = CatalogClient()
    client.cluster = team_cluster()

    catalog = asyncio.run(_destinations(client))

    assert catalog["profiles"] == []
    assert any("team" in warning for warning in catalog["warnings"])
    # And once the server is configured for the profile's team, it appears.
    monkeypatch.setenv(server.TEAM_ENV, "alpha")
    catalog = asyncio.run(_destinations(client))
    assert [row["name"] for row in catalog["profiles"]] == ["team-only"]


async def _destinations(client):
    """Drive the real DestinationsHandler so the trusted scope is exercised."""

    class Handler(server._ClientMixin):
        current_user = "researcher"
        authorizer = AllowAllAuthorizer()
        settings = {}

        def client(self):
            return client

        def get_argument(self, name, default=None):
            return default

        def set_header(self, name, value):
            pass

        def finish(self, value):
            self.result = value

    handler = Handler()
    await server.DestinationsHandler.get(handler)
    return handler.result
