#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise real kubectl raw-delete transport against an isolated loopback mock."""

import json
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


def main():
    path = "/apis/adx-mon.azure.com/v1/namespaces/test/functions/test-gpu-health"
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_DELETE(self):
            self.connection.settimeout(5)
            if self.headers.get("Transfer-Encoding") == "chunked":
                chunks = []
                while True:
                    size = int(self.rfile.readline().strip(), 16)
                    if size == 0:
                        assert self.rfile.readline() == b"\r\n"
                        break
                    chunks.append(self.rfile.read(size))
                    assert self.rfile.read(2) == b"\r\n"
                raw = b"".join(chunks)
            else:
                raw = self.rfile.read(int(self.headers["Content-Length"]))
            body = json.loads(raw)
            requests.append((self.path, body))
            matches = body.get("preconditions") == {"uid": "current-uid", "resourceVersion": "10"}
            code = 200 if matches else 409
            response = json.dumps({
                "apiVersion": "v1", "kind": "Status",
                "status": "Success" if matches else "Failure",
                "reason": "" if matches else "Conflict",
                "message": "deleted" if matches else "precondition failed",
                "code": code,
            }).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(response)))
            self.end_headers()
            self.wfile.write(response)

    with HTTPServer(("127.0.0.1", 0), Handler) as server, tempfile.TemporaryDirectory() as directory:
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            config = Path(directory) / "kubeconfig.json"
            config.write_text(json.dumps({
                "apiVersion": "v1", "kind": "Config",
                "clusters": [{"name": "mock", "cluster": {"server": f"http://127.0.0.1:{server.server_port}"}}],
                "contexts": [{"name": "mock", "context": {"cluster": "mock", "user": "mock"}}],
                "users": [{"name": "mock", "user": {}}],
                "current-context": "mock",
            }))
            for name, uid, version, expected_code in (
                ("current object", "current-uid", "10", 0),
                ("recreated same name", "old-uid", "10", 1),
                ("updated same UID", "current-uid", "9", 1),
            ):
                options = {
                    "apiVersion": "v1", "kind": "DeleteOptions",
                    "preconditions": {"uid": uid, "resourceVersion": version},
                }
                result = subprocess.run(
                    ["kubectl", "delete", "--raw", path, "--filename", "-",
                     "--kubeconfig", str(config), "--request-timeout=2s"],
                    input=json.dumps(options), text=True, capture_output=True, timeout=10,
                    check=False,
                )
                assert result.returncode == expected_code, (name, result.stdout, result.stderr)
                assert requests[-1] == (path + "?timeout=2s", options), requests
                if expected_code:
                    assert "Error from server (Conflict):" in result.stderr, result.stderr
                print(f"PASS kubectl transport: {name}: exit={result.returncode}, exact DeleteOptions preserved")
            assert len(requests) == 3, requests
        finally:
            server.shutdown()
            worker.join(timeout=5)


if __name__ == "__main__":
    main()
