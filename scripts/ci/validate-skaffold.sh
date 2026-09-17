#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"
readonly REPO_ROOT
readonly SKAFFOLD="${SKAFFOLD_BIN:-skaffold}"
readonly EXPECTED_VERSION="v2.24.0"

actual_version="$("$SKAFFOLD" version)"
if [[ "$actual_version" != "$EXPECTED_VERSION" ]]; then
  echo "expected Skaffold ${EXPECTED_VERSION}, got ${actual_version}" >&2
  exit 1
fi

"$SKAFFOLD" diagnose \
  --filename "${REPO_ROOT}/skaffold.yaml" \
  --yaml-only >/dev/null

"${SCRIPT_DIR}/tests/skaffold-acr-build_test.sh"
"${SCRIPT_DIR}/tests/skaffold-build_test.sh"

echo "Validated Skaffold ${EXPECTED_VERSION} configuration and ACR Tasks builder."
