#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# GPU XID error detection script using journalctl
readonly OK=0
readonly NONOK=1
readonly time_threshold_minutes=2
readonly logfile="${GPU_XID_LOGFILE:-/tmp/gpu_xid_error_check.log}"
readonly GPU_XID_TEST="GPU Xid errors detected"
readonly XID_EC="48 56 57 58 62 63 64 65 68 69 73 74 79 80 81 92 119 120"
journal_output=$(journalctl --since "${time_threshold_minutes} minutes ago" --no-pager)
# Fingerprint logging is deduplicated, but health remains non-OK while a matching
# XID is still present in the lookback window.
active_xid_found=false
for XID in $XID_EC; do
    xid_lines=$(echo "$journal_output" | grep -E "[X]id.*: $XID,")

    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        active_xid_found=true

        # Create a fingerprint for the log line
        log_fingerprint=$(echo "$line" | sha256sum | awk '{print $1}')

        # Check if this specific error was already logged
        if grep -q "$log_fingerprint" "$logfile" 2>/dev/null; then
            echo "XID $XID already logged: $line"
        else
            echo "$log_fingerprint" >> "$logfile"
            echo "$GPU_XID_TEST: $line. FaultCode: NHC2001"
        fi
    done <<< "$xid_lines"
done
if [ "$active_xid_found" = true ]; then
    exit $NONOK
else
    echo "GPU XID error check passed."
    exit $OK
fi
