#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

# Thresholds
THRESH_DRAM_UCE=4
THRESH_SRAM_PARITY_UCE=4
THRESH_SRAM_SECDED_UCE=2

TMP=$(mktemp)

if ! timeout 8s nvidia-smi -q > "$TMP" 2>/dev/null; then
  echo "Error: failed to run nvidia-smi (timeout or error)"
  rm -f "$TMP"
  exit $UNKNOWN
fi

bad_gpus=0
bad_gpu_lines=()

# Internal state
gpu_serial=""
gpu_uuid=""
dram_uce=""
sram_parity_uce=""
sram_secded_uce=""

print_gpu_info() {
  local msg=""

  if [[ -n "$gpu_uuid" && -n "$gpu_serial" ]]; then
    [[ "$dram_uce" =~ ^[0-9]+$ ]] && (( dram_uce > THRESH_DRAM_UCE )) && msg+="DRAM_UCE=$dram_uce "
    [[ "$sram_parity_uce" =~ ^[0-9]+$ ]] && (( sram_parity_uce > THRESH_SRAM_PARITY_UCE )) && msg+="SRAM_PARITY_UCE=$sram_parity_uce "
    [[ "$sram_secded_uce" =~ ^[0-9]+$ ]] && (( sram_secded_uce > THRESH_SRAM_SECDED_UCE )) && msg+="SRAM_SECDED_UCE=$sram_secded_uce "

    if [[ -n "$msg" ]]; then
      bad_gpu_lines+=("Serial=$gpu_serial UUID=$gpu_uuid $msg")
      ((bad_gpus++))
    fi
  fi

  gpu_serial=""; gpu_uuid=""
  dram_uce=""; sram_parity_uce=""; sram_secded_uce=""
}

while IFS= read -r line; do
  case "$line" in
    "GPU "*)
      print_gpu_info
      ;;
    "    Serial Number"*)
      gpu_serial=$(echo "$line" | awk -F': ' '{print $2}')
      ;;
    "    GPU UUID"*)
      gpu_uuid=$(echo "$line" | awk -F': ' '{print $2}')
      ;;
    *"DRAM Uncorrectable"*)
      dram_uce=$(echo "$line" | awk -F': ' '{print $2}')
      ;;
    *"SRAM Uncorrectable Parity"*)
      sram_parity_uce=$(echo "$line" | awk -F': ' '{print $2}')
      ;;
    *"SRAM Uncorrectable SEC-DED"*)
      sram_secded_uce=$(echo "$line" | awk -F': ' '{print $2}')
      ;;
  esac
done < "$TMP"

print_gpu_info
rm -f "$TMP"

if (( bad_gpus == 0 )); then
  echo "ECC OK"
  exit $OK
else
  echo -n "Total bad GPUs: $bad_gpus | "
  echo "${bad_gpu_lines[*]}"
  exit $NONOK
fi
