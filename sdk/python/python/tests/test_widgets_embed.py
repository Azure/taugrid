# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline tests for the TensorBoard-style embed module and its integrations.

Everything here is local: a tiny stdlib HTTP server stands in for the portal
and the proxy binds an ephemeral port on 127.0.0.1. No cluster, no real portal,
and no external network are touched.
"""

import json
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from tau.widgets.embed import (
    PortalProxy,
    experiment_path,
    iframe_html,
    ray_dashboard_path,
    run_view_path,
    same_origin,
)
from tau.widgets.ipython import _parse_magic
from tau.widgets.panel import TauGridPanel
from tau.widgets.status import RunStatus


# --- fake portal upstream -------------------------------------------------


class _FakePortalHandler(BaseHTTPRequestHandler):
    """Serves one framed HTML page, one JSON path, and a 404 for anything else."""

    protocol_version = "HTTP/1.1"

    def do_GET(self) -> None:  # noqa: N802 - http.server API
        self._respond("GET")

    def do_HEAD(self) -> None:  # noqa: N802 - http.server API
        self._respond("HEAD")

    def _respond(self, method: str) -> None:
        if self.path == "/page":
            body = b"<html>portal page</html>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
        elif self.path == "/data.json":
            body = json.dumps({"ok": True, "path": self.path}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
        else:
            body = b"not found"
            self.send_response(404)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if method != "HEAD":
            self.wfile.write(body)

    def log_message(self, *args: object) -> None:  # keep pytest output clean
        return


class _FakePortal:
    """A local stand-in for the TauGrid portal, bound to an ephemeral port."""

    def __init__(self) -> None:
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), _FakePortalHandler)
        self._server.daemon_threads = True  # type: ignore[attr-defined]
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def __enter__(self) -> "_FakePortal":
        return self

    def __exit__(self, *exc: object) -> bool:
        self.stop()
        return False


# --- path builders --------------------------------------------------------


def test_run_view_path_quotes_both_segments():
    assert run_view_path("ray", "my-job") == "/portal/runs/ray/my-job"
    # A slash and a space must both be escaped, not treated as path structure.
    assert run_view_path("my ns", "a/b") == "/portal/runs/my%20ns/a%2Fb"


def test_ray_dashboard_path_quotes_segments():
    assert ray_dashboard_path("ray", "cluster") == "/api/portal/ray/proxy/ray/cluster/"
    assert ray_dashboard_path("my ns", "cluster/1") == "/api/portal/ray/proxy/my%20ns/cluster%2F1/"


def test_experiment_path_quotes_run_id():
    assert experiment_path("job-123") == "/stellar?target=job-123"
    assert experiment_path("job id/1") == "/stellar?target=job%20id%2F1"


# --- same_origin ----------------------------------------------------------


def test_same_origin_true_for_matching_scheme_host_and_default_port():
    assert same_origin("http://portal.example.com/a", "http://portal.example.com/b") is True
    assert same_origin("http://portal.example.com:80/a", "http://portal.example.com/b") is True
    assert same_origin("https://portal.example.com/a", "https://portal.example.com:443/b") is True


def test_same_origin_false_for_scheme_host_or_port_mismatch():
    assert same_origin("http://portal.example.com", "https://portal.example.com") is False
    assert same_origin("http://portal.example.com:8080", "http://portal.example.com") is False
    assert same_origin("http://a.example.com", "http://b.example.com") is False


# --- iframe_html ----------------------------------------------------------


def test_iframe_html_escapes_url_and_title():
    url = 'http://portal.example.com/x"><script>alert(1)</script>'
    title = '"<b>run</b>'
    out = iframe_html(url, height=500, title=title)
    assert "height:500px" in out
    assert "<script>" not in out
    assert "<b>" not in out
    assert "&lt;script&gt;" in out
    assert "&quot;" in out
    # No link fallback unless one is supplied.
    assert "<a href=" not in out


def test_iframe_html_renders_escaped_link_fallback():
    url = "http://portal.example.com/portal/runs/ray/job"
    link = 'http://portal.example.com/portal/runs/ray/job?a=1&b=2"<'
    out = iframe_html(url, link=link)
    assert "<a href=" in out
    assert "Open in the TauGrid portal" in out
    assert "&amp;" in out
    assert '"<' not in out


# --- PortalProxy ----------------------------------------------------------


def test_portal_proxy_relays_body_and_relaxes_frame_headers():
    with _FakePortal() as upstream:
        proxy = PortalProxy(upstream.base_url).start()
        try:
            with urllib.request.urlopen(proxy.url("/page"), timeout=10) as response:
                assert response.status == 200
                assert response.read() == b"<html>portal page</html>"
                assert response.headers.get("X-Frame-Options") is None
                csp = response.headers.get("Content-Security-Policy")
                assert csp is not None
                assert "frame-ancestors" not in csp.lower()
                assert "default-src 'self'" in csp
        finally:
            proxy.stop()


def test_portal_proxy_relays_json_path():
    with _FakePortal() as upstream:
        proxy = PortalProxy(upstream.base_url).start()
        try:
            with urllib.request.urlopen(proxy.url("/data.json"), timeout=10) as response:
                payload = json.loads(response.read().decode("utf-8"))
                assert payload == {"ok": True, "path": "/data.json"}
        finally:
            proxy.stop()


def test_portal_proxy_relays_upstream_404_not_502():
    with _FakePortal() as upstream:
        proxy = PortalProxy(upstream.base_url).start()
        try:
            with pytest.raises(urllib.error.HTTPError) as excinfo:
                urllib.request.urlopen(proxy.url("/missing"), timeout=10)
            assert excinfo.value.code == 404
            assert excinfo.value.read() == b"not found"
        finally:
            proxy.stop()


def test_portal_proxy_forwards_head_without_body():
    with _FakePortal() as upstream:
        proxy = PortalProxy(upstream.base_url).start()
        try:
            request = urllib.request.Request(proxy.url("/page"), method="HEAD")
            with urllib.request.urlopen(request, timeout=10) as response:
                assert response.status == 200
                assert response.read() == b""
                assert response.headers.get("X-Frame-Options") is None
        finally:
            proxy.stop()


# --- panel.embed ----------------------------------------------------------


def test_panel_embed_run_same_origin_is_direct():
    panel = TauGridPanel(namespace="ray", run_name="my run", portal_url="https://portal.example.com")
    view = panel.embed(target="run", page_origin="https://portal.example.com")
    assert view.proxied is False
    assert view.path == "/portal/runs/ray/my%20run"
    assert view.portal_url == "https://portal.example.com"
    assert view.url == "https://portal.example.com/portal/runs/ray/my%20run"
    assert view._repr_html_() == view.html
    assert "<iframe" in view.html
    assert 'href="https://portal.example.com/portal/runs/ray/my%20run"' in view.html


def test_panel_embed_cross_origin_uses_local_proxy():
    with _FakePortal() as upstream:
        panel = TauGridPanel(namespace="ray", run_name="job", portal_url=upstream.base_url)
        try:
            view = panel.embed(target="run", page_origin="http://notebook.example.com")
            assert view.proxied is True
            assert view.url.startswith("http://127.0.0.1:")
            assert view.url.endswith("/portal/runs/ray/job")
            assert panel._proxy is not None
        finally:
            panel.close()
        assert panel._proxy is None


def test_panel_embed_without_page_origin_proxies():
    with _FakePortal() as upstream:
        panel = TauGridPanel(namespace="ray", run_name="job", portal_url=upstream.base_url)
        try:
            view = panel.embed(target="run")
            assert view.proxied is True
            assert view.url.endswith("/portal/runs/ray/job")
        finally:
            panel.close()


def test_panel_embed_ray_and_experiments_paths():
    panel = TauGridPanel(namespace="ray", run_name="job", portal_url="https://portal.example.com")
    panel.status = RunStatus(
        name="job",
        namespace="ray",
        ray_cluster_name="cluster/1",
        job_id="job id/1",
    )
    ray = panel.embed(target="ray", page_origin="https://portal.example.com")
    assert ray.path == "/api/portal/ray/proxy/ray/cluster%2F1/"
    assert ray.proxied is False
    experiments = panel.embed(target="experiments", page_origin="https://portal.example.com")
    assert experiments.path == "/stellar?target=job%20id%2F1"


def test_panel_embed_run_requires_run_name():
    panel = TauGridPanel(namespace="ray", run_name="")
    with pytest.raises(ValueError):
        panel.embed(target="run")


def test_panel_embed_ray_requires_resolved_cluster():
    panel = TauGridPanel(namespace="ray", run_name="job")
    with pytest.raises(ValueError):
        panel.embed(target="ray")


def test_panel_embed_experiments_requires_job_id():
    panel = TauGridPanel(namespace="ray", run_name="job")
    panel.status = RunStatus(name="job", namespace="ray")
    with pytest.raises(ValueError):
        panel.embed(target="experiments")


def test_panel_embed_unknown_target_raises():
    panel = TauGridPanel(namespace="ray", run_name="job")
    with pytest.raises(ValueError):
        panel.embed(target="bogus")


def test_panel_close_stops_proxy_and_watcher():
    class _FakeWatcher:
        def __init__(self) -> None:
            self.stopped = False

        def stop(self) -> None:
            self.stopped = True

    with _FakePortal() as upstream:
        panel = TauGridPanel(namespace="ray", run_name="job", portal_url=upstream.base_url)
        watcher = _FakeWatcher()
        panel.watcher = watcher  # type: ignore[assignment]
        try:
            panel.embed(target="run")
            assert panel._proxy is not None
        finally:
            panel.close()
        assert watcher.stopped is True
        assert panel.watcher is None
        assert panel._proxy is None


# --- _parse_magic ---------------------------------------------------------


def test_parse_magic_space_separated_tensorboard_style():
    flags, kwargs = _parse_magic("--embed --name=x --namespace=y --height=500")
    assert flags == {"embed"}
    assert kwargs == {"name": "x", "namespace": "y", "height": "500"}


def test_parse_magic_comma_separated():
    flags, kwargs = _parse_magic("name=x, namespace=y")
    assert flags == set()
    assert kwargs == {"name": "x", "namespace": "y"}


def test_parse_magic_strips_surrounding_quotes():
    flags, kwargs = _parse_magic("name=\"myrun\", namespace='ray'")
    assert flags == set()
    assert kwargs == {"name": "myrun", "namespace": "ray"}


def test_parse_magic_empty_line():
    assert _parse_magic("") == (set(), {})
