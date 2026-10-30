# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for the eval preflight in run_eval_vllm.

vllm is replaced with a stub, so this needs CPU torch only:
  python -m pytest examples/cobol-cpt-grpo/07-eval/test_run_eval.py -v

Checks that a missing or empty suite, a suite built without the current task
fields, and a missing or broken cobc fail the run before any model is loaded,
that a cobc that disappears mid-run fails instead of counting tasks as skipped,
and that prompts too long for the model context are set aside and recorded,
failing the run if a suite is left empty.

Also checks that paragraph completions are cut at the next paragraph and
compiled with the rest of the original procedure. With GnuCOBOL on PATH, a
reference completion for a paragraph that PERFORMs a later one must compile.
"""

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import types
from unittest import mock

import pytest

EVAL_DIR = os.path.dirname(os.path.abspath(__file__))


@pytest.fixture
def run_eval(tmp_path, monkeypatch):
    monkeypatch.setenv("OUT", str(tmp_path / "out"))
    fake_vllm = types.ModuleType("vllm")
    fake_vllm.LLM = mock.Mock(side_effect=AssertionError("model loaded"))
    fake_vllm.SamplingParams = object
    monkeypatch.setitem(sys.modules, "vllm", fake_vllm)
    spec = importlib.util.spec_from_file_location(
        "run_eval_vllm", os.path.join(EVAL_DIR, "run_eval_vllm.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _write_suites(eval_dir, names, empty=(), drop=None):
    eval_dir.mkdir(parents=True, exist_ok=True)
    for name in names:
        with open(eval_dir / f"{name}.jsonl", "w") as f:
            if name not in empty:
                task = {"id": f"{name}-1", "prompt": "X", "reference": "Y"}
                task.update({"text": "X", "suffix": "", "reference_text": "XY"})
                if drop and drop[0] == name:
                    del task[drop[1]]
                f.write(json.dumps(task) + "\n")


def _cobc_ok(args, **kwargs):
    return subprocess.CompletedProcess(args, 0, "cobc (GnuCOBOL) 3.1.2\n", "")


def test_load_required_suites_returns_every_suite(run_eval, tmp_path):
    _write_suites(tmp_path / "eval", run_eval.REQUIRED_SUITES)
    suites = run_eval.load_required_suites(str(tmp_path / "eval"))
    assert sorted(suites) == sorted(run_eval.REQUIRED_SUITES)
    assert "copybook_usage" in run_eval.REQUIRED_SUITES


def test_missing_and_empty_suites_fail_together(run_eval, tmp_path):
    present = [n for n in run_eval.REQUIRED_SUITES if n != "program_completion"]
    _write_suites(tmp_path / "eval", present, empty=("perplexity",))
    with pytest.raises(RuntimeError) as err:
        run_eval.load_required_suites(str(tmp_path / "eval"))
    assert "program_completion.jsonl is missing" in str(err.value)
    assert "perplexity.jsonl has no tasks" in str(err.value)


def test_paragraph_tasks_without_suffix_fail(run_eval, tmp_path):
    _write_suites(
        tmp_path / "eval",
        run_eval.REQUIRED_SUITES,
        drop=("paragraph_completion", "suffix"),
    )
    with pytest.raises(
        RuntimeError, match="paragraph_completion.jsonl tasks lack suffix"
    ):
        run_eval.load_required_suites(str(tmp_path / "eval"))


def test_check_compiler_reports_version(run_eval):
    with mock.patch.object(run_eval.subprocess, "run", side_effect=_cobc_ok):
        assert run_eval.check_compiler() == "cobc (GnuCOBOL) 3.1.2"


@pytest.mark.parametrize(
    "side_effect",
    [
        FileNotFoundError("cobc"),
        subprocess.TimeoutExpired("cobc", 10),
        lambda args, **kw: subprocess.CompletedProcess(args, 1, "", "boom"),
    ],
)
def test_check_compiler_fails_closed(run_eval, side_effect):
    with mock.patch.object(run_eval.subprocess, "run", side_effect=side_effect):
        with pytest.raises(RuntimeError):
            run_eval.check_compiler()


def test_main_fails_before_loading_the_model(run_eval, tmp_path, monkeypatch):
    monkeypatch.setattr(run_eval, "EVAL_DIR", str(tmp_path / "missing"))
    with mock.patch.object(run_eval.subprocess, "run", side_effect=_cobc_ok):
        with pytest.raises(RuntimeError, match="run 03-benchmarks first"):
            run_eval.main()
    run_eval.LLM.assert_not_called()


def test_compilation_fails_when_cobc_disappears(run_eval):
    with mock.patch.object(
        run_eval.subprocess, "run", side_effect=FileNotFoundError("cobc")
    ):
        with pytest.raises(RuntimeError, match="cobc not found while compiling"):
            run_eval.eval_compilation([{"id": "t1", "prompt": "X"}], ["Y"], ["XY"])


def test_compile_tolerates_non_utf8_compiler_output(run_eval, tmp_path):
    fake_cobc = tmp_path / "cobc"
    fake_cobc.write_text("#!/bin/sh\nprintf 'bad \\200 byte' >&2\nexit 1\n")
    fake_cobc.chmod(0o755)
    with mock.patch.dict(os.environ, {"PATH": str(tmp_path)}):
        success, errors, _ = run_eval.try_compile("X")
    assert success is False
    assert "bad" in errors


def test_cut_at_next_paragraph_keeps_only_the_target_body(run_eval):
    body = "           MOVE 1 TO WS-A.\n           DISPLAY WS-A.\n"
    later = "       NEXT-PARA.\n           STOP RUN.\n"
    assert run_eval.cut_at_next_paragraph(body + later) == body
    assert run_eval.cut_at_next_paragraph(body) == body
    assert run_eval.cut_at_next_paragraph("       0100-START.\n" + body) == ""


def test_eval_compilation_appends_suffix_and_counts_reference_and_empty(run_eval):
    compiled_texts = []

    def fake_compile(text):
        compiled_texts.append(text)
        return text.endswith("SUFFIX") or text == "REF-OK", "", "fixed"

    tasks = [
        {"id": "a", "prompt": "P1-", "suffix": "SUFFIX"},
        {"id": "b", "prompt": "P2-"},
    ]
    with mock.patch.object(run_eval, "try_compile", side_effect=fake_compile):
        result = run_eval.eval_compilation(tasks, ["G1-", "G2"], ["REF-OK", "REF-BAD"])
    assert compiled_texts == [
        "P1-G1-SUFFIX",
        "REF-OK",
        "P1-SUFFIX",
        "P2-G2",
        "REF-BAD",
        "P2-",
    ]
    assert result["compiled"] == 1
    assert result["reference_compiled"] == 1
    assert result["compiled_where_reference_compiles"] == 1
    assert result["empty_compiled"] == 1
    assert [d["reference_compiled"] for d in result["details"]] == [True, False]
    assert [d["empty_compiled"] for d in result["details"]] == [True, False]


def _load_benchmarks(tmp_path, monkeypatch):
    monkeypatch.setenv("OUT", str(tmp_path / "bench"))
    path = os.path.join(EVAL_DIR, "..", "03-benchmarks", "build_benchmarks.py")
    spec = importlib.util.spec_from_file_location("build_benchmarks", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


CALLS_LATER_PARAGRAPH = (
    "       IDENTIFICATION DIVISION.\n"
    "       PROGRAM-ID. CALLER.\n"
    "       DATA DIVISION.\n"
    "       WORKING-STORAGE SECTION.\n"
    "       01 WS-COUNT PIC 9(4) VALUE 0.\n"
    "       PROCEDURE DIVISION.\n"
    "       MAIN-PARA.\n"
    "           MOVE 1 TO WS-COUNT\n"
    "           PERFORM ADD-PARA\n"
    "           DISPLAY WS-COUNT\n"
    "           STOP RUN.\n"
    "       ADD-PARA.\n"
    "           ADD 2 TO WS-COUNT.\n"
)


def test_paragraph_regex_matches_stage_03(run_eval, tmp_path, monkeypatch):
    bench = _load_benchmarks(tmp_path, monkeypatch)
    assert run_eval.PARA_RE.pattern == bench.PARA_RE.pattern
    assert run_eval.PARA_RE.flags == bench.PARA_RE.flags


@pytest.mark.skipif(shutil.which("cobc") is None, reason="needs GnuCOBOL")
def test_reference_completion_compiles_with_later_paragraphs(
    run_eval, tmp_path, monkeypatch
):
    bench = _load_benchmarks(tmp_path, monkeypatch)
    tasks = bench.build_paragraph_completion(
        [{"text": CALLS_LATER_PARAGRAPH, "is_complete_program": True}]
    )
    assert [t["paragraph_name"] for t in tasks] == ["MAIN-PARA"]
    task = tasks[0]
    assert "ADD-PARA." in task["suffix"]
    assert not run_eval.try_compile(task["prompt"] + task["reference"])[0]
    result = run_eval.eval_compilation(
        [task],
        [task["reference"]],
        [task["prompt"] + task["reference"] + task["suffix"]],
    )
    assert result["compiled"] == 1
    assert result["reference_compiled"] == 1


def _fake_tokenizer(text):
    return {"input_ids": text.split()}


def _suites_with_prompts(run_eval, lengths):
    return {
        name: [
            {"id": f"{name}-{n}", "prompt": " ".join(["w"] * n)}
            for n in lengths.get(name, [10])
        ]
        for name in run_eval.REQUIRED_SUITES
    }


def test_fit_to_context_sets_aside_prompts_over_each_budget(run_eval):
    budgets = run_eval.GENERATION_BUDGETS
    max_len = 2048
    para_limit = max_len - budgets["paragraph_completion"]
    prog_limit = max_len - budgets["program_completion"]
    suites = _suites_with_prompts(
        run_eval,
        {
            "paragraph_completion": [para_limit, para_limit + 1],
            "program_completion": [prog_limit + 1, 10],
        },
    )
    excluded = run_eval.fit_to_context(suites, _fake_tokenizer, max_len)
    assert [t["id"] for t in suites["paragraph_completion"]] == [
        f"paragraph_completion-{para_limit}"
    ]
    assert [t["id"] for t in suites["program_completion"]] == ["program_completion-10"]
    assert excluded == {
        "paragraph_completion": {
            "prompt_token_limit": para_limit,
            "tasks": [
                {
                    "id": f"paragraph_completion-{para_limit + 1}",
                    "prompt_tokens": para_limit + 1,
                }
            ],
        },
        "program_completion": {
            "prompt_token_limit": prog_limit,
            "tasks": [
                {
                    "id": f"program_completion-{prog_limit + 1}",
                    "prompt_tokens": prog_limit + 1,
                }
            ],
        },
    }
    assert len(suites["perplexity"]) == 1


def test_fit_to_context_fails_when_a_suite_is_emptied(run_eval):
    suites = _suites_with_prompts(run_eval, {"copybook_usage": [5000]})
    with pytest.raises(RuntimeError, match="copybook_usage"):
        run_eval.fit_to_context(suites, _fake_tokenizer, 2048)
