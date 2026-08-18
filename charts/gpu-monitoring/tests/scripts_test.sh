#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_ROOT/bin"

fail() {
  echo "assertion failed: $*" >&2
  exit 1
}

assert_contains() {
  case "$2" in
    *"$3"*) ;;
    *) fail "$1: expected to contain '$3', got: $2" ;;
  esac
}

assert_not_contains() {
  case "$2" in
    *"$3"*) fail "$1: expected not to contain '$3', got: $2" ;;
  esac
}

cat >"$TEST_ROOT/bin/nvidia-smi" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "nvlink --status")
    if [[ "${NVIDIA_SMI_NVLINK_STATUS_FAIL:-0}" == "1" ]]; then
      echo "nvlink status query failed" >&2
      exit 1
    fi
    [[ "${NVIDIA_SMI_EMPTY_NVLINK_STATUS:-0}" == "1" ]] && exit 0
    echo "Link 0: 50 GB/s"
    ;;
  "nvlink -s -i 0")
    if [[ "${NVIDIA_SMI_NVLINK_DETAIL_FAIL:-0}" == "1" ]]; then
      echo "nvlink detail query failed" >&2
      exit 1
    fi
    echo "Link 0: 50 GB/s"
    ;;
  "nvlink --id=0 --status")
    if [[ "${NVIDIA_SMI_NVLINK_DETAIL_FAIL:-0}" == "1" ]]; then
      echo "nvlink detail query failed" >&2
      exit 1
    fi
    echo "Link 0: 50 GB/s"
    ;;
  "c2c --id=0 --status")
    if [[ "${NVIDIA_SMI_C2C_FAIL:-0}" == "1" ]]; then
      echo "c2c status query failed" >&2
      exit 1
    fi
    echo "Link 0: 50 GB/s"
    ;;
  "topo -m")
    echo "NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18"
    ;;
  "--id=0 --query-gpu=driver_version --format=csv,noheader")
    echo "580.126.09"
    ;;
  "--query-gpu=clocks_event_reasons.active --format=csv,noheader,nounits")
    if [[ "${NVIDIA_SMI_THROTTLE_FAIL:-0}" == "1" ]]; then
      echo "active reason query failed" >&2
      exit 1
    fi
    echo "${NVIDIA_SMI_THROTTLE_OUTPUT:-0x0000000000000000}"
    ;;
  "-q")
    if [[ "${NVIDIA_SMI_QUERY_FAIL:-0}" == "1" ]]; then
      echo "device query failed" >&2
      exit 1
    fi
    echo "==============NVSMI LOG=============="
    if [[ "${NVIDIA_SMI_NO_VBIOS:-0}" != "1" ]]; then
      for vbios_version in ${NVIDIA_SMI_VBIOS_VERSIONS:-96.00.BC.00.02}; do
        echo "    VBIOS Version                         : ${vbios_version}"
      done
    fi
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod +x "$TEST_ROOT/bin/nvidia-smi"

cat >"$TEST_ROOT/bin/pgrep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$TEST_ROOT/bin/pgrep"

cat >"$TEST_ROOT/bin/dcgmi" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "health -c")
    exit 0
    ;;
  "health -t")
    echo "${DCGMI_HEALTH_OUTPUT:-DCGM health check passed}"
    exit "${DCGMI_HEALTH_EXIT_CODE:-0}"
    ;;
  *)
    exit 2
    ;;
esac
EOF
chmod +x "$TEST_ROOT/bin/dcgmi"

cat >"$TEST_ROOT/bin/nsenter" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$NSENTER_ARGS_FILE"
EOF
chmod +x "$TEST_ROOT/bin/nsenter"

cat >"$TEST_ROOT/bin/journalctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${JOURNALCTL_OUTPUT:-}"
exit "${JOURNALCTL_EXIT_CODE:-0}"
EOF
chmod +x "$TEST_ROOT/bin/journalctl"

cat >"$TEST_ROOT/bin/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "+%s" && -n "${TEST_NOW:-}" ]]; then
  echo "$TEST_NOW"
  exit 0
fi
exec /bin/date "$@"
EOF
chmod +x "$TEST_ROOT/bin/date"

cat >"$TEST_ROOT/bin/ibstat" <<'EOF'
#!/usr/bin/env bash
cat <<'OUTPUT'
CA 'mlx5_0'
  Port 1:
    State: Active
OUTPUT
EOF
chmod +x "$TEST_ROOT/bin/ibstat"

