#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly GODEBUG_LDFLAG="-X=runtime.godebugDefault=ms_tlsmldsacert=0"
readonly GO_TOOLCHAIN_VERSION="1.27.3"
readonly GO_MODULE_VERSION="1.27.2"
readonly MICROSOFT_GO_TOOLSET="go1.27.3-1-microsoft"

readonly -a BUILD_FILES=(
  "cli/Makefile"
  "portal/Makefile"
  "controllers/tau-core/Makefile"
  "metrics/experiment-metrics-collector/Makefile"
  "monitoring/gpu-health-checker/Makefile"
  "monitoring/gpu-metrics-collector/Makefile"
  "images/tau/Dockerfile"
  "images/tau-core-controller/Dockerfile"
  "images/taugrid-portal/Dockerfile"
  "images/taugrid-metrics-collector/Dockerfile"
  "images/gpu-metrics-collector/Dockerfile"
  "images/adx-mon/Dockerfile.alerter"
  "images/adx-mon/Dockerfile.collector"
  "images/adx-mon/Dockerfile.ingestor"
  "images/adx-mon/Dockerfile.operator"
  "cli/scripts/test-install-tau.ps1"
)

readonly -a MODULE_FILES=(
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

readonly -a MICROSOFT_GO_PIPELINE_FILES=(
  ".ado/tau-aks-e2e.yml"
  ".ado/tau-cli-e2e.yml"
  ".ado/tau-core-e2e.yml"
  ".pipelines/taugrid-flex-nightly.yml"
  ".pipelines/taugrid-unbounded-stable-nightly.yml"
)

readonly -a SYSTEM_CRYPTO_BUILD_FILES=(
  "images/tau-core-controller/Dockerfile"
  "images/adx-mon/Dockerfile.alerter"
  "images/adx-mon/Dockerfile.collector"
  "images/adx-mon/Dockerfile.ingestor"
  "images/adx-mon/Dockerfile.operator"
)

test "$(cat "${REPO_ROOT}/.go-version")" = "${GO_TOOLCHAIN_VERSION}"
grep -Fq "MICROSOFT_GO_TOOLSET := ${MICROSOFT_GO_TOOLSET}" "${REPO_ROOT}/cli/Makefile"
grep -Fq "DARWIN_MIN_VERSION := 13.0" "${REPO_ROOT}/cli/Makefile"

for file in "${MODULE_FILES[@]}"; do
  grep -Fxq "go ${GO_MODULE_VERSION}" "${REPO_ROOT}/${file}"
done

for file in "${BUILD_FILES[@]}"; do
  grep -Fq -- "${GODEBUG_LDFLAG}" "${REPO_ROOT}/${file}"
done

for file in "${MICROSOFT_GO_PIPELINE_FILES[@]}"; do
  grep -Fq "1.27.3-1" "${REPO_ROOT}/${file}"
  grep -Fq "goDownloadUrl: https://aka.ms/golang/release/latest" "${REPO_ROOT}/${file}"
done

for file in "${SYSTEM_CRYPTO_BUILD_FILES[@]}"; do
  if grep -Eq "GOEXPERIMENT=(systemcrypto|nosystemcrypto)" "${REPO_ROOT}/${file}"; then
    exit 1
  fi
  if grep -Fq "MS_GO_NOSYSTEMCRYPTO=1" "${REPO_ROOT}/${file}"; then
    exit 1
  fi
done

grep -Fq "MS_GO_NOSYSTEMCRYPTO=1" "${REPO_ROOT}/images/gpu-metrics-collector/Dockerfile"
if grep -Eq "GOEXPERIMENT=(systemcrypto|nosystemcrypto|none)" "${REPO_ROOT}/images/gpu-metrics-collector/Dockerfile"; then
  exit 1
fi

PROBE_DIR="$(mktemp -d)"
readonly PROBE_DIR
trap 'rm -rf -- "${PROBE_DIR}"' EXIT

cat >"${PROBE_DIR}/go.mod" <<EOF
module example.com/taugrid-godebug-contract

go ${GO_MODULE_VERSION}
EOF

cat >"${PROBE_DIR}/main.go" <<'EOF'
package main

func main() {}
EOF

(
  cd "${PROBE_DIR}"
  GOTOOLCHAIN=local go build -ldflags "${GODEBUG_LDFLAG}" -o probe .
  go version -m probe | grep -Fq -- "${GODEBUG_LDFLAG}"
)

echo "Go runtime default contract tests passed"
