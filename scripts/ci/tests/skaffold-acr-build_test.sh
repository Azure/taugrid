#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../../.." && pwd -P)"
readonly REPO_ROOT
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-skaffold-test.XXXXXX")"
readonly TEST_ROOT
cleanup() {
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

cat >"${TEST_ROOT}/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

context="$(cd -- "${!#}" && pwd -P)"
[[ -d "$context" ]]
[[ ! -e "$context/.git" ]]
[[ -f "$context/$EXPECTED_DOCKERFILE" ]]
if grep -Fq -- 'FROM --platform=$BUILDPLATFORM' "$context/$EXPECTED_DOCKERFILE"; then
  echo "staged Dockerfile retained unsupported BUILDPLATFORM syntax" >&2
  exit 1
fi
if grep -Fq -- 'RUN --mount=' "$context/$EXPECTED_DOCKERFILE"; then
  echo "staged Dockerfile retained unsupported BuildKit cache mounts" >&2
  exit 1
fi

actual_top_levels="$(
  find "$context" -mindepth 1 -maxdepth 1 -print |
    sed 's#.*/##' |
    sort |
    paste -sd, -
)"
[[ "$actual_top_levels" == "$EXPECTED_TOP_LEVELS" ]]
printf '%s\n' "$*" >"$AZ_ARGS_FILE"
EOF
chmod +x "${TEST_ROOT}/az"

run_builder_test() {
  local artifact="$1"
  local dockerfile="$2"
  local top_levels="$3"
  local args_file="${TEST_ROOT}/${artifact}.args"

  IMAGE="aksairuntime.azurecr.io/dev/tester/${artifact}:input-digest" \
    PUSH_IMAGE=true \
    BUILD_CONTEXT="$REPO_ROOT" \
    PLATFORMS=linux/amd64 \
    TAU_DEV_NAMESPACE=tester \
    AZ_BIN="${TEST_ROOT}/az" \
    EXPECTED_DOCKERFILE="$dockerfile" \
    EXPECTED_TOP_LEVELS="$top_levels" \
    AZ_ARGS_FILE="$args_file" \
    "${REPO_ROOT}/scripts/skaffold-acr-build.sh" "$artifact"

  grep -Fq -- "acr build --registry aksairuntime" "$args_file"
  grep -Fq -- "--image dev/tester/${artifact}:input-digest" "$args_file"
  grep -Fq -- "--file ${dockerfile}" "$args_file"
  grep -Fq -- "--platform linux/amd64" "$args_file"
}

run_builder_test tau images/tau/Dockerfile "cli,core,images"
run_builder_test taugrid-portal images/taugrid-portal/Dockerfile "core,images,portal"
run_builder_test tau-core-controller images/tau-core-controller/Dockerfile "controllers,core,images"

if IMAGE="aksairuntime.azurecr.io/dev/tester/tau:bad" \
  PUSH_IMAGE=false \
  BUILD_CONTEXT="$REPO_ROOT" \
  PLATFORMS=linux/amd64 \
  TAU_DEV_NAMESPACE=tester \
  AZ_BIN="${TEST_ROOT}/az" \
  "${REPO_ROOT}/scripts/skaffold-acr-build.sh" tau >/dev/null 2>&1; then
  echo "builder accepted PUSH_IMAGE=false" >&2
  exit 1
fi

if IMAGE="example.invalid/dev/tester/tau:bad" \
  PUSH_IMAGE=true \
  BUILD_CONTEXT="$REPO_ROOT" \
  PLATFORMS=linux/amd64 \
  TAU_DEV_NAMESPACE=tester \
  AZ_BIN="${TEST_ROOT}/az" \
  "${REPO_ROOT}/scripts/skaffold-acr-build.sh" tau >/dev/null 2>&1; then
  echo "builder accepted a non-aksairuntime registry" >&2
  exit 1
fi

echo "Validated bounded ACR Tasks contexts for all Skaffold artifacts."
