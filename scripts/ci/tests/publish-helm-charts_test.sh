#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

TEST_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEST_DIR
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/publish-helm-charts-test.XXXXXX")"
readonly TEST_ROOT
readonly ISOLATED_REPO="${TEST_ROOT}/repo"
readonly PACKAGE_DIR="${ISOLATED_REPO}/packages"
readonly COMMAND_LOG="${TEST_ROOT}/commands.log"
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
  "${ISOLATED_REPO}/scripts/ci" \
  "${ISOLATED_REPO}/charts/tau-core-controller" \
  "${ISOLATED_REPO}/charts/gpu-monitoring" \
  "${ISOLATED_REPO}/charts/taugrid-core" \
  "${ISOLATED_REPO}/charts/adx-mon" \
  "${ISOLATED_REPO}/charts/taugrid" \
  "$PACKAGE_DIR" \
  "${TEST_ROOT}/bin"
cp "${TEST_DIR}/../publish-helm-charts.sh" "${ISOLATED_REPO}/scripts/ci/"
cp "${TEST_DIR}/../release_artifacts.py" "${ISOLATED_REPO}/scripts/ci/"

write_chart() {
  local name="$1"
  local version="$2"

  cat >"${ISOLATED_REPO}/charts/${name}/Chart.yaml" <<EOF
apiVersion: v2
name: ${name}
version: ${version}
appVersion: ${version}
EOF
}

write_chart tau-core-controller 0.3.0
write_chart gpu-monitoring 0.1.4
write_chart taugrid-core 0.3.0
write_chart adx-mon 0.3.3
write_chart taugrid 0.3.0
(
  cd "$ISOLATED_REPO"
  git init --quiet
  git add charts
  git -c user.name=Test -c user.email=test@example.invalid commit --quiet -m fixture
)
export RELEASE_SOURCE_COMMIT
RELEASE_SOURCE_COMMIT=$(git -C "$ISOLATED_REPO" rev-parse HEAD)
export RELEASE_VERSION=0.3.0
printf '#!/usr/bin/env bash\nexit 0\n' >"${ISOLATED_REPO}/scripts/ci/vendor-taugrid-dependencies.sh"
chmod +x "${ISOLATED_REPO}/scripts/ci/vendor-taugrid-dependencies.sh"

cat >"${TEST_ROOT}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'helm %s\n' "$*" >>"$COMMAND_LOG"
case "$1 $2" in
  "show chart")
    cat "$3/Chart.yaml"
    ;;
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

cat >"${TEST_ROOT}/bin/yq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

field="${2#.}"
awk -v field="$field" '$1 == field ":" {gsub(/^"|"$/, "", $2); print $2; exit}'
EOF
chmod +x "${TEST_ROOT}/bin/yq"

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
  chart_name="${repository##*/}"

  if [[ "${FAKE_ERROR_CHART:-}" == "$chart_name" ]]; then
    echo "authorization failed" >&2
    exit 1
  fi

  version=$(awk '$1 == "version:" {gsub(/^"|"$/, "", $2); print $2; exit}' \
    "charts/${chart_name}/Chart.yaml")
  if [[ "${FAKE_UNPUBLISHED_CHART:-}" == "$chart_name" ]]; then
    echo "0.0.1"
  else
    echo "$version"
  fi
  exit 0
fi

if [[ "$1 $2 $3" == "acr repository show" ]]; then
  echo "sha256:0123456789abcdef"
  exit 0
fi

echo "unexpected az command: $*" >&2
exit 64
EOF
chmod +x "${TEST_ROOT}/bin/az"

export COMMAND_LOG
export PACKAGE_DIR
export ACR_NAME=testacr
PATH="${TEST_ROOT}/bin:${PATH}"
export PATH

(
  cd "$ISOLATED_REPO"
  scripts/ci/publish-helm-charts.sh package "$PACKAGE_DIR"
)
[[ -s "${PACKAGE_DIR}/release-manifest.json" ]] || fail "package must seal release metadata"

run_publish() {
  (
    cd "$ISOLATED_REPO"
    scripts/ci/publish-helm-charts.sh publish "$PACKAGE_DIR"
  )
}

: >"$COMMAND_LOG"
existing_output=$(run_publish)
for chart in tau-core-controller gpu-monitoring taugrid-core adx-mon taugrid; do
  version=$(awk '$1 == "version:" {print $2; exit}' \
    "${ISOLATED_REPO}/charts/${chart}/Chart.yaml")
  assert_output_contains "$existing_output" \
    "Chart ${chart}:${version} already exists with matching source provenance; reusing."
done
assert_log_excludes "az acr login"
assert_log_excludes "helm push"
assert_log_count 5 "helm pull"

: >"$COMMAND_LOG"
export FAKE_UNPUBLISHED_CHART=taugrid
new_version_output=$(run_publish)
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$new_version_output" \
  "Published and verified taugrid:0.3.0 at sha256:0123456789abcdef"
assert_log_count 1 "az acr login"
assert_log_count 1 "helm push"
assert_log_contains \
  "helm push ${PACKAGE_DIR}/taugrid-0.3.0.tgz oci://testacr.azurecr.io/public/aks/ai-runtime/helm"
assert_log_contains \
  "az acr repository show --name testacr --image public/aks/ai-runtime/helm/taugrid:0.3.0"
assert_log_contains \
  "helm pull oci://testacr.azurecr.io/public/aks/ai-runtime/helm/taugrid --version 0.3.0"

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
mkdir "${ISOLATED_REPO}/remote"
cp "${PACKAGE_DIR}/"*.tgz "${ISOLATED_REPO}/remote/"
export REMOTE_PACKAGE_DIR="${ISOLATED_REPO}/remote"
(
  cd "$ISOLATED_REPO"
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
)
if conflict_output=$(run_publish 2>&1); then
  fail "conflicting immutable release chart unexpectedly reused"
fi
unset REMOTE_PACKAGE_DIR
assert_output_contains "$conflict_output" "chart release provenance mismatch"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
rm -f "${PACKAGE_DIR}/taugrid-0.3.0.tgz"
export FAKE_UNPUBLISHED_CHART=taugrid
if missing_archive_output=$(run_publish 2>&1); then
  fail "publishing without the candidate archive unexpectedly succeeded"
fi
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$missing_archive_output" \
  "taugrid-0.3.0.tgz"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

echo "publish-helm-charts tests passed"
