#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks IB partition-key membership. Copied from Azure HPC again
# but with many bug fixes.
#
# This check asserts PKey membership only. Link state, physical state, and rate
# are asserted separately by check_ib.sh so that a link fault and a PKey
# misconfiguration surface as independent, correctly-attributed conditions.
readonly EXPECTED_IB_DEVS="${IB_DEVICES:-}"
readonly EXPECTED_IB_NUM_DEVS=`grep -o ":" <<<"$EXPECTED_IB_DEVS" | wc -l`
readonly SYSFS_ROOT="${SYSFS_ROOT:-/sys}"

# IB_PKEY is the per-SKU expected tenant PKey. When it is unset the historical
# derivation applies: 8-device SKUs expect the Mango PDX tenant PKey and every
# other SKU expects the default partition.
if [[ -n "${IB_PKEY:-}" ]]; then
  configured_pkey="$IB_PKEY"
elif [[ $EXPECTED_IB_NUM_DEVS -eq 8 ]]; then
  configured_pkey="0x8003"
else
  configured_pkey="0xffff"
fi

# The kernel emits port PKeys as "0x%04x", so compare canonical forms. Without
# this, an operator-supplied "0xFFFF" or "0x1" would never match the sysfs value
# and would fail the check on every node.
function normalize_pkey() {
    local value="$1"
    value="${value#0x}"
    value="${value#0X}"
    if [[ ! "$value" =~ ^[0-9a-fA-F]{1,4}$ ]]; then
      # Pass unrecognized values through so the failure message shows what was
      # actually configured.
      echo "$1"
      return
    fi
    printf '0x%04x' "$((16#$value))"
}

readonly EXPECTED_IB_PKEY="$(normalize_pkey "$configured_pkey")"

HW_IB_STATE=( )
HW_IB_PHYS_STATE=()
HW_IB_RATE=( )
HW_IB_DEV=()
HW_IB_PKEY=()

function gather_ib_data() {
    local IFS LINE CORES SIBLINGS MHZ PROCESSOR PHYS_ID PORT INDEX DEV
    local -a FIELD PHYS_IDS IB_PORTS


    # Gather IB info
    set +f
    IFS=''
    IB_PORTS=( "$SYSFS_ROOT"/class/infiniband/*/ports/* )
    IFS=$' \t\n'
    set -f
    for PORT in "${IB_PORTS[@]}" ; do
        test -e "$PORT" || break
        INDEX=${#HW_IB_STATE[*]}
        IFS=' :'
        read LINE < $PORT/state
        FIELD=( $LINE )
        HW_IB_STATE[$INDEX]=${FIELD[1]}
        read LINE < $PORT/phys_state
        FIELD=( $LINE )
        HW_IB_PHYS_STATE[$INDEX]=${FIELD[1]}
        read LINE < $PORT/rate
        FIELD=( $LINE )
        HW_IB_RATE[$INDEX]=${FIELD[0]}
        read LINE < $PORT/pkeys/0
        FIELD=( $LINE )
        HW_IB_PKEY[$INDEX]=${FIELD[0]}
        HW_IB_DEV[$INDEX]="$(basename "$(dirname "$(dirname "$PORT")")"):$(basename "$PORT")"
        IFS=$' \t\n'
	 #echo "Found ${HW_IB_STATE[$INDEX]} (${HW_IB_PHYS_STATE[$INDEX]}) IB Port ${HW_IB_DEV[$INDEX]} (${HW_IB_RATE[$INDEX]} Gb/sec) with (${HW_IB_PKEY[$INDEX]}) PKEY"
    done
    export HW_IB_STATE HW_IB_PHYS_STATE HW_IB_RATE HW_IB_PKEY

    # Check if user-leved mad driver loaded and IB diag tools will succeed to run
    if [[ -f "$SYSFS_ROOT/class/infiniband_mad/abi_version" ]]; then
      read HW_IB_UMAD_ABI_VER < "$SYSFS_ROOT/class/infiniband_mad/abi_version"
    else
      HW_IB_UMAD_ABI_VER=0
    fi
    export HW_IB_UMAD_ABI_VER
}

# Check that the named device ($1) is a member of the expected PKey.
#
# Ports that are absent from sysfs or not ACTIVE are skipped: their partition
# table is not authoritative while the link is down, and check_ib.sh already
# reports that fault. Asserting here too would report one fault as two
# unrelated conditions, which is what this split exists to prevent.
function check_ib_pkey() {
    local DEV="$1"
    local PKEY="$EXPECTED_IB_PKEY"
    local observed
    local i

    if [[ ${#HW_IB_DEV[*]} -eq 0 ]]; then
      gather_ib_data
    fi

    for ((i=0; i < ${#HW_IB_DEV[*]}; i++)); do
        if [[ -n "$DEV" && "${HW_IB_DEV[$i]}" != "$DEV" ]]; then
          continue
        fi
        if [[ "${HW_IB_STATE[$i]}" != "ACTIVE" ]]; then
          continue
        fi
        observed="$(normalize_pkey "${HW_IB_PKEY[$i]}")"
        if [[ "$observed" != "$PKEY" ]]; then
          echo "IB port ${HW_IB_DEV[$i]} PKey mismatch: expected $PKEY, observed $observed."
          exit 1
        fi
    done

    return 0
}

for ib_dev in $EXPECTED_IB_DEVS
do
    check_ib_pkey $ib_dev
done

echo "IB PKeys are ok"
exit 0
