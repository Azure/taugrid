#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd -P)"

usage() {
  cat <<'EOF'
Usage: validate-image-build-contract.sh <make-directory> <image-name>

Validate that an image Makefile's docker-push target uses the repository-root
build context, the image-local Dockerfile, and the requested image tag. The
target is inspected with make -n; no image is built or pushed.
EOF
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi

if [[ $# -ne 2 ]]; then
  usage >&2
  exit 2
fi

readonly MAKE_DIRECTORY="$1"
readonly IMAGE_NAME="$2"

if [[ "$MAKE_DIRECTORY" == /* ]]; then
  echo "make directory must be relative to the repository root: ${MAKE_DIRECTORY}" >&2
  exit 2
fi
if [[ ! "$IMAGE_NAME" =~ ^[a-z0-9][a-z0-9._-]*$ ]]; then
  echo "invalid image name: ${IMAGE_NAME}" >&2
  exit 2
fi

readonly MAKE_DIRECTORY_ABS="$(realpath -m -- "${REPO_ROOT}/${MAKE_DIRECTORY}")"
case "${MAKE_DIRECTORY_ABS}/" in
  "${REPO_ROOT}/"*) ;;
  *)
    echo "make directory escapes the repository root: ${MAKE_DIRECTORY}" >&2
    exit 2
    ;;
esac
if [[ ! -f "${MAKE_DIRECTORY_ABS}/Makefile" ]]; then
  echo "image Makefile not found: ${MAKE_DIRECTORY}/Makefile" >&2
  exit 2
fi

readonly EXPECTED_IMAGE="example.invalid/${IMAGE_NAME}:ci-contract"
build_command="$({
  make --no-print-directory \
    -C "$MAKE_DIRECTORY_ABS" \
    -n docker-push \
    IMG="$EXPECTED_IMAGE" \
    TAG=ci-contract \
    PLATFORMS=linux/amd64
} 2>&1)" || {
  printf '%s\n' "$build_command" >&2
  exit 1
}

if [[ "$(grep -Fc -- 'docker buildx build' <<<"$build_command")" -ne 1 ]]; then
  echo "docker-push must expand to exactly one docker buildx build command" >&2
  printf '%s\n' "$build_command" >&2
  exit 1
fi
if ! grep -Fq -- '--push' <<<"$build_command"; then
  echo "docker-push does not pass --push" >&2
  printf '%s\n' "$build_command" >&2
  exit 1
fi
if ! grep -Fq -- "--tag ${EXPECTED_IMAGE}" <<<"$build_command"; then
  echo "docker-push does not use the requested image tag ${EXPECTED_IMAGE}" >&2
  printf '%s\n' "$build_command" >&2
  exit 1
fi

dockerfile="$(awk '$1 == "--file" { print $2; exit }' <<<"$build_command")"
if [[ -z "$dockerfile" ]]; then
  echo "docker-push does not declare a Dockerfile with --file" >&2
  printf '%s\n' "$build_command" >&2
  exit 1
fi
readonly DOCKERFILE_ABS="$(realpath -m -- "${MAKE_DIRECTORY_ABS}/${dockerfile}")"
if [[ "$DOCKERFILE_ABS" != "${MAKE_DIRECTORY_ABS}/Dockerfile" ]]; then
  echo "docker-push must use ${MAKE_DIRECTORY}/Dockerfile, got ${dockerfile}" >&2
  exit 1
fi

context="$(awk 'NF { value=$1 } END { print value }' <<<"$build_command")"
readonly CONTEXT_ABS="$(realpath -m -- "${MAKE_DIRECTORY_ABS}/${context}")"
if [[ "$CONTEXT_ABS" != "$REPO_ROOT" ]]; then
  echo "docker-push context must resolve to the repository root, got ${context}" >&2
  exit 1
fi

echo "Validated ${IMAGE_NAME} image build contract."
