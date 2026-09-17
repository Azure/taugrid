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
EOF
chmod +x "${TEST_ROOT}/az" "${TEST_ROOT}/skaffold"

readonly OUTPUT="${TEST_ROOT}/output/images.json"
readonly ARGS_FILE="${TEST_ROOT}/skaffold.args"
SKAFFOLD_BIN="${TEST_ROOT}/skaffold" \
  AZ_BIN="${TEST_ROOT}/az" \
  SKAFFOLD_ARGS_FILE="$ARGS_FILE" \
  "${REPO_ROOT}/scripts/skaffold-build.sh" tester "$OUTPUT" --quiet

grep -Fq -- "build --filename ${REPO_ROOT}/skaffold.yaml" "$ARGS_FILE"
grep -Fq -- "--default-repo aksairuntime.azurecr.io/dev/tester" "$ARGS_FILE"
grep -Fq -- "--file-output ${OUTPUT}" "$ARGS_FILE"
grep -Fq -- "--quiet" "$ARGS_FILE"

echo "Validated Docker-free Skaffold build wrapper."
