# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""CPU-impossible A100 proof for the TauGrid GPU quickstart.

`Job Complete` proves nothing about a GPU: a workload that silently fell back to
CPU exits 0 too. This script is built so a CPU cannot produce its output. Three
independent gates must all pass, and each one fails the run if it does not:

1. Device identity  -- the training actor must see an A100 (compute capability
   8.0) with ~80 GB of VRAM, reported from inside the Ray worker rather than
   from the driver.
2. Tensor-core throughput -- a TF32 matmul must sustain MIN_TFLOPS. An A100
   does 150-300 TFLOP/s here; a server CPU does well under 5. The floor is set
   low enough to tolerate a shared or throttled GPU but far above anything a
   CPU can reach, so it excludes silent CPU fallback without being flaky.
3. On-device convergence -- every parameter and every batch is asserted to live
   on CUDA, and the loss must actually go down. A GPU that is present but
   unused would pass gates 1 and 2 and fail this one.

Evidence is printed to stdout as JSON (so `tau run logs` alone is sufficient)
and additionally written to the durable checkpoints directory when one is
mounted. Note that in the default quickstart `/data` is an emptyDir, so the
file lives only as long as the pod -- stdout is the durable copy.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import sys
import time
from pathlib import Path

import ray
import torch
import torch.nn as nn
from ray.train import ScalingConfig
from ray.train.torch import TorchTrainer, prepare_model

# A100 is compute capability 8.0. Kept as a tuple so the failure message can
# report what was actually found instead of just "wrong GPU".
EXPECTED_CAPABILITY = (8, 0)
EXPECTED_VRAM_GIB = 70.0  # A100 80GB reports ~79.2 GiB usable; allow margin.
MATMUL_DIM = 8192
MATMUL_ITERS = 30
MIN_TFLOPS = 20.0

STEPS = 400
BATCH = 512
FEATURES = 1024
HIDDEN = 2048
CLASSES = 64


def _under_torchrun() -> bool:
    """True when tau launched this script with torchrun instead of Ray Train.

    tau's RayJob template picks the launcher from the manifest: `workers > 1`
    runs one process that drives Ray Train across pods, while `workers: 1` with
    `compute.gpus > 1` runs `torchrun --nproc_per_node=<gpus>` so a single pod
    can use every GPU it was granted. torchrun sets RANK/LOCAL_RANK/WORLD_SIZE
    in each child, and Ray Train does not, so their presence is the signal.
    Without this the script would call ray.init() once per GPU process and
    start <gpus> competing TorchTrainer jobs.
    """
    return all(v in os.environ for v in ("RANK", "LOCAL_RANK", "WORLD_SIZE"))


def _local_rank() -> int:
    return int(os.environ.get("LOCAL_RANK", "0"))


def _is_primary() -> bool:
    """Only one process should emit the evidence block."""
    return int(os.environ.get("RANK", "0")) == 0


def _durable_dir() -> Path:
    """Prefer the durable checkpoints mount, fall back to /tmp.

    TAU_DURABLE_CHECKPOINTS_DIR is the contract tau injects; it is emptyDir in
    this quickstart, so treat the write as best-effort and never fail the run
    on it -- stdout carries the same payload.
    """
    for key in ("TAU_DURABLE_CHECKPOINTS_DIR", "TAU_CHECKPOINTS_DIR"):
        value = os.environ.get(key)
        if value:
            return Path(value) / "aks-gpu-quickstart"
    return Path("/tmp/aks-gpu-quickstart")


