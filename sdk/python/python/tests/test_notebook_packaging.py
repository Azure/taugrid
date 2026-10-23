# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Check wheel packaging refuses an absent or incomplete frontend."""
import json
import runpy
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest


@pytest.mark.parametrize("with_manifest", [False, True])
def test_packaging_refuses_missing_prebuilt_assets(tmp_path, monkeypatch, with_manifest):
    source = Path(__file__).parents[1] / "setup.py"
    copied = tmp_path / "setup.py"
    copied.write_text(source.read_text(encoding="utf-8"), encoding="utf-8")
    if with_manifest:
        folder = tmp_path / "tau" / "labextension"
        folder.mkdir(parents=True)
        (folder / "package.json").write_text(json.dumps({"jupyterlab": {"_build": {"load": "static/remoteEntry.missing.js"}}}), encoding="utf-8")
    monkeypatch.setitem(sys.modules, "setuptools", SimpleNamespace(setup=lambda **kwargs: None))
    with pytest.raises(RuntimeError, match="prebuilt"):
        runpy.run_path(str(copied))
