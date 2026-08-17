#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks that the GPU VBIOS versions on the node are on the
# allow-list.
#
# It asserts allow-list membership only. Whether all GPUs agree on one version
# is asserted separately by check_gpu_vbios_consistency.sh, so a mixed-VBIOS
# hardware fault and fleet configuration drift surface as independent,
# correctly-attributed node conditions.

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

# Empty expected = skip check
if [ ${#expected_versions[@]} -eq 0 ] || [ -z "${expected_versions[0]}" ]; then
  echo "No expected VBIOS versions configured, skipping check"
  exit $OK
fi

# Every observed version must be on the allow-list. Versions differing between
# GPUs is not a failure here: check_gpu_vbios_consistency.sh owns that fault, so
# GPUVbiosMismatch means exactly "VBIOS is not on the allow-list".
unexpected_versions=()
for observed_version in "${uniq_vbios_versions[@]}"; do
  matched=0
  for expected_version in "${expected_versions[@]}"; do
    if [[ "$expected_version" == "$observed_version" ]]; then
      matched=1
      break
    fi
  done
  if [ $matched -eq 0 ]; then
    unexpected_versions+=("$observed_version")
  fi
done

# FaultCode NHC2001 is the documented catch-all in the Azure HPC fault
# dictionary and the code upstream azure_gpu_vbios.nhc emits for this check.
if [ ${#unexpected_versions[@]} -ne 0 ]; then
  echo "GPU VBIOS version (${unexpected_versions[*]}) does not match one of the expected versions '${expected_versions[*]}'. FaultCode: NHC2001"
  exit $NONOK
fi

echo "GPU VBIOS version (${uniq_vbios_versions[*]}) matches one of the expected versions '${expected_versions[*]}'"
exit $OK
