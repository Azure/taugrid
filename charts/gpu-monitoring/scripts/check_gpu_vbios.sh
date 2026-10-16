#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

eval "readonly expected_versions=${VBIOS_VERSIONS:-'("")'}"

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

if [ ${#expected_versions[@]} -eq 0 ] || [ -z "${expected_versions[0]}" ]; then
  echo "No expected VBIOS versions configured, skipping check"
  exit $OK
fi

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

if [ ${#unexpected_versions[@]} -ne 0 ]; then
  echo "GPU VBIOS version (${unexpected_versions[*]}) does not match one of the expected versions '${expected_versions[*]}'. FaultCode: NHC2001"
  exit $NONOK
fi

echo "GPU VBIOS version (${uniq_vbios_versions[*]}) matches one of the expected versions '${expected_versions[*]}'"
exit $OK
