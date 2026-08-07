"""tau-py unit tests. Stdlib + pytest only — no tau binary required."""

from __future__ import annotations

import inspect
import os
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest
import yaml

import tau
from tau._cluster import render_wrapper


def _write_python(path: Path, source: str) -> None:
    path.write_text(textwrap.dedent(source).strip() + "\n")


def _write_argv_recorder(path: Path, recorder: Path, *, append: bool = False) -> None:
    if append:
        body = (
            "#!/usr/bin/env python3\n"
            "import pathlib\n"
            "import sys\n"
            f"with pathlib.Path({str(recorder)!r}).open('a') as f:\n"
            "    f.write(repr(sys.argv) + '\\n')\n"
        )
    else:
        body = (
            "#!/usr/bin/env python3\n"
            "import pathlib\n"
            "import sys\n"
            f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        )
    path.write_text(body)
    path.chmod(0o755)


def _write_config_recorder(path: Path, recorder: Path, copied_config: Path) -> None:
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_config)!r})\n"
    )
    path.chmod(0o755)


# ============================================================================
# @tau.eval decorator + cluster wrapper + orchestrator coverage
# ============================================================================


def test_eval_decorator_returns_handle():
    captured = {}

    def eval_fn(ctx: tau.Ctx, threshold: float = 0.5) -> tau.Ctx:
        captured["ctx"] = ctx
        captured["threshold"] = threshold
        return ctx

    f = tau.eval(name="ev1", after="t1", gpus=1, cpu_workers=4, team="research")(
        eval_fn
    )

    assert getattr(f, "_tau_eval_handle", False) is True
    assert getattr(f, "_tau_train_handle", False) is False
    assert f.__name__ == "eval_fn"
    assert callable(f)
    assert hasattr(f, "submit")
    assert f.after == "t1"
    assert inspect.unwrap(f) is eval_fn
    signature = inspect.signature(f)
    assert tuple(signature.parameters) == ("threshold", "upstream_checkpoint")
    signature.bind(0.75, upstream_checkpoint="/tmp/checkpoint")

    result = f(0.75, upstream_checkpoint="/tmp/checkpoint")
    assert result is captured["ctx"]
    assert captured["threshold"] == 0.75
    assert captured["ctx"].upstream_checkpoint == Path("/tmp/checkpoint")


def test_eval_decorator_rejects_reserved_local_keyword():
    with pytest.raises(ValueError, match="reserves upstream_checkpoint"):

        @tau.eval(name="ev1")
        def eval_fn(ctx, *, upstream_checkpoint=None):
            return ctx, upstream_checkpoint


def test_eval_manifest_shape():
    @tau.eval(name="my-ev", after="my-tr", gpus=1, cpu_workers=19, team="research")
    def f(ctx):
        pass

    m = f.manifest()
    assert m["schema_version"] == 1
    assert m["name"] == "my-ev"
    assert m["compute"] == {"gpus": 1}
    assert m["eval"]["cpu_workers"] == 19
    assert m["eval"]["upstream"] == "my-tr"
    # The Go side dispatches on cpu_workers > 0 (IsEval), so it must be present.
    assert "cpu_workers" in m["eval"]


def test_eval_manifest_no_upstream_when_after_omitted():
    @tau.eval(name="standalone-ev", gpus=1, cpu_workers=4)
    def f(ctx):
        pass

    m = f.manifest()
    assert m["eval"]["cpu_workers"] == 4
    assert "upstream" not in m["eval"]


def test_eval_manifest_declares_primary_data_pvc():
    @tau.eval(
        name="eval-pvc",
        gpus=1,
        cpu_workers=4,
        data_pvc="lustre-research",
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
    )
    def f(ctx):
        pass

    assert f.manifest()["storage"]["data_pvc"] == "lustre-research"


def test_eval_manifest_declares_resource_sizing():
    @tau.eval(
        name="eval-small",
        gpus=1,
        cpu_workers=4,
        cpu_request=4,
        memory_request="32Gi",
        worker_cpu_request=2,
        worker_memory_request="8Gi",
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
    )
    def f(ctx):
        pass

    assert f.manifest()["compute"] == {
        "gpus": 1,
        "cpus": 4,
        "memory": "32Gi",
        "worker_cpus": 2,
        "worker_memory": "8Gi",
    }


