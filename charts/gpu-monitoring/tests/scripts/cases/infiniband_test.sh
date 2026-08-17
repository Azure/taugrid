#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case infiniband

readonly FLEX_A100_SYSFS="$TEST_ROOT/flex-a100-sys"
flex_a100_devices="$(create_ib_fabric "$FLEX_A100_SYSFS" mlx5_ 200 0x8003)"
SYSFS_ROOT="$FLEX_A100_SYSFS" IB_DEVICES="$flex_a100_devices" \
  EXPECTED_IB_GBPS=200 bash "$CHART_DIR/scripts/check_ib.sh"
SYSFS_ROOT="$FLEX_A100_SYSFS" IB_DEVICES="$flex_a100_devices" \
  EXPECTED_IB_PKEY=0x8003 bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

readonly FLEX_H200_SYSFS="$TEST_ROOT/flex-h200-sys"
flex_h200_devices="$(create_ib_fabric "$FLEX_H200_SYSFS" mlx5_ib 400 0xffff)"
SYSFS_ROOT="$FLEX_H200_SYSFS" IB_DEVICES="$flex_h200_devices" \
  EXPECTED_IB_GBPS=400 bash "$CHART_DIR/scripts/check_ib.sh"
SYSFS_ROOT="$FLEX_H200_SYSFS" IB_DEVICES="$flex_h200_devices" \
  EXPECTED_IB_PKEY=0xffff bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

readonly EAST_H200_SYSFS="$TEST_ROOT/east-h200-sys"
east_h200_devices="$(create_ib_fabric "$EAST_H200_SYSFS" mlx5_ 400 0x8001)"
SYSFS_ROOT="$EAST_H200_SYSFS" IB_DEVICES="$east_h200_devices" \
  EXPECTED_IB_GBPS=400 bash "$CHART_DIR/scripts/check_ib.sh"
SYSFS_ROOT="$EAST_H200_SYSFS" IB_DEVICES="$east_h200_devices" \
  EXPECTED_IB_PKEY=0x8001 bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

# Link health remains healthy when only the PKey is wrong.
printf '0x8003\n' >"$EAST_H200_SYSFS/class/infiniband/mlx5_0/ports/1/pkeys/0"
SYSFS_ROOT="$EAST_H200_SYSFS" IB_DEVICES="$east_h200_devices" \
  EXPECTED_IB_GBPS=400 bash "$CHART_DIR/scripts/check_ib.sh"
if pkey_mismatch_output="$(
  SYSFS_ROOT="$EAST_H200_SYSFS" IB_DEVICES="$east_h200_devices" \
    EXPECTED_IB_PKEY=0x8001 bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  fail "expected the PKey check to reject a mismatched East H200 PKey"
fi
assert_contains pkey_mismatch_output "$pkey_mismatch_output" \
  "mlx5_0:1 expected PKey 0x8001; observed 0x8003"

# PKey health remains healthy when only link state and rate are wrong.
printf '2: DOWN\n' >"$FLEX_H200_SYSFS/class/infiniband/mlx5_ib0/ports/1/state"
printf '200 Gb/sec\n' >"$FLEX_H200_SYSFS/class/infiniband/mlx5_ib0/ports/1/rate"
SYSFS_ROOT="$FLEX_H200_SYSFS" IB_DEVICES="$flex_h200_devices" \
  EXPECTED_IB_PKEY=0xffff bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
if link_mismatch_output="$(
  SYSFS_ROOT="$FLEX_H200_SYSFS" IB_DEVICES="$flex_h200_devices" \
    EXPECTED_IB_GBPS=400 bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  fail "expected the link check to reject a down, rate-mismatched Flex H200 port"
fi
assert_contains link_mismatch_output "$link_mismatch_output" \
  "mlx5_ib0:1 expected state=ACTIVE physical_state=LinkUp rate=400Gbps; observed state=DOWN physical_state=LinkUp rate=200Gbps"

if missing_pkey_output="$(
  SYSFS_ROOT="$FLEX_A100_SYSFS" IB_DEVICES="$flex_a100_devices" \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  fail "expected the PKey check to require an explicit PKey"
fi
assert_contains missing_pkey_output "$missing_pkey_output" \
  "EXPECTED_IB_PKEY must be an explicit hexadecimal PKey"
