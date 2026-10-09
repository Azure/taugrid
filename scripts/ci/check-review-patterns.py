#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Mechanical checks for the defect classes a reviewer has actually caught.

AGENTS.md "Review and Validation Discipline" and docs/TESTING.md already list
these classes as rules; the rules alone did not stop them from reaching pushed
commits, because every one of them was checked by reading a diff.  This script
turns the mechanically detectable ones into failing checks.

Each check carries the defect numbers it would have caught and ships with a
fixture test (`--self-test`) that reintroduces the defect in a scratch tree and
asserts the check fails.  A check that cannot fail on its own fixture is not a
check, so the self-test is what makes these trustworthy.

Usage:
    python scripts/ci/check-review-patterns.py            # run against the repo
    python scripts/ci/check-review-patterns.py --self-test # test the checks
    python scripts/ci/check-review-patterns.py --list      # check -> defect map
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterable

REPO_ROOT = Path(__file__).resolve().parent.parent.parent

# Directories that never contain first-party production source for these checks.
_IGNORED_PARTS = {
    ".git",
    ".venv",
    "node_modules",
    ".ipynb_checkpoints",
    "__pycache__",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
}
# `build/` holds installed/duplicated copies of `tau/`; they are byte-identical
# artifacts, not a second implementation.
_IGNORED_SUBPATHS = ("/build/lib/", "\\build\\lib\\", "/build/scripts", "\\build\\scripts")


# --------------------------------------------------------------------------- #
# helpers
# --------------------------------------------------------------------------- #


def _iter_files(root: Path, suffixes: tuple[str, ...]) -> Iterable[Path]:
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.suffix not in suffixes:
            continue
        parts = set(path.relative_to(root).parts)
        if parts & _IGNORED_PARTS:
            continue
        as_posix = path.as_posix()
        if any(marker.strip("/\\").replace("\\", "/") in as_posix for marker in _IGNORED_SUBPATHS):
            continue
        yield path


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="replace")


def _rel(root: Path, path: Path) -> str:
    return path.relative_to(root).as_posix()


def _run(cmd: list[str], cwd: Path, timeout: int = 120) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, cwd=str(cwd), capture_output=True, text=True, timeout=timeout)


def _ver_tuple(value: str) -> tuple[int, ...]:
    parts = re.findall(r"\d+", value)
    return tuple(int(part) for part in (parts + ["0", "0", "0"])[:3])


# --------------------------------------------------------------------------- #
# check 1: harness modules import cleanly with browser env absent
# --------------------------------------------------------------------------- #

_BROWSER_ENV_KEYS = (
    "TEMP",
    "TMP",
    "LOCALAPPDATA",
    "PLAYWRIGHT_PACKAGE_ROOT",
    "PLAYWRIGHT_PROFILE_DIR",
    "HEADLESS",
    "NODE_TEST_CONTEXT",
    "TAUGRID_SERVER",
    "TAUGRID_TOKEN",
    "TAUGRID_NAMESPACE",
    "TAUGRID_E2E_RUN",
    "TAUGRID_E2E_NOTEBOOK",
    "TAUGRID_E2E_NAMESPACE",
    "KEEP_OPEN",
    "SUBMIT_PROFILE",
    "SUBMIT_QUEUE",
)


def harness_modules(root: Path) -> list[Path]:
    """Every `tools/*.mjs` harness/utility, excluding `node:test` files.

    `*.test.mjs` files register tests when imported, so importing one runs the
    suite; they are executed by `node --test`, not imported.
    """
    return [p for p in sorted((root / "tools").glob("*.mjs")) if not p.name.endswith(".test.mjs")]


