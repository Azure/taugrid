# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""RayJob entrypoint for COBOL GRPO training.

TauGrid runs this file as the Ray driver on a CPU-only head pod, so it must not import
trl, vllm, transformers or the training module. The run uses the two GPU worker pods
of one Ray cluster:

1. A VLLMServer actor reserves all 8 GPUs of one worker and runs `trl vllm-serve`
   (TP=8) as a subprocess. The driver waits for
   http://<server node IP>:<VLLM_PORT>/health.
2. A Ray Train TorchTrainer starts 8 workers, one GPU each. The server holds every GPU
   of its node, so they land on the other worker. Each sets VLLM_SERVER_URL and calls
   train_grpo.main().

Ray Train initializes torch.distributed (NCCL on GPUs) and sets RANK, LOCAL_RANK,
WORLD_SIZE and MASTER_ADDR/PORT in every worker before train_fn runs, the same contract
torchrun gives. train_grpo is imported inside train_fn; TauGrid ships this
directory as the Ray working_dir, so the import resolves on the workers. Configuration
(MODEL_ID, OUTPUT_DIR, K_SAMPLES, MAX_STEPS, ...) comes from the pod env, which TauGrid
sets on the head and on every worker.

TRL's weight sync does not go through this driver: trainer rank 0 opens an NCCL group
on the group port (51216 in TRL 1.13.0) and the server's TP workers join it.

HF Trainer keeps its own checkpointing and resume under OUTPUT_DIR; Ray Train only
stores its run metadata under OUTPUT_DIR/ray. The server is stopped when training
returns or fails.
"""

import os
import signal
import subprocess
import time
import urllib.request

import ray
from ray.train import RunConfig, ScalingConfig
from ray.train.torch import TorchTrainer

SERVER_GPUS = 8
NUM_WORKERS = 8
HEALTH_TIMEOUT_S = 900
STOP_TIMEOUT_S = 30
SERVER_ACTOR_NAME = "vllm-server"


def vllm_serve_command():
    """The `trl vllm-serve` command line, built from the pod env."""
    model_id = os.environ.get(
        "MODEL_ID", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt/final"
    )
    port = int(os.environ.get("VLLM_PORT", "8000"))
    tp_size = int(os.environ.get("TP_SIZE", str(SERVER_GPUS)))
    max_prompt = int(os.environ.get("MAX_PROMPT_LEN", "3072"))
    max_gen = int(os.environ.get("MAX_GEN_LEN", "1024"))
    max_len = max_prompt + max_gen + 256
    return [
        "trl",
        "vllm-serve",
        "--model",
        model_id,
        "--port",
        str(port),
        "--dtype",
        "bfloat16",
        "--tensor-parallel-size",
        str(tp_size),
        "--gpu-memory-utilization",
        os.environ.get("GPU_MEM_UTIL", "0.90"),
        "--max-model-len",
        str(max_len),
        "--trust-remote-code",
    ]


@ray.remote(num_gpus=SERVER_GPUS)
class VLLMServer:
    """Owns the vLLM server subprocess on a GPU worker."""

    def __init__(self, cmd):
        self._cmd = cmd
        self._proc = None

    def start(self):
        print(f"Starting vLLM server: {' '.join(self._cmd)}", flush=True)
        self._proc = subprocess.Popen(self._cmd, start_new_session=True)
        return ray.util.get_node_ip_address()

    def returncode(self):
        return self._proc.poll() if self._proc else None

    def stop(self):
        if self._proc is None:
            return
        if self._proc.poll() is None:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=STOP_TIMEOUT_S)
            except subprocess.TimeoutExpired:
                pass
        try:
            os.killpg(self._proc.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        self._proc.wait()


def wait_for_health(server, base_url, timeout_s):
    """Poll <base_url>/health until it answers, the server exits, or timeout_s passes."""
    health_url = base_url + "/health"
    start = time.monotonic()
    next_log = 0
    while True:
        rc = ray.get(server.returncode.remote())
        if rc is not None:
            raise RuntimeError(
                f"vLLM server exited with code {rc} before {health_url} was ready"
            )
        try:
            with urllib.request.urlopen(health_url, timeout=3):
                print(f"vLLM ready at {base_url}", flush=True)
                return
        except OSError:
            pass
        elapsed = time.monotonic() - start
        if elapsed >= timeout_s:
            raise TimeoutError(f"vLLM at {health_url} not ready after {timeout_s:.0f}s")
        if elapsed >= next_log:
            print(f"  Waiting for vLLM at {health_url}... ({elapsed:.0f}s)", flush=True)
            next_log += 30
        time.sleep(1)


def stop_server(server):
    try:
        ray.get(server.stop.remote(), timeout=STOP_TIMEOUT_S + 30)
    except ray.exceptions.RayError as e:
        print(f"WARNING: stopping the vLLM server failed: {e}", flush=True)
    ray.kill(server)


def train_fn(config):
    os.environ["VLLM_SERVER_URL"] = config["vllm_server_url"]
    import train_grpo

    train_grpo.main()


def build_trainer(
    vllm_server_url, num_workers=NUM_WORKERS, use_gpu=True, storage_path=None
):
    if storage_path is None:
        output_dir = os.environ.get(
            "OUTPUT_DIR", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/grpo"
        )
        storage_path = os.path.join(output_dir, "ray")
    return TorchTrainer(
        train_loop_per_worker=train_fn,
        train_loop_config={"vllm_server_url": vllm_server_url},
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=use_gpu),
        run_config=RunConfig(storage_path=storage_path),
    )


def run(
    server_cmd=None,
    port=None,
    num_workers=NUM_WORKERS,
    use_gpu=True,
    storage_path=None,
    health_timeout_s=HEALTH_TIMEOUT_S,
):
    if server_cmd is None:
        server_cmd = vllm_serve_command()
    if port is None:
        port = int(os.environ.get("VLLM_PORT", "8000"))

    server = VLLMServer.options(name=SERVER_ACTOR_NAME).remote(server_cmd)
    try:
        deadline = time.monotonic() + health_timeout_s
        ip = ray.get(server.start.remote(), timeout=health_timeout_s)
        base_url = f"http://{ip}:{port}"
        wait_for_health(server, base_url, max(0.0, deadline - time.monotonic()))
        build_trainer(
            base_url,
            num_workers=num_workers,
            use_gpu=use_gpu,
            storage_path=storage_path,
        ).fit()
    finally:
        stop_server(server)


def main():
    ray.init(address="auto", ignore_reinit_error=True)
    run()


if __name__ == "__main__":
    main()