def test_eval_rejects_zero_cpu_workers():
    with pytest.raises(ValueError, match="cpu_workers must be >= 1"):
        @tau.eval(name="bad", cpu_workers=0)
        def f(ctx):
            pass


def test_eval_rejects_negative_gpus():
    with pytest.raises(ValueError, match="gpus must be >= 1"):
        @tau.eval(name="bad", gpus=0, cpu_workers=4)
        def f(ctx):
            pass


def test_eval_local_call_passes_ctx_with_upstream_checkpoint(tmp_path):
    captured = {}

    @tau.eval(name="local-ev", after="prev", gpus=1, cpu_workers=2)
    def f(ctx):
        captured["ctx"] = ctx

    fake_ckpt = tmp_path / "model.safetensors"
    fake_ckpt.write_bytes(b"")
    f(upstream_checkpoint=fake_ckpt)
    ctx = captured["ctx"]
    assert ctx.name == "local-ev"
    assert ctx.is_remote is False
    assert ctx.upstream_checkpoint == fake_ckpt


def test_eval_submit_invokes_tau_cli_with_correct_args(tmp_path, monkeypatch):
    """Replace tau binary with a recorder; assert eval-specific argv."""
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "eval.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.eval(name="ev-submit", after="prev-tr", gpus=1, cpu_workers=8, team="research", extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
    def f(ctx):
        pass

    res = f.submit(
        upstream_checkpoint="/data/checkpoints/prev-tr/last.safetensors",
        dry_run="client",
        kube_context="kind-taugrid",
        namespace="ray",
    )
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307 - test owns the file
    assert argv[0].endswith("tau")
    assert argv[1:3] == ["run", "--config"]
    assert argv[argv.index("--dry-run") + 1] == "client"
    config = yaml.safe_load(copied_config.read_text())
    assert config["run"]["workload_kind"] == "rayjob-eval"
    assert config["eval"]["cpu_workers"] == 8
    assert config["workflow"]["upstream_checkpoint"] == "/data/checkpoints/prev-tr/last.safetensors"
    assert config["run"]["entrypoint"].endswith("tau_py_wrapper.py")
    assert config["policy"]["namespace"] == "ray"
    assert config["policy"]["team"] == "research"


def test_eval_submit_forwards_gpu_resource_mode_and_node_selector(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_config = tmp_path / "eval.yaml"
    fake_tau = tmp_path / "tau"
    _write_config_recorder(fake_tau, recorder, copied_config)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.eval(
        name="ev-gpu-mode",
        after="prev-tr",
        gpus=1,
        cpu_workers=4,
        gpu_resource_mode="device-plugin",
        node_selector={"gpu": "a100"},
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
    )
    def f(ctx):
        pass

    res = f.submit(
        upstream_checkpoint="/data/checkpoints/prev-tr/last.safetensors",
        dry_run="client",
        namespace="e2e-stack",
        gpu_class="any",
    )
    assert res.returncode == 0

    argv = eval(recorder.read_text())  # noqa: S307
    assert argv[1:3] == ["run", "--config"]
    config = yaml.safe_load(copied_config.read_text())
    assert config["policy"]["gpu_class"] == "any"
    assert config["compute"]["gpu_resource_mode"] == "device-plugin"
    assert config["policy"]["node_selector"] == {"gpu": "a100"}


def test_eval_submit_data_pvc_override_rewrites_manifest(tmp_path, monkeypatch):
    recorder = tmp_path / "tau_argv.txt"
    copied_manifest = tmp_path / "manifest.yaml"
    fake_tau = tmp_path / "tau"
    fake_tau.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"pathlib.Path({str(recorder)!r}).write_text(repr(sys.argv))\n"
        f"shutil.copyfile(sys.argv[sys.argv.index('--config') + 1], {str(copied_manifest)!r})\n"
    )
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.eval(
        name="ev-pvc-submit",
        after="prev-tr",
        gpus=1,
        cpu_workers=4,
        data_pvc="captioner2-data",
        extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
    )
    def f(ctx):
        pass

    res = f.submit(
        upstream_checkpoint="/data/checkpoints/prev-tr/last.safetensors",
        data_pvc="lustre-research",
        dry_run="client",
        namespace="ray",
        capture=True,
    )
    assert res.returncode == 0
    manifest = yaml.safe_load(copied_manifest.read_text())
    assert manifest["storage"]["data_pvc"] == "lustre-research"


