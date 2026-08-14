#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# This plugin checks if nvme is accessible and has the expected size

readonly OK=0
readonly NONOK=1

readonly EXPECTED_NVME_TOTAL="${NVME_TOTAL:-0}"
readonly EXPECTED_NVME_SIZE_COUNT="${NVME_SIZE_COUNT:-0}"
readonly EXPECTED_NVME_SIZE="${NVME_SIZE:-0}"

nvmes=$(lsblk --nodeps --noheadings --output NAME,TYPE,SIZE | awk '$2 == "disk" && $1 ~ /^nvme/ {print $1 " " $3}')

# find total number of nvme devices
nvme_total=$(echo "$nvmes" | wc -l)
if [[ "$nvme_total" -lt $EXPECTED_NVME_TOTAL ]]; then
  echo "Expected at least $EXPECTED_NVME_TOTAL nvme devices, found $nvme_total"
  exit $NONOK
fi

nvme_sized=$(echo "$nvmes" | grep -E " $EXPECTED_NVME_SIZE$")
if [[ $(echo "$nvme_sized" | wc -l) -lt $EXPECTED_NVME_SIZE_COUNT ]]; then
  echo "Expected at least $EXPECTED_NVME_SIZE_COUNT nvme devices with size $EXPECTED_NVME_SIZE, found $(echo "$nvme_sized" | wc -l)"
  exit $NONOK
fi

echo "Found a total of $nvme_total nvme devices, with $EXPECTED_NVME_SIZE_COUNT with the expected size of $EXPECTED_NVME_SIZE."
exit $OK
