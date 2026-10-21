# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for continued_pretrain's launch contract.

transformers and datasets are replaced with fakes, so this needs CPU torch only:
  python -m pytest examples/cobol-cpt-grpo/04-cpt/test_continued_pretrain.py -v

Checks that importing the module has no side effects, that TrainingArguments are the
same with MAX_STEPS set or unset, that MAX_STEPS only adds a stop callback, and that
saves use 5GB shards written without a temp-file rename.
"""

import dataclasses
import importlib.util
import json
import os
import subprocess
import sys
import types

import pytest
import torch

CPT_DIR = os.path.dirname(os.path.abspath(__file__))

EXPECTED_ARGS = {
    "num_train_epochs": 3,
    "per_device_train_batch_size": 4,
    "per_device_eval_batch_size": 4,
    "gradient_accumulation_steps": 4,
    "learning_rate": 2e-5,
    "lr_scheduler_type": "cosine",
    "warmup_steps": 0.05,
    "weight_decay": 0.1,
    "bf16": True,
    "logging_steps": 10,
    "eval_strategy": "steps",
    "eval_steps": 25,
    "save_strategy": "steps",
    "save_steps": 25,
    "save_total_limit": 5,
    "load_best_model_at_end": False,
    "dataloader_num_workers": 4,
    "dataloader_pin_memory": True,
    "gradient_checkpointing": True,
    "gradient_checkpointing_kwargs": {"use_reentrant": False},
    "report_to": "none",
    "seed": 42,
    "remove_unused_columns": False,
}

CPT_ENV = {
    "EPOCHS": "3",
    "LR": "2e-5",
    "BATCH_SIZE": "4",
    "GRAD_ACCUM": "4",
    "MAX_SEQ_LEN": "8",
    "SAVE_STEPS": "25",
    "WARMUP_RATIO": "0.05",
    "WEIGHT_DECAY": "0.1",
}


class _StopBeforeTrainer(Exception):
    pass


class _FakeTokenizer:
    pad_token = None
    eos_token = "<eos>"
    eos_token_id = 0

    def __call__(self, text, **kwargs):
        return {"input_ids": list(range(1, len(text) + 1))}


class _FakeModel:
    def __init__(self):
        self.saves = []

    def parameters(self):
        return [torch.nn.Parameter(torch.zeros(2))]

    def save_pretrained(self, output_dir, **kwargs):
        self.saves.append((output_dir, kwargs))


def _fake_hf_modules(captured):
    transformers = types.ModuleType("transformers")
    transformers.AutoTokenizer = types.SimpleNamespace(
        from_pretrained=lambda *a, **k: _FakeTokenizer()
    )
    transformers.AutoModelForCausalLM = types.SimpleNamespace(
        from_pretrained=lambda *a, **k: _FakeModel()
    )
    transformers.TrainerCallback = type("TrainerCallback", (), {})

    def training_arguments(**kwargs):
        captured["args"] = kwargs
        return types.SimpleNamespace(**kwargs)

    def trainer(**kwargs):
        captured["callbacks"] = kwargs["callbacks"]
        captured["model"] = kwargs["model"]
        raise _StopBeforeTrainer

    transformers.TrainingArguments = training_arguments
    transformers.modeling_utils = types.SimpleNamespace(safe_save_file=None)
    transformers.Trainer = trainer

    datasets = types.ModuleType("datasets")
    datasets.Dataset = types.SimpleNamespace(from_list=lambda rows: rows)
    return {"transformers": transformers, "datasets": datasets}


@pytest.fixture
def cpt(tmp_path, monkeypatch):
    """Imports continued_pretrain with fake HF modules and fails on any subprocess."""
    captured = {}
    for name, module in _fake_hf_modules(captured).items():
        monkeypatch.setitem(sys.modules, name, module)

    def no_subprocess(*args, **kwargs):
        raise AssertionError(f"unexpected subprocess: {args}")

    for fn in ("run", "call", "check_call", "check_output", "Popen"):
        monkeypatch.setattr(subprocess, fn, no_subprocess)

    output_dir = tmp_path / "out"
    monkeypatch.setenv("OUTPUT_DIR", str(output_dir))
    monkeypatch.setenv("HF_HOME", str(tmp_path / "hf"))
    monkeypatch.delenv("MAX_STEPS", raising=False)
    monkeypatch.delenv("TAU_RESUME_FROM", raising=False)
    for key in ("RANK", "LOCAL_RANK", "WORLD_SIZE"):
        monkeypatch.delenv(key, raising=False)

    orig_load = torch.load
    spec = importlib.util.spec_from_file_location(
        "continued_pretrain", os.path.join(CPT_DIR, "continued_pretrain.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    yield types.SimpleNamespace(
        module=module, captured=captured, output_dir=output_dir, orig_load=orig_load
    )
    torch.load = orig_load


def _run_main(cpt, tmp_path, monkeypatch):
    data = tmp_path / "data.jsonl"
    data.write_text("".join(json.dumps({"text": "x" * 30}) + "\n" for _ in range(4)))
    for key, value in CPT_ENV.items():
        monkeypatch.setenv(key, value)
    monkeypatch.setenv("TRAIN_FILE", str(data))
    monkeypatch.setenv("VAL_FILE", str(data))
    with pytest.raises(_StopBeforeTrainer):
        cpt.module.main()
    return cpt.captured


def test_import_has_no_side_effects(cpt):
    assert torch.load is cpt.orig_load
    assert not cpt.output_dir.exists()
    assert not torch.distributed.is_initialized()
    assert not torch.cuda.is_initialized()
    for name in ("LOCAL_RANK", "MODEL_ID", "OUTPUT_DIR", "BATCH_SIZE"):
        assert not hasattr(cpt.module, name)


def _expected_args(cpt):
    output_dir = str(cpt.output_dir)
    return {
        "output_dir": output_dir,
        "deepspeed": os.path.join(output_dir, "ds_config.json"),
        **EXPECTED_ARGS,
    }


def _stop_callbacks(cpt):
    return [
        cb
        for cb in cpt.captured["callbacks"]
        if isinstance(cb, cpt.module.StopAtStepCallback)
    ]


def test_max_steps_unset_keeps_training_arguments(cpt, tmp_path, monkeypatch):
    captured = _run_main(cpt, tmp_path, monkeypatch)
    assert captured["args"] == _expected_args(cpt)
    assert _stop_callbacks(cpt) == []
    assert len(captured["callbacks"]) == 1
    assert torch.load is cpt.module._patched_torch_load


def test_max_steps_set_adds_stop_callback_only(cpt, tmp_path, monkeypatch):
    monkeypatch.setenv("MAX_STEPS", "50")
    captured = _run_main(cpt, tmp_path, monkeypatch)
    assert captured["args"] == _expected_args(cpt)
    stops = _stop_callbacks(cpt)
    assert len(stops) == 1
    assert stops[0].max_steps == 50


def test_stop_callback_stops_at_max_steps(cpt):
    cb = cpt.module.StopAtStepCallback(50)
    for step, should_stop in ((1, False), (49, False), (50, True), (51, True)):
        control = types.SimpleNamespace(should_training_stop=False)
        state = types.SimpleNamespace(global_step=step)
        cb.on_step_end(None, state, control)
        assert control.should_training_stop is should_stop, step


def test_training_arguments_exist_in_installed_transformers():
    """The fake TrainingArguments accepts any kwarg; check the names against the real one."""
    real = pytest.importorskip("transformers")
    fields = {f.name for f in dataclasses.fields(real.TrainingArguments)}
    assert set(EXPECTED_ARGS) - fields == set()
    assert {"output_dir", "deepspeed"} <= fields


def test_saves_use_5gb_shards(cpt, tmp_path, monkeypatch):
    model = _run_main(cpt, tmp_path, monkeypatch)["model"]
    model.save_pretrained("ckpt")
    model.save_pretrained("final", state_dict={}, max_shard_size="1GB")
    assert model.saves == [
        ("ckpt", {"max_shard_size": "5GB"}),
        ("final", {"state_dict": {}, "max_shard_size": "1GB"}),
    ]


def test_main_replaces_safe_save_file(cpt, tmp_path, monkeypatch):
    _run_main(cpt, tmp_path, monkeypatch)
    modeling_utils = sys.modules["transformers"].modeling_utils
    assert modeling_utils.safe_save_file is cpt.module._save_file_direct


def test_save_file_direct_matches_save_file_without_temp_file(cpt, tmp_path):
    """Needs the real safetensors package; the fixture only fakes transformers and datasets."""
    st = pytest.importorskip("safetensors.torch")
    tensors = {
        "a": torch.arange(6, dtype=torch.bfloat16).reshape(2, 3),
        "b": torch.ones(4),
    }
    metadata = {"format": "pt"}
    st.save_file(tensors, str(tmp_path / "ref.safetensors"), metadata=metadata)

    opened = []
    real_open = open

    def tracking_open(path, *args, **kwargs):
        opened.append(os.path.basename(path))
        return real_open(path, *args, **kwargs)

    renames = []
    out = tmp_path / "direct.safetensors"
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr("builtins.open", tracking_open)
        mp.setattr(os, "rename", lambda *a: renames.append(a))
        mp.setattr(os, "replace", lambda *a: renames.append(a))
        cpt.module._save_file_direct(tensors, str(out), metadata=metadata)

    assert opened == ["direct.safetensors"] and renames == []
    assert out.read_bytes() == (tmp_path / "ref.safetensors").read_bytes()