def test_eval_submit_rejects_missing_upstream_checkpoint(monkeypatch, tmp_path):
    fake_tau = tmp_path / "tau"
    fake_tau.write_text("#!/usr/bin/env true\n")
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.eval(name="ev", after="tr", cpu_workers=4)
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="requires upstream_checkpoint"):
        f.submit()


def test_eval_submit_refuses_inside_cluster(monkeypatch):
    monkeypatch.setenv("TAU_DATA_DIR", "/data")

    @tau.eval(name="ev", cpu_workers=4)
    def f(ctx):
        pass

    with pytest.raises(RuntimeError, match="inside what looks like a cluster-submitted job"):
        f.submit(upstream_checkpoint="/data/x")


def test_eval_extra_manifest_rejects_reserved_keys():
    @tau.eval(
        name="x",
        cpu_workers=4,
        extra_manifest={"compute": {"gpus": 99}},
    )
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="cannot override reserved keys"):
        f.manifest()


def test_cluster_wrapper_dispatches_to_eval_handle(tmp_path):
    """End-to-end cluster wrapper: drop a user file with @tau.eval, run it."""
    user_module = tmp_path / "researcher_eval.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.eval(name="cluster-ev", after="upstream-tr", gpus=1, cpu_workers=4)
        def go(ctx):
            print("CTX_NAME=" + ctx.name)
            print("CTX_GPUS=" + str(ctx.gpus))
            print("CTX_UPSTREAM=" + str(ctx.upstream_checkpoint))
            print("CTX_STORAGE=" + ctx.storage_hot_status + ":" + str(ctx.storage_hot_write_mbps))
            print("CTX_REMOTE=" + str(ctx.is_remote))
        """,
    )

    manifest_path = tmp_path / "m.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "cluster-ev",
            "compute": {"gpus": 1},
            "eval": {"cpu_workers": 4, "upstream": "upstream-tr"},
        },
        manifest_path.open("w"),
    )

    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)

    env = os.environ.copy()
    env["TAU_UPSTREAM_CHECKPOINT"] = "/data/checkpoints/upstream-tr/last.safetensors"
    env["TAU_STORAGE_HOT_STATUS"] = "hot"
    env["TAU_STORAGE_HOT_WRITE_MBPS"] = "123.5"
    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True,
        text=True,
        check=False,
        env=env,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}\nstdout:\n{res.stdout}"
    assert "CTX_NAME=cluster-ev" in res.stdout
    assert "CTX_GPUS=1" in res.stdout
    assert "CTX_UPSTREAM=/data/checkpoints/upstream-tr/last.safetensors" in res.stdout
    assert "CTX_STORAGE=hot:123.5" in res.stdout
    assert "CTX_REMOTE=True" in res.stdout
    # Wrapper banner should call out the eval kind so debug logs are clear.
    assert "kind=eval" in res.stdout
    assert "cpu_workers=4" in res.stdout


def test_cluster_wrapper_loads_user_module_under_staged_file_stem(tmp_path):
    """Ray workers import functions by their defining module name."""
    user_module = tmp_path / "tau_user_module.py"
    _write_python(
        user_module,
        """
        import tau

        def worker_helper():
            return "ok"

        @tau.eval(name="cluster-ev", cpu_workers=4)
        def go(ctx):
            print("MODULE_NAME=" + __name__)
            print("HELPER_MODULE=" + worker_helper.__module__)
        """,
    )
    manifest_path = tmp_path / "m.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "cluster-ev",
            "compute": {"gpus": 1},
            "eval": {"cpu_workers": 4},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)

    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}\nstdout:\n{res.stdout}"
    assert "MODULE_NAME=tau_user_module" in res.stdout
    assert "HELPER_MODULE=tau_user_module" in res.stdout


def test_cluster_wrapper_rejects_reserved_tau_module_filename(tmp_path):
    """A staged file named tau.py would shadow the cluster tau shim."""
    user_module = tmp_path / "tau.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="x", gpus=1)
        def go(ctx):
            pass
        """,
    )
    manifest_path = tmp_path / "m.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "x",
            "compute": {"gpus": 1},
            "eval": {},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)

    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert res.returncode != 0
    assert "tau.py is reserved" in res.stderr


