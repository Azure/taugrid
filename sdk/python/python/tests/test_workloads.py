# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""tau-py unit tests. Stdlib + pytest only — no tau binary required."""

from __future__ import annotations

import inspect
import json
import textwrap
from pathlib import Path

import pytest
import yaml

import tau
from tau.serve import serve as serve_workload

_workloads = tau.workloads


def _write_python(path: Path, source: str) -> None:
    path.write_text(textwrap.dedent(source).strip() + "\n")


def _write_argv_recorder(path: Path, recorder: Path) -> None:
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib\n"
        "import sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
    )
    path.chmod(0o755)


def _write_config_recorder(path: Path, recorder: Path, copied_config: Path) -> None:
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_config)!r})\n"
    )
    path.chmod(0o755)


def test_decorator_returns_handle():
    captured = {}

    def train_fn(ctx: tau.Ctx, steps: int = 1) -> tau.Ctx:
        captured["ctx"] = ctx
        captured["steps"] = steps
        return ctx

    f = tau.train(name="t1", gpus=2)(train_fn)

    assert getattr(f, "_tau_train_handle", False) is True
    assert f.__name__ == "train_fn"
    assert callable(f)
    assert hasattr(f, "submit")
    assert inspect.unwrap(f) is train_fn
    signature = inspect.signature(f)
    assert tuple(signature.parameters) == ("steps",)
    signature.bind()
    signature.bind(5)

    result = f(5)
    assert result is captured["ctx"]
    assert captured["steps"] == 5
    assert captured["ctx"].name == "t1"


def test_finetune_module_is_not_exported():
    with pytest.raises(ImportError):
        from tau import finetune  # noqa: F401


def test_serve_accepts_train_handle_and_delegates_to_cli(tmp_path):
    assert tau.serve is serve_workload
    recorder = tmp_path / "tau_argv.txt"
    fake = tmp_path / "tau"
    _write_argv_recorder(fake, recorder)

    @tau.train(name="ft-run")
    def f(ctx):
        return ctx

    handle = serve_workload(
        name="ft-endpoint",
        profile="research-serve",
        from_finetune=f,
        namespace="ray",
        kind="deployment",
        replicas=2,
        args="--model /model",
    )
    assert getattr(handle, "_tau_serve_handle", False) is True
    handle.submit(
        tau_binary=str(fake),
        dry_run="client",
        kube_context="kind-taugrid",
        capture=True,
    )

    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[:4] == [str(fake), "serve", "deploy", "ft-endpoint"]
    assert argv[argv.index("--profile") + 1] == "research-serve"
    assert argv[argv.index("--from-finetune") + 1] == "ft-run"
    assert argv[argv.index("-n") + 1] == "ray"
    assert argv[argv.index("--kind") + 1] == "deployment"
    assert argv[argv.index("--replicas") + 1] == "2"
    assert argv[argv.index("--args") + 1] == "--model /model"
    assert argv[argv.index("--dry-run") + 1] == "client"
    assert argv[argv.index("--context") + 1] == "kind-taugrid"


def test_serve_rejects_multiple_checkpoint_sources():
    serve_workload(name="svc", profile="research-serve")
    with pytest.raises(ValueError, match="at most one"):
        serve_workload(
            name="svc",
            profile="research-serve",
            from_finetune="ft",
            checkpoint="/data/checkpoint",
        )


def test_serve_accepts_model_ref_and_delegates_to_cli(tmp_path):
    recorder = tmp_path / "tau_argv.txt"
    fake = tmp_path / "tau"
    _write_argv_recorder(fake, recorder)

    handle = serve_workload(
        name="sample-endpoint",
        profile="research-serve",
        from_model="sample-lora:best-loss",
    )
    handle.submit(tau_binary=str(fake), capture=True)

    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[:4] == [str(fake), "serve", "deploy", "sample-endpoint"]
    assert argv[argv.index("--from-model") + 1] == "sample-lora:best-loss"


def test_manifest_shape_matches_tau_v1():
    @tau.train(name="my-ft", gpus=2)
    def f(ctx):
        pass

    m = f.manifest()
    assert m["schema_version"] == 1
    assert m["name"] == "my-ft"
    assert m["compute"] == {"gpus": 2}
    assert m["eval"] == {}
    assert m["artifacts"] == {"checkpoint": "last.safetensors"}


