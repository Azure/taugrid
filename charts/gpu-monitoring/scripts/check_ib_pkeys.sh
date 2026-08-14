#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks IB devices. Copied from Azure HPC again but with many bug fixes.
readonly EXPECTED_IB_Gbps="${EXPECTED_IB_GBPS:-400}"
readonly EXPECTED_IB_DEVS="${IB_DEVICES:-}"
readonly EXPECTED_IB_NUM_DEVS=`grep -o ":" <<<"$EXPECTED_IB_DEVS" | wc -l`
readonly SYSFS_ROOT="${SYSFS_ROOT:-/sys}"

# Only check for tenant PKEY on Mango PDX
if [[ $EXPECTED_IB_NUM_DEVS -eq 8 ]]; then
  readonly EXPECTED_IB_PKEY="0x8003"
else
  readonly EXPECTED_IB_PKEY="0xffff"
fi

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

# Check if IB state, phys_state, and rate ($1) all match.
function check_ib() {
    local STATE="ACTIVE"
    local PHYS_STATE="LinkUp"
    local RATE="$1"
    local DEV="$2"
    local PKEY="$EXPECTED_IB_PKEY"
    local i

    if [[ ${#HW_IB_STATE[*]} -eq 0 ]]; then
      gather_ib_data
    fi

    if [[ $HW_IB_UMAD_ABI_VER -eq 0 ]]; then
      echo "Version mismatch between kernel OFED drivers and userspace OFED libraries."
      exit 1
    fi

    for ((i=0; i < ${#HW_IB_STATE[*]}; i++)); do
        if [[ "${HW_IB_STATE[$i]}" == "$STATE" && "${HW_IB_PHYS_STATE[$i]}" == "$PHYS_STATE"  && "${HW_IB_PKEY[$i]}" == "$PKEY" ]]; then
          if [[ (-z "$DEV" || "${HW_IB_DEV[$i]}" == "$DEV") && (-z "$RATE" || "${HW_IB_RATE[$i]}" == "$RATE") && (-z "$PKEY" || "${HW_IB_PKEY[$i]}" == "$PKEY") ]]; then
              return 0
          fi
        fi
    done

    if [[ -n "$DEV" ]]; then
      DEV=" $DEV"
    fi
    if [[ -n "$RATE" ]]; then
      RATE=" $RATE Gb/sec"
    fi
    if [[ -n "$PKEY" ]]; then
      PKEY=" $PKEY"
    fi

    echo "No IB port$DEV is $STATE ($PHYS_STATE$RATE)."
    exit 1
}

for ib_dev in $EXPECTED_IB_DEVS
do
    check_ib $EXPECTED_IB_Gbps $ib_dev
done

echo "IB devices are ok"
exit 0