def test_cluster_wrapper_eval_runtime_env_exports_user_module_and_tau_shim(tmp_path):
    """Eval Ray actors/tasks need an importable user module and tau shim."""
    fake_ray = tmp_path / "ray.py"
    _write_python(
        fake_ray,
        """
        _initialized = False
        last_address = None
        last_runtime_env = None

        def is_initialized():
            return _initialized

        def init(address=None, runtime_env=None):
            global _initialized, last_address, last_runtime_env
            _initialized = True
            last_address = address
            last_runtime_env = runtime_env
            print("RAY_INIT_ADDRESS=" + str(address))
            print("RAY_RUNTIME_KEYS=" + repr(sorted((runtime_env or {}).keys())))
        """,
    )
    user_module = tmp_path / "tau_user_module.py"
    _write_python(
        user_module,
        """
        import os
        import pathlib
        import subprocess
        import sys
        import tau

        def worker_helper():
            return "ok"

        @tau.eval(name="cluster-ev", cpu_workers=4)
        def go(ctx):
            import ray

            runtime_env = ray.last_runtime_env
            if runtime_env is None:
                raise RuntimeError("ray.init was not called")
            if set(runtime_env) != {"working_dir"}:
                raise RuntimeError("runtime_env included conflicting keys: " + repr(runtime_env))

            working_dir = pathlib.Path(runtime_env["working_dir"])
            if not (working_dir / "tau_user_module.py").is_file():
                raise RuntimeError("working_dir missing staged user module")
            if not (working_dir / "tau" / "__init__.py").is_file():
                raise RuntimeError("working_dir missing tau shim package")

            code = (
                "import tau_user_module, tau\\n"
                "from tau import workloads\\n"
                "try:\\n"
                "    from tau import finetune\\n"
                "except ImportError:\\n"
                "    print('NO_FINETUNE_SHIM')\\n"
                "else:\\n"
                "    raise RuntimeError('unexpected tau.finetune shim')\\n"
                "print(tau_user_module.worker_helper.__module__)\\n"
                "print(callable(tau.eval))\\n"
                "print(callable(workloads.train))\\n"
            )
            env = os.environ.copy()
            env["PYTHONPATH"] = str(working_dir)
            env["PYTHONNOUSERSITE"] = "1"
            child = subprocess.run(
                [sys.executable, "-c", code],
                cwd=str(working_dir),
                env=env,
                capture_output=True,
                text=True,
                check=False,
            )
            print(child.stdout, end="")
            if child.returncode != 0:
                raise RuntimeError(child.stderr)
            print("WORKER_IMPORT_OK")
        """,
    )
    manifest_path = tmp_path / "m.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "cluster-ev",
            "compute": {"gpus": 1},
            "eval": {"cpu_workers": 4},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)

    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}\nstdout:\n{res.stdout}"
    assert "RAY_INIT_ADDRESS=auto" in res.stdout
    assert "RAY_RUNTIME_KEYS=['working_dir']" in res.stdout
    assert "tau_user_module" in res.stdout
    assert "NO_FINETUNE_SHIM" in res.stdout
    assert "True\nTrue" in res.stdout
    assert "WORKER_IMPORT_OK" in res.stdout


