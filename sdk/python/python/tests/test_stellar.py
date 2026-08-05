from __future__ import annotations

import argparse
import json
import os
import sqlite3
from pathlib import Path

import pytest

import tau
from tau import stellar


def _write_jsonl_argv_recorder(path: Path, recorder: Path) -> None:
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        "import pathlib\n"
        "import sys\n"
        f"with pathlib.Path({str(recorder)!r}).open('a') as f:\n"
        "    f.write(json.dumps(sys.argv[1:]) + '\\n')\n"
    )
    path.chmod(0o755)


def _track_call(calls: list[list[str]]) -> list[str]:
    return next(call for call in calls if call[3:4] == ["track"])


class _FakeTensor:
    def __init__(self, value: float):
        self.value = value

    def detach(self) -> "_FakeTensor":
        return self

    def cpu(self) -> "_FakeTensor":
        return self

    def item(self) -> float:
        return self.value


def test_stellar_log_writes_wandb_shaped_history_and_artifacts(tmp_path):
    image = tmp_path / "overlay.png"
    image.write_bytes(b"png")

    run = stellar.init(
        project="radiology-foundation-models",
        run="captioner-lora-v1",
        group="captioner-lora",
        experiment_id="img-captioner-finetune-validation",
        dir=tmp_path / "runs",
        config={"lr": 2e-5},
    )
    stellar.log({"train/loss": 1.25, "val/radgraph_f1": 0.41}, step=3)
    stellar.log({"examples/grounding": stellar.Image(image, caption="case 1")}, step=3)
    stellar.log_artifact("examples/report_diffs", stellar.Html("<h1>diff</h1>"))
    stellar.finish()

    rows = [json.loads(line) for line in run.history_path.read_text().splitlines()]
    assert len(rows) == 1
    assert rows[0]["_step"] == 3
    assert rows[0]["_timestamp"] == pytest.approx(rows[0]["_timestamp"])
    assert rows[0]["train/loss"] == 1.25
    assert rows[0]["val/radgraph_f1"] == 0.41
    manifest = json.loads(run.manifest_path.read_text())
    assert manifest["project"] == "radiology-foundation-models"
    assert {artifact["key"] for artifact in manifest["artifacts"]} == {"examples/grounding", "examples/report_diffs"}
    grounding = next(artifact for artifact in manifest["artifacts"] if artifact["key"] == "examples/grounding")
    assert grounding["caption"] == "case 1"
    assert grounding["step"] == 3
    assert (run.artifacts_dir / "examples-grounding-step-3.png").read_bytes() == b"png"
    assert (run.artifacts_dir / "examples-report_diffs.html").read_text() == "<h1>diff</h1>"


def test_stellar_preserves_same_media_key_at_multiple_steps(tmp_path):
    image1 = tmp_path / "step1.png"
    image2 = tmp_path / "step2.png"
    image1.write_bytes(b"one")
    image2.write_bytes(b"two")

    run = stellar.init(project="p", run="r", dir=tmp_path / "runs")
    stellar.log({"examples/predictions": stellar.Image(image1, caption="first")}, step=1)
    stellar.log({"examples/predictions": stellar.Image(image2, caption="second")}, step=2)
    stellar.finish()

    manifest = json.loads(run.manifest_path.read_text())
    artifacts = manifest["artifacts"]
    assert [artifact["step"] for artifact in artifacts] == [1, 2]
    assert [artifact["caption"] for artifact in artifacts] == ["first", "second"]
    assert (run.artifacts_dir / "examples-predictions-step-1.png").read_bytes() == b"one"
    assert (run.artifacts_dir / "examples-predictions-step-2.png").read_bytes() == b"two"


def test_stellar_rejects_non_scalar_metric_values(tmp_path):
    stellar.init(project="p", run="r", dir=tmp_path)
    with pytest.raises(TypeError, match="numeric scalars"):
        stellar.log({"train/status": "ok"})
    stellar.finish()


def test_stellar_rejects_non_finite_metrics(tmp_path):
    stellar.init(project="p", run="r", dir=tmp_path)
    with pytest.raises(ValueError, match="finite"):
        stellar.log({"train/loss": float("nan")})
    stellar.finish()


