#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


set -euo pipefail

readonly ACR_NAME="${ACR_NAME:-aksmcrimagescommon}"
readonly REPOSITORY_NAME="${REPOSITORY_NAME:-public/aks/ai-runtime}"
readonly CHARTS=(
  charts/tau-core-controller
  charts/gpu-monitoring
  charts/taugrid-core
  charts/adx-mon
  charts/taugrid
)

usage() {
  echo "usage: $0 package <output-directory> | publish <package-directory>" >&2
  exit 2
}

chart_field() {
  local chart="$1"
  local field="$2"
  helm show chart "$chart" | yq -r ".${field}"
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

verify_existing_chart() {
  local archive="$1"
  local chart_name="$2"
  local chart_version="$3"
  local work_dir="$4"
  local repository="${REPOSITORY_NAME}/helm/${chart_name}"
  local existing_dir="${work_dir}/existing"
  local candidate_dir="${work_dir}/candidate"

  mkdir -p "$existing_dir" "$candidate_dir"
  helm pull \
    "oci://${ACR_NAME}.azurecr.io/${repository}" \
    --version "$chart_version" \
    --untar \
    --untardir "$existing_dir"
  tar -xzf "$archive" -C "$candidate_dir"

  # Helm rewrites this timestamp on every dependency build. It is not part of
  # the resolved dependency set, so ignore it when checking publish retries.
  find \
    "${existing_dir}/${chart_name}" \
    "${candidate_dir}/${chart_name}" \
    -type f -name Chart.lock -exec sed -i '/^generated:/d' {} +

  if ! diff -ru "${existing_dir}/${chart_name}" "${candidate_dir}/${chart_name}"; then
    echo "Chart ${chart_name}:${chart_version} already exists with different content." >&2
    echo "Bump version in charts/${chart_name}/Chart.yaml before publishing." >&2
    return 1
  fi

  echo "Chart ${chart_name}:${chart_version} is already published with identical content; skipping."
}

cleanup_work_root() {
  local work_root="$1"
  rm -rf "$work_root"
}

publish_charts() {
  local package_dir="$1"
  local work_root
  work_root=$(mktemp -d)
  trap "cleanup_work_root $(printf '%q' "$work_root")" EXIT

  az acr login --name "$ACR_NAME"

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

    if [[ ! -f "$archive" ]]; then
      echo "Expected chart package not found: ${archive}" >&2
      return 1
    fi

    tags=$(repository_tags "$repository")
    if grep -Fxq "$chart_version" <<<"$tags"; then
      verify_existing_chart \
        "$archive" \
        "$chart_name" \
        "$chart_version" \
        "${work_root}/${chart_name}"
      continue
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

    mkdir -p "${work_root}/${chart_name}-verified"
    helm pull \
      "oci://${ACR_NAME}.azurecr.io/${repository}" \
      --version "$chart_version" \
      --destination "${work_root}/${chart_name}-verified"
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