def test_cluster_wrapper_finds_train_when_both_handles_present(tmp_path):
    """experiment.py with both train+eval: cluster wrapper picks train when manifest lacks cpu_workers."""
    user_module = tmp_path / "experiment.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="exp-tr", gpus=1)
        def train(ctx):
            print("TRAIN_RAN")

        @tau.eval(name="exp-ev", after="exp-tr", gpus=1, cpu_workers=4)
        def evaluate(ctx):
            print("EVAL_RAN")
        """,
    )
    # Manifest without cpu_workers → train branch.
    manifest_path = tmp_path / "tr.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "exp-tr",
            "compute": {"gpus": 1},
            "eval": {},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)
    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True, text=True, check=False,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}"
    assert "TRAIN_RAN" in res.stdout
    assert "EVAL_RAN" not in res.stdout
    assert "kind=train" in res.stdout


def test_cluster_wrapper_finds_eval_when_both_handles_present(tmp_path):
    """Same module, but eval manifest → eval branch picked."""
    user_module = tmp_path / "experiment.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="exp-tr", gpus=1)
        def train(ctx):
            print("TRAIN_RAN")

        @tau.eval(name="exp-ev", after="exp-tr", gpus=1, cpu_workers=4)
        def evaluate(ctx):
            print("EVAL_RAN")
        """,
    )
    manifest_path = tmp_path / "ev.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "exp-ev",
            "compute": {"gpus": 1},
            "eval": {"cpu_workers": 4, "upstream": "exp-tr"},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)
    env = os.environ.copy()
    env["TAU_UPSTREAM_CHECKPOINT"] = "/data/checkpoints/exp-tr/last.safetensors"
    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True, text=True, check=False, env=env,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}"
    assert "EVAL_RAN" in res.stdout
    assert "TRAIN_RAN" not in res.stdout
    assert "kind=eval" in res.stdout