def test_stellar_finish_sync_invokes_tau_contract(tmp_path):
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    image = tmp_path / "retrieval.png"
    image.write_bytes(b"png")
    store = tmp_path / "store"

    run = stellar.init(
        project="radiology-foundation-models",
        run="vit-enc-lora-v1",
        group="vit-enc-lora",
        experiment="Can Stellar track ViT-Enc fine-tuning?",
        experiment_id="img-captioner-finetune-validation",
        dir=tmp_path / "runs",
        store=store,
        tau_binary=str(fake),
    )
    run.log({"retrieval/recall_at_5": 0.67}, step=1)
    run.log_artifact("retrieval/neighbors", stellar.Image(image))
    run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    # Assert the full `init` argv rather than probing for individual flags:
    # taugrid-portal rejects unknown flags outright, so the exact argv is the
    # contract worth pinning.
    assert calls[0] == [
        "experiment", "--store", str(store), "init", "img-captioner-finetune-validation",
        "--project", "radiology-foundation-models",
        "--description", "Can Stellar track ViT-Enc fine-tuning?",
        "--group", "vit-enc-lora",
    ]
    assert calls[1][:5] == ["experiment", "--store", str(store), "track", "vit-enc-lora-v1"]
    artifact_index = calls[1].index("--artifact") + 1
    spec = json.loads(calls[1][artifact_index])
    assert spec["direction"] == "output"
    assert spec["type"] == "retrieval"
    assert spec["name"] == "retrieval-neighbors"
    assert spec["uri"] == "artifacts/vit-enc-lora-v1/retrieval-neighbors.png"
    # Runs are attached to their experiment explicitly, since `track` carries
    # no experiment flag of its own.
    assert calls[2] == [
        "experiment", "--store", str(store), "experiments", "tag-run",
        "vit-enc-lora-v1",
        "--experiment", "img-captioner-finetune-validation",
        "--name", "Can Stellar track ViT-Enc fine-tuning?",
    ]
    assert calls[3][:6] == ["experiment", "--store", str(store), "import", "jsonl", "--run"]
    assert (store / "artifacts" / "vit-enc-lora-v1" / "retrieval-neighbors.png").read_bytes() == b"png"


def test_stellar_sync_groups_runs_under_experiment_not_project(tmp_path):
    """`experiment=` is the grouping axis; runs must not collapse into the project.

    Regression test for the migration off the removed `question` axis: the
    experiment id has to fall back to the experiment *name* before the project
    name, otherwise every run in a sweep lands in one undifferentiated bucket.
    """
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    store = tmp_path / "store"

    for run_name, lr in (("lr-5e-4", 5e-4), ("lr-5e-2", 5e-2)):
        run = stellar.init(
            project="taugrid-cpu-quickstart",
            run=run_name,
            experiment="lr-sweep",
            config={"lr": lr},
            dir=tmp_path / "runs",
            store=store,
            tau_binary=str(fake),
        )
        run.log({"train/loss": 0.7}, step=1)
        run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    inits = [c for c in calls if c[3:4] == ["init"]]
    assert inits, "expected an `experiment init` call"
    # The experiment name wins over the project name.
    assert {c[4] for c in inits} == {"lr-sweep"}

    tags = [c for c in calls if c[3:5] == ["experiments", "tag-run"]]
    assert len(tags) == 2
    assert {c[5] for c in tags} == {"lr-5e-4", "lr-5e-2"}
    for call in tags:
        assert call[call.index("--experiment") + 1] == "lr-sweep"