cat >"$TEST_ROOT/bin/timeout" <<'EOF'
#!/usr/bin/env bash
shift
exec "$@"
EOF
chmod +x "$TEST_ROOT/bin/timeout"

PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
  bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 \
  bash "$CHART_DIR/scripts/check_temp_imex.sh"

if nvlink_empty_output="$(
  PATH="$TEST_ROOT/bin:$PATH" EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_EMPTY_NVLINK_STATUS=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink.sh" 2>&1
)"; then
  echo "expected an empty NVLink status to fail the generic check" >&2
  exit 1
fi
assert_contains nvlink_empty_output "$nvlink_empty_output" "NVLINK is not enabled"

if b200_empty_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_EMPTY_NVLINK_STATUS=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected an empty NVLink status to fail the Blackwell check" >&2
  exit 1
fi
assert_contains b200_empty_output "$b200_empty_output" "NVLINK is not enabled"

if b200_query_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_NVLINK_DETAIL_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected a failed Blackwell NVLink query to fail the check" >&2
  exit 1
fi
assert_contains b200_query_output "$b200_query_output" "error code 1"

if b200_c2c_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_C2C_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected a failed Blackwell C2C query to fail the check" >&2
  exit 1
fi
assert_contains b200_c2c_output "$b200_c2c_output" "error code 1"

skip_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=H100 EXPECTED_NUM_GPU=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
)"
assert_contains skip_output "$skip_output" "Not a Blackwell node"

create_ib_fabric() {
  local root="$1"
  local device_prefix="$2"
  local rate="$3"
  local pkey="$4"
  local devices=""

  for index in {0..7}; do
    local device="${device_prefix}${index}"
    local port_root="$root/class/infiniband/$device/ports/1"
    mkdir -p "$port_root/pkeys"
    printf '4: ACTIVE\n' >"$port_root/state"
    printf '5: LinkUp\n' >"$port_root/phys_state"
    printf '%s Gb/sec\n' "$rate" >"$port_root/rate"
    printf '%s\n' "$pkey" >"$port_root/pkeys/0"
    devices+="${devices:+ }${device}:1"
  done

  printf '%s\n' "$devices"
}

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
  echo "expected the PKey check to reject a mismatched East H200 PKey" >&2
  exit 1
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
  echo "expected the link check to reject a down, rate-mismatched Flex H200 port" >&2
  exit 1
fi
assert_contains link_mismatch_output "$link_mismatch_output" \
  "mlx5_ib0:1 expected state=ACTIVE physical_state=LinkUp rate=400Gbps; observed state=DOWN physical_state=LinkUp rate=200Gbps"

if missing_pkey_output="$(
  SYSFS_ROOT="$FLEX_A100_SYSFS" IB_DEVICES="$flex_a100_devices" \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  echo "expected the PKey check to require an explicit PKey" >&2
  exit 1
fi
assert_contains missing_pkey_output "$missing_pkey_output" \
  "EXPECTED_IB_PKEY must be an explicit hexadecimal PKey"

if throttle_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("580.126.09")' \
    NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000008 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  echo "expected hardware slowdown to fail the GPU throttle check" >&2
  exit 1
fi
assert_contains throttle_output "$throttle_output" "GPU 0 throttled"

PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("580.126.09")' \
  NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000004 \
  bash "$CHART_DIR/scripts/check_gpu_throttle.sh"
PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("580.126.09")' \
  NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000001 \
  bash "$CHART_DIR/scripts/check_gpu_throttle.sh"

empty_version_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("")' \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh"
)"
assert_contains empty_version_output "$empty_version_output" "No GPU throttling detected"
if throttle_query_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("")' \
    NVIDIA_SMI_THROTTLE_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  echo "expected an active-reason query failure to fail the GPU throttle check" >&2
  exit 1
fi
assert_contains throttle_query_output "$throttle_query_output" "return code is 1"

PATH="$TEST_ROOT/bin:$PATH" NPD_DCGM_REQUIRED=1 \
  DCGMI_HEALTH_OUTPUT="DCGM health check passed" \
  bash "$CHART_DIR/scripts/check-dcgm-health.sh"
if dcgm_output="$(
  PATH="$TEST_ROOT/bin:$PATH" NPD_DCGM_REQUIRED=1 \
    DCGMI_HEALTH_OUTPUT="DCGM diagnostic failure" DCGMI_HEALTH_EXIT_CODE=3 \
    bash "$CHART_DIR/scripts/check-dcgm-health.sh" 2>&1
)"; then
  echo "expected a failed dcgmi health command to fail the check" >&2
  exit 1
