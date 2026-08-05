"""Deterministic decorator build artifact coverage."""

from __future__ import annotations

import json
import sys
import textwrap
from pathlib import Path

import pytest
import yaml

from tau import cli as tau_cli
from tau.build import BuildArtifactError, BuildOverrides, load_artifact


def _write_module(path: Path, source: str) -> None:
    path.write_text(textwrap.dedent(source).strip() + "\n")


def _artifact_bytes(root: Path) -> dict[str, bytes]:
    return {
        path.relative_to(root).as_posix(): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def _build(
    module: Path,
    output: Path,
    *,
    overrides: BuildOverrides | None = None,
) -> Path:
    rc = tau_cli._orchestrate_build(
        module,
        output=output,
        force=False,
        overrides=overrides or BuildOverrides(),
    )
    assert rc == 0
    return output


def test_build_is_byte_stable_and_records_final_train_eval_intent(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(
            name="train-model",
            gpus=2,
            workers=3,
            namespace="decorator-ns",
            checkpoint_artifact="best/model.safetensors",
            runtime_pip=["torch==2.4.0"],
            env={"MODE": "train"},
            mounts=[
                tau.pvc_mount(
                    "model-cache",
                    pvc="model-cache",
                    mount_path="/models",
                    read_only=True,
                )
            ],
        )
        def train_job(ctx):
            return ctx

        @tau.eval(
            name="eval-model",
            after="train-model",
            gpus=1,
            cpu_workers=7,
            namespace="decorator-ns",
            extra_manifest={
                "runtime": {
                    "env": [{"name": "MODE", "value": "eval"}],
                    "pip": ["torch==2.4.0"],
                },
            },
        )
        def eval_job(ctx):
            return ctx

        endpoint = tau.serve(
            name="model-api",
            profile="research-serve",
            from_finetune=train_job,
        )
        """,
    )
    overrides = BuildOverrides(
        namespace="export-ns",
        data_pvc="durable-data",
        queue="priority-queue",
        gpu_class="any",
        worker_cpu_request=12,
        worker_memory_limit="96Gi",
    )

    first = _build(module, tmp_path / "build-a", overrides=overrides)
    second = _build(module, tmp_path / "build-b", overrides=overrides)

    assert _artifact_bytes(first) == _artifact_bytes(second)
    combined = b"\n".join(_artifact_bytes(first).values())
    assert str(tmp_path).encode() not in combined

    root, index = load_artifact(first)
    assert root == first.resolve()
    assert index["kind"] == "tau.python.build"
    assert index["schema_version"] == 1
    assert index["generator"]["name"] == "tau-py"
    assert index["separate_serve"] == [
        {
            "attribute": "endpoint",
            "name": "model-api",
            "reason": (
                "tau.serve maps directly to tau serve deploy and is not "
                "a managed workflow"
            ),
        }
    ]
    assert [(item["kind"], item["name"]) for item in index["workloads"]] == [
        ("train", "train-model"),
        ("eval", "eval-model"),
    ]
    train_record, eval_record = index["workloads"]
    expected_checkpoint = (
        "/data/checkpoints/finetunes/train-model/artifacts/"
        "best/model.safetensors"
    )
    assert eval_record["after"] == "train-model"
    assert eval_record["upstream_checkpoint"] == expected_checkpoint

    train_config = yaml.safe_load((first / train_record["config"]).read_text())
    eval_config = yaml.safe_load((first / eval_record["config"]).read_text())
    for config in (train_config, eval_config):
        assert config["policy"]["namespace"] == "export-ns"
        assert config["storage"]["data_pvc"] == "durable-data"
        assert config["policy"]["gpu_class"] == "any"
        assert config["compute"]["worker_cpus"] == 12
        assert config["compute"]["worker_memory_limit"] == "96Gi"
        assert not Path(config["run"]["entrypoint"]).is_absolute()
        for spec in config["workflow"]["extra_scripts"]:
            assert not Path(spec.split(":", 1)[0]).is_absolute()
    assert train_config["policy"]["queue"] == "priority-queue"
    assert train_config["storage"]["mounts"] == [
        {
            "mountPath": "/models",
            "name": "model-cache",
            "pvc": "model-cache",
            "readOnly": True,
        }
    ]
    assert "queue" not in eval_config["policy"]
    assert eval_config["workflow"]["upstream_checkpoint"] == expected_checkpoint

    golden_dir = (
        Path(__file__).parents[3]
        / "cli"
        / "internal"
        / "cli"
        / "testdata"
        / "python-build"
    )
    assert (first / train_record["config"]).read_bytes() == (golden_dir / "train.yaml").read_bytes()
    assert (first / eval_record["config"]).read_bytes() == (golden_dir / "eval.yaml").read_bytes()

    stderr = capsys.readouterr().err
    assert "tau.serve handles remain separate" in stderr


def test_build_preserves_decorator_namespace_without_cli_override(tmp_path: Path) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(
            name="namespace-test",
            namespace="team-ns",
            runtime_pip=["torch==2.4.0"],
        )
        def train_job(ctx):
            return ctx
        """,
    )

    output = _build(module, tmp_path / "generated")
    _, index = load_artifact(output)
    record = index["workloads"][0]
    config = yaml.safe_load((output / record["config"]).read_text())

    assert record["namespace"] == "team-ns"
    assert config["policy"]["namespace"] == "team-ns"


@pytest.mark.parametrize("filename", ["tau_py_wrapper.py", "tau.yaml"])
def test_build_rejects_extra_scripts_that_collide_with_generated_files(
    tmp_path: Path,
    filename: str,
) -> None:
    (tmp_path / filename).write_text("print('extra script')\n")
    (tmp_path / "entry.py").write_text("def main():\n    return None\n")
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        f"""
        from pathlib import Path

        import tau

        train_job = tau.train(
            name="collision-test",
            gpus=1,
            entrypoint=Path(__file__).with_name("entry.py"),
            extra_scripts=[Path(__file__).with_name({filename!r})],
            runtime_pip=["torch==2.4.0"],
        )
        """,
    )
    output = tmp_path / "generated"

    with pytest.raises(BuildArtifactError, match="collides with a generated build file"):
        _build(module, output)

    assert not output.exists()


