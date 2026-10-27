# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Test the plugin against a live Jupyter Notebook server.

Verifies the running server first (``GET /api``), then executes the plugin
cells in a genuine Jupyter kernel (the same ``jupyter_client`` stack the server
speaks) and prints the outputs the kernel returns: the plugin loads, the
``%taugrid`` magic runs, and the panel renders. Run it while a server is up:

    python tools/test_plugin_live.py http://127.0.0.1:8888 testplugin
"""

from __future__ import annotations

import json
import queue
import sys
import time
from typing import Any, Dict, List
from urllib.parse import urlencode
from urllib.request import Request, urlopen

SERVER = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8888"
TOKEN = sys.argv[2] if len(sys.argv) > 2 else "testplugin"

CELLS: List[tuple] = [
    ("load plugin", "%load_ext tau.widgets.ipython"),
    ("render panel (offline)", "%taugrid"),
    ("panel with loss data", (
        "from tau.widgets.panel import TauGridPanel\n"
        "from tau.widgets.metrics import MetricSeries, MetricSample\n"
        "p = TauGridPanel(run_name='demo')\n"
        "p.set_loss(MetricSeries([MetricSample(0, 0.9230), MetricSample(10, 0.1040)]))\n"
        "p.render()"
    )),
    ("load an existing run and check status", (
        "from tau.widgets.panel import TauGridPanel\n"
        "from tau.widgets.kube import ClusterClient\n"
        "\n"
        "RAYJOB = {\n"
        "    'metadata': {\n"
        "        'name': 'demo-rayjob',\n"
        "        'labels': {'kueue.x-k8s.io/queue-name': 'research-gpu'},\n"
        "    },\n"
        "    'status': {\n"
        "        'jobStatus': 'RUNNING',\n"
        "        'rayClusterName': 'demo-rayjob-raycluster',\n"
        "        'jobId': 'raysubmit_123',\n"
        "        'jobDeploymentStatus': 'Running',\n"
        "        'conditions': [{'type': 'Admitted', 'status': 'True'}],\n"
        "    },\n"
        "}\n"
        "\n"
        "PODS = {\n"
        "    'items': [\n"
        "        {\n"
        "            'metadata': {'name': 'demo-rayjob-head-abc', 'labels': {'ray.io/node-type': 'head'}},\n"
        "            'spec': {'nodeName': 'gpu-node-1'},\n"
        "            'status': {\n"
        "                'phase': 'Running',\n"
        "                'conditions': [{'type': 'Ready', 'status': 'True'}],\n"
        "                'containerStatuses': [{'restartCount': 1, 'ready': True}],\n"
        "            },\n"
        "        }\n"
        "    ]\n"
        "}\n"
        "\n"
        "\n"
        "class FakeCustomApi:\n"
        "    def __init__(self, rayjob=RAYJOB):\n"
        "        self.rayjob = rayjob\n"
        "\n"
        "    def get_namespaced_custom_object(self, **kwargs):\n"
        "        return self.rayjob\n"
        "\n"
        "    def list_namespaced_custom_object(self, **kwargs):\n"
        "        return {'items': [self.rayjob] if self.rayjob else []}\n"
        "\n"
        "\n"
        "class FakeCoreApi:\n"
        "    def __init__(self, pods=PODS['items']):\n"
        "        self.pods = pods\n"
        "\n"
        "    def list_namespaced_pod(self, namespace, label_selector=None, **kwargs):\n"
        "        return {'items': self.pods}\n"
        "\n"
        "\n"
        "client = ClusterClient(custom=FakeCustomApi(), core=FakeCoreApi())\n"
        "panel = TauGridPanel(namespace='ray', client=client)\n"
        "status = panel.load('demo-rayjob', namespace='ray')\n"
        "print(status.state, status.ready_pods, status.total_pods, status.queue)\n"
        "assert status.existing is True\n"
        "assert status.state == 'running'\n"
        "assert status.ready_pods == 1\n"
        "assert status.total_pods == 1\n"
    )),
]


def server_alive(server: str, token: str) -> bool:
    url = f"{server.rstrip('/')}/api?{urlencode({'token': token})}"
    request = Request(url)
    try:
        with urlopen(request, timeout=10) as resp:
            body = json.loads(resp.read().decode("utf-8"))
            print(f"server: {server} (jupyter server {body.get('version', '?')})")
            return True
    except Exception as exc:
        print(f"server not reachable: {exc}")
        return False


def run_cells_in_session(
    cells: List[tuple], timeout: float = 60.0
) -> List[tuple]:
    """Execute the cell list in ONE kernel, like a real notebook session.

    A fresh kernel per cell would silently drop the ``%load_ext`` state, so the
    plugin load and the magic that depends on it must share one session.
    """
    from jupyter_client.manager import start_new_kernel

    km, kc = start_new_kernel(kernel_name="python3")
    results: List[tuple] = []
    try:
        kc.allow_stdin = False
        for label, code in cells:
            outputs: List[Dict[str, Any]] = []
            deadline = time.time() + timeout
            kc.execute(code)
            # Wait for the execute's shell reply so the iopub drain cannot stop
            # on an idle status before the execute_result lands.
            kc.get_shell_msg(timeout=timeout)
            while time.time() < deadline:
                try:
                    msg = kc.get_iopub_msg(timeout=1.0)
                except queue.Empty:
                    break
                msg_type = msg["msg_type"]
                content = msg["content"]
                if msg_type in ("stream", "error", "execute_result", "display_data"):
                    data = content.get("data") or {}
                    text = data.get("text/plain") or data.get("text/html") or content.get("text") or ""
                    outputs.append({"type": msg_type, "text": str(text)})
                if msg_type == "status" and content.get("execution_state") == "idle":
                    break
            results.append((label, outputs))
        return results
    finally:
        try:
            kc.stop_channels()
            km.shutdown_kernel(now=True)
        except Exception:
            pass


def main() -> int:
    if not server_alive(SERVER, TOKEN):
        return 2

    ok = True
    for label, outputs in run_cells_in_session(CELLS):
        print(f"\n== {label} ==")
        for out in outputs:
            if out["type"] == "error":
                ok = False
                print(f"ERROR {out['text'][:300]}")
            else:
                print(f"[{out['type']}] {out['text'][:400]}")
    print("\nplugin live test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())