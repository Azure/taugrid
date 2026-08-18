#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TEST_CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_REPO_ROOT="$(cd -- "${TEST_CHART_DIR}/../.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  echo "assertion failed: $*" >&2
  exit 1
}

mkdir -p "$TEST_ROOT/bin" "$TEST_ROOT/source-probe/charts"
printf 'source guard marker\n' >"$TEST_ROOT/source-probe/charts/marker"
cat >"$TEST_ROOT/bin/helm" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$HELM_CALL_LOG"
exit 99
EOF
chmod +x "$TEST_ROOT/bin/helm"

original_path="$PATH"
export HELM_CALL_LOG="$TEST_ROOT/helm-calls"
export PATH="$TEST_ROOT/bin:$PATH"
# shellcheck source=../../../scripts/ci/vendor-taugrid-dependencies.sh
source "${TEST_REPO_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh" "$TEST_ROOT/source-probe"
export PATH="$original_path"

[[ -f "$TEST_ROOT/source-probe/charts/marker" ]] ||
  fail "sourcing the vendoring helper mutated the chart directory"
[[ ! -e "$HELM_CALL_LOG" ]] ||
  fail "sourcing the vendoring helper executed Helm"

assert_older() {
  local candidate="$1"
  local reference="$2"

  version_is_older "$candidate" "$reference" ||
    fail "expected $candidate to have lower precedence than $reference"
}

assert_not_older() {
  local candidate="$1"
  local reference="$2"
  local status

  if version_is_older "$candidate" "$reference"; then
    fail "expected $candidate not to have lower precedence than $reference"
  else
    status=$?
  fi
  [[ "$status" -eq 1 ]] || fail "expected valid semantic versions: $candidate and $reference"
}

assert_invalid() {
  local candidate="$1"
  local status

  if version_is_older "$candidate" "1.0.0" >/dev/null 2>&1; then
    fail "expected invalid semantic version to be rejected: $candidate"
  else
    status=$?
  fi
  [[ "$status" -eq 2 ]] || fail "expected status 2 for invalid semantic version $candidate, got $status"
}

assert_equivalent() {
  assert_not_older "$1" "$2"
  assert_not_older "$2" "$1"
}

assert_older "0.1.3" "0.1.4"
assert_not_older "0.1.4" "0.1.4"
assert_not_older "0.2.0" "0.1.4"
assert_older "999999999999999999999.0.0" "1000000000000000000000.0.0"
assert_not_older "1000000000000000000000.0.0" "999999999999999999999.0.0"

assert_older "0.1.4-rc" "0.1.4"
assert_older "0.1.4-rc.1" "0.1.4-rc.2"
assert_older "0.1.4-999999999999999999999" "0.1.4-1000000000000000000000"
assert_older "0.1.4-1" "0.1.4-rc"
assert_older "0.1.4-rc" "0.1.4-rc.1"
assert_older "1.0.0-alpha" "1.0.0-alpha.1"
assert_older "1.0.0-alpha.1" "1.0.0-alpha.beta"
assert_older "1.0.0-alpha.beta" "1.0.0-beta"
assert_older "1.0.0-beta" "1.0.0-beta.2"
assert_older "1.0.0-beta.2" "1.0.0-beta.11"
assert_older "1.0.0-beta.11" "1.0.0-rc.1"
assert_older "1.0.0-rc.1" "1.0.0"
assert_equivalent "0.1.4+build.2" "0.1.4+build.1"
assert_equivalent "0.1.4-rc.1+build.2" "0.1.4-rc.1+build.1"

assert_invalid "0.1"
assert_invalid "01.1.0"
assert_invalid "0.1.4-rc.01"
assert_invalid "0.1.4-"
assert_invalid "0.1.4+"
assert_invalid "0.1.4-rc..1"
assert_invalid "0.1.4-rc."
assert_invalid "0.1.4+build."
assert_invalid "0.1.4+build..1"

echo "Semantic version comparison tests passed"
