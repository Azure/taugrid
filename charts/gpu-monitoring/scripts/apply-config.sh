#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

namespace="${NAMESPACE:-kube-system}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

kubectl -n "${namespace}" create configmap gpu-monitoring-gpu \
  --from-file=custom-plugin-monitor.json="${root}/deploy/custom-plugin-monitor.json" \
  --from-file=kernel-monitor.json="${root}/deploy/kernel-monitor.json" \
  --from-file=system-log-monitor.json="${root}/deploy/system-log-monitor.json" \
  --from-file=system-stats-monitor.json="${root}/deploy/system-stats-monitor.json" \
  --from-file=known-modules.json="${root}/deploy/guestosconfig/known-modules.json" \
  --from-file=check-nvidia-smi.sh="${root}/scripts/check-nvidia-smi.sh" \
  --from-file=check-nvidia-device-files.sh="${root}/scripts/check-nvidia-device-files.sh" \
  --from-file=check-dcgm-health.sh="${root}/scripts/check-dcgm-health.sh" \
  --from-file=check_gpu_count.sh="${root}/scripts/check_gpu_count.sh" \
  --from-file=check_gpu_nvlink.sh="${root}/scripts/check_gpu_nvlink.sh" \
  --from-file=check_gpu_xid.sh="${root}/scripts/check_gpu_xid.sh" \
  --from-file=check_gpu_ecc.sh="${root}/scripts/check_gpu_ecc.sh" \
  --from-file=check_gpu_ecc_from_sai.sh="${root}/scripts/check_gpu_ecc_from_sai.sh" \
  --from-file=check_ib.sh="${root}/scripts/check_ib.sh" \
  --from-file=check_ib_pkeys.sh="${root}/scripts/check_ib_pkeys.sh" \
  --from-file=check_gpu_vbios.sh="${root}/scripts/check_gpu_vbios.sh" \
  --from-file=check_gpu_throttle.sh="${root}/scripts/check_gpu_throttle.sh" \
  --from-file=check_gpu_driver.sh="${root}/scripts/check_gpu_driver.sh" \
  --from-file=check_gpu_ecc_remap_pending.sh="${root}/scripts/check_gpu_ecc_remap_pending.sh" \
  --from-file=check_gpu_ecc_remap_failure.sh="${root}/scripts/check_gpu_ecc_remap_failure.sh" \
  --from-file=check_gpu_nvlink_b200.sh="${root}/scripts/check_gpu_nvlink_b200.sh" \
  --from-file=check_gpu_xid_always_fail.sh="${root}/scripts/check_gpu_xid_always_fail.sh" \
  --from-file=check_ib_flaps.sh="${root}/scripts/check_ib_flaps.sh" \
  --from-file=check_nvme_mount.sh="${root}/scripts/check_nvme_mount.sh" \
  --from-file=check_temp_imex.sh="${root}/scripts/check_temp_imex.sh" \
  --from-file=dcgmi-wrapper.sh="${root}/scripts/dcgmi-wrapper.sh" \
  --dry-run=client -o yaml | kubectl apply -f -
