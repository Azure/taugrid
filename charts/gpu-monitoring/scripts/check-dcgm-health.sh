#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly OK=0
readonly NONOK=1

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

if [[ "${NPD_DCGM_REQUIRED:-0}" != "1" ]]; then
  echo "dcgm health check not required for this profile"
  exit 0
fi

if ! command -v dcgmi >/dev/null 2>&1; then
  echo "dcgmi not found"
  exit 1
fi

if ! dcgmi health -c >/dev/null 2>&1; then
  echo "dcgmi health configuration failed"
  exit 1
fi

if output="$(dcgmi health -t 2>&1)"; then
  health_rc=$OK
else
  health_rc=$?
fi
if [[ $health_rc -ne $OK ]]; then
  [[ -n "${output}" ]] && echo "${output}"
  echo "dcgmi health check failed with return code ${health_rc}"
  exit $NONOK
fi
if [[ -z "${output}" ]]; then
  echo "dcgmi health check failed"
  exit $NONOK
fi

echo "${output}"
