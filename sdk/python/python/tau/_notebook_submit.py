# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Shared pure-Python notebook packaging, profile resolution and submission.

Native Jupyter uses K8sJobMode/600s behind its release gate; legacy widgets
retain HTTPMode/15s. Callers supply identity and may opt into local staging.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any, Dict, List, Mapping, Optional

from tau._backend import KubernetesBackend, SubmittedRun
from tau._notebook_pkg import MAX_INPUT_BYTES, NotebookInvalid, StagedPayload, package
from tau._payload import MAX_DECODED_BYTES, MAX_ENV_ENTRY_BYTES, PayloadTooLarge
from tau._profile import NoTauClusterFound, resolve as resolve_profile_queue
from tau._render import Profile, UnsupportedShape, render_rayjob
from tau._kube_io import read_document

if TYPE_CHECKING:
    from tau.widgets.kube import ClusterClient

TAU_CLUSTER_GROUP = "tau.azure.com"
TAU_CLUSTER_VERSION = "v1alpha1"
TAU_CLUSTER_PLURAL = "clusters"

#: Cap on a submitted notebook payload name, to keep the RayJob name valid.
MAX_NAME_LENGTH = 63


class SubmitError(ValueError):
    """A user-actionable refusal; status is the HTTP code the API should use."""

    def __init__(self, message: str, status: int = 400) -> None:
        super().__init__(message)
        self.status = status


