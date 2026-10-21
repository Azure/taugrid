# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local test for the GRPO RayJob driver.

Runs the driver on a local Ray cluster with 8 logical GPUs. A tiny HTTP server stands
in for `trl vllm-serve` and a stub stands in for train_grpo on 2 CPU workers (gloo).
Checks the health wait, the fast failure when the server or its actor dies before it
is ready, the cleanup of the actor and its subprocess, the env each trainer worker sees,
and that the driver process never imported trl, vllm or transformers. Needs ray[train]
and CPU torch, not trl or a GPU:
  python -m pytest examples/cobol-cpt-grpo/06-grpo/test_driver.py -v
"""

import importlib.util
import json
import os
import shutil
import socket
import sys
import time

import pytest
import ray

RL_DIR = os.path.dirname(os.path.abspath(__file__))

FAKE_SERVER = """\
import http.server
import json
import os
import signal
import sys
import time

port, delay, mode, out = int(sys.argv[1]), float(sys.argv[2]), sys.argv[3], sys.argv[4]
with open(out, "w") as f:
    json.dump({"pid": os.getpid(), "cuda": os.environ.get("CUDA_VISIBLE_DEVICES")}, f)
time.sleep(delay)
if mode == "kill-parent":
    os.kill(os.getppid(), signal.SIGKILL)
    sys.exit(0)
if mode.startswith("exit-"):
    sys.exit(int(mode[len("exit-"):]))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == "/health" else 404)
        self.end_headers()

    def log_message(self, *args):
        pass


http.server.HTTPServer(("0.0.0.0", port), Handler).serve_forever()
"""

STUB_TRAIN_GRPO = """\
import json
import os
import urllib.request

import torch
import torch.distributed as dist
import transformers  # noqa: F401
import trl  # noqa: F401
import vllm  # noqa: F401


def main():
    t = torch.ones(1)
    dist.all_reduce(t)
    url = os.environ["VLLM_SERVER_URL"]
    with urllib.request.urlopen(url + "/health", timeout=5) as resp:
        health = resp.status
    rank = os.environ["RANK"]
    with open(os.path.join(os.environ["STUB_OUT"], f"rank-{rank}.json"), "w") as f:
        json.dump({
            "rank": int(rank),
            "local_rank": int(os.environ["LOCAL_RANK"]),
            "world_size": int(os.environ["WORLD_SIZE"]),
            "master_addr": os.environ.get("MASTER_ADDR"),
            "master_port": os.environ.get("MASTER_PORT"),
            "vllm_server_url": url,
            "health": health,
            "dist_initialized": dist.is_initialized(),
            "backend": dist.get_backend(),
            "all_reduce": t.item(),
            "pid": os.getpid(),
        }, f)