def test_train_manifest_declares_custom_checkpoint_artifact():
    @tau.train(name="my-ft", gpus=2, checkpoint_artifact="rank0/final.safetensors")
    def f(ctx):
        pass

    assert f.manifest()["artifacts"] == {"checkpoint": "rank0/final.safetensors"}


def test_train_manifest_declares_model_registry_metadata():
    @tau.train(
        name="my-ft",
        gpus=2,
        model="sample-lora",
        base_model="microsoft/sample",
        task="weather",
        tags={"dataset": "era5"},
        primary_metric="loss",
        metric_direction="lower",
    )
    def f(ctx):
        pass

    assert f.manifest()["model"] == {
        "name": "sample-lora",
        "base": "microsoft/sample",
        "task": "weather",
        "tags": {"dataset": "era5"},
        "primary_metric": "loss",
        "metric_direction": "lower",
    }


def test_train_manifest_rejects_model_metadata_conflict():
    with pytest.raises(ValueError, match="model metadata"):
        @tau.train(
            name="my-ft",
            gpus=2,
            model="sample-lora",
            extra_manifest={"model": {"name": "other-model"}},
        )
        def f(ctx):
            pass

        f.manifest()


def test_train_rejects_unsafe_checkpoint_artifact():
    with pytest.raises(ValueError, match="checkpoint_artifact"):
        @tau.train(name="my-ft", gpus=2, checkpoint_artifact="../final.safetensors")
        def f(ctx):
            pass


def test_train_manifest_allows_cpu_only_gpus_zero():
    @tau.train(name="cpu-only", gpus=0, workers=4)
    def f(ctx):
        pass

    m = f.manifest()
    assert m["compute"] == {"gpus": 0, "workers": 4}


def test_manifest_extras_merge():
    @tau.train(
        name="x",
        gpus=4,
        extra_manifest={"lora": {"target_modules": ["q", "v"]}},
    )
    def f(ctx):
        pass

    m = f.manifest()
    assert m["lora"] == {"target_modules": ["q", "v"]}
    # extras must not clobber the canonical fields:
    assert m["compute"] == {"gpus": 4}


def test_train_config_env_secret_and_mount_contract(tmp_path):
    cfg = tmp_path / "train.yaml"
    cfg.write_text(
        """
schema_version: 1
name: config-run
compute:
  gpus: 2
runtime:
  pip:
    - torch==2.4.0
dataset:
  path: /datasets/base/train.jsonl
"""
    )

    @tau.train(
        config=cfg,
        name="override-run",
        runtime_pip=["torch==2.5.0", "transformers"],
        env={"HF_TOKEN": tau.secret_ref("hf-token", "token"), "WANDB_MODE": "offline"},
        mounts=[tau.pvc_mount("dataset", pvc="captioner-data", mount_path="/datasets/captioner", read_only=True)],
    )
    def f(ctx):
        pass

    m = f.manifest()
    assert m["name"] == "override-run"
    assert m["compute"] == {"gpus": 2}
    assert m["runtime"]["pip"] == ["torch==2.5.0", "transformers"]
    assert {
        "name": "HF_TOKEN",
        "valueFrom": {"secretKeyRef": {"name": "hf-token", "key": "token"}},
    } in m["runtime"]["env"]
    assert {"name": "WANDB_MODE", "value": "offline"} in m["runtime"]["env"]
    assert m["storage"]["mounts"] == [
        {"name": "dataset", "pvc": "captioner-data", "mountPath": "/datasets/captioner", "readOnly": True}
    ]
    assert m["dataset"]["path"] == "/datasets/base/train.jsonl"


def test_train_manifest_declares_primary_data_pvc():
    @tau.train(
        name="pvc-run",
        gpus=1,
        data_pvc="lustre-research",
        runtime_pip=["torch==2.4.0"],
        mounts=[tau.pvc_mount("labels", pvc="labels-pvc", mount_path="/labels", read_only=True)],
    )
    def f(ctx):
        pass

    assert f.manifest()["storage"] == {
        "data_pvc": "lustre-research",
        "mounts": [
            {"name": "labels", "pvc": "labels-pvc", "mountPath": "/labels", "readOnly": True}
        ],
    }


def test_train_manifest_declares_resource_sizing():
    @tau.train(
        name="small-a10",
        gpus=1,
        cpu_request=4,
        memory_request="32Gi",
        worker_cpu_request=2,
        worker_memory_request="16Gi",
        runtime_pip=["torch==2.4.0"],
    )
    def f(ctx):
        pass

    assert f.manifest()["compute"] == {
        "gpus": 1,
        "cpus": 4,
        "memory": "32Gi",
        "worker_cpus": 2,
        "worker_memory": "16Gi",
    }


