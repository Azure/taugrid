#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-storage-smoke.sh"

fail() {
  echo "TauGrid Flex storage smoke test failed: $*" >&2
  exit 1
}

[ -x "$SMOKE" ] || fail "storage smoke helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/artifacts"

cat >"${fixture}/bin/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == *"storage account show"* ]] && printf 'westeurope\n'
EOF
cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_KUBECTL_LOG}"
if [[ "$*" == *"get pvc blob-training"* ]]; then
  printf 'Bound'
elif [[ "$*" == *"get jobs -l"* ]]; then
  exit 0
elif [[ "$*" == *"logs job/"* ]]; then
  echo "marker"
else
  cat >/dev/null || true
  echo "ok"
fi
EOF
chmod +x "${fixture}/bin/az" "${fixture}/bin/kubectl"

export PATH="${fixture}/bin:${PATH}"
export KUBECTL_BIN="${fixture}/bin/kubectl"
export FAKE_KUBECTL_LOG="${fixture}/kubectl.log"
export BUILD_BUILDID=42
export SYSTEM_JOBATTEMPT=1
export FLEX_NIGHTLY_STORAGE_NAMESPACE=tau-default
export FLEX_NIGHTLY_STORAGE_PVC=blob-training
export FLEX_NIGHTLY_STORAGE_ACCOUNT=storage
export FLEX_NIGHTLY_STORAGE_ACCOUNT_REGION=westeurope
export FLEX_NIGHTLY_A100_SITE=westeurope
export FLEX_NIGHTLY_H200_SITE=eastus2euap
export FLEX_NIGHTLY_A100_REGION=westeurope
export FLEX_NIGHTLY_H200_REGION=eastus2euap
export RAY_E2E_IMAGE=example.test/ray:latest
export FLEX_NIGHTLY_STORAGE_ARTIFACT_DIR="${fixture}/artifacts"

"$SMOKE"

[ "$(wc -l <"${fixture}/artifacts/storage-results.jsonl" | tr -d ' ')" -eq 3 ] ||
  fail "storage smoke must record all three cross-region cases"
jq -e '.status == "passed"' "${fixture}/artifacts/storage-cleanup-result.json" >/dev/null ||
  fail "storage cleanup must pass"
grep -Fq "kueue.azure.com/gpu-series: ndm-a100-v4" \
  "${fixture}/artifacts/diagnostics/tau-storage-a100-42-1.yaml" ||
  fail "storage smoke must target A100 nodes"
grep -Fq "kueue.azure.com/gpu-series: nd-h200-v5" \
  "${fixture}/artifacts/diagnostics/tau-storage-h200-42-1.yaml" ||
  fail "storage smoke must target H200 nodes"
grep -Fq "tau.azure.com/site: eastus2euap" \
  "${fixture}/artifacts/diagnostics/tau-storage-h200-42-1.yaml" ||
  fail "storage smoke must target the eastus2euap site"
grep -Fq "topology.kubernetes.io/region: eastus2euap" \
  "${fixture}/artifacts/diagnostics/tau-storage-h200-42-1.yaml" ||
  fail "storage smoke must target the eastus2euap Azure region"

echo "TauGrid Flex storage smoke tests passed"
