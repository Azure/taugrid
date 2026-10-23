# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local test for the CPT RayJob driver.

Runs the driver's TorchTrainer on a local Ray cluster with 2 CPU workers (gloo) and a
stub continued_pretrain, then checks that every worker ran main() with the torchrun-style
env and an initialized process group, and that the driver process itself never imported
transformers or deepspeed. Needs ray[train] and CPU torch, not transformers or a GPU:
  python -m pytest examples/cobol-cpt-grpo/04-cpt/test_driver.py -v
"""

import importlib.util
import json
import os
import shutil
import sys

import pytest
import ray

CPT_DIR = os.path.dirname(os.path.abspath(__file__))

STUB_CONTINUED_PRETRAIN = """\
import json
import os

import deepspeed  # noqa: F401
import torch
import torch.distributed as dist
import transformers  # noqa: F401


def main():
    t = torch.ones(1)
    dist.all_reduce(t)
    rank = os.environ["RANK"]
    with open(os.path.join(os.environ["STUB_OUT"], f"rank-{rank}.json"), "w") as f:
        json.dump({
            "rank": int(rank),
            "local_rank": int(os.environ["LOCAL_RANK"]),
            "world_size": int(os.environ["WORLD_SIZE"]),
            "master_addr": os.environ.get("MASTER_ADDR"),
            "master_port": os.environ.get("MASTER_PORT"),
            "dist_initialized": dist.is_initialized(),
            "dist_rank": dist.get_rank(),
            "dist_world_size": dist.get_world_size(),
            "backend": dist.get_backend(),
            "all_reduce": t.item(),
            "pid": os.getpid(),
        }, f)
"""


@pytest.fixture
def work_dir(tmp_path, monkeypatch):
    work = tmp_path / "work"
    work.mkdir()
    shutil.copy(os.path.join(CPT_DIR, "driver.py"), work / "driver.py")
    (work / "continued_pretrain.py").write_text(STUB_CONTINUED_PRETRAIN)
    (work / "transformers.py").write_text("")
    (work / "deepspeed.py").write_text("")
    monkeypatch.syspath_prepend(str(work))
    for name in ("driver", "continued_pretrain", "transformers", "deepspeed"):
        monkeypatch.delitem(sys.modules, name, raising=False)
    return work


def test_build_trainer_defaults_match_plan(work_dir, monkeypatch):
    monkeypatch.setenv(
        "OUTPUT_DIR", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt"
    )
    import driver

    trainer = driver.build_trainer()
    scaling = trainer.scaling_config
    assert scaling.num_workers == 16
    assert scaling.use_gpu is True
    assert scaling.resources_per_worker == {"CPU": 6, "GPU": 1}
    assert (
        trainer.run_config.storage_path
        == "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt/ray"
    )


def test_driver_runs_main_on_every_worker_without_hf_imports(work_dir, tmp_path):
    out = tmp_path / "out"
    out.mkdir()
    ray.init(
        num_cpus=4,
        include_dashboard=False,
        runtime_env={"working_dir": str(work_dir), "env_vars": {"STUB_OUT": str(out)}},
    )
    try:
        import driver

        driver.build_trainer(
            num_workers=2,
            use_gpu=False,
            cpus_per_worker=1,
            storage_path=str(tmp_path / "ray"),
        ).fit()
    finally:
        ray.shutdown()

    results = sorted(
        (json.loads(p.read_text()) for p in out.glob("rank-*.json")),
        key=lambda r: r["rank"],
    )
    assert [r["rank"] for r in results] == [0, 1]
    for r in results:
        assert r["world_size"] == 2
        assert r["dist_initialized"] is True
        assert r["dist_rank"] == r["rank"]
        assert r["dist_world_size"] == 2
        assert r["backend"] == "gloo"
        assert r["all_reduce"] == 2.0
        assert r["master_addr"] and r["master_port"]
        assert r["pid"] != os.getpid()
    assert sorted(r["local_rank"] for r in results) == [0, 1]

    assert importlib.util.find_spec("transformers") is not None
    assert importlib.util.find_spec("deepspeed") is not None
    for name in ("continued_pretrain", "transformers", "deepspeed"):
        assert name not in sys.modules