def check_harness_import_clean(root: Path) -> list[str]:
    node = shutil.which("node")
    if node is None:
        return ["[import-clean] node is not on PATH; cannot import harness modules"]
    prelude = ";".join(f"delete process.env.{key}" for key in _BROWSER_ENV_KEYS)
    findings: list[str] = []
    for module in harness_modules(root):
        url = module.as_uri()
        code = f"{prelude}; await import({json.dumps(url)})"
        result = _run([node, "--input-type=module", "-e", code], cwd=root)
        lines = [line for line in (result.stderr or result.stdout).splitlines() if line.strip()]
        # The first lines carry the thrown error; the tail is node's stack.
        detail = " | ".join(lines[:4]) if lines else ""
        if result.returncode != 0:
            findings.append(
                f"[import-clean] {_rel(root, module)} throws at module load with "
                f"TEMP/LOCALAPPDATA/PLAYWRIGHT_* absent: {detail}"
            )
        elif result.stdout.strip() or result.stderr.strip():
            findings.append(
                f"[import-clean] {_rel(root, module)} is not side-effect free on import: {detail}"
            )
    return findings


# --------------------------------------------------------------------------- #
# check 2: duplicated version pins and their assertions agree
# --------------------------------------------------------------------------- #

_PIN_RE = re.compile(r"\b([A-Za-z][A-Za-z0-9_.\-]*)==([0-9][0-9A-Za-z_.\-]*)")
_ASSERT_RE = re.compile(
    r"\(\s*[\"']([A-Za-z][A-Za-z0-9_.\-]*)[\"']\s*,\s*[\"']([^\"']+)[\"']\s*\)"
)


def check_workflow_pin_assertion_agreement(root: Path) -> list[str]:
    findings: list[str] = []
    workflows = sorted((root / ".github" / "workflows").glob("*.y*ml"))
    for workflow in workflows:
        text = _read(workflow)
        pins: dict[str, set[str]] = {}
        for name, version in _PIN_RE.findall(text):
            pins.setdefault(name, set()).add(version)
        asserts: dict[str, set[str]] = {}
        for name, version in _ASSERT_RE.findall(text):
            asserts.setdefault(name, set()).add(version)
        rel = _rel(root, workflow)
        for name, versions in pins.items():
            if len(versions) > 1:
                findings.append(
                    f"[pin-agreement] {rel} pins {name} to {sorted(versions)}; "
                    "one file must pin one version"
                )
        for name, expected in asserts.items():
            if name in pins and pins[name] != expected:
                findings.append(
                    f"[pin-agreement] {rel} pins {name}=={sorted(pins[name])} but "
                    f"asserts {sorted(expected)}"
                )
    return findings


# --------------------------------------------------------------------------- #
# check 3: go.mod directive <= the toolchain of the image that builds it
# --------------------------------------------------------------------------- #

_GOLANG_FROM_RE = re.compile(r"golang:(\d+\.\d+\.\d+)")
_GO_MOD_DIRECTIVE_RE = re.compile(r"^go\s+(\d+\.\d+(?:\.\d+)?)\s*$", re.MULTILINE)
_COPY_MODULE_RE = re.compile(r"^\s*COPY\s+(?:--from=\S+\s+)?(\S+/go\.mod)\b", re.MULTILINE)


def _go_modules_for_dockerfile(root: Path, dockerfile: Path) -> list[Path]:
    modules: list[Path] = []
    text = _read(dockerfile)
    for copied in _COPY_MODULE_RE.findall(text):
        module = root / Path(copied).parent
        if (module / "go.mod").is_file():
            modules.append(module)
    if re.search(r"^\s*COPY\s+go\.mod\b", text, re.MULTILINE):
        # Bare `COPY go.mod` means the build context *is* the module. Resolve it
        # by the image directory name; this is how images/gpu-metrics-collector
        # maps to monitoring/gpu-metrics-collector.
        wanted = dockerfile.parent.name
        for gomod in sorted(root.glob("**/go.mod")):
            if ".git" in gomod.parts:
                continue
            if gomod.parent.name == wanted:
                modules.append(gomod.parent)
                break
    return modules


def check_go_directive_within_image_toolchain(root: Path) -> list[str]:
    findings: list[str] = []
    for dockerfile in sorted(root.glob("images/*/Dockerfile")):
        text = _read(dockerfile)
        match = _GOLANG_FROM_RE.search(text)
        if match is None:
            continue
        toolchain = match.group(1)
        for module in _go_modules_for_dockerfile(root, dockerfile):
            directive_match = _GO_MOD_DIRECTIVE_RE.search(_read(module / "go.mod"))
            if directive_match is None:
                continue
            directive = directive_match.group(1)
            if _ver_tuple(directive) > _ver_tuple(toolchain):
                findings.append(
                    f"[go-toolchain] {_rel(root, module / 'go.mod')} requires go >= {directive}, "
                    f"but {_rel(root, dockerfile)} builds with GOTOOLCHAIN=local on golang:{toolchain}"
                )
    return findings


