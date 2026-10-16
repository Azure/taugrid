#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

readonly EXPECTED_NUM_GPU="${EXPECTED_NUM_GPU:-0}"

if ! command -v nvidia-smi &>/dev/null; then
  echo "nvidia-smi not found"
  exit $UNKNOWN
fi

gpu_output=$(nvidia-smi --list-gpus 2>&1)
smi_rc=$?
if [ $smi_rc -ne 0 ]; then
  echo "nvidia-smi --list-gpus failed (rc=$smi_rc): $gpu_output"
  exit $UNKNOWN
fi
gpu_count=$(printf '%s\n' "$gpu_output" | grep -c '^GPU ')

if [ "$EXPECTED_NUM_GPU" -eq 0 ]; then
  if [ "$gpu_count" -gt 0 ]; then
    echo "Auto-detected $gpu_count GPU(s)"
    exit $OK
  else
    echo "No GPUs detected (expected at least 1). FaultCode: NHC2009"
    exit $NONOK
  fi
fi

if [ "$gpu_count" -ne "$EXPECTED_NUM_GPU" ]; then
  echo "Expected to see $EXPECTED_NUM_GPU but found $gpu_count. FaultCode: NHC2009"
  exit $NONOK
else
  echo "Expected $EXPECTED_NUM_GPU and found $gpu_count"
  exit $OK
fi
