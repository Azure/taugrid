#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks that every GPU on the node runs the same VBIOS version.
#
# It asserts consistency only. Whether the observed version is one the fleet
# expects is asserted separately by check_gpu_vbios.sh, so a mixed-VBIOS
# hardware fault and fleet configuration drift surface as independent,
# correctly-attributed node conditions. NPD maps one script to one condition, so
# the two properties need two scripts.
#
# Consistency needs no allow-list, so this check runs regardless of whether
# VBIOS_VERSIONS is configured.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if ! timeout 8s nvidia-smi -q > "$TMP" 2>/dev/null; then
  echo "Error: failed to run nvidia-smi (timeout or error)"
  exit $UNKNOWN
fi

# Collect unique VBIOS versions into an array. A newline-separated scalar string
# would make ${#var[@]} always 1, so multi-version detection would never fire.
uniq_vbios_versions=()
while IFS= read -r v; do
    [ -n "$v" ] && uniq_vbios_versions+=("$v")
done < <(grep "VBIOS Version" "$TMP" | cut -d ':' -f 2 | tr -d ' ' | sort -u)

if [ ${#uniq_vbios_versions[@]} -eq 0 ]; then
    echo "No VBIOS version found in nvidia-smi output"
    exit $UNKNOWN
fi

# FaultCode NHC2001 is the documented catch-all in the Azure HPC fault
# dictionary and the code upstream azure_gpu_vbios.nhc emits. There is no
# VBIOS-specific code, and an invented one degrades back to the catch-all, so
# the condition type carries the distinction instead.
if [ ${#uniq_vbios_versions[@]} -ne 1 ]; then
    echo "More than 1 VBIOS version found on GPUs! Found '${uniq_vbios_versions[*]}'. FaultCode: NHC2001"
    exit $NONOK
fi

echo "All GPUs report the same VBIOS version (${uniq_vbios_versions[0]})"
exit $OK