# --------------------------------------------------------------------------- #
# check 4: the bounded reader has exactly one production implementation
# --------------------------------------------------------------------------- #

BOUNDED_READER = "sdk/python/python/tau/_kube_io.py"


def check_bounded_reader_single_implementation(root: Path) -> list[str]:
    findings: list[str] = []
    production: list[str] = []
    # This checker names `read1(` in its own pattern; it is not a read loop.
    self_path = Path(__file__).resolve()
    for path in _iter_files(root, (".py",)):
        rel = _rel(root, path)
        parts = path.relative_to(root).parts
        if path.resolve() == self_path:
            continue
        if "tests" in parts or path.name.startswith("test_") or path.name == "conftest.py":
            continue
        if re.search(r"read1\s*\(", _read(path)):
            production.append(rel)
    for rel in production:
        if rel != BOUNDED_READER:
            findings.append(
                f"[reader-single-source] {rel} opens its own read1() loop; bounded reads must "
                f"route through {BOUNDED_READER}"
            )
    reader = root / BOUNDED_READER
    if not reader.is_file():
        findings.append(f"[reader-single-source] {BOUNDED_READER} is missing")
    elif not re.search(r"def bounded_body\s*\(", _read(reader)):
        findings.append(f"[reader-single-source] {BOUNDED_READER} no longer defines bounded_body")
    return findings


# --------------------------------------------------------------------------- #
# check 5: every RayJob submitter Job is pinned to zero retries
# --------------------------------------------------------------------------- #

_SUBMITTER_KEY_RE = re.compile(r"[\"']submitterConfig[\"']\s*:|^submitterConfig\s*:", re.MULTILINE)
_BACKOFF_ZERO_RE = re.compile(r"backoffLimit[\s\S]{0,32}?\b0\b")


def check_submitter_backoff_pinned(root: Path) -> list[str]:
    findings: list[str] = []
    seen: set[str] = set()
    for path in _iter_files(root, (".py", ".go", ".yaml", ".yml")):
        rel = _rel(root, path)
        parts = path.relative_to(root).parts
        if "tests" in parts or path.name.endswith("_test.go") or rel.startswith("charts/"):
            continue
        if not rel.startswith(("cli/", "core/", "sdk/python/python/tau/", "portal/")):
            continue
        text = _read(path)
        for match in _SUBMITTER_KEY_RE.finditer(text):
            window = text[match.start() : match.start() + 320]
            if not _BACKOFF_ZERO_RE.search(window):
                findings.append(
                    f"[submitter-retry] {rel}: submitterConfig is emitted without "
                    "backoffLimit: 0; a retried submitter Job reports success after a failure"
                )
                break
        seen.add(rel)
    return findings


# --------------------------------------------------------------------------- #
# check 6: dedup identity keeps full wall-time precision
# --------------------------------------------------------------------------- #

_METRIC_IDENTITY = "core/exptelemetry/metric.go"
_IDENTITY_OPS = ("WallTime.Unix", "WallTime.Truncate", "WallTime.Round", "WallTime.Format")


def check_identity_preserves_wall_time(root: Path) -> list[str]:
    path = root / _METRIC_IDENTITY
    if not path.is_file():
        return [f"[identity-precision] {_METRIC_IDENTITY} is missing"]
    text = _read(path)
    match = re.search(
        r"func \(e MetricEvent\) DeterministicEventID\(\) string \{(?P<body>.*?)\n\}",
        text,
        re.DOTALL,
    )
    if match is None:
        return [f"[identity-precision] DeterministicEventID not found in {_METRIC_IDENTITY}"]
    body = match.group("body")
    findings: list[str] = []
    for op in _IDENTITY_OPS:
        if op in body:
            findings.append(
                f"[identity-precision] {_METRIC_IDENTITY} narrows a key that participates in "
                f"identity/dedup via {op}(...) ; same-tag same-step points would collide"
            )
    if "time.Time" not in body or "e.WallTime" not in body:
        findings.append(
            f"[identity-precision] {_METRIC_IDENTITY} no longer folds full-precision wall_time "
            "into the deterministic identity"
        )
    return findings


