# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for the eval preflight in run_eval_vllm.

vllm is replaced with a stub, so this needs CPU torch only:
  python -m pytest examples/cobol-cpt-grpo/07-eval/test_run_eval.py -v

Checks that a missing or empty suite and a missing or broken cobc fail the run
before any model is loaded, that a cobc that disappears mid-run fails instead
of counting tasks as skipped, and that prompts too long for the model context are
set aside and recorded, failing the run if a suite is left empty.
"""

import importlib.util
import json
import os
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


def _write_suites(eval_dir, names, empty=()):
    eval_dir.mkdir(parents=True, exist_ok=True)
    for name in names:
        with open(eval_dir / f"{name}.jsonl", "w") as f:
            if name not in empty:
                f.write(json.dumps({"id": f"{name}-1", "prompt": "X"}) + "\n")


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
            run_eval.eval_compilation([{"id": "t1", "prompt": "X"}], ["Y"])


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
