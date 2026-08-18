#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# This plugin checks if the NVIDIA GPU driver is loaded and healthy.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

readonly NVIDIA_DRIVER_ROOT="${NVIDIA_DRIVER_ROOT:-/proc/driver/nvidia}"
readonly NVIDIA_DRIVER_PARAMS_FILE="${NVIDIA_DRIVER_PARAMS_FILE:-${NVIDIA_DRIVER_ROOT}/params}"
readonly CREATE_IMEX_CHANNEL_EXPECTED="${CREATE_IMEX_CHANNEL_EXPECTED:-}"

case "$CREATE_IMEX_CHANNEL_EXPECTED" in
  "" | "0" | "1") ;;
  *)
    echo "'CREATE_IMEX_CHANNEL_EXPECTED' must be empty, 0, or 1; got '$CREATE_IMEX_CHANNEL_EXPECTED'"
    exit $NONOK
    ;;
esac

if ! command -v nvidia-smi &>/dev/null; then
  echo "'nvidia-smi' not found."
  exit $NONOK
fi

if [ ! -d "$NVIDIA_DRIVER_ROOT" ]; then
  echo "'nvidia' driver not loaded"
  exit $NONOK
fi

imex_value=$(grep '^CreateImexChannel0' "$NVIDIA_DRIVER_PARAMS_FILE" 2>/dev/null | awk '{print $2}')
if [ -z "$imex_value" ]; then
  if [ -z "$CREATE_IMEX_CHANNEL_EXPECTED" ]; then
    echo "'nvidia' driver is loaded; CreateImexChannel0 is not exposed by this driver."
    exit $OK
  fi
  echo "'nvidia' driver is loaded, but CreateImexChannel0 was not found in '$NVIDIA_DRIVER_PARAMS_FILE' (expected $CREATE_IMEX_CHANNEL_EXPECTED)"
  exit $NONOK
fi

imex_expected="${CREATE_IMEX_CHANNEL_EXPECTED:-1}"
if [ "$imex_value" != "$imex_expected" ]; then
  echo "'CreateImexChannel0' is $imex_value (expected $imex_expected for this profile)"
  exit $NONOK
fi

if [ -z "$CREATE_IMEX_CHANNEL_EXPECTED" ]; then
  echo "'nvidia' driver is loaded; CreateImexChannel0 is 1 as required by the legacy contract."
else
  echo "'nvidia' driver is loaded; CreateImexChannel0 is $imex_value as expected."
fi
exit $OK