# --------------------------------------------------------------------------- #
# check 7: a browser harness pass condition needs real evidence
# --------------------------------------------------------------------------- #

_PASS_EXPR_RE = re.compile(r"(?:const|let)\s+(?:pass|ok)\s*=\s*(?P<expr>[^;]+);")
_REGEX_LITERAL_RE = re.compile(r"/(?:\\.|[^/\\\n])+/[a-z]*")
_NEGATED_RE = re.compile(
    r"!\s*(?:"
    r"(?:/(?:\\.|[^/\\\n])+/[a-z]*)"
    r"|(?:[\w$.]+\s*\.\s*)"
    r")\s*(?:includes|test)\s*\((?:[^()]|\([^()]*\))*\)",
    re.DOTALL,
)
_STRING_LITERAL_RE = re.compile(r"\"([^\"\n]*)\"|'([^'\n]*)'")
_IDENTIFIER_RE = re.compile(r"\b([A-Za-z_$][\w$]*)\b")

# A token that says "this product exists", not "the thing under test worked".
_PRODUCT_TERMS = {
    "taugrid",
    "tau",
    "grid",
    "jupyter",
    "jupyterlab",
    "notebook",
    "portal",
    "plugin",
    "ray",
    "rayjob",
    "dashboard",
    "widget",
    "panel",
}
# Names that carry no evidence about the change under test.
_GENERIC_IDENTIFIERS = {
    "const",
    "let",
    "true",
    "false",
    "null",
    "undefined",
    "test",
    "includes",
    "body",
    "text",
    "frametext",
    "statustext",
    "markertext",
    "herotext",
    "src",
    "rendered",
    "ok",
    "pass",
    "steps",
    "before",
    "document",
    "innertext",
    "textcontent",
    "queryselectorall",
    "length",
    "boolean",
    "date",
    "json",
    "string",
    "object",
}


def _evidence_atoms(expr: str) -> list[str]:
    positive = _NEGATED_RE.sub(" ", expr)
    atoms: list[str] = []
    for match in _STRING_LITERAL_RE.finditer(positive):
        atoms.append(match.group(1) or match.group(2) or "")
    for match in _REGEX_LITERAL_RE.finditer(positive):
        atoms.append(re.sub(r"^/|/[a-z]*$", "", match.group(0)))
    # Identifiers are only meaningful outside a literal. Counting the words
    # inside `/TauGrid/` again as identifiers is what made a bare product-name
    # regex look like it carried evidence.
    without_literals = _REGEX_LITERAL_RE.sub(" ", positive)
    atoms.extend(_IDENTIFIER_RE.findall(without_literals))
    return [atom for atom in atoms if atom]


def _is_evidence(atom: str) -> bool:
    stripped = re.sub(r"[^a-z0-9]", "", atom.lower())
    if not stripped:
        return False
    if stripped in _PRODUCT_TERMS:
        return False
    if atom.lower() in _GENERIC_IDENTIFIERS:
        return False
    return True


def check_harness_pass_evidence(root: Path) -> list[str]:
    findings: list[str] = []
    harnesses = sorted((root / "tools").glob("run-*-e2e.mjs"))
    for harness in harnesses:
        rel = _rel(root, harness)
        text = _read(harness)
        matches = list(_PASS_EXPR_RE.finditer(text))
        if not matches:
            findings.append(f"[pass-evidence] {rel} has no `const pass`/`const ok` result expression")
            continue
        expr = matches[-1].group("expr")
        atoms = _evidence_atoms(expr)
        if not atoms:
            findings.append(
                f"[pass-evidence] {rel} pass condition `{expr.strip()}` keys on no evidence"
            )
            continue
        if not any(_is_evidence(atom) for atom in atoms):
            findings.append(
                f"[pass-evidence] {rel} pass condition `{expr.strip()}` is satisfied by product "
                "or generic tokens only; a failure banner containing the product name passes"
            )
    return findings


