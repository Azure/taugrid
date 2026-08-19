#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if ! command -v nvidia-smi >/dev/null 2>&1; then
  if [[ "${NPD_GPU_REQUIRED:-}" == "1" ]]; then
    echo "nvidia-smi not found"
    exit 1
  fi
  echo "nvidia-smi not found; gpu not required"
  exit 0
fi

output="$(nvidia-smi -L)"
status=$?
if [[ ${status} -ne 0 ]]; then
  echo "nvidia-smi failed"
  exit 1
fi

if [[ -z "${output}" ]]; then
  echo "nvidia-smi returned no output"
  exit 1
fi

echo "${output}"