"""

FAKE_MODULES = ("trl", "vllm", "transformers")


def _free_port():
    with socket.socket() as s:
        s.bind(("", 0))
        return s.getsockname()[1]


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


@pytest.fixture
def work_dir(tmp_path, monkeypatch):
    work = tmp_path / "work"
    work.mkdir()
    shutil.copy(os.path.join(RL_DIR, "driver.py"), work / "driver.py")
    (work / "train_grpo.py").write_text(STUB_TRAIN_GRPO)
    (work / "fake_server.py").write_text(FAKE_SERVER)
    for name in FAKE_MODULES:
        (work / f"{name}.py").write_text("")
    monkeypatch.syspath_prepend(str(work))
    for name in ("driver", "train_grpo", *FAKE_MODULES):
        monkeypatch.delitem(sys.modules, name, raising=False)
    return work


@pytest.fixture
def ray_cluster(work_dir, tmp_path):
    out = tmp_path / "out"
    out.mkdir()
    ray.init(
        num_cpus=4,
        num_gpus=8,
        include_dashboard=False,
        runtime_env={"working_dir": str(work_dir), "env_vars": {"STUB_OUT": str(out)}},
    )
    try:
        yield out
    finally:
        ray.shutdown()


def _server_cmd(work_dir, port, delay, mode, record):
    return [
        sys.executable,
        str(work_dir / "fake_server.py"),
        str(port),
        str(delay),
        mode,
        str(record),
    ]


def _assert_cleaned_up(record):
    import driver

    with pytest.raises(ValueError):
        ray.get_actor(driver.SERVER_ACTOR_NAME)
    pid = json.loads(record.read_text())["pid"]
    deadline = time.monotonic() + 10
    while _pid_alive(pid) and time.monotonic() < deadline:
        time.sleep(0.2)
    assert not _pid_alive(pid)


def test_defaults_match_plan(work_dir, monkeypatch):
    env = {
        "MODEL_ID": "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt/final",
        "MAX_PROMPT_LEN": "3072",
        "MAX_GEN_LEN": "1024",
        "OUTPUT_DIR": "/data/projects/taugrid-default/runs/cobol-cpt-grpo/grpo",
    }
    for key in ("VLLM_PORT", "TP_SIZE", "GPU_MEM_UTIL"):
        monkeypatch.delenv(key, raising=False)
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    import driver

    assert driver.vllm_serve_command() == [
        "trl",
        "vllm-serve",
        "--model",
        "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt/final",
        "--port",
        "8000",
        "--dtype",
        "bfloat16",
        "--tensor-parallel-size",
        "8",
        "--gpu-memory-utilization",
        "0.90",
        "--max-model-len",
        "4352",
        "--trust-remote-code",
    ]
    assert driver.VLLMServer._default_options["num_gpus"] == 8
    assert driver.HEALTH_TIMEOUT_S == 900

    trainer = driver.build_trainer("http://10.0.0.1:8000")
    assert trainer.scaling_config.num_workers == 8
    assert trainer.scaling_config.use_gpu is True
    assert (
        trainer.run_config.storage_path
        == "/data/projects/taugrid-default/runs/cobol-cpt-grpo/grpo/ray"
    )
    assert trainer.train_loop_config == {"vllm_server_url": "http://10.0.0.1:8000"}


def test_run_waits_for_health_trains_and_cleans_up(work_dir, ray_cluster, tmp_path):
    import driver

    port = _free_port()
    record = tmp_path / "server.json"
    driver.run(
        server_cmd=_server_cmd(work_dir, port, 3, "serve", record),
        port=port,
        num_workers=2,
        use_gpu=False,
        storage_path=str(tmp_path / "ray"),
    )

    server = json.loads(record.read_text())
    assert len(server["cuda"].split(",")) == 8

    results = sorted(
        (json.loads(p.read_text()) for p in ray_cluster.glob("rank-*.json")),
        key=lambda r: r["rank"],
    )
    assert [r["rank"] for r in results] == [0, 1]
    for r in results:
        assert r["world_size"] == 2
        assert r["dist_initialized"] is True
        assert r["backend"] == "gloo"
        assert r["all_reduce"] == 2.0
        assert r["master_addr"] and r["master_port"]
        assert r["vllm_server_url"] == f"http://{ray.util.get_node_ip_address()}:{port}"
        assert not r["vllm_server_url"].endswith("/v1")
        assert r["health"] == 200
        assert r["pid"] != os.getpid()
    assert sorted(r["local_rank"] for r in results) == [0, 1]

    _assert_cleaned_up(record)

    for name in FAKE_MODULES:
        assert importlib.util.find_spec(name) is not None
    for name in ("train_grpo", *FAKE_MODULES):
        assert name not in sys.modules


@pytest.mark.parametrize(
    ("mode", "error"),
    [
        ("exit-3", RuntimeError),
        ("kill-parent", ray.exceptions.RayActorError),
    ],
)
def test_run_fails_fast_when_server_dies(work_dir, ray_cluster, tmp_path, mode, error):
    import driver

    port = _free_port()
    record = tmp_path / "server.json"
    start = time.monotonic()
    with pytest.raises(error):
        driver.run(
            server_cmd=_server_cmd(work_dir, port, 1, mode, record),
            port=port,
            num_workers=2,
            use_gpu=False,
            storage_path=str(tmp_path / "ray"),
        )
    assert time.monotonic() - start < 60
    assert not list(ray_cluster.glob("rank-*.json"))
    _assert_cleaned_up(record)
