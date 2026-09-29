# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Package a notebook plus a thin runner into the CLI's embedded payload.

The notebook-as-job decision means the cluster executes the current notebook. We
stage the .ipynb bytes next to a small runner script and embed both in the
RayJob with the same tau-payload envelope the CLI uses, so the run stays
self-contained and needs no object-store upload, ConfigMap, or PVC.

Notebook-specific steps, in order:

1. validate the saved notebook is a v4 document with a Python kernel;
2. strip code outputs and execution counts, then drop approved launcher cells;
3. measure and encode only the prepared payload against the CLI transport caps.

The original file is never modified. A local input-read guard bounds parsing;
it is not a transport budget.
"""

from __future__ import annotations

import ast
import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Tuple

from tau._payload import EncodedPayload, PayloadTooLarge, encode

#: Local parse guard on the raw saved notebook (not a transport budget).
MAX_INPUT_BYTES = 10 * 1024 * 1024

RUNNER_NAME = "_tau_runner.py"
NOTEBOOK_NAME = "analysis.ipynb"
CONTEXT_NAME = "_tau_notebook_context.json"



class NotebookInvalid(Exception):
    """The saved notebook is not a valid, executable v4 notebook."""


@dataclass(frozen=True)
class StagedPayload:
    """A self-contained staged run payload written under staging_dir."""

    notebook_path: Path | None
    runner_path: Path | None
    entrypoint: str
    byte_count: int
    prepared_bytes: int = 0
    files: Dict[str, bytes] = field(default_factory=dict)
    encoding: EncodedPayload = None  # type: ignore[assignment]
    dropped_cells: List[str] = field(default_factory=list)
    included_files: List[str] = field(default_factory=list)


def validate_notebook(notebook_bytes: bytes) -> dict:
    """Return the parsed notebook, or raise NotebookInvalid.

    Requires nbformat 4 with a cells list and a Python kernelspec/language so the
    submitted run has a defined executor contract.
    """
    try:
        notebook = json.loads(notebook_bytes.decode("utf-8"))
    except Exception as exc:
        raise NotebookInvalid(f"saved notebook is not valid JSON: {exc}") from exc
    if not isinstance(notebook, dict) or notebook.get("nbformat") != 4:
        raise NotebookInvalid("saved notebook must be an nbformat 4 document")
    if not isinstance(notebook.get("cells"), list):
        raise NotebookInvalid("saved notebook must contain a cells list")
    metadata = notebook.get("metadata", {})
    if not isinstance(metadata, dict):
        raise NotebookInvalid("notebook metadata must be an object")
    for key in ("kernelspec", "language_info"):
        if key in metadata and not isinstance(metadata[key], dict):
            raise NotebookInvalid(f"metadata.{key} must be an object")
    for cell in notebook["cells"]:
        if not isinstance(cell, dict) or cell.get("cell_type") not in ("code", "markdown", "raw"):
            raise NotebookInvalid("notebook contains an invalid cell")
        source = cell.get("source", "")
        if not isinstance(source, str) and not (isinstance(source, list) and all(isinstance(line, str) for line in source)):
            raise NotebookInvalid("cell source must be text or a list of text lines")
        cell_metadata = cell.get("metadata", {})
        if not isinstance(cell_metadata, dict) or not isinstance(cell_metadata.get("tau", {}), dict):
            raise NotebookInvalid("cell metadata and metadata.tau must be objects")
    kernelspec = metadata.get("kernelspec") or {}
    language = (metadata.get("language_info") or {}).get("name")
    if kernelspec.get("language") != "python" and language != "python":
        raise NotebookInvalid("saved notebook must declare a Python kernel (kernelspec.language or language_info.name)")
    if not any(isinstance(c, dict) and c.get("cell_type") == "code" for c in notebook["cells"]):
        raise NotebookInvalid("saved notebook contains no code cells to execute")
    return notebook


def _cell_source(cell: dict) -> str:
    source = cell.get("source")
    return "".join(source) if isinstance(source, list) else str(source or "")


def classify_launcher_cell(cell: dict) -> bool:
    """True when a code cell is an approved whole-cell launcher.

    Explicit metadata wins. Otherwise the cell must be launcher-only: every
    non-blank, non-comment line is a recognized launcher statement. A cell that
    merely mentions tau.widgets in ordinary code is never a launcher.
    """
    if (cell.get("metadata") or {}).get("tau", {}).get("launcher") is True:
        return True
    meaningful = [line for line in _cell_source(cell).splitlines()
                  if line.strip() and not line.strip().startswith("#")]
    if not meaningful:
        return False
    magic = re.compile(r"^%(?:(?:load|reload)_ext\s+tau\.widgets\.ipython|(?:taugrid|tau)(?:\s.*)?)$")
    python = [line for line in meaningful if not magic.fullmatch(line.strip())]
    try:
        statements = ast.parse("\n".join(python)).body
    except SyntaxError:
        return False
    modules = {"tau.widgets"}
    functions = set()
    for statement in statements:
        if isinstance(statement, ast.Import):
            for alias in statement.names:
                if alias.name != "tau.widgets":
                    return False
                modules.add(alias.asname or alias.name)
        elif isinstance(statement, ast.ImportFrom) and statement.module == "tau.widgets" and statement.level == 0:
            for alias in statement.names:
                if alias.name != "panel":
                    return False
                functions.add(alias.asname or alias.name)
        elif isinstance(statement, ast.Expr) and isinstance(statement.value, ast.Call):
            call = statement.value
            if ast.unparse(call.func) not in functions | {module + ".panel" for module in modules}:
                return False
            try:
                for value in [*call.args, *(keyword.value for keyword in call.keywords)]:
                    ast.literal_eval(value)
            except (ValueError, TypeError):
                return False
        else:
            return False
    return True


def prepare_notebook(notebook_bytes: bytes) -> Tuple[bytes, List[str]]:
    """Validate, strip outputs, and drop approved launcher cells."""
    notebook = validate_notebook(notebook_bytes)
    dropped: List[str] = []
    kept: List[dict] = []
    for index, cell in enumerate(notebook["cells"]):
        if not isinstance(cell, dict):
            continue
        if cell.get("cell_type") == "code":
            if classify_launcher_cell(cell):
                dropped.append(str(cell.get("id") or index))
                continue
            cell["outputs"] = []
            cell["execution_count"] = None
        kept.append(cell)
    notebook["cells"] = kept
    if not any(cell.get("cell_type") == "code" for cell in kept):
        raise NotebookInvalid("every code cell was a launcher; nothing would execute on the cluster")
    return json.dumps(notebook, separators=(",", ":"), ensure_ascii=False).encode("utf-8"), dropped


def _runner_script() -> str:
    return (
        "#!/usr/bin/env python3\n"
        "import shutil\n"
        "import sys\n"
        "from pathlib import Path\n"
        "RESERVED = {'analysis.ipynb', '_tau_runner.py', '_tau_notebook_context.json'}\n"
        "def main() -> int:\n"
        "    here = Path(__file__).resolve().parent\n"
        "    work = Path('/data')\n"
        "    name = 'analysis.ipynb'\n"
        "    argv = sys.argv[1:]\n"
        "    if '--notebook' in argv:\n"
        "        name = argv[argv.index('--notebook') + 1]\n"
        "    nb = here / name\n"
        "    print(f'tg-runner: executing {nb.name} from {here}', flush=True)\n"
        "    try:\n"
        "        import nbconvert\n"
        "        import nbformat\n"
        "    except ImportError:\n"
        "        print('tg-runner: nbconvert not installed in the runtime image', file=sys.stderr, flush=True)\n"
        "        return 3\n"
        "    # Files the researcher chose to ship land beside the notebook so that\n"
        "    # relative imports and file reads resolve while it runs. The notebook\n"
        "    # itself stays in /script and is executed with /data as its directory.\n"
        "    work.mkdir(parents=True, exist_ok=True)\n"
        "    staged = []\n"
        "    for item in sorted(here.iterdir()):\n"
        "        if item.is_file() and item.name not in RESERVED and not item.name.startswith('.'):\n"
        "            shutil.copyfile(item, work / item.name)\n"
        "            staged.append(item.name)\n"
        "    if staged:\n"
        "        joined = ', '.join(staged)\n"
        "        print(f'tg-runner: staged {len(staged)} file(s) into {work}: {joined}', flush=True)\n"
        "    raw = nbformat.read(nb, as_version=4)\n"
        "    class StreamingExecutor(nbconvert.preprocessors.ExecutePreprocessor):\n"
        "        def process_message(self, msg, cell, cell_index):\n"
        "            if msg.get('msg_type') == 'stream':\n"
        "                content = msg.get('content', {})\n"
        "                stream = sys.stderr if content.get('name') == 'stderr' else sys.stdout\n"
        "                stream.write(content.get('text', ''))\n"
        "                stream.flush()\n"
        "            return super().process_message(msg, cell, cell_index)\n"
        "    ep = StreamingExecutor(timeout=600)\n"
        "    ep.preprocess(raw, {'metadata': {'path': str(work)}})\n"
        "    out = work / 'analysis.executed.ipynb'\n"
        "    nbformat.write(raw, out, version=4)\n"
        "    print(f'tg-runner: wrote {out}', flush=True)\n"
        "    return 0\n"
        "if __name__ == '__main__':\n"
        "    raise SystemExit(main())\n"
    )


def _context_manifest(notebook_bytes: bytes, prepared: bytes, dropped: List[str]) -> bytes:
    """Versioned audit context: digests and excluded cell ids only, no values."""
    import hashlib

    manifest = {
        "schema": "tau.notebook.context/v1",
        "runner_version": 3,
        "source_sha256": hashlib.sha256(notebook_bytes).hexdigest(),
        "prepared_sha256": hashlib.sha256(prepared).hexdigest(),
        "excluded_cell_ids": dropped,
    }
    return json.dumps(manifest, separators=(",", ":"), sort_keys=True).encode("utf-8")


def package(
    notebook_bytes: bytes,
    *,
    staging_dir: Path | None = None,
    input_cap: int = MAX_INPUT_BYTES,
    extra_files: Dict[str, bytes] | None = None,
) -> StagedPayload:
    """Stage the prepared notebook plus runner and encode the embedded payload.

    extra_files are the files the researcher chose to ship beside the notebook.
    They ride the same envelope, so the transport caps apply to them too: encode
    raises PayloadTooLarge with the byte math when the selection does not fit.

    Raises NotebookInvalid for a malformed notebook and PayloadTooLarge when the
    prepared payload exceeds the CLI transport caps.
    """
    if len(notebook_bytes) > input_cap:
        raise PayloadTooLarge(
            f"saved notebook is {len(notebook_bytes)} bytes, above the {input_cap}-byte local "
            "parse guard; reduce the file before submitting"
        )

    prepared, dropped = prepare_notebook(notebook_bytes)
    runner_bytes = _runner_script().encode("utf-8")
    manifest = _context_manifest(notebook_bytes, prepared, dropped)
    files = {NOTEBOOK_NAME: prepared, RUNNER_NAME: runner_bytes, CONTEXT_NAME: manifest}
    included: List[str] = []
    for name, data in (extra_files or {}).items():
        if name in files:
            raise NotebookInvalid(f"{name!r} collides with a generated payload file; rename it or deselect it")
        files[name] = data
        included.append(name)
    encoding = encode(files)

    nb_path = runner_path = None
    if staging_dir is not None:
        staging_dir.mkdir(parents=True, exist_ok=True)
        nb_path = staging_dir / NOTEBOOK_NAME
        nb_path.write_bytes(prepared)
        runner_path = staging_dir / RUNNER_NAME
        runner_path.write_bytes(runner_bytes)
        for name, data in (extra_files or {}).items():
            (staging_dir / name).write_bytes(data)

    entrypoint = f"python3 /script/{RUNNER_NAME} --notebook {NOTEBOOK_NAME}"
    return StagedPayload(
        notebook_path=nb_path,
        runner_path=runner_path,
        entrypoint=entrypoint,
        byte_count=len(notebook_bytes),
        prepared_bytes=len(prepared),
        files=files,
        encoding=encoding,
        dropped_cells=dropped,
        included_files=sorted(included),
    )


__all__ = [
    "StagedPayload",
    "PayloadTooLarge",
    "NotebookInvalid",
    "package",
    "prepare_notebook",
    "validate_notebook",
    "classify_launcher_cell",
    "MAX_INPUT_BYTES",
    "RUNNER_NAME",
    "NOTEBOOK_NAME",
    "CONTEXT_NAME",
]
