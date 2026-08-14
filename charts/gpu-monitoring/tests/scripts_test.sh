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
  "nvlink --status"|"nvlink --id=0 --status"|"c2c --id=0 --status")
    echo "Link 0: 50 GB/s"
    ;;
  "topo -m")
    echo "NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18 NV18"
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

PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 EXPECTED_NUM_GPU=1 \
  bash "$CHART_DIR/scripts/check_gpu_nvlink_b200.sh"
PATH="$TEST_ROOT/bin:$PATH" GPU_TYPE=GB300 \
  bash "$CHART_DIR/scripts/check_temp_imex.sh"

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

printf '800 Gb/sec (8X NDR)\n' >"$PORT_ROOT/rate"
SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
  bash "$CHART_DIR/scripts/check_ib.sh"

printf '400 Gb/sec (4X HDR)\n' >"$PORT_ROOT/rate"
if mismatch_output="$(
  SYSFS_ROOT="$TEST_ROOT/sys" IB_DEVICES="mlx5_0:1" EXPECTED_IB_GBPS=800 \
    bash "$CHART_DIR/scripts/check_ib.sh" 2>&1
)"; then
  echo "expected the 800-Gbps check to reject a 400-Gbps link" >&2
  exit 1
fi
[[ "$mismatch_output" == *"800 Gb/sec"* ]]
