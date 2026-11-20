#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

TEST_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEST_DIR
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/publish-helm-charts-test.XXXXXX")"
readonly TEST_ROOT
readonly CONTROL_ROOT="${TEST_ROOT}/control"
readonly SOURCE_ROOT="${TEST_ROOT}/source"
readonly PACKAGE_DIR="${TEST_ROOT}/packages"
readonly PUBLICATION_STATUS_PATH="${TEST_ROOT}/publication/publication.json"
readonly COMMAND_LOG="${TEST_ROOT}/commands.log"
readonly MALICIOUS_SENTINEL="${TEST_ROOT}/source-script-executed"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  echo "publish-helm-charts test failed: $*" >&2
  exit 1
}

assert_output_contains() {
  local output="$1"
  local expected="$2"
  [[ "$output" == *"$expected"* ]] ||
    fail "expected output to contain '$expected', got: $output"
}

assert_log_contains() {
  local expected="$1"
  grep -Fq "$expected" "$COMMAND_LOG" ||
    fail "expected command log to contain '$expected'"
}

assert_log_excludes() {
  local unexpected="$1"
  if grep -Fq "$unexpected" "$COMMAND_LOG"; then
    fail "expected command log not to contain '$unexpected'"
  fi
}

assert_log_count() {
  local expected_count="$1"
  local pattern="$2"
  local actual_count
  actual_count=$(grep -Fc "$pattern" "$COMMAND_LOG" || true)
  [[ "$actual_count" -eq "$expected_count" ]] ||
    fail "expected $expected_count occurrences of '$pattern', got $actual_count"
}

mkdir -p \
  "${CONTROL_ROOT}/scripts/ci" \
  "${SOURCE_ROOT}/charts/tau-core-controller" \
  "${SOURCE_ROOT}/charts/gpu-monitoring" \
  "${SOURCE_ROOT}/charts/taugrid-core" \
  "${SOURCE_ROOT}/charts/adx-mon" \
  "${SOURCE_ROOT}/charts/taugrid/tests" \
  "$PACKAGE_DIR" \
  "${TEST_ROOT}/bin"
cp "${TEST_DIR}/../publish-helm-charts.sh" "${CONTROL_ROOT}/scripts/ci/"
cp "${TEST_DIR}/../release_artifacts.py" "${CONTROL_ROOT}/scripts/ci/"
cp "${TEST_DIR}/../verify_protected_chart_packages.py" "${CONTROL_ROOT}/scripts/ci/"
printf '#!/usr/bin/env bash\nexit 0\n' >"${CONTROL_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh"
chmod +x \
  "${CONTROL_ROOT}/scripts/ci/publish-helm-charts.sh" \
  "${CONTROL_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh"

write_chart() {
  local name="$1"
  cat >"${SOURCE_ROOT}/charts/${name}/Chart.yaml" <<EOF
apiVersion: v2
name: ${name}
version: 0.3.0
appVersion: 0.3.0
EOF
}

for chart in tau-core-controller gpu-monitoring taugrid-core adx-mon taugrid; do
  write_chart "$chart"
done
cat >"${SOURCE_ROOT}/charts/taugrid/tests/scripts_test.sh" <<EOF
#!/usr/bin/env bash
touch "${MALICIOUS_SENTINEL}"
EOF
chmod +x "${SOURCE_ROOT}/charts/taugrid/tests/scripts_test.sh"
(
  cd "$SOURCE_ROOT"
  git init --quiet -b main
  git config user.name "Release Test"
  git config user.email release@example.invalid
  git add charts
  git commit --quiet -m fixture
)
export RELEASE_SOURCE_COMMIT
RELEASE_SOURCE_COMMIT=$(git -C "$SOURCE_ROOT" rev-parse HEAD)
export RELEASE_TAG=v0.3.0
export RELEASE_VERSION=0.3.0
export PUBLICATION_STATUS_PATH

cat >"${TEST_ROOT}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'helm %s\n' "$*" >>"$COMMAND_LOG"
case "$1 $2" in
  "push "*)
    ;;
  "pull "*)
    name="${2##*/}"
    cp "${REMOTE_PACKAGE_DIR:-$PACKAGE_DIR}/${name}-${4}.tgz" "${6}/"
    ;;
  "package "*)
    name="${2##*/}"
    version=$(awk '$1 == "version:" {print $2}' "$2/Chart.yaml")
    tar -czf "${4}/${name}-${version}.tgz" -C "$(dirname "$2")" "$name"
    ;;
  "repo "*|"dependency build"|"lint "*|"template "*)
    ;;
  *)
    echo "unexpected helm command: $*" >&2
    exit 64
    ;;
esac
EOF
chmod +x "${TEST_ROOT}/bin/helm"

cat >"${TEST_ROOT}/bin/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'az %s\n' "$*" >>"$COMMAND_LOG"
if [[ "$1 $2" == "acr login" ]]; then
  exit 0
