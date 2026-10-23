# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local test for the eval RayJob driver.

Starts a local Ray cluster with 8 fake logical GPUs and replaces run_eval_vllm with a
stub, then checks that the driver runs main() in a task holding all 8 GPUs and that the
driver process itself never imports vllm or torch. Needs ray, not vllm or a GPU:
  python -m pytest examples/cobol-cpt-grpo/07-eval/test_driver.py -v
"""

import json
import os
import shutil
import sys

import pytest
import ray

EVAL_DIR = os.path.dirname(os.path.abspath(__file__))

STUB_RUN_EVAL = """\
import json
import os

import ray


def main():
    with open(os.environ["STUB_MARKER"], "w") as f:
        json.dump({"gpu_ids": ray.get_gpu_ids(), "pid": os.getpid()}, f)
"""


@pytest.fixture
def ray_cluster(tmp_path, monkeypatch):
    work = tmp_path / "work"
    work.mkdir()
    shutil.copy(os.path.join(EVAL_DIR, "driver.py"), work / "driver.py")
    (work / "run_eval_vllm.py").write_text(STUB_RUN_EVAL)
    marker = tmp_path / "marker.json"

    monkeypatch.delenv("TP_SIZE", raising=False)
    monkeypatch.syspath_prepend(str(work))
    sys.modules.pop("driver", None)
    ray.init(
        num_cpus=2,
        num_gpus=8,
        include_dashboard=False,
        runtime_env={
            "working_dir": str(work),
            "env_vars": {"STUB_MARKER": str(marker)},
        },
    )
    yield marker
    ray.shutdown()
    sys.modules.pop("driver", None)


def test_driver_runs_eval_on_gpu_task_without_gpu_imports(ray_cluster):
    assert "vllm" not in sys.modules
    assert "torch" not in sys.modules

    import driver

    driver.main()

    result = json.loads(ray_cluster.read_text())
    assert len(result["gpu_ids"]) == 8
    assert result["pid"] != os.getpid()
    assert "run_eval_vllm" not in sys.modules
    assert "vllm" not in sys.modules
    assert "torch" not in sys.modules
