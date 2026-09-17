#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../../.." && pwd -P)"
readonly REPO_ROOT
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/taugrid-acr-test.XXXXXX")"
readonly TEST_ROOT
cleanup() {
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

cat >"${TEST_ROOT}/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$1 $2 $3" == "acr repository show" ]]; then
  image=""
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--image" ]]; then
      image="$2"
      break
    fi
    shift
  done
  case "$image" in
    */tau:*) printf 'sha256:%064d\n' 1 ;;
    */taugrid-portal:*) printf 'sha256:%064d\n' 2 ;;
    */tau-core-controller:*) printf 'sha256:%064d\n' 3 ;;
    *) exit 1 ;;
  esac
  exit 0
fi

[[ "$1 $2" == "acr build" ]]
context="$(cd -- "${!#}" && pwd -P)"
[[ -d "$context" ]]
[[ ! -e "$context/.git" ]]
dockerfile=""
image=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --file) dockerfile="$2"; shift 2 ;;
    --image) image="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -f "$context/$dockerfile" ]]
! grep -Fq -- 'FROM --platform=$BUILDPLATFORM' "$context/$dockerfile"
! grep -Fq -- 'RUN --mount=' "$context/$dockerfile"
printf '%s\n' "$image" >>"$AZ_BUILDS_FILE"
EOF
chmod +x "${TEST_ROOT}/az"

readonly OUTPUT="${TEST_ROOT}/images.json"
readonly BUILDS="${TEST_ROOT}/builds"
AZ_BIN="${TEST_ROOT}/az" AZ_BUILDS_FILE="$BUILDS" \
  "${REPO_ROOT}/scripts/acr-build-images.sh" \
  --namespace tester \
  --output "$OUTPUT" \
  tau taugrid-portal tau

[[ "$(awk 'END { print NR }' "$BUILDS")" == "2" ]]
grep -Eq '^dev/tester/tau:dev-' "$BUILDS"
grep -Eq '^dev/tester/taugrid-portal:dev-' "$BUILDS"

python3 - "$OUTPUT" <<'PY'
import json
import re
import sys

document = json.load(open(sys.argv[1]))
assert document["schemaVersion"] == "taugrid.azure.com/acr-build-output/v1"
assert document["registry"] == "aksairuntime.azurecr.io"
assert [image["name"] for image in document["images"]] == ["tau", "taugrid-portal"]
for image in document["images"]:
    assert re.search(r"@sha256:[0-9a-f]{64}$", image["reference"])
PY

readonly COMPLETE_OUTPUT="${TEST_ROOT}/complete-images.json"
: >"$BUILDS"
AZ_BIN="${TEST_ROOT}/az" AZ_BUILDS_FILE="$BUILDS" \
  "${REPO_ROOT}/scripts/acr-build-images.sh" \
  --namespace pr-123 \
  --output "$COMPLETE_OUTPUT" \
  all >/dev/null
python3 "${REPO_ROOT}/scripts/ci/validate-acr-build-output.py" \
  "$COMPLETE_OUTPUT" \
  pr-123
python3 - "$COMPLETE_OUTPUT" "${TEST_ROOT}/mutable-images.json" <<'PY'
import json
import sys

document = json.load(open(sys.argv[1]))
document["images"][0]["reference"] = document["images"][0]["reference"].split("@")[0]
json.dump(document, open(sys.argv[2], "w"))
PY
if python3 "${REPO_ROOT}/scripts/ci/validate-acr-build-output.py" \
  "${TEST_ROOT}/mutable-images.json" \
  pr-123 >/dev/null 2>&1; then
  echo "output validator accepted a mutable image reference" >&2
  exit 1
fi

: >"$BUILDS"
AZ_BIN="${TEST_ROOT}/az" AZ_BUILDS_FILE="$BUILDS" \
  "${REPO_ROOT}/scripts/acr-build-images.sh" --namespace tester all >/dev/null
[[ "$(awk 'END { print NR }' "$BUILDS")" == "3" ]]

if AZ_BIN="${TEST_ROOT}/az" AZ_BUILDS_FILE="$BUILDS" \
  "${REPO_ROOT}/scripts/acr-build-images.sh" --namespace tester unknown \
  >/dev/null 2>&1; then
  echo "builder accepted an unknown image" >&2
  exit 1
fi

echo "Validated explicit ACR image builds and output contract."
