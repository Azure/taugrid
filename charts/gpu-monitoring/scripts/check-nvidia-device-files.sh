#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if [[ "${NPD_GPU_REQUIRED:-}" == "1" ]]; then
  if [[ -e /dev/nvidiactl || -e /dev/nvidia0 ]]; then
    echo "nvidia device files present"
    exit 0
  fi
  echo "nvidia device files missing"
  exit 1
fi

echo "nvidia device files not required"