def test_train_manifest_rejects_conflicting_data_pvc():
    @tau.train(
        name="pvc-conflict",
        gpus=1,
        data_pvc="lustre-research",
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}, "storage": {"data_pvc": "captioner2-data"}},
    )
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="data_pvc=.*conflicts"):
        f.manifest()


def test_submit_data_pvc_override_rewrites_manifest(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_manifest = tmp_path / "manifest.yaml"
    fake = tmp_path / "tau"
    fake.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_manifest)!r})\n"
    )
    fake.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake))

    @tau.train(
        name="submit-pvc",
        gpus=1,
        data_pvc="captioner2-data",
        runtime_pip=["torch==2.4.0"],
    )
    def f(ctx):
        pass

    res = f.submit(data_pvc="lustre-research", dry_run="client", namespace="ray", capture=True)
    assert res.returncode == 0
    manifest = yaml.safe_load(copied_manifest.read_text())
    assert manifest["storage"]["data_pvc"] == "lustre-research"


def test_submit_resource_override_rewrites_manifest(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_manifest = tmp_path / "manifest.yaml"
    fake = tmp_path / "tau"
    fake.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_manifest)!r})\n"
    )
    fake.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake))

    @tau.train(
        name="submit-resources",
        gpus=1,
        cpu_request=8,
        memory_request="64Gi",
        runtime_pip=["torch==2.4.0"],
    )
    def f(ctx):
        pass

    res = f.submit(
        cpu_request=4,
        memory_request="32Gi",
        cpu_limit=4,
        memory_limit="32Gi",
        dry_run="client",
        namespace="ray",
        capture=True,
    )
    assert res.returncode == 0
    manifest = yaml.safe_load(copied_manifest.read_text())
    assert manifest["compute"]["cpus"] == 4
    assert manifest["compute"]["memory"] == "32Gi"
    assert manifest["compute"]["cpu_limit"] == 4
    assert manifest["compute"]["memory_limit"] == "32Gi"


def test_secret_from_env_rewrites_to_job_scoped_secret(tmp_path, monkeypatch):
    recorder = tmp_path / "argv.txt"
    copied_manifest = tmp_path / "manifest.yaml"
    copied_secret = tmp_path / "secret.json"
    fake = tmp_path / "tau"
    fake.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys, yaml\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_manifest)!r})\n"
        "cfg = yaml.safe_load(open(sys.argv[sys.argv.index('--config') + 1]))\n"
        "secret = ((cfg.get('workflow') or {}).get('secret_payload'))\n"
        "if secret:\n"
        f"    shutil.copyfile(secret, {str(copied_secret)!r})\n"
    )
    fake.chmod(0o755)
    monkeypatch.setenv("HF_TOKEN", "fake-token-value")

    @tau.train(
        name="secret-job",
        gpus=1,
        runtime_pip=["torch==2.4.0"],
        env={
            "HF_TOKEN": tau.secret_from_env("HF_TOKEN"),
            "HUGGING_FACE_HUB_TOKEN": tau.secret_from_env("HF_TOKEN"),
        },
    )
    def f(ctx):
        pass

    res = f.submit(tau_binary=str(fake), dry_run="client", namespace="e2e-stack", capture=True)
    assert res.returncode == 0
    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[1:3] == ["run", "--config"]
    payload = json.loads(copied_secret.read_text())
    assert payload == {
        "name": "tau-secret-job-secrets",
        "stringData": {"HF_TOKEN": "fake-token-value"},
    }
    manifest_text = copied_manifest.read_text()
    manifest = yaml.safe_load(manifest_text)
    refs = {
        item["name"]: item["valueFrom"]["secretKeyRef"]
        for item in manifest["runtime"]["env"]
    }
    assert refs["HF_TOKEN"] == {"name": "tau-secret-job-secrets", "key": "HF_TOKEN"}
    assert refs["HUGGING_FACE_HUB_TOKEN"] == {
        "name": "tau-secret-job-secrets",
        "key": "HF_TOKEN",
    }
    assert "fake-token-value" not in manifest_text


