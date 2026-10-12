# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""IPython/Jupyter plugin loading for the TauGrid notebook panel.

A platform-authored bootstrap cell loads the plugin without the end user ever
writing an import: IPython auto-loads the load_ipython_extension below when it
is registered (e.g. via %load_ext tau.widgets.ipython) and the plugin
self-registers a %taugrid line magic.

Two modes, mirroring the TensorBoard notebook plugin:

    %load_ext tau.widgets.ipython
    %taugrid                                   # interactive widget panel
    %taugrid name=my-rayjob namespace=ray      # attach to an existing run
    %taugrid --embed --name=my-rayjob --namespace=ray --portal=http://portal:8080

The --embed mode serves the TauGrid portal run view through a separate loopback-origin
proxy and frames it in the cell output, so the notebook shows the same UI as the
portal (the TensorBoard pattern).
"""

from __future__ import annotations

import re
from typing import Any, Dict, Set, Tuple

_EMBED_KEYS = ("target", "height", "portal", "page_origin", "workspace")


def _parse_magic(line: str) -> Tuple[Set[str], Dict[str, str]]:
    """Parse a magic argument line into (flags, kwargs).

    Accepts both the TensorBoard-style space-separated form
    (--embed --name=x --namespace=y) and the comma-separated form
    (name=x, namespace=y).
    """
    flags: Set[str] = set()
    kwargs: Dict[str, str] = {}
    if not line:
        return flags, kwargs
    for raw in re.split(r"[,\s]+", line.strip()):
        token = raw.strip()
        if not token:
            continue
        if token.startswith("--"):
            token = token[2:]
            if "=" in token:
                key, _, value = token.partition("=")
                kwargs[key.strip()] = value.strip().strip("'\"")
            else:
                flags.add(token.strip())
        elif "=" in token:
            key, _, value = token.partition("=")
            kwargs[key.strip()] = value.strip().strip("'\"")
    return flags, kwargs


def _truthy(value: Any) -> bool:
    return str(value).strip().lower() in ("1", "true", "yes", "on")


def load_ipython_extension(ipython: Any) -> None:  # pragma: no cover - ipython
    """Register the %taugrid line magic on the running IPython instance."""
    from IPython.core.magic import Magics, line_magic, magics_class  # type: ignore[import-not-found]

    from tau.widgets.panel import panel

    @magics_class
    class _TauWidgetMagics(Magics):
        @line_magic
        def taugrid(self, line: str) -> Any:
            flags, kwargs = _parse_magic(line)

            # name= attaches to an existing run; it is not a constructor arg.
            run_name = kwargs.pop("name", None)
            if "portal" in kwargs:
                kwargs["portal_url"] = kwargs.pop("portal")

            embed = "embed" in flags or _truthy(kwargs.pop("embed", "false"))
            embed_opts: Dict[str, Any] = {}
            if embed:
                for key in _EMBED_KEYS:
                    if key in kwargs:
                        embed_opts[key] = kwargs.pop(key)
                if "portal" in embed_opts:
                    embed_opts["portal_url"] = embed_opts.pop("portal")
                if "height" in embed_opts:
                    embed_opts["height"] = int(embed_opts["height"])

            ctrl = panel(**kwargs)  # type: ignore[call-arg]
            if run_name:
                ctrl.load(str(run_name))

            if embed:
                # TensorBoard pattern: the cell output is the framed portal UI.
                return ctrl.embed(**embed_opts)

            # Return the panel, not a string: Jupyter renders its
            # _repr_html_ as text/html, so the browser gets real SVG and class
            # hooks instead of an escaped text repr.
            return ctrl

        @line_magic
        def tau(self, line: str) -> Any:  # compatibility alias
            return self.taugrid(line)

    ipython.register_magics(_TauWidgetMagics)


def unload_ipython_extension(ipython: Any) -> None:  # pragma: no cover - ipython
    """Remove the %taugrid magic when the extension is unloaded."""
    try:
        from tau.widgets.panel import TauGridPanel  # noqa: F401

        del TauGridPanel
    except Exception:
        pass


__all__ = ["load_ipython_extension", "unload_ipython_extension"]
