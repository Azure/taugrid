# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline request/confirmation regressions through the real handlers."""
import asyncio
import json
from types import SimpleNamespace

import pytest
from tornado.web import HTTPError

from tau.jupyter import server
from tests.test_jupyter_submit import FakeClient, FakeCustom, VALID_NB, cluster_doc


class Handler(server._SubmitMixin):
    current_user = "researcher"

    def __init__(self, body, client):
        self.request = SimpleNamespace(body=json.dumps(body).encode())
        self._client = client
        self.result = None

    def client(self):
        return self._client

    def finish(self, result):
        self.result = result


def body():
    return {"notebook": VALID_NB.decode(), "namespace": "ray", "name": "reviewed"}


@pytest.mark.parametrize("key,value", [("namespace", []), ("name", 42), ("profile", {}),
    ("queue", False), ("pip", "requests"), ("pip", [7]), ("env", []),
    ("env", {"TOKEN": 2}), ("env_secret", {"TOKEN": []})])
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
