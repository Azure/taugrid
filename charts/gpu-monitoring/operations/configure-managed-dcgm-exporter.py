#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Opt-in, reversible counter configuration for an existing AKS port-19400 exporter."""

import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.request


SERVICE = "nvidia-dcgm-exporter.service"
ENGINE = "nvidia-dcgm.service"
DEFAULT = Path("/etc/dcgm-exporter/default-counters.csv")
DIRECTORY = Path("/etc/taugrid/dcgm-exporter")
STATE = DIRECTORY / "state.json"
DROPIN = Path("/etc/systemd/system/nvidia-dcgm-exporter.service.d/90-taugrid-metrics.conf")
BASE_ARGV = ["/usr/bin/dcgm-exporter", "-f", str(DEFAULT), "--address", ":19400"]


def run(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=90).stdout.strip()


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def fields(text):
    result = {}
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        parts = [part.strip() for part in line.split(",", 2)]
        if (len(parts) != 3 or not re.fullmatch(r"DCGM_FI_[A-Z0-9_]+", parts[0])
                or parts[1] not in ("gauge", "counter", "label") or parts[0] in result):
            raise ValueError(f"Invalid or duplicate DCGM CSV field: {line!r}")
        result[parts[0]] = line
    if not result:
        raise ValueError("An empty counter CSV is not allowed")
    return result


def merge_csv(existing, additional):
    old, new = fields(existing), fields(additional)
    # Keep all AKS/package-provided field definitions, including their types.
    additions = [line for name, line in new.items() if name not in old]
    return existing.rstrip() + "\n\n# TauGrid additional health counters\n" + "\n".join(additions) + "\n"


def service_pid(service):
    if run("systemctl", "is-active", service) != "active":
        raise RuntimeError(f"{service} is not active")
    pid = int(run("systemctl", "show", service, "--property=MainPID", "--value"))
    if pid <= 1:
        raise RuntimeError(f"{service} has no running process")
    return pid


def exporter_argv():
    return Path(f"/proc/{service_pid(SERVICE)}/cmdline").read_bytes().decode().rstrip("\0").split("\0")


def protected_files():
    paths = {DEFAULT, Path("/usr/bin/dcgm-exporter")}
    for property_name in ("FragmentPath", "DropInPaths"):
        paths.update(Path(item) for item in run(
            "systemctl", "show", SERVICE, f"--property={property_name}", "--value"
        ).split() if Path(item) != DROPIN)
    aks = Path("/etc/systemd/system/nvidia-dcgm-exporter.service.d/10-aks-override.conf")
    if aks not in paths:
        raise RuntimeError("Expected AKS-owned port-19400 override is absent")
    return {str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(paths)}


def gpu_uuids():
    output = run("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader")
    uuids = output.splitlines()
    if not uuids or len(set(uuids)) != len(uuids) or any(
        not re.fullmatch(r"GPU-[0-9a-f-]+", uuid) for uuid in uuids
    ):
        raise RuntimeError("Cannot establish the exact physical GPU identities")
    return sorted(uuids)


def wait_exporter(expected):
    deadline = time.monotonic() + 90
    last_error = "No complete physical-GPU utilization samples"
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    while time.monotonic() < deadline:
        try:
            with opener.open("http://127.0.0.1:19400/metrics", timeout=5) as response:
                text = response.read(8 * 1024 * 1024).decode()
            found = set()
            for line in text.splitlines():
                if not line.startswith("DCGM_FI_DEV_GPU_UTIL{"):
                    continue
                match = re.search(r'UUID="(GPU-[0-9a-f-]+)"', line)
                values = line.rsplit("}", 1)[-1].split()
                if match and values and math.isfinite(float(values[0])):
                    found.add(match[1])
            if found == set(expected):
                return
            last_error = f"Exporter identities: expected {len(expected)}, observed {len(found)}"
        except (urllib.error.URLError, TimeoutError, ValueError) as error:
            last_error = str(error)
        time.sleep(2)
    raise RuntimeError("Exporter did not recover on port 19400: " + last_error)


def verify(state, expected_argv):
    if protected_files() != state["protected_files"]:
        raise RuntimeError("AKS/package-owned exporter files changed")
    if service_pid(ENGINE) != state["hostengine_pid"]:
        raise RuntimeError("The protected DCGM host engine restarted")
    if exporter_argv() != expected_argv:
        raise RuntimeError("Unexpected exporter process arguments")
    if gpu_uuids() != state["gpu_uuids"]:
        raise RuntimeError("Physical GPU identities changed")
    wait_exporter(state["gpu_uuids"])


def safe_destination(path):
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise RuntimeError(f"Refusing symlink in owned destination: {parent}")