def test_inspect_cli_lists_both_train_and_eval_handles(tmp_path, capsys):
    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="exp-tr", gpus=1)
        def train(ctx): pass

        @tau.eval(name="exp-ev", after="exp-tr", cpu_workers=4)
        def evaluate(ctx): pass
        """,
    )
    from tau.cli import main as cli_main
    rc = cli_main(["inspect", str(user_module)])
    assert rc == 0
    out = capsys.readouterr().out
    assert "kind=train" in out
    assert "kind=eval" in out
    assert "exp-tr" in out
    assert "exp-ev" in out
    assert out.count("schema_version: 1") == 2


def test_orchestrator_rejects_eval_after_mismatch(tmp_path, capsys):
    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="actual-tr", gpus=1)
        def train(ctx): pass

        @tau.eval(name="ev", after="wrong-name", cpu_workers=4)
        def evaluate(ctx): pass
        """,
    )
    from tau.cli import main as cli_main
    rc = cli_main(["submit", str(user_module), "--dry-run=client"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "does not match" in err
    assert "wrong-name" in err
    assert "actual-tr" in err


def test_orchestrator_rejects_eval_after_no_train(tmp_path, capsys):
    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.eval(name="ev", after="ghost-train", cpu_workers=4)
        def evaluate(ctx): pass
        """,
    )
    from tau.cli import main as cli_main
    rc = cli_main(["submit", str(user_module), "--dry-run=client"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "no @tau.train handle is defined" in err


def test_orchestrator_train_and_eval_dry_run(tmp_path, monkeypatch, capsys):
    """Orchestrator: dry-run skips polling and still submits both jobs."""
    recorder = tmp_path / "argv-log.txt"
    copied_configs = tmp_path / "configs"
    copied_configs.mkdir()
    fake_tau = tmp_path / "tau"
    fake_tau.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"rec = pathlib.Path({str(recorder)!r})\n"
        "with rec.open('a') as f:\n"
        "    f.write(repr(sys.argv) + '\\n')\n"
        "cfg = pathlib.Path(sys.argv[sys.argv.index('--config') + 1])\n"
        f"shutil.copyfile(cfg, pathlib.Path({str(copied_configs)!r}) / (cfg.stem + '.yaml'))\n"
    )
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="exp-tr", gpus=1, extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
        def train(ctx): pass

        @tau.eval(name="exp-ev", after="exp-tr", cpu_workers=4, extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
        def evaluate(ctx): pass
        """,
    )

    from tau.cli import main as cli_main
    rc = cli_main(["submit", str(user_module), "--dry-run=client"])
    assert rc == 0

    invocations = [eval(line) for line in recorder.read_text().splitlines() if line]
    assert len(invocations) == 2, f"expected 2 tau CLI invocations, got: {invocations}"
    train_argv, eval_argv = invocations
    assert train_argv[1:3] == ["run", "--config"]
    assert eval_argv[1:3] == ["run", "--config"]
    train_config = yaml.safe_load((copied_configs / "exp-tr.yaml").read_text())
    eval_config = yaml.safe_load((copied_configs / "exp-ev.yaml").read_text())
    assert train_config["run"]["workload_kind"] == "rayjob"
    assert eval_config["run"]["workload_kind"] == "rayjob-eval"
    assert eval_config["workflow"]["upstream_checkpoint"] == "/data/checkpoints/finetunes/exp-tr/artifacts/last.safetensors"
    assert eval_config["eval"]["cpu_workers"] == 4


def test_orchestrator_uses_declared_train_checkpoint_artifact(tmp_path, monkeypatch):
    recorder = tmp_path / "argv-log.txt"
    copied_configs = tmp_path / "configs"
    copied_configs.mkdir()
    fake_tau = tmp_path / "tau"
    fake_tau.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, shutil, sys\n"
        f"rec = pathlib.Path({str(recorder)!r})\n"
        "with rec.open('a') as f:\n"
        "    f.write(repr(sys.argv) + '\\n')\n"
        "cfg = pathlib.Path(sys.argv[sys.argv.index('--config') + 1])\n"
        f"shutil.copyfile(cfg, pathlib.Path({str(copied_configs)!r}) / (cfg.stem + '.yaml'))\n"
    )
    fake_tau.chmod(0o755)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(
            name="exp-tr",
            gpus=1,
            checkpoint_artifact="rank0/final.safetensors",
            extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}},
        )
        def train(ctx): pass

        @tau.eval(name="exp-ev", after="exp-tr", extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
        def evaluate(ctx): pass
        """,
    )

    from tau.cli import main as cli_main

    rc = cli_main(["submit", str(user_module), "--dry-run=client"])
    assert rc == 0

    eval_config = yaml.safe_load((copied_configs / "exp-ev.yaml").read_text())
    assert eval_config["workflow"]["upstream_checkpoint"] == "/data/checkpoints/finetunes/exp-tr/artifacts/rank0/final.safetensors"


@pytest.mark.parametrize("checkpoint", ["", "../final.safetensors"])
def test_orchestrator_rejects_invalid_train_checkpoint_artifact(checkpoint):
    from tau.cli import _checkpoint_artifact_for_handle

    class BadHandle:
        def manifest(self):
            return {"artifacts": {"checkpoint": checkpoint}}

    with pytest.raises(RuntimeError, match="artifacts.checkpoint"):
        _checkpoint_artifact_for_handle(BadHandle())


def test_orchestrator_resource_name_for_handle_uses_manifest_prefix():
    from tau.cli import _resource_name_for_handle

    @tau.train(name="default-name", gpus=1, extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
    def default_train(ctx):
        pass

    @tau.train(
        name="custom-name",
        gpus=1,
        extra_manifest={
            "resource_naming": {"prefix": "diffusion"},
            "runtime": {"pip": ["torch==2.4.0"]},
        },
    )
    def custom_train(ctx):
        pass

    assert _resource_name_for_handle(default_train) == "tau-default-name"
    assert _resource_name_for_handle(custom_train) == "diffusion-custom-name"


def test_orchestrator_rejects_multiple_train_handles(tmp_path, capsys):
    user_module = tmp_path / "exp.py"
    _write_python(
        user_module,
        """
        import tau

        @tau.train(name="tr-a", gpus=1)
        def a(ctx): pass

        @tau.train(name="tr-b", gpus=1)
        def b(ctx): pass
        """,
    )
    from tau.cli import main as cli_main
    rc = cli_main(["submit", str(user_module), "--dry-run=client"])
    assert rc == 2
    err = capsys.readouterr().err
    assert "multiple @tau.train handles" in err


def test_parse_duration_handles_units():
    from tau.cli import _parse_duration
    assert _parse_duration("30s") == 30.0
    assert _parse_duration("5m") == 300.0
    assert _parse_duration("2h") == 7200.0
    assert _parse_duration("90") == 90.0  # bare number = seconds
    with pytest.raises(ValueError, match="invalid duration"):
        _parse_duration("not-a-duration")


def test_ctx_dataclass_has_upstream_checkpoint_field():
    """Regression: Ctx default must carry None for non-eval invocations."""
    ctx = tau.Ctx(name="x", gpus=1)
    assert ctx.upstream_checkpoint is None


def test_cluster_wrapper_top_level_eval_import_works(tmp_path):
    """`from tau import eval` (alias) must work in user modules on-cluster."""
    user_module = tmp_path / "ev_import.py"
    _write_python(
        user_module,
        """
        from tau import eval as tau_eval

        @tau_eval(name="aliased-ev", cpu_workers=4)
        def go(ctx):
            print("ALIASED_OK")
        """,
    )
    manifest_path = tmp_path / "m.yaml"
    yaml.safe_dump(
        {
            "schema_version": 1,
            "name": "aliased-ev",
            "compute": {"gpus": 1},
            "eval": {"cpu_workers": 4},
        },
        manifest_path.open("w"),
    )
    wrapper_src = render_wrapper(user_module.name).replace(
        'USER_MODULE_PATH = "/script/" + USER_MODULE_FILENAME',
        f'USER_MODULE_PATH = {str(user_module)!r}',
    )
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper_src)
    res = subprocess.run(
        [sys.executable, str(wrapper_path), "--manifest", str(manifest_path)],
        capture_output=True, text=True, check=False,
    )
    assert res.returncode == 0, f"stderr:\n{res.stderr}\nstdout:\n{res.stdout}"
    assert "ALIASED_OK" in res.stdout


# ----- runtime.pip deprecation warning ------------------------------------


def _make_fake_tau(tmp_path):
    fake_tau = tmp_path / "tau"
    fake_tau.write_text("#!/usr/bin/env python3\n")
    fake_tau.chmod(0o755)
    return fake_tau


def test_runtime_pip_passes_through_to_manifest():
    """User-supplied runtime.pip must reach the rendered manifest unmodified."""
    @tau.train(
        name="rt-pip", gpus=1,
        extra_manifest={"runtime": {"pip": ["torch==2.4.0", "mypkg==1.0"]}},
    )
    def f(ctx):
        return ctx
    m = f.manifest()
    assert m["runtime"] == {"pip": ["torch==2.4.0", "mypkg==1.0"]}


def test_submit_raises_when_runtime_pip_unset(tmp_path, monkeypatch):
    """Submitting without an explicit runtime.pip must hard-fail with ValueError.

    Replaces the prior DeprecationWarning behavior: the SDK now raises before
    spawning the tau binary so misconfigured jobs don't silently fall back
    to a default pip list (tau ships no defaults)."""
    fake_tau = _make_fake_tau(tmp_path)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(name="warn-default", gpus=1)
    def f(ctx):
        pass

    with pytest.raises(ValueError, match="runtime.pip"):
        f.submit(dry_run="client", kube_context="kind-taugrid", namespace="ray")


def test_submit_silent_when_runtime_pip_set(tmp_path, monkeypatch):
    """Submitting with an explicit runtime.pip must NOT raise."""
    import warnings as _w
    fake_tau = _make_fake_tau(tmp_path)
    monkeypatch.setenv("TAU_BINARY", str(fake_tau))

    @tau.train(
        name="warn-explicit", gpus=1,
        extra_manifest={"runtime": {"pip": ["torch==2.4.0", "transformers==4.45.0"]}},
    )
    def f(ctx):
        pass

    with _w.catch_warnings(record=True) as caught:
        _w.simplefilter("always")
        f.submit(dry_run="client", kube_context="kind-taugrid", namespace="ray")
    runtime_warnings = [w for w in caught if "runtime.pip" in str(w.message)]
    assert not runtime_warnings, (
        "unexpected DeprecationWarning fired: "
        + repr([str(w.message) for w in runtime_warnings])
    )
