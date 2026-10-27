# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded-read compatibility across HTTP client generations.

The ``widgets`` extra allows ``urllib3>=1.26``, and urllib3 1.26 has no
``HTTPResponse.read1``. A response from that generation must still be decoded by
``read_document`` and ``bounded_body``; returning it raw looks like an empty or
malformed document to every caller, which silently broke namespace discovery, run
discovery, submission planning and log reads.

These tests pin both generations: a response with ``read1``, and one with only
``read``.
"""

import io
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer
from types import SimpleNamespace

import pytest
import urllib3

from tau._kube_io import DOCUMENT_BYTES, bounded_body, read_document


class LegacyBody:
    """urllib3 1.26-shaped: no ``read1`` on the response itself.

    A real 1.26 response still exposes ``_fp``, an ``http.client.HTTPResponse``
    that does have ``read1``, so the fake carries one too. Modelling the response
    as ``read``-only would test a shape no supported client produces.
    """

    def __init__(self, data: bytes) -> None:
        self._body = io.BytesIO(data)
        self._fp = self._body
        self.closed = False
        self.reads: list[int] = []

    def read(self, size: int = -1) -> bytes:
        self.reads.append(size)
        return self._body.read(size)

    def set_read_timeout(self, timeout: float) -> None:
        assert 0 < timeout <= 5

    def close(self) -> None:
        self.closed = True
        self._body.close()


class NoRead1Body:
    """A response with no read1 anywhere must be refused, not read unbounded."""

    def __init__(self, data: bytes) -> None:
        self._body = io.BytesIO(data)
        self.closed = False

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def set_read_timeout(self, timeout: float) -> None:
        assert 0 < timeout <= 5

    def close(self) -> None:
        self.closed = True


class SocketBody:
    """urllib3 1.26 socket path: no ``set_read_timeout`` either."""

    def __init__(self, data: bytes) -> None:
        self._body = io.BytesIO(data)
        self.closed = False
        self.sock = SimpleNamespace(timeouts=[], settimeout=lambda value: self.sock.timeouts.append(value))
        self._fp = SimpleNamespace(
            read1=self._body.read1,
            fp=SimpleNamespace(raw=SimpleNamespace(_sock=self.sock)),
        )

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def close(self) -> None:
        self.closed = True
        self._body.close()


class Read1Body(io.BytesIO):
    """urllib3 2.x-shaped: ``read1`` is available."""

    def set_read_timeout(self, timeout: float) -> None:
        assert 0 < timeout <= 5


def test_read_document_decodes_a_response_without_read1():
    """Regression: the raw response must never be returned to callers."""
    body = LegacyBody(b'{"metadata":{"uid":"run"}}')

    document = read_document(lambda **kwargs: body, time.monotonic() + 10)

    assert isinstance(document, dict), "a decoded document is required, not the response object"
    assert document["metadata"]["uid"] == "run"
    assert body.closed


def test_read_document_decodes_a_response_with_read1():
    body = Read1Body(b'{"metadata":{"uid":"run"}}')
    assert read_document(lambda **kwargs: body, time.monotonic() + 10)["metadata"]["uid"] == "run"
    assert body.closed


def test_read_document_bounds_a_read_only_response():
    """The per-file ceiling still applies on the legacy read path."""
    body = LegacyBody(b" " * (DOCUMENT_BYTES + 1))
    with pytest.raises(ValueError, match="Identity document exceeded"):
        read_document(lambda **kwargs: body, time.monotonic() + 10)
    assert body.closed
    assert sum(body.reads) <= DOCUMENT_BYTES + 2


def test_bounded_body_reads_without_read1():
    """Logs and portal series read through bounded_body directly."""
    body = LegacyBody(b"x" * 100_000)
    data = bounded_body(body, time.monotonic() + 10)
    assert len(data) == 65537
    assert body.closed


def test_bounded_body_arms_the_legacy_socket_deadline():
    body = SocketBody(b"hello")
    assert bounded_body(body, time.monotonic() + 10) == b"hello"
    assert body.sock.timeouts, "the socket read timeout must be armed before each read"
    assert all(0 < value <= 5 for value in body.sock.timeouts)
    assert body.closed


def test_slow_drip_response_cannot_outlive_the_deadline():
    """A dripping peer resets the inactivity timer, so a read that waits for a
    full block can outlive the deadline. read1 keeps the loop in control."""
    total = 20
    step = 0.2

    class Drip(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Length", str(total))
            self.end_headers()
            try:
                for _ in range(total):
                    self.wfile.write(b"x")
                    self.wfile.flush()
                    time.sleep(step)
            except OSError:
                # Expected: the reader enforces its deadline and hangs up mid-drip.
                # Windows reports this as reset, aborted or broken pipe depending
                # on timing, so catch the socket base class.
                pass

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Drip)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_address[1]}/"
    manager = urllib3.PoolManager()

    def reader(**kwargs):
        kwargs.pop("_preload_content", None)
        kwargs.pop("_request_timeout", None)
        return manager.request("GET", url, preload_content=False, retries=False)

    started = time.monotonic()
    try:
        # Either our own deadline check or the client's socket timeout may fire
        # first; the contract is that the read stops near the deadline.
        with pytest.raises((TimeoutError, urllib3.exceptions.HTTPError)):
            read_document(reader, time.monotonic() + 1)
        elapsed = time.monotonic() - started
        # Waiting for the whole block would take total * step (4s); read1 lets the
        # loop notice the deadline shortly after it passes.
        assert elapsed < 3, f"slow-drip read outlived its deadline by {elapsed:.1f}s"
    finally:
        server.shutdown()


def test_chunked_slow_drip_interrupts_framing_and_releases_workers():
    """A chunked peer can drip framing bytes just under the socket inactivity
    timeout. ``http.client`` reads chunk-size lines and chunk extensions with a
    buffered ``readline`` inside ``read1``, so the per-chunk timeout never fires
    and the deadline check between chunks is never reached. The watchdog must
    break the framing read, and every reader must release its slot: four such
    drips otherwise hold all four collection slots."""
    step = 0.05

    class Drip(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            try:
                while True:
                    # An unterminated chunk-extension line: no CRLF ever arrives,
                    # so http.client keeps extending the same readline.
                    self.wfile.write(b";drip")
                    self.wfile.flush()
                    time.sleep(step)
            except OSError:
                # Expected once the reader's watchdog hangs up at the deadline.
                pass

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Drip)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_address[1]}/"

    slots = threading.BoundedSemaphore(4)
    outcomes: list[str] = []
    started = time.monotonic()

    def reader():
        with slots:
            manager = urllib3.PoolManager()
            try:
                response = manager.request("GET", url, preload_content=False, retries=False,
                                           timeout=urllib3.Timeout(connect=2, read=30))
                try:
                    bounded_body(response, time.monotonic() + 1)
                    outcomes.append("returned")
                except TimeoutError:
                    outcomes.append("deadline")
                finally:
                    response.close()
            finally:
                manager.clear()

    workers = [threading.Thread(target=reader, daemon=True) for _ in range(4)]
    for worker in workers:
        worker.start()
    for worker in workers:
        worker.join(timeout=8)
    elapsed = time.monotonic() - started
    try:
        assert not any(worker.is_alive() for worker in workers), "a drip read held its worker past the deadline"
        assert outcomes == ["deadline"] * 4, outcomes
        # The deadline, not a fast failure, is what ended each read...
        assert elapsed >= 1, f"chunked framing drip failed before its deadline ({elapsed:.2f}s)"
        # ...and it ended near the deadline rather than after the peer's drip ran on.
        assert elapsed < 4, f"chunked framing drip outlived its deadline by {elapsed:.1f}s"
        assert all(slots.acquire(blocking=False) for _ in range(4)), "collection slots were not released"
        for _ in range(4):
            slots.release()
    finally:
        server.shutdown()
        server.server_close()


def test_read_document_passes_decoded_doubles_through_unchanged():
    """Test doubles return documents, not responses; they stay untouched."""
    document = {"items": [{"metadata": {"name": "ns"}}]}
    assert read_document(lambda **kwargs: document, time.monotonic() + 10) is document


def test_bounded_body_refuses_a_response_without_read1():
    # read(size) waits for the whole block and its inactivity timer resets on a
    # drip, so it cannot honour an absolute deadline. Refuse rather than read on.
    with pytest.raises(ValueError, match="does not support bounded reads"):
        bounded_body(NoRead1Body(b"hello"), time.monotonic() + 10)


def test_bounded_body_refuses_when_no_deadline_can_be_enforced():
    # An object with neither a read timeout nor a reachable socket must not be
    # read unbounded; failing closed is the point of the deadline check.
    with pytest.raises(TimeoutError, match="Cannot enforce remaining response read deadline"):
        bounded_body(SimpleNamespace(close=lambda: None), time.monotonic() + 10)
