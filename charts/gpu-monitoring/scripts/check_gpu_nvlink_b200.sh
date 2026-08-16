#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks if is the GPU NVlink is working correctly.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

readonly _NUM_GPU_ENV="${EXPECTED_NUM_GPU:-0}"
if [ "$_NUM_GPU_ENV" -eq 0 ]; then
  if gpu_count_output=$(nvidia-smi --query-gpu=count --format=csv,noheader 2>&1); then
    readonly _NUM_GPU=$(printf '%s\n' "$gpu_count_output" | head -1)
  else
    gpu_count_rc=$?
    echo "Failed to get GPU count with error code $gpu_count_rc. FaultCode: NHC2016"
    exit $NONOK
  fi
else
  readonly _NUM_GPU=$_NUM_GPU_ENV
fi
readonly EXPECTED_NVLINK_COUNT=12

# Detect GPU type — prefer env var, fallback to nvidia-smi
if [ -z "${GPU_TYPE:-}" ]; then
  if gpu_name_output=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>&1); then
    GPU_TYPE=$(printf '%s\n' "$gpu_name_output" | head -1)
  else
    gpu_name_rc=$?
    echo "Failed to get GPU type with error code $gpu_name_rc. FaultCode: NHC2016"
    exit $NONOK
  fi
fi
case "$GPU_TYPE" in
  *GB200*|*GB300*) ;;
  *)
    echo "Not a Blackwell node, skipping NVLink check"
    exit $OK
    ;;
esac



# Check if nvlink is enabled
if nvlink_status=$(nvidia-smi nvlink --status 2>&1); then
  :
else
  nvlink_rc=$?
  echo "Failed to get NVLINK status with error code $nvlink_rc. FaultCode: NHC2016"
  exit $NONOK
fi
if [ -z "$nvlink_status" ]; then
  echo "NVLINK is not enabled"
  exit $NONOK
fi


# Check the number of nvlinks
if topo_output=$(nvidia-smi topo -m 2>&1); then
  NVL_COUNT=$(printf '%s\n' "$topo_output" | grep -o 'NV18' | wc -l | tr -d '[:space:]')
else
  topo_rc=$?
  echo "Failed to get NVLINK topology with error code $topo_rc. FaultCode: NHC2016"
  exit $NONOK
fi
if [[ $NVL_COUNT -ne $EXPECTED_NVLINK_COUNT ]]; then
  echo "NVLINK is not enabled. Expected $EXPECTED_NVLINK_COUNT links, found $NVL_COUNT"
  exit $NONOK
fi

# Check effective bandwidth of all nvlinks
for ((gpu_id=0; gpu_id<_NUM_GPU; gpu_id++)); do

  if nvl_output=$(nvidia-smi nvlink --id="$gpu_id" --status 2>&1); then
    :
  else
    nvl_rc=$?
    echo "Failed to get NVLINK status for GPU $gpu_id with error code $nvl_rc. FaultCode: NHC2016"
    exit $NONOK
  fi
  if [ -z "$nvl_output" ]; then
    echo "NVLINK is not enabled for GPU $gpu_id. FaultCode: NHC2016"
    exit $NONOK
  fi
  NVL_BAD=$(printf '%s\n' "$nvl_output" | grep Link | grep -v 'GB/s' | paste -sd, - | tr -d '[:space:]')
  if [ -n "$NVL_BAD" ]; then
    echo "NVLINK is down"
    exit $NONOK
  fi

  if nvlc2c_output=$(nvidia-smi c2c --id="$gpu_id" --status 2>&1); then
    :
  else
    nvlc2c_rc=$?
    echo "Failed to get NVLINK C2C status for GPU $gpu_id with error code $nvlc2c_rc. FaultCode: NHC2016"
    exit $NONOK
  fi
  if [ -z "$nvlc2c_output" ]; then
    echo "NVLINK C2C is not enabled for GPU $gpu_id. FaultCode: NHC2016"
    exit $NONOK
  fi
  NVLC2C_BAD=$(printf '%s\n' "$nvlc2c_output" | grep Link | grep -v 'GB/s' | paste -sd, - | tr -d '[:space:]')
  if [ -n "$NVLC2C_BAD" ]; then
    echo "NVLINK C2C is down"
    exit $NONOK
  fi
done

exit $OK
