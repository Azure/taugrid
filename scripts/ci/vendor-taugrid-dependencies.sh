#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

parse_chart_release_version() {
  local version="$1"
  local description="$2"

  if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "Invalid ${description} chart version '${version}': expected exactly X.Y.Z with three numeric components and no prerelease suffix" >&2
    return 2
  fi

  VERSION_MAJOR="${BASH_REMATCH[1]}"
  VERSION_MINOR="${BASH_REMATCH[2]}"
  VERSION_PATCH="${BASH_REMATCH[3]}"
}

numeric_component_is_less() {
  local candidate="$1"
  local reference="$2"

  if [[ "${#candidate}" -ne "${#reference}" ]]; then
    [[ "${#candidate}" -lt "${#reference}" ]]
    return
  fi
  [[ "$candidate" < "$reference" ]]
}

version_is_older() {
  local candidate_major
  local candidate_minor
  local candidate_patch
  local reference_major
  local reference_minor
  local reference_patch

  parse_chart_release_version "$1" "candidate" || return $?
  candidate_major="$VERSION_MAJOR"
  candidate_minor="$VERSION_MINOR"
  candidate_patch="$VERSION_PATCH"

  parse_chart_release_version "$2" "reference" || return $?
  reference_major="$VERSION_MAJOR"
  reference_minor="$VERSION_MINOR"
  reference_patch="$VERSION_PATCH"

  if [[ "$candidate_major" != "$reference_major" ]]; then
    numeric_component_is_less "$candidate_major" "$reference_major"
    return
  fi
  if [[ "$candidate_minor" != "$reference_minor" ]]; then
    numeric_component_is_less "$candidate_minor" "$reference_minor"
    return
  fi
  if [[ "$candidate_patch" != "$reference_patch" ]]; then
    numeric_component_is_less "$candidate_patch" "$reference_patch"
    return
  fi
  return 1
}

vendor_taugrid_dependencies() {
  local chart_dir="${1:-${REPO_ROOT}/charts/taugrid}"
  local vendor_dir="${chart_dir}/charts"
  local name
  local version
  local repository
  local local_chart
  local local_version
  local dependencies
  local dependency_line=0

  if [[ ! -f "${chart_dir}/Chart.yaml" ]] ||
    [[ "$(awk '$1 == "name:" { print $2; exit }' "${chart_dir}/Chart.yaml")" != "taugrid" ]]; then
    echo "Expected the TauGrid umbrella chart, got: ${chart_dir}" >&2
    return 1
  fi

  rm -rf "$vendor_dir"
  mkdir -p "$vendor_dir"

  dependencies="$(helm dependency list "$chart_dir")"
  while read -r name version repository _; do
    dependency_line=$((dependency_line + 1))
    [[ "$dependency_line" -eq 1 || -z "$name" ]] && continue

    local_chart="${REPO_ROOT}/charts/${name}"
    if [[ -f "${local_chart}/Chart.yaml" ]]; then
      local_version=$(awk '$1 == "version:" { print $2; exit }' "${local_chart}/Chart.yaml")
      if [[ "$local_version" != "$version" ]]; then
        # gpu-monitoring has an independent release cadence. When the umbrella
        # intentionally remains on an older published version, vendor that OCI
        # artifact instead of forcing an unrelated TauGrid distribution bump.
        if [[ "$name" == "gpu-monitoring" && "$repository" == oci://* ]] &&
          version_is_older "$version" "$local_version"; then
          helm pull "${repository}/${name}" --version "$version" --destination "$vendor_dir"
          continue
        fi
        echo "${chart_dir} requires ${name}:${version}, but ${local_chart} is ${local_version}" >&2
        return 1
      fi
      helm package "$local_chart" --destination "$vendor_dir"
      continue
    fi

    case "$repository" in
      oci://*)
        helm pull "${repository}/${name}" --version "$version" --destination "$vendor_dir"
        ;;
      http://*|https://*)
        helm pull "$name" --repo "$repository" --version "$version" --destination "$vendor_dir"
        ;;
      *)
        echo "Unsupported dependency repository for ${name}: ${repository}" >&2
        return 1
        ;;
    esac
  done <<<"$dependencies"

  if ! helm dependency list "$chart_dir" |
    awk 'NR > 1 && NF > 0 && $4 != "ok" { print; failed = 1 } END { exit failed }'; then
    echo "Vendored dependencies do not match ${chart_dir}/Chart.yaml" >&2
    return 1
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  vendor_taugrid_dependencies "$@"
fi
