#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Mechanical checks for the defect classes a reviewer has actually caught.

AGENTS.md "Review and Validation Discipline" and docs/TESTING.md already list
these classes as rules; the rules alone did not stop them from reaching pushed
commits, because every one of them was checked by reading a diff.  This script
turns the mechanically detectable ones into failing checks.

It was built from one review round, and it passed ten green checks over a tree
that still held eighteen defects.  The first ten ratcheted against the *shapes*
of that round.  The next eight target the *modes* behind them, keyed to this
repository's real seams: the authorization decision, the per-request server
lifetime, the render/metadata contract, and the CI image inventory.  The stance
those modes come from is written down in `skills/taugrid-review/SKILL.md`.

A check whose defect is still present in this tree is listed in
``EXPECTED_FAILURES``: it still runs, still fails, and still prints every
finding, but it does not block the build.  The entry is the defect's tracking
note, and deleting it turns the check back into a hard failure.  A check listed
there that starts passing is reported as stale so the entry is removed rather
than quietly widening the blind spot it was meant to close.

Each check carries the defect numbers it would have caught and ships with a
fixture test (`--self-test`) that reintroduces the defect in a scratch tree and
asserts the check fails.  A check that cannot fail on its own fixture is not a
check, so the self-test is what makes these trustworthy.

Usage:
    python scripts/ci/check-review-patterns.py                # run against the repo
    python scripts/ci/check-review-patterns.py --self-test    # test the checks
    python scripts/ci/check-review-patterns.py --list         # check -> defect map
    python scripts/ci/check-review-patterns.py --scope-report # blast radius, advisory
