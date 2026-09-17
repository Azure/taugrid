#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly ACR_NAME="aksairuntime"
readonly ACR_LOGIN_SERVER="${ACR_NAME}.azurecr.io"

usage() {
  cat <<'EOF'
Usage: skaffold-acr-build.sh <tau|taugrid-portal|tau-core-controller>

Build the Skaffold-provided IMAGE with Azure Container Registry Tasks. Skaffold
must set IMAGE, PUSH_IMAGE, BUILD_CONTEXT, and PLATFORMS. The caller must set
TAU_DEV_NAMESPACE to the developer namespace used in --default-repo.
EOF
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ $# -ne 1 ]]; then
  usage >&2
  exit 2
fi

readonly ARTIFACT="$1"
readonly IMAGE_REF="${IMAGE:?Skaffold must set IMAGE}"
readonly BUILD_ROOT="${BUILD_CONTEXT:?Skaffold must set BUILD_CONTEXT}"
readonly DEV_NAMESPACE="${TAU_DEV_NAMESPACE:?set TAU_DEV_NAMESPACE for the developer image namespace}"
readonly AZ="${AZ_BIN:-az}"

if [[ "${PUSH_IMAGE:-}" != "true" ]]; then
  echo "ACR Tasks builds require PUSH_IMAGE=true; local Docker builds are not supported" >&2
  exit 2
fi
if [[ "${PLATFORMS:-}" != "linux/amd64" ]]; then
  echo "ACR Tasks preview builds require PLATFORMS=linux/amd64, got ${PLATFORMS:-<unset>}" >&2
  exit 2
fi
if [[ ! "$DEV_NAMESPACE" =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]]; then
  echo "TAU_DEV_NAMESPACE must match [a-z0-9][a-z0-9._-]{0,62}" >&2
  exit 2
fi
if [[ ! -d "$BUILD_ROOT/.git" && ! -f "$BUILD_ROOT/.git" ]]; then
  echo "BUILD_CONTEXT is not a Git worktree root: ${BUILD_ROOT}" >&2
  exit 2
fi
if [[ "$IMAGE_REF" != "${ACR_LOGIN_SERVER}/"* ]]; then
  echo "IMAGE must target ${ACR_LOGIN_SERVER}, got ${IMAGE_REF}" >&2
  exit 2
fi

readonly REPOSITORY_AND_TAG="${IMAGE_REF#"${ACR_LOGIN_SERVER}"/}"
if [[ "$REPOSITORY_AND_TAG" != *:* ]]; then
  echo "IMAGE must include a tag: ${IMAGE_REF}" >&2
  exit 2
fi
readonly REPOSITORY="${REPOSITORY_AND_TAG%:*}"
readonly TAG="${REPOSITORY_AND_TAG##*:}"
readonly EXPECTED_REPOSITORY="dev/${DEV_NAMESPACE}/${ARTIFACT}"
if [[ "$REPOSITORY" != "$EXPECTED_REPOSITORY" ]]; then
  echo "IMAGE repository must be ${ACR_LOGIN_SERVER}/${EXPECTED_REPOSITORY}, got ${IMAGE_REF}" >&2
  exit 2
fi
if [[ ! "$TAG" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
  echo "IMAGE has an invalid OCI tag: ${TAG}" >&2
  exit 2
fi

case "$ARTIFACT" in
  tau)
    readonly DOCKERFILE="images/tau/Dockerfile"
    SOURCE_PATHS=("images/tau/Dockerfile" "cli" "core")
    ;;
  taugrid-portal)
    readonly DOCKERFILE="images/taugrid-portal/Dockerfile"
    SOURCE_PATHS=("images/taugrid-portal/Dockerfile" "portal" "core")
    ;;
  tau-core-controller)
    readonly DOCKERFILE="images/tau-core-controller/Dockerfile"
    SOURCE_PATHS=("images/tau-core-controller/Dockerfile" "controllers/tau-core" "core")
    ;;
  *)
    echo "unsupported Skaffold artifact: ${ARTIFACT}" >&2
    usage >&2
    exit 2
    ;;
esac

STAGING_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-acr-build.XXXXXX")"
readonly STAGING_ROOT
readonly FILE_LIST="${STAGING_ROOT}/files"
readonly CONTEXT_DIR="${STAGING_ROOT}/context"
cleanup() {
  rm -rf "$STAGING_ROOT"
}
trap cleanup EXIT
mkdir -p "$CONTEXT_DIR"

git -C "$BUILD_ROOT" ls-files -z --cached --others --exclude-standard -- "${SOURCE_PATHS[@]}" >"$FILE_LIST"
if [[ ! -s "$FILE_LIST" ]]; then
  echo "no source files found for ${ARTIFACT}" >&2
  exit 1
fi
tar -C "$BUILD_ROOT" --null -T "$FILE_LIST" -cf - | tar -C "$CONTEXT_DIR" -xf -
if [[ ! -f "$CONTEXT_DIR/$DOCKERFILE" ]]; then
  echo "staged context is missing ${DOCKERFILE}" >&2
  exit 1
fi

# ACR Tasks selects one target platform with --platform. Its dependency scanner
# cannot parse BuildKit's variable FROM platform syntax, which is redundant for
# these single-platform preview builds.
awk '{ sub(/^FROM --platform=\$BUILDPLATFORM /, "FROM "); print }' \
  "$CONTEXT_DIR/$DOCKERFILE" >"${CONTEXT_DIR}/${DOCKERFILE}.acr"
mv "${CONTEXT_DIR}/${DOCKERFILE}.acr" "$CONTEXT_DIR/$DOCKERFILE"
python3 - "$CONTEXT_DIR/$DOCKERFILE" <<'PY'
import pathlib
import re
import sys

dockerfile = pathlib.Path(sys.argv[1])
content = dockerfile.read_text()
content = re.sub(
    r"RUN (?:(?:--mount=\S+)[ \t]*(?:\\\n[ \t]*)?)+",
    "RUN ",
    content,
)
dockerfile.write_text(content)
PY

BUILD_ARGS=()
if [[ "$ARTIFACT" == "tau" || "$ARTIFACT" == "taugrid-portal" ]]; then
  COMMIT="$(git -C "$BUILD_ROOT" rev-parse --short=12 HEAD)"
  readonly COMMIT
  DATE="$(git -C "$BUILD_ROOT" show -s --format=%cI HEAD)"
  readonly DATE
  BUILD_ARGS=(
    --build-arg "VERSION=${TAG}"
    --build-arg "COMMIT=${COMMIT}"
    --build-arg "DATE=${DATE}"
  )
fi

(
  cd "$CONTEXT_DIR"
  "$AZ" acr build \
    --registry "$ACR_NAME" \
    --image "${REPOSITORY}:${TAG}" \
    --file "$DOCKERFILE" \
    --platform linux/amd64 \
    "${BUILD_ARGS[@]}" \
    .
)
