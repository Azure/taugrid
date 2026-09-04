#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

parse_semver() {
  local original="$1"
  local description="$2"
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
    if [[ -z "$build" || "$build" == *+* ||
      "$build" == .* || "$build" == *. || "$build" == *..* ]]; then
      echo "Invalid ${description} chart version '${original}': build metadata must contain non-empty dot-separated identifiers" >&2
      return 2
    fi
    IFS=. read -r -a identifiers <<<"$build"
    for identifier in "${identifiers[@]}"; do
      if [[ ! "$identifier" =~ ^[0-9A-Za-z-]+$ ]]; then
        echo "Invalid ${description} chart version '${original}': build metadata identifier '${identifier}' must contain only ASCII letters, digits, and hyphens" >&2
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
      echo "Invalid ${description} chart version '${original}': prerelease must contain non-empty dot-separated identifiers" >&2
      return 2
    fi
    IFS=. read -r -a identifiers <<<"$SEMVER_PRERELEASE"
    for identifier in "${identifiers[@]}"; do
      if [[ ! "$identifier" =~ ^[0-9A-Za-z-]+$ ]]; then
        echo "Invalid ${description} chart version '${original}': prerelease identifier '${identifier}' must contain only ASCII letters, digits, and hyphens" >&2
        return 2
      fi
      if [[ "$identifier" =~ ^[0-9]+$ &&
        "$identifier" != "0" && "$identifier" == 0* ]]; then
        echo "Invalid ${description} chart version '${original}': numeric prerelease identifier '${identifier}' must not contain leading zeros" >&2
        return 2
      fi
    done
  fi

  if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "Invalid ${description} chart version '${original}': core must be MAJOR.MINOR.PATCH with numeric components and no leading zeros" >&2
    return 2
  fi
  IFS=. read -r SEMVER_MAJOR SEMVER_MINOR SEMVER_PATCH <<<"$version"
}

numeric_identifier_is_older() {
  local candidate="$1"
  local reference="$2"

  if [[ "${#candidate}" -ne "${#reference}" ]]; then
    [[ "${#candidate}" -lt "${#reference}" ]]
    return
  fi
  [[ "$candidate" < "$reference" ]]
}

version_is_older() {
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

  parse_semver "$1" "candidate" || return $?
  candidate_major="$SEMVER_MAJOR"
  candidate_minor="$SEMVER_MINOR"
  candidate_patch="$SEMVER_PATCH"
  candidate_prerelease="$SEMVER_PRERELEASE"

  parse_semver "$2" "reference" || return $?
  reference_major="$SEMVER_MAJOR"
  reference_minor="$SEMVER_MINOR"
  reference_patch="$SEMVER_PATCH"
  reference_prerelease="$SEMVER_PRERELEASE"

  if [[ "$candidate_major" != "$reference_major" ]]; then
    numeric_identifier_is_older "$candidate_major" "$reference_major"
    return
  fi
  if [[ "$candidate_minor" != "$reference_minor" ]]; then
    numeric_identifier_is_older "$candidate_minor" "$reference_minor"
    return
  fi
  if [[ "$candidate_patch" != "$reference_patch" ]]; then
    numeric_identifier_is_older "$candidate_patch" "$reference_patch"
    return
  fi

  if [[ -z "$candidate_prerelease" ]]; then
    return 1
  fi
  if [[ -z "$reference_prerelease" ]]; then
    return 0
  fi

  IFS=. read -r -a candidate_identifiers <<<"$candidate_prerelease"
  IFS=. read -r -a reference_identifiers <<<"$reference_prerelease"
  for ((index = 0;
    index < ${#candidate_identifiers[@]} ||
      index < ${#reference_identifiers[@]};
    index++)); do
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

preserve_kueue_crd_retention() (
  local archive="$1"
  local work_dir
  local chart_dir
  local crd
  local patched=0

  # Kueue 0.18.2 used this policy, so restoring it in 0.19.0 also makes a
  # direct enabled-to-disabled upgrade retain the CRDs owned by the old release.
  work_dir="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-kueue.XXXXXX")"
  trap 'rm -rf "$work_dir"' EXIT
  tar -xzf "$archive" -C "$work_dir"
  chart_dir="${work_dir}/kueue"

  for crd in "${chart_dir}"/templates/crd/*.yaml; do
    [[ -f "$crd" ]] || {
      echo "Kueue dependency contains no templated CRDs: ${archive}" >&2
      return 1
    }
    if grep -Fq 'helm.sh/resource-policy: keep' "$crd"; then
      patched=$((patched + 1))
      continue
    fi
    awk '
      { print }
      /^  annotations:$/ {
        print "    helm.sh/resource-policy: keep"
        inserted = 1
      }
      END { if (!inserted) exit 1 }
    ' "$crd" >"${crd}.tmp" || {
      rm -f "${crd}.tmp"
      echo "Could not add CRD retention policy to ${crd}" >&2
      return 1
    }
    mv "${crd}.tmp" "$crd"
    patched=$((patched + 1))
  done

  [[ "$patched" -gt 0 ]] || {
    echo "Kueue dependency contains no CRDs to retain: ${archive}" >&2
    return 1
  }
  rm -f "$archive"
  COPYFILE_DISABLE=1 tar -czf "$archive" -C "$work_dir" kueue
)

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
        if [[ "$name" == "kueue" ]]; then
          preserve_kueue_crd_retention "${vendor_dir}/${name}-${version}.tgz"
        fi
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