def test_manifest_secret_from_file_resolves_payload_and_refs(tmp_path):
    token_file = tmp_path / "hf-token.txt"
    token_file.write_text("file-token-value\n")
    manifest = {
        "schema_version": 1,
        "name": "file-secret-job",
        "compute": {"gpus": 1},
        "runtime": {
            "env": [
                {
                    "name": "HF_TOKEN",
                    "valueFrom": {
                        "tauSecretSource": {
                            "key": "hf-token",
                            "path": str(token_file),
                        }
                    },
                }
            ]
        },
    }

    rewritten, payload = _workloads._resolve_secret_payload(manifest, None)

    assert payload == {
        "name": "tau-file-secret-job-secrets",
        "stringData": {"hf-token": "file-token-value"},
    }
    assert rewritten["runtime"]["env"] == [
        {
            "name": "HF_TOKEN",
            "valueFrom": {
                "secretKeyRef": {
                    "name": "tau-file-secret-job-secrets",
                    "key": "hf-token",
                }
            },
        }
    ]


def test_secret_from_env_missing_fails_before_submit(tmp_path):
    fake = tmp_path / "tau"
    fake.write_text("#!/usr/bin/env python3\nraise SystemExit(99)\n")
    fake.chmod(0o755)

    @tau.train(
        name="secret-job",
        gpus=1,
        runtime_pip=["torch==2.4.0"],
        env={"HF_TOKEN": tau.secret_from_env("HF_TOKEN", env="MISSING_HF_TOKEN")},
    )
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="MISSING_HF_TOKEN"):
        f.submit(tau_binary=str(fake), dry_run="client", capture=True)


def test_local_call_passes_ctx():
    captured = {}

    @tau.train(name="local", gpus=1, smoke_pairs=3)
    def f(ctx):
        captured["ctx"] = ctx

    f()
    ctx = captured["ctx"]
    assert ctx.name == "local"
    assert ctx.gpus == 1
    assert ctx.smoke_pairs == 3
    assert ctx.is_remote is False
    assert ctx.manifest["compute"]["gpus"] == 1


def test_source_path_resolves_to_file_defining_decorator():
    @tau.train(name="src", gpus=1)
    def f(ctx):
        pass

    assert f.source_path.name == "test_workloads.py"
    assert f.source_path.is_file()


def test_submit_invokes_tau_cli_with_correct_args(tmp_path, monkeypatch):
    """Replace tau binary with a fake recorder; assert the subprocess argv."""
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "tau.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(
        name="submit-test",
        gpus=2,
        smoke_pairs=4,
        team="research",
        preset="azure.research.training.2x", extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}}
    )
    def f(ctx):
        pass

    res = f.submit(dry_run="client", kube_context="kind-taugrid", namespace="ray")
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[0].endswith("tau")
    assert argv[1:3] == ["run", "--config"]
    manifest_path = argv[argv.index("--config") + 1]
    assert manifest_path.endswith(".yaml")
    assert argv[argv.index("--dry-run") + 1] == "client"
    assert argv[argv.index("--context") + 1] == "kind-taugrid"
    config = yaml.safe_load(copied_config.read_text())
    assert config["run"]["workload_kind"] == "rayjob"
    assert config["run"]["entrypoint"].endswith("tau_py_wrapper.py")
    assert config["run"]["smoke_pairs"] == 4
    assert config["workflow"]["extra_scripts"][0].endswith(":tau_user_module.py")
    assert config["policy"]["namespace"] == "ray"
    assert config["policy"]["team"] == "research"
    assert config["policy"]["preset"] == "azure.research.training.2x"


def test_submit_forwards_profiler_flags(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "tau.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(
        name="profile-submit",
        gpus=8,
        workers=2,
        extra_manifest={
            "runtime": {"pip": ["torch==2.4.0"]},
            "storage": {"data_pvc": "taugrid-datasets"},
        },
    )
    def f(ctx):
        pass

    res = f.submit(
        dry_run="client",
        namespace="ray",
        queue="dev",
        gpu_class="any",
        profiler="nsys",
        profile_rank="0,8",
        profile_warmup="30s",
        profile_duration="2m",
    )
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307
    assert argv[1:3] == ["run", "--config"]
    config = yaml.safe_load(copied_config.read_text())
    assert config["profiler"] == {"mode": "nsys", "rank": "0,8", "warmup": "30s", "duration": "2m"}
    assert config["policy"]["queue"] == "dev"
    assert config["policy"]["gpu_class"] == "any"


