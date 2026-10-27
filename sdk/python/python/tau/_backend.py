# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Direct Kubernetes backend shared by native notebooks and legacy widgets.

The existing CLI-executed SDK APIs are separate; notebook submission never
invokes them. Injectable API clients keep this backend offline-testable.
"""

from __future__ import annotations

from tau._kube_io import read_document

from dataclasses import dataclass
from typing import Any, Dict, Optional, Protocol, runtime_checkable

@runtime_checkable
class CustomObjectsApiProtocol(Protocol):
    """Duck-typed surface of ``kubernetes.client.CustomObjectsApi`` we use."""

    def create_namespaced_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        body: dict[str, Any],
        **kwargs: Any,
    ) -> Any: ...  # pragma: no cover - delegated to the real client

    def get_namespaced_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        name: str,
        **kwargs: Any,
    ) -> Any: ...  # pragma: no cover - delegated to the real client

    def list_cluster_custom_object(
        self,
        group: str,
        version: str,
        plural: str,
        **kwargs: Any,
    ) -> Any: ...  # pragma: no cover - delegated to the real client


@dataclass(frozen=True)
class SubmittedRun:
    """Identifier of a successfully applied run object."""

    name: str
    namespace: str
    kind: str = "RayJob"


class KubernetesBackend:
    """Submit path that talks straight to the Kubernetes API server.

    Runtime deps are the Kubernetes SDK clients supplied via the constructor;
    the backend performs no shell-out. ``custom_objects`` is injectable for
    offline tests. The ``core`` argument remains for caller compatibility but is
    not used by the create operation.
    """

    def __init__(
        self,
        custom_objects: Optional[Any] = None,
        core: Optional[Any] = None,
    ) -> None:
        self._custom = custom_objects if custom_objects is not None else _lazy_custom_api()

    def submit_rayjob(self, manifest: Dict[str, Any], namespace: str) -> SubmittedRun:
        """Apply the rendered RayJob and return a normalized run handle.

        The raw API response is a dict (the real client returns one), so the
        handle is derived from the applied manifest: design AC S008 ("apply via
        the SDK, get a handle with name and namespace").
        """
        body = dict(manifest)
        read_document(self._custom.create_namespaced_custom_object,
            group="ray.io",
            version="v1",
            namespace=namespace,
            plural="rayjobs",
            body=body,
            _request_timeout=(2, 5),
        )
        metadata = body.get("metadata") or {}
        return SubmittedRun(
            name=str(metadata.get("name", "")),
            namespace=namespace,
            kind=str(body.get("kind", "RayJob")),
        )


def _lazy_custom_api() -> Any:
    from kubernetes import client  # pragma: no cover - exercised when kubernetes installed

    return client.CustomObjectsApi()
