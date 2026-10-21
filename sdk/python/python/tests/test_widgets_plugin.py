# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline test that the plugin loads as an IPython/Jupyter extension."""

import importlib


def test_plugin_entry_point_resolves():
    """The ``ipython.extensions`` entry point resolves to the loader."""
    from importlib.metadata import entry_points

    eps = entry_points(group="ipython.extensions")
    matches = [ep for ep in eps if ep.name == "taugrid"]
    assert matches, "tau[widgets] must register an 'ipython.extensions' entry point 'taugrid'"
    load = matches[0].load()
    assert callable(load)


def test_load_ipython_extension_registers_magic():
    """Installing the extension registers the %taugrid magic (no notebook needed)."""
    ipy = _FakeInterm()
    mod = importlib.import_module("tau.widgets.ipython")
    mod.load_ipython_extension(ipy)
    # The %taugrid line magic is defined on the registered magics class.
    assert len(ipy.registered) == 1
    assert hasattr(ipy.registered[0], "taugrid")
    # Unload must not raise.
    mod.unload_ipython_extension(ipy)


def test_taugrid_magic_returns_panel():
    """%taugrid returns the panel, whose _repr_html_ renders as real HTML."""
    from tau.widgets.ipython import load_ipython_extension
    from tau.widgets.panel import TauGridPanel
    from IPython.core.interactiveshell import InteractiveShell

    shell = InteractiveShell.instance()
    load_ipython_extension(shell)
    result = shell.run_line_magic("taugrid", "")
    assert isinstance(result, TauGridPanel)
    # The notebook display convention: _repr_html_ carries the real HTML.
    html = result._repr_html_()
    assert html.startswith("<div")
    assert "taugrid-panel" in html


class _FakeInterm:
    """Minimal IPython stub that records registered magic classes."""

    def __init__(self) -> None:
        self.registered = []

    def register_magics(self, magics: object) -> None:
        self.registered.append(magics)

    def run_line_magic(self, name: str, line: str) -> object:
        return None