# --------------------------------------------------------------------------- #
# check 8: doc citations resolve
# --------------------------------------------------------------------------- #

_CITATION_RE = re.compile(
    r"\b((?:[\w.@-]+/)*[\w.@-]+\.(?:go|py|ts|tsx|js|mjs|yaml|yml|json|md|sh|sql|kql|toml|css|html))"
    r"[:#](\d+)"
)
_CITATION_ROOTS = ("", "sdk/python/python", "sdk/python/python/labextension", "portal/frontend")


def check_doc_citations_resolve(root: Path) -> list[str]:
    findings: list[str] = []
    for doc in sorted((root / "docs").rglob("*.md")):
        rel = _rel(root, doc)
        for match in _CITATION_RE.finditer(_read(doc)):
            cited, line = match.group(1), int(match.group(2))
            if "..." in cited or cited.startswith("site/"):
                # Elided or site-only paths are not verifiable here.
                continue
            candidates = [doc.parent / cited]
            candidates += [root / base / cited if base else root / cited for base in _CITATION_ROOTS]
            resolved = [c for c in candidates if c.is_file()]
            if not resolved:
                findings.append(
                    f"[doc-citation] {rel}:{match.start()} cites {cited}:{line}, which does not "
                    "exist in the tree"
                )
                continue
            for candidate in resolved:
                length = len(_read(candidate).splitlines())
                if line > length:
                    findings.append(
                        f"[doc-citation] {rel} cites {cited}:{line}, but that file has {length} lines"
                    )
    return findings


# --------------------------------------------------------------------------- #
# check 9: no harness output is tracked and the ignore rules cover it
# --------------------------------------------------------------------------- #

_JUNK_BASENAME_GLOBS = (r".*-e2e.*\.png$", r"^jupyter-.*\.png$")
_JUNK_PROBES = (
    "junk-e2e.png",
    "custom-prefix-e2e-state.png",
    "jupyter-button-e2e-viewport.png",
    "examples/.ipynb_checkpoints/probe.ipynb",
    "a/b/.ipynb_checkpoints/probe.ipynb",
)


def _tracked_junk(root: Path) -> list[str]:
    result = _run(["git", "ls-files", "-z"], cwd=root)
    if result.returncode != 0:
        return []
    tracked = [p for p in result.stdout.split("\0") if p]
    findings: list[str] = []
    for path in tracked:
        name = path.rsplit("/", 1)[-1]
        if "/" not in path and any(re.match(glob, name) for glob in _JUNK_BASENAME_GLOBS):
            findings.append(f"[tracked-junk] {path} is a harness screenshot committed to the repo")
        if ".ipynb_checkpoints/" in path:
            findings.append(f"[tracked-junk] {path} is a Jupyter checkpoint committed to the repo")
    return findings


def check_no_tracked_harness_output(root: Path) -> list[str]:
    findings = _tracked_junk(root)
    for probe in _JUNK_PROBES:
        result = _run(["git", "check-ignore", "--no-index", "-q", "--", probe], cwd=root)
        if result.returncode != 0:
            findings.append(f"[tracked-junk] .gitignore does not ignore `{probe}`")
    return findings


# --------------------------------------------------------------------------- #
# check 10: the vulnerability scan runs on the pinned CI toolchain
# --------------------------------------------------------------------------- #

_JOB_HEADER_RE = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$", re.MULTILINE)


def _workflow_jobs(text: str) -> list[tuple[str, str]]:
    match = re.search(r"^jobs:\s*$", text, re.MULTILINE)
    if match is None:
        return []
    body = text[match.start() :]
    headers = list(_JOB_HEADER_RE.finditer(body))
    jobs: list[tuple[str, str]] = []
    for index, header in enumerate(headers):
        end = headers[index + 1].start() if index + 1 < len(headers) else len(body)
        jobs.append((header.group(1), body[header.start() : end]))
    return jobs


