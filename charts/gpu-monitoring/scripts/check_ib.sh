#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks only InfiniBand port identity, state, and rate. PKey
# validation is intentionally owned by check_ib_pkeys.sh.
readonly EXPECTED_IB_GBPS="${EXPECTED_IB_GBPS:-400}"
readonly EXPECTED_IB_DEVS="${IB_DEVICES:-}"
readonly SYSFS_ROOT="${SYSFS_ROOT:-/sys}"

if [[ -z "$EXPECTED_IB_DEVS" ]]; then
  echo "No InfiniBand devices configured, skipping link check"
  exit 0
fi

errors=()
for ib_dev in $EXPECTED_IB_DEVS; do
  device="${ib_dev%:*}"
  port="${ib_dev##*:}"
  port_root="$SYSFS_ROOT/class/infiniband/$device/ports/$port"

  if [[ ! -d "$port_root" ]]; then
    errors+=("$ib_dev is missing")
    continue
  fi

  state="$(cut -d: -f2- "$port_root/state" 2>/dev/null | xargs)"
  physical_state="$(cut -d: -f2- "$port_root/phys_state" 2>/dev/null | xargs)"
  rate="$(awk '{print $1}' "$port_root/rate" 2>/dev/null)"

  if [[ "$state" != "ACTIVE" || "$physical_state" != "LinkUp" || "$rate" != "$EXPECTED_IB_GBPS" ]]; then
    errors+=("$ib_dev expected state=ACTIVE physical_state=LinkUp rate=${EXPECTED_IB_GBPS}Gbps; observed state=${state:-unknown} physical_state=${physical_state:-unknown} rate=${rate:-unknown}Gbps")
  fi
done

if (( ${#errors[@]} > 0 )); then
  printf 'InfiniBand link check failed: %s\n' "$(IFS='; '; echo "${errors[*]}")"
  exit 1
fi

echo "InfiniBand devices are ACTIVE, LinkUp, and at the expected rate"
