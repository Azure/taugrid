#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks only the configured InfiniBand PKey. Link identity, state,
# and rate validation is intentionally owned by check_ib.sh.
readonly EXPECTED_IB_DEVS="${IB_DEVICES:-}"
readonly EXPECTED_IB_PKEY="${EXPECTED_IB_PKEY:-}"
readonly SYSFS_ROOT="${SYSFS_ROOT:-/sys}"

if [[ -z "$EXPECTED_IB_DEVS" ]]; then
  echo "No InfiniBand devices configured, skipping PKey check"
  exit 0
fi

if [[ ! "$EXPECTED_IB_PKEY" =~ ^0x[0-9a-fA-F]{4}$ ]]; then
  echo "EXPECTED_IB_PKEY must be an explicit hexadecimal PKey such as 0xffff; got '${EXPECTED_IB_PKEY:-unset}'"
  exit 1
fi

normalized_expected_pkey="$(printf '%s' "$EXPECTED_IB_PKEY" | tr '[:upper:]' '[:lower:]')"
errors=()
for ib_dev in $EXPECTED_IB_DEVS; do
  device="${ib_dev%:*}"
  port="${ib_dev##*:}"
  pkey_file="$SYSFS_ROOT/class/infiniband/$device/ports/$port/pkeys/0"

  if [[ ! -f "$pkey_file" ]]; then
    errors+=("$ib_dev PKey file is missing")
    continue
  fi

  observed_pkey="$(tr '[:upper:]' '[:lower:]' < "$pkey_file" | xargs)"
  if [[ "$observed_pkey" != "$normalized_expected_pkey" ]]; then
    errors+=("$ib_dev expected PKey $normalized_expected_pkey; observed ${observed_pkey:-unknown}")
  fi
done

if (( ${#errors[@]} > 0 )); then
  printf 'InfiniBand PKey check failed: %s\n' "$(IFS='; '; echo "${errors[*]}")"
  exit 1
fi

echo "InfiniBand devices have expected PKey $normalized_expected_pkey"
