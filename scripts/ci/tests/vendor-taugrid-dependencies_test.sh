#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TEST_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vendor-taugrid-test.XXXXXX")"
readonly HELM_LOG="${TEST_ROOT}/helm.log"
readonly ISOLATED_REPO="${TEST_ROOT}/repo"
readonly TEST_CHART="${ISOLATED_REPO}/charts/taugrid"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  echo "vendor-taugrid-dependencies test failed: $*" >&2
  exit 1
}

assert_older() {
  version_is_older "$1" "$2" ||
    fail "expected $1 to be older than $2"
}

assert_not_older() {
  local status

  set +e
  version_is_older "$1" "$2"
  status=$?
  set -e
  [[ "$status" -eq 1 ]] ||
    fail "expected $1 not to be older than $2, got status $status"
}

assert_invalid() {
  local candidate="$1"
  local reference="$2"
  local expected="$3"
  local output
  local status

  set +e
  output="$(version_is_older "$candidate" "$reference" 2>&1)"
  status=$?
  set -e
  [[ "$status" -eq 2 ]] ||
    fail "expected invalid comparison $candidate / $reference to return 2, got $status"
  case "$output" in
    *"$expected"*) ;;
    *) fail "expected invalid comparison output to contain '$expected', got: $output" ;;
  esac
}

mkdir -p "$TEST_ROOT/bin"
cat >"$TEST_ROOT/bin/helm" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$HELM_LOG"
case "$1 $2" in
  "dependency list")
    cat <<OUTPUT
NAME VERSION REPOSITORY STATUS
gpu-monitoring ${FAKE_DEPENDENCY_VERSION} oci://mcr.microsoft.com/aks/ai-runtime/helm ok
OUTPUT
    ;;
  "pull oci://"*)
    version=""
    destination=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --version)
          version="$2"
          shift 2
          ;;
        --destination)
          destination="$2"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    touch "${destination}/gpu-monitoring-${version}.tgz"
    ;;
  *)
    exit 64
    ;;
esac
EOF
chmod +x "$TEST_ROOT/bin/helm"
export HELM_LOG
PATH="${TEST_ROOT}/bin:${PATH}"
export PATH

# Sourcing defines helpers without invoking Helm or deleting existing artifacts.
mkdir -p \
  "${ISOLATED_REPO}/scripts/ci" \
  "${ISOLATED_REPO}/charts/gpu-monitoring" \
  "${TEST_CHART}/charts"
cp "${TEST_DIR}/../vendor-taugrid-dependencies.sh" "${ISOLATED_REPO}/scripts/ci/"
cat >"${ISOLATED_REPO}/charts/gpu-monitoring/Chart.yaml" <<'EOF'
apiVersion: v2
name: gpu-monitoring
version: 0.1.4
EOF
cat >"$TEST_CHART/Chart.yaml" <<'EOF'
apiVersion: v2
name: taugrid
version: 0.0.0
EOF
touch "$TEST_CHART/charts/source-safety-sentinel"
# shellcheck source=../vendor-taugrid-dependencies.sh
source "${ISOLATED_REPO}/scripts/ci/vendor-taugrid-dependencies.sh"
[[ ! -e "$HELM_LOG" ]] || fail "sourcing the vendoring script invoked Helm"
[[ -f "$TEST_CHART/charts/source-safety-sentinel" ]] ||
  fail "sourcing the vendoring script deleted an existing chart artifact"

assert_older 0.1.3 0.1.4
assert_older 0.9.9 1.0.0
assert_older 1.2.3 1.3.0
assert_older 9223372036854775807.0.0 9223372036854775808.0.0
assert_not_older 1.2.3 1.2.3
assert_not_older 1.2.4 1.2.3
assert_not_older 2.0.0 1.99.99
assert_not_older 9223372036854775808.0.0 9223372036854775807.0.0
assert_invalid 1.2 1.2.3 "expected exactly X.Y.Z"
assert_invalid 1.02.3 1.2.3 "expected exactly X.Y.Z"
assert_invalid v1.2.3 1.2.3 "expected exactly X.Y.Z"
assert_invalid 1.2.3-rc.1 1.2.3 "no prerelease suffix"
assert_invalid 1.2.3 1.2.3+build.1 "no prerelease suffix"

export FAKE_DEPENDENCY_VERSION=0.1.3
vendor_taugrid_dependencies "$TEST_CHART"
[[ -f "$TEST_CHART/charts/gpu-monitoring-0.1.3.tgz" ]] ||
  fail "older pinned gpu-monitoring chart was not pulled"
grep -Fq "pull oci://mcr.microsoft.com/aks/ai-runtime/helm/gpu-monitoring --version 0.1.3" "$HELM_LOG" ||
  fail "older pinned gpu-monitoring chart was not pulled from OCI"

rm -f "$HELM_LOG"
export FAKE_DEPENDENCY_VERSION=0.1.5
if mismatch_output="$(vendor_taugrid_dependencies "$TEST_CHART" 2>&1)"; then
  fail "newer gpu-monitoring dependency mismatch unexpectedly succeeded"
fi
case "$mismatch_output" in
  *"requires gpu-monitoring:0.1.5"*"is 0.1.4"*) ;;
  *) fail "newer mismatch did not explain the required and local versions: $mismatch_output" ;;
esac
if [[ -e "$HELM_LOG" ]] && grep -q '^pull ' "$HELM_LOG"; then
  fail "newer gpu-monitoring mismatch attempted an OCI pull"
fi

echo "vendor-taugrid-dependencies tests passed"