def check_ci_vuln_scan_pinned(root: Path) -> list[str]:
    pin_file = root / ".go-version"
    if not pin_file.is_file():
        return ["[ci-toolchain] .go-version is missing; CI toolchain cannot be pinned"]
    pin = _read(pin_file).strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+", pin):
        return [f"[ci-toolchain] .go-version does not hold a semantic version: {pin!r}"]
    findings: list[str] = []
    for workflow in sorted((root / ".github" / "workflows").glob("*.y*ml")):
        text = _read(workflow)
        for name, chunk in _workflow_jobs(text):
            if "govulncheck" not in chunk:
                continue
            pinned = "go-version-file: .go-version" in chunk or f"go-version: {pin}" in chunk
            if not pinned:
                findings.append(
                    f"[ci-toolchain] {_rel(root, workflow)} job `{name}` runs govulncheck without "
                    "pinning the .go-version toolchain; a newer local toolchain reports different "
                    "standard-library advisories than CI"
                )
    return findings


# --------------------------------------------------------------------------- #
# registry
# --------------------------------------------------------------------------- #


@dataclass(frozen=True)
class Check:
    name: str
    defects: str
    summary: str
    run: Callable[[Path], list[str]]
    defect_fixture: Callable[[Path], None]
    clean_fixture: Callable[[Path], None]


# --------------------------------------------------------------------------- #
# fixtures (used only by --self-test)
# --------------------------------------------------------------------------- #


def _write(root: Path, rel: str, text: str) -> None:
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def _git_init(root: Path) -> None:
    _run(["git", "init", "-q"], cwd=root)
    _run(["git", "config", "user.email", "self-test@example.com"], cwd=root)
    _run(["git", "config", "user.name", "self-test"], cwd=root)


def _fixture_harness_defect(root: Path) -> None:
    _write(
        root,
        "tools/run-bad-e2e.mjs",
        "const B = `${A}`;\nconst A = 'late';\nexport { A, B };\n",
    )


def _fixture_harness_clean(root: Path) -> None:
    _write(root, "tools/run-good-e2e.mjs", "export const ok = 'fine';\n")


def _fixture_pin_defect(root: Path) -> None:
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  floors:\n    steps:\n"
        "      - run: python -m pip install 'urllib3==1.26.20'\n"
        "      - run: |\n"
        "          for name, expected in ((\"urllib3\", \"1.26.0\"),):\n"
        "              pass\n",
    )


def _fixture_pin_clean(root: Path) -> None:
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  floors:\n    steps:\n"
        "      - run: python -m pip install 'urllib3==1.26.20'\n"
        "      - run: |\n"
        "          for name, expected in ((\"urllib3\", \"1.26.20\"),):\n"
        "              pass\n",
    )


def _fixture_go_defect(root: Path) -> None:
    _write(root, "images/widget/Dockerfile", "FROM golang:1.26.8 AS builder\nCOPY go.mod go.sum ./\n")
    _write(root, "monitoring/widget/go.mod", "module example.com/widget\n\ngo 1.26.9\n")


def _fixture_go_clean(root: Path) -> None:
    _write(root, "images/widget/Dockerfile", "FROM golang:1.26.8 AS builder\nCOPY go.mod go.sum ./\n")
    _write(root, "monitoring/widget/go.mod", "module example.com/widget\n\ngo 1.26.5\n")


def _fixture_reader_defect(root: Path) -> None:
    _write(root, BOUNDED_READER, "def bounded_body(response, deadline):\n    return response.read1(64)\n")
    _write(root, "tools/run-demo.py", "def call(response):\n    return response.read1(64)\n")


def _fixture_reader_clean(root: Path) -> None:
    _write(root, BOUNDED_READER, "def bounded_body(response, deadline):\n    return response.read1(64)\n")


def _fixture_submitter_defect(root: Path) -> None:
    _write(
        root,
        "cli/internal/rayjobrender/render.go",
        'spec := map[string]any{"submitterConfig": map[string]any{"backoffLimit": int64(2)}}\n',
    )


def _fixture_submitter_clean(root: Path) -> None:
    _write(
        root,
        "cli/internal/rayjobrender/render.go",
        'spec := map[string]any{"submitterConfig": map[string]any{"backoffLimit": int64(0)}}\n',
    )


