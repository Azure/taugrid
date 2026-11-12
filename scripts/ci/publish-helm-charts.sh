#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly ACR_NAME="${ACR_NAME:-}"
readonly REPOSITORY_NAME="${REPOSITORY_NAME:-public/aks/ai-runtime}"
readonly SOURCE_BRANCH="${SOURCE_BRANCH:-${BUILD_SOURCEBRANCH:-}}"
readonly CHARTS=(
  charts/tau-core-controller
  charts/gpu-monitoring
  charts/taugrid-core
  charts/adx-mon
  charts/taugrid
)
readonly COORDINATED_CHARTS=(
  tau-core-controller
  taugrid-core
  taugrid
)
WORK_ROOT=""

usage() {
  echo "usage: $0 package <output-directory> | publish <package-directory>" >&2
  exit 2
}

chart_field() {
  local chart="$1"
  local field="$2"
  helm show chart "$chart" | yq -r ".${field}"
}

release_tag() {
  local tag="${RELEASE_TAG:-}"
  if [[ -z "$tag" && "$SOURCE_BRANCH" == refs/tags/* ]]; then
    tag="${SOURCE_BRANCH#refs/tags/}"
  fi
  if [[ -n "$tag" && ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "Release tag must be v-prefixed SemVer, got: ${tag}" >&2
    return 2
  fi
  printf '%s' "$tag"
}

is_coordinated_chart() {
  local name="$1"
  local coordinated
  for coordinated in "${COORDINATED_CHARTS[@]}"; do
    if [[ "$name" == "$coordinated" ]]; then
      return 0
    fi
  done
  return 1
}

package_charts() {
  local output_dir="$1"
  local tag
  tag=$(release_tag)
  mkdir -p "$output_dir"

  helm repo add \
    prometheus-community \
    https://prometheus-community.github.io/helm-charts \
    --force-update
  helm repo update prometheus-community

  if [[ -n "$tag" ]]; then
    python3 scripts/ci/release_artifacts.py package-charts \
      --source . \
      --tag "$tag" \
      --output "$output_dir"
  fi

  for chart in "${CHARTS[@]}"; do
    local chart_name
    chart_name=$(chart_field "$chart" name)
    if [[ -n "$tag" ]] && is_coordinated_chart "$chart_name"; then
      continue
    fi
    echo "Validating ${chart}"
    if [[ "$chart" == "charts/taugrid" ]]; then
      scripts/ci/vendor-taugrid-dependencies.sh "$chart"
    else
      helm dependency build "$chart"
    fi
    helm lint "$chart"
    if [[ -f "${chart}/tests/scripts_test.sh" ]]; then
      bash "${chart}/tests/scripts_test.sh"
    fi
    helm template test "$chart" > /dev/null
    helm package "$chart" --destination "$output_dir"
  done
}

repository_tags() {
  local repository="$1"
  local error_file
  error_file=$(mktemp)

  if az acr repository show-tags \
    --name "$ACR_NAME" \
    --repository "$repository" \
    --output tsv 2>"$error_file"; then
    rm -f "$error_file"
    return 0
  fi

  if grep -Eqi 'NAME_UNKNOWN|repository.*(not found|does not exist)' "$error_file"; then
    rm -f "$error_file"
    return 0
  fi

  cat "$error_file" >&2
  rm -f "$error_file"
  return 1
}

cleanup_work_root() {
  rm -rf "$WORK_ROOT"
}

publish_charts() {
  local package_dir="$1"
  local registry_logged_in=false
  local tag
  local release_version

  tag=$(release_tag)
  release_version="${tag#v}"

  if [[ -z "$ACR_NAME" ]]; then
    echo "ACR_NAME must be set to publish Helm charts." >&2
    return 2
  fi

  WORK_ROOT=$(mktemp -d)
  trap cleanup_work_root EXIT

  for chart in "${CHARTS[@]}"; do
    local chart_name
    local chart_version
    local archive
    local repository
    local tags

    chart_name=$(chart_field "$chart" name)
    if is_coordinated_chart "$chart_name"; then
      if [[ -z "$tag" ]]; then
        echo "Skipping release-neutral coordinated chart ${chart_name} outside a release tag."
        continue
      fi
      chart_version="$release_version"
    else
      chart_version=$(chart_field "$chart" version)
    fi
    archive="${package_dir}/${chart_name}-${chart_version}.tgz"
    repository="${REPOSITORY_NAME}/helm/${chart_name}"

    tags=$(repository_tags "$repository")
    if grep -Fxq "$chart_version" <<<"$tags"; then
      echo "Chart ${chart_name}:${chart_version} already exists in the publish registry; skipping."
      continue
    fi

    if [[ ! -f "$archive" ]]; then
      echo "Expected chart package not found: ${archive}" >&2
      return 1
    fi

    if [[ "$registry_logged_in" == false ]]; then
      az acr login --name "$ACR_NAME"
      registry_logged_in=true
    fi

    helm push \
      "$archive" \
      "oci://${ACR_NAME}.azurecr.io/${REPOSITORY_NAME}/helm"

    local digest
    digest=$(az acr repository show \
      --name "$ACR_NAME" \
      --image "${repository}:${chart_version}" \
      --query digest \
      --output tsv)
    if [[ -z "$digest" ]]; then
      echo "Published chart verification returned no digest for ${chart_name}:${chart_version}." >&2
      return 1
    fi

    mkdir -p "${WORK_ROOT}/${chart_name}-verified"
    helm pull \
      "oci://${ACR_NAME}.azurecr.io/${repository}" \
      --version "$chart_version" \
      --destination "${WORK_ROOT}/${chart_name}-verified"
    echo "Published and verified ${chart_name}:${chart_version} at ${digest}"
  done
}

[[ $# -eq 2 ]] || usage

case "$1" in
  package)
    package_charts "$2"
    ;;
  publish)
    publish_charts "$2"
    ;;
  *)
    usage
    ;;
esac
