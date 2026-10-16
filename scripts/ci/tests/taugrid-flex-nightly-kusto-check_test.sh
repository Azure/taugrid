#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly CHECK="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-kusto-check.sh"

fail() {
  echo "TauGrid Flex Kusto check test failed: $*" >&2
  exit 1
}

[ -x "$CHECK" ] || fail "Kusto check helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/artifacts"

cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"port-forward"* ]]; then
  sleep 60
fi
EOF
cat >"${fixture}/bin/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'fake-token\n'
EOF
cat >"${fixture}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
if [[ "$args" == *"/healthz"* ]]; then
  printf '{"status":"ok"}\n'
elif [[ "$args" == *"/capabilities"* ]]; then
  printf '{"sources":["kusto"]}\n'
elif [[ "$args" == *"/experiments"* ]]; then
  printf '{"items":[]}\n'
else
  config="$(cat)"
  [[ "$config" == 'header = "Authorization: Bearer '"fake-token"'"' ]] ||
    exit 1
  printf '{"Tables":[{"TableKind":"PrimaryResult","Rows":[[7,1]]}]}\n'
fi
EOF
chmod +x "${fixture}/bin/kubectl" "${fixture}/bin/az" "${fixture}/bin/curl"

export PATH="${fixture}/bin:${PATH}"
export KUBECTL_BIN="${fixture}/bin/kubectl"
export FLEX_NIGHTLY_KUSTO_ENDPOINT=https://example.kusto.windows.net
export FLEX_NIGHTLY_KUSTO_QUERY_DATABASE=Metrics
export FLEX_NIGHTLY_KUSTO_RESULTS_DATABASE=CITests
export FLEX_NIGHTLY_PORTAL_NAMESPACE=tau
export FLEX_NIGHTLY_PORTAL_SERVICE=taugrid-portal
export FLEX_NIGHTLY_PORTAL_WORKSPACE=default
export FLEX_NIGHTLY_KUSTO_ARTIFACT_DIR="${fixture}/artifacts"
export BUILD_BUILDID=42

"$CHECK"

jq -e '
  .status == "passed"
  and .test_outcomes.rows == 7
  and .test_outcomes.failures == 1
' "${fixture}/artifacts/kusto-check-result.json" >/dev/null ||
  fail "Kusto check must record Portal and TestOutcomes success"

echo "TauGrid Flex Kusto check tests passed"