def test_stellar_sync_tags_experiment_id_run_without_history(tmp_path):
    """`experiment_id` alone must still link the run, even with no metrics.

    The experiment link used to be gated on the optional display name, and
    otherwise only rode along on the `import jsonl` call. A run given just an
    id and no scalar history hit neither path. Against a fresh store the
    skipped `init` also left `track` with no manifest.json, so sync failed
    outright.
    """
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    store = tmp_path / "store"

    run = stellar.init(
        project="radiology",
        run="config-only",
        experiment_id="wanted",
        config={"lr": 2e-5},
        dir=tmp_path / "runs",
        store=store,
        tau_binary=str(fake),
    )
    run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    assert not [c for c in calls if c[3:5] == ["import", "jsonl"]], "expected no history import"
    # The store is still bootstrapped; `--description` is simply omitted.
    assert [c for c in calls if c[3:4] == ["init"]] == [
        [
            "experiment", "--store", str(store), "init", "wanted",
            "--project", "radiology",
            "--group", "default",
        ]
    ]
    # The id doubles as the display name when no name was supplied.
    assert [c for c in calls if c[3:5] == ["experiments", "tag-run"]] == [
        [
            "experiment", "--store", str(store), "experiments", "tag-run",
            "config-only",
            "--experiment", "wanted",
            "--name", "wanted",
        ]
    ]


def test_stellar_sync_uses_json_artifact_spec_for_captioned_media(tmp_path):
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    image = tmp_path / "prediction.png"
    image.write_bytes(b"png")

    run = stellar.init(
        project="p",
        run="captioned",
        dir=tmp_path / "runs",
        store=tmp_path / "store",
        tau_binary=str(fake),
    )
    run.log({"media/prediction-gallery": stellar.Image(image, caption="validation examples")}, step=7)
    run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    track = _track_call(calls)
    artifact_index = track.index("--artifact") + 1
    spec = json.loads(track[artifact_index])
    assert spec == {
        "caption": "validation examples",
        "direction": "output",
        "name": "media-prediction-gallery-step-7",
        "type": "image",
        "uri": "artifacts/captioned/media-prediction-gallery-step-7.png",
    }


def test_stellar_sync_tracks_config_without_artifacts(tmp_path):
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)

    run = stellar.init(
        project="p",
        run="config-only",
        dir=tmp_path / "runs",
        store=tmp_path / "store",
        config={"lr": 1e-4},
        tau_binary=str(fake),
    )
    run.log({"loss": 1.0}, step=1)
    run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    track = _track_call(calls)
    assert track[:5] == ["experiment", "--store", str(tmp_path / "store"), "track", "config-only"]
    assert "--config" in track
    imports = [c for c in calls if c[3:5] == ["import", "jsonl"]]
    assert [c[:6] for c in imports] == [
        ["experiment", "--store", str(tmp_path / "store"), "import", "jsonl", "--run"]
    ]


def test_stellar_sync_tracks_repro_context_for_metrics_only_run(tmp_path):
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)

    run = stellar.init(
        project="p",
        run="metrics-only",
        dir=tmp_path / "runs",
        store=tmp_path / "store",
        tau_binary=str(fake),
    )
    run.log({"loss": 1.0}, step=1)
    run.finish(sync=True)

    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    track = _track_call(calls)
    assert track[:5] == ["experiment", "--store", str(tmp_path / "store"), "track", "metrics-only"]
    assert "--runtime" in track
    assert "--dependencies" in track
    assert "--log-uri" in track
    imports = [c for c in calls if c[3:5] == ["import", "jsonl"]]
    assert [c[:6] for c in imports] == [
        ["experiment", "--store", str(tmp_path / "store"), "import", "jsonl", "--run"]
    ]


