#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

taugrid_image_names() {
  printf '%s\n' tau taugrid-portal tau-core-controller
}

taugrid_image_spec() {
  local name="$1"

  TAUGRID_IMAGE_CONTEXT=.
  case "${name}" in
    tau)
      TAUGRID_IMAGE_REPOSITORY=tau
      TAUGRID_IMAGE_DOCKERFILE=images/tau/Dockerfile
      TAUGRID_IMAGE_SOURCE_PATHS=(images/tau/Dockerfile cli core)
      ;;
    taugrid-portal)
      TAUGRID_IMAGE_REPOSITORY=taugrid-portal
      TAUGRID_IMAGE_DOCKERFILE=images/taugrid-portal/Dockerfile
      TAUGRID_IMAGE_SOURCE_PATHS=(images/taugrid-portal/Dockerfile portal core)
      ;;
    tau-core-controller)
      TAUGRID_IMAGE_REPOSITORY=tau-core-controller
      TAUGRID_IMAGE_DOCKERFILE=images/tau-core-controller/Dockerfile
      TAUGRID_IMAGE_SOURCE_PATHS=(images/tau-core-controller/Dockerfile controllers/tau-core core)
      ;;
    *)
      echo "unknown TauGrid image: ${name}" >&2
      return 2
      ;;
  esac
}
