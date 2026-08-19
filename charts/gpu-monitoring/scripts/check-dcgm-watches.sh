#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if [[ "${NPD_DCGM_REQUIRED:-0}" != "1" ]]; then
  echo "dcgm health watches not required for this profile"
  exit 0
fi

if ! command -v dcgmi >/dev/null 2>&1; then
  echo "dcgmi not found"
  exit 1
fi

if output="$(LC_ALL=C dcgmi health -f 2>&1)"; then
  :
else
  fetch_rc=$?
  [[ -n "${output}" ]] && echo "${output}"
  echo "failed to fetch dcgm health watches with return code ${fetch_rc}"
  exit 1
fi

if ! printf '%s\n' "${output}" | awk -F '|' '
function trim(value) {
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
  return value
}
BEGIN {
  required["PCIe"] = 1
  required["NVLINK"] = 1
  required["Memory"] = 1
  required["InfoROM"] = 1
  required["Thermal"] = 1
  required["Power"] = 1
  required["Driver"] = 1
  required["NvSwitch NF"] = 1
  required["NvSwitch F"] = 1
}
NF >= 3 {
  watch = trim($2)
  state = trim($3)
  if ((watch in required) && state == "On") {
    enabled[watch] = 1
  }
}
END {
  for (watch in required) {
    if (!(watch in enabled)) {
      exit 1
    }
  }
}'; then
  [[ -n "${output}" ]] && echo "${output}"
  echo "dcgm health watches are not fully enabled"
  exit 1
fi

echo "dcgm health watches enabled"
