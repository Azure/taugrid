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
readonly EXPECTED_NVLINK_COUNT=12

# Detect GPU type — prefer env var, fallback to nvidia-smi
GPU_TYPE="${GPU_TYPE:-$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)}"
case "$GPU_TYPE" in
  *GB200*|*GB300*) ;;
  *)
    echo "Not a Blackwell node, skipping NVLink check"
    exit $OK
    ;;
esac



# Check if nvlink is enabled
nvlink_status=$(nvidia-smi nvlink --status)
if [ $? -ne 0 ]; then
  echo "Failed to get NVLINK status with error code $?. FaultCode: NHC2016"
  exit $NONOK
fi
if [ -z "$nvlink_status" ]; then
  echo "NVLINK is not enabled"
  exit $OK
fi


# Check the number of nvlinks
NVL_COUNT=$(nvidia-smi topo -m | egrep -o 'NV18' | wc -l)
if [[ $NVL_COUNT -ne $EXPECTED_NVLINK_COUNT ]]; then
  echo "NVLINK is not enabled. Expected $EXPECTED_NVLINK_COUNT links, found $NVL_COUNT"
  exit $NONOK
fi

# Check effective bandwidth of all nvlinks
for ((gpu_id=0; gpu_id<_NUM_GPU; gpu_id++)); do

  NVL_BAD=$(nvidia-smi nvlink --id=$gpu_id --status | grep Link | grep -v 'GB/s' | paste -sd, - | tr -d '[:space:]')
  if [ -n "$NVL_BAD" ]; then
    echo "NVLINK is down"
    exit $NONOK
  fi

  NVLC2C_BAD=$(nvidia-smi c2c --id=$gpu_id --status | grep Link | grep -v 'GB/s' | paste -sd, - | tr -d '[:space:]')
  if [ -n "$NVLC2C_BAD" ]; then
    echo "NVLINK C2C is down"
    exit $NONOK
  fi
done

exit $OK
