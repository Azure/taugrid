#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case gpu-runtime

if throttle_output="$(
  GPU_DRIVER_VERSIONS='("580.126.09")' \
    NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000008 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  fail "expected hardware slowdown to fail the GPU throttle check"
fi
assert_contains throttle_output "$throttle_output" "GPU 0 throttled"

GPU_DRIVER_VERSIONS='("580.126.09")' \
  NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000004 \
  bash "$CHART_DIR/scripts/check_gpu_throttle.sh"
GPU_DRIVER_VERSIONS='("580.126.09")' \
  NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000001 \
  bash "$CHART_DIR/scripts/check_gpu_throttle.sh"

empty_version_output="$(
  GPU_DRIVER_VERSIONS='("")' bash "$CHART_DIR/scripts/check_gpu_throttle.sh"
)"
assert_contains empty_version_output "$empty_version_output" "No GPU throttling detected"
if throttle_query_output="$(
  GPU_DRIVER_VERSIONS='("")' NVIDIA_SMI_THROTTLE_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  fail "expected an active-reason query failure to fail the GPU throttle check"
fi
assert_contains throttle_query_output "$throttle_query_output" "return code is 1"

NPD_DCGM_REQUIRED=1 DCGMI_HEALTH_OUTPUT="DCGM health check passed" \
  bash "$CHART_DIR/scripts/check-dcgm-health.sh"
if dcgm_output="$(
  NPD_DCGM_REQUIRED=1 DCGMI_HEALTH_OUTPUT="DCGM diagnostic failure" \
    DCGMI_HEALTH_EXIT_CODE=3 bash "$CHART_DIR/scripts/check-dcgm-health.sh" 2>&1
)"; then
  fail "expected a failed dcgmi health command to fail the check"
fi
assert_contains dcgm_output "$dcgm_output" "DCGM diagnostic failure"
assert_contains dcgm_output "$dcgm_output" "return code 3"

dcgm_skip_output="$(
  NPD_DCGM_REQUIRED=0 bash "$CHART_DIR/scripts/check-dcgm-health.sh"
)"
assert_contains dcgm_skip_output "$dcgm_skip_output" "not required for this profile"

readonly NSENTER_LOG="$TEST_ROOT/nsenter-args"
NSENTER_ARGS_FILE="$NSENTER_LOG" bash "$CHART_DIR/scripts/dcgmi-wrapper.sh" health -t
[[ "$(<"$NSENTER_LOG")" == "--target 1 --mount -- dcgmi health -t" ]] ||
  fail "unexpected nsenter arguments: $(<"$NSENTER_LOG")"

readonly XID_LOGFILE="$TEST_ROOT/gpu-xid.log"
readonly XID_EVENT="kernel: NVRM: Xid (PCI:0000:01:00): 79, pid=1234"
if first_xid_output="$(
  GPU_XID_LOGFILE="$XID_LOGFILE" JOURNALCTL_OUTPUT="$XID_EVENT" \
    bash "$CHART_DIR/scripts/check_gpu_xid.sh" 2>&1
)"; then
  fail "expected an active XID to fail the first check"
fi
assert_contains first_xid_output "$first_xid_output" "GPU Xid errors detected"
if second_xid_output="$(
  GPU_XID_LOGFILE="$XID_LOGFILE" JOURNALCTL_OUTPUT="$XID_EVENT" \
    bash "$CHART_DIR/scripts/check_gpu_xid.sh" 2>&1
)"; then
  fail "expected a deduplicated active XID to remain unhealthy"
fi
assert_contains second_xid_output "$second_xid_output" "XID 79 already logged"
