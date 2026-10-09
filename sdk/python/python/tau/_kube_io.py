# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded, non-preloaded Kubernetes and HTTP reads shared by notebook surfaces.

Responses are read with ``read1`` when the HTTP client provides it (urllib3 2.x)
and with a bounded ``read`` otherwise. urllib3 1.26.x — still allowed by the
``widgets`` extra — has no ``read1``, and returning such a response undecoded
silently breaks every caller, because they treat the result as a document.

Both paths arm a socket read timeout before each blocking read, so a stalled peer
cannot outlive the caller's deadline regardless of which generation is installed.
"""

from __future__ import annotations

import json
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


def _arm_read_timeout(response, remaining):
    """Bound the next blocking read by ``remaining`` seconds."""
    setter = getattr(response, "set_read_timeout", None)
    if callable(setter):
        setter(remaining)
        return
    # urllib3 1.26 exposes the socket through the file wrapper instead.
    raw = getattr(getattr(getattr(response, "_fp", None), "fp", None), "raw", None)
    sock = getattr(raw, "_sock", None)
    if sock is not None:
        sock.settimeout(remaining)
        return
    isclosed = getattr(response, "isclosed", None)
    if not callable(isclosed) or not isclosed():
        raise TimeoutError("Cannot enforce remaining response read deadline")


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
    try:
        while len(result) <= limit:
            remaining = timeout(deadline)[1]
            _arm_read_timeout(response, remaining)
            chunk = _read_chunk(response, min(CHUNK_BYTES, limit + 1 - len(result)))
            if not chunk:
                break
            result.extend(chunk)
            timeout(deadline)
        return bytes(result)
    finally:
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