def test_stellar_use_artifact_downloads_prior_output_and_records_input_lineage(tmp_path):
    store = tmp_path / "store"
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    artifact_dir = store / "artifacts" / "run-a"
    artifact_dir.mkdir(parents=True)
    source = artifact_dir / "model.ckpt"
    source.write_bytes(b"checkpoint")
    conn = sqlite3.connect(store / "index.sqlite")
    conn.execute(
        """
CREATE TABLE artifacts (
  artifact_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, type TEXT NOT NULL, uri TEXT NOT NULL,
  name TEXT NOT NULL, digest TEXT, size_bytes INTEGER, created_at TEXT NOT NULL, preview TEXT,
  external_ref TEXT, caption TEXT, direction TEXT, alias TEXT, source_artifact_id TEXT,
  source_run_id TEXT, source_dataset_name TEXT, source_dataset_version TEXT, source_dataset_digest TEXT
)
"""
    )
    conn.execute(
        """
INSERT INTO artifacts(artifact_id, run_id, type, uri, name, created_at, direction, alias)
VALUES ('artifact-run-a-best', 'run-a', 'checkpoint', 'artifacts/run-a/model.ckpt', 'checkpoint/best', '2026-06-17T00:00:00Z', 'output', 'best')
"""
    )
    conn.commit()
    conn.close()

    run = stellar.init(project="p", run="run-b", dir=tmp_path / "runs", store=store, tau_binary=str(fake))
    downloaded = run.use_artifact("best")
    run.finish(sync=True)

    assert downloaded.read_bytes() == b"checkpoint"
    manifest = json.loads(run.manifest_path.read_text())
    artifact = manifest["artifacts"][0]
    assert artifact["direction"] == "input"
    assert artifact["source_artifact_id"] == "artifact-run-a-best"
    assert artifact["source_run_id"] == "run-a"
    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    track = _track_call(calls)
    artifact_index = track.index("--artifact") + 1
    spec = json.loads(track[artifact_index])
    assert spec["direction"] == "input"
    assert spec["source_artifact_id"] == "artifact-run-a-best"
    assert spec["source_run_id"] == "run-a"


def test_stellar_sync_sanitizes_run_name_for_artifact_path(tmp_path):
    fake = tmp_path / "tau"
    fake.write_text("#!/usr/bin/env python3\nraise SystemExit(0)\n")
    fake.chmod(0o755)
    source = tmp_path / "artifact.txt"
    source.write_text("payload")
    store = tmp_path / "store"
    escaped = tmp_path / "escaped-run-dir"

    run = stellar.init(
        project="p",
        run=str(escaped),
        dir=tmp_path / "runs",
        store=store,
        tau_binary=str(fake),
    )
    run.log_artifact("artifact", source)
    run.finish(sync=True)

    copied = list((store / "artifacts").rglob("artifact.txt"))
    assert len(copied) == 1
    assert copied[0].read_text() == "payload"
    assert not (escaped / "artifact.txt").exists()


def test_stellar_run_context_manager_finishes(tmp_path):
    with stellar.init(project="p", run="ctx", dir=tmp_path) as run:
        stellar.log({"train/loss": 0.4}, step=1)

    assert run._closed is True
    rows = [json.loads(line) for line in run.history_path.read_text().splitlines()]
    assert rows[0]["train/loss"] == 0.4


def test_stellar_is_public_api():
    assert tau.stellar is stellar


def test_stellar_logger_records_lightning_style_workflow(tmp_path):
    recorder = tmp_path / "argv.jsonl"
    fake = tmp_path / "tau"
    _write_jsonl_argv_recorder(fake, recorder)
    checkpoint = tmp_path / "model.ckpt"
    checkpoint.write_bytes(b"checkpoint")

    logger = stellar.StellarLogger(
        project="radiology",
        name="lightning-run",
        group="lora",
        experiment_id="vit-enc",
        dir=tmp_path / "runs",
        store=tmp_path / "store",
        config={"model": "squeezenet"},
        tau_binary=str(fake),
    )
    logger.log_hyperparams(argparse.Namespace(batch_size=16, lr=2e-5))
    with pytest.warns(RuntimeWarning, match="skipped unsupported or non-finite metric"):
        logger.log_metrics({"train/loss": _FakeTensor(0.5), "val/nan": float("nan")}, step=4)
    logger.experiment.log(
        {"validation/table": stellar.Table([{"image": "case-1", "prediction": "ok"}], caption="validation predictions")},
        step=4,
    )
    logger.after_save_checkpoint(type("Checkpoint", (), {"best_model_path": str(checkpoint), "last_model_path": ""})())
    logger.finalize("success")

    rows = [json.loads(line) for line in logger.experiment.history_path.read_text().splitlines()]
    assert len(rows) == 1
    assert rows[0]["_step"] == 4
    assert rows[0]["_timestamp"] == pytest.approx(rows[0]["_timestamp"])
    assert rows[0]["train/loss"] == 0.5
    config = json.loads(logger.experiment.config_path.read_text())
    assert config["model"] == "squeezenet"
    assert config["batch_size"] == 16
    manifest = json.loads(logger.experiment.manifest_path.read_text())
    assert {artifact["type"] for artifact in manifest["artifacts"]} == {"table", "checkpoint"}
    table = next(artifact for artifact in manifest["artifacts"] if artifact["type"] == "table")
    assert table["caption"] == "validation predictions"
    assert table["step"] == 4
    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    assert _track_call(calls)[:5] == ["experiment", "--store", str(tmp_path / "store"), "track", "lightning-run"]
    imports = [c for c in calls if c[3:5] == ["import", "jsonl"]]
    assert [c[:6] for c in imports] == [
        ["experiment", "--store", str(tmp_path / "store"), "import", "jsonl", "--run"]
    ]


