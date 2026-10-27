# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline request/confirmation regressions through the real handlers."""
import asyncio
import json
from types import SimpleNamespace

import pytest
from jupyter_server.auth import AllowAllAuthorizer
from tornado.web import HTTPError

from tau.jupyter import server
from tests.cluster_fixtures import ready_cluster
from tests.test_jupyter_submit import FakeClient, FakeCustom, VALID_NB, cluster_doc, team_cluster


class Handler(server._SubmitMixin):
    current_user = "researcher"
    # Every handler now consults the Jupyter Authorizer; a permissive Authorizer
    # is the stock Jupyter configuration these lifecycle tests run under.
    authorizer = AllowAllAuthorizer()

    def __init__(self, body, client):
        self.request = SimpleNamespace(method="POST", body=json.dumps(body).encode())
        self._client = client
        self.result = None

    def client(self):
        return self._client

    def finish(self, result):
        self.result = result


def body():
    # The fixture cluster resolves one GPU profile, and a notebook always runs in
    # the control-only head, so the request must declare that GPU work is
    # dispatched through Ray. Otherwise render_rayjob refuses the submission.
    return {"notebook": VALID_NB.decode(), "namespace": "ray", "name": "reviewed",
            "gpuExecution": "ray"}


@pytest.mark.parametrize("key,value", [("namespace", []), ("name", 42), ("profile", {}),
    ("queue", False), ("pip", "requests"), ("pip", [7]), ("env", []),
    ("env", {"TOKEN": 2}), ("env_secret", {"TOKEN": []}), ("gpuExecution", 5)])
def test_preview_rejects_malformed_fields(key, value):
    handler = Handler({**body(), key: value}, FakeClient(FakeCustom(cluster=cluster_doc())))
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(handler))
    assert error.value.status_code == 400


def test_review_digest_required_and_drift_refused_before_create(monkeypatch):
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=cluster_doc())
    client = FakeClient(api)
    review = Handler(body(), client)
    asyncio.run(server.PreviewHandler.post(review))
    assert not api.created
    for digest in (None, "0" * 64):
        attempt = Handler({**body(), "confirm": True, "planDigest": digest}, client)
        with pytest.raises(HTTPError) as error:
            asyncio.run(server.SubmitHandler.post(attempt))
        assert error.value.status_code in (400, 409)
        assert not api.created
    digest = review.result["plan"]["planDigest"]
    monkeypatch.setenv("TAUGRID_RUNTIME_IMAGE", "example:drifted")
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.SubmitHandler.post(Handler({**body(), "confirm": True, "planDigest": digest}, client)))
    assert error.value.status_code == 409
    assert not api.created
    monkeypatch.delenv("TAUGRID_RUNTIME_IMAGE")
    attempt = Handler({**body(), "confirm": True, "planDigest": digest}, client)
    asyncio.run(server.SubmitHandler.post(attempt))
    assert attempt.result["submitted"] is True
    assert len(api.created) == 1


def test_disabled_submit_never_creates(monkeypatch):
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", False)
    api = FakeCustom(cluster=cluster_doc())
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.SubmitHandler.post(Handler({**body(), "confirm": True}, FakeClient(api))))
    assert error.value.status_code == 409
    assert not api.created


def test_preview_and_create_run_off_event_loop(monkeypatch):
    import threading
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=cluster_doc())
    client = FakeClient(api)
    handler = Handler(body(), client)
    threads = []
    def get_client():
        threads.append(threading.get_ident())
        return client
    handler.client = get_client
    asyncio.run(server.PreviewHandler.post(handler))
    handler.request.body = json.dumps({**body(), "confirm": True, "planDigest": handler.result["plan"]["planDigest"]}).encode()
    asyncio.run(server.SubmitHandler.post(handler))
    assert len(threads) == 3
    assert all(identity != threading.get_ident() for identity in threads)
    assert len(api.created) == 1


@pytest.mark.parametrize("fail", [False, True])
def test_request_owned_client_is_closed_even_after_failure(fail):
    closed = []
    client = SimpleNamespace(close=lambda: closed.append(True))
    handler = SimpleNamespace(client=lambda: client)
    def operation(client):
        if fail:
            raise ValueError("refused")
        return 42
    if fail:
        with pytest.raises(ValueError):
            server._call_with_client(handler, operation)
    else:
        assert server._call_with_client(handler, operation) == 42
    assert closed == [True]


