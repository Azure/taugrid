#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_ROOT/bin"

# macOS ships bash 3.2, where `set -e` does not abort on a failing `[[ ]]`
# compound command. Bare `[[ ]]` assertions are therefore silent no-ops on a
# developer laptop even though they fire on the Linux CI runner. Assert through
# these helpers so every assertion fails loudly on every supported bash.
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
assert_contains mismatch_output "$mismatch_output" "800 Gb/sec"

# A link-only failure must stay silent about PKeys, so IBLinkIssue and
# IBPKeyIssue remain independently actionable.
assert_not_contains mismatch_output "$mismatch_output" "PKey"
assert_not_contains mismatch_output "$mismatch_output" "0xffff"

# The PKey check owns PKey membership alone: it must not fail a link fault that
# check_ib.sh already reports.
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
  bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

printf '400 Gb/sec (4X HDR)\n' >"$PORT_ROOT/rate"

# A configured PKey that matches the fabric passes.
printf '0x8001\n' >"$PORT_ROOT/pkeys/0"
pkey_ok_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x8001 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains pkey_ok_output "$pkey_ok_output" "IB PKeys are ok"

# A configured PKey that does not match fails, and reports both sides.
if pkey_mismatch_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x8003 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  echo "expected a 0x8003 PKey expectation to reject a 0x8001 port" >&2
  exit 1
fi
assert_contains pkey_mismatch_output "$pkey_mismatch_output" "expected 0x8003"
assert_contains pkey_mismatch_output "$pkey_mismatch_output" "observed 0x8001"
assert_contains pkey_mismatch_output "$pkey_mismatch_output" "mlx5_0:1"

# A PKey-only failure must not blame link state, which is what made the
# original conflated check page operators on the wrong subsystem.
assert_not_contains pkey_mismatch_output "$pkey_mismatch_output" "is ACTIVE"
assert_not_contains pkey_mismatch_output "$pkey_mismatch_output" "LinkUp"
assert_not_contains pkey_mismatch_output "$pkey_mismatch_output" "Gb/sec"

# A healthy link with an unexpected PKey is still a healthy link.
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
  bash "$CHART_DIR/scripts/check_ib.sh"

# Without IB_PKEY the historical derivation must hold: a single-device profile
# expects 0xffff, so a 0x8001 port fails.
if pkey_default_single_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  echo "expected the default single-device PKey expectation to stay 0xffff" >&2
  exit 1
fi
assert_contains pkey_default_single_output "$pkey_default_single_output" "expected 0xffff"

# ...and an 8-device profile expects 0x8003.
if pkey_default_eight_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" \
    IB_DEVICES="mlx5_0:1 mlx5_1:1 mlx5_2:1 mlx5_3:1 mlx5_4:1 mlx5_5:1 mlx5_6:1 mlx5_7:1" \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  echo "expected the default 8-device PKey expectation to stay 0x8003" >&2
  exit 1
fi
assert_contains pkey_default_eight_output "$pkey_default_eight_output" "expected 0x8003"

# An expected device that is absent from sysfs is a link fault, reported once by
# check_ib.sh; the PKey check must not double-report it.
printf '0xffff\n' >"$PORT_ROOT/pkeys/0"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_9:1" \
  bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
if absent_dev_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_9:1" \
    bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  echo "expected an absent IB device to fail the link check" >&2
  exit 1
fi
assert_contains absent_dev_output "$absent_dev_output" "mlx5_9:1"

# A genuinely down link fails the link check without mentioning PKeys.
printf '1: DOWN\n' >"$PORT_ROOT/state"
printf '3: Disabled\n' >"$PORT_ROOT/phys_state"
if link_down_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
    bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  echo "expected a down IB link to fail the link check" >&2
  exit 1
fi
assert_contains link_down_output "$link_down_output" "is ACTIVE"
assert_not_contains link_down_output "$link_down_output" "PKey"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" \
  bash "$CHART_DIR/scripts/check_ib_pkeys.sh"

printf '4: ACTIVE\n' >"$PORT_ROOT/state"
printf '5: LinkUp\n' >"$PORT_ROOT/phys_state"

# The kernel prints port PKeys as "0x%04x", so operator-supplied casing and
# zero-padding must be canonicalized before comparison. Without this an
# uppercase or short PKey would mismatch on every node.
printf '0xffff\n' >"$PORT_ROOT/pkeys/0"
pkey_upper_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0xFFFF \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains pkey_upper_output "$pkey_upper_output" "IB PKeys are ok"

printf '0x0001\n' >"$PORT_ROOT/pkeys/0"
pkey_short_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x1 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains pkey_short_output "$pkey_short_output" "IB PKeys are ok"

# A mismatch reports both sides in canonical form.
if pkey_canonical_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x8001 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh" 2>&1
)"; then
  echo "expected a 0x8001 expectation to reject a 0x0001 port" >&2
  exit 1
fi
assert_contains pkey_canonical_output "$pkey_canonical_output" "expected 0x8001"
assert_contains pkey_canonical_output "$pkey_canonical_output" "observed 0x0001"

# Defensive: the kernel emits "0x%04x", but normalize the observed side too so a
# non-canonical sysfs value cannot fail a correctly-configured fleet.
printf '0XFFFF\n' >"$PORT_ROOT/pkeys/0"
pkey_sysfs_upper_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0xffff \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains pkey_sysfs_upper_output "$pkey_sysfs_upper_output" "IB PKeys are ok"

printf '0x1\n' >"$PORT_ROOT/pkeys/0"
pkey_sysfs_short_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x0001 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains pkey_sysfs_short_output "$pkey_sysfs_short_output" "IB PKeys are ok"

# A port whose link is down has no authoritative partition table, and
# check_ib.sh already reports it. The PKey check must stay silent so one fault
# does not light up two conditions.
printf '0x0000\n' >"$PORT_ROOT/pkeys/0"
printf '1: DOWN\n' >"$PORT_ROOT/state"
printf '3: Disabled\n' >"$PORT_ROOT/phys_state"
down_port_pkey_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x8001 \
    bash "$CHART_DIR/scripts/check_ib_pkeys.sh"
)"
assert_contains down_port_pkey_output "$down_port_pkey_output" "IB PKeys are ok"
if down_port_link_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" IB_PKEY=0x8001 \
    bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  echo "expected a down link to fail the link check" >&2
  exit 1
fi
assert_contains down_port_link_output "$down_port_link_output" "is ACTIVE"

printf '4: ACTIVE\n' >"$PORT_ROOT/state"
printf '5: LinkUp\n' >"$PORT_ROOT/phys_state"
printf '0xffff\n' >"$PORT_ROOT/pkeys/0"

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
[[ "$(<"$NSENTER_LOG")" == "--target 1 --mount -- dcgmi health -t" ]] || fail "nsenter args: got $(<"$NSENTER_LOG")"

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
grep -q '^6300 ' "$FLAP_STATE_FILE" && fail "stale 6300 flap entry outside the window was not pruned"
grep -q '^6500 ' "$FLAP_STATE_FILE"
[[ "$(wc -l < "$FLAP_STATE_FILE" | tr -d ' ')" -gt 10 ]] || fail "flap state file was truncated too aggressively"
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
[[ "$ORIGINAL_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] || fail "unexpected bundle name: $ORIGINAL_BUNDLE_NAME"
[[ "$MUTATED_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] || fail "unexpected bundle name: $MUTATED_BUNDLE_NAME"
[[ "$ORIGINAL_BUNDLE_NAME" != "$MUTATED_BUNDLE_NAME" ]] || fail "bundle name did not change when a script changed"
