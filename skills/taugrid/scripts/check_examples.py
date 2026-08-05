#!/usr/bin/env python3
"""Validate every runnable YAML example in the taugrid skill against the
installed `tau` binary.

Why this exists: a worked example that does not validate is worse than no
example, because it gets copy-pasted and fails on first use. During the skill's
first eval round exactly that happened -- a multi-node torchrun example paired
`engine: job` with `compute.gpus_per_worker`, which the CLI rejects. Prose can
drift from the implementation silently; `tau run validate` cannot.

Run from the repository root:

    python3 skills/taugrid/scripts/check_examples.py

Exit code 0 = every example validates. Non-zero = at least one is broken, with
the CLI's own error printed.
"""

from __future__ import annotations

import os
import pathlib
import re
import subprocess
import sys
import tempfile

# Placeholders that are intentionally not real values. Substitute something
# schema-valid so validation exercises structure rather than tripping on a
# deliberate blank.
PLACEHOLDERS = {
    "<pinned-image>": "example.invalid/img:1",
    "<preset>": "azure.research.training.l",
}

SKILL_ROOT = pathlib.Path(__file__).resolve().parents[1]
TARGETS = [
    SKILL_ROOT / "SKILL.md",
    SKILL_ROOT / "references" / "run-config.md",
    SKILL_ROOT / "references" / "troubleshooting.md",
    SKILL_ROOT / "references" / "platform.md",
]


def is_run_config(block: str) -> bool:
    """A full run config, not a fragment or a different schema.

    Fragments (a bare `resilience:` stanza) and the bootstrap manifest are
    documented deliberately and are not `tau run validate` inputs.
    """
    if "schema: tau.workspace.bootstrap" in block:
        return False
    if "schema: tau.projects" in block:
        return False
    return re.search(r"^name:", block, re.M) is not None


def main() -> int:
    if subprocess.run(["which", "tau"], capture_output=True).returncode != 0:
        print("tau not on PATH; run `make install-tau-cli` first", file=sys.stderr)
        return 2

    checked = failures = 0
    for target in TARGETS:
        if not target.exists():
            continue
        text = target.read_text()
        rel = target.relative_to(SKILL_ROOT.parent)
        for index, block in enumerate(re.findall(r"```yaml\n(.*?)```", text, re.S)):
            if not is_run_config(block):
                continue
            body = block
            for needle, value in PLACEHOLDERS.items():
                body = body.replace(needle, value)

            with tempfile.NamedTemporaryFile(
                "w", suffix=".yaml", delete=False, dir="/tmp"
            ) as handle:
                handle.write(body)
                path = handle.name
            try:
                result = subprocess.run(
                    ["tau", "run", "validate", "--config", path],
                    capture_output=True,
                    text=True,
                )
            finally:
                os.unlink(path)

            checked += 1
            name = re.search(r"^name:\s*(\S+)", body, re.M)
            label = name.group(1) if name else f"block#{index}"
            if result.returncode == 0:
                print(f"OK   {rel} :: {label}")
            else:
                failures += 1
                detail = (result.stdout + result.stderr).strip().splitlines()
                print(f"FAIL {rel} :: {label}")
                if detail:
                    print(f"     {detail[0]}")

    print(f"\nchecked={checked} failures={failures}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
