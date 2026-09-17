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
readonly EXPECTED_SKAFFOLD_VERSION="v2.24.0"
readonly SKAFFOLD="${SKAFFOLD_BIN:-skaffold}"
readonly AZ="${AZ_BIN:-az}"

usage() {
  cat <<'EOF'
Usage: skaffold-build.sh <developer-namespace> <file-output> [skaffold build flags...]

Build TauGrid local-preview images with ACR Tasks and write Skaffold's immutable
image map to file-output. No local Docker daemon or Kubernetes cluster is used.
EOF
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ $# -lt 2 ]]; then
  usage >&2
  exit 2
fi

readonly DEV_NAMESPACE="$1"
readonly FILE_OUTPUT="$2"
shift 2

if [[ ! "$DEV_NAMESPACE" =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]]; then
  echo "developer namespace must match [a-z0-9][a-z0-9._-]{0,62}" >&2
  exit 2
fi
if [[ "$("$SKAFFOLD" version)" != "$EXPECTED_SKAFFOLD_VERSION" ]]; then
  echo "Skaffold ${EXPECTED_SKAFFOLD_VERSION} is required" >&2
  exit 2
fi

DOCKER_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-docker-config.XXXXXX")"
readonly DOCKER_CONFIG_DIR
cleanup() {
  rm -rf "$DOCKER_CONFIG_DIR"
}
trap cleanup EXIT
chmod 700 "$DOCKER_CONFIG_DIR"

ACCESS_TOKEN="$(
  "$AZ" acr login \
    --name "$ACR_NAME" \
    --expose-token \
    --query accessToken \
    --output tsv
)"
readonly ACCESS_TOKEN
if [[ -z "$ACCESS_TOKEN" ]]; then
  echo "Azure CLI returned an empty ACR access token" >&2
  exit 1
fi
AUTH="$(
  printf '00000000-0000-0000-0000-000000000000:%s' "$ACCESS_TOKEN" |
    base64
)"
readonly AUTH
printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$ACR_LOGIN_SERVER" "$AUTH" \
  >"${DOCKER_CONFIG_DIR}/config.json"
chmod 600 "${DOCKER_CONFIG_DIR}/config.json"

mkdir -p "$(dirname -- "$FILE_OUTPUT")"
DOCKER_CONFIG="$DOCKER_CONFIG_DIR" \
  TAU_DEV_NAMESPACE="$DEV_NAMESPACE" \
  "$SKAFFOLD" build \
    --filename "${REPO_ROOT}/skaffold.yaml" \
    --default-repo "${ACR_LOGIN_SERVER}/dev/${DEV_NAMESPACE}" \
    --file-output "$FILE_OUTPUT" \
    "$@"
