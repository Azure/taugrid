#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly WARMUP_SECONDS="${NPD_DCGM_WARMUP_SECONDS:-60}"
readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ "${NPD_DCGM_REQUIRED:-0}" != "1" ]]; then
  echo "dcgm health watches not required for this profile"
  exit 0
fi

if ! command -v dcgmi >/dev/null 2>&1; then
  echo "dcgmi not found"
  exit 1
fi

if output="$(dcgmi health -s a 2>&1)"; then
  :
else
  set_rc=$?
  [[ -n "${output}" ]] && echo "${output}"
  echo "failed to enable dcgm health watches with return code ${set_rc}"
  exit 1
fi

bash "${SCRIPT_DIR}/check-dcgm-watches.sh"

echo "enabled dcgm health watches; waiting ${WARMUP_SECONDS}s for sampled systems"
sleep "${WARMUP_SECONDS}"

if output="$(dcgmi health -c 2>&1)"; then
  :
else
  check_rc=$?
  [[ -n "${output}" ]] && echo "${output}"
  echo "dcgm health watches unavailable after warmup (return code ${check_rc})"
  exit 1
fi

bash "${SCRIPT_DIR}/check-dcgm-watches.sh"

[[ -n "${output}" ]] && echo "${output}"
echo "dcgm health watches initialized"
