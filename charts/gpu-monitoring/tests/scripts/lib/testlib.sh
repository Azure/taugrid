#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TESTLIB_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly CHART_DIR="$(cd -- "${TESTLIB_DIR}/../../.." && pwd)"

TEST_CASE="${TEST_CASE:-unnamed}"
TEST_ROOT=""
MOCK_BIN=""
RUN_OUTPUT=""
RUN_STATUS=0

fail() {
  echo "${TEST_CASE}: assertion failed: $*" >&2
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

assert_status() {
  [ "$RUN_STATUS" -eq "$2" ] ||
    fail "$1: expected exit $2, got $RUN_STATUS: $RUN_OUTPUT"
}

cleanup_test_case() {
  if [[ -n "$TEST_ROOT" && -d "$TEST_ROOT" ]]; then
    rm -rf "$TEST_ROOT"
  fi
}

setup_test_case() {
  TEST_CASE="$1"
  TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/gpu-monitoring-${TEST_CASE}.XXXXXX")"
  MOCK_BIN="${TEST_ROOT}/bin"
  mkdir -p "$MOCK_BIN"
  trap cleanup_test_case EXIT
  install_common_mocks
  PATH="${MOCK_BIN}:${PATH}"
  export PATH
}

run_check() {
  set +e
  RUN_OUTPUT="$(env "$@" 2>&1)"
  RUN_STATUS=$?
  set -e
}

create_ib_fabric() {
  local root="$1"
  local device_prefix="$2"
  local rate="$3"
  local pkey="$4"
  local devices=""
  local index
  local device
  local port_root

  for index in {0..7}; do
    device="${device_prefix}${index}"
    port_root="$root/class/infiniband/$device/ports/1"
    mkdir -p "$port_root/pkeys"
    printf '4: ACTIVE\n' >"$port_root/state"
    printf '5: LinkUp\n' >"$port_root/phys_state"
    printf '%s Gb/sec\n' "$rate" >"$port_root/rate"
    printf '%s\n' "$pkey" >"$port_root/pkeys/0"
    devices+="${devices:+ }${device}:1"
  done

  printf '%s\n' "$devices"
}

install_common_mocks() {
  cat >"${MOCK_BIN}/nvidia-smi" <<'EOF'
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

  cat >"${MOCK_BIN}/pgrep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

  cat >"${MOCK_BIN}/dcgmi" <<'EOF'
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

  cat >"${MOCK_BIN}/nsenter" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >"$NSENTER_ARGS_FILE"
EOF

  cat >"${MOCK_BIN}/journalctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${JOURNALCTL_OUTPUT:-}"
exit "${JOURNALCTL_EXIT_CODE:-0}"
EOF

  cat >"${MOCK_BIN}/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "+%s" && -n "${TEST_NOW:-}" ]]; then
  echo "$TEST_NOW"
  exit 0
fi
exec /bin/date "$@"
EOF

  cat >"${MOCK_BIN}/ibstat" <<'EOF'
#!/usr/bin/env bash
cat <<'OUTPUT'
CA 'mlx5_0'
  Port 1:
    State: Active
OUTPUT
EOF

  cat >"${MOCK_BIN}/timeout" <<'EOF'
#!/usr/bin/env bash
shift
exec "$@"
EOF

  chmod +x "${MOCK_BIN}"/*
}