def test_stellar_logger_is_lightning_logger_when_lightning_is_installed(tmp_path):
    lightning_loggers = pytest.importorskip("lightning.pytorch.loggers")
    lightning = pytest.importorskip("lightning.pytorch")
    callbacks = pytest.importorskip("lightning.pytorch.callbacks")
    torch = pytest.importorskip("torch")

    class TinyLitModule(lightning.LightningModule):
        def __init__(self):
            super().__init__()
            self.layer = torch.nn.Linear(1, 1)
            self.save_hyperparameters({"hidden_size": 1, "lr": 0.01})

        def training_step(self, batch, batch_idx):
            x, y = batch
            loss = torch.nn.functional.mse_loss(self.layer(x), y)
            self.log("train/loss", loss)
            return loss

        def validation_step(self, batch, batch_idx):
            x, y = batch
            loss = torch.nn.functional.mse_loss(self.layer(x), y)
            self.log("val/loss", loss)
            return loss

        def configure_optimizers(self):
            return torch.optim.SGD(self.parameters(), lr=0.01)

    x = torch.tensor([[0.0], [1.0], [2.0], [3.0]])
    y = torch.tensor([[0.0], [1.0], [2.0], [3.0]])
    loader = torch.utils.data.DataLoader(torch.utils.data.TensorDataset(x, y), batch_size=2)
    logger = stellar.StellarLogger(project="p", name="accepted-by-trainer", dir=tmp_path)
    assert isinstance(logger, lightning_loggers.Logger)
    checkpoint_callback = callbacks.ModelCheckpoint(
        dirpath=tmp_path / "checkpoints",
        monitor="val/loss",
        mode="min",
        save_last=True,
    )
    trainer = lightning.Trainer(
        logger=logger,
        callbacks=[checkpoint_callback],
        max_epochs=1,
        enable_model_summary=False,
        accelerator="cpu",
        devices=1,
        log_every_n_steps=1,
        limit_train_batches=2,
        limit_val_batches=1,
    )
    trainer.fit(TinyLitModule(), train_dataloaders=loader, val_dataloaders=loader)

    rows = [json.loads(line) for line in logger.experiment.history_path.read_text().splitlines()]
    assert any("train/loss" in row for row in rows)
    assert any("val/loss" in row for row in rows)
    config = json.loads(logger.experiment.config_path.read_text())
    assert config["hidden_size"] == 1
    assert config["lr"] == 0.01
    manifest = json.loads(logger.experiment.manifest_path.read_text())
    assert any(artifact["type"] == "checkpoint" for artifact in manifest["artifacts"])


def test_stellar_group_defaults_to_env_var(tmp_path, monkeypatch):
    """When group is not provided, use TAU_GROUP env var if set."""
    monkeypatch.setenv("TAU_GROUP", "my-experiment-batch")
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.group == "my-experiment-batch"
    stellar.finish()


def test_stellar_group_defaults_to_job_name_prefix(tmp_path, monkeypatch):
    """When group is not provided and no TAU_GROUP, use JOB_NAME prefix."""
    monkeypatch.setenv("JOB_NAME", "vision-training-20250616-abc123")
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.group == "vision"
    stellar.finish()


def test_stellar_group_defaults_to_default_when_no_env(tmp_path, monkeypatch):
    """When group is not provided and no env vars, use 'default'."""
    monkeypatch.delenv("TAU_GROUP", raising=False)
    monkeypatch.delenv("JOB_NAME", raising=False)
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.group == "default"
    stellar.finish()


