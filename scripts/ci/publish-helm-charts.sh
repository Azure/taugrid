#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly ACR_NAME="${ACR_NAME:-}"
readonly REPOSITORY_NAME="${REPOSITORY_NAME:-public/aks/ai-runtime}"
readonly RELEASE_TAG="${RELEASE_TAG:-}"
readonly RELEASE_SOURCE_COMMIT="${RELEASE_SOURCE_COMMIT:-}"
readonly RELEASE_VERSION="${RELEASE_VERSION:-}"
readonly PUBLICATION_STATUS_PATH="${PUBLICATION_STATUS_PATH:-}"
CONTROL_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly CONTROL_ROOT
SOURCE_ROOT="$(cd -- "${TAUGRID_SOURCE_ROOT:-$CONTROL_ROOT}" && pwd)"
readonly SOURCE_ROOT
readonly CHARTS=(
  tau-core-controller
  gpu-monitoring
  taugrid-core
  adx-mon
  taugrid
)
WORK_ROOT=""

usage() {
  echo "usage: $0 package <output-directory> | publish <sealed-package-directory>" >&2
  exit 2
}

require_release() {
  [[ "$RELEASE_TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "RELEASE_TAG must be stable vX.Y.Z." >&2
    return 2
  }
  [[ "$RELEASE_SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
    echo "RELEASE_SOURCE_COMMIT must be a full lowercase SHA." >&2
    return 2
  }
  [[ "$RELEASE_VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "RELEASE_VERSION must be stable X.Y.Z." >&2
    return 2
  }
  [[ "$RELEASE_VERSION" == "${RELEASE_TAG#v}" ]] || {
    echo "RELEASE_TAG and RELEASE_VERSION must agree." >&2
    return 2
  }
}

package_charts() {
  local output_dir="$1"
  require_release
  mkdir -p "$output_dir"
  output_dir="$(cd -- "$output_dir" && pwd)"

  (
    cd "$SOURCE_ROOT"
    python3 "${CONTROL_ROOT}/scripts/ci/release_artifacts.py" validate-source
  )
  if find "${SOURCE_ROOT}/charts" -type l -print -quit | grep -q .; then
    echo "Release chart sources must not contain symbolic links." >&2
    return 1
  fi

  helm repo add \
    prometheus-community \
    https://prometheus-community.github.io/helm-charts \
    --force-update
  helm repo update prometheus-community

  for chart in "${CHARTS[@]}"; do
    local chart_path="${SOURCE_ROOT}/charts/${chart}"
    echo "Validating ${chart_path}"
    if [[ "$chart" == "taugrid" ]]; then
      "${CONTROL_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh" "$chart_path"
    else
      helm dependency build "$chart_path"
    fi
    if find "$chart_path" -type l -print -quit | grep -q .; then
      echo "Release chart packages must not contain symbolic links." >&2
      return 1
    fi
    helm lint "$chart_path"
    helm template test "$chart_path" > /dev/null
    helm package "$chart_path" --destination "$output_dir"
  done

  (
    cd "$SOURCE_ROOT"
    python3 "${CONTROL_ROOT}/scripts/ci/release_artifacts.py" seal "$output_dir"
  )
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

chart_version() {
  local package_dir="$1"
  local chart="$2"
  python3 - "$package_dir/release-manifest.json" "$chart" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as manifest:
    print(json.load(manifest)["charts"][sys.argv[2]]["version"])
PY
}

cleanup_work_root() {
  if [[ -n "$WORK_ROOT" ]]; then
    rm -rf "$WORK_ROOT"
  fi
}

record_publication() {
  local chart="$1"
  local version="$2"
  local repository="$3"
  local digest="$4"
  local reused="$5"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "Registry returned an invalid digest for ${chart}:${version}." >&2
    return 1
  }
  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$chart" "$version" "$repository" "$digest" "$reused" \
    >>"${WORK_ROOT}/publication.tsv"
}

write_publication_status() {
  local output="$1"
  mkdir -p "$(dirname -- "$output")"
  python3 - \
    "${WORK_ROOT}/publication.tsv" \
    "$output" \
    "$RELEASE_TAG" \
    "$RELEASE_VERSION" \
    "$RELEASE_SOURCE_COMMIT" \
    "${ACR_NAME}.azurecr.io" \
    "$REPOSITORY_NAME" <<'PY'
import json
from pathlib import Path
import sys

records, output, tag, version, commit, registry, repository_root = sys.argv[1:]
charts = []
for line in Path(records).read_text().splitlines():
    name, chart_version, repository, digest, reused = line.split("\t")
    charts.append(
        {
            "name": name,
            "version": chart_version,
            "repository": repository,
            "digest": digest,
            "reused": reused == "true",
        }
    )
Path(output).write_text(
    json.dumps(
        {
            "schemaVersion": 1,
            "releaseTag": tag,
            "releaseVersion": version,
            "releaseSourceCommit": commit,
            "registry": registry,
            "repositoryRoot": repository_root,
            "charts": charts,
        },
        indent=2,
        sort_keys=True,
    )
    + "\n"
)
PY
}

verify_directory() {
  local package_dir="$1"
  python3 "${CONTROL_ROOT}/scripts/ci/verify_protected_chart_packages.py" \
    directory \
    --directory "$package_dir" \
    --commit "$RELEASE_SOURCE_COMMIT" \
    --version "$RELEASE_VERSION"
}

verify_archive() {
  local package_dir="$1"
  local chart="$2"
  local archive="$3"
  python3 "${CONTROL_ROOT}/scripts/ci/verify_protected_chart_packages.py" \
    archive \
    --directory "$package_dir" \
    --commit "$RELEASE_SOURCE_COMMIT" \
    --version "$RELEASE_VERSION" \
    --chart "$chart" \
    --archive "$archive" \
    --expected "$package_dir/${chart}-$(chart_version "$package_dir" "$chart").tgz"
}

repository_digest() {
  local repository="$1"
  local version="$2"
  az acr repository show \
    --name "$ACR_NAME" \
    --image "${repository}:${version}" \
    --query digest \
    --output tsv
}

publish_charts() {
  local package_dir="$1"
  local registry_logged_in=false
  require_release

  if [[ -z "$ACR_NAME" ]]; then
    echo "ACR_NAME must be set to publish Helm charts." >&2
    return 2
  fi
  if [[ -z "$PUBLICATION_STATUS_PATH" ]]; then
    echo "PUBLICATION_STATUS_PATH must be set to publish Helm charts." >&2
    return 2
  fi

  verify_directory "$package_dir"
  WORK_ROOT=$(mktemp -d)
  trap cleanup_work_root EXIT
  : >"${WORK_ROOT}/publication.tsv"

  ensure_registry_login() {
    if [[ "$registry_logged_in" == false ]]; then
      az acr login --name "$ACR_NAME"
      registry_logged_in=true
    fi
  }

  for chart in "${CHARTS[@]}"; do
    local chart_version
    local archive
    local digest
    local repository
    local tags
    local verified_digest

    chart_version=$(chart_version "$package_dir" "$chart")
    archive="${package_dir}/${chart}-${chart_version}.tgz"
    repository="${REPOSITORY_NAME}/helm/${chart}"
    tags=$(repository_tags "$repository")

    if grep -Fxq "$chart_version" <<<"$tags"; then
      digest=$(repository_digest "$repository" "$chart_version")
      mkdir -p "${WORK_ROOT}/${chart}-existing"
      ensure_registry_login
      helm pull \
        "oci://${ACR_NAME}.azurecr.io/${repository}" \
        --version "$chart_version" \
        --destination "${WORK_ROOT}/${chart}-existing"
      verify_archive \
        "$package_dir" \
        "$chart" \
        "${WORK_ROOT}/${chart}-existing/${chart}-${chart_version}.tgz"
      verified_digest=$(repository_digest "$repository" "$chart_version")
      if [[ "$verified_digest" != "$digest" ]]; then
        echo "Immutable chart tag changed during verification: ${chart}:${chart_version}." >&2
        return 1
      fi
      record_publication "$chart" "$chart_version" "$repository" "$digest" true
      echo "Chart ${chart}:${chart_version} already exists with matching source provenance; reusing."
      continue
    fi

    if [[ ! -f "$archive" ]]; then
      echo "Expected chart package not found: ${archive}" >&2
      return 1
    fi

    ensure_registry_login
    helm push \
      "$archive" \
      "oci://${ACR_NAME}.azurecr.io/${REPOSITORY_NAME}/helm"

    digest=$(repository_digest "$repository" "$chart_version")
    if [[ -z "$digest" ]]; then
      echo "Published chart verification returned no digest for ${chart}:${chart_version}." >&2
      return 1
    fi

    mkdir -p "${WORK_ROOT}/${chart}-verified"
    helm pull \
      "oci://${ACR_NAME}.azurecr.io/${repository}" \
      --version "$chart_version" \
      --destination "${WORK_ROOT}/${chart}-verified"
    verify_archive \
      "$package_dir" \
      "$chart" \
      "${WORK_ROOT}/${chart}-verified/${chart}-${chart_version}.tgz"
    cmp "$archive" "${WORK_ROOT}/${chart}-verified/${chart}-${chart_version}.tgz"
    verified_digest=$(repository_digest "$repository" "$chart_version")
    if [[ "$verified_digest" != "$digest" ]]; then
      echo "Published chart tag changed during verification: ${chart}:${chart_version}." >&2
      return 1
    fi
    record_publication "$chart" "$chart_version" "$repository" "$digest" false
    echo "Published and verified ${chart}:${chart_version} at ${digest}"
  done

  write_publication_status "$PUBLICATION_STATUS_PATH"
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
