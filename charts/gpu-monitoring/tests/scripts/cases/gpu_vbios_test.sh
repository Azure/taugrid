#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case gpu-vbios

# check_gpu_vbios.sh asserts allow-list membership; check_gpu_vbios_consistency.sh
# asserts that all GPUs agree. NPD maps one script to one condition, so the two
# properties must stay independently reported.
readonly VBIOS_ALLOWED='("96.00.BC.00.02" "96.00.BC.00.01")'

# Uniform VBIOS on the allow-list: both checks pass.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.02" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_uniform_allowed 0
assert_contains vbios_uniform_allowed "$RUN_OUTPUT" "matches one of the expected versions"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.02" \
  bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_uniform 0
assert_contains vbios_consistency_uniform "$RUN_OUTPUT" "All GPUs report the same VBIOS version"

# Uniform VBIOS that is not on the allow-list is fleet drift: the allow-list
# check fails and the consistency check stays healthy.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.99 96.00.BC.00.99" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_uniform_unexpected 1
assert_contains vbios_uniform_unexpected "$RUN_OUTPUT" "does not match one of the expected versions"
assert_contains vbios_uniform_unexpected "$RUN_OUTPUT" "96.00.BC.00.99"
assert_contains vbios_uniform_unexpected "$RUN_OUTPUT" "FaultCode: NHC2001"
assert_not_contains vbios_uniform_unexpected "$RUN_OUTPUT" "More than 1 VBIOS version"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.99 96.00.BC.00.99" \
  bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_unexpected 0
assert_contains vbios_consistency_unexpected "$RUN_OUTPUT" "All GPUs report the same VBIOS version"

# Mixed allow-listed VBIOS is a hardware fault, not allow-list drift.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_mixed 1
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "More than 1 VBIOS version"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "96.00.BC.00.01"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "96.00.BC.00.02"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "FaultCode: NHC2001"
assert_not_contains vbios_consistency_mixed "$RUN_OUTPUT" "does not match one of the expected versions"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_mixed_allowed 0
assert_contains vbios_mixed_allowed "$RUN_OUTPUT" "matches one of the expected versions"

# Mixed VBIOS with an off-list version fails only the allow-list check.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.99" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_mixed_unexpected 1
assert_contains vbios_mixed_unexpected "$RUN_OUTPUT" "GPU VBIOS version (96.00.BC.00.99) does not match"
assert_not_contains vbios_mixed_unexpected "$RUN_OUTPUT" "More than 1 VBIOS version"

# An empty allow-list skips only the allow-list check.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.99 96.00.BC.00.99" \
  VBIOS_VERSIONS='("")' bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_empty_allow_list 0
assert_contains vbios_empty_allow_list "$RUN_OUTPUT" "No expected VBIOS versions configured, skipping check"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  VBIOS_VERSIONS='("")' bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_no_allow_list 1
assert_contains vbios_consistency_no_allow_list "$RUN_OUTPUT" "More than 1 VBIOS version"

# Failed or incomplete nvidia-smi output is UNKNOWN for both checks.
run_check NVIDIA_SMI_QUERY_FAIL=1 VBIOS_VERSIONS="$VBIOS_ALLOWED" \
  bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_query_fail 2
assert_contains vbios_query_fail "$RUN_OUTPUT" "failed to run nvidia-smi"

run_check NVIDIA_SMI_QUERY_FAIL=1 bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_query_fail 2
assert_contains vbios_consistency_query_fail "$RUN_OUTPUT" "failed to run nvidia-smi"

run_check NVIDIA_SMI_NO_VBIOS=1 VBIOS_VERSIONS="$VBIOS_ALLOWED" \
  bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_missing 2
assert_contains vbios_missing "$RUN_OUTPUT" "No VBIOS version found"

run_check NVIDIA_SMI_NO_VBIOS=1 bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_missing 2
assert_contains vbios_consistency_missing "$RUN_OUTPUT" "No VBIOS version found"
