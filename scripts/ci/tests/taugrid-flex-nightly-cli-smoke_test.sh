#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-cli-smoke.sh"

fail() {
  echo "TauGrid Flex CLI smoke test failed: $*" >&2
  exit 1
}

[ -x "$SMOKE" ] || fail "CLI smoke helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/artifacts"

cat >"${fixture}/bin/tau" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_TAU_LOG}"
case "$*" in
  *"run logs tau-flex-job-"*) echo "tau flex job smoke complete" ;;
  *"run logs tau-flex-ray-"*) echo "tau flex ray smoke complete" ;;
  *"run list "*) printf '{"runs":[{"name":"tau-flex-job-42-1"},{"name":"tau-flex-ray-42-1"}]}\n' ;;
  *"run schema "*) printf '{"type":"object"}\n' ;;
  *) echo "ok" ;;
esac
EOF

cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_KUBECTL_LOG}"
case "$*" in
  *"get cluster.tau.azure.com cluster -o json"*)
    cat <<'JSON'
{"status":{"workloadProfiles":{"profiles":[{"name":"cpu","gpusPerWorker":0,"workerCount":1,"applicability":{"lanes":["training"],"namespaces":["ray"]},"localQueues":[{"namespace":"ray","name":"backfill"}],"conditions":[{"type":"Ready","status":"True"}]}]}}}
JSON
    ;;
  *"get rayjob.ray.io tau-flex-ray-42-1 -o jsonpath="*) printf 'SUCCEEDED' ;;
  *"get job tau-flex-job-42-1"* | *"get rayjob.ray.io tau-flex-ray-42-1"*) exit 1 ;;
  *) echo "ok" ;;
esac
EOF
chmod +x "${fixture}/bin/tau" "${fixture}/bin/kubectl"

export FAKE_TAU_LOG="${fixture}/tau.log"
export FAKE_KUBECTL_LOG="${fixture}/kubectl.log"
export TAU_BIN="${fixture}/bin/tau"
export KUBECTL_BIN="${fixture}/bin/kubectl"
export BUILD_BUILDID=42
export SYSTEM_JOBATTEMPT=1
export RAY_E2E_IMAGE=example.test/ray:latest
export FLEX_NIGHTLY_TAU_ARTIFACT_DIR="${fixture}/artifacts"

"$SMOKE"

jq -e '
  select(.command == "cluster-validate" and .status == "passed")
' "${fixture}/artifacts/cli-smoke-results.jsonl" >/dev/null ||
  fail "structured results must include installation validation"
jq -e '
  select(.command == "run-list" and .status == "passed")
' "${fixture}/artifacts/cli-smoke-results.jsonl" >/dev/null ||
  fail "structured results must include run listing"
jq -e '.status == "passed"' "${fixture}/artifacts/cleanup-result.json" >/dev/null ||
  fail "cleanup must pass when both workloads are absent"
grep -Fq "run --config ${fixture}/artifacts/configs/tau-job.yaml --context flex-nightly --dry-run=server" \
  "${fixture}/tau.log" ||
  fail "Job server dry-run must be exercised"
grep -Fq "run cancel tau-flex-ray-42-1" "${fixture}/tau.log" ||
  fail "Ray cleanup must use tau run cancel"

echo "TauGrid Flex CLI smoke tests passed"