fi
if [[ "$1 $2 $3" == "acr repository show-tags" ]]; then
  repository=""
  while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == "--repository" ]]; then
      repository="$2"
      break
    fi
    shift
  done
  chart="${repository##*/}"
  if [[ "${FAKE_ERROR_CHART:-}" == "$chart" ]]; then
    echo "authorization failed" >&2
    exit 1
  fi
  if [[ "${FAKE_UNPUBLISHED_CHART:-}" == "$chart" ]]; then
    echo "0.0.1"
  else
    echo "0.3.0"
  fi
  exit 0
fi
if [[ "$1 $2 $3" == "acr repository show" ]]; then
  echo "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  exit 0
fi
echo "unexpected az command: $*" >&2
exit 64
EOF
chmod +x "${TEST_ROOT}/bin/az"

export COMMAND_LOG
export PACKAGE_DIR
export ACR_NAME=testacr
export TAUGRID_SOURCE_ROOT="$SOURCE_ROOT"
PATH="${TEST_ROOT}/bin:${PATH}"
export PATH

"${CONTROL_ROOT}/scripts/ci/publish-helm-charts.sh" package "$PACKAGE_DIR"
[[ -s "${PACKAGE_DIR}/release-manifest.json" ]] ||
  fail "package must seal release metadata"
[[ ! -e "$MALICIOUS_SENTINEL" ]] ||
  fail "release-selected chart scripts must not execute"
rm -rf "$SOURCE_ROOT"
unset TAUGRID_SOURCE_ROOT

run_publish() {
  "${CONTROL_ROOT}/scripts/ci/publish-helm-charts.sh" publish "$PACKAGE_DIR"
}

: >"$COMMAND_LOG"
existing_output=$(run_publish)
for chart in tau-core-controller gpu-monitoring taugrid-core adx-mon taugrid; do
  assert_output_contains "$existing_output" \
    "Chart ${chart}:0.3.0 already exists with matching source provenance; reusing."
done
assert_log_excludes "az acr login"
assert_log_excludes "helm push"
assert_log_count 5 "helm pull"

: >"$COMMAND_LOG"
export FAKE_UNPUBLISHED_CHART=taugrid
new_version_output=$(run_publish)
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$new_version_output" \
  "Published and verified taugrid:0.3.0 at sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
assert_log_count 1 "az acr login"
assert_log_count 1 "helm push"
assert_log_contains \
  "helm push ${PACKAGE_DIR}/taugrid-0.3.0.tgz oci://testacr.azurecr.io/public/aks/ai-runtime/helm"
python3 - "$PUBLICATION_STATUS_PATH" "$RELEASE_SOURCE_COMMIT" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as status_file:
    status = json.load(status_file)
assert status["schemaVersion"] == 1
assert status["releaseTag"] == "v0.3.0"
assert status["releaseVersion"] == "0.3.0"
assert status["releaseSourceCommit"] == sys.argv[2]
assert len(status["charts"]) == 5
assert {chart["name"] for chart in status["charts"]} == {
    "tau-core-controller",
    "gpu-monitoring",
    "taugrid-core",
    "adx-mon",
    "taugrid",
}
PY

: >"$COMMAND_LOG"
export FAKE_ERROR_CHART=tau-core-controller
if lookup_error_output=$(run_publish 2>&1); then
  fail "unexpected registry lookup success"
fi
unset FAKE_ERROR_CHART
assert_output_contains "$lookup_error_output" "authorization failed"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
mkdir "${TEST_ROOT}/remote"
cp "${PACKAGE_DIR}/"*.tgz "${TEST_ROOT}/remote/"
export REMOTE_PACKAGE_DIR="${TEST_ROOT}/remote"
python3 - <<'PY'
import io
import os
from pathlib import Path
import re
import tarfile

path = Path(os.environ["REMOTE_PACKAGE_DIR"]) / "tau-core-controller-0.3.0.tgz"
with tarfile.open(path) as archive:
    data = archive.extractfile("tau-core-controller/Chart.yaml").read()
data, replacements = re.subn(
    rb'(org\.opencontainers\.image\.revision:[ \t]*")[0-9a-f]{40}(")',
    rb"\g<1>" + b"d" * 40 + rb"\g<2>",
    data,
    count=1,
)
if replacements != 1:
    raise SystemExit("release revision annotation not found")
with tarfile.open(path, "w:gz") as archive:
    member = tarfile.TarInfo("tau-core-controller/Chart.yaml")
    member.size = len(data)
    archive.addfile(member, io.BytesIO(data))
PY
if conflict_output=$(run_publish 2>&1); then
  fail "conflicting immutable release chart unexpectedly reused"
fi
unset REMOTE_PACKAGE_DIR
assert_output_contains "$conflict_output" "chart release provenance mismatch"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

echo "publish-helm-charts tests passed"
