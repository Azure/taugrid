# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Submit the loss notebook once through an already-running TauGrid server.

No Jupyter server or kernel is started by this script. The remote notebook
runtime must already contain nbconvert, nbformat and ipykernel.
"""
from __future__ import annotations

import argparse
import json
import math
import os
import sys
import time
import urllib.parse
from pathlib import Path

import urllib3

REPO_ROOT = Path(__file__).resolve().parent.parent
NOTEBOOK = REPO_ROOT / "examples" / "notebook-loss-curve-demo.ipynb"
API_LIMIT = 1024 * 1024


def call(server, token, path, *, deadline, body=None, params=None):
    parsed = urllib.parse.urlsplit(server)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("server must be an HTTP(S) URL without credentials, query or fragment")
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("Demo deadline expired")
    manager = urllib3.PoolManager()
    response = None
    try:
        response = manager.request(
            "POST" if body is not None else "GET",
            f"{server.rstrip('/')}/taugrid/api/{path}?{urllib.parse.urlencode(params or {})}",
            body=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": f"token {token}", "Content-Type": "application/json"},
            timeout=urllib3.Timeout(connect=min(2, remaining), read=min(30, remaining)),
            preload_content=False, retries=False, redirect=False,
        )
        data = bytearray()
        while len(data) <= API_LIMIT:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("Demo deadline expired")
            raw = getattr(getattr(getattr(response, "_fp", None), "fp", None), "raw", None)
            sock = getattr(raw, "_sock", None)
            if sock is not None:
                sock.settimeout(min(30, remaining))
            elif not response.isclosed():
                raise TimeoutError("Cannot enforce response read deadline")
            chunk = response.read1(min(4096, API_LIMIT + 1 - len(data)))
            if not chunk:
                break
            data.extend(chunk)
        if time.monotonic() >= deadline:
            raise TimeoutError("Demo deadline expired")
        if len(data) > API_LIMIT:
            raise ValueError("API response exceeded 1 MiB")
        result = json.loads(data or b"{}")
        if response.status != 200:
            raise ValueError(f"{path} returned HTTP {response.status}: {result.get('message', 'request failed')}")
        return result
    finally:
        if response is not None:
            response.close()
        manager.clear()


def finite_samples(status):
    metrics = status.get("metrics") or {}
    source = metrics.get("source") or {}
    if not status.get("uid") or source.get("runUid") != status["uid"] or not metrics.get("checkedAt"):
        return []
    return [sample for sample in metrics.get("samples", [])
            if type(sample.get("step")) is int and 0 <= sample["step"] <= 9007199254740991
            and type(sample.get("value")) in (int, float) and math.isfinite(sample["value"])]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://127.0.0.1:8888")
    parser.add_argument("--token", default=os.environ.get("JUPYTER_TOKEN"))
    parser.add_argument("--namespace", default="tau-notebook-e2e")
    parser.add_argument("--name", default="cpu-loss-demo")
    parser.add_argument("--profile", default="azure.research.cpu.small")
    parser.add_argument("--queue")
    parser.add_argument("--timeout", type=int, default=900)
    parser.add_argument("--notebook", default=str(NOTEBOOK))
    parser.add_argument("--notebook-path", default="",
                        help="notebook path relative to the Jupyter server root (required with --files)")
    parser.add_argument("--files", nargs="*", default=[],
                        help="files beside the notebook to ship with it, chosen from the notebook directory")
    args = parser.parse_args(argv)
    if not args.token or args.timeout <= 0:
        parser.error("provide --token (or JUPYTER_TOKEN) and a positive --timeout")
    deadline = time.monotonic() + args.timeout

    def request(path, **kwargs):
        return call(args.server, args.token, path, deadline=deadline, **kwargs)

    try:
        notebook = Path(args.notebook)
        if notebook.stat().st_size > API_LIMIT:
            raise ValueError("Demo notebook exceeds 1 MiB")
        if args.files and not args.notebook_path:
            raise ValueError("--files needs --notebook-path so the server can find them beside the notebook")
        payload = {"notebook": notebook.read_text(encoding="utf-8"), "path": args.notebook_path or notebook.name,
                   "namespace": args.namespace, "name": args.name, "profile": args.profile}
        if args.queue:
            payload["queue"] = args.queue
        if args.files:
            payload["files"] = args.files
        if not request("capabilities").get("submissionEnabled"):
            raise ValueError("Submission disabled: configure TAUGRID_SUBMISSION_ENABLED=1")
        preview = request("preview", body=payload)
        if not preview.get("submittable") or not preview.get("submissionEnabled"):
            raise ValueError("Preview is not enabled for submission")
        plan = preview["plan"]
        print("PREVIEW", json.dumps(plan), flush=True)
        if plan.get("profile") != args.profile or not plan.get("gpusPerWorker") or any(plan["gpusPerWorker"]):
            raise ValueError("Reviewed plan does not establish the requested CPU-only profile")
        if plan.get("submissionMode") != "K8sJobMode" or plan.get("retentionSeconds") != 600:
            raise ValueError("Reviewed plan lacks the notebook submitter/retention contract")
        result = request("submit", body={**payload, "namespace": plan["namespace"], "name": plan["name"], "confirm": True, "planDigest": plan["planDigest"]})
        print("SUBMITTED", json.dumps(result), flush=True)
        target = {"namespace": result["namespace"], "name": result["name"], "kind": result.get("kind", "RayJob")}
        observed = False
        run_uid = None
        logs_read = False
        while time.monotonic() < deadline:
            status = request("status", params={**target, "includeMetrics": "true"})
            if status.get("uid"):
                if run_uid is not None and run_uid != status["uid"]:
                    raise ValueError("Workload identity changed during demonstration")
                run_uid = status["uid"]
            samples = finite_samples(status)
            observed = observed or bool(samples)
            metrics = status.get("metrics") or {}
            print("STATUS", status.get("state"), "samples", len(samples), "stale", metrics.get("stale"),
                  "coverage", metrics.get("truncationReasons"), flush=True)
            source = metrics.get("source") or {}
            if not logs_read and source.get("type") == "stdout" and samples:
                try:
                    logs = request("logs", params={**target, "pod": source["pod"], "container": source["container"], "tail": 1000})
                    print("BOUNDED LOG SNAPSHOT", json.dumps(logs), flush=True)
                    logs_read = True
                except (ValueError, TimeoutError, urllib3.exceptions.HTTPError) as exc:
                    print("LOGS UNAVAILABLE", str(exc), flush=True)
            if status.get("terminal"):
                succeeded = status.get("state") == "complete"
                if succeeded and not observed:
                    raise ValueError("Run succeeded but never produced real finite loss samples")
                print("RESULT", "SUCCEEDED WITH LOSS" if succeeded else "FAILED", flush=True)
                return 0 if succeeded else 1
            time.sleep(min(10, max(0, deadline - time.monotonic())))
        raise TimeoutError("Demo deadline expired; workload was not cancelled")
    except (ValueError, KeyError, OSError, TimeoutError, urllib3.exceptions.HTTPError) as exc:
        print(f"Demo failed: {exc}. Submissions are never retried; inspect runs before resubmitting.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())