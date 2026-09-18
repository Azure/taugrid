#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/image-specs.sh"

while IFS= read -r image; do
  taugrid_image_spec "${image}"
  make_directory="$(dirname -- "${TAUGRID_IMAGE_DOCKERFILE}")"
  "${REPO_ROOT}/scripts/ci/validate-image-build-contract.sh" "${make_directory}" "${image}"
done < <(taugrid_image_names)

echo "Image build contract tests passed"
