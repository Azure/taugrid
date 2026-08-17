#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CASE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/testlib.sh
source "${CASE_DIR}/../lib/testlib.sh"
setup_test_case bundle-wiring

readonly ORIGINAL_BUNDLE_NAME="$(
  helm template content-hash "$CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    awk '$1 == "name:" { print $2; exit }'
)"
readonly MUTATED_CHART_DIR="$TEST_ROOT/gpu-monitoring"
cp -R "$CHART_DIR" "$MUTATED_CHART_DIR"
printf '\n# content hash regression probe\n' >>"$MUTATED_CHART_DIR/scripts/check_gpu_xid.sh"
readonly MUTATED_BUNDLE_NAME="$(
  helm template content-hash "$MUTATED_CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    awk '$1 == "name:" { print $2; exit }'
)"
[[ "$ORIGINAL_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] ||
  fail "unexpected original bundle name: $ORIGINAL_BUNDLE_NAME"
[[ "$MUTATED_BUNDLE_NAME" =~ ^gpu-monitoring-gpu-[a-f0-9]{10}$ ]] ||
  fail "unexpected mutated bundle name: $MUTATED_BUNDLE_NAME"
[[ "$ORIGINAL_BUNDLE_NAME" != "$MUTATED_BUNDLE_NAME" ]] ||
  fail "bundle name did not change when a script changed"

# Every custom-config subPath the DaemonSet mounts must exist in the bundle.
readonly BUNDLE_KEYS="$(
  helm template wiring "$CHART_DIR" --show-only templates/executable-bundle-secret.yaml |
    sed -n 's/^  \([A-Za-z0-9._-]*\): |$/\1/p' | sort -u
)"
readonly MOUNTED_SUBPATHS="$(
  helm template wiring "$CHART_DIR" --show-only templates/daemonset.yaml |
    awk '/^ *- name: /{ volume = $3 } /^ *subPath: / { if (volume == "custom-config") print $2 }' |
    sort -u
)"
[ "$(printf '%s\n' "$MOUNTED_SUBPATHS" | grep -c '\.sh$')" -ge 15 ] ||
  fail "bundle_wiring: parsed too few mounted scripts: $MOUNTED_SUBPATHS"
for mounted_subpath in $MOUNTED_SUBPATHS; do
  printf '%s\n' "$BUNDLE_KEYS" | grep -Fxq -- "$mounted_subpath" ||
    fail "bundle_wiring: the DaemonSet mounts $mounted_subpath but the executable bundle has no such key"
done
assert_contains bundle_wiring "$MOUNTED_SUBPATHS" "check_gpu_vbios.sh"
assert_contains bundle_wiring "$MOUNTED_SUBPATHS" "check_gpu_vbios_consistency.sh"

# Every plugin path the monitor configs reference must also be mounted.
readonly REFERENCED_PLUGINS="$(
  sed -n 's#^ *"path": "/custom-config/\(.*\)",*$#\1#p' "$CHART_DIR"/configs/custom-plugin-monitor*.json |
    sort -u
)"
[ "$(printf '%s\n' "$REFERENCED_PLUGINS" | grep -c '\.sh$')" -ge 15 ] ||
  fail "bundle_wiring: parsed too few referenced plugins: $REFERENCED_PLUGINS"
for referenced_plugin in $REFERENCED_PLUGINS; do
  printf '%s\n' "$MOUNTED_SUBPATHS" | grep -Fxq -- "$referenced_plugin" ||
    fail "bundle_wiring: monitor configs reference $referenced_plugin but the DaemonSet does not mount it"
done
assert_contains bundle_wiring "$REFERENCED_PLUGINS" "check_gpu_vbios_consistency.sh"

# Every SKU must retain both independent VBIOS conditions and scripts.
readonly MONITOR_CONFIGS=("$CHART_DIR"/configs/custom-plugin-monitor*.json)
[ "${#MONITOR_CONFIGS[@]}" -eq 5 ] ||
  fail "vbios_wiring: expected 5 monitor configs, found ${#MONITOR_CONFIGS[@]}"
for monitor_config in "${MONITOR_CONFIGS[@]}"; do
  config_name="$(basename "$monitor_config")"
  config_body="$(<"$monitor_config")"
  assert_contains "$config_name" "$config_body" '"type": "GPUVbiosMismatch"'
  assert_contains "$config_name" "$config_body" '"type": "GPUVbiosInconsistent"'
  assert_contains "$config_name" "$config_body" '"condition": "GPUVbiosInconsistent"'
  assert_contains "$config_name" "$config_body" '"path": "/custom-config/check_gpu_vbios.sh"'
  assert_contains "$config_name" "$config_body" '"path": "/custom-config/check_gpu_vbios_consistency.sh"'
done
