#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

taugrid_image_names() {
  printf '%s\n' tau taugrid-portal tau-core-controller experiment-metrics-collector
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
    experiment-metrics-collector)
      TAUGRID_IMAGE_REPOSITORY=experiment-metrics-collector
      TAUGRID_IMAGE_DOCKERFILE=images/experiment-metrics-collector/Dockerfile
      TAUGRID_IMAGE_SOURCE_PATHS=(images/experiment-metrics-collector/Dockerfile metrics/experiment-metrics-collector core)
      ;;
    *)
      echo "unknown TauGrid image: ${name}" >&2
      return 2
      ;;
  esac
}