@dataclass(frozen=True)
class SubmitPlan:
    """Everything that will be created, resolved before any cluster write."""

    manifest: Dict[str, Any]
    name: str
    namespace: str
    queue: str
    profile: str
    digest: str
    notebook_bytes: int
    prepared_bytes: int
    encoded_env_bytes: int
    excluded_cells: List[str] = field(default_factory=list)
    staged: StagedPayload | None = None
    file_sizes: Dict[str, int] = field(default_factory=dict)

    def summary(self) -> Dict[str, Any]:
        spec = self.manifest["spec"]
        groups = spec["rayClusterSpec"]["workerGroupSpecs"]
        resources = [group["template"]["spec"]["containers"][0]["resources"]["requests"] for group in groups]
        return {
            "workers": sum(group["replicas"] for group in groups),
            "cpusPerWorker": [item["cpu"] for item in resources],
            "memoryPerWorker": [item["memory"] for item in resources],
            "fileSizes": self.file_sizes,
            "packagedFileSizes": {key: len(value) for key, value in self.staged.encoding.files.items()} if self.staged else {},
            "decodedBytes": self.staged.encoding.decoded_bytes if self.staged else None,
            "payloadBudgetBytes": MAX_DECODED_BYTES,
            "encodedBudgetBytes": MAX_ENV_ENTRY_BYTES,
            "shutdownAfterFinish": spec["shutdownAfterJobFinishes"],
            "planDigest": hashlib.sha256(json.dumps(self.manifest, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
            "name": self.name,
            "namespace": self.namespace,
            "queue": self.queue,
            "profile": self.profile,
            "payloadDigest": self.digest,
            "notebookBytes": self.notebook_bytes,
            "preparedBytes": self.prepared_bytes,
            "encodedEnvBytes": self.encoded_env_bytes,
            "excludedCells": self.excluded_cells,
            "includedFiles": list(self.staged.included_files) if self.staged else [],
            "submissionMode": self.manifest["spec"]["submissionMode"],
            "retentionSeconds": self.manifest["spec"]["ttlSecondsAfterFinished"],
            "submitterImage": self.manifest["spec"]["rayClusterSpec"]["headGroupSpec"]["template"]["spec"]["containers"][0]["image"],
            "gpusPerWorker": [int(group["rayStartParams"].get("num-gpus", "0")) for group in self.manifest["spec"]["rayClusterSpec"]["workerGroupSpecs"]],
        }


@dataclass(frozen=True)
class SubmitResult:
    name: str
    namespace: str
    kind: str
    digest: str

    def as_dict(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "namespace": self.namespace,
            "kind": self.kind,
            "payloadDigest": self.digest,
        }


def _read_tau_cluster(client: ClusterClient, cluster: Optional[Mapping[str, Any]]) -> Mapping[str, Any]:
    """Return an explicit TauCluster or the sole bounded discovery result."""
    if cluster is not None:
        return cluster
    if client is None:
        raise NoTauClusterFound("profile resolution needs a TauCluster client or an explicit profile")
    listing = read_document(client.custom.list_cluster_custom_object,
                            group=TAU_CLUSTER_GROUP, version=TAU_CLUSTER_VERSION,
                            plural=TAU_CLUSTER_PLURAL, limit=2)
    if isinstance(listing, Mapping):
        items = listing.get("items")
        if (listing.get("metadata") or {}).get("continue") or (isinstance(items, list) and len(items) > 1):
            raise SubmitError("TauCluster discovery is ambiguous; expected exactly one complete result", status=409)
        if isinstance(items, list) and len(items) == 1:
            return items[0]

    raise SubmitError(
        "no TauCluster found: the cluster has no tau.azure.com/clusters resource, so no "
        "worker profile or default queue can be resolved",
        status=409,
    )


def _select_profile(profiles: List[Profile], requested: Optional[str]) -> Profile:
    if not profiles:
        raise SubmitError(
            "the TauCluster exposes no ready workload profile; ask the platform owner to "
            "publish one before submitting",
            status=409,
        )
    if requested:
        for profile in profiles:
            if profile.name == requested:
                return profile
        names = ", ".join(sorted(p.name for p in profiles))
        raise SubmitError(f"profile {requested!r} is not available; choose one of: {names}")
    return profiles[0]


def _safe_name(raw: str) -> str:
    """Coerce a user string into a DNS-1123-friendly RayJob name."""

    cleaned = re.sub(r"[^a-z0-9]+", "-", raw.lower()).strip("-")
    return cleaned[:MAX_NAME_LENGTH].strip("-") or "notebook-run"


def build_plan(
    *,
    client: ClusterClient,
    notebook_bytes: bytes,
    namespace: str = "default",
    name: Optional[str] = None,
    profile: Optional[str | Profile] = None,
    queue: Optional[str] = None,
    pip: Optional[List[str]] = None,
    extra_files: Optional[Mapping[str, bytes]] = None,
    env: Optional[Mapping[str, str]] = None,
    env_secret: Optional[Mapping[str, str]] = None,
    cluster: Optional[Mapping[str, Any]] = None,
    input_cap: int = MAX_INPUT_BYTES,
    staging_dir: Path | None = None,
    submission_mode: str = "HTTPMode",
    ttl_seconds: int = 15,
) -> SubmitPlan:
    """Resolve everything the submission needs, without writing to the cluster.

    Raises SubmitError for anything a user can fix (bad notebook, oversize,
    unknown profile, no TauCluster).
    """
    if not isinstance(namespace, str) or not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?", namespace):
        raise SubmitError("namespace must be a DNS label")

    try:
        staged = package(
        notebook_bytes, staging_dir=staging_dir, input_cap=input_cap, extra_files=extra_files
    )
    except NotebookInvalid as exc:
        raise SubmitError(f"cannot submit this notebook: {exc}") from exc
    except PayloadTooLarge as exc:
        raise SubmitError(str(exc), status=413) from exc

    if isinstance(profile, Profile):
        selected, effective_queue = profile, queue or "default"
    else:
        try:
            cluster_doc = _read_tau_cluster(client, cluster)
            profiles, effective_queue = resolve_profile_queue(cluster_doc, explicit_queue=queue)
        except NoTauClusterFound as exc:
            raise SubmitError(str(exc), status=409) from exc
        except SubmitError:
            raise
        except Exception as exc:
            raise SubmitError(f"could not resolve TauCluster: {exc}", status=502) from exc
        selected = _select_profile(profiles, profile)
    stem = Path(name).stem if name else "notebook"
    run_name = _safe_name(name or f"{stem}-{staged.encoding.digest[:8]}")

    try:
        manifest = render_rayjob(
            name=run_name,
            namespace=namespace,
            queue=effective_queue,
            profile=selected,
            entrypoint=staged.entrypoint,
            runtime_env={"pip": list(pip or [])} if pip else None,
            env=dict(env or {}) or None,
            env_secret=dict(env_secret or {}) or None,
            payload=staged.encoding,
            submission_mode=submission_mode,
            ttl_seconds=ttl_seconds,
        )
    except UnsupportedShape as exc:
        raise SubmitError(str(exc)) from exc

    return SubmitPlan(
        manifest=manifest,
        name=run_name,
        namespace=namespace,
        queue=effective_queue,
        profile=selected.name,
        digest=staged.encoding.digest,
        notebook_bytes=staged.byte_count,
        prepared_bytes=staged.prepared_bytes,
        encoded_env_bytes=staged.encoding.env_entry_bytes,
        excluded_cells=list(staged.dropped_cells),
        staged=staged,
        file_sizes={key: len(value) for key, value in (extra_files or {}).items()},
    )


def submit_plan(
    *,
    client: ClusterClient,
    plan: SubmitPlan,
    backend: Optional[KubernetesBackend] = None,
) -> SubmitResult:
    """Apply a resolved plan. Refuses to replace an existing run of the same name."""
    backend = backend or KubernetesBackend(
        custom_objects=client.custom if client else None,
        core=client.core if client else None,
    )
    try:
        handle: SubmittedRun = backend.submit_rayjob(plan.manifest, namespace=plan.namespace)
    except Exception as exc:
        raise SubmitError(f"the cluster rejected the RayJob: {exc}", status=409 if getattr(exc, "status", None) == 409 else 502) from exc
    return SubmitResult(
        name=handle.name, namespace=handle.namespace, kind=handle.kind, digest=plan.digest
    )


__all__ = [
    "SubmitError",
    "SubmitPlan",
    "SubmitResult",
    "build_plan",
    "submit_plan",
]