def _fixture_identity_defect(root: Path) -> None:
    _write(
        root,
        _METRIC_IDENTITY,
        "package exptelemetry\n\n"
        "func (e MetricEvent) DeterministicEventID() string {\n"
        "    identity := struct {\n"
        "        Step     int64 `json:\"step\"`\n"
        "        WallTime int64 `json:\"wall_time\"`\n"
        "    }{Step: e.Step, WallTime: e.WallTime.Unix()}\n"
        "    return hash(identity)\n"
        "}\n",
    )


def _fixture_identity_clean(root: Path) -> None:
    _write(
        root,
        _METRIC_IDENTITY,
        "package exptelemetry\n\n"
        "import \"time\"\n\n"
        "func (e MetricEvent) DeterministicEventID() string {\n"
        "    identity := struct {\n"
        "        Step     int64     `json:\"step\"`\n"
        "        WallTime time.Time `json:\"wall_time\"`\n"
        "    }{Step: e.Step, WallTime: e.WallTime.UTC()}\n"
        "    return hash(identity)\n"
        "}\n",
    )


def _fixture_pass_defect(root: Path) -> None:
    _write(
        root,
        "tools/run-embed-e2e.mjs",
        "let frameText = '';\nconst pass = /TauGrid/i.test(frameText);\n",
    )


def _fixture_pass_clean(root: Path) -> None:
    _write(
        root,
        "tools/run-embed-e2e.mjs",
        "let frameText = '';\nlet heroText = '';\nlet svgCount = 0;\n"
        "const pass = /loss down/.test(heroText) && svgCount >= 1 && !/could not reach/.test(frameText);\n",
    )


def _fixture_citation_defect(root: Path) -> None:
    _write(
        root,
        "docs/design/thing.md",
        "Claim: see `portal/frontend/src/stellar/evidence-helpers.ts:85` for the helper.\n",
    )


def _fixture_citation_clean(root: Path) -> None:
    _write(root, "portal/frontend/src/real.ts", "export const evidence = 1;\n")
    _write(root, "docs/design/thing.md", "Claim: see `portal/frontend/src/real.ts:1` for the helper.\n")


def _fixture_junk_defect(root: Path) -> None:
    _git_init(root)
    _write(root, ".gitignore", "/*-e2e*.png\n.ipynb_checkpoints/\n")
    _write(root, "junk-e2e.png", "png")
    _write(root, "examples/.ipynb_checkpoints/probe.ipynb", "{}")
    _run(["git", "add", "-f", "junk-e2e.png", "examples/.ipynb_checkpoints/probe.ipynb"], cwd=root)


def _fixture_junk_clean(root: Path) -> None:
    _git_init(root)
    _write(root, ".gitignore", "/*-e2e*.png\n.ipynb_checkpoints/\n")
    _write(root, "docs/design/assets/notebook-runs-sidebar.png", "png")
    _run(["git", "add", "docs/design/assets/notebook-runs-sidebar.png"], cwd=root)


def _fixture_ci_defect(root: Path) -> None:
    _write(root, ".go-version", "1.26.9\n")
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  go:\n    steps:\n      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...\n",
    )


def _fixture_ci_clean(root: Path) -> None:
    _write(root, ".go-version", "1.26.9\n")
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  go:\n    steps:\n"
        "      - uses: actions/setup-go@v7\n"
        "        with:\n"
        "          go-version-file: .go-version\n"
        "      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...\n",
    )


