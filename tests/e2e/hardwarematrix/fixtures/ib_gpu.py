# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
from pathlib import Path
import re
import socket

import ray
from ray import train
from ray.train import ScalingConfig
from ray.train.torch import TorchTrainer

from runtime_contract import check_runtime

WORKERS = int(os.environ["MATRIX_WORKERS"])
IB_HCAS = [f"mlx5_ib{index}" for index in range(8)]


def require_ib_transport(logs):
    using = re.findall(r"NET/IB\s*:\s*Using[^\n]*", logs)
    if not using:
        raise RuntimeError("NCCL did not report active InfiniBand devices")
    for line in using:
        devices = re.findall(r"\[\d+\]([\w.-]+):(\d+)/(\w+)", line)
        if not devices or any(hca not in IB_HCAS or port != "1" or link != "IB"
                              for hca, port, link in devices):
            raise RuntimeError("NCCL selected an unapproved HCA or non-InfiniBand link")
    if not re.search(r"Using network IB\b", logs):
        raise RuntimeError("NCCL did not select the IB network")
    if re.search(r"NET/Socket\s*:\s*Using|Using network Socket\b", logs):
        raise RuntimeError("NCCL selected TCP instead of InfiniBand")


def require_ib_devices(root=Path("/sys/class/infiniband")):
    expected = "=" + ",".join(f"{hca}:1" for hca in IB_HCAS)
    if os.environ.get("NCCL_IB_HCA") != expected:
        raise RuntimeError("NCCL_IB_HCA must select the exact approved InfiniBand ports")
    ports = {}
    for hca in IB_HCAS:
        path = root / hca / "ports" / "1"
        state = {name: (path / name).read_text().strip()
                 for name in ("link_layer", "state", "phys_state")}
        if (state["link_layer"] != "InfiniBand" or state["state"] != "4: ACTIVE"
                or state["phys_state"] != "5: LinkUp"):
            raise RuntimeError(f"required InfiniBand port {hca}:1 is not ACTIVE/LinkUp: {state}")
        ports[hca + ":1"] = state
    return ports


def require_rank_results(records, workers):
    if len(records) != workers or sorted(item["rank"] for item in records) != list(range(workers)):
        raise RuntimeError("collective results do not contain every unique rank")
    if len({item["hostname"] for item in records}) != workers:
        raise RuntimeError("collective ranks did not execute in distinct worker Pods")
    expected = [float(index * workers + workers * (workers - 1) // 2) for index in range(16)]
    for item in records:
        if item["all_reduce"] != expected:
            raise RuntimeError(f"rank {item['rank']} returned an incorrect CUDA all_reduce")
        require_ib_transport(item["nccl_log"])


def collective_loop():
    import torch
    import torch.distributed as dist

    from runtime_contract import check_memlock, check_runtime

    packages = check_runtime()
    memlock = check_memlock()
    ib_ports = require_ib_devices()
    context = train.get_context()
    rank = context.get_world_rank()
    world_size = context.get_world_size()
    if world_size != WORKERS or dist.get_backend() != "nccl":
        raise RuntimeError(f"expected {WORKERS} NCCL ranks, got {world_size}/{dist.get_backend()}")
    if os.environ.get("NCCL_NET") != "IB" or os.environ.get("NCCL_IB_DISABLE") != "0":
        raise RuntimeError("dedicated IB conformance requires NCCL_NET=IB and NCCL_IB_DISABLE=0")
    if not torch.cuda.is_available() or torch.cuda.device_count() != 1:
        raise RuntimeError(f"rank {rank} requires exactly one allocated CUDA device")
    hostname = socket.gethostname()
    log_path = Path(f"/tmp/taugrid-nccl.{hostname}.{os.getpid()}.log")
    try:
        device = torch.device("cuda")
        torch.cuda.set_device(device)
        values = torch.arange(16, dtype=torch.float32, device=device) + rank
        dist.all_reduce(values)
        expected = torch.arange(16, dtype=torch.float32, device=device) * world_size
        expected += world_size * (world_size - 1) // 2
        if not torch.equal(values, expected):
            raise RuntimeError(f"rank {rank} returned an incorrect CUDA all_reduce")
        rank_tensor = torch.tensor([rank], dtype=torch.int64, device=device)
        gathered_ranks = [torch.empty_like(rank_tensor) for _ in range(world_size)]
        dist.all_gather(gathered_ranks, rank_tensor)
        if [int(value.item()) for value in gathered_ranks] != list(range(world_size)):
            raise RuntimeError(f"rank {rank} observed incorrect CUDA all_gather ranks")
        torch.cuda.synchronize()
    finally:
        print("MATRIX_IB_NCCL_LOG=" + json.dumps({
            "rank": rank, "hostname": hostname,
            "log": log_path.read_text() if log_path.is_file() else "NCCL log file was not created",
        }, sort_keys=True), flush=True)
    logs = log_path.read_text()
    require_ib_transport(logs)
    cap_eff = next(line.split()[1] for line in Path("/proc/self/status").read_text().splitlines()
                   if line.startswith("CapEff:"))
    if os.getuid() != 65532 or int(cap_eff, 16) != 0:
        raise RuntimeError(f"rank {rank} requires UID65532 and no effective capabilities")
    metadata = {
        "rank": rank,
        "hostname": hostname,
        "pid": os.getpid(),
        "uid": os.getuid(),
        "cap_eff": cap_eff,
        "memlock": memlock,
        "verbs_devices": sorted(path.name for path in Path("/dev/infiniband").glob("uverbs*")),
        "hcas": sorted(path.name for path in Path("/sys/class/infiniband").glob("*")),
        "ib_ports": ib_ports,
        "device_name": torch.cuda.get_device_name(0),
        "visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES", ""),
        "packages": packages,
        "world_size": world_size,
        "backend": str(dist.get_backend()),
        "all_reduce": values.cpu().tolist(),
        "all_gather_ranks": [int(value.item()) for value in gathered_ranks],
        "nccl_log": logs,
    }
    if not metadata["verbs_devices"] or not metadata["hcas"]:
        raise RuntimeError(f"rank {rank} has no InfiniBand devices")
    records = [None] * world_size
    dist.all_gather_object(records, metadata)
    require_rank_results(records, world_size)
    if rank == 0:
        print("MATRIX_IB_RESULT=" + json.dumps(
            {"expected_workers": world_size, "workers": records}, sort_keys=True,
        ), flush=True)
    train.report({"rank": rank, "world_size": world_size, "checksum": float(values.sum().item())})


if __name__ == "__main__":
    print("MATRIX_RUNTIME_RESULT=" + json.dumps(check_runtime(), sort_keys=True), flush=True)
    ray.init()
    result = TorchTrainer(
        train_loop_per_worker=collective_loop,
        scaling_config=ScalingConfig(num_workers=WORKERS, use_gpu=True),
    ).fit()
    if result.error:
        raise result.error
    print("SUCCESS: CUDA NCCL InfiniBand collectives completed on every GPU worker", flush=True)
