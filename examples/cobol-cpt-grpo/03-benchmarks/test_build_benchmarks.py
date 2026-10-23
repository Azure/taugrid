# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for the prompt boundary in build_benchmarks.

Needs no cobc and no GPU:
  python -m pytest examples/cobol-cpt-grpo/03-benchmarks/test_build_benchmarks.py -v

Checks that a PROCEDURE DIVISION header in any case ends the prompt prefix, so the
procedure body that holds the reference never appears in a prompt. Also checks
that a paragraph task keeps the procedure text after the masked paragraph as its
suffix, and that a program task keeps the original program.
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


MULTI_PARAGRAPH = (
    "       IDENTIFICATION DIVISION.\n"
    "       PROGRAM-ID. MULTI.\n"
    "       DATA DIVISION.\n"
    "       WORKING-STORAGE SECTION.\n"
    "       01 WS-COUNT PIC 9(4).\n"
    "       PROCEDURE DIVISION.\n"
    "       INIT-PARA.\n"
    "           MOVE 0 TO WS-COUNT.\n"
    "       MAIN-PARA.\n"
    "           PERFORM INIT-PARA\n"
    "           PERFORM ADD-PARA\n"
    "           DISPLAY WS-COUNT\n"
    "           STOP RUN.\n"
    "\n"
    "       ADD-PARA.\n"
    "           ADD 2 TO WS-COUNT.\n"
    "       END-PARA.\n"
    "           EXIT.\n"
)


def _code_lines(text):
    return [ln.rstrip() for ln in text.split("\n") if ln.strip()]


def test_paragraph_task_keeps_the_rest_of_the_procedure(bench):
    tasks = bench.build_paragraph_completion(
        [{"text": MULTI_PARAGRAPH, "is_complete_program": True, "source": "m.cbl"}]
    )
    assert len(tasks) == 1
    task = tasks[0]
    assert task["paragraph_name"] == "MAIN-PARA"
    assert "ADD-PARA." in task["suffix"]
    assert "END-PARA." in task["suffix"]
    assert "ADD-PARA." not in task["prompt"]
    assert "ADD-PARA." not in task["reference"]
    assert _code_lines(
        task["prompt"] + task["reference"] + task["suffix"]
    ) == _code_lines(MULTI_PARAGRAPH)


def test_program_task_keeps_the_original_program(bench):
    tasks = bench.build_program_completion(
        [
            {
                "text": MULTI_PARAGRAPH
                + "".join(f"           DISPLAY 'LINE {i}'.\n" for i in range(5)),
                "is_complete_program": True,
            }
        ]
    )
    assert len(tasks) == 1
    assert tasks[0]["reference_text"].startswith(MULTI_PARAGRAPH)
