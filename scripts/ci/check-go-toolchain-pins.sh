#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly TOOLCHAIN_VERSION="1.27.3"
readonly MODULE_VERSION="1.27.2"
readonly EXPECTED_MICROSOFT_GO_COMMIT="9f24672a32abfcdd8266343e9ea1f1f1cbe3aaf6"

fail() {
  echo "Go toolchain pin check failed: $*" >&2
  exit 1
}

[[ "$(tr -d '[:space:]' <"${REPO_ROOT}/.go-version")" == "${TOOLCHAIN_VERSION}" ]] ||
  fail ".go-version must be ${TOOLCHAIN_VERSION}"

readonly MODULE_FILES=(
  "cli/go.mod"
  "controllers/tau-core/go.mod"
  "core/go.mod"
  "metrics/experiment-metrics-collector/go.mod"
  "monitoring/gpu-health-checker/go.mod"
  "monitoring/gpu-metrics-collector/go.mod"
  "portal/go.mod"
  "site/go.mod"
  "tests/e2e/go.mod"
)

for relative_path in "${MODULE_FILES[@]}"; do
  grep -Fxq "go ${MODULE_VERSION}" "${REPO_ROOT}/${relative_path}" ||
    fail "${relative_path} must require Go ${MODULE_VERSION}"
done

readonly TOOLCHAIN_PIN_FILES=(
  ".ado/tau-aks-e2e.yml"
  ".ado/tau-cli-e2e.yml"
  ".ado/tau-core-e2e.yml"
  ".pipelines/taugrid-flex-nightly.yml"
  ".pipelines/taugrid-unbounded-stable-nightly.yml"
  "AGENTS.md"
  "images/adx-mon/Dockerfile.alerter"
  "images/adx-mon/Dockerfile.collector"
  "images/adx-mon/Dockerfile.ingestor"
  "images/adx-mon/Dockerfile.operator"
  "images/gpu-metrics-collector/Dockerfile"
  "images/tau-core-controller/Dockerfile"
  "images/tau/Dockerfile"
  "images/taugrid-metrics-collector/Dockerfile"
  "images/taugrid-portal/Dockerfile"
  "tests/e2e/README.md"
)

for relative_path in "${TOOLCHAIN_PIN_FILES[@]}"; do
  grep -Fq "${TOOLCHAIN_VERSION}" "${REPO_ROOT}/${relative_path}" ||
    fail "${relative_path} does not pin Go ${TOOLCHAIN_VERSION}"
done

stale_repository_pins="$(
  git -C "${REPO_ROOT}" grep -nE '(go)?1\.26\.(8|9|10)|1\.27\.(0|1)([^0-9]|$)' -- \
    . ':(exclude)cli/releases/v0.4.4.md' || true
)"
[[ -z "${stale_repository_pins}" ]] ||
  fail "repository contains unsupported Go toolchain pins: ${stale_repository_pins}"

grep -Fq "ARG MICROSOFT_GO_COMMIT=${EXPECTED_MICROSOFT_GO_COMMIT}" \
  "${REPO_ROOT}/images/tau-core-controller/Dockerfile" ||
  fail "images/tau-core-controller/Dockerfile has an unexpected Microsoft Go source commit"
grep -Fq "go-version-file: .go-version" \
  "${REPO_ROOT}/.github/workflows/taugrid-validation.yml" ||
  fail "TauGrid validation must use the repository Go toolchain"

echo "Go toolchain pins are consistent: toolchain ${TOOLCHAIN_VERSION}, modules ${MODULE_VERSION}"
