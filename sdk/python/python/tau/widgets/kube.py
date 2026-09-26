# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Kubernetes client access for the notebook widget.

The client is lazily built from kubeconfig / in-cluster config, and is
injectable so the panel is testable offline with fakes (no cluster, no network).
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass
class ClusterClient:
    """Bundle of the two Kubernetes API surfaces the widget uses.

    ``custom`` is a ``CustomObjectsApi``-shaped object; ``core`` is a
    ``CoreV1Api``-shaped object. Each is injectable for offline tests.
    """

    custom: Any
    core: Any
    batch: Any = None
    owned_api_client: Any = None

    def close(self):
        if self.owned_api_client is not None:
            self.owned_api_client.close()


def load_client(*, core: Any = None, custom: Any = None, batch: Any = None, retries: int | None = None) -> ClusterClient:
    """Build a ``ClusterClient`` from runtime config or the injected fakes.

    When ``core``/``custom`` are provided they are used as-is (offline tests pass
    fakes here). Otherwise the ``kubernetes`` client config is loaded lazily.
    """
    if core is not None and custom is not None:
        return ClusterClient(custom=custom, core=core, batch=batch)

    from kubernetes import client, config  # lazy: only when a real cluster is used

    try:
        config.load_incluster_config()
    except Exception:  # pragma: no cover - exercised when NOT in a cluster
        try:
            config.load_kube_config()
        except Exception:  # pragma: no cover - no config available
            config.load_config()

    configuration = client.Configuration.get_default_copy()
    if retries is not None:
        configuration.retries = retries
    api = client.ApiClient(configuration)
    return ClusterClient(custom=client.CustomObjectsApi(api), core=client.CoreV1Api(api), batch=client.BatchV1Api(api), owned_api_client=api)


__all__ = ["ClusterClient", "load_client"]
