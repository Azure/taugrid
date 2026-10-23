# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Load an existing RayJob from the current kubeconfig and print its status.

This is the real-cluster smoke test for the notebook plugin's read path. It
uses the same code the panel uses (tau.widgets.status.read_run_status) against
the cluster selected by the active kube context, so it proves the plugin can
load a job from a Kubernetes cluster and check its status without a notebook.

Usage:

    python tools/test_plugin_cluster.py <namespace> <rayjob-name>

Exit codes: 0 when the run was found and read, 1 when it was not found, 2 when
there is no usable kubeconfig or client.
"""

from __future__ import annotations

import sys

from tau.widgets.kube import load_client
from tau.widgets.panel import TauGridPanel


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: test_plugin_cluster.py <namespace> <rayjob-name>")
        return 2

    namespace, name = sys.argv[1], sys.argv[2]
    try:
        client = load_client()
    except Exception as exc:
        print(f"no usable Kubernetes client: {exc}")
        return 2

    panel = TauGridPanel(namespace=namespace, client=client)
    status = panel.load(name)

    print(f"run        : {status.namespace}/{status.name}")
    print(f"existing   : {status.existing}")
    print(f"state      : {status.state} ({status.display_state})")
    print(f"queue      : {status.queue}")
    print(f"ray cluster: {status.ray_cluster_name}")
    print(f"job id     : {status.job_id}")
    print(f"deployment : {status.deployment_status}")
    print(f"pods       : {status.ready_pods}/{status.total_pods} ready")
    for pod in status.pods:
        print(f"  - {pod.name} {pod.phase} ready={pod.ready} restarts={pod.restarts} node={pod.node}")
    for diag in status.diagnostics:
        print(f"  [{diag.severity}] {diag.code}: {diag.message}")
    print(f"header     : {panel._status_html()}")

    return 0 if status.existing else 1


if __name__ == "__main__":
    sys.exit(main())
