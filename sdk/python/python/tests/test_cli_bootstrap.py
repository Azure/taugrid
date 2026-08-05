"""Runtime CLI integration tests."""

from pathlib import Path

import pytest

from tau import cli as tau_cli


def _write_tau_binary(
    path: Path,
    body: bytes = (
        b'#!/bin/sh\n'
        b'test "$1" = "version" && test "$2" = "--short" || exit 2\n'
        b'echo v9.9.9\n'
    ),
) -> None:
    path.write_bytes(body)
    path.chmod(0o755)


def test_doctor_rejects_cli_sdk_version_skew(tmp_path, monkeypatch, capsys):
    binary = tmp_path / "tau"
    _write_tau_binary(binary)
    monkeypatch.setattr(tau_cli, "_find_tau_binary", lambda: str(binary))

    rc = tau_cli._doctor(kube_context=None, namespace="ray")

    assert rc == 1
    assert "version mismatch" in capsys.readouterr().err


def test_main_uses_canonical_prog_name(capsys):
    with pytest.raises(SystemExit):
        tau_cli.main(["--help"])

    out = capsys.readouterr().out
    assert "usage: tau python" in out
