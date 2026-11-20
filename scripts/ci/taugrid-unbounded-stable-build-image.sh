#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

require_value() {
  local name="$1"
  local value="${!name:-}"
  if [[ -z "${value}" || "${value}" == '$('* ]]; then
    echo "##vso[task.logissue type=error]${name} must be configured in Azure DevOps"
    exit 1
  fi
}

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <controller|portal|collector>" >&2
  exit 2
fi

readonly component="$1"
case "$component" in
  controller | portal | collector) ;;
  *)
    echo "unsupported nightly image component: ${component}" >&2
    exit 2
    ;;
esac

require_value UNBOUNDED_STABLE_ACR_NAME
require_value NIGHTLY_IMAGE_REPOSITORY_PREFIX
require_value BUILD_SOURCEVERSION
require_value BUILD_BUILDID

readonly registry="${UNBOUNDED_STABLE_ACR_NAME}.azurecr.io"
readonly image_tag="nightly-${BUILD_SOURCEVERSION:0:12}-${BUILD_BUILDID}"
readonly builder="taugrid-nightly-${component}-${BUILD_BUILDID}-${SYSTEM_JOBATTEMPT:-1}"

az acr login --name "${UNBOUNDED_STABLE_ACR_NAME}"
docker buildx create --name "${builder}" --driver docker-container --use
trap 'docker buildx rm "${builder}" >/dev/null 2>&1 || true' EXIT

case "$component" in
  controller)
    make -C images/tau-core-controller docker-push \
      IMG="${registry}/${NIGHTLY_IMAGE_REPOSITORY_PREFIX}/tau-core-controller:${image_tag}" \
      TAG="${image_tag}" \
      PLATFORMS=linux/amd64,linux/arm64
    ;;
  portal)
    make -C images/taugrid-portal docker-push \
      IMG="${registry}/${NIGHTLY_IMAGE_REPOSITORY_PREFIX}/taugrid-portal:${image_tag}" \
      TAG="${image_tag}" \
      VERSION="${image_tag}" \
      COMMIT="${BUILD_SOURCEVERSION}" \
      DATE="$(git show -s --format=%cI "${BUILD_SOURCEVERSION}")" \
      PLATFORMS=linux/amd64,linux/arm64
    ;;
  collector)
    make -C images/gpu-metrics-collector docker-push \
      ACR_REGISTRY="${registry}" \
      PUSH_IMG="${registry}/${NIGHTLY_IMAGE_REPOSITORY_PREFIX}/gpu-metrics-collector:${image_tag}" \
      TAG="${image_tag}" \
      PLATFORMS=linux/amd64,linux/arm64
    ;;
esac
