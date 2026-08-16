#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# This plugin checks if the NVIDIA GPU driver is loaded and healthy.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

if ! command -v nvidia-smi &>/dev/null; then
  echo "'nvidia-smi' not found."
  exit $NONOK
fi

if [ ! -e /proc/driver/nvidia ]; then
  echo "'nvidia' driver not loaded"
  exit $NONOK
fi

# Check IMEX channel support (required for GPU Direct RDMA / multi-node GPU comms)
imex_value=$(grep CreateImexChannel0 /proc/driver/nvidia/params 2>/dev/null | awk '{print $2}')
if [ -z "$imex_value" ]; then
  echo "'nvidia' driver loaded, CreateImexChannel0 parameter not found (older driver or unsupported SKU)"
  exit $OK
elif [ "$imex_value" != "1" ]; then
  echo "'CreateImexChannel0' is $imex_value (expected 1) — IMEX channels not enabled, GPU Direct RDMA may not function"
  exit $NONOK
fi

echo "'nvidia' driver is loaded, IMEX channels enabled."
exit $OK
