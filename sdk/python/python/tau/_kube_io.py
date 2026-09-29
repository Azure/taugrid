# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded, non-preloaded Kubernetes reads shared by notebook surfaces."""

from __future__ import annotations

import json
import time

DOCUMENT_BYTES = 4 * 1024 * 1024


def timeout(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("Notebook API deadline expired")
    return min(2, remaining / 2), min(5, remaining / 2)


def bounded_body(response, deadline, limit=65536):
    result = bytearray()
    try:
        while len(result) <= limit:
            remaining = timeout(deadline)[1]
            setter = getattr(response, "set_read_timeout", None)
            if setter:
                setter(remaining)
            else:
                raw = getattr(getattr(getattr(response, "_fp", None), "fp", None), "raw", None)
                sock = getattr(raw, "_sock", None)
                if sock is not None:
                    sock.settimeout(remaining)
                elif not response.isclosed():
                    raise TimeoutError("Cannot enforce remaining response read deadline")
            chunk = response.read1(min(4096, limit + 1 - len(result)))
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
    if not hasattr(response, "read1"):
        return response
    data = bounded_body(response, deadline, limit_bytes)
    if len(data) > limit_bytes:
        raise ValueError("Identity document exceeded byte limit")
    return json.loads(data)
