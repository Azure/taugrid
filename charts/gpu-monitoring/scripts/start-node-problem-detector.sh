#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly NPD_BINARY="${NPD_BINARY:-/node-problem-detector}"

if [[ "${NPD_DCGM_REQUIRED:-0}" == "1" ]]; then
  if ! bash "${SCRIPT_DIR}/check-dcgm-watches.sh"; then
    echo "dcgm health watches unavailable; reinitializing before NPD startup"
    bash "${SCRIPT_DIR}/init-dcgm-health.sh"
  fi
fi

exec "${NPD_BINARY}" "$@"