fi
assert_contains dcgm_output "$dcgm_output" "DCGM diagnostic failure"
assert_contains dcgm_output "$dcgm_output" "return code 3"

dcgm_skip_output="$(
  PATH="$TEST_ROOT/bin:$PATH" NPD_DCGM_REQUIRED=0 \
    bash "$CHART_DIR/scripts/check-dcgm-health.sh"
)"
assert_contains dcgm_skip_output "$dcgm_skip_output" "not required for this profile"

readonly NSENTER_LOG="$TEST_ROOT/nsenter-args"
PATH="$TEST_ROOT/bin:$PATH" NSENTER_ARGS_FILE="$NSENTER_LOG" \
  bash "$CHART_DIR/scripts/dcgmi-wrapper.sh" health -t
[[ "$(<"$NSENTER_LOG")" == "--target 1 --mount -- dcgmi health -t" ]] ||
  fail "unexpected nsenter arguments: $(<"$NSENTER_LOG")"

readonly XID_LOGFILE="$TEST_ROOT/gpu-xid.log"
readonly XID_EVENT="kernel: NVRM: Xid (PCI:0000:01:00): 79, pid=1234"
if first_xid_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_XID_LOGFILE="$XID_LOGFILE" \
    JOURNALCTL_OUTPUT="$XID_EVENT" \
    bash "$CHART_DIR/scripts/check_gpu_xid.sh" 2>&1
)"; then
  echo "expected an active XID to fail the first check" >&2
  exit 1
fi
assert_contains first_xid_output "$first_xid_output" "GPU Xid errors detected"
if second_xid_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_XID_LOGFILE="$XID_LOGFILE" \
    JOURNALCTL_OUTPUT="$XID_EVENT" \
    bash "$CHART_DIR/scripts/check_gpu_xid.sh" 2>&1
)"; then
  echo "expected a deduplicated active XID to remain unhealthy" >&2
  exit 1
fi
assert_contains second_xid_output "$second_xid_output" "XID 79 already logged"

readonly UNKNOWN_FLAP_STATE_FILE="$TEST_ROOT/ib-flap-unknown-state.txt"
cat >"$UNKNOWN_FLAP_STATE_FILE" <<'EOF'
7000 mlx5_0:1=up
7600 mlx5_0:1=unknown
8200 mlx5_0:1=error
8800 mlx5_0:1=unknown
9400 mlx5_0:1=up
EOF
PATH="$TEST_ROOT/bin:$PATH" TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
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
  PATH="$TEST_ROOT/bin:$PATH" TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
    IB_FLAP_THRESHOLD_SHORT=2 IB_FLAP_CHECK_WINDOW=3600 \
    IB_FLAP_STATE_FILE="$FLAP_STATE_FILE" \
    bash "$CHART_DIR/scripts/check_ib_flaps.sh" 2>&1
)"; then
  echo "expected two hour-window IB flaps to fail the first check" >&2
  exit 1
fi
assert_contains first_flap_output "$first_flap_output" "2 ibstat state flaps"
grep -q '^6300 ' "$FLAP_STATE_FILE" &&
  fail "stale flap entry outside the time window was not pruned"
grep -q '^6500 ' "$FLAP_STATE_FILE"
[[ "$(wc -l < "$FLAP_STATE_FILE" | tr -d ' ')" -gt 10 ]] ||
  fail "flap state file was truncated too aggressively"
if second_flap_output="$(
  PATH="$TEST_ROOT/bin:$PATH" TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
    IB_FLAP_THRESHOLD_SHORT=2 IB_FLAP_CHECK_WINDOW=3600 \
    IB_FLAP_STATE_FILE="$FLAP_STATE_FILE" \
    bash "$CHART_DIR/scripts/check_ib_flaps.sh" 2>&1
)"; then
  echo "expected retained hour-window IB flaps to fail the repeat check" >&2
  exit 1
fi
assert_contains second_flap_output "$second_flap_output" "2 ibstat state flaps"

# --- GPU VBIOS checks -------------------------------------------------------
#
# check_gpu_vbios.sh asserts allow-list membership; check_gpu_vbios_consistency.sh
# asserts that all GPUs agree. NPD maps one script to one condition, so the two
# properties must stay independently reported.