def test_train_entrypoint_returns_handle_and_runs_local_script(tmp_path):
    script = tmp_path / "probe.py"
    _write_python(
        script,
        """
        def run(left, right, *, sep=":"):
            return f"{left}{sep}{right}"
        """,
    )

    handle = tau.train(
        name="entrypoint-local",
        gpus=0,
        runtime_pip=["torch==2.4.0"],
        entrypoint=f"{script}:run",
        entrypoint_args=["a", "b"],
        entrypoint_kwargs={"sep": "/"},
    )

    assert getattr(handle, "_tau_train_handle", False) is True
    assert handle() == "a/b"
    manifest = handle.manifest()
    assert manifest["entrypoint"] == {
        "script": "/script/tau_entrypoint_probe.py",
        "function": "run",
        "args": ["a", "b"],
        "kwargs": {"sep": "/"},
        "pass_ctx": False,
    }


def test_train_entrypoint_can_pass_ctx_to_local_script(tmp_path):
    script = tmp_path / "job.py"
    _write_python(
        script,
        """
        def main(ctx, value):
            return f"{ctx.name}:{ctx.gpus}:{value}"
        """,
    )

    handle = tau.train(
        name="entrypoint-ctx",
        gpus=2,
        runtime_pip=["torch==2.4.0"],
        entrypoint=script,
        entrypoint_args=["ok"],
        entrypoint_pass_ctx=True,
    )

    assert handle() == "entrypoint-ctx:2:ok"