def test_build_cli_exports_source_secret_locators_without_values_and_replays(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.chdir(tmp_path)
    (tmp_path / "token.txt").write_text("file-secret\n")
    monkeypatch.setenv("TAU_TEST_TOKEN", "environment-secret")
    module = tmp_path / "secret_workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(
            name="secret-train",
            gpus=1,
            runtime_pip=["torch==2.4.0"],
            env={
                "FROM_ENV": tau.secret_from_env(
                    "env-token",
                    env="TAU_TEST_TOKEN",
                ),
                "FROM_FILE": tau.secret_from_file(
                    "file-token",
                    path="token.txt",
                ),
                "EXTERNAL": tau.secret_ref("shared-secret", "api-key"),
            },
        )
        def train_job(ctx):
            return ctx
        """,
    )
    output = tmp_path / "generated"

    rc = tau_cli.main(
        [
            "build",
            str(module),
            "--output",
            str(output),
            "--namespace",
            "replay-ns",
        ]
    )
    assert rc == 0

    artifact_content = b"\n".join(_artifact_bytes(output).values())
    assert b"environment-secret" not in artifact_content
    assert b"file-secret" not in artifact_content
    _, index = load_artifact(output)
    record = index["workloads"][0]
    assert record["secret_sources"] == [
        {
            "env_name": "FROM_ENV",
            "key": "env-token",
            "source": {"kind": "env", "name": "TAU_TEST_TOKEN"},
        },
        {
            "env_name": "FROM_FILE",
            "key": "file-token",
            "source": {"kind": "file", "path": "token.txt"},
        },
    ]
    exported = yaml.safe_load((output / record["config"]).read_text())
    refs = {entry["name"]: entry["valueFrom"]["secretKeyRef"] for entry in exported["runtime"]["env"]}
    assert refs["EXTERNAL"] == {"name": "shared-secret", "key": "api-key"}
    assert refs["FROM_ENV"] == {
        "name": record["generated_secret"],
        "key": "env-token",
    }

    recorder = tmp_path / "recorded.json"
    fake_tau = tmp_path / "fake-tau"
    fake_tau.write_text(
        f"#!{sys.executable}\n"
        "import json, os, pathlib, sys, yaml\n"
        "config = yaml.safe_load(pathlib.Path(sys.argv[sys.argv.index('--config') + 1]).read_text())\n"
        "payload = json.loads(pathlib.Path(config['workflow']['secret_payload']).read_text())\n"
        f"pathlib.Path({str(recorder)!r}).write_text(json.dumps({{'argv': sys.argv, 'cwd': os.getcwd(), 'config': config, 'payload': payload}}, sort_keys=True))\n"
    )
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    rc = tau_cli.main(
        [
            "submit-build",
            str(output),
            "--dry-run",
            "client",
            "--context",
            "test-context",
        ]
    )
    assert rc == 0

    replay = json.loads(recorder.read_text())
    assert replay["cwd"] == str(output.resolve())
    assert replay["payload"] == {
        "name": record["generated_secret"],
        "stringData": {
            "env-token": "environment-secret",
            "file-token": "file-secret",
        },
    }
    assert replay["argv"][-4:] == [
        "--dry-run",
        "client",
        "--context",
        "test-context",
    ]
    assert Path(replay["config"]["run"]["entrypoint"]).is_absolute()


def test_build_replay_fails_if_source_secret_is_unavailable(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(
            name="missing-secret",
            runtime_pip=["torch==2.4.0"],
            env={"TOKEN": tau.secret_from_env("token", env="NOT_SET")},
        )
        def train_job(ctx):
            return ctx
        """,
    )
    output = _build(module, tmp_path / "generated")
    monkeypatch.delenv("NOT_SET", raising=False)
    monkeypatch.setattr(tau_cli, "_find_tau_binary", lambda: "tau")

    with pytest.raises(ValueError, match="secret env source 'NOT_SET'.*is not set"):
        tau_cli._orchestrate_submit_build(
            output,
            kube_context=None,
            dry_run="client",
            timeout="1m",
            poll_interval=0.01,
            keep_train_rayjob=False,
            cleanup_timeout="1m",
        )


