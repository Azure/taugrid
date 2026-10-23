# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import io
import json
import math
import time
from pathlib import Path
from types import SimpleNamespace

import pytest


@pytest.fixture
def demo():
    path = Path(__file__).resolve().parents[4] / "tools" / "run-cpu-ray-demo.py"
    spec = importlib.util.spec_from_file_location("notebook_loss_demo", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def prepare(demo, monkeypatch, samples, *, gpus=0, state="complete", source_uid="run"):
    calls = []
    plan = {"name": "reviewed", "namespace": "ray", "profile": "cpu",
            "gpusPerWorker": [gpus], "submissionMode": "K8sJobMode", "retentionSeconds": 600, "planDigest": "a" * 64}

    def call(server, token, path, **kwargs):
        calls.append((path, kwargs))
        if path == "capabilities":
            return {"submissionEnabled": True}
        if path == "preview":
            return {"submittable": True, "submissionEnabled": True, "plan": plan}
        if path == "submit":
            return {"submitted": True, "name": "reviewed", "namespace": "ray", "kind": "RayJob"}
        if path == "status":
            return {"uid": "run", "terminal": True, "state": state, "metrics": {
                "source": {"type": "stdout", "runUid": source_uid, "pod": "driver", "container": "submitter"},
                "checkedAt": "2026-09-22T00:00:00Z", "samples": samples}}
        if path == "logs":
            return {"text": "step=1 loss=2\n", "possiblyTruncated": True}
        raise AssertionError(path)

    monkeypatch.setattr(demo, "call", call)
    return calls


@pytest.mark.parametrize("samples", [[], [{"step": 1, "value": math.nan}],
                                     [{"step": -1, "value": 2}], [{"step": 1, "value": True}]])
def test_success_without_real_finite_loss_fails(demo, monkeypatch, samples):
    calls = prepare(demo, monkeypatch, samples)
    assert demo.main(["--token", "test", "--profile", "cpu"]) == 1
    assert [path for path, _ in calls].count("submit") == 1


def test_loss_success_submits_once_and_polls_opt_in(demo, monkeypatch):
    calls = prepare(demo, monkeypatch, [{"step": 1, "value": 2}])
    assert demo.main(["--token", "test", "--profile", "cpu", "--queue", "cpu-q"]) == 0
    assert [path for path, _ in calls] == ["capabilities", "preview", "submit", "status", "logs"]
    reviewed = calls[1][1]["body"]
    submitted = calls[2][1]["body"]
    assert submitted["planDigest"] == "a" * 64
    assert submitted["notebook"] == reviewed["notebook"]
    assert submitted["profile"] == "cpu" and submitted["queue"] == "cpu-q"
    assert submitted["name"] == "reviewed" and submitted["confirm"] is True
    assert calls[3][1]["params"]["includeMetrics"] == "true"


@pytest.mark.parametrize("options", [{"gpus": 1}, {"state": "failed"}, {"source_uid": "other"}])
def test_gpu_failed_or_replaced_run_cannot_pass(demo, monkeypatch, options):
    calls = prepare(demo, monkeypatch, [{"step": 1, "value": 2}], **options)
    assert demo.main(["--token", "test", "--profile", "cpu"]) == 1
    if options.get("gpus"):
        assert "submit" not in [path for path, _ in calls]


def test_demo_notebook_is_deterministic_and_cpu_only(demo):
    notebook = json.loads(demo.NOTEBOOK.read_text(encoding="utf-8"))
    source = "".join(notebook["cells"][1]["source"])
    assert "step=" in source and "loss=" in source and "flush=True" in source
    assert "import time" in source
    assert "random" not in source and "torch" not in source


class LegacyHTTPResponse:
    """urllib3 1.26 shape: no ``read1`` on the response itself, only on ``_fp``."""

    def __init__(self, data, status=200):
        self.status = status
        self._body = io.BytesIO(data)
        self.closed = False
        self._fp = SimpleNamespace(read1=self._body.read1)

    def set_read_timeout(self, timeout):
        assert 0 < timeout

    def close(self):
        self.closed = True
        self._body.close()


class LegacyPoolManager:
    """Transport double so the real ``call`` path runs, not a replaced stub."""

    def __init__(self, response):
        self.response = response
        self.requests = []

    def request(self, method, url, **kwargs):
        self.requests.append((method, url, kwargs))
        return self.response

    def clear(self):
        pass


def _legacy_call(demo, monkeypatch, response):
    manager = LegacyPoolManager(response)
    monkeypatch.setattr(demo.urllib3, "PoolManager", lambda: manager)
    result = demo.call("http://127.0.0.1:8888", "tok", "capabilities", deadline=time.monotonic() + 10)
    return manager, result


def test_call_decodes_a_legacy_response_without_response_read1(demo, monkeypatch):
    """The demo must reuse the shared reader: its old local loop called
    ``response.read1``, which urllib3 1.26 does not have, so the first API reply
    raised AttributeError on the supported floor while the floor tests passed."""
    response = LegacyHTTPResponse(json.dumps({"submissionEnabled": True}).encode())
    assert not hasattr(response, "read1"), "the floor response has no read1 to fall back on"

    manager, result = _legacy_call(demo, monkeypatch, response)

    assert result == {"submissionEnabled": True}
    assert response.closed
    _, _, kwargs = manager.requests[0]
    assert kwargs["preload_content"] is False
    assert kwargs["retries"] is False


def test_call_keeps_the_byte_ceiling_and_cleanup_on_the_shared_reader(demo, monkeypatch):
    response = LegacyHTTPResponse(b"x" * (demo.API_LIMIT + 1))
    monkeypatch.setattr(demo.urllib3, "PoolManager", lambda: LegacyPoolManager(response))

    with pytest.raises(ValueError, match="exceeded 1 MiB"):
        demo.call("http://127.0.0.1:8888", "tok", "capabilities", deadline=time.monotonic() + 10)

    assert response.closed
