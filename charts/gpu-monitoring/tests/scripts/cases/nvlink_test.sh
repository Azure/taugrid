#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case nvlink

GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
GPU_TYPE=GB300 bash "$CHART_DIR/scripts/check_temp_imex.sh"

if nvlink_empty_output="$(
  EXPECTED_NUM_GPU=1 NVIDIA_SMI_EMPTY_NVLINK_STATUS=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink.sh" 2>&1
)"; then
  fail "expected an empty NVLink status to fail the generic check"
fi
assert_contains nvlink_empty_output "$nvlink_empty_output" "NVLINK is not enabled"

if b200_empty_output="$(
  GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 NVIDIA_SMI_EMPTY_NVLINK_STATUS=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  fail "expected an empty NVLink status to fail the Blackwell check"
fi
assert_contains b200_empty_output "$b200_empty_output" "NVLINK is not enabled"

if b200_query_output="$(
  GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 NVIDIA_SMI_NVLINK_DETAIL_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  fail "expected a failed Blackwell NVLink query to fail the check"
fi
assert_contains b200_query_output "$b200_query_output" "error code 1"

if b200_c2c_output="$(
  GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 NVIDIA_SMI_C2C_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  fail "expected a failed Blackwell C2C query to fail the check"
fi
assert_contains b200_c2c_output "$b200_c2c_output" "error code 1"

skip_output="$(
  GPU_TYPE=H100 EXPECTED_NUM_GPU=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
)"
assert_contains skip_output "$skip_output" "Not a Blackwell node"
