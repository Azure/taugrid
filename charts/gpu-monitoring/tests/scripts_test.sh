#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEST_CASES=(
  nvlink_test.sh
  infiniband_test.sh
  gpu_runtime_test.sh
  ib_flaps_test.sh
  gpu_vbios_test.sh
  bundle_wiring_test.sh
)

for test_case in "${TEST_CASES[@]}"; do
  printf '=== RUN  %s\n' "$test_case"
  bash "${TESTS_DIR}/scripts/cases/${test_case}"
  printf '=== PASS %s\n' "$test_case"
done
