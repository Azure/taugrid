#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

case "${NPD_DCGM_SIMULATION:-}" in
  healthy)
    echo "dcgm health ok"
    exit 0
    ;;
  unhealthy)
    echo "DCGM health check failed: XID error"
    exit 1
    ;;
  missing-driver)
    echo "DCGM error: driver not loaded"
    exit 1
    ;;
  "")
    ;;
  *)
    echo "Unknown NPD_DCGM_SIMULATION value: ${NPD_DCGM_SIMULATION}"
    exit 2
    ;;
esac

if ! command -v dcgmi >/dev/null 2>&1; then
  if [[ "${NPD_DCGM_REQUIRED:-}" == "1" ]]; then
    echo "dcgmi not found"
    exit 1
  fi
  echo "dcgmi not found; dcgm not required"
  exit 0
fi

if ! dcgmi health -c >/dev/null 2>&1; then
  echo "dcgmi health configuration failed"
  exit 1
fi

output="$(dcgmi health -t 2>/dev/null || true)"
if [[ -z "${output}" ]]; then
  echo "dcgmi health check failed"
  exit 1
fi

echo "${output}"
