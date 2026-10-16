#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-report.sh"

fail() {
  echo "TauGrid Flex nightly report test failed: $*" >&2
  exit 1
}

[ -x "$REPORTER" ] || fail "report generator must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/contract" "${fixture}/matrix/diagnostics"

cat >"${fixture}/contract/hardware-matrix.json" <<'EOF'
{
  "targets": [
    {
      "name": "target-a",
      "status": "available",
      "ready_nodes": 2,
      "gpu_available": 15,
      "gpu_requested": 1,
      "gpu_total": 16,
      "max_gpu_available_per_node": 8
    },
    {
      "name": "target-b",
      "status": "busy",
      "reason": "capacity is currently allocated",
      "ready_nodes": 2,
      "gpu_available": 0,
      "gpu_requested": 16,
      "gpu_total": 16,
      "max_gpu_available_per_node": 0
    }
  ]
}
EOF
cat >"${fixture}/matrix/matrix-results.jsonl" <<'EOF'
{"target":"target-a","workload":"serve","workers":1,"expected_hosts":1,"required_topology":"hostname","status":"passed"}
{"target":"target-a","workload":"train","workers":16,"expected_hosts":2,"required_topology":"site","status":"failed","reason":"go test exited non-zero"}
{"target":"target-b","workload":"serve","workers":8,"expected_hosts":1,"required_topology":"hostname","status":"skipped","reason":"target=busy; insufficient free GPUs for 1x8"}
{"workload":"cross-site","status":"skipped","reason":"fewer than two hardware sites are available"}
EOF
cat >"${fixture}/matrix/diagnostics/postflight-status.json" <<'EOF'
{
  "cleanup": {"status": "passed"},
  "capability": {"status": "failed", "reason": "capability differed from preflight"}
}
EOF
cat >"${fixture}/matrix/e2e-results.jsonl" <<'EOF'
{"status":"pass"}
{"status":"fail"}
{"status":"skip"}
EOF

output="${fixture}/nightly-report.md"
BUILD_BUILDNUMBER=20261007.1 \
BUILD_BUILDID=42 \
BUILD_SOURCEBRANCH=refs/heads/main \
BUILD_SOURCEVERSION=0123456789abcdef \
FLEX_NIGHTLY_PROFILE=full \
FLEX_NIGHTLY_RDMA_ENABLED=true \
PREFLIGHT_JOB_RESULT=Succeeded \
MATRIX_JOB_RESULT=Failed \
RDMA_JOB_RESULT=Skipped \
"$REPORTER" "$fixture" "$output"

grep -Fq '| target-a | **available** | 2 | 15 free / 1 requested / 16 total | 8 |' "$output" ||
  fail "report must render preflight capacity"
grep -Fq '| target-a | train | 16 | 2 | site | **failed** | go test exited non-zero |' "$output" ||
  fail "report must render failed matrix cases and reasons"
grep -Fq '| target-b | serve | 8 | 1 | hostname | **skipped** | target=busy; insufficient free GPUs for 1x8 |' "$output" ||
  fail "report must render skipped matrix cases and reasons"
grep -Fq 'Outcome: **skipped** - fewer than two hardware sites are available' "$output" ||
  fail "report must render cross-site skips"
grep -Fq 'Capability restoration: **failed** - capability differed from preflight' "$output" ||
  fail "report must render postflight capability failures"
grep -Fq 'Job outcome: **SKIP**' "$output" ||
  fail "report must render optional RDMA status"
grep -Fq '| fail | 1 |' "$output" ||
  fail "report must summarize structured E2E outcomes"

empty_fixture="${fixture}/empty"
mkdir -p "$empty_fixture"
empty_output="${fixture}/empty-report.md"
PREFLIGHT_JOB_RESULT=Failed \
MATRIX_JOB_RESULT=Skipped \
FLEX_NIGHTLY_RDMA_ENABLED=false \
"$REPORTER" "$empty_fixture" "$empty_output"
grep -Fq 'No hardware contract was produced. Preflight job outcome: **FAIL**.' "$empty_output" ||
  fail "report must explain missing preflight output after failure"
grep -Fq 'No matrix cases were recorded. Hardware matrix job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain missing matrix output after skip"

echo "TauGrid Flex nightly report tests passed"
