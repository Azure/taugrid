# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Backward-compatible import shim for ``tau.serve``."""

from tau.serve import ServeHandle, serve

__all__ = ["ServeHandle", "serve"]