class DenyingAuthorizer:
    """Denies every action unless its action name is in ``allowed``."""

    def __init__(self, allowed=()):
        self.allowed = set(allowed)
        self.calls = []

    def is_authorized(self, handler, user, action, resource):
        self.calls.append((action, resource))
        return action in self.allowed


class AuthorizedHandler(Handler):
    """Handler whose Authorizer decisions the test controls per instance."""


# -- C1: a caller scope that is not trusted must fail closed -----------------


def team_body(**overrides):
    base = {**body(), "profile": "team-only"}
    base.update(overrides)
    return base


@pytest.mark.parametrize("configured", [None, "beta"])
def test_team_scoped_profile_cannot_be_submitted_without_a_trusted_team(monkeypatch, configured):
    """An unknown or non-matching server team must not authorize the profile.

    C1's defect was that an unknown caller scope was left unfiltered and the
    submitting team was then derived from the profile being authorized, so the
    profile authorized itself. Here the scope comes only from server config.
    """
    if configured is None:
        monkeypatch.delenv(server.TEAM_ENV, raising=False)
    else:
        monkeypatch.setenv(server.TEAM_ENV, configured)
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=team_cluster())
    client = FakeClient(api)

    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(Handler(team_body(), client)))
    assert error.value.status_code == 400
    assert "team" in str(error.value.log_message)

    digest = "0" * 64
    with pytest.raises(HTTPError) as error:
        asyncio.run(
            server.SubmitHandler.post(
                Handler(team_body(confirm=True, planDigest=digest), client)
            )
        )
    assert error.value.status_code in (400, 409)
    assert api.created == []


def test_a_matching_trusted_team_authorizes_list_and_submit(monkeypatch):
    monkeypatch.setenv(server.TEAM_ENV, "alpha")
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=team_cluster())
    client = FakeClient(api)

    review = Handler(team_body(), client)
    asyncio.run(server.PreviewHandler.post(review))
    assert review.result["plan"]["profile"] == "team-only"

    attempt = Handler(
        team_body(confirm=True, planDigest=review.result["plan"]["planDigest"]), client
    )
    asyncio.run(server.SubmitHandler.post(attempt))
    assert attempt.result["submitted"] is True
    assert len(api.created) == 1


@pytest.mark.parametrize("field", ["team", "lane"])
def test_request_body_cannot_supply_an_authorization_scope(field):
    """team/lane are server configuration; a request that names one is refused."""
    handler = Handler({**body(), field: "alpha"}, FakeClient(FakeCustom(cluster=cluster_doc())))
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(handler))
    assert error.value.status_code == 400
    assert "cannot be set by a request" in str(error.value.log_message)


# -- C1: namespace applicability must not be authorized by the request -------


def namespace_cluster(visible_namespace):
    """A catalog whose only profile reserves capacity for one namespace."""
    return ready_cluster(
        [
            {"name": "ns-only", "compute": {"workerReplicas": 1, "gpusPerWorker": 0},
             "applicability": {"namespaces": [visible_namespace]}},
        ]
    )


@pytest.mark.parametrize("endpoint", ["preview", "submit"])
def test_namespace_scoped_profile_is_not_authorized_by_an_invisible_namespace(monkeypatch, endpoint):
    """C1: a caller cannot authorize a namespace-scoped profile by naming its namespace.

    The namespace is a request value, so it is verified against the namespaces
    this server can actually list before a profile may scope on it. The profile
    below authorizes ``victim-team-ns``; the fake server identity cannot see it,
    so preview and submit must both refuse instead of resolving a plan from the
    request value. This is the same fail-closed property team and lane already
    have when no trusted value is configured.
    """
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=namespace_cluster("victim-team-ns"))
    client = FakeClient(api)
    handler = Handler(
        {**body(), "namespace": "victim-team-ns", "confirm": True, "planDigest": "0" * 64},
        client,
    )
    call = server.PreviewHandler.post if endpoint == "preview" else server.SubmitHandler.post
    with pytest.raises(HTTPError) as error:
        asyncio.run(call(handler))
    assert error.value.status_code == 403
    assert "not in the namespaces this server can see" in str(error.value.log_message)
    assert api.created == []


