# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for the prompt boundary in build_benchmarks.

Needs no cobc and no GPU:
  python -m pytest examples/cobol-cpt-grpo/03-benchmarks/test_build_benchmarks.py -v

Checks that a PROCEDURE DIVISION header in any case ends the prompt prefix, so the
procedure body that holds the reference never appears in a prompt.
"""

import importlib.util
import os

import pytest

BENCH_DIR = os.path.dirname(os.path.abspath(__file__))

PROGRAM = (
    "       IDENTIFICATION DIVISION.\n"
    "       PROGRAM-ID. MIXED.\n"
    "       DATA DIVISION.\n"
    "       WORKING-STORAGE SECTION.\n"
    "       01 WS-COUNT PIC 9(4).\n"
    "       Procedure Division.\n"
    "       MAIN-PARA.\n"
    "           MOVE 1 TO WS-COUNT\n"
    "           ADD 2 TO WS-COUNT\n"
    "           DISPLAY 'SECRET REFERENCE LINE'\n"
    "           STOP RUN.\n"
)


@pytest.fixture
def bench(tmp_path, monkeypatch):
    monkeypatch.setenv("OUT", str(tmp_path / "eval"))
    spec = importlib.util.spec_from_file_location(
        "build_benchmarks", os.path.join(BENCH_DIR, "build_benchmarks.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.mark.parametrize(
    "header", ["PROCEDURE DIVISION", "Procedure Division", "procedure division"]
)
def test_prefix_stops_at_procedure_division_in_any_case(bench, header):
    text = PROGRAM.replace("Procedure Division", header)
    prefix = bench.text_before_procedure_division(text)
    assert prefix == text[: text.index(header)]
    assert "SECRET REFERENCE LINE" not in prefix


def test_prefix_is_empty_without_procedure_division(bench):
    assert bench.text_before_procedure_division("       DATA DIVISION.\n") == ""


def test_paragraph_prompt_does_not_leak_mixed_case_body(bench):
    tasks = bench.build_paragraph_completion(
        [{"text": PROGRAM, "is_complete_program": True, "source": "mixed.cbl"}]
    )
    assert len(tasks) == 1
    assert "SECRET REFERENCE LINE" in tasks[0]["reference"]
    assert "SECRET REFERENCE LINE" not in tasks[0]["prompt"]
    assert "WS-COUNT PIC 9(4)" in tasks[0]["prompt"]
