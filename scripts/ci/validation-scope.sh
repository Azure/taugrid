#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

suite="${1:?expected image, docs, or kubernetes}"
paths=(
  scripts/ci/validation-scope.sh
  scripts/ci/tests/required-gates-contract_test.sh
)
case "$suite" in
  image)
    paths+=(
      .go-version
      .dockerignore
      'cli/**'
      'controllers/tau-core/**'
      'core/**'
      'images/tau/**'
      'images/tau-core-controller/**'
      'images/taugrid-portal/**'
      'images/taugrid-metrics-collector/**'
      'images/gpu-metrics-collector/**'
      'images/adx-mon/**'
      'metrics/experiment-metrics-collector/**'
      'monitoring/gpu-health-checker/**'
      'monitoring/gpu-metrics-collector/**'
      'portal/**'
      site/go.mod
      tests/e2e/go.mod
      scripts/ci/tests/image-build-contract_test.sh
      scripts/ci/tests/go-runtime-default-contract_test.sh
      scripts/ci/validate-image-build-contract.sh
      scripts/lib/image-specs.sh
      .github/workflows/taugrid-image-validation.yml
    )
    ;;
  docs)
    paths+=(
      '**/*.md'
      'site/**'
      .github/workflows/taugrid-docs-validation.yml
    )
    ;;
  kubernetes)
    paths+=(
      .github/workflows/taugrid-kubernetes-validation.yml
      'charts/tau-core-controller/**'
      'charts/taugrid/**'
      'charts/taugrid-core/**'
      'cli/**'
      'controllers/tau-core/**'
      'core/**'
      'examples/**'
      'images/tau-core-controller/**'
      scripts/ci/vendor-taugrid-dependencies.sh
    )
    ;;
  *)
    echo "Unknown validation suite: $suite" >&2
    exit 1
    ;;
esac

enabled=true
case "${GITHUB_EVENT_NAME:?}" in
  workflow_dispatch | schedule)
    ;;
  pull_request | push)
    head="${HEAD_SHA:?}"
    git cat-file -e "${head}^{commit}"
    base="${BASE_SHA:-}"
    if [[ -n "$base" ]] && git cat-file -e "${base}^{commit}" 2>/dev/null; then
      if [[ "$GITHUB_EVENT_NAME" == pull_request ]]; then
        base="$(git merge-base "$base" "$head")"
      fi
      pathspecs=()
      for path in "${paths[@]}"; do
        pathspecs+=(":(top,glob)$path")
      done
      if git diff --quiet --no-ext-diff --no-renames "$base" "$head" -- "${pathspecs[@]}"; then
        enabled=false
      else
        result=$?
        if [[ "$result" != 1 ]]; then
          exit "$result"
        fi
      fi
    fi
    ;;
  *)
    echo "Unsupported validation event: $GITHUB_EVENT_NAME" >&2
    exit 1
    ;;
esac

echo "enabled=$enabled" >> "${GITHUB_OUTPUT:?}"
