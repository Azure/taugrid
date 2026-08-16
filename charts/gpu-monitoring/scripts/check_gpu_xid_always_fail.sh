#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# GPU XID error detection script using journalctl
readonly OK=0
readonly NONOK=1
readonly time_threshold_minutes=2
readonly logfile="/tmp/gpu_xid_error_check.log"
readonly xidflagfile="/tmp/gpu_xid_error_check.flag"
readonly GPU_XID_TEST="GPU Xid errors detected"
readonly XID_EC="[0-9]+"

# Always fail if XID error flag file exists to match gpu operator behavior
if [[ -f $xidflagfile ]]; then
    echo "XID error flag file exists, xid found previously."
    exit $NONOK
fi

journal_output=$(journalctl --since "${time_threshold_minutes} minutes ago" --no-pager)
# Track if any new XID error was found
xid_error_found=false

for XID in $XID_EC; do
    xid_lines=$(echo "$journal_output" | grep -E "[X]id.*: $XID,")

    while IFS= read -r line; do
        [[ -z "$line" ]] && continue

        # Create a fingerprint for the log line
        log_fingerprint=$(echo "$line" | sha256sum | awk '{print $1}')

        # Check if this specific error was already logged
        if grep -q "$log_fingerprint" "$logfile" 2>/dev/null; then
            echo "XID $XID already logged: $line"
        else
            echo "$log_fingerprint" >> "$logfile"
            echo "$GPU_XID_TEST: $line. FaultCode: NHC2001"
            xid_error_found=true
        fi
    done <<< "$xid_lines"
done
if [ "$xid_error_found" = true ]; then
    touch "$xidflagfile"
    exit $NONOK
else
    echo "GPU XID error check passed."
    exit $OK
fi
