#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-gpu-profile-smoke.sh"

fail() {
  echo "TauGrid Flex GPU profile smoke test failed: $*" >&2
  exit 1
}

[ -x "$SMOKE" ] || fail "GPU profile smoke helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/artifacts"

cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"get cluster.tau.azure.com cluster -o json"* ]]; then
  cat <<'JSON'
{"status":{"workloadProfiles":{"profiles":[
  {"name":"research-one","gpusPerWorker":1,"workerCount":1,"mode":"fixed","placement":"unconstrained","executionTarget":"singleCluster","applicability":{"teams":["research"],"lanes":["training"]},"localQueues":[{"namespace":"ray","name":"backfill","clusterQueue":"gpu"}],"conditions":[{"type":"Ready","status":"True","message":"ready"}]},
  {"name":"experimental-one","gpusPerWorker":1,"workerCount":1,"mode":"fixed","placement":"unconstrained","executionTarget":"singleCluster","applicability":{"teams":["experimental"],"lanes":["training"]},"localQueues":[{"namespace":"ray","name":"backfill","clusterQueue":"gpu"}],"conditions":[{"type":"Ready","status":"True","message":"ready"}]},
  {"name":"research-two-same-host","gpusPerWorker":2,"workerCount":1,"mode":"fixed","placement":"same-host","executionTarget":"singleCluster","applicability":{"teams":["research"],"lanes":["large-memory"]},"localQueues":[{"namespace":"ray","name":"backfill","clusterQueue":"gpu"}],"conditions":[{"type":"Ready","status":"True","message":"ready"}]}
]}}}
JSON
elif [[ "$*" == *"get rayjob.ray.io"* ]]; then
  printf 'SUCCEEDED'
else
  echo "unexpected kubectl arguments: $*" >&2
  exit 1
fi
EOF

cat >"${fixture}/bin/tau" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_TAU_LOG}"
if [[ "$*" == *" run logs "* || "$*" == "run logs "* ]]; then
  printf 'tau gpu profile smoke complete\n'
fi
EOF
chmod +x "${fixture}/bin/kubectl" "${fixture}/bin/tau"

export KUBECTL_BIN="${fixture}/bin/kubectl"
export TAU_BIN="${fixture}/bin/tau"
export FAKE_TAU_LOG="${fixture}/tau.log"
export FLEX_NIGHTLY_GPU_PROFILE_ARTIFACT_DIR="${fixture}/artifacts"
export RAY_E2E_IMAGE=example.test/ray:latest
export BUILD_BUILDID=42
export SYSTEM_JOBATTEMPT=1

"$SMOKE"

[[ "$(jq -s '[.[] | select(.phase == "dry-run" and .status == "passed")] | length' \
  "${fixture}/artifacts/gpu-profile-results.jsonl")" -eq 3 ]] ||
  fail "every ready GPU profile must pass dry-run validation"
[[ "$(jq 'length' "${fixture}/artifacts/gpu-profile-representatives.json")" -eq 2 ]] ||
  fail "duplicate team variants must collapse to one live shape representative"
[[ "$(jq -s '[.[] | select(.phase == "live" and .status == "passed")] | length' \
  "${fixture}/artifacts/gpu-profile-results.jsonl")" -eq 2 ]] ||
  fail "one representative per unique shape must pass live validation"
jq -e '.status == "passed"' \
  "${fixture}/artifacts/gpu-profile-cleanup-result.json" >/dev/null ||
  fail "GPU profile workload cleanup must pass"

echo "TauGrid Flex GPU profile smoke tests passed"