def test_namespace_scoped_profile_is_offered_for_a_visible_namespace(monkeypatch):
    """The same profile is usable where the server can really see the namespace."""
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    client = FakeClient(FakeCustom(cluster=namespace_cluster("ray")))
    handler = Handler({**body(), "profile": "ns-only"}, client)
    asyncio.run(server.PreviewHandler.post(handler))
    assert handler.result["plan"]["profile"] == "ns-only"
    assert handler.result["plan"]["namespace"] == "ray"


# -- C2: the Jupyter Authorizer must be consulted, not just authentication ---


def test_denying_authorizer_refuses_reads_and_submission(monkeypatch):
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=cluster_doc())
    client = FakeClient(api)

    read = AuthorizedHandler(body(), client)
    read.authorizer = DenyingAuthorizer()
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(read))
    assert error.value.status_code == 403
    # The namespace is part of the Authorizer resource, not hidden behind the
    # coarse "taugrid" string.
    assert read.authorizer.calls == [("read", "taugrid:ray")]

    submit = AuthorizedHandler(
        {**body(), "confirm": True, "planDigest": "0" * 64}, client
    )
    submit.authorizer = DenyingAuthorizer()
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.SubmitHandler.post(submit))
    assert error.value.status_code == 403
    assert submit.authorizer.calls == [("execute", "taugrid:ray")]
    assert api.created == []


def test_execute_only_authorizer_refuses_reads_and_allows_submit(monkeypatch):
    monkeypatch.setenv(server.TEAM_ENV, "alpha")
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    api = FakeCustom(cluster=team_cluster())
    client = FakeClient(api)

    read = AuthorizedHandler(team_body(), client)
    read.authorizer = DenyingAuthorizer(allowed=("execute",))
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(read))
    assert error.value.status_code == 403

    submit = AuthorizedHandler(team_body(), client)
    submit.authorizer = DenyingAuthorizer(allowed=("execute",))
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.SubmitHandler.post(submit))
    # Authorized for execute, but the missing planDigest is a 400, not a 403.
    assert error.value.status_code == 400
    assert submit.authorizer.calls == [("execute", "taugrid:ray")]
    assert api.created == []


def test_anonymous_caller_is_refused_before_any_cluster_read():
    class Anonymous(Handler):
        current_user = None
        authorizer = AllowAllAuthorizer()

    handler = Anonymous(body(), FakeClient(FakeCustom(cluster=cluster_doc())))
    with pytest.raises(HTTPError) as error:
        asyncio.run(server.PreviewHandler.post(handler))
    assert error.value.status_code == 403


# -- F6: the gate cannot be enabled pointed at a runtime without the executor --


class FakeServerApp:
    class log:
        @staticmethod
        def info(*args, **kwargs):
            pass

    class web_app:
        settings = {"base_url": "/"}
        added = []

        @classmethod
        def add_handlers(cls, *args, **kwargs):
            cls.added.append((args, kwargs))


def test_enabling_submission_without_a_runtime_image_fails_at_startup(monkeypatch):
    """The gate must not enable the base ray image as the notebook runtime.

    The renderer's built-in default ships ray but not nbformat/nbconvert, so a
    misconfigured operator would get a preview advertising an image whose runner
    exits 3 on every run. Requiring TAUGRID_RUNTIME_IMAGE at load turns that into
    a startup failure with a clear message.
    """
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", True)
    monkeypatch.delenv(server.RUNTIME_IMAGE_ENV, raising=False)

    problem = server.submission_config_error()
    assert problem is not None and server.RUNTIME_IMAGE_ENV in problem
    with pytest.raises(RuntimeError, match="TAUGRID_RUNTIME_IMAGE"):
        server._load_jupyter_server_extension(FakeServerApp)
    assert FakeServerApp.web_app.added == []

    monkeypatch.setenv(server.RUNTIME_IMAGE_ENV, "taugrid-notebook-runtime:local")
    assert server.submission_config_error() is None
    server._load_jupyter_server_extension(FakeServerApp)
    assert FakeServerApp.web_app.added

    # With the gate off, no runtime image is required.
    FakeServerApp.web_app.added.clear()
    monkeypatch.setattr(server, "SUBMISSION_ENABLED", False)
    monkeypatch.delenv(server.RUNTIME_IMAGE_ENV, raising=False)
    assert server.submission_config_error() is None