def test_stellar_explicit_group_overrides_env(tmp_path, monkeypatch):
    """Explicit group parameter overrides env vars."""
    monkeypatch.setenv("TAU_GROUP", "from-env")
    monkeypatch.setenv("JOB_NAME", "from-job-name-abc")
    run = stellar.init(project="p", name="r", group="explicit-group", dir=tmp_path)
    assert run.group == "explicit-group"
    stellar.finish()


def test_stellar_experiment_id_defaults_to_env_var(tmp_path, monkeypatch):
    """When experiment_id is not provided, use TAU_EXPERIMENT_ID env var if set."""
    monkeypatch.setenv("TAU_EXPERIMENT_ID", "img-captioner-validation")
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.experiment_id == "img-captioner-validation"
    stellar.finish()


def test_stellar_experiment_id_defaults_to_tau_experiment(tmp_path, monkeypatch):
    """When experiment_id is not provided, fall back to TAU_EXPERIMENT env var."""
    monkeypatch.delenv("TAU_EXPERIMENT_ID", raising=False)
    monkeypatch.setenv("TAU_EXPERIMENT", "my-experiment")
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.experiment_id == "my-experiment"
    stellar.finish()


def test_stellar_experiment_id_prefers_tau_experiment_id_over_tau_experiment(tmp_path, monkeypatch):
    """TAU_EXPERIMENT_ID takes precedence over TAU_EXPERIMENT."""
    monkeypatch.setenv("TAU_EXPERIMENT_ID", "primary-id")
    monkeypatch.setenv("TAU_EXPERIMENT", "fallback")
    run = stellar.init(project="p", name="r", dir=tmp_path)
    assert run.experiment_id == "primary-id"
    stellar.finish()


def test_stellar_experiment_id_defaults_to_project_when_no_experiment(tmp_path, monkeypatch):
    """With no experiment and no env vars, the experiment id inherits the project."""
    monkeypatch.delenv("TAU_EXPERIMENT_ID", raising=False)
    monkeypatch.delenv("TAU_EXPERIMENT", raising=False)
    run = stellar.init(project="my-project", name="r", dir=tmp_path)
    assert run.experiment_id == "my-project"
    stellar.finish()


def test_stellar_explicit_experiment_id_overrides_env(tmp_path, monkeypatch):
    """Explicit experiment_id parameter overrides env vars."""
    monkeypatch.setenv("TAU_EXPERIMENT_ID", "from-env")
    monkeypatch.setenv("TAU_EXPERIMENT", "from-fallback")
    run = stellar.init(project="p", name="r", experiment_id="explicit-id", dir=tmp_path)
    assert run.experiment_id == "explicit-id"
    stellar.finish()


def test_stellar_combined_env_defaults(tmp_path, monkeypatch):
    """Both group and experiment_id can use env vars simultaneously."""
    monkeypatch.setenv("TAU_GROUP", "training-batch-3")
    monkeypatch.setenv("TAU_EXPERIMENT_ID", "vit-enc-ablation")
    monkeypatch.setenv("JOB_NAME", "ignored-when-tau-group-set")
    run = stellar.init(project="radiology", name="run-001", dir=tmp_path)
    assert run.group == "training-batch-3"
    assert run.experiment_id == "vit-enc-ablation"
    stellar.finish()


def _write_verb_aware_binary(path: Path, recorder: Path, *, supports_experiment: bool) -> None:
    """Write a fake CLI that accepts or rejects `experiment` like a real binary."""
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        "import pathlib\n"
        "import sys\n"
        f"supports = {supports_experiment!r}\n"
        "argv = sys.argv[1:]\n"
        "if argv[:1] == ['experiment'] and not supports:\n"
        "    sys.stderr.write('unknown command \"experiment\"\\n')\n"
        "    sys.exit(1)\n"
        f"with pathlib.Path({str(recorder)!r}).open('a') as f:\n"
        "    f.write(json.dumps(argv) + '\\n')\n"
    )
    path.chmod(0o755)


