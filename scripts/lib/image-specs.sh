#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Sets TAUGRID_IMAGE_REPOSITORY, TAUGRID_IMAGE_CONTEXT,
# TAUGRID_IMAGE_DOCKERFILE, and TAUGRID_IMAGE_SOURCE_PATHS for a first-party
# deployable image.
# shellcheck disable=SC2034 # These variables are the sourced library interface.
taugrid_image_spec() {
  local image="$1"

  unset TAUGRID_IMAGE_REPOSITORY TAUGRID_IMAGE_CONTEXT
  unset TAUGRID_IMAGE_DOCKERFILE TAUGRID_IMAGE_SOURCE_PATHS
  case "$image" in
    tau)
      TAUGRID_IMAGE_DOCKERFILE="images/tau/Dockerfile"
      TAUGRID_IMAGE_SOURCE_PATHS=("images/tau/Dockerfile" "cli" "core")
      ;;
    taugrid-portal)
      TAUGRID_IMAGE_DOCKERFILE="images/taugrid-portal/Dockerfile"
      TAUGRID_IMAGE_SOURCE_PATHS=("images/taugrid-portal/Dockerfile" "portal" "core")
      ;;
    tau-core-controller)
      TAUGRID_IMAGE_DOCKERFILE="images/tau-core-controller/Dockerfile"
      TAUGRID_IMAGE_SOURCE_PATHS=("images/tau-core-controller/Dockerfile" "controllers/tau-core" "core")
      ;;
    *)
      echo "unsupported TauGrid image: ${image}" >&2
      return 2
      ;;
  esac
  TAUGRID_IMAGE_REPOSITORY="$image"
  TAUGRID_IMAGE_CONTEXT="."
}

taugrid_image_names() {
  printf '%s\n' tau taugrid-portal tau-core-controller
}
