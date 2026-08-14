#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# This plugin checks for GPU ECC remap failures reported by nvidia-smi.

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if ! nvidia-smi -q > "$TMP" 2>/dev/null; then
  echo "Error: failed to run nvidia-smi"
  exit $UNKNOWN
fi

mapfile -t offenders < <(awk -v field="Remapping Failure Occurred" '
function flush() {
  if (flag) {
    msg = "GPU[" gpu_idx "]"
    if (length(gpu_label)) {
      msg = msg "=" gpu_label
    }
    if (length(serial)) {
      msg = msg " Serial=" serial
    }
    print msg
  }
  flag = 0
}
BEGIN {
  gpu_idx = -1
  gpu_label = ""
  serial = ""
  in_remap = 0
  flag = 0
  remap_indent = 0
}
{
  if (in_remap) {
    if ($0 ~ /^[[:space:]]*$/) {
      flush()
      in_remap = 0
      next
    }
    indent = match($0, /[^[:space:]]/)
    if (indent == 0) {
      indent = 1
    }
    indent--
    if (indent <= remap_indent) {
      flush()
      in_remap = 0
    }
  }
  if ($0 ~ /^GPU /) {
    flush()
    gpu_idx++
    gpu_label = $0
    sub(/^GPU[[:space:]]*/, "", gpu_label)
    serial = ""
    in_remap = 0
    flag = 0
    next
  }
  if ($0 ~ /^[[:space:]]*Serial Number/) {
    serial = $0
    sub(/^.*: /, "", serial)
    next
  }
  if ($0 ~ /^[[:space:]]*Remapped Rows$/) {
    in_remap = 1
    flag = 0
    remap_indent = match($0, /[^[:space:]]/)
    if (remap_indent == 0) {
      remap_indent = 1
    }
    remap_indent--
    next
  }
  if (in_remap) {
    n = split($0, parts, ":")
    if (n < 2) {
      next
    }
    key = parts[1]
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
    value = parts[2]
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
    if (key == field && value == "Yes") {
      flag = 1
    }
  }
}
END {
  flush()
}
' "$TMP")

if ((${#offenders[@]} == 0)); then
  echo "GPU ECC remap failure: none"
  exit $OK
fi

printf -v details "%s; " "${offenders[@]}"
details=${details%; }
echo "GPU ECC remap failure detected on ${#offenders[@]} GPU(s): $details"
exit $NONOK
