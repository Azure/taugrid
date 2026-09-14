#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Check bundled links, CLI examples, and direct run YAML without cluster access.

Requires Python 3.10+, PyYAML, Git, and a compatible tau binary. Run with --help.
Default checks execute only help and run validate. Pass --snapshot to also
render against an explicitly supplied offline fixture. Training scripts and
image contents are never executed or fetched.
"""

from __future__ import annotations

import argparse
import copy
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from urllib.parse import unquote, urlsplit

try:
    import yaml
except ImportError:
    yaml = None

SKILL_ROOT = Path(__file__).resolve().parents[1]
PLACEHOLDERS = {
    "<pinned-image>": "example.invalid/runtime:1",
    "<portal-image>": "example.invalid/taugrid-portal:1",
    "<source-image-digest>": "example.invalid/source@sha256:" + "a" * 64,
    "<asset-image-digest>": "example.invalid/asset@sha256:" + "b" * 64,
}
FENCE = re.compile(r"^```(\w+)[^\n]*\n(.*?)^```\s*$", re.MULTILINE | re.DOTALL)


def markdown_files(root: Path) -> list[Path]:
    if not (root / "SKILL.md").is_file():
        raise ValueError(f"missing {root / 'SKILL.md'}")
    return [root / "SKILL.md", *sorted((root / "references").rglob("*.md"))]


def check_links(path: Path) -> int:
    checked = 0
    for target in re.findall(r"\[[^\]]+\]\(([^)]+)\)", path.read_text()):
        parsed = urlsplit(target)
        if parsed.scheme or parsed.netloc:
            continue
        destination = path.parent / unquote(parsed.path) if parsed.path else path
        if not destination.is_file():
            raise ValueError(f"{path}: missing linked file {target}")
        if parsed.fragment:
            headings = re.findall(r"^#+\s+(.*)$", destination.read_text(), re.MULTILINE)
            anchors = {
                re.sub(r"[^\w\s-]", "", heading.lower()).replace(" ", "-")
                for heading in headings
            }
            if unquote(parsed.fragment) not in anchors:
                raise ValueError(f"{path}: missing heading {target}")
        checked += 1
    return checked


def run_examples(text: str) -> list[str]:
    examples = []
    for language, body in FENCE.findall(text):
        if language not in {"yaml", "yml"}:
            continue
        for document in yaml.safe_load_all(body):
            if not isinstance(document, dict):
                continue
            nested = document.get("run") or {}
            if any(
                key in document for key in ("name", "engine", "entrypoint", "script")
            ) or (
                isinstance(nested, dict)
                and any(
                    key in nested for key in ("name", "engine", "entrypoint", "script")
                )
            ):
                if document.get("apiVersion") or document.get("schema"):
                    continue
                # Run examples are single-document inputs. Preserve the original
                # YAML for CLI validation, including duplicate/unknown fields.
                examples.append(body)
                break
    return examples


def shell_examples(text: str) -> list[list[str]]:
    examples = []
    for language, body in FENCE.findall(text):
        if language not in {"bash", "sh", "shell"}:
            continue
        for line in body.replace("\\\n", " ").splitlines():
            tokens = shlex.split(line, comments=True)
            if tokens and tokens[0] == "tau":
                examples.append(tokens[1:])
    return examples


def child_commands(help_text: str) -> set[str]:
    section = help_text.partition("Available Commands:\n")[2].split("\nFlags:")[0]
    return set(re.findall(r"^  (\S+)[ \t]+", section, re.MULTILINE))


class Tau:
    def __init__(self, binary: Path, cwd: Path, timeout: int = 30):
        self.binary = binary
        self.cwd = cwd
        self.timeout = timeout
        git = shutil.which("git")
        if not git:
            raise ValueError("Git is required for Tau's local repository discovery")
        tools = cwd / "local-tools"
        tools.mkdir(exist_ok=True)
        (tools / Path(git).name).symlink_to(Path(git).resolve())
        self.env = {
            key: value
            for key, value in os.environ.items()
            if not key.startswith(("TAU_", "GIT_"))
        }
        self.env.update(
            KUBECONFIG=str(cwd / "no-kubeconfig"),
            AZURE_CONFIG_DIR=str(cwd / "azure"),
            XDG_CONFIG_HOME=str(cwd / "config"),
            XDG_CACHE_HOME=str(cwd / "cache"),
            PATH=str(tools),
            GIT_CONFIG_GLOBAL=os.devnull,
            GIT_CONFIG_NOSYSTEM="1",
        )
        self.help_cache: dict[tuple[str, ...], str] = {}

    def run(self, *args: str) -> str:
        result = subprocess.run(
            [str(self.binary), *args],
            cwd=self.cwd,
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=self.timeout,
            check=False,
        )
        if result.returncode:
            raise ValueError(
                f"tau {' '.join(args)}:\n{result.stderr.strip() or result.stdout.strip()}"
            )
        return result.stdout

    def help(self, path: tuple[str, ...]) -> str:
        if path not in self.help_cache:
            self.help_cache[path] = self.run(*path, "--help")
        return self.help_cache[path]

    def check_command(self, tokens: list[str]) -> None:
        path: tuple[str, ...] = ()
        remaining = list(tokens)
        while remaining:
            children = child_commands(self.help(path))
            if remaining[0] not in children:
                if children and not remaining[0].startswith("-") and path != ("run",):
                    raise ValueError(
                        f"unknown subcommand: tau {' '.join((*path, remaining[0]))}"
                    )
                break
            path += (remaining.pop(0),)
            if path == ("python",) and remaining:
                # Go deliberately delegates argument parsing to the optional SDK.
                children = child_commands(self.help(path))
                if remaining[0] not in children and remaining[0] != "--help":
                    raise ValueError(f"unknown SDK command: {remaining[0]}")
                return
        flag_lines = "\n".join(
            line
            for line in self.help(path).splitlines()
            if line.lstrip().startswith("-")
        )
        flags = set(re.findall(r"--[\w-]+|(?<!\w)-[A-Za-z0-9](?!\w)", flag_lines))
        for token in remaining:
            if token.startswith("-") and token.split("=", 1)[0] not in flags:
                raise ValueError(f"unknown flag in tau {' '.join(tokens)}: {token}")


def inside(root: Path, relative: str) -> Path:
    target = (root / relative).resolve()
    if not target.is_relative_to(root.resolve()):
        raise ValueError(f"example path escapes its temporary directory: {relative}")
    return target


def prepare_example(
    body: str, directory: Path, snapshot: Path | None = None
) -> tuple[Path, Path | None, dict]:
    for placeholder, value in PLACEHOLDERS.items():
        body = body.replace(placeholder, value)
    config = yaml.safe_load(body)
    if not isinstance(config, dict):
        raise TypeError("run example must be a mapping")
    if "schema_version" in config or config.get("workflow"):
        raise ValueError("checker supports direct run examples, not managed workflows")
    policy = config.get("policy") or {}
    if not isinstance(policy, dict) or not policy.get("profile"):
        raise ValueError("runnable examples must select an explicit fixture profile")
    if "workload_profile_snapshot" in policy:
        raise ValueError("runnable examples must not supply their own test snapshot")
    nested = config.get("run") or {}
    if not nested.get("source"):
        entrypoint = (
            nested.get("entrypoint")
            or nested.get("script")
            or config.get("entrypoint")
            or config.get("script")
        )
        if not entrypoint:
            raise ValueError("runnable example is missing its entrypoint")
        script = inside(directory, entrypoint)
        script.parent.mkdir(parents=True, exist_ok=True)
        script.write_text("print('offline fixture; never executed')\n")
    original = directory / "original.yaml"
    original.write_text(body)
    if snapshot is None:
        return original, None, config
    offline = copy.deepcopy(config)
    offline_policy = offline.setdefault("policy", {})
    for field, value in {
        "namespace": "skill-tests",
        "team": "research",
        "lane": "training",
    }.items():
        offline_policy.setdefault(field, value)
    if (offline.get("metrics") or {}).get("offload", {}).get("enabled"):
        # Snapshot mode skips the live workspace that normally supplies this
        # metrics identity. This is fixture metadata, not an authorization check.
        offline_policy.setdefault("workspace", "skill-tests")
    offline_policy["workload_profile_snapshot"] = str(snapshot)
    rendered_input = directory / "offline.yaml"
    rendered_input.write_text(yaml.safe_dump(offline, sort_keys=False))
    return original, rendered_input, config


def check_render(rendered: str, config: dict, snapshot: dict) -> None:
    documents = [doc for doc in yaml.safe_load_all(rendered) if isinstance(doc, dict)]
    engine = config.get("engine") or (config.get("run") or {}).get("engine")
    kind = "RayJob" if engine in {"ray", "rayjob"} else "Job"
    workloads = [doc for doc in documents if doc.get("kind") == kind]
    if len(workloads) != 1:
        raise ValueError(f"expected one rendered {kind}, got {len(workloads)}")
    workload = workloads[0]
    profile_name = config["policy"]["profile"]
    profile = next(p for p in snapshot["profiles"] if p["name"] == profile_name)
    annotations = workload["metadata"].get("annotations", {})
    for key, value in {
        "tau.azure.com/workload-profile": profile_name,
        "tau.azure.com/workload-profile-set-hash": snapshot["profileSetHash"],
        "tau.azure.com/tau-cluster-generation": str(snapshot["tauClusterGeneration"]),
    }.items():
        if str(annotations.get(key)) != value:
            raise ValueError(f"missing or incorrect rendered profile provenance: {key}")
    spec = workload["spec"]
    if kind == "Job":
        if spec.get("completions", 1) != profile["workerCount"]:
            raise ValueError("rendered Job pod count differs from profile")
        pods = [spec["template"]["spec"]]
    else:
        cluster = spec["rayClusterSpec"]
        workers = cluster["workerGroupSpecs"]
        if sum(group["replicas"] for group in workers) != profile["workerCount"]:
            raise ValueError("rendered Ray worker count differs from profile")
        head = cluster["headGroupSpec"]["template"]["spec"]["containers"][0]
        if (
            int(head.get("resources", {}).get("limits", {}).get("nvidia.com/gpu", 0))
            != 0
        ):
            raise ValueError("Ray head unexpectedly requests a GPU")
        pods = [group["template"]["spec"] for group in workers]
    for pod in pods:
        gpu_count = int(
            pod["containers"][0]
            .get("resources", {})
            .get("limits", {})
            .get("nvidia.com/gpu", 0)
        )
        if gpu_count != profile["gpusPerWorker"]:
            raise ValueError("rendered GPU count differs from profile")
    for reference in (config.get("runtime") or {}).get("env_secret", {}).values():
        secret_name, secret_key = reference.split(":", 1)
        if f"name: {secret_name}" in rendered or f"key: {secret_key}" in rendered:
            raise ValueError("client dry-run did not redact a Secret reference")


def check(
    root: Path,
    binary: Path,
    timeout: int = 30,
    *,
    snapshot_path: Path | None = None,
) -> tuple[int, int, int, int]:
    files = markdown_files(root)
    snapshot = None
    if snapshot_path is not None:
        snapshot_path = snapshot_path.resolve()
        if not snapshot_path.is_file():
            raise ValueError(f"missing {snapshot_path}")
        snapshot = yaml.safe_load(snapshot_path.read_text())
    links = commands = examples = renders = 0
    with tempfile.TemporaryDirectory(prefix="tau-skill-check-") as temporary:
        work = Path(temporary)
        tau = Tau(binary, work, timeout)
        for path in files:
            links += check_links(path)
            text = path.read_text()
            for tokens in shell_examples(text):
                try:
                    tau.check_command(tokens)
                except ValueError as error:
                    raise ValueError(f"{path}: {error}") from error
                commands += 1
            for index, body in enumerate(run_examples(text), 1):
                directory = work / f"example-{examples + 1}"
                directory.mkdir()
                try:
                    original, offline, config = prepare_example(
                        body, directory, snapshot_path
                    )
                    tau.run("run", "validate", "--config", str(original))
                    if offline is not None:
                        rendered = tau.run(
                            "run", "--config", str(offline), "--dry-run=client"
                        )
                        check_render(rendered, config, snapshot)
                        renders += 1
                except (ValueError, KeyError, StopIteration) as error:
                    raise ValueError(
                        f"{path}: YAML example {index}: {error}"
                    ) from error
                examples += 1
                validation = "validate + render" if offline is not None else "validate"
                print(
                    f"OK {path.relative_to(root)} :: {config.get('name', index)} ({validation})"
                )
    if examples == 0:
        raise ValueError("no complete direct run examples found")
    return links, commands, examples, renders


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--tau", default="tau", help="tau executable path or name (default: PATH)"
    )
    parser.add_argument(
        "--skill-root", type=Path, default=SKILL_ROOT, help="skill directory to check"
    )
    parser.add_argument(
        "--timeout", type=int, default=30, help="timeout per CLI call in seconds"
    )
    parser.add_argument(
        "--snapshot",
        type=Path,
        help="optional offline fixture snapshot for the documented example profiles",
    )
    args = parser.parse_args(argv)
    if yaml is None:
        parser.error(
            "PyYAML is required; use a Python environment with PyYAML installed"
        )
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    binary = shutil.which(args.tau)
    if not binary:
        parser.error(
            f"tau executable not found: {args.tau}; build with `make -C cli build`"
        )
    try:
        links, commands, examples, renders = check(
            args.skill_root.resolve(),
            Path(binary).resolve(),
            args.timeout,
            snapshot_path=args.snapshot,
        )
    except (
        ValueError,
        TypeError,
        OSError,
        subprocess.TimeoutExpired,
        yaml.YAMLError,
    ) as error:
        print(f"FAIL {error}", file=sys.stderr)
        return 1
    print(
        f"links={links} commands={commands} validated={examples} rendered={renders}; offline checks passed"
    )
    if args.snapshot is None:
        print(
            "Rendering not checked; pass --snapshot with a compatible offline fixture."
        )
    print(
        "SDK proxy flags, portal commands, images, training code, and live cluster behavior are not exercised."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
