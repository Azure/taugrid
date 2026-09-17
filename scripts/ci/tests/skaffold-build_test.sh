#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../../.." && pwd -P)"
readonly REPO_ROOT
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-skaffold-wrapper-test.XXXXXX")"
readonly TEST_ROOT
cleanup() {
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

cat >"${TEST_ROOT}/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "acr login --name aksairuntime --expose-token --query accessToken --output tsv" ]]
printf 'test-access-token\n'
EOF

cat >"${TEST_ROOT}/skaffold" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "version" ]]; then
  printf 'v2.24.0\n'
  exit 0
fi
[[ "${1:-}" == "build" ]]
[[ "$TAU_DEV_NAMESPACE" == "tester" ]]
[[ -f "$DOCKER_CONFIG/config.json" ]]
grep -Fq -- '"aksairuntime.azurecr.io"' "$DOCKER_CONFIG/config.json"
printf '%s\n' "$*" >"$SKAFFOLD_ARGS_FILE"
cat >"$SKAFFOLD_OUTPUT_FILE" <<'JSON'
{
  "builds": [
    {
      "imageName": "tau",
      "tag": "aksairuntime.azurecr.io/dev/tester/tau:input-digest@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    },
    {
      "imageName": "taugrid-portal",
      "tag": "aksairuntime.azurecr.io/dev/tester/taugrid-portal:input-digest@sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    {
      "imageName": "tau-core-controller",
      "tag": "aksairuntime.azurecr.io/dev/tester/tau-core-controller:input-digest@sha256:3333333333333333333333333333333333333333333333333333333333333333"
    }
  ]
}
JSON
EOF
chmod +x "${TEST_ROOT}/az" "${TEST_ROOT}/skaffold"

readonly OUTPUT="${TEST_ROOT}/output/images.json"
readonly ARGS_FILE="${TEST_ROOT}/skaffold.args"
SKAFFOLD_BIN="${TEST_ROOT}/skaffold" \
  AZ_BIN="${TEST_ROOT}/az" \
  SKAFFOLD_ARGS_FILE="$ARGS_FILE" \
  SKAFFOLD_OUTPUT_FILE="$OUTPUT" \
  "${REPO_ROOT}/scripts/skaffold-build.sh" tester "$OUTPUT"

grep -Fq -- "build --filename ${REPO_ROOT}/skaffold.yaml" "$ARGS_FILE"
grep -Fq -- "--default-repo aksairuntime.azurecr.io/dev/tester" "$ARGS_FILE"
grep -Fq -- "--file-output ${OUTPUT}" "$ARGS_FILE"
if grep -Eq -- '(^| )(-m|--module)(=| |$)' "$ARGS_FILE"; then
  echo "wrapper selected a Skaffold module" >&2
  exit 1
fi

python3 - "$OUTPUT" <<'PY'
import json
import sys

builds = json.load(open(sys.argv[1]))["builds"]
assert {build["imageName"] for build in builds} == {
    "tau",
    "taugrid-portal",
    "tau-core-controller",
}
PY

if SKAFFOLD_BIN="${TEST_ROOT}/skaffold" \
  AZ_BIN="${TEST_ROOT}/az" \
  SKAFFOLD_ARGS_FILE="$ARGS_FILE" \
  SKAFFOLD_OUTPUT_FILE="$OUTPUT" \
  "${REPO_ROOT}/scripts/skaffold-build.sh" tester "$OUTPUT" --module tau \
  >/dev/null 2>&1; then
  echo "wrapper accepted component-selection arguments" >&2
  exit 1
fi

cat >"${TEST_ROOT}/skaffold-incomplete" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "version" ]]; then
  printf 'v2.24.0\n'
  exit 0
fi
cat >"$SKAFFOLD_OUTPUT_FILE" <<'JSON'
{"builds":[{"imageName":"tau","tag":"aksairuntime.azurecr.io/dev/tester/tau:input-digest@sha256:1111111111111111111111111111111111111111111111111111111111111111"}]}
JSON
EOF
chmod +x "${TEST_ROOT}/skaffold-incomplete"

if SKAFFOLD_BIN="${TEST_ROOT}/skaffold-incomplete" \
  AZ_BIN="${TEST_ROOT}/az" \
  SKAFFOLD_OUTPUT_FILE="$OUTPUT" \
  "${REPO_ROOT}/scripts/skaffold-build.sh" tester "$OUTPUT" \
  >/dev/null 2>&1; then
  echo "wrapper accepted an incomplete image set" >&2
  exit 1
fi

echo "Validated complete Docker-free Skaffold image-set wrapper."
