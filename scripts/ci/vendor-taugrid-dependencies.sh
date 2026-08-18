#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly CHART_DIR="${1:-${REPO_ROOT}/charts/taugrid}"
readonly VENDOR_DIR="${CHART_DIR}/charts"

parse_semver() {
  local original="$1"
  local version="$original"
  local build=""
  local identifier
  local identifiers

  SEMVER_MAJOR=""
  SEMVER_MINOR=""
  SEMVER_PATCH=""
  SEMVER_PRERELEASE=""

  if [[ "$version" == *+* ]]; then
    build="${version#*+}"
    version="${version%%+*}"
    if [[ -z "$build" || "$build" == *+* || "$build" == .* || "$build" == *. || "$build" == *..* ]]; then
      echo "Invalid semantic version '$original': build metadata must contain non-empty dot-separated identifiers" >&2
      return 2
    fi
    IFS=. read -r -a identifiers <<<"$build"
    for identifier in "${identifiers[@]}"; do
      if [[ -z "$identifier" || ! "$identifier" =~ ^[0-9A-Za-z-]+$ ]]; then
        echo "Invalid semantic version '$original': build metadata contains invalid identifier '$identifier'" >&2
        return 2
      fi
    done
  fi

  if [[ "$version" == *-* ]]; then
    SEMVER_PRERELEASE="${version#*-}"
    version="${version%%-*}"
    if [[ -z "$SEMVER_PRERELEASE" ||
      "$SEMVER_PRERELEASE" == .* ||
      "$SEMVER_PRERELEASE" == *. ||
      "$SEMVER_PRERELEASE" == *..* ]]; then
      echo "Invalid semantic version '$original': prerelease identifiers must not be empty" >&2
      return 2
    fi
    IFS=. read -r -a identifiers <<<"$SEMVER_PRERELEASE"
    for identifier in "${identifiers[@]}"; do
      if [[ -z "$identifier" || ! "$identifier" =~ ^[0-9A-Za-z-]+$ ]]; then
        echo "Invalid semantic version '$original': prerelease contains invalid identifier '$identifier'" >&2
        return 2
      fi
      if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" != "0" && "$identifier" == 0* ]]; then
        echo "Invalid semantic version '$original': numeric prerelease identifier '$identifier' has a leading zero" >&2
        return 2
      fi
    done
  fi

  if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "Invalid semantic version '$original': expected MAJOR.MINOR.PATCH with no leading zeros" >&2
    return 2
  fi
  IFS=. read -r SEMVER_MAJOR SEMVER_MINOR SEMVER_PATCH <<<"$version"
}

numeric_identifier_is_older() {
  local candidate="$1"
  local reference="$2"

  if (( ${#candidate} != ${#reference} )); then
    (( ${#candidate} < ${#reference} ))
    return
  fi
  [[ "$candidate" < "$reference" ]]
}

version_is_older() {
  local candidate="$1"
  local reference="$2"
  local LC_ALL=C
  local candidate_major
  local candidate_minor
  local candidate_patch
  local candidate_prerelease
  local reference_major
  local reference_minor
  local reference_patch
  local reference_prerelease
  local candidate_identifiers
  local reference_identifiers
  local candidate_identifier
  local reference_identifier
  local index

  parse_semver "$candidate" || return 2
  candidate_major="$SEMVER_MAJOR"
  candidate_minor="$SEMVER_MINOR"
  candidate_patch="$SEMVER_PATCH"
  candidate_prerelease="$SEMVER_PRERELEASE"

  parse_semver "$reference" || return 2
  reference_major="$SEMVER_MAJOR"
  reference_minor="$SEMVER_MINOR"
  reference_patch="$SEMVER_PATCH"
  reference_prerelease="$SEMVER_PRERELEASE"

  for index in 0 1 2; do
    case "$index" in
      0)
        candidate_identifier="$candidate_major"
        reference_identifier="$reference_major"
        ;;
      1)
        candidate_identifier="$candidate_minor"
        reference_identifier="$reference_minor"
        ;;
      *)
        candidate_identifier="$candidate_patch"
        reference_identifier="$reference_patch"
        ;;
    esac
    if numeric_identifier_is_older "$candidate_identifier" "$reference_identifier"; then
      return 0
    fi
    if numeric_identifier_is_older "$reference_identifier" "$candidate_identifier"; then
      return 1
    fi
  done

  if [[ -z "$candidate_prerelease" ]]; then
    return 1
  fi
  if [[ -z "$reference_prerelease" ]]; then
    return 0
  fi

  IFS=. read -r -a candidate_identifiers <<<"$candidate_prerelease"
  IFS=. read -r -a reference_identifiers <<<"$reference_prerelease"
  for ((index = 0; index < ${#candidate_identifiers[@]} || index < ${#reference_identifiers[@]}; index++)); do
    if (( index >= ${#candidate_identifiers[@]} )); then
      return 0
    fi
    if (( index >= ${#reference_identifiers[@]} )); then
      return 1
    fi

    candidate_identifier="${candidate_identifiers[$index]}"
    reference_identifier="${reference_identifiers[$index]}"
    [[ "$candidate_identifier" == "$reference_identifier" ]] && continue

    if [[ "$candidate_identifier" =~ ^[0-9]+$ ]]; then
      if [[ ! "$reference_identifier" =~ ^[0-9]+$ ]]; then
        return 0
      fi
      numeric_identifier_is_older "$candidate_identifier" "$reference_identifier"
      return
    fi
    if [[ "$reference_identifier" =~ ^[0-9]+$ ]]; then
      return 1
    fi
    [[ "$candidate_identifier" < "$reference_identifier" ]]
    return
  done

  return 1
}

main() {
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
          if [[ "$name" == "gpu-monitoring" && "$repository" == oci://* ]]; then
            if version_is_older "$version" "$local_version"; then
              helm pull "${repository}/${name}" --version "$version" --destination "$VENDOR_DIR"
              continue
            else
              comparison_status=$?
              (( comparison_status == 2 )) && exit 1
            fi
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
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
