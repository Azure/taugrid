#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly CHART_DIR="${1:-${REPO_ROOT}/charts/taugrid}"
readonly VENDOR_DIR="${CHART_DIR}/charts"

version_is_older() {
  local candidate="${1%%-*}"
  local reference="${2%%-*}"
  local candidate_parts
  local reference_parts
  local index

  IFS=. read -r -a candidate_parts <<<"$candidate"
  IFS=. read -r -a reference_parts <<<"$reference"
  for index in 0 1 2; do
    if (( 10#${candidate_parts[$index]:-0} < 10#${reference_parts[$index]:-0} )); then
      return 0
    fi
    if (( 10#${candidate_parts[$index]:-0} > 10#${reference_parts[$index]:-0} )); then
      return 1
    fi
  done
  return 1
}

if [[ ! -f "${CHART_DIR}/Chart.yaml" ]] ||
  [[ "$(awk '$1 == "name:" { print $2; exit }' "${CHART_DIR}/Chart.yaml")" != "taugrid" ]]; then
  echo "Expected the TauGrid umbrella chart, got: ${CHART_DIR}" >&2
  exit 1
fi

rm -rf "$VENDOR_DIR"
mkdir -p "$VENDOR_DIR"

helm dependency list "$CHART_DIR" | tail -n +2 |
  while read -r name version repository _; do
    [[ -n "$name" ]] || continue

    local_chart="${REPO_ROOT}/charts/${name}"
    if [[ -f "${local_chart}/Chart.yaml" ]]; then
      local_version=$(awk '$1 == "version:" { print $2; exit }' "${local_chart}/Chart.yaml")
      if [[ "$local_version" != "$version" ]]; then
        # gpu-monitoring has an independent release cadence. When the umbrella
        # intentionally remains on an older published version, vendor that OCI
        # artifact instead of forcing an unrelated TauGrid distribution bump.
        if [[ "$name" == "gpu-monitoring" && "$repository" == oci://* ]] &&
          version_is_older "$version" "$local_version"; then
          helm pull "${repository}/${name}" --version "$version" --destination "$VENDOR_DIR"
          continue
        fi
        echo "${CHART_DIR} requires ${name}:${version}, but ${local_chart} is ${local_version}" >&2
        exit 1
      fi
      helm package "$local_chart" --destination "$VENDOR_DIR"
      continue
    fi

    case "$repository" in
      oci://*)
        helm pull "${repository}/${name}" --version "$version" --destination "$VENDOR_DIR"
        ;;
      http://*|https://*)
        helm pull "$name" --repo "$repository" --version "$version" --destination "$VENDOR_DIR"
        ;;
      *)
        echo "Unsupported dependency repository for ${name}: ${repository}" >&2
        exit 1
        ;;
    esac
  done

if ! helm dependency list "$CHART_DIR" |
  awk 'NR > 1 && NF > 0 && $4 != "ok" { print; failed = 1 } END { exit failed }'; then
  echo "Vendored dependencies do not match ${CHART_DIR}/Chart.yaml" >&2
  exit 1
fi