# Run a check script under the mock PATH, capturing output and exit status.
# `set -e` aborts on a non-zero status, and a `!`-negated command is exempt from
# `set -e` in every bash, so capture the status explicitly instead.
run_check() {
  set +e
  RUN_OUTPUT="$(PATH="$TEST_ROOT/bin:$PATH" env "$@" 2>&1)"
  RUN_STATUS=$?
  set -e
}

assert_status() {
  [ "$RUN_STATUS" -eq "$2" ] || fail "$1: expected exit $2, got $RUN_STATUS: $RUN_OUTPUT"
}

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
# An allow-list failure must not claim the GPUs disagree, or GPUVbiosMismatch
# and GPUVbiosInconsistent would not be independent signals.
assert_not_contains vbios_uniform_unexpected "$RUN_OUTPUT" "More than 1 VBIOS version"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.99 96.00.BC.00.99" \
  bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_unexpected 0
assert_contains vbios_consistency_unexpected "$RUN_OUTPUT" "All GPUs report the same VBIOS version"

# Mixed VBIOS whose versions are all allow-listed is a hardware fault, not
# drift: the consistency check fails and the allow-list check passes. Before the
# split this reported GPUVbiosMismatch=True, which routed a page-worthy fault to
# the drift ticket queue.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_mixed 1
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "More than 1 VBIOS version"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "96.00.BC.00.01"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "96.00.BC.00.02"
assert_contains vbios_consistency_mixed "$RUN_OUTPUT" "FaultCode: NHC2001"
# A consistency failure must not claim the VBIOS is off the allow-list.
assert_not_contains vbios_consistency_mixed "$RUN_OUTPUT" "does not match one of the expected versions"

run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_mixed_allowed 0
assert_contains vbios_mixed_allowed "$RUN_OUTPUT" "matches one of the expected versions"

# Mixed VBIOS where one version is off the allow-list still fails the allow-list
# check, which names the offending version only.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.99" \
  VBIOS_VERSIONS="$VBIOS_ALLOWED" bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_mixed_unexpected 1
assert_contains vbios_mixed_unexpected "$RUN_OUTPUT" "GPU VBIOS version (96.00.BC.00.99) does not match"
assert_not_contains vbios_mixed_unexpected "$RUN_OUTPUT" "More than 1 VBIOS version"

# An empty allow-list still skips the allow-list check, and still exits 0. The
# chart always sets VBIOS_VERSIONS, using ("") for profiles with no allow-list.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.99 96.00.BC.00.99" \
  VBIOS_VERSIONS='("")' bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_empty_allow_list 0
assert_contains vbios_empty_allow_list "$RUN_OUTPUT" "No expected VBIOS versions configured, skipping check"

# The consistency check needs no allow-list, so it must still fire without one.
run_check NVIDIA_SMI_VBIOS_VERSIONS="96.00.BC.00.02 96.00.BC.00.01" \
  VBIOS_VERSIONS='("")' bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_no_allow_list 1
assert_contains vbios_consistency_no_allow_list "$RUN_OUTPUT" "More than 1 VBIOS version"

# A failed nvidia-smi is UNKNOWN (exit 2), not a fault, for both checks.
run_check NVIDIA_SMI_QUERY_FAIL=1 VBIOS_VERSIONS="$VBIOS_ALLOWED" \
  bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_query_fail 2
assert_contains vbios_query_fail "$RUN_OUTPUT" "failed to run nvidia-smi"

run_check NVIDIA_SMI_QUERY_FAIL=1 bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_query_fail 2
assert_contains vbios_consistency_query_fail "$RUN_OUTPUT" "failed to run nvidia-smi"

# nvidia-smi output without a VBIOS line is equally UNKNOWN for both checks.
run_check NVIDIA_SMI_NO_VBIOS=1 VBIOS_VERSIONS="$VBIOS_ALLOWED" \
  bash "$CHART_DIR/scripts/check_gpu_vbios.sh"
assert_status vbios_missing 2
assert_contains vbios_missing "$RUN_OUTPUT" "No VBIOS version found"

run_check NVIDIA_SMI_NO_VBIOS=1 bash "$CHART_DIR/scripts/check_gpu_vbios_consistency.sh"
assert_status vbios_consistency_missing 2
assert_contains vbios_consistency_missing "$RUN_OUTPUT" "No VBIOS version found"

