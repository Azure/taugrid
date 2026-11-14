#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import uuid


def kubectl(*arguments, input_text=None):
    return subprocess.run(
        ["kubectl", *arguments],
        input=input_text,
        capture_output=True,
        text=True,
        check=True,
        timeout=30,
    ).stdout


def namespace_state(namespace):
    response = kubectl("get", "namespace", namespace, "--ignore-not-found", "-o", "json")
    return json.loads(response) if response.strip() else None


def identity(state, namespace, run_id):
    metadata = state["metadata"]
    if metadata["name"] != namespace:
        raise RuntimeError("namespace name does not match the run")
    if metadata.get("labels", {}).get("tau.azure.com/e2e-run") != run_id:
        raise RuntimeError("namespace run label does not match the run")
    uid = metadata["uid"]
    uuid.UUID(uid)
    return {"namespace": namespace, "run_id": run_id, "uid": uid}


def run(action, namespace, run_id, owner_path):
    if not re.fullmatch(r"[0-9]+-[0-9]+", run_id):
        raise RuntimeError("invalid nightly run identity")
    if namespace != f"taugrid-nightly-matrix-{run_id}":
        raise RuntimeError("cleanup is restricted to this run's matrix namespace")
    state = namespace_state(namespace)
    if action == "record":
        if state is None:
            raise RuntimeError("cannot record an absent namespace")
        owner = identity(state, namespace, run_id)
        temporary = owner_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(owner))
        temporary.replace(owner_path)
        return "recorded"
    if state is None:
        return "absent"
    owner = json.loads(owner_path.read_text())
    if identity(state, namespace, run_id) != owner:
        raise RuntimeError("namespace UID does not match the recorded run owner")
    if json.loads(kubectl("get", "pvc", "-n", namespace, "-o", "json"))["items"]:
        raise RuntimeError("refusing namespace cleanup while PVCs exist")
    kubectl(
        "delete", "--raw", f"/api/v1/namespaces/{namespace}", "-f", "-",
        input_text=json.dumps({
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "preconditions": {"uid": owner["uid"]},
            "propagationPolicy": "Foreground",
        }),
    )
    deadline = time.monotonic() + 900
    while time.monotonic() < deadline:
        state = namespace_state(namespace)
        if state is None:
            return "deleted"
        if identity(state, namespace, run_id) != owner:
            raise RuntimeError("namespace identity changed during cleanup")
        time.sleep(5)
    raise RuntimeError("owned namespace still exists after 15 minutes")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("record", "cleanup"))
    parser.add_argument("namespace")
    parser.add_argument("run_id")
    parser.add_argument("owner_path", type=Path)
    args = parser.parse_args()
    try:
        status = run(args.action, args.namespace, args.run_id, args.owner_path)
    except subprocess.TimeoutExpired as error:
        parser.exit(1, f"Nightly owned cleanup failed: kubectl timed out after {error.timeout} seconds\n")
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.CalledProcessError) as error:
        detail = error.stderr if isinstance(error, subprocess.CalledProcessError) else str(error)
        parser.exit(1, f"Nightly owned cleanup failed: {detail}\n")
    print(json.dumps({"action": args.action, "status": status}))


if __name__ == "__main__":
    main()
