#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks GPU VBIOS version

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

# Allow for the following VBIOS versions.
# VBIOS_VERSIONS is a bash array literal, e.g. ("ver1" "ver2")
eval "readonly expected_versions=${VBIOS_VERSIONS:-'("")'}"

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if ! timeout 8s nvidia-smi -q > "$TMP" 2>/dev/null; then
  echo "Error: failed to run nvidia-smi (timeout or error)"
  exit $UNKNOWN
fi

# Collect unique VBIOS versions into an array. Previously uniq_vbios_versions
# was a newline-separated scalar string, so ${#var[@]} was always 1 and
# multi-version detection never fired.
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

uniq_vbios_version="${uniq_vbios_versions[0]}"

# Empty expected = skip check
if [ ${#expected_versions[@]} -eq 0 ] || [ -z "${expected_versions[0]}" ]; then
  echo "No expected VBIOS versions configured, skipping check"
  exit $OK
fi

for expected_version in "${expected_versions[@]}"; do
  if [[ "$expected_version" == "$uniq_vbios_version" ]]; then
    echo "GPU VBIOS version (${uniq_vbios_version}) matches one of the expected versions '${expected_versions[*]}'"
    exit $OK
  fi
done

echo "GPU VBIOS version (${uniq_vbios_version}) does not match one of the expected versions '${expected_versions[*]}'. FaultCode: NHC2001"
exit $NONOK