def exclusive_write(path, text):
    safe_destination(path)
    with path.open("x") as stream:
        try:
            os.fchmod(stream.fileno(), 0o644)
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        except OSError:
            path.unlink()
            raise


def load_state():
    safe_destination(STATE)
    state = json.loads(STATE.read_text())
    metrics = Path(state["metrics_path"])
    safe_destination(metrics)
    safe_destination(DROPIN)
    if (metrics.parent != DIRECTORY
            or metrics.name != f"metrics-{state['metrics_sha256']}.csv"
            or sha(metrics.read_text()) != state["metrics_sha256"]
            or DROPIN.read_text() != state["dropin"]):
        raise RuntimeError("Owned configuration changed; refusing to overwrite or remove it")
    return state


def restart_exporter():
    run("systemctl", "daemon-reload")
    run("systemctl", "restart", SERVICE)


def rollback(state):
    if protected_files() != state["protected_files"] or service_pid(ENGINE) != state["hostengine_pid"]:
        raise RuntimeError("Protected host state changed; manual recovery required")
    DROPIN.unlink()
    try:
        restart_exporter()
        verify(state, BASE_ARGV)
    except (OSError, subprocess.SubprocessError, RuntimeError, ValueError):
        # Preserve the recovery recipe even when the host cannot restart.
        exclusive_write(DROPIN, state["dropin"])
        run("systemctl", "daemon-reload")
        raise
    Path(state["metrics_path"]).unlink()
    STATE.unlink()
    return {"rolled_back": True, "port": 19400, "hostengine_unchanged": True}


def configure(action, additional):
    safe_destination(DIRECTORY)
    safe_destination(DROPIN)
    if STATE.exists():
        state = load_state()
        if action == "rollback":
            return rollback(state)
        desired = merge_csv(DEFAULT.read_text(), additional)
        if sha(desired) != state["metrics_sha256"]:
            raise RuntimeError("Different configuration already installed; roll it back first")
        verify(state, state["argv"])
        return {"changed": False, "port": 19400, "metrics_path": state["metrics_path"]}
    if action == "rollback":
        raise RuntimeError("No TauGrid-owned configuration exists")
    if DROPIN.exists() or exporter_argv() != BASE_ARGV:
        raise RuntimeError("Exporter is not using the expected unmodified AKS invocation")
    desired = merge_csv(DEFAULT.read_text(), additional)
    metrics = DIRECTORY / f"metrics-{sha(desired)}.csv"
    if metrics.exists():
        raise RuntimeError("Unowned destination already exists")
    argv = ["/usr/bin/dcgm-exporter", "-f", str(metrics), "--address", ":19400"]
    dropin = "# Owned by TauGrid; remove using the configurator rollback action.\n[Service]\n"
    dropin += "ExecStart=\nExecStart=" + " ".join(argv) + "\n"
    state = {
        "metrics_path": str(metrics), "metrics_sha256": sha(desired), "dropin": dropin,
        "argv": argv, "protected_files": protected_files(),
        "hostengine_pid": service_pid(ENGINE), "gpu_uuids": gpu_uuids(),
    }
    verify(state, BASE_ARGV)
    if action == "plan":
        return {"changed": False, "plan": state, "added_fields": len(fields(desired)) - len(fields(DEFAULT.read_text()))}
    DIRECTORY.mkdir(parents=True, exist_ok=True)
    created = []
    try:
        for path, text in ((metrics, desired), (STATE, json.dumps(state, indent=2)), (DROPIN, dropin)):
            exclusive_write(path, text)
            created.append(path)
    except OSError:
        for path in reversed(created):
            path.unlink()
        raise
    try:
        restart_exporter()
        verify(state, argv)
    except (OSError, subprocess.SubprocessError, RuntimeError, ValueError) as error:
        rollback(state)
        raise RuntimeError(f"Apply failed; original AKS exporter restored: {error}") from error
    return {"changed": True, "port": 19400, "metrics_path": str(metrics), "hostengine_unchanged": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("plan", "apply", "rollback"), nargs="?", default="plan")
    metrics = parser.add_mutually_exclusive_group()
    metrics.add_argument("--metrics-file", type=Path)
    metrics.add_argument("--metrics-csv", help="CSV content for streamed execution; contains no credentials")
    args = parser.parse_args()
    if args.action != "rollback" and args.metrics_file is None and args.metrics_csv is None:
        parser.error("plan/apply requires --metrics-file or --metrics-csv")
    text = args.metrics_file.read_text() if args.metrics_file else args.metrics_csv
    if args.action == "plan":
        result = configure(args.action, text)
    else:
        if os.geteuid() != 0:
            parser.error("apply/rollback must run as root on the target host")
        with open("/run/lock/taugrid-dcgm-exporter.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = configure(args.action, text)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
