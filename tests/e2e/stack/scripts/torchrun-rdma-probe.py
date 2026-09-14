#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
import hmac
import json
import os
import re
import resource
import secrets
import socket
import sys
import time
from datetime import timedelta
from pathlib import Path

import torch
import torch.distributed as dist


def fail(message: str) -> None:
    print(f"TAUGRID_RDMA_FAIL reason={message}", flush=True)
    raise SystemExit(1)


def read_auth_key(path: str) -> bytes:
    key = Path(path).read_bytes()
    if len(key) != 32:
        fail(f"auth-key-length-{len(key)}-not-32")
    return key


def signed_identity(key: bytes, identity: dict[str, object]) -> dict[str, object]:
    encoded = json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()
    return {
        "identity": identity,
        "mac": hmac.new(key, encoded, hashlib.sha256).hexdigest(),
    }


def verify_identities(
    key: bytes,
    receipts: list[dict[str, object]],
    run_id: str,
    world_size: int,
) -> list[dict[str, object]]:
    identities: list[dict[str, object]] = []
    for receipt in receipts:
        identity = receipt.get("identity")
        received_mac = receipt.get("mac")
        if not isinstance(identity, dict) or not isinstance(received_mac, str):
            fail("malformed-peer-auth-receipt")
        expected = signed_identity(key, identity)["mac"]
        if not hmac.compare_digest(received_mac, str(expected)):
            fail("peer-auth-hmac-mismatch")
        if identity.get("run_id") != run_id:
            fail("peer-auth-run-mismatch")
        identities.append(identity)

    ranks = {identity.get("rank") for identity in identities}
    nodes = {identity.get("node") for identity in identities}
    nonces = {identity.get("nonce") for identity in identities}
    if ranks != set(range(world_size)):
        fail(f"peer-auth-ranks-{sorted(ranks, key=str)}")
    if len(nodes) != world_size:
        fail(f"peer-auth-nodes-not-distinct-{sorted(nodes, key=str)}")
    if len(nonces) != world_size:
        fail("peer-auth-nonces-not-distinct")
    return sorted(identities, key=lambda item: int(item["rank"]))


def nccl_version() -> str:
    version = torch.cuda.nccl.version()
    if isinstance(version, tuple):
        return ".".join(str(part) for part in version)
    encoded = int(version)
    return f"{encoded // 10000}.{(encoded % 10000) // 100}.{encoded % 100}"


def gpu_uuid(properties: object) -> str:
    visible = os.environ.get("NVIDIA_VISIBLE_DEVICES", "")
    visible_devices = [item.strip() for item in visible.split(",") if item.strip()]
    if len(visible_devices) == 1 and re.fullmatch(r"GPU-[A-Za-z0-9-]+", visible_devices[0]):
        return visible_devices[0]
    value = str(getattr(properties, "uuid", ""))
    if re.fullmatch(r"GPU-[A-Za-z0-9-]+", value):
        return value
    fail("gpu-uuid-unavailable")
    raise AssertionError("unreachable")


def active_rdma_endpoint() -> dict[str, str]:
    for device_path in sorted(Path("/sys/class/infiniband").glob("*")):
        for port_path in sorted((device_path / "ports").glob("*")):
            state = (port_path / "state").read_text().strip()
            if "ACTIVE" not in state.upper():
                continue
            interfaces: set[str] = set()
            for path in (port_path / "gid_attrs" / "ndevs").glob("*"):
                interface = path.read_text().strip()
                if interface:
                    interfaces.add(interface)
            interfaces.update(path.name for path in (device_path / "device" / "net").glob("*"))
            if not interfaces:
                fail(f"active-rdma-device-{device_path.name}-has-no-interface")
            return {
                "rdma_device": device_path.name,
                "rdma_interface": sorted(interfaces)[0],
                "rdma_link_state": state,
            }
    fail("no-active-rdma-device")
    raise AssertionError("unreachable")