"""

from __future__ import annotations

import argparse
import ast
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
            # The pin may be written directly, or reached through a GitHub
            # expression that resolves to it per matrix leg - a job that selects
            # one module's own go.mod and falls back to .go-version is pinning the
            # toolchain, and rejecting it for not being the literal string is a
            # false positive rather than a finding.
            version_file = re.search(r"go-version-file:\s*(\S.*)", chunk)
            pinned = bool(version_file and ".go-version" in version_file.group(1))
            pinned = pinned or f"go-version: {pin}" in chunk
            if not pinned:
                findings.append(
                    f"[ci-toolchain] {_rel(root, workflow)} job `{name}` runs govulncheck without "
                    "pinning the .go-version toolchain; a newer local toolchain reports different "
                    "standard-library advisories than CI"
                )
    return findings


# --------------------------------------------------------------------------- #
# checks 11-18: the defect classes a diff-reader missed
#
# The ten checks above ratcheted against the shapes of one review round.  These
# target the *modes* that round missed, and are keyed to TauGrid's real seams
# rather than to a shape: the authorization seam, the per-request server
# lifetime, the render/metadata contract, and the CI image inventory.  Their
# stance is written down in `skills/taugrid-review/SKILL.md`.
#
# Class map (see AGENTS.md "Review and Validation Discipline"):
#   C1  authorization input derived from the object being authorized
#   C2  handler that reads or writes cluster state without an authorization call
#   C3  a count of past attempts treated as a terminal-state predicate
#   C4  a value resolved for the target replaced by a fallback
#   C5  a per-request resource created and never disposed
#   C6  a discovery label stamped without the consumer's identity labels
#   C7  a multi-line value interpolated into a shell command without framing
#   C10 an image the feature depends on that no workflow builds
# --------------------------------------------------------------------------- #

#: Production Python surfaces the notebook plugin owns.
_NOTEBOOK_SURFACE = ("sdk/python/python/tau/",)
_JUPYTER_SERVER = "sdk/python/python/tau/jupyter/server.py"
_WORKLOAD_KEYS = "core/workloadmeta/keys.go"

#: The per-request surface where a resource created for one submission can
#: outlive it.  A CLI one-shot (tau/build.py, which releases in a finally) and
#: an in-workload worker (tau/_cluster.py, whose runtime_env working dir must
#: survive for the lifetime of the job) have different lifetimes and are
#: deliberately out of scope: a blanket "no unreleased mkdtemp" rule would flag
#: both, and neither is the leak class.
_SERVER_LIFETIME_SURFACE = (
    "sdk/python/python/tau/jupyter/",
    "sdk/python/python/tau/widgets/",
)

#: Callees that mean a handler is reading or writing cluster state.
_CLUSTER_TOUCH_CALLS = {
    "read",
    "_call_with_client",
    "load_client",
    "build",
    "build_plan",
    "submit_plan",
    "read_run",
    "read_logs",
    "list_runs",
    "list_namespaces",
    "list_destinations",
    "server_root",
}

#: A permission decision, as opposed to an identity check.  `@web.authenticated`
#: and `load_client` say *who*; only these say *whether*.
_AUTHZ_CALL_RE = re.compile(
    r"(is_authorized|authoriz|can_access|has_permission|permitted|allowed|check_access)",
    re.IGNORECASE,
)

#: Resolvers that derive an authorization input from a named workload profile.
_AUTH_INPUT_RESOLVERS = {
    "resolve_applicability",
    "resolve_scope",
    "resolve_team",
    "resolve_lane",
    "authorization_scope",
    "scope_for_profile",
    "applicability_for",
}

#: Helpers that pick the workload profile under consideration.
_SELECTION_CALLS = {"_select_profile", "select_profile", "pick_profile", "choose_profile"}

#: Names that hold the object being authorized.
_CANDIDATE_HINTS = {"selected", "candidate", "requested", "profile_name", "wanted", "chosen"}

#: Functions that make an authorization decision from a scope.
_AUTHZ_PREDICATES = {
    "ready_profiles",
    "destination_profiles",
    "authorized_profiles",
    "allowed_profiles",
    "permitted_profiles",
    "is_authorized",
    "authorize",
    "check_authorization",
    "require_authorization",
}

#: Status fields that are counters of past attempts, not current-state
#: predicates.  `status.failed` stays positive across a retry and after that
#: retry succeeds, so it can never classify a healthy run.
_ATTEMPT_COUNTERS = {
    "failed",
    "failures",
    "restarts",
    "restart_count",
    "restartcount",
    "attempts",
    "retries",
    "backoff",
}

_TERMINAL_STATES = {"failed", "failure", "complete", "completed", "succeeded", "success", "error"}

#: Resolvers that answer "which queue does *this profile* use in *this
#: namespace*".  ``_profile.profile_queue`` mirrors the Go ``RenderProfile``
#: precedence: namespace LocalQueue, else the profile default, else fallback.
_PROFILE_QUEUE_RESOLVERS = {
    "_profile_queue",
    "profile_queue",
    "resolved_queue",
    "local_queue_for",
    "render_queue",
}

#: Keys whose value is handed to a shell as a command line.
_SHELL_COMMAND_KEYS = {"entrypoint", "command", "args", "shell", "script"}

#: How a multi-line command is made safe to interpolate.
_SHELL_FRAMING_MARKERS = ("shlex.quote", "base64", "printf", "<<", "heredoc", "quoted_command")


def _production_python(root: Path, prefixes: tuple[str, ...]) -> Iterable[Path]:
    """First-party Python under ``prefixes``, excluding tests and build copies."""
    for path in _iter_files(root, (".py",)):
        rel = _rel(root, path)
        if not rel.startswith(prefixes):
            continue
        parts = path.relative_to(root).parts
        if "tests" in parts or path.name.startswith("test_") or path.name == "conftest.py":
            continue
        yield path


def _parse_python(path: Path) -> Optional[ast.Module]:
    try:
        return ast.parse(_read(path), filename=str(path))
    except SyntaxError:
        return None


def _call_name(node: ast.AST) -> str:
    if not isinstance(node, ast.Call):
        return ""
    func = node.func
    if isinstance(func, ast.Name):
        return func.id
    if isinstance(func, ast.Attribute):
        return func.attr
    return ""


def _names_in(node: ast.AST) -> set[str]:
    return {child.id for child in ast.walk(node) if isinstance(child, ast.Name)}


def _assigned_names(node: ast.Assign) -> set[str]:
    """Every name an assignment binds, including tuple-unpacking targets.

    `derived_team, derived_lane = resolve_applicability(...)` is the shape this
    exists for: a tuple target, so a flat `target.id` check sees nothing.
    """
    names: set[str] = set()
    for target in node.targets:
        for child in ast.walk(target):
            if isinstance(child, ast.Name):
                names.add(child.id)
    return names


def _attr_names(node: ast.Assign) -> set[str]:
    names: set[str] = set()
    for target in node.targets:
        for child in ast.walk(target):
            if isinstance(child, ast.Attribute):
                names.add(child.attr)
    return names


def _called(node: ast.AST, names: set[str]) -> bool:
    return any(isinstance(child, ast.Call) and _call_name(child) in names for child in ast.walk(node))


def _string_subscripts(node: ast.AST, wanted: set[str]) -> set[str]:
    found: set[str] = set()
    for child in ast.walk(node):
        if not isinstance(child, ast.Subscript):
            continue
        index = child.slice
        if isinstance(index, ast.Constant) and isinstance(index.value, str):
            if index.value.lower() in wanted:
                found.add(index.value)
    return found


# --------------------------------------------------------------------------- #
# check 11 (C1): an authorization input never comes from the object authorized
# --------------------------------------------------------------------------- #


def check_authorization_input_provenance(root: Path) -> list[str]:
    """Flag an authorization input read out of the candidate being authorized.

    The shape: a resolver derives a scope value from a *selected profile*, that
    derived value builds the scope handed to an authorization predicate, and the
    profile is then authorized against a decision it supplied the input for.  A
    caller with no team then authorizes itself for team-scoped capacity.

    This is a targeted structural rule over TauGrid's authorization seam rather
    than a generic detector.  The semantics still need a negative behavioural
    test: a team-scoped profile must be refused when the caller's team is
    unknown.  The structural rule cannot prove that, only that the construction
    which makes it impossible is absent.
    """
    findings: list[str] = []
    for path in _production_python(root, _NOTEBOOK_SURFACE):
        tree = _parse_python(path)
        if tree is None:
            continue
        rel = _rel(root, path)
        for fn in ast.walk(tree):
            if not isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            candidates: set[str] = set()
            for node in ast.walk(fn):
                if isinstance(node, ast.Assign) and _called(node.value, _SELECTION_CALLS):
                    candidates |= _assigned_names(node)
            candidates |= {
                child.id
                for child in ast.walk(fn)
                if isinstance(child, ast.Name) and child.id in _CANDIDATE_HINTS
            }
            if not candidates:
                continue

            derived: set[str] = set()
            for node in ast.walk(fn):
                if not isinstance(node, ast.Assign):
                    continue
                resolver_calls = [
                    child
                    for child in ast.walk(node.value)
                    if isinstance(child, ast.Call) and _call_name(child) in _AUTH_INPUT_RESOLVERS
                ]
                if resolver_calls and any(_names_in(call) & candidates for call in resolver_calls):
                    derived |= _assigned_names(node)
            if not derived:
                continue

            scope_calls = [
                child
                for child in ast.walk(fn)
                if isinstance(child, ast.Call)
                and _call_name(child) == "ProfileScope"
                and (_names_in(child) & derived)
            ]
            if not scope_calls:
                continue

            scope_names: set[str] = set()
            for node in ast.walk(fn):
                if isinstance(node, ast.Assign) and isinstance(node.value, ast.Call):
                    if any(node.value is call for call in scope_calls):
                        scope_names |= _assigned_names(node)

            for node in ast.walk(fn):
                if not isinstance(node, ast.Call) or _call_name(node) not in _AUTHZ_PREDICATES:
                    continue
                references = _names_in(node) & (derived | scope_names)
                positional = any(arg is call for arg in node.args for call in scope_calls)
                if not (references or positional):
                    continue
                findings.append(
                    f"[authorization-provenance] {rel}:{node.lineno} {fn.name}() builds an "
                    "authorization scope from a value resolved out of the candidate being "
                    f"authorized ({', '.join(sorted(references or derived))}); a caller with no "
                    "team authorizes itself for team-scoped capacity"
                )
                break
    return findings


# --------------------------------------------------------------------------- #
# check 12 (C2): cluster-state handlers must authorize, not just authenticate
# --------------------------------------------------------------------------- #


def _class_methods(cls: ast.ClassDef) -> dict[str, ast.AST]:
    """Top-level methods defined on one class, keyed by name."""
    return {
        node.name: node
        for node in cls.body
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
    }


def _calls_in(node: ast.AST) -> list[ast.Call]:
    return [child for child in ast.walk(node) if isinstance(child, ast.Call)]


def _authorization_call(node: ast.AST) -> bool:
    return any(_AUTHZ_CALL_RE.search(_call_name(call)) for call in _calls_in(node))


def _touches_cluster(node: ast.AST) -> bool:
    """Whether ``node`` reaches a cluster read/write.

    A direct call covers ``self.read(...)``. A cluster-touching method passed as
    a value covers ``asyncio.to_thread(self.build, body)``, whose outer call is
    ``to_thread`` and which the call-name check alone would miss.
    """
    for call in _calls_in(node):
        if _call_name(call) in _CLUSTER_TOUCH_CALLS:
            return True
    return any(
        isinstance(child, ast.Attribute) and child.attr in _CLUSTER_TOUCH_CALLS
        for child in ast.walk(node)
    )


def _guard_methods(cls: ast.ClassDef) -> set[str]:
    """Methods on ``cls`` that consult an authorizer as part of their own work.

    A method that *is* the authorization helper (``authorize``, ``is_authorized``,
    ``has_permission``...) defines the primitive; the fact that its body calls the
    low-level Authorizer does not guard any cluster operation by itself, and
    counting it is what made this check vacuous: ``_ClientMixin`` defines
    ``authorize()``, so every handler inheriting the mixin passed without ever
    calling it. Excluding the helper's own definition leaves only methods that
    actually decide before doing work.
    """
    guarded: set[str] = set()
    for name, fn in _class_methods(cls).items():
        if _AUTHZ_CALL_RE.search(name):
            continue
        if _authorization_call(fn):
            guarded.add(name)
    return guarded


def _referenced_names(node: ast.AST) -> set[str]:
    """Method names reached inside ``node``: call targets and passed callables.

    Attribute references are included so ``asyncio.to_thread(self.read, ...)``
    counts as calling ``read``. The result is only ever intersected with the set
    of method names that actually guard, so reading an unrelated attribute cannot
    make a handler pass.
    """
    names: set[str] = set()
    for call in _calls_in(node):
        func = call.func
        if isinstance(func, ast.Attribute):
            names.add(func.attr)
        elif isinstance(func, ast.Name):
            names.add(func.id)
    names |= {
        child.attr for child in ast.walk(node) if isinstance(child, ast.Attribute)
    }
    return names


def _base_names(cls: ast.ClassDef) -> set[str]:
    return {
        base.id if isinstance(base, ast.Name) else base.attr
        for base in cls.bases
        if isinstance(base, (ast.Name, ast.Attribute))
    }


def _inherited_guard_names(cls: ast.ClassDef, classes: dict[str, ast.ClassDef]) -> set[str]:
    """Guard-method names reachable on ``cls`` through its inheritance chain.

    Only names defined on a base are included; the handler still has to *call*
    one of them for the base to guard anything on its behalf.
    """
    guarded = set(_guard_methods(cls))
    seen = set()
    pending = list(_base_names(cls))
    while pending:
        name = pending.pop()
        if name in seen or name not in classes:
            continue
        seen.add(name)
        base = classes[name]
        guarded |= _guard_methods(base)
        pending.extend(_base_names(base))
    return guarded


def check_handler_authorization_required(root: Path) -> list[str]:
    """Flag a handler that reads or writes cluster state without a permission call.

    ``@web.authenticated`` establishes identity.  It is not permission, so a
    denied Jupyter user would submit or read under the server's Kubernetes
    identity.  Evidence must be a call, not the definition of an authorization
    helper: a handler passes when one of its cluster-touching methods also makes
    an authorization call, or when it calls a guarded method it inherits (a
    ``_ClientMixin.read`` that itself calls ``self.authorize(...)``).
    """
    path = root / _JUPYTER_SERVER
    if not path.is_file():
        return []
    tree = _parse_python(path)
    if tree is None:
        return []
    classes = {node.name: node for node in ast.walk(tree) if isinstance(node, ast.ClassDef)}
    findings: list[str] = []
    for name, cls in classes.items():
        if not name.endswith("Handler"):
            continue
        methods = _class_methods(cls)
        touching = {
            method for method, fn in methods.items() if _touches_cluster(fn)
        }
        if not touching:
            continue
        # (a) The method that touches the cluster also makes the permission call.
        direct = touching & _guard_methods(cls)
        # (b) The handler calls a guarded method it inherits. This accepts the
        # "centralize the check in _ClientMixin.read" fix without accepting a
        # mixin that merely defines an authorize() helper.
        reached = set().union(*(_referenced_names(methods[method]) for method in touching))
        inherited = reached & _inherited_guard_names(cls, classes)
        if direct or inherited:
            continue
        findings.append(
            f"[handler-authorization] {_JUPYTER_SERVER} class {name} reads or writes cluster "
            "state with only @web.authenticated; authentication is not authorization, so a denied "
            "user acts under the server's Kubernetes identity"
        )
    return findings


# --------------------------------------------------------------------------- #
# check 13 (C3): a terminal state is decided from current state, not from a count
# --------------------------------------------------------------------------- #


def check_terminal_state_from_current_state(root: Path) -> list[str]:
    """Flag an attempt counter used as the predicate for a terminal state.

    A counter of past failures is not a current-state predicate: it stays
    positive through a retry and after that retry succeeds, so the classifier
    reports a healthy run as failed and stops watching a live one.
    """
    findings: list[str] = []
    for path in _production_python(root, _NOTEBOOK_SURFACE):
        tree = _parse_python(path)
        if tree is None:
            continue
        rel = _rel(root, path)
        for node in ast.walk(tree):
            if not isinstance(node, ast.If):
                continue
            counters = {
                child.attr
                for child in ast.walk(node.test)
                if isinstance(child, ast.Attribute) and child.attr.lower() in _ATTEMPT_COUNTERS
            }
            counters |= _string_subscripts(node.test, _ATTEMPT_COUNTERS)
            if not counters:
                continue
            terminal = {
                child.value
                for child in ast.walk(node)
                if isinstance(child, ast.Return)
                for child in ast.walk(child)
                if isinstance(child, ast.Constant)
                and isinstance(child.value, str)
                and child.value.lower() in _TERMINAL_STATES
            }
            if not terminal:
                continue
            line = getattr(node, "lineno", 0)
            findings.append(
                f"[terminal-state] {rel}:{line} classifies {sorted(terminal)} from the attempt "
                f"counter {sorted(counters)}, which counts past attempts and stays positive after a "
                "retry succeeds; decide from the current status field instead"
            )
    return findings


# --------------------------------------------------------------------------- #
# check 14 (C4): a value resolved for the target is not replaced by a fallback
# --------------------------------------------------------------------------- #


def check_resolved_value_not_discarded(root: Path) -> list[str]:
    """Flag a renderer that resolves a profile but renders a fallback queue.

    ``_profile.profile_queue`` mirrors the Go ``RenderProfile`` precedence and
    answers "which LocalQueue does this profile use in this namespace".  When
    the submission path resolves a profile and then renders the workspace
    default instead, GPU work can be admitted against the wrong quota path.
    """
    findings: list[str] = []
    for path in _production_python(root, _NOTEBOOK_SURFACE):
        tree = _parse_python(path)
        if tree is None:
            continue
        rendered = [
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.Call) and _call_name(node) == "render_rayjob"
        ]
        if not rendered:
            continue
        if not any(
            isinstance(node, ast.Call) and _call_name(node) in _AUTHZ_PREDICATES
            for node in ast.walk(tree)
        ):
            continue
        if any(
            isinstance(node, ast.Call) and _call_name(node) in _PROFILE_QUEUE_RESOLVERS
            for node in ast.walk(tree)
        ):
            continue
        call = rendered[0]
        findings.append(
            f"[profile-queue] {_rel(root, path)}:{call.lineno} resolves a workload profile but "
            "renders render_rayjob(queue=...) without consulting that profile's resolved local "
            "queues; the workspace default is a fallback, not the resolved queue"
        )
    return findings


# --------------------------------------------------------------------------- #
# check 15 (C5): a per-request resource is tracked and released
# --------------------------------------------------------------------------- #


def check_staging_resource_disposal(root: Path) -> list[str]:
    """Flag a staging directory created per submission and never released.

    A long-running Jupyter server that creates a directory per submission leaks
    notebook contents on success, on failure, and on close unless the object
    that creates it also disposes it.  Scope is the per-request surface; see
    ``_SERVER_LIFETIME_SURFACE`` for why the CLI and in-workload worker are out.
    """
    findings: list[str] = []
    disposals = {"rmtree", "rmdir", "unlink", "cleanup", "shutil_rmtree"}
    creators = {"mkdtemp", "TemporaryDirectory", "mkstemp"}
    for path in _production_python(root, _SERVER_LIFETIME_SURFACE):
        tree = _parse_python(path)
        if tree is None:
            continue
        rel = _rel(root, path)
        disposed_attributes: set[str] = set()
        for cls in [node for node in ast.walk(tree) if isinstance(node, ast.ClassDef)]:
            for node in ast.walk(cls):
                if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    continue
                if node.name not in {"close", "cleanup", "dispose", "__exit__", "stop"}:
                    continue
                for child in ast.walk(node):
                    if isinstance(child, ast.Call) and _call_name(child) in disposals:
                        disposed_attributes |= _names_in(child)
                        # `shutil.rmtree(self._staging_dir)` names the released
                        # path through an attribute, not a local.
                        disposed_attributes |= {
                            inner.attr
                            for inner in ast.walk(child)
                            if isinstance(inner, ast.Attribute)
                        }
        for fn in [
            node
            for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
        ]:
            created = [
                child
                for child in ast.walk(fn)
                if isinstance(child, ast.Call) and _call_name(child) in creators
            ]
            for node in created:
                # A `with tempfile.TemporaryDirectory(...)` block releases on exit.
                if any(
                    isinstance(parent, (ast.With, ast.AsyncWith))
                    and any(item is node for item in ast.walk(parent.context_expr))
                    for parent in ast.walk(fn)
                ):
                    continue
                bound: set[str] = set()
                stored: set[str] = set()
                for parent in ast.walk(fn):
                    if isinstance(parent, ast.Assign) and any(
                        child is node for child in ast.walk(parent.value)
                    ):
                        bound |= _assigned_names(parent)
                        stored |= _attr_names(parent)
                if not bound and not stored:
                    findings.append(
                        f"[staging-disposal] {rel}:{node.lineno} {fn.name}() returns an untracked "
                        "temporary directory; there is no owner to release it on success, failure, "
                        "or close"
                    )
                    break
                if stored & disposed_attributes:
                    continue
                released = any(
                    isinstance(child, ast.Call)
                    and _call_name(child) in disposals
                    and (_names_in(child) & bound)
                    for child in ast.walk(tree)
                )
                if released:
                    continue
                findings.append(
                    f"[staging-disposal] {rel}:{node.lineno} {fn.name}() creates a staging "
                    "directory but nothing releases it; a per-submission directory that is never "
                    "removed leaks notebook contents"
                )
                break
    return findings


# --------------------------------------------------------------------------- #
# check 16 (C6): a shared discovery label is stamped with its identity labels
# --------------------------------------------------------------------------- #


def _workload_label_values(root: Path) -> dict[str, str]:
    path = root / _WORKLOAD_KEYS
    if not path.is_file():
        return {}
    text = _read(path)
    values: dict[str, str] = {}
    for name in ("LabelRunID", "LabelWorkloadKind"):
        match = re.search(rf"\b{name}\s*=\s*\"([^\"]+)\"", text)
        if match:
            values[name] = match.group(1)
    return values


def _label_constants(tree: ast.Module, literal: str) -> set[str]:
    """Module-level names bound to a label string, so `CONST` counts as the label.

    The renderer spells the managed-by key through ``TAU_MANAGED_BY_LABEL``; a
    rule that only matched the literal would silently stop matching the moment
    the code named it.  After the fix, the identity keys will likely arrive the
    same way.
    """
    names: set[str] = set()
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        for child in ast.walk(node.value):
            if isinstance(child, ast.Constant) and child.value == literal:
                names |= _assigned_names(node)
    return names


def _mentions(source: str, literal: str, names: set[str]) -> bool:
    if literal in source:
        return True
    return any(re.search(rf"\b{re.escape(name)}\b", source) for name in names)


def check_run_metadata_contract_complete(root: Path) -> list[str]:
    """Flag a workload that stamps the discovery label but not the identity labels.

    ``cli/internal/runhistory`` discovers a run by any ``tau.azure.com/`` label
    (``hasTauLabel``) and then reads its columns from ``run-id`` and
    ``workload-kind``.  Stamping only ``managed-by`` makes the row appear and be
    incomplete, which is worse for a shared consumer than not appearing at all.
    """
    labels = _workload_label_values(root)
    if len(labels) < 2:
        return [
            f"[run-metadata] {_WORKLOAD_KEYS} no longer declares the run identity labels that "
            "the run-history reader consumes"
        ]
    run_id, workload_kind = labels["LabelRunID"], labels["LabelWorkloadKind"]
    findings: list[str] = []
    for path in _production_python(root, _NOTEBOOK_SURFACE):
        text = _read(path)
        if "managed-by" not in text:
            continue
        tree = _parse_python(path)
        if tree is None:
            continue
        rel = _rel(root, path)
        managed_names = _label_constants(tree, "tau.azure.com/managed-by")
        identity_names = _label_constants(tree, run_id) | _label_constants(tree, workload_kind)
        for fn in [
            node
            for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
        ]:
            body = ast.get_source_segment(text, fn) or ""
            if not _mentions(body, "tau.azure.com/managed-by", managed_names):
                continue
            if _mentions(body, run_id, identity_names) and _mentions(
                body, workload_kind, identity_names
            ):
                continue
            findings.append(
                f"[run-metadata] {rel}:{fn.lineno} {fn.name}() stamps the managed-by label that "
                "makes a shared consumer discover the run, but omits "
                f"{run_id!r} and {workload_kind!r}, which that consumer reads to identify it"
            )
    return findings


# --------------------------------------------------------------------------- #
# check 17 (C7): a multi-line command is framed before it reaches a shell
# --------------------------------------------------------------------------- #


def _multiline_producers(tree: ast.Module, text: str) -> dict[str, str]:
    """Top-level functions whose result is a multi-line string, and their source.

    The source matters: framing must be local to the function that builds the
    command.  A `base64` mention anywhere else in the file - a comment, an
    unrelated helper - must not excuse an unframed entrypoint.
    """
    producers: dict[str, str] = {}
    for node in tree.body:
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        for child in ast.walk(node):
            if isinstance(child, ast.Call) and _call_name(child) == "join":
                if any(
                    isinstance(inner, ast.Constant) and inner.value == "\n"
                    for inner in ast.walk(child)
                ):
                    producers[node.name] = ast.get_source_segment(text, node) or ""
                    break
    return producers


def check_entrypoint_shell_framed(root: Path) -> list[str]:
    """Flag a multi-line value placed into entrypoint/command/args unframed.

    KubeRay's submitter interpolates the entrypoint into its own shell command.
    A value containing a newline terminates the ``ray job submit`` line early,
    so the notebook runs in the submitter instead of on the Ray head - and any
    assertion that only compares the Python string still passes.
    """
    findings: list[str] = []
    for path in _production_python(root, _NOTEBOOK_SURFACE):
        tree = _parse_python(path)
        if tree is None:
            continue
        text = _read(path)
        producers = _multiline_producers(tree, text)
        rel = _rel(root, path)
        for node in ast.walk(tree):
            key = ""
            value: Optional[ast.AST] = None
            if isinstance(node, ast.Dict):
                for dict_key, dict_value in zip(node.keys, node.values):
                    if isinstance(dict_key, ast.Constant) and dict_key.value in _SHELL_COMMAND_KEYS:
                        key, value = str(dict_key.value), dict_value
                        break
            elif isinstance(node, ast.Call):
                for keyword in node.keywords:
                    if keyword.arg in _SHELL_COMMAND_KEYS:
                        key, value = keyword.arg, keyword.value
                        break
            if value is None:
                continue
            framed_sources: list[str] = []
            multiline = False
            if isinstance(value, ast.Call) and _call_name(value) in producers:
                multiline = True
                framed_sources.append(producers[_call_name(value)])
            elif isinstance(value, ast.Call) and _call_name(value) == "join":
                multiline = any(
                    isinstance(inner, ast.Constant) and inner.value == "\n"
                    for inner in ast.walk(value)
                )
                framed_sources.append(ast.get_source_segment(text, node) or "")
            elif isinstance(value, ast.Constant) and isinstance(value.value, str):
                multiline = "\n" in value.value
                framed_sources.append(ast.get_source_segment(text, node) or "")
            if not multiline:
                continue
            producer_source = "\n".join(framed_sources)
            if any(marker in producer_source for marker in _SHELL_FRAMING_MARKERS):
                continue
            findings.append(
                f"[shell-framing] {rel}:{getattr(node, 'lineno', 0)} a multi-line value is "
                f"assigned to {key!r} without quoting, heredoc, or base64 framing; the newline "
                "reaches the shell and truncates the command"
            )
    return findings


# --------------------------------------------------------------------------- #
# check 18 (C10): an image the design contract names is built by CI
# --------------------------------------------------------------------------- #

_IMAGE_REFERENCE_RE = re.compile(r"images/([A-Za-z0-9._-]+)/Dockerfile")


def check_images_built_in_ci(root: Path) -> list[str]:
    """Flag a runtime image the design contract names that no workflow builds.

    Scope is deliberately the images a document under ``docs/`` names as part of
    a shipped contract, not every Dockerfile in ``images/``.  ``images/ray`` and
    ``images/gpu-metrics-collector`` are also unbuilt by any workflow, but they
    pre-date this work and are built through documented local flows; making a
    feature branch responsible for them would turn a real in-scope gap into
    repo-wide noise nobody can close here.

    An image the feature depends on at runtime, which CI never builds, leaves
    "the feature can execute" unproven: the Dockerfile can rot, the base tag can
    vanish, and no job notices.
    """
    declared: set[str] = set()
    for doc in sorted((root / "docs").rglob("*.md")):
        declared |= set(_IMAGE_REFERENCE_RE.findall(_read(doc)))
    if not declared:
        return []
    corpus = "\n".join(
        _read(workflow) for workflow in sorted((root / ".github" / "workflows").glob("*.y*ml"))
    ).replace("\\", "/")
    findings: list[str] = []
    for name in sorted(declared):
        if not (root / "images" / name / "Dockerfile").is_file():
            continue
        if f"images/{name}/" in corpus:
            continue
        findings.append(
            f"[image-not-built] docs/ names images/{name}/Dockerfile as part of the shipped "
            "contract but no workflow builds or triggers on it; nothing proves the image the "
            "runtime depends on can be built"
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


def _write_workload_keys(root: Path) -> None:
    """The run identity labels the shared run-history reader consumes."""
    _write(
        root,
        _WORKLOAD_KEYS,
        "package workloadmeta\n\nconst (\n"
        '\tLabelRunID        = "tau.azure.com/run-id"\n'
        '\tLabelWorkloadKind = "tau.azure.com/workload-kind"\n'
        ")\n",
    )


_AUTHZ_DEFECT_SOURCE = '''def build_plan(cluster_doc, namespace, team, lane, profile):
    profiles, skipped = ready_profiles(
        cluster_doc, scope=ProfileScope(namespace=namespace, team=team, lane=lane)
    )
    selected = _select_profile(profiles, profile, skipped=skipped)
    if team is None or lane is None:
        derived_team, derived_lane = resolve_applicability(cluster_doc, selected.name)
        scope = ProfileScope(
            namespace=namespace,
            team=team if team is not None else derived_team,
            lane=lane if lane is not None else derived_lane,
        )
        authorized, _ = ready_profiles(cluster_doc, scope=scope)
        if selected.name not in {candidate.name for candidate in authorized}:
            raise SubmitError("profile does not authorize this scope")
    return selected
'''

_AUTHZ_CLEAN_SOURCE = '''def build_plan(cluster_doc, namespace, team, lane, profile):
    scope = ProfileScope(namespace=namespace, team=team, lane=lane)
    profiles, skipped = ready_profiles(cluster_doc, scope=scope)
    selected = _select_profile(profiles, profile, skipped=skipped, scope=scope)
    authorized, _ = ready_profiles(cluster_doc, scope=trusted_scope(cluster_doc, namespace))
    if selected.name not in {candidate.name for candidate in authorized}:
        raise SubmitError("profile does not authorize this scope")
    return selected
'''


def _fixture_authorization_defect(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_notebook_submit.py", _AUTHZ_DEFECT_SOURCE)


def _fixture_authorization_clean(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_notebook_submit.py", _AUTHZ_CLEAN_SOURCE)


_HANDLER_DEFECT_SOURCE = '''class _ClientMixin:
    AUTH_RESOURCE = "taugrid"

    async def authorize(self, action):
        allowed = self.authorizer.is_authorized(self, self.current_user, action, self.AUTH_RESOURCE)
        if not allowed:
            raise web.HTTPError(403, "not authorized to " + action)

    async def read(self, reader, **kwargs):
        return await reader(**kwargs)


class RunsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        namespace = self.get_argument("namespace", "default")
        self.finish(await self.read(runs.list_runs, namespace=namespace))
'''

_HANDLER_CLEAN_SOURCE = '''class _ClientMixin:
    AUTH_RESOURCE = "taugrid"

    async def authorize(self, action):
        allowed = self.authorizer.is_authorized(self, self.current_user, action, self.AUTH_RESOURCE)
        if not allowed:
            raise web.HTTPError(403, "not authorized to " + action)

    async def read(self, reader, **kwargs):
        await self.authorize("read")
        return await reader(**kwargs)


class RunsHandler(APIHandler, _ClientMixin):
    @web.authenticated
    async def get(self) -> None:
        namespace = self.get_argument("namespace", "default")
        self.finish(await self.read(runs.list_runs, namespace=namespace))
'''


def _fixture_handler_authz_defect(root: Path) -> None:
    _write(root, _JUPYTER_SERVER, _HANDLER_DEFECT_SOURCE)


def _fixture_handler_authz_clean(root: Path) -> None:
    _write(root, _JUPYTER_SERVER, _HANDLER_CLEAN_SOURCE)


_TERMINAL_DEFECT_SOURCE = '''def classify(rayjob):
    status = rayjob.get("status") or {}
    if isinstance(status.get("failed"), int) and status["failed"] > 0:
        return "failed"
    return "running"
'''

_TERMINAL_CLEAN_SOURCE = '''def classify(rayjob):
    status = rayjob.get("status") or {}
    job = str(status.get("jobStatus") or "").upper()
    if job in ("FAILED", "ERROR"):
        return "failed"
    if job == "SUCCEEDED":
        return "complete"
    return "running"
'''


def _fixture_terminal_state_defect(root: Path) -> None:
    _write(root, "sdk/python/python/tau/widgets/status.py", _TERMINAL_DEFECT_SOURCE)


def _fixture_terminal_state_clean(root: Path) -> None:
    _write(root, "sdk/python/python/tau/widgets/status.py", _TERMINAL_CLEAN_SOURCE)


_QUEUE_DEFECT_SOURCE = '''def build_plan(cluster_doc, namespace, queue, profile):
    profiles, skipped = ready_profiles(
        cluster_doc, scope=ProfileScope(namespace=namespace, team=None, lane=None)
    )
    effective_queue = queue or default_queue(cluster_doc) or "default"
    return render_rayjob(queue=effective_queue)
'''

_QUEUE_CLEAN_SOURCE = '''def build_plan(cluster_doc, namespace, queue, profile):
    profiles, skipped = ready_profiles(
        cluster_doc, scope=ProfileScope(namespace=namespace, team=None, lane=None)
    )
    selected = _select_profile(profiles, profile, skipped=skipped)
    resolved = _resolved_profile(cluster_doc, selected.name)
    effective_queue = queue or profile_queue(
        resolved, namespace, default_queue(cluster_doc) or "default"
    )
    return render_rayjob(queue=effective_queue)
'''


def _fixture_profile_queue_defect(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_notebook_submit.py", _QUEUE_DEFECT_SOURCE)


def _fixture_profile_queue_clean(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_notebook_submit.py", _QUEUE_CLEAN_SOURCE)


_STAGING_DEFECT_SOURCE = '''class Panel:
    def _default_staging_dir(self):
        import tempfile

        return Path(tempfile.mkdtemp(prefix="tau-notebook-"))

    def close(self) -> None:
        self.stop_watching()
'''

_STAGING_CLEAN_SOURCE = '''class Panel:
    def submit(self):
        import tempfile

        self._staging_dir = Path(tempfile.mkdtemp(prefix="tau-notebook-"))

    def close(self) -> None:
        self.stop_watching()
        if getattr(self, "_staging_dir", None):
            shutil.rmtree(self._staging_dir, ignore_errors=True)
'''


def _fixture_staging_defect(root: Path) -> None:
    _write(root, "sdk/python/python/tau/widgets/panel.py", _STAGING_DEFECT_SOURCE)


def _fixture_staging_clean(root: Path) -> None:
    _write(root, "sdk/python/python/tau/widgets/panel.py", _STAGING_CLEAN_SOURCE)


_METADATA_DEFECT_SOURCE = '''def render_rayjob(name, queue, run_id):
    metadata = {
        "name": name,
        "labels": {
            "tau.azure.com/queue": queue,
            "tau.azure.com/managed-by": "taugrid",
        },
    }
    return {"metadata": metadata}
'''

_METADATA_CLEAN_SOURCE = '''def render_rayjob(name, queue, run_id, workload_kind):
    metadata = {
        "name": name,
        "labels": {
            "tau.azure.com/queue": queue,
            "tau.azure.com/managed-by": "taugrid",
            "tau.azure.com/run-id": run_id,
            "tau.azure.com/workload-kind": workload_kind,
        },
    }
    return {"metadata": metadata}
'''


def _fixture_run_metadata_defect(root: Path) -> None:
    _write_workload_keys(root)
    _write(root, "sdk/python/python/tau/_render.py", _METADATA_DEFECT_SOURCE)


def _fixture_run_metadata_clean(root: Path) -> None:
    _write_workload_keys(root)
    _write(root, "sdk/python/python/tau/_render.py", _METADATA_CLEAN_SOURCE)


_FRAMING_DEFECT_SOURCE = '''def _entrypoint(base, pip):
    lines = ["set -eu"]
    lines.append(base)
    return "\\n".join(lines) + "\\n"


def render_rayjob(name, entrypoint):
    return {"spec": {"entrypoint": _entrypoint(entrypoint, [])}}
'''

_FRAMING_CLEAN_SOURCE = '''def _entrypoint(base, pip):
    lines = ["set -eu"]
    lines.append(base)
    payload = "\\n".join(lines) + "\\n"
    return "printf %s " + shlex.quote(payload)


def render_rayjob(name, entrypoint):
    return {"spec": {"entrypoint": _entrypoint(entrypoint, [])}}
'''


def _fixture_shell_framing_defect(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_render.py", _FRAMING_DEFECT_SOURCE)


def _fixture_shell_framing_clean(root: Path) -> None:
    _write(root, "sdk/python/python/tau/_render.py", _FRAMING_CLEAN_SOURCE)


def _fixture_image_defect(root: Path) -> None:
    _write(root, "images/widget/Dockerfile", "FROM scratch\n")
    _write(
        root,
        "docs/design/widget.md",
        "The widget runtime is built from `images/widget/Dockerfile`.\n",
    )
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  images:\n    steps:\n      - run: docker build images/other\n",
    )


def _fixture_image_clean(root: Path) -> None:
    _write(root, "images/widget/Dockerfile", "FROM scratch\n")
    _write(
        root,
        "docs/design/widget.md",
        "The widget runtime is built from `images/widget/Dockerfile`.\n",
    )
    _write(
        root,
        ".github/workflows/ci.yml",
        "jobs:\n  images:\n    steps:\n"
        "      - uses: docker/build-push-action@v6\n"
        "        with:\n"
        "          dockerfile: images/widget/Dockerfile\n",
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
        "Every emitted KubeRay submitterConfig pins backoffLimit to 0. Scope is a *wrong* pin, "
        "not an omitted submitterConfig: the CLI renders HTTPMode jobs, which have no submitter "
        "Job for submitterConfig to govern.",
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
    Check(
        "authorization-input-provenance",
        "C1",
        "An authorization input is never derived from the object being authorized.",
        check_authorization_input_provenance,
        _fixture_authorization_defect,
        _fixture_authorization_clean,
    ),
    Check(
        "handler-authorization-required",
        "C2",
        "Every cluster-state Jupyter handler carries a permission check, not just @web.authenticated.",
        check_handler_authorization_required,
        _fixture_handler_authz_defect,
        _fixture_handler_authz_clean,
    ),
    Check(
        "terminal-state-from-current-state",
        "C3",
        "A terminal state is decided from the current status, not from a count of past attempts.",
        check_terminal_state_from_current_state,
        _fixture_terminal_state_defect,
        _fixture_terminal_state_clean,
    ),
    Check(
        "resolved-value-not-discarded",
        "C4",
        "A renderer that resolves a profile renders that profile's resolved queue, not a fallback.",
        check_resolved_value_not_discarded,
        _fixture_profile_queue_defect,
        _fixture_profile_queue_clean,
    ),
    Check(
        "staging-resource-disposal",
        "C5",
        "A per-submission staging directory is tracked and released on success, failure, and close.",
        check_staging_resource_disposal,
        _fixture_staging_defect,
        _fixture_staging_clean,
    ),
    Check(
        "run-metadata-contract-complete",
        "C6",
        "A workload that stamps managed-by also stamps the run-id and workload-kind its consumers read.",
        check_run_metadata_contract_complete,
        _fixture_run_metadata_defect,
        _fixture_run_metadata_clean,
    ),
    Check(
        "entrypoint-shell-framed",
        "C7",
        "A multi-line entrypoint/command is quoted, heredoc'd, or base64-framed before it reaches a shell.",
        check_entrypoint_shell_framed,
        _fixture_shell_framing_defect,
        _fixture_shell_framing_clean,
    ),
    Check(
        "images-built-in-ci",
        "C10",
        "Every images/*/Dockerfile is built or triggered by at least one workflow.",
        check_images_built_in_ci,
        _fixture_image_defect,
        _fixture_image_clean,
    ),
)


#: Checks whose underlying defect is still present in this tree, so the check is
#: documenting it rather than blocking it.  An entry here is a ratchet held open
#: on purpose; it is NOT a weaker check - the check still fails and still prints
#: every finding.  Delete the entry the moment the defect is fixed, and the check
#: becomes a build failure again.
#:
#: Currently empty: every check is a hard failure.  It existed while the PR #300
#: notebook review round was mid-flight, when the fixes and this harness landed in
#: the same tree; each entry was removed as its fix arrived.  Keep it empty unless
#: a check knowingly lands ahead of its fix, and name the fix's owner and why it
#: cannot land now.
EXPECTED_FAILURES: dict[str, str] = {}


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
    expected: list[str] = []
    stale: list[str] = []
    print_scope_report(root)
    for check in CHECKS:
        findings = check.run(root)
        if findings and check.name in EXPECTED_FAILURES:
            expected.append(check.name)
            print(f"NOTE {check.name}  (catches defects {check.defects}) - documented, not blocking")
            print(f"     still open because: {EXPECTED_FAILURES[check.name]}")
            for finding in findings:
                print(f"  - {finding}")
        elif findings:
            print(f"FAIL {check.name}  (catches defects {check.defects})")
            for finding in findings:
                print(f"  - {finding}")
            failures.extend(findings)
        else:
            if check.name in EXPECTED_FAILURES:
                stale.append(check.name)
            print(f"ok   {check.name}  (catches defects {check.defects})")
    if stale:
        print(
            "\nHarness hygiene: these checks now pass but are still listed in EXPECTED_FAILURES. "
            "Delete the entries so they become build failures again:\n  - "
            + "\n  - ".join(stale),
            file=sys.stderr,
        )
    if failures:
        print(f"\n{len(failures)} review-pattern finding(s); see above.", file=sys.stderr)
        return 1
    if expected:
        print(
            f"\n{len(CHECKS)} review-pattern checks ran; {len(expected)} documented defect(s) still "
            f"open ({', '.join(expected)}).",
            file=sys.stderr,
        )
    else:
        print(f"\nAll {len(CHECKS)} review-pattern checks passed.")
    return 0


# --------------------------------------------------------------------------- #
# advisory: blast radius of the change under review
# --------------------------------------------------------------------------- #

#: Subsystems a notebook-plugin change has no reason to touch.  Rides the same
#: diff means the reviewer must justify unrelated work, not that the change is
#: wrong: a toolchain bump or a repo-wide policy change can be legitimate.
_UNRELATED_SUBSYSTEMS = (
    "go.mod",
    "go.sum",
    "Chart.lock",
    ".github/workflows/",
    "AGENTS.md",
    "Makefile",
    "DEVELOPMENT.md",
)


def scope_report(root: Path) -> list[str]:
    """List paths in the change that share no subsystem with it.

    This is a *report*, never a failure.  Its job is to make the reviewer ask
    why four unrelated requests are riding one feature branch; a structural
    check cannot tell a justified cross-cutting change from scope creep.
    """
    base = _run(["git", "merge-base", "HEAD", "origin/main"], cwd=root)
    if base.returncode != 0 or not base.stdout.strip():
        return []
    diff = _run(["git", "diff", "--name-only", f"{base.stdout.strip()}..HEAD"], cwd=root)
    if diff.returncode != 0:
        return []
    changed = [line for line in diff.stdout.splitlines() if line.strip()]
    if not any(path.startswith("sdk/python/python/tau/") for path in changed):
        return []
    unrelated = [
        path for path in changed if any(marker in path for marker in _UNRELATED_SUBSYSTEMS)
    ]
    if not unrelated:
        return []
    return [
        "[blast-radius] the change adds notebook-plugin SDK code and also modifies "
        f"{len(unrelated)} unrelated path(s) - " + ", ".join(sorted(unrelated)[:8])
    ]


def print_scope_report(root: Path) -> None:
    for note in scope_report(root):
        print(f"NOTE {note}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="prove each check fails on its defect fixture")
    parser.add_argument("--list", action="store_true", help="list checks and the defects they catch")
    parser.add_argument("--scope-report", action="store_true", help="report blast radius, then exit")
    parser.add_argument("--root", type=Path, default=REPO_ROOT, help="repository root to check")
    args = parser.parse_args(argv)

    if args.scope_report:
        for note in scope_report(args.root.resolve()):
            print(note)
        return 0
    if args.list:
        for check in CHECKS:
            marker = " (documented open defect)" if check.name in EXPECTED_FAILURES else ""
            print(f"{check.name:32} defects {check.defects:10} {check.summary}{marker}")
        return 0
    if args.self_test:
        return run_self_test()
    return run_checks(args.root.resolve())


if __name__ == "__main__":
    raise SystemExit(main())
