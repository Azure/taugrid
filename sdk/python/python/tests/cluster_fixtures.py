# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Shared TauCluster fixtures shaped like the real CRD.

The notebook plugin reads readiness and applicability from
``status.workloadProfiles``, which the controller publishes as a
``ProfileSetStatus`` whose ``profiles[]`` entries embed the full
``WorkloadProfile`` inline. A spec-only document is therefore *declaration
without resolution*, and must not be treated as ready.

``ready_cluster`` takes compact profile definitions, keeps any legacy
scheduling extras on the spec entry (``cpusPerWorker``, ``memoryPerWorker``,
``image``, ``nodeSelector`` — none of which the CRD models), and derives a
matching status catalog with current-generation Ready conditions.
"""

from __future__ import annotations

from typing import Any, Dict, Iterable, List, Optional

READY_CONDITION = "Ready"

#: CRD fields copied onto the resolved status entry verbatim when present.
_CRD_PASSTHROUGH = (
    "description",
    "applicability",
    "defaultLocalQueue",
    "priorities",
    "priorityClass",
    "mode",
    "placement",
    "executionTarget",
)


def _worker_count(item: Dict[str, Any]) -> Any:
    compute = item.get("compute") or {}
    return compute.get("workerReplicas", item.get("workerCount", 1))


def _gpus_per_worker(item: Dict[str, Any]) -> Any:
    compute = item.get("compute") or {}
    return compute.get("gpusPerWorker", item.get("gpusPerWorker", 0))


def ready_cluster(
    profiles: Iterable[Dict[str, Any]],
    *,
    generation: int = 1,
    observed_generation: Optional[int] = None,
    default_queue: Optional[str] = None,
    spec_default_queue: Optional[str] = None,
) -> Dict[str, Any]:
    """Build a TauCluster document with a consistent resolved status catalog.

    Each profile may set ``ready: False`` to publish a non-Ready condition, or
    supply ``conditions`` to control readiness precisely (for testing stale
    generations). ``applicability`` on a profile is copied to both the spec
    entry and the resolved status entry.
    """
    spec_profiles: List[Dict[str, Any]] = []
    status_profiles: List[Dict[str, Any]] = []
    for raw in profiles:
        item = dict(raw)
        ready = item.pop("ready", True)
        conditions = item.pop("conditions", None)
        resolved: Dict[str, Any] = {
            "name": item["name"],
            "workerCount": _worker_count(item),
            "gpusPerWorker": _gpus_per_worker(item),
        }
        for key in _CRD_PASSTHROUGH:
            if key in item:
                resolved[key] = item[key]
        if conditions is None:
            conditions = [
                {
                    "type": READY_CONDITION,
                    "status": "True" if ready else "False",
                    "observedGeneration": generation,
                    "reason": "Reconciled" if ready else "CapacityUnavailable",
                    "message": "" if ready else "no capacity published yet",
                }
            ]
        resolved["conditions"] = conditions
        status_profiles.append(resolved)
        spec_profiles.append(item)

    document: Dict[str, Any] = {
        "metadata": {"generation": generation},
        "spec": {"workloadProfiles": spec_profiles},
        "status": {
            "workloadProfiles": {
                "observedGeneration": (
                    generation if observed_generation is None else observed_generation
                ),
                "profiles": status_profiles,
            }
        },
    }
    if spec_default_queue is not None:
        document["spec"]["workspaceDefaults"] = {"defaultQueue": spec_default_queue}
    elif default_queue is not None:
        document["spec"]["workspaceDefaults"] = {"defaultQueue": default_queue}
    return document


__all__ = ["ready_cluster", "READY_CONDITION"]