def main() -> None:
    backend = os.environ.get("TAUGRID_BACKEND", "nccl")
    live = os.environ.get("TAUGRID_LIVE_RDMA", "0") == "1"
    node_name = os.environ.get("TAUGRID_NODE_NAME", "")
    job_index = os.environ.get("JOB_COMPLETION_INDEX", "")
    run_id = os.environ.get("TAUGRID_RUN_ID", "")
    auth_key_path = os.environ.get("TAUGRID_AUTH_KEY_FILE", "")

    if backend not in {"gloo", "nccl"}:
        fail(f"unsupported-backend-{backend}")
    if live != (backend == "nccl"):
        fail("live-mode-must-use-nccl")
    if not node_name:
        fail("missing-node-name")
    if not re.fullmatch(r"nccl-rdma-[a-f0-9]{32}", run_id):
        fail("invalid-run-id")
    if not auth_key_path:
        fail("missing-auth-key-file")

    rank = int(os.environ["RANK"])
    world_size = int(os.environ["WORLD_SIZE"])
    local_rank = int(os.environ["LOCAL_RANK"])
    if world_size != 2:
        fail(f"world-size-{world_size}-not-2")
    if local_rank != 0:
        fail(f"local-rank-{local_rank}-not-0")
    if not job_index or int(job_index) != rank:
        fail(f"job-index-{job_index}-rank-{rank}-mismatch")

    soft_memlock, hard_memlock = resource.getrlimit(resource.RLIMIT_MEMLOCK)
    print(
        "TAUGRID_MEMLOCK "
        + json.dumps(
            {
                "rank": rank,
                "soft": soft_memlock,
                "hard": hard_memlock,
                "infinity": resource.RLIM_INFINITY,
            },
            sort_keys=True,
        ),
        flush=True,
    )

    if backend == "nccl":
        if not torch.cuda.is_available():
            fail("cuda-unavailable")
        if torch.cuda.device_count() != 1:
            fail(f"visible-gpu-count-{torch.cuda.device_count()}-not-1")
        torch.cuda.set_device(0)
        device = torch.device("cuda", 0)
        properties = torch.cuda.get_device_properties(0)
        rdma_endpoint = active_rdma_endpoint()
        os.environ["NCCL_IB_HCA"] = rdma_endpoint["rdma_device"]
        runtime = {
            "rank": rank,
            "node": node_name,
            "host": socket.gethostname(),
            "gpu_model": properties.name,
            "gpu_uuid": gpu_uuid(properties),
            "nccl_version": nccl_version(),
            "environment": {
                name: os.environ[name]
                for name in (
                    "NCCL_DEBUG",
                    "NCCL_DEBUG_SUBSYS",
                    "NCCL_IB_DISABLE",
                    "TAUGRID_BACKEND",
                    "TAUGRID_ELEMENTS",
                    "TAUGRID_ITERATIONS",
                    "TAUGRID_LIVE_RDMA",
                    "TAUGRID_WARMUP",
                )
            },
            **rdma_endpoint,
        }
        print(f"TAUGRID_RDMA_RUNTIME {json.dumps(runtime, sort_keys=True)}", flush=True)
    else:
        device = torch.device("cpu")

    dist.init_process_group(backend=backend, timeout=timedelta(seconds=180))

    auth_key = read_auth_key(auth_key_path)
    identity = {
        "run_id": run_id,
        "rank": rank,
        "node": node_name,
        "host": socket.gethostname(),
        "nonce": secrets.token_hex(16),
    }
    signed = signed_identity(auth_key, identity)
    signed_receipts: list[dict[str, object] | None] = [None] * world_size
    dist.all_gather_object(signed_receipts, signed)
    verified = verify_identities(
        auth_key,
        [receipt for receipt in signed_receipts if receipt is not None],
        run_id,
        world_size,
    )
    print(f"TAUGRID_PEER_AUTH rank={rank} peers={len(verified)}", flush=True)

    elements = int(os.environ.get("TAUGRID_ELEMENTS", str(16 * 1024 * 1024)))
    warmup = int(os.environ.get("TAUGRID_WARMUP", "5"))
    iterations = int(os.environ.get("TAUGRID_ITERATIONS", "20"))
    if elements <= 0 or warmup <= 0 or iterations <= 0:
        fail("non-positive-benchmark-parameters")

    tensor = torch.full((elements,), float(rank + 1), dtype=torch.float32, device=device)
    dist.all_reduce(tensor)
    expected = float(world_size * (world_size + 1) // 2)
    max_error = torch.max(torch.abs(tensor - expected))
    if backend == "nccl":
        torch.cuda.synchronize()
    if max_error.item() != 0.0:
        fail(f"correctness-max-error-{max_error.item()}")

    tensor.fill_(1.0)
    for _ in range(warmup):
        dist.all_reduce(tensor)
    if backend == "nccl":
        torch.cuda.synchronize()

    started = time.perf_counter()
    for _ in range(iterations):
        dist.all_reduce(tensor)
    if backend == "nccl":
        torch.cuda.synchronize()
    elapsed = time.perf_counter() - started

    elapsed_tensor = torch.tensor([elapsed], dtype=torch.float64, device=device)
    dist.all_reduce(elapsed_tensor, op=dist.ReduceOp.MAX)
    max_elapsed = elapsed_tensor.item()
    local_seconds_per_iteration = elapsed / iterations
    seconds_per_iteration = max_elapsed / iterations
    payload_bytes = tensor.numel() * tensor.element_size()
    local_algbw_gbps = payload_bytes / local_seconds_per_iteration / 1e9
    local_busbw_gbps = local_algbw_gbps * (2 * (world_size - 1) / world_size)
    algbw_gbps = payload_bytes / seconds_per_iteration / 1e9
    busbw_gbps = algbw_gbps * (2 * (world_size - 1) / world_size)
    if (
        local_algbw_gbps <= 0
        or local_busbw_gbps <= 0
        or algbw_gbps <= 0
        or busbw_gbps <= 0
    ):
        fail("non-positive-bandwidth")

    print(
        "TAUGRID_RDMA_MEASUREMENT "
        + json.dumps(
            {
                "rank": rank,
                "elapsed_seconds": elapsed,
                "algbw_gbps": local_algbw_gbps,
                "busbw_gbps": local_busbw_gbps,
            },
            sort_keys=True,
        ),
        flush=True,
    )
    receipt = {
        "backend": backend,
        "nccl_version": runtime["nccl_version"] if backend == "nccl" else "",
        "world_size": world_size,
        "nodes": [item["node"] for item in verified],
        "hosts": [item["host"] for item in verified],
        "peer_auth_verified": True,
        "payload_bytes": payload_bytes,
        "iterations": iterations,
        "max_elapsed_seconds": max_elapsed,
        "seconds_per_iteration": seconds_per_iteration,
        "algbw_gbps": algbw_gbps,
        "busbw_gbps": busbw_gbps,
        "max_error": max_error.item(),
    }
    print(f"TAUGRID_RDMA_RANK_PASS rank={rank}", flush=True)
    if rank == 0:
        sentinel = "TAUGRID_RDMA_PASS" if live else "TAUGRID_CONTROL_PLANE_PASS"
        print(f"{sentinel} {json.dumps(receipt, sort_keys=True)}", flush=True)

    dist.barrier()
    dist.destroy_process_group()
    sys.exit(0)


if __name__ == "__main__":
    main()