readonly ORIGINAL_BUNDLE_NAME="$(
  helm template content-hash "$CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    awk '$1 == "name:" { print $2; exit }'
)"
readonly MUTATED_CHART_DIR="$TEST_ROOT/gpu-monitoring"
cp -R "$CHART_DIR" "$MUTATED_CHART_DIR"
printf '\n# content hash regression probe\n' >>"$MUTATED_CHART_DIR/scripts/check_gpu_xid.sh"
readonly MUTATED_BUNDLE_NAME="$(
  helm template content-hash "$MUTATED_CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    awk '$1 == "name:" { print $2; exit }'
)"
[[ "$ORIGINAL_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] ||
  fail "unexpected original bundle name: $ORIGINAL_BUNDLE_NAME"
[[ "$MUTATED_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] ||
  fail "unexpected mutated bundle name: $MUTATED_BUNDLE_NAME"
[[ "$ORIGINAL_BUNDLE_NAME" != "$MUTATED_BUNDLE_NAME" ]] ||
  fail "bundle name did not change when a script changed"

# Every custom-config subPath the DaemonSet mounts must exist as a key in the
# executable bundle. A check that is wired into a monitor config and mounted but
# left out of the bundle renders a DaemonSet whose plugin path does not exist,
# and no other assertion in this suite or in helm unittest catches it.
readonly BUNDLE_KEYS="$(
  helm template wiring "$CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    sed -n 's/^  \([A-Za-z0-9._-]*\): |$/\1/p' | sort -u
)"
readonly MOUNTED_SUBPATHS="$(
  helm template wiring "$CHART_DIR" --show-only templates/daemonset.yaml |
    awk '/^ *- name: /{ volume = $3 } /^ *subPath: / { if (volume == "custom-config") print $2 }' |
    sort -u
)"
# Guard against a parse that silently matches nothing, which would make the
# loop below vacuous.
[ "$(printf '%s\n' "$MOUNTED_SUBPATHS" | grep -c '\.sh$')" -ge 15 ] ||
  fail "bundle_wiring: parsed too few mounted scripts: $MOUNTED_SUBPATHS"
for mounted_subpath in $MOUNTED_SUBPATHS; do
  printf '%s\n' "$BUNDLE_KEYS" | grep -Fxq -- "$mounted_subpath" ||
    fail "bundle_wiring: the DaemonSet mounts $mounted_subpath but the executable bundle has no such key"
done
assert_contains bundle_wiring "$MOUNTED_SUBPATHS" "check_gpu_vbios.sh"
assert_contains bundle_wiring "$MOUNTED_SUBPATHS" "check_gpu_vbios_consistency.sh"

# Every plugin path the monitor configs reference must be mounted, for the same
# reason: NPD reports a missing script as a failing check on every node.
readonly REFERENCED_PLUGINS="$(
  sed -n 's#^ *"path": "/custom-config/\(.*\)",*$#\1#p' "$CHART_DIR"/configs/custom-plugin-monitor*.json |
    sort -u
)"
[ "$(printf '%s\n' "$REFERENCED_PLUGINS" | grep -c '\.sh$')" -ge 15 ] ||
  fail "bundle_wiring: parsed too few referenced plugins: $REFERENCED_PLUGINS"
for referenced_plugin in $REFERENCED_PLUGINS; do
  printf '%s\n' "$MOUNTED_SUBPATHS" | grep -Fxq -- "$referenced_plugin" ||
    fail "bundle_wiring: monitor configs reference $referenced_plugin but the DaemonSet does not mount it"
done
assert_contains bundle_wiring "$REFERENCED_PLUGINS" "check_gpu_vbios_consistency.sh"

# The split only routes correctly if every SKU declares both conditions and runs
# both scripts. A monitor config left behind keeps reporting a mixed-VBIOS
# hardware fault as allow-list drift on that SKU.
readonly MONITOR_CONFIGS=("$CHART_DIR"/configs/custom-plugin-monitor*.json)
[ "${#MONITOR_CONFIGS[@]}" -eq 5 ] ||
  fail "vbios_wiring: expected 5 monitor configs, found ${#MONITOR_CONFIGS[@]}"
for monitor_config in "${MONITOR_CONFIGS[@]}"; do
  config_name="$(basename "$monitor_config")"
  config_body="$(cat "$monitor_config")"
  assert_contains "$config_name" "$config_body" '"type": "GPUVbiosMismatch"'
  assert_contains "$config_name" "$config_body" '"type": "GPUVbiosInconsistent"'
  assert_contains "$config_name" "$config_body" '"condition": "GPUVbiosInconsistent"'
  assert_contains "$config_name" "$config_body" '"path": "/custom-config/check_gpu_vbios.sh"'
  assert_contains "$config_name" "$config_body" '"path": "/custom-config/check_gpu_vbios_consistency.sh"'
done
