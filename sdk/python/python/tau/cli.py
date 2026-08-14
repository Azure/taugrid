# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Tau Python SDK CLI for inspect, submit, and doctor.

The Go CLI's ``tau python ...`` command proxies to this module. Direct module
execution via ``python -m tau.cli`` is also supported for local SDK debugging.
"""

from __future__ import annotations

import argparse
import os
import platform
import shutil
import subprocess
import sys
import time
from pathlib import Path, PurePosixPath
from typing import Optional

import yaml

from tau import __version__ as TAU_VERSION
from tau._artifacts import DEFAULT_CHECKPOINT_ARTIFACT, validate_checkpoint_artifact
from tau.build import (
    BuildArtifactError,
    BuildOverrides,
    build_artifact,
    execution_config,
    load_artifact,
)
from tau.entrypoints import load_staged_module

# Pulled in to share the binary-discovery + storage-path logic with the
# decorator's submit code path.
from tau.workloads import _find_portal_binary, _find_tau_binary  # type: ignore[attr-defined]

EXPECTED_TAU_CLI_VERSION = "v" + TAU_VERSION


def _load_module(path: Path):
    try:
        return load_staged_module(path, module_name="__tau_inspect__")
    except (FileNotFoundError, ImportError, ValueError) as exc:
        raise RuntimeError(f"cannot import {path}: {exc}") from exc


def _find_handles_of_kind(module, kind: str):
    """Return [(attr_name, handle), ...] for handles of the given kind.

    `kind` is "train" or "eval". Dedupes aliased re-exports by id() so
    `tr = train` doesn't trip "multiple handles".
    """
    sentinel = f"_tau_{kind}_handle"
    seen_ids = set()
    handles = []
    for name in dir(module):
        obj = getattr(module, name)
        if not getattr(obj, sentinel, False):
            continue
        if id(obj) in seen_ids:
            continue
        seen_ids.add(id(obj))
        handles.append((name, obj))
    return handles


def _find_handles(module):
    """Combined train + eval handle list — used by `inspect`."""
    return _find_handles_of_kind(module, "train") + _find_handles_of_kind(module, "eval")


def _checkpoint_artifact_for_handle(train_handle) -> str:
    manifest = train_handle.manifest()
    artifacts = manifest.get("artifacts") or {}
    if not isinstance(artifacts, dict):
        raise RuntimeError("tau python submit: train manifest artifacts must be a mapping")
    checkpoint = artifacts.get("checkpoint")
    if checkpoint is None:
        checkpoint = DEFAULT_CHECKPOINT_ARTIFACT
    return validate_checkpoint_artifact(
        checkpoint,
        subject="tau python submit: train manifest artifacts.checkpoint",
        error_type=RuntimeError,
    )


def _resolve_upstream_checkpoint_path(train_handle, namespace: str, kube_context: Optional[str]) -> str:
    """Compute the pod-side absolute path of the train run's checkpoint artifact.

    Completed train artifacts are finalized under
    ``/data/checkpoints/finetunes/<train-name>/artifacts/``. The train handle's
    ``artifacts.checkpoint`` manifest entry declares which relative file under
    that directory is the eval input. This function returns that absolute path
    so eval submit can pass it as ``--upstream-checkpoint``.

    We don't kubectl-introspect the train pod to discover the actual
    file because the path is contractual (set by the train handle + the
    SDK's Ctx defaults); reading from kubectl would just be a slower way to
    confirm what we already wrote.
    """
    artifact = _checkpoint_artifact_for_handle(train_handle)
    return (
        PurePosixPath("/data/checkpoints")
        / "finetunes"
        / train_handle._params.name
        / "artifacts"
        / artifact
    ).as_posix()


def _resource_name_for_handle(handle) -> str:
    extra_manifest = getattr(handle, "_extra_manifest", {}) or {}
    resource_naming = extra_manifest.get("resource_naming")
    prefix = None
    if isinstance(resource_naming, dict):
        prefix = resource_naming.get("prefix")
    if not prefix:
        prefix = "tau"
    return f"{prefix}-{handle._params.name}"


def _kubectl_base(kube_context: Optional[str], namespace: str) -> list[str]:
    if not shutil.which("kubectl"):
        raise RuntimeError("tau python submit: kubectl not found on PATH")
    cmd = ["kubectl"]
    if kube_context:
        cmd += ["--context", kube_context]
    return cmd + ["-n", namespace]


def _wait_for_rayjob_success(
    rayjob_name: str,
    run_name: str,
    namespace: str,
    kube_context: Optional[str],
    timeout: str = "24h",
    poll_interval: float = 15.0,
) -> None:
    deadline = time.monotonic() + _parse_duration(timeout)
    base = _kubectl_base(kube_context, namespace) + [
        "get",
        "rayjob",
        rayjob_name,
        "-o",
        "jsonpath={.status.jobStatus}",
    ]
    print(
        f"tau python submit: waiting for rayjob/{rayjob_name} in {namespace} "
        f"(timeout={timeout}, poll={poll_interval}s)...",
        flush=True,
    )
    last_status = ""
    while time.monotonic() < deadline:
        try:
            res = subprocess.run(base, capture_output=True, text=True, check=False)
        except OSError as e:
            raise RuntimeError(f"tau python submit: kubectl exec failed: {e}") from e
        status = (res.stdout or "").strip()
        if status != last_status:
            print(f"tau python submit:   rayjob/{rayjob_name} status={status or '<unknown>'}", flush=True)
            last_status = status
        if status == "SUCCEEDED":
            return
        if status == "FAILED":
            raise RuntimeError(
                f"tau python submit: rayjob/{rayjob_name} reports FAILED; "
                f"check `tau run logs {run_name} -n {namespace}`."
            )
        time.sleep(poll_interval)
    raise RuntimeError(
        f"tau python submit: timed out after {timeout} waiting for "
        f"rayjob/{rayjob_name} to reach SUCCEEDED (last status: {last_status or '<none>'})"
    )


def _delete_completed_rayjob(
    rayjob_name: str,
    namespace: str,
    kube_context: Optional[str],
    timeout: str = "5m",
) -> None:
    cmd = _kubectl_base(kube_context, namespace) + [
        "delete",
        "rayjob",
        rayjob_name,
        "--wait=true",
        f"--timeout={timeout}",
        "--ignore-not-found=true",
    ]
    print(
        f"tau python submit: deleting completed rayjob/{rayjob_name} "
        f"to release RayCluster resources before eval...",
        flush=True,
    )
    res = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        detail = (res.stderr or res.stdout or "").strip()
        raise RuntimeError(
            f"tau python submit: failed to delete completed rayjob/{rayjob_name} "
            f"before eval"
            + (": " + detail if detail else "")
        )


def _parse_duration(s: str) -> float:
    """Mini Go-style duration parser: accepts 30s, 5m, 2h, 24h, 90m."""
    s = s.strip()
    if not s:
        return 24 * 3600.0
    units = {"s": 1.0, "m": 60.0, "h": 3600.0}
    if s[-1] in units:
        try:
            return float(s[:-1]) * units[s[-1]]
        except ValueError:
            pass
    try:
        return float(s)
    except ValueError as e:
        raise ValueError(f"Tau Python SDK: invalid duration {s!r}; expected e.g. 30s, 5m, 2h") from e


def _doctor(*, kube_context: Optional[str], namespace: str) -> int:
    print(f"tau python doctor: Tau Python SDK {TAU_VERSION}")
    print(f"tau python doctor: python {platform.python_version()} ({sys.executable})")
    try:
        tau_binary = _find_tau_binary()
        res = subprocess.run(
            [tau_binary, "version", "--short"], capture_output=True, text=True, check=False
        )
        version = (res.stdout or res.stderr or "").strip()
        if res.returncode != 0:
            print(f"tau python doctor: tau CLI failed: {version}", file=sys.stderr)
            return res.returncode
        expected_version = EXPECTED_TAU_CLI_VERSION
        if version != expected_version:
            print(
                "tau python doctor: version mismatch: Tau Python SDK "
                + TAU_VERSION
                + " requires tau CLI "
                + expected_version
                + ", found "
                + repr(version)
                + "; install the matching Tau CLI release",
                file=sys.stderr,
            )
            return 1
        print(f"tau python doctor: tau {version} ({tau_binary})")
    except RuntimeError as e:
        print(str(e), file=sys.stderr)
        print("tau python doctor: install the Tau CLI or set TAU_BINARY=/path/to/tau", file=sys.stderr)
        return 1

    # Stellar sync shells to taugrid-portal, not tau. Advisory only: most runs
    # never call stellar.sync(), so a missing portal CLI is not a doctor failure.
    try:
        portal_binary = _find_portal_binary()
        print(f"tau python doctor: taugrid-portal ({portal_binary})")
    except RuntimeError:
        print(
            "tau python doctor: taugrid-portal not found; stellar.sync() will fail. "
            "Install it (make install-taugrid-portal) or set "
            "TAUGRID_PORTAL_BINARY=/path/to/taugrid-portal"
        )

    try:
        cmd = _kubectl_base(kube_context, namespace) + ["auth", "can-i", "create", "jobs.batch"]
    except RuntimeError:
        print("tau python doctor: kubectl not found on PATH; submit polling needs kubectl", file=sys.stderr)
        return 1
    res = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        detail = (res.stderr or res.stdout or "").strip()
        print("tau python doctor: kubectl auth check failed" + (": " + detail if detail else ""), file=sys.stderr)
        return res.returncode
    print(f"tau python doctor: kubectl can create jobs.batch in namespace {namespace}")
    print("tau python doctor: ok")
    return 0


def _orchestrate_build(
    module_path: Path,
    *,
    output: Path,
    force: bool,
    overrides: BuildOverrides,
) -> int:
    mod = _load_module(module_path.resolve())
    train_handles = _find_handles_of_kind(mod, "train")
    eval_handles = _find_handles_of_kind(mod, "eval")
    serve_handles = _find_handles_of_kind(mod, "serve")
    result = build_artifact(
        train_handles=train_handles,
        eval_handles=eval_handles,
        serve_handles=serve_handles,
        output_dir=output,
        generator_version=TAU_VERSION,
        overrides=overrides,
        force=force,
    )
    if serve_handles:
        names = ", ".join(str(handle.name) for _, handle in serve_handles)
        print(
            "tau python build: tau.serve handles remain separate and were not "
            f"exported ({names}); use ServeHandle.deploy() or tau serve deploy",
            file=sys.stderr,
        )
    print(result)
    return 0


def _orchestrate_submit_build(
    build_path: Path,
    *,
    kube_context: Optional[str],
    dry_run: Optional[str],
    timeout: str,
    poll_interval: float,
    keep_train_rayjob: bool,
    cleanup_timeout: str,
) -> int:
    root, index = load_artifact(build_path)
    workloads = index["workloads"]
    has_eval = any(record.get("kind") == "eval" for record in workloads)
    binary = _find_tau_binary()

    for record in workloads:
        kind = str(record.get("kind") or "")
        name = str(record.get("name") or "")
        namespace = str(record.get("namespace") or "ray")
        resource_name = str(record.get("resource_name") or "")
        if kind not in ("train", "eval") or not name or not resource_name:
            raise BuildArtifactError("Tau build contains an invalid workload record")
        print(
            f"tau python submit-build: submitting {kind} name={name!r} "
            f"from {record.get('config')}",
            flush=True,
        )
        with execution_config(root, record) as config_path:
            argv = [binary, "run", "--config", str(config_path)]
            if dry_run:
                argv += ["--dry-run", dry_run]
            if kube_context:
                argv += ["--context", kube_context]
            subprocess.run(argv, check=True, cwd=root)

        if dry_run is not None:
            continue
        if kind == "train" and has_eval:
            _wait_for_rayjob_success(
                rayjob_name=resource_name,
                run_name=name,
                namespace=namespace,
                kube_context=kube_context,
                timeout=timeout,
                poll_interval=poll_interval,
            )
            if not keep_train_rayjob:
                _delete_completed_rayjob(
                    rayjob_name=resource_name,
                    namespace=namespace,
                    kube_context=kube_context,
                    timeout=cleanup_timeout,
                )
        elif kind == "eval":
            _wait_for_rayjob_success(
                rayjob_name=resource_name,
                run_name=name,
                namespace=namespace,
                kube_context=kube_context,
                timeout=timeout,
                poll_interval=poll_interval,
            )
            _delete_completed_rayjob(
                rayjob_name=resource_name,
                namespace=namespace,
                kube_context=kube_context,
                timeout=cleanup_timeout,
            )
    return 0


def _orchestrate_submit(
    module_path: Path,
    *,
    namespace: Optional[str],
    kube_context: Optional[str],
    dry_run: Optional[str],
    data_pvc: Optional[str],
    queue: Optional[str],
    gpu_class: Optional[str],
    disable_default_priorities: bool,
    node_selector: Optional[list[str]],
    timeout: str,
    poll_interval: float,
    keep_train_rayjob: bool,
    cleanup_timeout: str,
    profiler: Optional[str] = None,
    profile_rank: Optional[str] = None,
    profile_warmup: Optional[str] = None,
    profile_duration: Optional[str] = None,
    resource_overrides: Optional[dict[str, object]] = None,
) -> int:
    """Discover train+eval handles, submit train, poll, submit eval."""
    mod = _load_module(module_path.resolve())
    train_handles = _find_handles_of_kind(mod, "train")
    eval_handles = _find_handles_of_kind(mod, "eval")

    if not train_handles and not eval_handles:
        print(
            f"tau python submit: no @tau.train or @tau.eval functions found in {module_path}",
            file=sys.stderr,
        )
        return 2
    if len(train_handles) > 1:
        names = ", ".join(n for n, _ in train_handles)
        print(
            f"tau python submit: multiple @tau.train handles found ({names}); "
            "v1 supports at most one per file",
            file=sys.stderr,
        )
        return 2
    if len(eval_handles) > 1:
        names = ", ".join(n for n, _ in eval_handles)
        print(
            f"tau python submit: multiple @tau.eval handles found ({names}); "
            "v1 supports at most one per file",
            file=sys.stderr,
        )
        return 2

    train_handle = train_handles[0][1] if train_handles else None
    eval_handle = eval_handles[0][1] if eval_handles else None

    # Cross-check: if eval declares `after`, it should match the train
    # handle's name (or no train handle). Mismatched chains are likely a
    # typo in the researcher's code — fail loudly rather than silently
    # ignoring.
    if eval_handle is not None and eval_handle.after:
        if train_handle is None:
            print(
                f"tau python submit: @tau.eval references after={eval_handle.after!r} "
                "but no @tau.train handle is defined in this file",
                file=sys.stderr,
            )
            return 2
        if eval_handle.after != train_handle._params.name:
            print(
                f"tau python submit: @tau.eval after={eval_handle.after!r} does "
                f"not match @tau.train name={train_handle._params.name!r} "
                "in this file (typo?)",
                file=sys.stderr,
            )
            return 2

    ns = namespace or "ray"

    if train_handle is not None:
        print(
            f"tau python submit: submitting @tau.train name={train_handle._params.name!r} "
            f"(gpus={train_handle._params.gpus}, workers={train_handle._params.workers})",
            flush=True,
        )
        train_handle.submit(
            namespace=ns,
            dry_run=dry_run,
            kube_context=kube_context,
            data_pvc=data_pvc,
            queue=queue,
            gpu_class=gpu_class,
            node_selector=node_selector,
            profiler=profiler,
            profile_rank=profile_rank,
            profile_warmup=profile_warmup,
            profile_duration=profile_duration,
            disable_default_priorities=disable_default_priorities,
            **(resource_overrides or {}),
        )

        # 2. Poll the train RayJob to completion (skip on dry-run).
        if dry_run is None and eval_handle is not None:
            rayjob_name = _resource_name_for_handle(train_handle)
            _wait_for_rayjob_success(
                rayjob_name=rayjob_name,
                run_name=train_handle._params.name,
                namespace=ns,
                kube_context=kube_context,
                timeout=timeout,
                poll_interval=poll_interval,
            )
            if not keep_train_rayjob:
                _delete_completed_rayjob(
                    rayjob_name=rayjob_name,
                    namespace=ns,
                    kube_context=kube_context,
                    timeout=cleanup_timeout,
                )

    # 3. Submit eval (if present).
    if eval_handle is not None:
        upstream_path = ""
        if train_handle is not None:
            upstream_path = _resolve_upstream_checkpoint_path(train_handle, ns, kube_context)
        else:
            # Eval-only submit needs an explicit upstream from the env.
            upstream_path = os.environ.get("TAU_UPSTREAM_CHECKPOINT", "")
            if not upstream_path:
                print(
                    "tau python submit: @tau.eval without a sibling @tau.train "
                    "needs TAU_UPSTREAM_CHECKPOINT in the env (or call "
                    "handle.submit(upstream_checkpoint=...) directly)",
                    file=sys.stderr,
                )
                return 2
        print(
            f"tau python submit: submitting @tau.eval name={eval_handle._params.name!r} "
            f"(gpus={eval_handle._params.gpus}, "
            f"cpu_workers={eval_handle._params.cpu_workers}, "
            f"upstream={upstream_path!r})",
            flush=True,
        )
        eval_handle.submit(
            upstream_checkpoint=upstream_path,
            namespace=ns,
            dry_run=dry_run,
            kube_context=kube_context,
            data_pvc=data_pvc,
            gpu_class=gpu_class,
            node_selector=node_selector,
            disable_default_priorities=disable_default_priorities,
            **(resource_overrides or {}),
        )
        if dry_run is None:
            eval_rayjob_name = _resource_name_for_handle(eval_handle)
            _wait_for_rayjob_success(
                rayjob_name=eval_rayjob_name,
                run_name=eval_handle._params.name,
                namespace=ns,
                kube_context=kube_context,
                timeout=timeout,
                poll_interval=poll_interval,
            )
            _delete_completed_rayjob(
                rayjob_name=eval_rayjob_name,
                namespace=ns,
                kube_context=kube_context,
                timeout=cleanup_timeout,
            )
    return 0


def _add_workload_override_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("-n", "--namespace", default=None, help="kubectl namespace (default: ray)")
    parser.add_argument(
        "--data-pvc",
        default=None,
        help="PVC to mount at /data for train/eval workloads (overrides storage.data_pvc)",
    )
    parser.add_argument(
        "--queue",
        default=None,
        help="Kueue local queue for train workloads (written to generated run config policy.queue)",
    )
    parser.add_argument(
        "--gpu-class",
        default=None,
        help="GPU class override, e.g. any, a100-80gb, h200-141gb, gb300-288gb (written to generated run config policy.gpu_class)",
    )
    parser.add_argument(
        "--disable-default-priorities",
        action="store_true",
        help="do not render default priorityClassName values",
    )
    parser.add_argument(
        "--node-selector",
        action="append",
        default=None,
        help="pod node selector key=value, repeatable; useful for PVC-capable node pools",
    )
    parser.add_argument(
        "--cpu-request",
        type=int,
        default=None,
        help="container CPU request in whole cores (overrides compute.cpus)",
    )
    parser.add_argument(
        "--memory-request",
        default=None,
        help="container memory request, e.g. 32Gi (overrides compute.memory)",
    )
    parser.add_argument(
        "--cpu-limit",
        type=int,
        default=None,
        help="container CPU limit in whole cores (overrides compute.cpu_limit)",
    )
    parser.add_argument(
        "--memory-limit",
        default=None,
        help="container memory limit, e.g. 32Gi (overrides compute.memory_limit)",
    )
    parser.add_argument(
        "--worker-cpu-request",
        type=int,
        default=None,
        help="Ray worker CPU request in whole cores (overrides compute.worker_cpus)",
    )
    parser.add_argument(
        "--worker-memory-request",
        default=None,
        help="Ray worker memory request, e.g. 8Gi (overrides compute.worker_memory)",
    )
    parser.add_argument(
        "--worker-cpu-limit",
        type=int,
        default=None,
        help="Ray worker CPU limit in whole cores (overrides compute.worker_cpu_limit)",
    )
    parser.add_argument(
        "--worker-memory-limit",
        default=None,
        help="Ray worker memory limit, e.g. 8Gi (overrides compute.worker_memory_limit)",
    )
    parser.add_argument(
        "--profiler",
        default=None,
        choices=("nsys",),
        help="opt-in Ray Train worker profiler for RayJob training",
    )
    parser.add_argument(
        "--profile-rank",
        default=None,
        help="rank selector for --profiler nsys: rank, comma-separated ranks, or all",
    )
    parser.add_argument(
        "--profile-warmup",
        default=None,
        help="Nsight Systems capture delay before collection starts, e.g. 30s",
    )
    parser.add_argument(
        "--profile-duration",
        default=None,
        help="Nsight Systems capture duration, e.g. 2m (default 2m in tau CLI)",
    )


def _build_overrides_from_args(args: argparse.Namespace) -> BuildOverrides:
    return BuildOverrides(
        namespace=args.namespace,
        data_pvc=args.data_pvc,
        queue=args.queue,
        gpu_class=args.gpu_class,
        node_selector=args.node_selector,
        disable_default_priorities=args.disable_default_priorities,
        cpu_request=args.cpu_request,
        memory_request=args.memory_request,
        cpu_limit=args.cpu_limit,
        memory_limit=args.memory_limit,
        worker_cpu_request=args.worker_cpu_request,
        worker_memory_request=args.worker_memory_request,
        worker_cpu_limit=args.worker_cpu_limit,
        worker_memory_limit=args.worker_memory_limit,
        profiler=args.profiler,
        profile_rank=args.profile_rank,
        profile_warmup=args.profile_warmup,
        profile_duration=args.profile_duration,
        upstream_checkpoint=getattr(args, "upstream_checkpoint", None),
    )


def main(argv: list[str] | None = None, *, prog: str = "tau python") -> int:
    raw_argv = list(argv) if argv is not None else sys.argv[1:]
    parser = argparse.ArgumentParser(prog=prog)
    sub = parser.add_subparsers(dest="cmd", required=True)

    insp = sub.add_parser(
        "inspect",
        help="print the YAML manifest a @tau.train / @tau.eval function would submit",
    )
    insp.add_argument("module", type=Path)

    build = sub.add_parser(
        "build",
        help="export a deterministic generated artifact for decorated train/eval workflows",
    )
    build.add_argument("module", type=Path)
    build.add_argument(
        "-o",
        "--output",
        type=Path,
        required=True,
        help="output directory for the generated tau.python.build artifact",
    )
    build.add_argument(
        "--force",
        action="store_true",
        help="replace an existing generated Tau build directory",
    )
    build.add_argument(
        "--upstream-checkpoint",
        default=None,
        help="required pod-side checkpoint path for eval-only exports",
    )
    _add_workload_override_args(build)

    sm = sub.add_parser(
        "submit",
        help="discover @tau.train and @tau.eval handles in <module> and submit them as a chained pipeline",
    )
    sm.add_argument("module", type=Path)
    _add_workload_override_args(sm)
    sm.add_argument("--context", default=None, dest="context", help="kubectl context")
    sm.add_argument("--dry-run", default=None, choices=("client", "server"), help="tau dry-run mode (skips polling and eval submit)")
    sm.add_argument("--timeout", default="24h", help="max time to wait for train RayJob to succeed (e.g. 30m, 2h, 24h)")
    sm.add_argument("--poll-interval", type=float, default=15.0, help="seconds between rayjob status polls")
    sm.add_argument("--cleanup-timeout", default="5m", help="max time to wait while deleting a completed train RayJob before chained eval")
    sm.add_argument(
        "--keep-train-rayjob",
        action="store_true",
        help="do not delete a completed train RayJob before chained eval (may leave the train RayCluster holding GPUs)",
    )

    submit_build = sub.add_parser(
        "submit-build",
        help="verify and submit a generated tau.python.build artifact through Go Tau",
    )
    submit_build.add_argument("build", type=Path)
    submit_build.add_argument("--context", default=None, dest="context", help="kubectl context")
    submit_build.add_argument(
        "--dry-run",
        default=None,
        choices=("client", "server"),
        help="tau dry-run mode (skips polling)",
    )
    submit_build.add_argument(
        "--timeout",
        default="24h",
        help="max time to wait for train RayJob to succeed (e.g. 30m, 2h, 24h)",
    )
    submit_build.add_argument(
        "--poll-interval",
        type=float,
        default=15.0,
        help="seconds between rayjob status polls",
    )
    submit_build.add_argument(
        "--cleanup-timeout",
        default="5m",
        help="max time to wait while deleting a completed train RayJob before chained eval",
    )
    submit_build.add_argument(
        "--keep-train-rayjob",
        action="store_true",
        help="do not delete a completed train RayJob before chained eval",
    )

    doc = sub.add_parser(
        "doctor",
        help="verify local Tau Python SDK, tau CLI, and basic kubectl prerequisites",
    )
    doc.add_argument("-n", "--namespace", default="ray", help="kubectl namespace to check (default: ray)")
    doc.add_argument("--context", default=None, dest="context", help="kubectl context")

    args = parser.parse_args(raw_argv)
    if args.cmd == "inspect":
        mod = _load_module(args.module.resolve())
        handles = _find_handles(mod)
        if not handles:
            print(f"no @tau.train or @tau.eval functions found in {args.module}", file=sys.stderr)
            return 2
        for name, h in handles:
            kind = "eval" if getattr(h, "_tau_eval_handle", False) else "train"
            print(f"# {name} (kind={kind}, source: {h.source_path})")
            yaml.safe_dump(h.manifest(), sys.stdout, sort_keys=False)
            print()
        return 0
    if args.cmd == "build":
        try:
            return _orchestrate_build(
                args.module,
                output=args.output,
                force=args.force,
                overrides=_build_overrides_from_args(args),
            )
        except (BuildArtifactError, RuntimeError, ValueError, OSError) as e:
            print(str(e), file=sys.stderr)
            return 2
    if args.cmd == "submit":
        try:
            resource_overrides = {
                key: value
                for key, value in {
                    "cpu_request": args.cpu_request,
                    "memory_request": args.memory_request,
                    "cpu_limit": args.cpu_limit,
                    "memory_limit": args.memory_limit,
                    "worker_cpu_request": args.worker_cpu_request,
                    "worker_memory_request": args.worker_memory_request,
                    "worker_cpu_limit": args.worker_cpu_limit,
                    "worker_memory_limit": args.worker_memory_limit,
                }.items()
                if value is not None
            }
            return _orchestrate_submit(
                args.module,
                namespace=args.namespace,
                kube_context=args.context,
                dry_run=args.dry_run,
                data_pvc=args.data_pvc,
                queue=args.queue,
                gpu_class=args.gpu_class,
                disable_default_priorities=args.disable_default_priorities,
                node_selector=args.node_selector,
                resource_overrides=resource_overrides,
                timeout=args.timeout,
                poll_interval=args.poll_interval,
                keep_train_rayjob=args.keep_train_rayjob,
                cleanup_timeout=args.cleanup_timeout,
                profiler=args.profiler,
                profile_rank=args.profile_rank,
                profile_warmup=args.profile_warmup,
                profile_duration=args.profile_duration,
            )
        except subprocess.CalledProcessError as e:
            print(f"tau python submit: tau CLI failed (exit={e.returncode})", file=sys.stderr)
            return e.returncode
        except RuntimeError as e:
            print(str(e), file=sys.stderr)
            return 1
    if args.cmd == "submit-build":
        try:
            return _orchestrate_submit_build(
                args.build,
                kube_context=args.context,
                dry_run=args.dry_run,
                timeout=args.timeout,
                poll_interval=args.poll_interval,
                keep_train_rayjob=args.keep_train_rayjob,
                cleanup_timeout=args.cleanup_timeout,
            )
        except subprocess.CalledProcessError as e:
            print(f"tau python submit-build: tau CLI failed (exit={e.returncode})", file=sys.stderr)
            return e.returncode
        except (BuildArtifactError, RuntimeError, ValueError, OSError) as e:
            print(str(e), file=sys.stderr)
            return 1
    if args.cmd == "doctor":
        return _doctor(kube_context=args.context, namespace=args.namespace)
    return 1


if __name__ == "__main__":
    sys.exit(main())
