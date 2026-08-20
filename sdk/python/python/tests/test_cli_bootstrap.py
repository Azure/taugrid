# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import pytest

from tau import cli as tau_cli


def test_main_uses_canonical_prog_name(capsys):
    with pytest.raises(SystemExit):
        tau_cli.main(["--help"])

    out = capsys.readouterr().out
    assert "usage: tau python" in out
