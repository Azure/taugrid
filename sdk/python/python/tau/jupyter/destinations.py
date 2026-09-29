# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded read-only choices for the native submission review."""

from tau._kube_io import read_document
from tau._notebook_submit import _read_tau_cluster
from tau._profile import destination_profiles
from tau.jupyter.runs import LIMIT, ReadError, list_namespaces, listing_items, truncated, validate_target

PROFILE_LIMIT = 200


def list_destinations(client, namespace=None):
    discovery = list_namespaces(client)
    namespaces = discovery["namespaces"]
    warnings = list(discovery["warnings"])
    if namespace:
        validate_target(namespace)
        if not any(row["name"] == namespace for row in namespaces):
            raise ReadError("Namespace is not in the visible list. Refresh destinations or ask the operator for namespace read permission.")
    elif namespaces:
        namespace = namespaces[0]["name"]
    catalog = {"profiles": [], "defaultQueue": None}
    try:
        catalog = destination_profiles(_read_tau_cluster(client, None), PROFILE_LIMIT)
        if catalog.pop("profilesTruncated"):
            warnings.append(f"Showing the first {PROFILE_LIMIT} resolved profiles; ask the operator to narrow the catalog if yours is missing.")
    except Exception as exc:
        warnings.append(f"Profiles could not be resolved. Ask the operator to publish ready profiles and grant TauCluster read permission, then refresh. Details: {exc}")
    queues = []
    if namespace:
        try:
            listing = read_document(client.custom.list_namespaced_custom_object,
                                    group="kueue.x-k8s.io", version="v1beta1", plural="localqueues",
                                    namespace=namespace, limit=LIMIT)
            items = listing_items(listing)
            queues = sorted({item["metadata"]["name"] for item in items[:LIMIT]
                             if isinstance(item, dict) and isinstance(item.get("metadata"), dict)
                             and isinstance(item["metadata"].get("name"), str)})
            if truncated(listing) or len(items) >= LIMIT:
                warnings.append(f"Queue discovery reached its {LIMIT}-queue bound; refresh or ask the operator if your queue is missing.")
        except Exception as exc:
            warnings.append(f"Queues could not be listed. Ask the operator for LocalQueue read permission and check the cluster connection, then refresh. Details: {exc}")
    if catalog["defaultQueue"] and catalog["defaultQueue"] not in queues:
        warnings.append(f"The resolved default queue {catalog['defaultQueue']} is not in this namespace's visible queue list. Select a visible queue or ask the operator to configure the default.")
    return {**discovery, **catalog, "namespace": namespace, "queues": queues,
            "warnings": warnings,
            "limits": {"namespaces": LIMIT, "profiles": PROFILE_LIMIT, "queues": LIMIT}}
