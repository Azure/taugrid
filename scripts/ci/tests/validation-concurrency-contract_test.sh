#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT

fail() {
  echo "validation concurrency contract test failed: $*" >&2
  exit 1
}

while read -r workflow prefix; do
  path="${REPO_ROOT}/.github/workflows/${workflow}"
  expected_group="group: ${prefix}-\${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}"
  expected_cancel="cancel-in-progress: \${{ github.event_name == 'pull_request' }}"

  grep -Fq "${expected_group}" "${path}" ||
    fail "${workflow} must group pull requests by number and non-pull-request runs by run ID"
  grep -Fq "${expected_cancel}" "${path}" ||
    fail "${workflow} must cancel only superseded pull request runs"
done <<'WORKFLOWS'
taugrid-validation.yml taugrid-validation
taugrid-docs-validation.yml taugrid-docs-validation
taugrid-kubernetes-validation.yml taugrid-kubernetes-validation
taugrid-sdk-validation.yml taugrid-sdk-validation
taugrid-image-validation.yml taugrid-image-validation
taugrid-terraform-validation.yml taugrid-terraform-validation
WORKFLOWS

if grep -R -Fq 'github.event.pull_request.number || github.ref' "${REPO_ROOT}/.github/workflows"; then
  fail "validation workflows must not share a branch-wide concurrency group"
fi

grep -Fq 'bash scripts/ci/tests/validation-concurrency-contract_test.sh' "${REPO_ROOT}/Makefile" ||
  fail "make test must include validation concurrency contracts"
grep -Fq 'bash scripts/ci/tests/validation-concurrency-contract_test.sh' \
  "${REPO_ROOT}/.github/workflows/taugrid-validation.yml" ||
  fail "core validation must run validation concurrency contracts"

echo "Validation concurrency contract tests passed"
