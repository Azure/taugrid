#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if ! timeout 8s nvidia-smi -q > "$TMP" 2>/dev/null; then
  echo "Error: failed to run nvidia-smi (timeout or error)"
  exit $UNKNOWN
fi

uniq_vbios_versions=()
while IFS= read -r v; do
    [ -n "$v" ] && uniq_vbios_versions+=("$v")
done < <(grep "VBIOS Version" "$TMP" | cut -d ':' -f 2 | tr -d ' ' | sort -u)

if [ ${#uniq_vbios_versions[@]} -eq 0 ]; then
    echo "No VBIOS version found in nvidia-smi output"
    exit $UNKNOWN
fi

if [ ${#uniq_vbios_versions[@]} -ne 1 ]; then
    echo "More than 1 VBIOS version found on GPUs! Found '${uniq_vbios_versions[*]}'. FaultCode: NHC2001"
    exit $NONOK
fi

echo "All GPUs report the same VBIOS version (${uniq_vbios_versions[0]})"
exit $OK