def test_train_entrypoint_submit_stages_generated_module_entrypoint_and_extra_scripts(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "tau.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))
    script = tmp_path / "train.py"
    helper = tmp_path / "helper.py"
    _write_python(script, "def main(): return 'ok'")
    _write_python(helper, "VALUE = 1")

    handle = tau.train(
        name="entrypoint-submit",
        gpus=1,
        runtime_pip=["torch==2.4.0"],
        entrypoint=script,
        extra_scripts=[helper],
    )

    res = handle.submit(dry_run="client", namespace="ray")

    assert res.returncode == 0
    config = yaml.safe_load(copied_config.read_text())
    extra_values = config["workflow"]["extra_scripts"]
    assert len(extra_values) == 3
    assert extra_values[0].endswith(":tau_user_module.py")
    assert extra_values[1].endswith(":tau_entrypoint_train.py")
    assert extra_values[2].endswith(":helper.py")
    manifest = handle.manifest()
    assert manifest["entrypoint"]["script"] == "/script/tau_entrypoint_train.py"
    assert manifest["entrypoint"]["function"] == "main"


def test_train_entrypoint_accepts_absolute_pvc_path_without_staging(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "tau.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    handle = tau.train(
        name="entrypoint-pvc",
        gpus=1,
        runtime_pip=["torch==2.4.0"],
        entrypoint="/data/scripts/train_probe.py:run_probe",
    )
    res = handle.submit(dry_run="client", namespace="ray")

    assert res.returncode == 0
    config = yaml.safe_load(copied_config.read_text())
    extra_values = config["workflow"]["extra_scripts"]
    assert len(extra_values) == 1
    assert handle.manifest()["entrypoint"]["script"] == "/data/scripts/train_probe.py"


def test_train_entrypoint_rejects_missing_relative_path(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)

    with pytest.raises(FileNotFoundError, match="relative entrypoint missing.py"):
        tau.train(name="missing", gpus=1, runtime_pip=["torch==2.4.0"], entrypoint="missing.py:main")


def test_train_entrypoint_rejects_non_json_args(tmp_path):
    script = tmp_path / "train.py"
    _write_python(script, "def main(): pass")

    with pytest.raises(ValueError, match="JSON-serializable"):
        tau.train(
            name="bad-args",
            gpus=1,
            runtime_pip=["torch==2.4.0"],
            entrypoint=script,
            entrypoint_args=[object()],
        )


def test_train_entrypoint_rejects_decorator_misuse(tmp_path):
    script = tmp_path / "train.py"
    _write_python(script, "def main(): pass")

    handle = tau.train(name="bad-decorator", gpus=1, runtime_pip=["torch==2.4.0"], entrypoint=script)

    with pytest.raises(TypeError, match="returns a train handle directly"):
        @handle
        def go(ctx):
            pass


def test_submit_forwards_gpu_resource_mode_and_node_selector_kwargs(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "tau.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(
        name="submit-gpu-mode",
        gpus=1,
        gpu_resource_mode="device-plugin",
        node_selector={"gpu": "a100", "agentpool": "gpu"},
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
    )
    def f(ctx):
        pass

    res = f.submit(dry_run="client", namespace="e2e-stack")
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307
    assert argv[1:3] == ["run", "--config"]
    config = yaml.safe_load(copied_config.read_text())
    assert config["compute"]["gpu_resource_mode"] == "device-plugin"
    assert config["policy"]["node_selector"] == {"agentpool": "gpu", "gpu": "a100"}


def test_serve_invokes_tau_cli_with_correct_args(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    fake_tau = tmp_path / "tau"
    _write_argv_recorder(fake_tau, recorder)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    service = serve_workload(
        name="example-serve",
        kind="rayservice",
        profile="ai-serve-gpu-l",
        image="registry.example.com/project/server:v1",
        import_path="project_server:app",
        checkpoint="example-run/last.safetensors",
        checkpoint_pvc="project-data",
        replicas=1,
        env={
            "MODEL_BACKEND": "compile",
            "COMPILE_MODE": "reduce-overhead",
            "HF_TOKEN": tau.secret_ref("hf-token", "token"),
        },
        runtime_pip=["transformers==4.45.0", "accelerate"],
        args=["--model", "example checkpoint"],
        volumes=["project-data=pvc:project-data"],
        mounts=["project-data:/data"],
    )

    res = service.deploy(dry_run="server", namespace="ray", kube_context="kind-taugrid", capture=True)
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[0].endswith("tau")
    assert argv[1:4] == ["serve", "deploy", "example-serve"]
    assert argv[argv.index("--kind") + 1] == "rayservice"
    assert argv[argv.index("--profile") + 1] == "ai-serve-gpu-l"
    assert argv[argv.index("--image") + 1] == "registry.example.com/project/server:v1"
    assert argv[argv.index("--import-path") + 1] == "project_server:app"
    assert argv[argv.index("--checkpoint") + 1] == "example-run/last.safetensors"
    assert argv[argv.index("--checkpoint-pvc") + 1] == "project-data"
    assert argv[argv.index("--replicas") + 1] == "1"
    env_values = [argv[idx + 1] for idx, item in enumerate(argv) if item == "--env"]
    assert "MODEL_BACKEND=compile" in env_values
    assert "COMPILE_MODE=reduce-overhead" in env_values
    env_secret_values = [argv[idx + 1] for idx, item in enumerate(argv) if item == "--env-secret"]
    assert "HF_TOKEN=hf-token:token" in env_secret_values
    pip_values = [argv[idx + 1] for idx, item in enumerate(argv) if item == "--runtime-pip"]
    assert pip_values == ["transformers==4.45.0", "accelerate"]
    assert argv[argv.index("--args") + 1] == "--model 'example checkpoint'"
    assert argv[argv.index("--volume") + 1] == "project-data=pvc:project-data"
    assert argv[argv.index("--mount") + 1] == "project-data:/data"
    assert argv[argv.index("-n") + 1] == "ray"
    assert argv[argv.index("--dry-run") + 1] == "server"
    assert argv[argv.index("--context") + 1] == "kind-taugrid"


def test_serve_validates_dry_run_without_spawning_tau():
    service = serve_workload(name="x", profile="ai-serve-gpu-l")
    with pytest.raises(ValueError, match="dry_run must be"):
        service.deploy(dry_run="bogus")


def test_submit_refuses_inside_cluster(monkeypatch):
    monkeypatch.setenv("TAU_DATA_DIR", "/data")

    @tau.train(name="x", gpus=1)
    def f(ctx):
        pass

    with pytest.raises(RuntimeError, match="inside what looks like a cluster-submitted job"):
        f.submit()


def test_submit_validates_dry_run(monkeypatch, tmp_path):
    fake_tau = tmp_path / "tau"
    fake_tau.write_text("#!/usr/bin/env python3\n")
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(name="x", gpus=1, extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="dry_run must be"):
        f.submit(dry_run="bogus")


def test_find_tau_binary_errors_clearly(monkeypatch):
    monkeypatch.delenv("TAU_BINARY", raising=False)
    monkeypatch.setenv("PATH", "/nonexistent")
    with pytest.raises(RuntimeError, match="cannot find the `tau` CLI"):
        _workloads._find_tau_binary()


def test_find_tau_binary_rejects_missing_override(monkeypatch, tmp_path):
    monkeypatch.setenv("TAU_BINARY", str(tmp_path / "ghost"))
    with pytest.raises(RuntimeError, match="does not exist"):
        _workloads._find_tau_binary()