def test_build_verification_rejects_tampered_staged_file(tmp_path: Path) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(name="tamper-test", runtime_pip=["torch==2.4.0"])
        def train_job(ctx):
            return ctx
        """,
    )
    output = _build(module, tmp_path / "generated")
    _, index = load_artifact(output)
    staged_path = output / index["workloads"][0]["files"][0]["path"]
    staged_path.write_text(staged_path.read_text() + "# modified\n")

    with pytest.raises(BuildArtifactError, match="(size|digest) mismatch"):
        load_artifact(output)


def test_build_verification_rejects_path_escape(tmp_path: Path) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(name="path-test", runtime_pip=["torch==2.4.0"])
        def train_job(ctx):
            return ctx
        """,
    )
    output = _build(module, tmp_path / "generated")
    marker = output / "tau-build.yaml"
    index = yaml.safe_load(marker.read_text())
    index["workloads"][0]["config"] = "../outside.yaml"
    marker.write_text(yaml.safe_dump(index, sort_keys=False))

    with pytest.raises(BuildArtifactError, match="invalid workload config path"):
        load_artifact(output)


def test_eval_only_build_requires_explicit_checkpoint(tmp_path: Path) -> None:
    module = tmp_path / "eval_only.py"
    _write_module(
        module,
        """
        import tau

        @tau.eval(
            name="standalone-eval",
            gpus=1,
            cpu_workers=3,
            extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
        )
        def eval_job(ctx):
            return ctx
        """,
    )

    with pytest.raises(BuildArtifactError, match="require --upstream-checkpoint"):
        _build(module, tmp_path / "missing-checkpoint")

    output = _build(
        module,
        tmp_path / "generated",
        overrides=BuildOverrides(upstream_checkpoint="/data/models/model.safetensors"),
    )
    _, index = load_artifact(output)
    record = index["workloads"][0]
    assert "after" not in record
    assert record["upstream_checkpoint"] == "/data/models/model.safetensors"


def test_build_rejects_mismatched_eval_lineage(tmp_path: Path) -> None:
    module = tmp_path / "mismatch.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(name="train-a", runtime_pip=["torch==2.4.0"])
        def train_job(ctx):
            return ctx

        @tau.eval(
            name="eval-a",
            after="train-b",
            extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
        )
        def eval_job(ctx):
            return ctx
        """,
    )

    with pytest.raises(BuildArtifactError, match="does not match"):
        _build(module, tmp_path / "generated")


def test_serve_only_build_is_explicitly_unsupported(tmp_path: Path) -> None:
    module = tmp_path / "serve_only.py"
    _write_module(
        module,
        """
        import tau

        endpoint = tau.serve(name="model-api", profile="research-serve")
        """,
    )

    with pytest.raises(BuildArtifactError, match="ServeHandle.deploy"):
        _build(module, tmp_path / "generated")


def test_build_requires_force_to_replace_existing_output(tmp_path: Path) -> None:
    module = tmp_path / "workflow.py"
    _write_module(
        module,
        """
        import tau

        @tau.train(name="replace-test", runtime_pip=["torch==2.4.0"])
        def train_job(ctx):
            return ctx
        """,
    )
    output = _build(module, tmp_path / "generated")

    with pytest.raises(BuildArtifactError, match="already exists"):
        _build(module, output)

    rc = tau_cli._orchestrate_build(
        module,
        output=output,
        force=True,
        overrides=BuildOverrides(),
    )
    assert rc == 0
    load_artifact(output)
