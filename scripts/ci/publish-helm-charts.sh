#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly ACR_NAME="${ACR_NAME:-}"
readonly REPOSITORY_NAME="${REPOSITORY_NAME:-public/aks/ai-runtime}"
readonly CHARTS=(
  charts/tau-core-controller
  charts/gpu-monitoring
  charts/taugrid-core
  charts/adx-mon
  charts/taugrid
)

usage() {
  echo "usage: $0 package <output-directory> | publish <package-directory> | verify-images <chart-package>" >&2
  exit 2
}

chart_field() {
  local chart="$1"
  local field="$2"
  helm show chart "$chart" | yq -r ".${field}"
}

verify_chart_images() {
  local archive="$1"
  local rendered images image

  rendered=$(helm template image-preflight "$archive" --include-crds) || return 1
  images=$(printf '%s\n' "$rendered" |
    yq -N -r 'select(.kind != "CustomResourceDefinition") |
      .. | select(kind == "map") |
      (.containers, .initContainers, .ephemeralContainers) |
      select(kind == "seq") | .[] | .image |
      select(. == null or . == "") = "null"' |
    sort -u) || return 1

  while IFS= read -r image; do
    [[ -n "$image" ]] || continue
    if [[ "$image" == "null" || "$image" == -* || "$image" == *[[:space:]]* ]]; then
      echo "Invalid required image in ${archive}: ${image}" >&2
      return 1
    fi
    echo "Verifying published image ${image} required by ${archive}"
    if ! docker buildx imagetools inspect "$image" >/dev/null; then
      echo "Required image is not available: ${image} (chart ${archive}). Publish the image and complete public registry syndication before publishing charts." >&2
      return 1
    fi
  done <<<"$images"
}

package_charts() {
  local output_dir="$1"
  mkdir -p "$output_dir"

  helm repo add \
    prometheus-community \
    https://prometheus-community.github.io/helm-charts \
    --force-update
  helm repo update prometheus-community

  for chart in "${CHARTS[@]}"; do
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
  local error_file="$2"

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
  rm -rf "$PUBLISH_WORK_ROOT"
}

publish_charts() {
  local package_dir="$1"
  local registry_logged_in=false

  if [[ -z "$ACR_NAME" ]]; then
    echo "ACR_NAME must be set to publish Helm charts." >&2
    return 2
  fi

  readonly PUBLISH_WORK_ROOT="${package_dir}/.publish-helm-charts-${BASHPID:-$$}"
  mkdir "$PUBLISH_WORK_ROOT"
  trap cleanup_work_root EXIT

  local archives=()
  local chart_names=()
  local chart_versions=()
  local repositories=()

  for chart in "${CHARTS[@]}"; do
    local chart_name
    local chart_version
    local archive
    local repository
    local tags

    chart_name=$(chart_field "$chart" name)
    chart_version=$(chart_field "$chart" version)
    archive="${package_dir}/${chart_name}-${chart_version}.tgz"
    repository="${REPOSITORY_NAME}/helm/${chart_name}"

    tags=$(repository_tags "$repository" "${PUBLISH_WORK_ROOT}/registry-error")
    if grep -Fxq "$chart_version" <<<"$tags"; then
      echo "Chart ${chart_name}:${chart_version} already exists in the publish registry; skipping."
      continue
    fi

    if [[ ! -f "$archive" ]]; then
      echo "Expected chart package not found: ${archive}" >&2
      return 1
    fi

    archives+=("$archive")
    chart_names+=("$chart_name")
    chart_versions+=("$chart_version")
    repositories+=("$repository")
  done

  local index
  for archive in "${archives[@]}"; do
    verify_chart_images "$archive"
  done

  for index in "${!archives[@]}"; do
    archive="${archives[$index]}"
    chart_name="${chart_names[$index]}"
    chart_version="${chart_versions[$index]}"
    repository="${repositories[$index]}"

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

    mkdir -p "${PUBLISH_WORK_ROOT}/${chart_name}-verified"
    helm pull \
      "oci://${ACR_NAME}.azurecr.io/${repository}" \
      --version "$chart_version" \
      --destination "${PUBLISH_WORK_ROOT}/${chart_name}-verified"
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
  verify-images)
    verify_chart_images "$2"
    ;;
  *)
    usage
    ;;
esac
