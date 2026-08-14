#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_ROOT/bin"

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
[[ "$nvlink_empty_output" == *"NVLINK is not enabled"* ]]

if b200_empty_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_EMPTY_NVLINK_STATUS=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected an empty NVLink status to fail the Blackwell check" >&2
  exit 1
fi
[[ "$b200_empty_output" == *"NVLINK is not enabled"* ]]

if b200_query_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_NVLINK_DETAIL_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected a failed Blackwell NVLink query to fail the check" >&2
  exit 1
fi
[[ "$b200_query_output" == *"error code 1"* ]]

if b200_c2c_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
    NVIDIA_SMI_C2C_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh" 2>&1
)"; then
  echo "expected a failed Blackwell C2C query to fail the check" >&2
  exit 1
fi
[[ "$b200_c2c_output" == *"error code 1"* ]]

skip_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=H100 EXPECTED_NUM_GPU=1 \
    bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
)"
[[ "$skip_output" == *"Not a Blackwell node"* ]]

readonly PORT_ROOT="$TEST_ROOT/sys/class/infiniband/mlx5_0/ports/1"
mkdir -p "$PORT_ROOT/pkeys" "$TEST_ROOT/sys/class/infiniband_mad"
printf '4: ACTIVE\n' >"$PORT_ROOT/state"
printf '5: LinkUp\n' >"$PORT_ROOT/phys_state"
printf '0xffff\n' >"$PORT_ROOT/pkeys/0"
printf '1\n' >"$TEST_ROOT/sys/class/infiniband_mad/abi_version"

printf '400 Gb/sec (4X HDR)\n' >"$PORT_ROOT/rate"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
  bash "$CHART_DIR/scripts/check_ib.sh"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
  bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

printf '800 Gb/sec (8X NDR)\n' >"$PORT_ROOT/rate"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
  bash "$CHART_DIR/scripts/check_ib.sh"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
  bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

printf '400 Gb/sec (4X HDR)\n' >"$PORT_ROOT/rate"
if mismatch_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
    bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  echo "expected the 800-Gbps check to reject a 400-Gbps link" >&2
  exit 1
fi
[[ "$mismatch_output" == *"800 Gb/sec"* ]]

if throttle_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("580.126.09")' \
    NVIDIA_SMI_THROTTLE_OUTPUT=0x0000000000000008 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  echo "expected hardware slowdown to fail the GPU throttle check" >&2
  exit 1
fi
[[ "$throttle_output" == *"GPU 0 throttled"* ]]

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
[[ "$empty_version_output" == *"No GPU throttling detected"* ]]
if throttle_query_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_DRIVER_VERSIONS='("")' \
    NVIDIA_SMI_THROTTLE_FAIL=1 \
    bash "$CHART_DIR/scripts/check_gpu_throttle.sh" 2>&1
)"; then
  echo "expected an active-reason query failure to fail the GPU throttle check" >&2
  exit 1
fi
[[ "$throttle_query_output" == *"return code is 1"* ]]

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
[[ "$dcgm_output" == *"DCGM diagnostic failure"* ]]
[[ "$dcgm_output" == *"return code 3"* ]]

dcgm_skip_output="$(
  PATH="$TEST_ROOT/bin:$PATH" NPD_DCGM_REQUIRED=0 \
    bash "$CHART_DIR/scripts/check-dcgm-health.sh"
)"
[[ "$dcgm_skip_output" == *"not required for this profile"* ]]

readonly NSENTER_LOG="$TEST_ROOT/nsenter-args"
PATH="$TEST_ROOT/bin:$PATH" NSENTER_ARGS_FILE="$NSENTER_LOG" \
  bash "$CHART_DIR/scripts/dcgmi-wrapper.sh" health -t
[[ "$(<"$NSENTER_LOG")" == "--target 1 --mount -- dcgmi health -t" ]]

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
[[ "$first_xid_output" == *"GPU Xid errors detected"* ]]
if second_xid_output="$(
  PATH="$TEST_ROOT/bin:$PATH" GPU_XID_LOGFILE="$XID_LOGFILE" \
    JOURNALCTL_OUTPUT="$XID_EVENT" \
    bash "$CHART_DIR/scripts/check_gpu_xid.sh" 2>&1
)"; then
  echo "expected a deduplicated active XID to remain unhealthy" >&2
  exit 1
fi
[[ "$second_xid_output" == *"XID 79 already logged"* ]]

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
[[ "$first_flap_output" == *"2 ibstat state flaps"* ]]
! grep -q '^6300 ' "$FLAP_STATE_FILE"
grep -q '^6500 ' "$FLAP_STATE_FILE"
[[ "$(wc -l < "$FLAP_STATE_FILE" | tr -d ' ')" -gt 10 ]]
if second_flap_output="$(
  PATH="$TEST_ROOT/bin:$PATH" TEST_NOW=10000 IB_DEVICES="mlx5_0:1" \
    IB_FLAP_THRESHOLD_SHORT=2 IB_FLAP_CHECK_WINDOW=3600 \
    IB_FLAP_STATE_FILE="$FLAP_STATE_FILE" \
    bash "$CHART_DIR/scripts/check_ib_flaps.sh" 2>&1
)"; then
  echo "expected retained hour-window IB flaps to fail the repeat check" >&2
  exit 1
fi
[[ "$second_flap_output" == *"2 ibstat state flaps"* ]]

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
[[ "$ORIGINAL_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]]
[[ "$MUTATED_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]]
[[ "$ORIGINAL_BUNDLE_NAME" != "$MUTATED_BUNDLE_NAME" ]]
