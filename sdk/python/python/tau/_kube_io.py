# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded, non-preloaded Kubernetes and HTTP reads shared by notebook surfaces.

Responses are read with ``read1`` when the HTTP client provides it (urllib3 2.x)
and with the ``_fp.read1`` urllib3 1.26 exposes otherwise. urllib3 1.26.x — still
allowed by the ``widgets`` extra — has no ``read1`` of its own, and returning such
a response undecoded silently breaks every caller, because they treat the result
as a document.

A socket read timeout armed before each read bounds *inactivity*, not the whole
read. ``http.client`` reads chunked framing (chunk-size lines, chunk extensions,
trailers) with a buffered ``readline`` before ``read1`` is ever reached, so a peer
dripping framing bytes just under that timeout holds the read forever. A watchdog
therefore also tears the response and its socket down at the absolute deadline,
which interrupts framing reads as well as body reads.
"""

from __future__ import annotations

import json
import socket
import threading
import time

DOCUMENT_BYTES = 4 * 1024 * 1024
CHUNK_BYTES = 4096


def timeout(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("Notebook API deadline expired")
    return min(2, remaining / 2), min(5, remaining / 2)


def _readable(response):
    """True when the object is an HTTP-like response we must decode ourselves."""
    return callable(getattr(response, "read1", None)) or callable(getattr(response, "read", None))


def _response_socket(response):
    """Return the socket under a response, when one is reachable.

    Framing reads happen inside ``http.client`` behind a buffered ``readline``,
    so the watchdog below must be able to tear the socket down from another
    thread to interrupt them; a timeout alone only bounds inactivity.
    """
    candidates = (
        getattr(response, "_sock", None),
        getattr(getattr(response, "_fp", None), "_sock", None),
        # urllib3 1.26 exposes the socket through the file wrapper instead.
        getattr(getattr(getattr(getattr(response, "_fp", None), "fp", None), "raw", None), "_sock", None),
        getattr(getattr(response, "_connection", None), "sock", None),
    )
    return next((candidate for candidate in candidates if candidate is not None), None)


def _arm_read_timeout(response, remaining):
    """Bound the next blocking read's inactivity by ``remaining`` seconds."""
    setter = getattr(response, "set_read_timeout", None)
    if callable(setter):
        setter(remaining)
        return
    sock = _response_socket(response)
    if sock is not None:
        sock.settimeout(remaining)
        return
    isclosed = getattr(response, "isclosed", None)
    if not callable(isclosed) or not isclosed():
        raise TimeoutError("Cannot enforce remaining response read deadline")


class _DeadlineWatchdog:
    """Interrupt a response read once the caller's absolute deadline passes.

    ``read1`` bounds body reads, but not the framing reads ``http.client`` does
    behind it, so the loop above cannot be relied on to regain control. Tearing
    the connection down from a timer thread unblocks whatever read is in flight
    -- framing or body -- and ``bounded_body`` turns the resulting failure into
    the deadline error.
    """

    def __init__(self, response, deadline):
        self._response = response
        self._timer = threading.Timer(max(0.0, deadline - time.monotonic()), self._interrupt)
        self._timer.daemon = True
        self._timer.start()

    def _interrupt(self):
        shutdown = getattr(_response_socket(self._response), "shutdown", None)
        if callable(shutdown):
            try:
                shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        try:
            self._response.close()
        except Exception:
            pass

    def cancel(self):
        self._timer.cancel()


def _read_chunk(response, size):
    """Read up to ``size`` bytes without waiting for the whole body.

    A plain ``read(size)`` waits for ``size`` bytes or EOF. The socket timeout
    armed just before the call bounds *inactivity*, not the whole read, so a peer
    that drips a byte at a time resets that timer indefinitely and the call can
    outlive the caller's deadline. ``read1`` returns as soon as any data is
    available, which is what lets the loop below re-check the deadline.

    urllib3 1.26 exposes no ``read1`` of its own, but its ``_fp`` is an
    ``http.client.HTTPResponse``, which has had ``read1`` since Python 3.3. Use it
    so the legacy generation keeps the same deadline behaviour instead of falling
    back to an unbounded read.
    """
    for candidate in (getattr(response, "read1", None),
                      getattr(getattr(response, "_fp", None), "read1", None)):
        if callable(candidate):
            return candidate(size)
    raise ValueError("Response object does not support bounded reads")


def bounded_body(response, deadline, limit=65536):
    result = bytearray()
    watchdog = _DeadlineWatchdog(response, deadline)
    try:
        while len(result) <= limit:
            remaining = timeout(deadline)[1]
            try:
                _arm_read_timeout(response, remaining)
                chunk = _read_chunk(response, min(CHUNK_BYTES, limit + 1 - len(result)))
            except Exception:
                # The watchdog fires at the deadline and closes the response (and
                # shuts its socket down), so a blocking framing or body read fails
                # instead of waiting forever. Report that as the deadline error
                # rather than leaking the incidental transport exception.
                if time.monotonic() >= deadline:
                    raise TimeoutError("Notebook API deadline expired") from None
                raise
            if not chunk:
                break
            result.extend(chunk)
            timeout(deadline)
        # A watchdog-triggered close can surface as a clean EOF, so re-check the
        # deadline before reporting success.
        timeout(deadline)
        return bytes(result)
    finally:
        watchdog.cancel()
        response.close()


def read_document(reader, deadline=None, *, limit_bytes=DOCUMENT_BYTES, **kwargs):
    deadline = time.monotonic() + 7 if deadline is None else deadline
    kwargs["_request_timeout"] = timeout(deadline)
    response = reader(**kwargs, _preload_content=False)
    # Test doubles hand back the document directly. A real HTTP response always
    # exposes a read, and is always decoded here: returning it raw would look
    # like an empty or malformed document to every caller.
    if not _readable(response):
        return response
    data = bounded_body(response, deadline, limit_bytes)
    if len(data) > limit_bytes:
        raise ValueError("Identity document exceeded byte limit")
    return json.loads(data)
