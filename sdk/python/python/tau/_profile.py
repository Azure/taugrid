# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Resolve a worker profile and Kueue queue from a ``tau.azure.com`` TauCluster.

The widget reads the cluster-scoped ``TauCluster`` CRD (kind ``cluster`` in the
group ``tau.azure.com``) to populate the profile and queue dropdowns and to pick
defaults for submit. Everything here is a pure function of the CRD document, so
tests can pass a plain dict and stay offline.
"""

from __future__ import annotations


from typing import Any, Dict, Optional, Tuple

from tau._render import Profile, _worker_resources, default_image

DEFAULT_QUEUE_FALLBACK = "default"


class NoTauClusterFound(Exception):
    """No readable ``TauCluster/cluster`` document was supplied."""


def _optional_float(value: Any) -> Optional[float]:
    if value in (None, ""):
        return None
    number = float(value)
    if isinstance(value, bool):
        raise ValueError("cpusPerWorker must be a positive finite number")
    return number


def _optional_str(value: Any) -> Optional[str]:
    if value in (None, ""):
        return None
    return str(value)


def default_queue(cluster: Dict[str, Any]) -> Optional[str]:
    """Read ``spec.workspaceDefaults.defaultQueue`` if present."""
    spec = cluster.get("spec") or {}
    workspace_defaults = spec.get("workspaceDefaults") or {}
    return _optional_str(workspace_defaults.get("defaultQueue"))


def workload_profiles(cluster: Dict[str, Any]) -> list[Profile]:
    """Map ``spec.workloadProfiles[]`` to resolved :class:`Profile` objects.

    Only ready profiles are returned when a status block sets a non-empty state.
    """
    spec = cluster.get("spec") or {}
    raw = spec.get("workloadProfiles") or []
    profiles: list[Profile] = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        status = item.get("status") or {}
        state = _optional_str(status.get("state"))
        if state is not None and state != "ready":
            continue
        name = _optional_str(item.get("name"))
        if not name:
            continue
        # The TauCluster CRD declares these flat on the profile
        # (workerCount/gpusPerWorker/cpusPerWorker); the nested compute.* shape is
        # accepted too so older or alternative documents still resolve.
        compute = item.get("compute") or {}
        workers = compute.get("workerReplicas", item.get("workerCount", 1))
        gpus = compute.get("gpusPerWorker", item.get("gpusPerWorker", 0))
        cpus = compute.get("cpusPerWorker", item.get("cpusPerWorker"))
        image = compute.get("image", item.get("image"))
        priorities = item.get("priorities") or {}
        selector = item.get("nodeSelector") or {}
        profiles.append(
            Profile(
                name=name,
                workers=workers,
                gpus_per_worker=gpus,
                num_cpus_per_worker=_optional_float(cpus),
                priority_class=_optional_str(
                    item.get("priorityClass") or priorities.get("podPriorityClassName")
                ),
                image=_optional_str(image),
                node_selector=selector or None,
                memory_per_worker=_optional_str(compute.get("memoryPerWorker", item.get("memoryPerWorker"))),
            )
        )
    return profiles


def resolve(cluster: Dict[str, Any], explicit_queue: Optional[str] = None) -> Tuple[list[Profile], str]:
    """Return ``(profiles, effective_queue)`` for the supplied ``TauCluster``.

    Raises :class:`NoTauClusterFound` when ``cluster`` is empty/None. The queue
    is the explicit one when given, else the workspace default, else fallback.
    """
    if not cluster:
        raise NoTauClusterFound(
            "no TauCluster found: looked for the cluster-scoped resource "
            "TauCluster (group tau.azure.com, kind cluster)"
        )
    profiles = workload_profiles(cluster)
    queue = explicit_queue or default_queue(cluster) or DEFAULT_QUEUE_FALLBACK
    return profiles, queue


def destination_profiles(cluster: Dict[str, Any], limit: int = 200) -> Dict[str, Any]:
    profiles, queue = resolve(cluster)
    descriptions = {
        item.get("name"): str(item.get("description") or "")[:1024]
        for item in (cluster.get("spec") or {}).get("workloadProfiles", [])
        if isinstance(item, dict)
    }
    rows = []
    for profile in profiles[:limit]:
        resources = _worker_resources(profile)
        rows.append({
            "name": profile.name,
            "description": descriptions.get(profile.name, ""),
            "workers": profile.workers,
            "gpusPerWorker": profile.gpus_per_worker,
            "cpusPerWorker": resources["cpu"],
            "memoryPerWorker": resources["memory"],
            "priority": profile.priority_class,
            "image": profile.image or default_image(),
            "queue": queue,
        })
    return {"profiles": rows, "defaultQueue": queue, "profilesTruncated": len(profiles) > limit}


__all__ = [
    "Profile",
    "NoTauClusterFound",
    "resolve",
    "workload_profiles",
    "default_queue",
    "DEFAULT_QUEUE_FALLBACK",
]
