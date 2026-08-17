#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case ib-flaps

readonly UNKNOWN_FLAP_STATE_FILE="$TEST_ROOT/ib-flap-unknown-state.txt"
cat >"$UNKNOWN_FLAP_STATE_FILE" <<'EOF'
7000 mlx5_0:1=up
7600 mlx5_0:1=unknown
8200 mlx5_0:1=error
8800 mlx5_0:1=unknown
9400 mlx5_0:1=up
EOF
TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
  IB_FLAP_THRESHOLD_SHORT=1 IB_FLAP_CHECK_WINDOW=3600 \
  IB_FLAP_STATE_FILE="$UNKNOWN_FLAP_STATE_FILE" \
  bash "$CHART_DIR/scripts/check_ib_flaps.sh"

readonly FLAP_STATE_FILE="$TEST_ROOT/ib-flap-state.txt"
# Four transitions 700 seconds apart form two flaps inside the one-hour window.
# More than ten newer stable samples prove retention is time-based, not count-based.
cat >"$FLAP_STATE_FILE" <<'EOF'
6300 mlx5_0:1=up
6500 mlx5_0:1=up
7200 mlx5_0:1=down
7900 mlx5_0:1=up
8600 mlx5_0:1=down
9300 mlx5_0:1=up
9350 mlx5_0:1=up
9400 mlx5_0:1=up
9450 mlx5_0:1=up
9500 mlx5_0:1=up
9550 mlx5_0:1=up
9600 mlx5_0:1=up
9650 mlx5_0:1=up
9700 mlx5_0:1=up
9750 mlx5_0:1=up
9800 mlx5_0:1=up
9850 mlx5_0:1=up
9900 mlx5_0:1=up
EOF
if first_flap_output="$(
  TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
    IB_FLAP_THRESHOLD_SHORT=2 IB_FLAP_CHECK_WINDOW=3600 \
    IB_FLAP_STATE_FILE="$FLAP_STATE_FILE" \
    bash "$CHART_DIR/scripts/check_ib_flaps.sh" 2>&1
)"; then
  fail "expected two hour-window IB flaps to fail the first check"
fi
assert_contains first_flap_output "$first_flap_output" "2 ibstat state flaps"
grep -q '^6300 ' "$FLAP_STATE_FILE" &&
  fail "stale flap entry outside the time window was not pruned"
grep -q '^6500 ' "$FLAP_STATE_FILE"
[[ "$(wc -l <"$FLAP_STATE_FILE" | tr -d ' ')" -gt 10 ]] ||
  fail "flap state file was truncated too aggressively"
if second_flap_output="$(
  TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
    IB_FLAP_THRESHOLD_SHORT=2 IB_FLAP_CHECK_WINDOW=3600 \
    IB_FLAP_STATE_FILE="$FLAP_STATE_FILE" \
    bash "$CHART_DIR/scripts/check_ib_flaps.sh" 2>&1
)"; then
  fail "expected retained hour-window IB flaps to fail the repeat check"
fi
assert_contains second_flap_output "$second_flap_output" "2 ibstat state flaps"