class Net(nn.Module):
    def __init__(self) -> None:
        super().__init__()
        self.body = nn.Sequential(
            nn.Linear(FEATURES, HIDDEN),
            nn.ReLU(),
            nn.Linear(HIDDEN, HIDDEN),
            nn.ReLU(),
            nn.Linear(HIDDEN, CLASSES),
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.body(x)


def train_loop(config: dict) -> None:
    """Runs inside the Ray Train worker -- this is where the GPU claim is made.

    The two gate helpers are nested rather than module-level on purpose. Ray
    ships this function to the worker with cloudpickle, which walks the module
    globals it references; a module-level measure_tflops drags
    torch.backends.cudnn into that graph and fails with
    "cannot pickle 'CudnnModule' object". Nesting keeps them out of the pickled
    global set entirely.

    They also cannot simply move to the driver: Ray runs the driver with
    num_gpus=0 and masks CUDA_VISIBLE_DEVICES, so torch.cuda.is_available() is
    False there. The worker is the process Ray actually grants the A100 to, so
    it is the only correct place to assert on the device.
    """
    def probe_device() -> dict:
        """Gate 1: assert we are on the GPU we think we are."""
        if not torch.cuda.is_available():
            raise SystemExit(
                "GATE 1 FAILED: torch.cuda.is_available() is False. The workload "
                "landed on a node without a usable GPU, or the image has no CUDA "
                "runtime."
            )

        index = torch.cuda.current_device()
        props = torch.cuda.get_device_properties(index)
        capability = (props.major, props.minor)
        vram_gib = props.total_memory / (1024**3)

        if capability != EXPECTED_CAPABILITY:
            raise SystemExit(
                f"GATE 1 FAILED: expected compute capability {EXPECTED_CAPABILITY} "
                f"(A100) but found {capability} on '{props.name}'. The pod was "
                "scheduled onto a different GPU SKU than the quickstart provisions."
            )
        if vram_gib < EXPECTED_VRAM_GIB:
            raise SystemExit(
                f"GATE 1 FAILED: expected >= {EXPECTED_VRAM_GIB} GiB of VRAM for an "
                f"A100 80GB but found {vram_gib:.1f} GiB on '{props.name}'."
            )

        return {
            "device_name": props.name,
            "compute_capability": f"{props.major}.{props.minor}",
            "vram_gib": round(vram_gib, 1),
            "multi_processor_count": props.multi_processor_count,
            "torch_version": torch.__version__,
            "cuda_runtime": torch.version.cuda,
            "driver_visible": torch.cuda.device_count(),
            "node": platform.node(),
        }


    def measure_tflops(device: torch.device) -> dict:
        """Gate 2: sustained TF32 matmul throughput, which a CPU cannot fake."""
        torch.backends.cuda.matmul.allow_tf32 = True
        torch.backends.cudnn.allow_tf32 = True

        a = torch.randn(MATMUL_DIM, MATMUL_DIM, device=device, dtype=torch.float32)
        b = torch.randn(MATMUL_DIM, MATMUL_DIM, device=device, dtype=torch.float32)

        # Warm up so we time steady-state compute rather than lazy CUDA module load
        # and autotuning, which would otherwise dominate a short measurement.
        for _ in range(5):
            _ = a @ b
        torch.cuda.synchronize()

        start = time.perf_counter()
        for _ in range(MATMUL_ITERS):
            _ = a @ b
        torch.cuda.synchronize()
        elapsed = time.perf_counter() - start

        flop = 2.0 * (MATMUL_DIM**3) * MATMUL_ITERS
        tflops = flop / elapsed / 1e12

        if tflops < MIN_TFLOPS:
            raise SystemExit(
                f"GATE 2 FAILED: sustained {tflops:.1f} TFLOP/s, below the "
                f"{MIN_TFLOPS} TFLOP/s floor. An A100 reaches 150-300 here, so "
                "this indicates CPU fallback or a severely degraded device."
            )

        return {
            "matmul_dim": MATMUL_DIM,
            "iterations": MATMUL_ITERS,
            "seconds": round(elapsed, 4),
            "tflops": round(tflops, 1),
            "floor_tflops": MIN_TFLOPS,
        }
    # Under torchrun each process owns exactly one GPU, selected by LOCAL_RANK;
    # without set_device every rank would default to cuda:0.
    if _under_torchrun():
        torch.cuda.set_device(_local_rank())
        device = torch.device(f"cuda:{_local_rank()}")
    else:
        device = torch.device("cuda")
    device_info = probe_device()
    throughput = measure_tflops(device)

    print("[gate 1] device: " + json.dumps(device_info), flush=True)
    print("[gate 2] throughput: " + json.dumps(throughput), flush=True)

    torch.manual_seed(0)
    if _under_torchrun():
        # Ray Train's prepare_model is a no-op outside a Ray Train worker, so
        # do the equivalent by hand: move to this rank's device and wrap in DDP.
        if not torch.distributed.is_initialized():
            torch.distributed.init_process_group(
                backend=os.environ.get("TAU_DIST_BACKEND", "nccl")
            )
        model = torch.nn.parallel.DistributedDataParallel(
            Net().to(device), device_ids=[_local_rank()]
        )
    else:
        model = prepare_model(Net())

    # Gate 3a: prepare_model must have actually moved the weights onto CUDA.
    for name, param in model.named_parameters():
        if param.device.type != "cuda":
            raise SystemExit(
                f"GATE 3 FAILED: parameter '{name}' is on {param.device}, not "
                "cuda. The model was never moved onto the GPU."
            )

    # A fixed random teacher gives a learnable signal, so a falling loss is
    # evidence of real optimisation rather than of collapsing to a constant.
    teacher = torch.randn(FEATURES, CLASSES, device=device)
    opt = torch.optim.AdamW(model.parameters(), lr=3e-4)
    loss_fn = nn.CrossEntropyLoss()

    history: list[dict] = []
    first_loss = None
    last_loss = None
    started = time.perf_counter()

    for step in range(1, STEPS + 1):
        x = torch.randn(BATCH, FEATURES, device=device)
        y = (x @ teacher).argmax(dim=1)

        # Gate 3b: assert the data path stayed on-device too.
        if x.device.type != "cuda" or y.device.type != "cuda":
            raise SystemExit(
                f"GATE 3 FAILED: batch tensors on {x.device}/{y.device}, not cuda."
            )

        logits = model(x)
        loss = loss_fn(logits, y)

        opt.zero_grad(set_to_none=True)
        loss.backward()
        opt.step()

        value = float(loss.item())
        if first_loss is None:
            first_loss = value
        last_loss = value

        if step % 25 == 0 or step == 1:
            accuracy = float((logits.argmax(dim=1) == y).float().mean().item())
            record = {"step": step, "loss": round(value, 4), "accuracy": round(accuracy, 4)}
            history.append(record)
            print(f"[train] {json.dumps(record)}", flush=True)

    torch.cuda.synchronize()
    duration = time.perf_counter() - started

    # Gate 3c: the run has to have actually learned something.
    if last_loss is None or first_loss is None or last_loss >= first_loss:
        raise SystemExit(
            f"GATE 3 FAILED: loss did not decrease ({first_loss} -> {last_loss}). "
            "The optimiser ran but the model did not train."
        )

    evidence = {
        "verdict": "PASS",
        "summary": (
            f"Trained {STEPS} steps on {device_info['device_name']} at "
            f"{throughput['tflops']} TFLOP/s; loss {first_loss:.4f} -> {last_loss:.4f}."
        ),
        "gate_1_device": device_info,
        "gate_2_throughput": throughput,
        "gate_3_training": {
            "steps": STEPS,
            "batch": BATCH,
            "first_loss": round(first_loss, 4),
            "final_loss": round(last_loss, 4),
            "seconds": round(duration, 2),
            "steps_per_second": round(STEPS / duration, 1),
            "history": history,
            "peak_vram_gib": round(torch.cuda.max_memory_allocated() / (1024**3), 2),
        },
        "tau": {
            "dist_backend": os.environ.get("TAU_DIST_BACKEND"),
            "num_workers": os.environ.get("TAU_NUM_WORKERS"),
        },
    }

    payload = json.dumps(evidence, indent=2)
    # stdout is the durable copy: `/data` is an emptyDir in this quickstart, so
    # anything written to disk dies with the pod.
    if not _is_primary():
        print(f"[rank {os.environ['RANK']}] gates passed", flush=True)
        return

    print("=== TAU-GPU-EVIDENCE-BEGIN ===", flush=True)
    print(payload, flush=True)
    print("=== TAU-GPU-EVIDENCE-END ===", flush=True)

    try:
        out_dir = _durable_dir()
        out_dir.mkdir(parents=True, exist_ok=True)
        target = out_dir / "gpu-evidence.json"
        target.write_text(payload)
        print(f"[artifact] wrote {target}", flush=True)
    except OSError as err:  # best-effort only; stdout already carries the proof
        print(f"[artifact] could not persist evidence file: {err}", flush=True)


def main() -> int:
    # tau's rendered entrypoint always passes these; accept and ignore them so
    # the script stays compatible with the managed workflow contract.
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", default=None)
    parser.add_argument("--smoke-pairs", type=int, default=0)
    parser.parse_args()

    if _under_torchrun():
        # torchrun already forked one process per GPU; Ray plays no part here.
        world = os.environ["WORLD_SIZE"]
        print(
            f"[torchrun] rank {os.environ['RANK']}/{world} "
            f"on local GPU {_local_rank()}",
            flush=True,
        )
        train_loop({})
        if torch.distributed.is_initialized():
            torch.distributed.destroy_process_group()
        if _is_primary():
            print("[done] all three gates passed", flush=True)
        return 0

    ray.init(address="auto", ignore_reinit_error=True)

    gpus = ray.cluster_resources().get("GPU", 0)
    print(f"[ray] cluster GPUs visible to scheduler: {gpus}", flush=True)
    if gpus < 1:
        raise SystemExit(
            "The Ray cluster advertises no GPUs. Check that the A100 node pool "
            "is Ready and that the NVIDIA device plugin is advertising "
            "nvidia.com/gpu."
        )

    workers = int(os.environ.get("TAU_NUM_WORKERS", "1"))
    trainer = TorchTrainer(
        train_loop,
        scaling_config=ScalingConfig(num_workers=workers, use_gpu=True),
    )
    trainer.fit()
    print("[done] all three gates passed", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