def test_find_portal_binary_prefers_taugrid_portal(tmp_path, monkeypatch):
    """taugrid-portal on PATH wins outright; no legacy probe is needed."""
    from tau.workloads import _find_portal_binary

    bindir = tmp_path / "bin"
    bindir.mkdir()
    portal = bindir / "taugrid-portal"
    _write_verb_aware_binary(portal, tmp_path / "portal.log", supports_experiment=True)
    legacy = bindir / "tau"
    _write_verb_aware_binary(legacy, tmp_path / "tau.log", supports_experiment=True)

    monkeypatch.delenv("TAUGRID_PORTAL_BINARY", raising=False)
    monkeypatch.delenv("TAU_BINARY", raising=False)
    monkeypatch.setenv("PATH", str(bindir) + os.pathsep + os.environ["PATH"])

    assert _find_portal_binary() == str(portal)


def test_find_portal_binary_honors_env_override(tmp_path, monkeypatch):
    from tau.workloads import _find_portal_binary

    explicit = tmp_path / "custom-portal"
    _write_verb_aware_binary(explicit, tmp_path / "custom.log", supports_experiment=True)
    monkeypatch.setenv("TAUGRID_PORTAL_BINARY", str(explicit))

    assert _find_portal_binary() == str(explicit)


def test_find_portal_binary_rejects_missing_env_override(tmp_path, monkeypatch):
    from tau.workloads import _find_portal_binary

    monkeypatch.setenv("TAUGRID_PORTAL_BINARY", str(tmp_path / "nope"))
    with pytest.raises(RuntimeError, match="does not exist"):
        _find_portal_binary()


def test_find_portal_binary_falls_back_to_pre_split_tau(tmp_path, monkeypatch):
    """A tau from before the split still owns `experiment`, so it is usable."""
    from tau.workloads import _find_portal_binary

    bindir = tmp_path / "bin"
    bindir.mkdir()
    legacy = bindir / "tau"
    _write_verb_aware_binary(legacy, tmp_path / "tau.log", supports_experiment=True)

    monkeypatch.delenv("TAUGRID_PORTAL_BINARY", raising=False)
    monkeypatch.delenv("TAU_BINARY", raising=False)
    monkeypatch.setenv("PATH", str(bindir) + os.pathsep + os.environ["PATH"])

    with pytest.warns(DeprecationWarning):
        assert _find_portal_binary() == str(legacy)


def test_find_portal_binary_rejects_post_split_tau(tmp_path, monkeypatch):
    """A post-split tau has no `experiment` verb, so fail with a clear message."""
    from tau.workloads import _find_portal_binary

    bindir = tmp_path / "bin"
    bindir.mkdir()
    post_split = bindir / "tau"
    _write_verb_aware_binary(post_split, tmp_path / "tau.log", supports_experiment=False)

    monkeypatch.delenv("TAUGRID_PORTAL_BINARY", raising=False)
    monkeypatch.delenv("TAU_BINARY", raising=False)
    monkeypatch.setenv("PATH", str(bindir) + os.pathsep + os.environ["PATH"])

    with pytest.raises(RuntimeError, match="taugrid-portal"):
        _find_portal_binary()


def test_stellar_sync_resolves_portal_binary_lazily(tmp_path, monkeypatch):
    """Constructing a Run must not require any CLI; only sync() resolves one."""
    bindir = tmp_path / "bin"
    bindir.mkdir()
    recorder = tmp_path / "argv.log"
    portal = bindir / "taugrid-portal"
    _write_jsonl_argv_recorder(portal, recorder)

    monkeypatch.delenv("TAUGRID_PORTAL_BINARY", raising=False)
    monkeypatch.delenv("TAU_BINARY", raising=False)
    monkeypatch.setenv("PATH", str(bindir) + os.pathsep + os.environ["PATH"])

    store = tmp_path / "store"
    run = stellar.init(project="p", name="lazy-run", dir=tmp_path / "runs", store=store)
    assert run.tau_binary is None

    run.log({"train/loss": 1.0}, step=1)
    run.finish(sync=True)

    assert run.tau_binary == str(portal)
    calls = [json.loads(line) for line in recorder.read_text().splitlines()]
    assert any(call[:1] == ["experiment"] for call in calls)
