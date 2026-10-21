#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SHARED_VERSION="$(tr -d '[:space:]' <"${REPO_ROOT}/.go-version")"
readonly HEALTH_CHECKER_VERSION="$(awk '$1 == "go" { print $2; exit }' "${REPO_ROOT}/monitoring/gpu-health-checker/go.mod")"
readonly EXPECTED_SHARED_VERSION="1.26.10"
readonly EXPECTED_HEALTH_CHECKER_VERSION="1.27.3"
readonly EXPECTED_MICROSOFT_GO_COMMIT="519c6ca6486122d981e6c1b5ee95e3df7ecbdc38"

fail() {
  echo "Go toolchain pin check failed: $*" >&2
  exit 1
}

[[ "${SHARED_VERSION}" == "${EXPECTED_SHARED_VERSION}" ]] ||
  fail ".go-version must be ${EXPECTED_SHARED_VERSION}, got ${SHARED_VERSION}"
[[ "${HEALTH_CHECKER_VERSION}" == "${EXPECTED_HEALTH_CHECKER_VERSION}" ]] ||
  fail "monitoring/gpu-health-checker/go.mod must use Go ${EXPECTED_HEALTH_CHECKER_VERSION}, got ${HEALTH_CHECKER_VERSION}"

readonly SHARED_PIN_FILES=(
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

for relative_path in "${SHARED_PIN_FILES[@]}"; do
  path="${REPO_ROOT}/${relative_path}"
  grep -Fq "${SHARED_VERSION}" "${path}" ||
    fail "${relative_path} does not pin Go ${SHARED_VERSION}"
  allowed_versions="^(go)?${SHARED_VERSION//./\\.}$"
  if [[ "${relative_path}" == "AGENTS.md" ]]; then
    allowed_versions="${allowed_versions}|^1\\.26\\.5$"
  fi
  stale_versions="$(grep -Eo '(go)?1\.26\.[0-9]+' "${path}" | sort -u | grep -Ev "${allowed_versions}" || true)"
  [[ -z "${stale_versions}" ]] ||
    fail "${relative_path} contains inconsistent Go 1.26 pins: ${stale_versions}"
done

stale_repository_pins="$(
  git -C "${REPO_ROOT}" grep -nE '(go)?1\.26\.(8|9)|go 1\.27\.2' -- \
    . ':(exclude)cli/releases/v0.4.4.md' || true
)"
[[ -z "${stale_repository_pins}" ]] ||
  fail "repository contains vulnerable Go toolchain pins: ${stale_repository_pins}"

grep -Fq "ARG MICROSOFT_GO_COMMIT=${EXPECTED_MICROSOFT_GO_COMMIT}" \
  "${REPO_ROOT}/images/tau-core-controller/Dockerfile" ||
  fail "images/tau-core-controller/Dockerfile has an unexpected Microsoft Go source commit"
grep -Fq "matrix.module == 'monitoring/gpu-health-checker' && 'monitoring/gpu-health-checker/go.mod' || '.go-version'" \
  "${REPO_ROOT}/.github/workflows/taugrid-validation.yml" ||
  fail "TauGrid validation must select the health checker module toolchain independently"

echo "Go toolchain pins are consistent: shared ${SHARED_VERSION}, GPU health checker ${HEALTH_CHECKER_VERSION}"
