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

assert_equivalent() {
  assert_not_older "$1" "$2"
  assert_not_older "$2" "$1"
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
assert_older 1.2.3-rc 1.2.3
assert_older 1.2.3-rc.1 1.2.3-rc.2
assert_older 1.2.3-999999999999999999999999999999999999 \
  1.2.3-1000000000000000000000000000000000000
assert_older 1.2.3-999999999999999999999999999999999999 \
  1.2.3-alpha

# Official SemVer 2.0.0 precedence example.
assert_older 1.0.0-alpha 1.0.0-alpha.1
assert_older 1.0.0-alpha.1 1.0.0-alpha.beta
assert_older 1.0.0-alpha.beta 1.0.0-beta
assert_older 1.0.0-beta 1.0.0-beta.2
assert_older 1.0.0-beta.2 1.0.0-beta.11
assert_older 1.0.0-beta.11 1.0.0-rc.1
assert_older 1.0.0-rc.1 1.0.0

assert_not_older 1.2.3 1.2.3
assert_not_older 1.2.4 1.2.3
assert_not_older 2.0.0 1.99.99
assert_not_older 9223372036854775808.0.0 9223372036854775807.0.0
assert_not_older 1.2.3-rc.2 1.2.3-rc.1
assert_not_older 1.2.3-alpha 1.2.3-999999999999999999999999999999999999
assert_equivalent 1.2.3+build.1 1.2.3+build.2
assert_equivalent 1.2.3-rc.1+build.1 1.2.3-rc.1+build.2
assert_equivalent 1.2.3+999999999999999999999999999999999999 1.2.3

assert_invalid 1.2 1.2.3 "core must be MAJOR.MINOR.PATCH"
assert_invalid 1.2.3.4 1.2.3 "core must be MAJOR.MINOR.PATCH"
assert_invalid 1.02.3 1.2.3 "no leading zeros"
assert_invalid 1.2.03 1.2.3 "no leading zeros"
assert_invalid v1.2.3 1.2.3 "core must be MAJOR.MINOR.PATCH"
assert_invalid 1.2.3- 1.2.3 "prerelease must contain non-empty"
assert_invalid 1.2.3-alpha..1 1.2.3 "prerelease must contain non-empty"
assert_invalid 1.2.3-alpha.01 1.2.3 "must not contain leading zeros"
assert_invalid 1.2.3-alpha_1 1.2.3 "prerelease identifier"
assert_invalid 1.2.3+ 1.2.3 "build metadata must contain non-empty"
assert_invalid 1.2.3+build..1 1.2.3 "build metadata must contain non-empty"
assert_invalid 1.2.3+build_1 1.2.3 "build metadata identifier"
assert_invalid 1.2.3+build+1 1.2.3 "build metadata must contain non-empty"
assert_invalid 1.2.3 1.2.3-01 "reference chart version"

retention_chart="$TEST_ROOT/kueue"
retention_archive="$TEST_ROOT/kueue-0.19.0.tgz"
mkdir -p "$retention_chart/templates/crd"
cat >"$retention_chart/Chart.yaml" <<'EOF'
apiVersion: v2
name: kueue
version: 0.19.0
EOF
for name in workloads clusterqueues; do
  cat >"$retention_chart/templates/crd/${name}.yaml" <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.20.1
  name: ${name}.kueue.x-k8s.io
EOF
done
COPYFILE_DISABLE=1 tar -czf "$retention_archive" -C "$TEST_ROOT" kueue
preserve_kueue_crd_retention "$retention_archive"
for name in workloads clusterqueues; do
  tar -xOf "$retention_archive" "kueue/templates/crd/${name}.yaml" |
    grep -Fq 'helm.sh/resource-policy: keep' ||
    fail "Kueue ${name} CRD did not retain the Helm keep policy"
done

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
