#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks if is the GPU NVlink is working correctly.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

readonly _NUM_GPU_ENV="${EXPECTED_NUM_GPU:-0}"
if [ "$_NUM_GPU_ENV" -eq 0 ]; then
  readonly _NUM_GPU=$(nvidia-smi --query-gpu=count --format=csv,noheader 2>/dev/null | head -1)
else
  readonly _NUM_GPU=$_NUM_GPU_ENV
fi

# H100 NVL: links 4,5,10,11,16,17 are inter-socket and normally inactive
# when only 2 GPUs are present on a single socket.
readonly GPU_TYPE_VAL="${GPU_TYPE:-}"
expected_inactive_pattern=""
if [ "$GPU_TYPE_VAL" = "h100-nvl" ]; then
  expected_inactive_pattern="^Link (4|5|10|11|16|17):"
fi

# Check if nvlink is enabled
num_gpus=$_NUM_GPU

nvlink_status=$(nvidia-smi nvlink --status)
if [ $? -ne 0 ]; then
  echo "Failed to get NVLINK status with error code $?. FaultCode: NHC2016"
  exit $NONOK
fi
if [ -z "$nvlink_status" ]; then
  echo "NVLINK is not enabled"
  exit $OK
fi
for ((i=0; i<num_gpus; i++)); do
    gpu_id=$i
    # Run nvlink command
    nvlink_output=$(nvidia-smi nvlink -s -i $gpu_id)
    if [ $? -ne 0 ]; then
      echo "Failed to get NVLINK status with error code $?. FaultCode: NHC2016"
      exit $NONOK
    fi
    # Check for inactive links
    if [[ $nvlink_output == *"inactive"* ]]; then
      # Filter out expected-inactive links for specific GPU types
      if [ -n "$expected_inactive_pattern" ]; then
        unexpected_inactive=$(echo "$nvlink_output" | grep "<inactive>" | grep -Ev "$expected_inactive_pattern")
      else
        unexpected_inactive=$(echo "$nvlink_output" | grep "<inactive>")
      fi
      if [ -n "$unexpected_inactive" ]; then
        inactive_links=$(echo "$unexpected_inactive" | sed 's/Link \([0-9]*\): <inactive>/Link \1: Inactive/')
        echo "GPU $gpu_id has nvlinks inactive: $inactive_links. FaultCode: NHC2016"
        exit $NONOK
      fi
    elif [[ $nvlink_output == *"all links are inActive"* ]]; then
        echo "GPU $gpu_id has all nvlinks inactive"
        exit $NONOK
    fi
done

echo "All GPUs have nvlinks active"
exit $OK
