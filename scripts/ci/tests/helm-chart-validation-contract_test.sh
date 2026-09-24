#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORKFLOW="${REPO_ROOT}/.github/workflows/ci-helm-charts.yml"

grep -Fq 'namespace_args=(--namespace kueue-system)' "$WORKFLOW" ||
  {
    echo "TauGrid chart validation must render in kueue-system for KEC" >&2
    exit 1
  }

grep -Fq 'helm lint "${{ matrix.chart }}" "${namespace_args[@]}"' "$WORKFLOW" ||
  {
    echo "Helm lint must use the chart-specific namespace arguments" >&2
    exit 1
  }

grep -Fq 'helm template test "${{ matrix.chart }}" "${namespace_args[@]}"' "$WORKFLOW" ||
  {
    echo "Helm template validation must use the chart-specific namespace arguments" >&2
    exit 1
  }

echo "Helm chart validation workflow contracts passed"
