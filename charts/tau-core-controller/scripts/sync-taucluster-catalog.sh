#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart_dir="$(cd "${script_dir}/.." && pwd)"
repo_root="$(cd "${chart_dir}/../.." && pwd)"
mode="${1:---check}"

if [[ "${mode}" != "--check" && "${mode}" != "--write" ]]; then
  echo "usage: $0 [--check|--write]" >&2
  exit 2
fi

canonical="$(
  helm template taucluster-catalog "${chart_dir}" \
    --show-only templates/taucluster.yaml |
    awk '
      /^  workloadProfiles:/ { copy=1 }
      copy && /^  [A-Za-z0-9][A-Za-z0-9]*:/ && $0 !~ /^  workloadProfiles:/ { exit }
      copy { print }
    '
)"

if [[ -z "${canonical}" ]]; then
  echo "rendered TauCluster has no spec.workloadProfiles" >&2
  exit 1
fi

targets=(
  "${chart_dir}/kustomize/taucluster.yaml"
  "${repo_root}/controllers/tau-core/config/samples/tau.azure.com_v1alpha1_taucluster.yaml"
)

extract_catalog() {
  awk '
    /BEGIN GENERATED WORKLOAD PROFILES/ { copy=1; next }
    /END GENERATED WORKLOAD PROFILES/ { copy=0 }
    copy && $0 !~ /^  #/ { print }
  ' "$1"
}

write_catalog() {
  local target="$1"
  local scratch="${repo_root}/.taucluster-catalog-sync.$$.yaml"
  trap 'rm -f "${scratch}"' RETURN

  while IFS= read -r line; do
    if [[ "${line}" == *"BEGIN GENERATED WORKLOAD PROFILES"* ]]; then
      printf '%s\n' "${line}"
      printf '%s\n' "  # Generated from charts/tau-core-controller/values.yaml; do not edit."
      printf '%s\n' "${canonical}"
      skipping=1
      continue
    fi
    if [[ "${line}" == *"END GENERATED WORKLOAD PROFILES"* ]]; then
      skipping=0
      printf '%s\n' "${line}"
      continue
    fi
    if [[ "${skipping:-0}" -eq 0 ]]; then
      printf '%s\n' "${line}"
    fi
  done <"${target}" >"${scratch}"

  mv "${scratch}" "${target}"
  trap - RETURN
}

for target in "${targets[@]}"; do
  if [[ "${mode}" == "--write" ]]; then
    write_catalog "${target}"
  fi
  actual="$(extract_catalog "${target}")"
  if [[ "${actual}" != "${canonical}" ]]; then
    echo "${target#${repo_root}/} workload profile catalog is out of sync" >&2
    echo "run charts/tau-core-controller/scripts/sync-taucluster-catalog.sh --write" >&2
    exit 1
  fi
done

echo "TauCluster workload profile catalog is synchronized"
