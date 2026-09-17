#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly REPO_ROOT
readonly ACR_NAME="aksairuntime"
readonly ACR_LOGIN_SERVER="${ACR_NAME}.azurecr.io"
readonly AZ="${AZ_BIN:-az}"

usage() {
  cat <<'EOF'
Usage:
  acr-build-images.sh --namespace <developer-namespace> [--output <file>] \
    <tau|taugrid-portal|tau-core-controller|all> [...]

Build one or more explicitly named TauGrid images with native linux/amd64 ACR
Tasks. "all" selects tau, taugrid-portal, and tau-core-controller. The command
prints each immutable image reference and optionally writes a versioned JSON
result. It does not use Docker, QEMU, Skaffold, or a Kubernetes cluster.
EOF
}

DEV_NAMESPACE=""
OUTPUT_FILE=""
IMAGES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      DEV_NAMESPACE="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      OUTPUT_FILE="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    --*)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      IMAGES+=("$1")
      shift
      ;;
  esac
done

if [[ ! "$DEV_NAMESPACE" =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]]; then
  echo "--namespace must match [a-z0-9][a-z0-9._-]{0,62}" >&2
  exit 2
fi
if [[ ${#IMAGES[@]} -eq 0 ]]; then
  echo "select at least one image or all" >&2
  usage >&2
  exit 2
fi
if [[ -n "$OUTPUT_FILE" ]]; then
  mkdir -p "$(dirname -- "$OUTPUT_FILE")"
  rm -f "$OUTPUT_FILE"
fi

SELECTED=()
declare -A SEEN=()
add_image() {
  local image="$1"
  if [[ -z "${SEEN[$image]:-}" ]]; then
    SELECTED+=("$image")
    SEEN["$image"]=1
  fi
}
for image in "${IMAGES[@]}"; do
  case "$image" in
    tau|taugrid-portal|tau-core-controller)
      add_image "$image"
      ;;
    all)
      add_image tau
      add_image taugrid-portal
      add_image tau-core-controller
      ;;
    *)
      echo "unsupported image: ${image}" >&2
      usage >&2
      exit 2
      ;;
  esac
done

COMMIT="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD)"
readonly COMMIT
DATE="$(git -C "$REPO_ROOT" show -s --format=%cI HEAD)"
readonly DATE
BUILD_TAG="dev-$(date -u +%Y%m%d%H%M%S)-${COMMIT}-$$"
readonly BUILD_TAG
RESULTS_DIR="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-acr-results.XXXXXX")"
readonly RESULTS_DIR
cleanup() {
  rm -rf "$RESULTS_DIR"
}
trap cleanup EXIT

build_image() {
  local image="$1"
  local dockerfile
  local -a source_paths
  local -a build_args=()

  case "$image" in
    tau)
      dockerfile="images/tau/Dockerfile"
      source_paths=("images/tau/Dockerfile" "cli" "core")
      build_args=(
        --build-arg "VERSION=${BUILD_TAG}"
        --build-arg "COMMIT=${COMMIT}"
        --build-arg "DATE=${DATE}"
      )
      ;;
    taugrid-portal)
      dockerfile="images/taugrid-portal/Dockerfile"
      source_paths=("images/taugrid-portal/Dockerfile" "portal" "core")
      build_args=(
        --build-arg "VERSION=${BUILD_TAG}"
        --build-arg "COMMIT=${COMMIT}"
        --build-arg "DATE=${DATE}"
      )
      ;;
    tau-core-controller)
      dockerfile="images/tau-core-controller/Dockerfile"
      source_paths=("images/tau-core-controller/Dockerfile" "controllers/tau-core" "core")
      ;;
  esac

  local staging_root
  staging_root="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-acr-build.XXXXXX")"
  local file_list="${staging_root}/files"
  local context_dir="${staging_root}/context"
  mkdir -p "$context_dir"

  git -C "$REPO_ROOT" ls-files -z --cached --others --exclude-standard \
    -- "${source_paths[@]}" >"$file_list"
  if [[ ! -s "$file_list" ]]; then
    echo "no source files found for ${image}" >&2
    rm -rf "$staging_root"
    return 1
  fi
  tar -C "$REPO_ROOT" --null -T "$file_list" -cf - | tar -C "$context_dir" -xf -

  # ACR Tasks already selects linux/amd64 and its hosted Docker builder does
  # not support variable FROM platforms or BuildKit cache mounts.
  awk '{ sub(/^FROM --platform=\$BUILDPLATFORM /, "FROM "); print }' \
    "$context_dir/$dockerfile" >"${context_dir}/${dockerfile}.acr"
  mv "${context_dir}/${dockerfile}.acr" "$context_dir/$dockerfile"
  python3 - "$context_dir/$dockerfile" <<'PY'
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

  local repository="dev/${DEV_NAMESPACE}/${image}"
  (
    cd "$context_dir"
    "$AZ" acr build \
      --registry "$ACR_NAME" \
      --image "${repository}:${BUILD_TAG}" \
      --file "$dockerfile" \
      --platform linux/amd64 \
      "${build_args[@]}" \
      .
  )
  rm -rf "$staging_root"

  local digest
  digest="$(
    "$AZ" acr repository show \
      --name "$ACR_NAME" \
      --image "${repository}:${BUILD_TAG}" \
      --query digest \
      --output tsv
  )"
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "failed to resolve manifest digest for ${repository}:${BUILD_TAG}" >&2
    return 1
  fi

  local reference="${ACR_LOGIN_SERVER}/${repository}:${BUILD_TAG}@${digest}"
  printf '%s\n' "$reference"
  printf '%s\n%s\n%s\n%s\n' "$image" "$reference" "$BUILD_TAG" "$digest" \
    >"${RESULTS_DIR}/${image}"
}

for image in "${SELECTED[@]}"; do
  build_image "$image"
done

if [[ -n "$OUTPUT_FILE" ]]; then
  python3 - "$RESULTS_DIR" "$OUTPUT_FILE" "$ACR_LOGIN_SERVER" <<'PY'
import json
import pathlib
import sys

results_dir = pathlib.Path(sys.argv[1])
output = pathlib.Path(sys.argv[2])
registry = sys.argv[3]
images = []
for path in sorted(results_dir.iterdir()):
    name, reference, tag, digest = path.read_text().splitlines()
    images.append(
        {
            "name": name,
            "reference": reference,
            "tag": tag,
            "digest": digest,
        }
    )
document = {
    "schemaVersion": "taugrid.azure.com/acr-build-output/v1",
    "registry": registry,
    "images": images,
}
temporary = output.with_suffix(output.suffix + ".tmp")
temporary.write_text(json.dumps(document, indent=2) + "\n")
temporary.replace(output)
PY
fi
