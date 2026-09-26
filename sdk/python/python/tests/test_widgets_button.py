# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the panel's interactive Submit button (S005/S008 surface).

The widget tree is constructible without a frontend: ipywidgets objects are
plain Python until displayed. A click is simulated by invoking the same handler
the button's ``on_click`` registers, which is the exact path a user click drives.
"""

import tempfile
from pathlib import Path

import pytest

from tau._backend import KubernetesBackend
from tau._render import Profile, TAU_QUEUE_LABEL
from tau.widgets.kube import ClusterClient
from tau.widgets.panel import TauGridPanel
from tests.test_widgets_submit import FakeCustomApi, NB_BYTES, _cluster_doc


def _real_notebook(tmp_path: Path) -> str:
    """Write a real notebook file so submit() can read its bytes."""
    src = tmp_path / "analysis.ipynb"
    src.write_bytes(NB_BYTES)
    return str(src)


def _panel_with_button(cluster=None):
    fake = FakeCustomApi(cluster=cluster)
    panel = TauGridPanel(namespace="ray", run_name="demo",
                         client=ClusterClient(custom=fake, core=None))
    tree = panel.build()
    return panel, tree, fake


def test_build_creates_submit_button_with_expected_labels():
    panel, tree, _ = _panel_with_button()
    assert panel._controls is not None
    submit = panel._controls["submit"]
    assert submit.description == "Submit notebook"
    assert submit.button_style == "primary"
    assert submit.disabled is True  # no notebook path yet (S005)
    assert panel._controls["refresh"].description == "Refresh"
    assert "taugrid-panel" in panel._controls["chart"].value


def test_submit_button_enables_once_notebook_path_is_set(tmp_path):
    panel, tree, _ = _panel_with_button()
    assert panel._controls["submit"].disabled is True
    assert "notebook path not resolved" in panel._controls["status"].value

    panel.set_notebook_path(_real_notebook(tmp_path))
    assert panel._controls["submit"].disabled is False
    assert "analysis.ipynb" in panel._controls["status"].value


def test_clicking_submit_runs_the_full_chain(tmp_path):
    panel, tree, fake = _panel_with_button(cluster=_cluster_doc())
    panel.set_notebook_path(_real_notebook(tmp_path))
    with tempfile.TemporaryDirectory() as d:
        # The click handler creates its own staging dir via the panel's factory;
        # point that factory at this temp dir so cleanup is deterministic.
        panel._default_staging_dir = lambda: Path(d)
        # Invoke the same handler the button's on_click registered.
        handle = panel._on_submit_clicked()
        assert handle.name == "demo" and handle.kind == "RayJob"
        assert panel.staged.notebook_path.exists()
        staged_dir = panel.staged.notebook_path.parent
        assert "_tau_runner.py" in {p.name for p in staged_dir.iterdir()}
    body = fake.created[0]["body"]
    assert body["metadata"]["labels"][TAU_QUEUE_LABEL] == "research-gpu"
    # After submit the button stays enabled (re-submission is allowed) and the
    # status line switches into run view.
    assert panel._controls["submit"].disabled is False
    assert "demo" in panel._controls["status"].value


def test_clicking_submit_with_no_notebook_raises_instead_of_misrendering():
    panel, tree, fake = _panel_with_button()
    with pytest.raises(ValueError):
        panel._on_submit_clicked()
    assert fake.created == []


def test_refresh_button_repaints_status_and_chart(tmp_path):
    panel, tree, _ = _panel_with_button(cluster=_cluster_doc())
    panel.set_notebook_path(_real_notebook(tmp_path))
    before = panel._controls["status"].value
    panel._on_refresh_clicked()
    assert panel._controls["status"].value == before
    assert "taugrid-panel" in panel._controls["chart"].value


def test_button_flow_uses_caller_profile_and_queue(tmp_path):
    fake = FakeCustomApi()
    panel = TauGridPanel(namespace="ray", run_name="demo")
    panel.set_notebook_path(_real_notebook(tmp_path))
    panel.build()
    with tempfile.TemporaryDirectory() as d:
        handle = panel.submit(
            notebook=panel.notebook_path,
            profile=Profile(name="cpu", workers=1, gpus_per_worker=0),
            queue="cpu-queue",
            staging_dir=Path(d),
            backend=KubernetesBackend(custom_objects=fake),
        )
        assert handle.name == "demo"
    body = fake.created[0]["body"]
    assert body["metadata"]["labels"][TAU_QUEUE_LABEL] == "cpu-queue"