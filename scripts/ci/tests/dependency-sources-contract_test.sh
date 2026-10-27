#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT

python3 - "${REPO_ROOT}" <<'PY'
import fnmatch
from pathlib import Path
import re
import sys

root = Path(sys.argv[1])
config = (root / ".github/dependabot.yml").read_text()
helm = re.search(
    r"(?ms)^  - package-ecosystem: helm\n(.*?)(?=^  - package-ecosystem:|\Z)",
    config,
)
assert helm is not None, "Helm chart updates must remain configured"
excluded = re.search(
    r"(?ms)^    exclude-paths:\n((?:^      - .*\n)+)", helm.group(1)
)
assert excluded is not None, "Helm image discovery must exclude values files"
patterns = re.findall(r'^      - "([^"]+)"$', excluded.group(1), re.MULTILINE)
directories = re.findall(r'^      - "(/charts/[^"]+)"$', helm.group(1), re.MULTILINE)
assert directories, "Helm chart update directories must remain configured"
for directory in directories:
    for name in ("values.yaml", "values.yml", "Chart.yaml", "Chart.lock"):
        path = f"{directory.lstrip('/')}/{name}"
        ignored = any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns)
        assert ignored == name.startswith("values."), (
            f"{path}: values files must be excluded without excluding chart dependencies"
        )
    assert (root / directory.lstrip("/") / "Chart.yaml").is_file(), directory
assert "  - package-ecosystem: docker\n" in config, "Docker base-image updates must remain enabled"
print("Dependency source contract tests passed")
PY
