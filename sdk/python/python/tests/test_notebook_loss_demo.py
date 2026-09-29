# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import json
import math
from pathlib import Path

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
