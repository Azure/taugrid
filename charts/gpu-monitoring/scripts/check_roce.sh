#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks RoCE (RDMA over Converged Ethernet) link status.
# Expects ROCE_DEVICES env var as a space-separated list of device:port pairs
# that should be ACTIVE, e.g. "roceP2p1s0f0:1 rocep1s0f0:1"

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

readonly EXPECTED_ROCE_DEVS="${ROCE_DEVICES:-}"

if [[ -z "$EXPECTED_ROCE_DEVS" ]]; then
  echo "No RoCE devices configured, skipping check"
  exit $OK
fi

readonly INFINIBAND_PATH="/sys/class/infiniband"

if [[ ! -d "$INFINIBAND_PATH" ]]; then
  echo "No RDMA devices found at $INFINIBAND_PATH"
  exit $NONOK
fi

errors=()
for roce_dev in $EXPECTED_ROCE_DEVS; do
  device="${roce_dev%%:*}"
  port="${roce_dev##*:}"

  state_file="$INFINIBAND_PATH/$device/ports/$port/state"
  phys_file="$INFINIBAND_PATH/$device/ports/$port/phys_state"
  link_file="$INFINIBAND_PATH/$device/ports/$port/link_layer"

  if [[ ! -f "$state_file" ]]; then
    errors+=("RoCE device $device port $port not found")
    continue
  fi

  link_layer=$(cat "$link_file" 2>/dev/null)
  if [[ "$link_layer" != "Ethernet" ]]; then
    errors+=("$device/$port link_layer is '$link_layer', expected 'Ethernet'")
    continue
  fi

  state=$(cat "$state_file" 2>/dev/null)
  if [[ "$state" != *"ACTIVE"* ]]; then
    phys=$(cat "$phys_file" 2>/dev/null)
    errors+=("$device/$port state=$state phys=$phys, expected ACTIVE")
  fi
done

if [[ ${#errors[@]} -gt 0 ]]; then
  echo "${errors[*]}"
  exit $NONOK
fi

echo "All expected RoCE devices are ACTIVE"
exit $OK
