# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""TensorBoard-style inline embedding for the notebook plugin.

The TensorBoard notebook plugin works by serving its UI locally and embedding it
in an iframe in the cell output. TauGrid follows the same shape: the magic
starts a small local reverse proxy in the kernel, points it at the TauGrid
portal, and embeds the portal run view in an iframe.

The portal sets X-Frame-Options: SAMEORIGIN, so a notebook served from a
different origin cannot frame it directly. The proxy is a separate loopback origin, not the notebook origin. It removes
upstream frame restrictions: use this opt-in legacy helper only with a trusted
local kernel and portal. Remote kernels require explicit port forwarding.
The native JupyterLab plugin never uses this proxy.

Everything here is offline-testable: the proxy takes its upstream URL and the
iframe helper is a pure function.
"""

from __future__ import annotations

import html
import threading
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, Optional

#: Hop-by-hop headers must not be forwarded by a proxy.
_HOP_BY_HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
}

#: Headers the proxy drops so the notebook can frame the portal.
_FRAME_HEADERS = {"x-frame-options"}


def run_view_path(namespace: str, name: str) -> str:
    """Portal path for a run's detail board (/portal/runs/<ns>/<name>)."""
    ns = urllib.parse.quote(str(namespace), safe="")
    nm = urllib.parse.quote(str(name), safe="")
    return f"/portal/runs/{ns}/{nm}"


def ray_dashboard_path(namespace: str, ray_cluster_name: str) -> str:
    """Portal path that reverse-proxies a RayCluster's Ray dashboard."""
    ns = urllib.parse.quote(str(namespace), safe="")
    cluster = urllib.parse.quote(str(ray_cluster_name), safe="")
    return f"/api/portal/ray/proxy/{ns}/{cluster}/"


def experiment_path(run_id: str) -> str:
    """Portal path for a run's Stellar experiments view."""
    return "/stellar?target=" + urllib.parse.quote(str(run_id), safe="")


def same_origin(a: str, b: str) -> bool:
    """True when two URLs share scheme, host, and effective port."""
    pa, pb = urllib.parse.urlparse(a), urllib.parse.urlparse(b)
    return (pa.scheme, pa.hostname, _port(pa)) == (pb.scheme, pb.hostname, _port(pb))


def _port(parsed: urllib.parse.ParseResult) -> Optional[int]:
    if parsed.port is not None:
        return parsed.port
    return {"http": 80, "https": 443}.get(parsed.scheme)


def iframe_html(url: str, *, height: int = 640, title: str = "TauGrid run view", link: Optional[str] = None) -> str:
    """A framed portal view plus a direct link fallback.

    The link is always rendered so a blocked frame is never a dead end.
    """
    safe_url = html.escape(url, quote=True)
    safe_title = html.escape(title, quote=True)
    parts = [
        f'<iframe src="{safe_url}" title="{safe_title}" loading="lazy" '
        f'style="width:100%;height:{int(height)}px;border:1px solid #e2e8f0;'
        f'border-radius:8px;background:#fff"></iframe>'
    ]
    if link:
        parts.append(
            f'<div class="tg-embed-link" style="font-size:12px;margin-top:4px">'
            f'<a href="{html.escape(link, quote=True)}" target="_blank" rel="noopener noreferrer">'
            f"Open in the TauGrid portal</a></div>"
        )
    return "".join(parts)


@dataclass
class EmbedView:
    """The result of an embed: the framed URL plus a direct portal link."""

    url: str
    path: str
    portal_url: str
    html: str
    proxied: bool = False

    def _repr_html_(self) -> str:
        return self.html

    def __str__(self) -> str:
        return self.url


class PortalProxy:
    """A local reverse proxy that serves the TauGrid portal on a separate loopback origin.

    Starts a ThreadingHTTPServer on 127.0.0.1 and forwards every request to the
    portal base URL, dropping X-Frame-Options so the notebook can frame it. The
    portal SPA uses root-absolute asset and API paths, so a transparent proxy
    works without HTML rewriting.
    """

    def __init__(
        self,
        portal_url: str,
        *,
        host: str = "127.0.0.1",
        port: int = 0,
        headers: Optional[Dict[str, str]] = None,
        timeout: float = 30.0,
    ) -> None:
        self.portal_url = portal_url.rstrip("/")
        self.host = host
        self._requested_port = port
        self.headers = dict(headers or {})
        self.timeout = timeout
        self._server: Optional[ThreadingHTTPServer] = None
        self._thread: Optional[threading.Thread] = None

    @property
    def port(self) -> int:
        if self._server is None:
            return self._requested_port
        return int(self._server.server_address[1])

    @property
    def base_url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def url(self, path: str) -> str:
        if not path.startswith("/"):
            path = "/" + path
        return self.base_url + path

    def start(self) -> "PortalProxy":
        if self._server is not None:
            return self
        handler = _make_handler(self.portal_url, self.headers, self.timeout)
        self._server = ThreadingHTTPServer((self.host, self._requested_port), handler)
        self._server.daemon_threads = True  # type: ignore[attr-defined]
        self._thread = threading.Thread(target=self._server.serve_forever, name="tau-portal-proxy", daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None
        if self._thread is not None:
            self._thread.join(timeout=5)
            self._thread = None


def _strip_frame_ancestors(csp: str) -> str:
    """Remove frame-ancestors for the opt-in legacy embed."""
    directives = [d.strip() for d in csp.split(";") if d.strip()]
    kept = [d for d in directives if not d.lower().startswith("frame-ancestors")]
    return "; ".join(kept)


def _make_handler(portal_url: str, headers: Dict[str, str], timeout: float) -> type:
    class _Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self) -> None:  # noqa: N802 - http.server API
            self._forward("GET")

        def do_HEAD(self) -> None:  # noqa: N802 - http.server API
            self._forward("HEAD")

        def _forward(self, method: str) -> None:
            target = portal_url + self.path
            request = urllib.request.Request(target, method=method, headers=dict(headers))
            try:
                with urllib.request.urlopen(request, timeout=timeout) as response:
                    body = response.read()
                    self._relay(response.status, response.headers.items(), body, method)
            except urllib.error.HTTPError as exc:
                body = exc.read()
                self._relay(exc.code, exc.headers.items() if exc.headers else [], body, method)
            except Exception as exc:  # pragma: no cover - network failure path
                message = f"TauGrid portal proxy could not reach {target}: {exc}".encode("utf-8")
                self.send_response(502)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.send_header("Content-Length", str(len(message)))
                self.end_headers()
                if method != "HEAD":
                    self.wfile.write(message)

        def _relay(self, status: int, header_items: Any, body: bytes, method: str) -> None:
            self.send_response(status)
            for key, value in header_items:
                lower = key.lower()
                if lower in _HOP_BY_HOP or lower in _FRAME_HEADERS:
                    continue
                if lower == "content-security-policy":
                    value = _strip_frame_ancestors(value)
                if lower == "content-length":
                    continue
                self.send_header(key, value)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if method != "HEAD":
                self.wfile.write(body)

        def log_message(self, *args: Any) -> None:  # keep the notebook output clean
            return

    return _Handler


__all__ = [
    "EmbedView",
    "PortalProxy",
    "run_view_path",
    "ray_dashboard_path",
    "experiment_path",
    "same_origin",
    "iframe_html",
]