CHECKS: tuple[Check, ...] = (
    Check(
        "harness-import-clean",
        "1, 8",
        "Every tools/*.mjs harness imports with TEMP/LOCALAPPDATA/PLAYWRIGHT_* absent, prints nothing.",
        check_harness_import_clean,
        _fixture_harness_defect,
        _fixture_harness_clean,
    ),
    Check(
        "workflow-pin-agreement",
        "2",
        "A version pin and the assertion about that pin agree inside one workflow file.",
        check_workflow_pin_assertion_agreement,
        _fixture_pin_defect,
        _fixture_pin_clean,
    ),
    Check(
        "go-directive-toolchain",
        "11",
        "Every go.mod built by an image has a go directive <= that image's golang tag.",
        check_go_directive_within_image_toolchain,
        _fixture_go_defect,
        _fixture_go_clean,
    ),
    Check(
        "bounded-reader-single-source",
        "3, 7, 9",
        "read1() appears in exactly one production file (tau/_kube_io.py).",
        check_bounded_reader_single_implementation,
        _fixture_reader_defect,
        _fixture_reader_clean,
    ),
    Check(
        "submitter-backoff-pinned",
        "4",
        "Every emitted KubeRay submitterConfig pins backoffLimit to 0.",
        check_submitter_backoff_pinned,
        _fixture_submitter_defect,
        _fixture_submitter_clean,
    ),
    Check(
        "identity-precision",
        "5",
        "The metric identity key folds full-precision wall_time, not a narrowed value.",
        check_identity_preserves_wall_time,
        _fixture_identity_defect,
        _fixture_identity_clean,
    ),
    Check(
        "harness-pass-evidence",
        "6",
        "A browser harness pass condition keys on evidence, not a product-name token.",
        check_harness_pass_evidence,
        _fixture_pass_defect,
        _fixture_pass_clean,
    ),
    Check(
        "doc-citations-resolve",
        "10",
        "Every file:line citation in docs/ resolves to a real file and line.",
        check_doc_citations_resolve,
        _fixture_citation_defect,
        _fixture_citation_clean,
    ),
    Check(
        "no-tracked-junk",
        "process",
        "No harness screenshot or Jupyter checkpoint is tracked, and .gitignore covers them.",
        check_no_tracked_harness_output,
        _fixture_junk_defect,
        _fixture_junk_clean,
    ),
    Check(
        "ci-toolchain-pinned",
        "12",
        "Every govulncheck job pins the .go-version toolchain.",
        check_ci_vuln_scan_pinned,
        _fixture_ci_defect,
        _fixture_ci_clean,
    ),
)


# --------------------------------------------------------------------------- #
# driver
# --------------------------------------------------------------------------- #


def run_self_test() -> int:
    failures: list[str] = []
    for check in CHECKS:
        with tempfile.TemporaryDirectory(prefix="tau-review-fixture-") as tmp:
            scratch = Path(tmp)
            check.defect_fixture(scratch)
            defect_findings = check.run(scratch)
            if not defect_findings:
                failures.append(
                    f"{check.name}: the defect fixture did NOT fail the check "
                    "(the check cannot detect its own defect)"
                )
        with tempfile.TemporaryDirectory(prefix="tau-review-fixture-") as tmp:
            scratch = Path(tmp)
            check.clean_fixture(scratch)
            clean_findings = check.run(scratch)
            if clean_findings:
                failures.append(
                    f"{check.name}: the clean fixture unexpectedly failed:\n    "
                    + "\n    ".join(clean_findings)
                )
        status = "ok" if not any(f.startswith(check.name + ":") for f in failures) else "FAIL"
        print(f"self-test {status:4}  {check.name}  (catches {check.defects})")
    if failures:
        print("\nSelf-test failures:", file=sys.stderr)
        for failure in failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1
    print(f"\n{len(CHECKS)} checks; every fixture defect failed its check and every clean fixture passed.")
    return 0


def run_checks(root: Path) -> int:
    failures: list[str] = []
    for check in CHECKS:
        findings = check.run(root)
        if findings:
            print(f"FAIL {check.name}  (catches defects {check.defects})")
            for finding in findings:
                print(f"  - {finding}")
            failures.extend(findings)
        else:
            print(f"ok   {check.name}  (catches defects {check.defects})")
    if failures:
        print(f"\n{len(failures)} review-pattern finding(s); see above.", file=sys.stderr)
        return 1
    print(f"\nAll {len(CHECKS)} review-pattern checks passed.")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="prove each check fails on its defect fixture")
    parser.add_argument("--list", action="store_true", help="list checks and the defects they catch")
    parser.add_argument("--root", type=Path, default=REPO_ROOT, help="repository root to check")
    args = parser.parse_args(argv)

    if args.list:
        for check in CHECKS:
            print(f"{check.name:32} defects {check.defects:10} {check.summary}")
        return 0
    if args.self_test:
        return run_self_test()
    return run_checks(args.root.resolve())


if __name__ == "__main__":
    raise SystemExit(main())
