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
mkdir -p \
  "${fixture}/contract" \
  "${fixture}/images" \
  "${fixture}/deployment" \
  "${fixture}/taucluster" \
  "${fixture}/cli" \
  "${fixture}/gpu-routing" \
  "${fixture}/gpu-profiles" \
  "${fixture}/storage" \
  "${fixture}/kusto" \
  "${fixture}/matrix/diagnostics" \
  "${fixture}/rdma/diagnostics"

cat >"${fixture}/images/images.json" <<'EOF'
{
  "registry": "example.azurecr.io",
  "repository_prefix": "nightly",
  "tag": "nightly-0123456789ab-42",
  "source_version": "0123456789abcdef"
}
EOF
cat >"${fixture}/deployment/deployment-result.json" <<'EOF'
{
  "environment": "flex",
  "status": "passed",
  "source_version": "0123456789abcdef",
  "image_tag": "nightly-0123456789ab-42",
  "previous_revision": "7",
  "current_revision": "8"
}
EOF
cat >"${fixture}/taucluster/gpu-class-inventory.json" <<'EOF'
{
  "required_gpu_classes": ["a100-80gb", "h100-95gb", "h200-141gb"],
  "nodes": [
    {"name":"a100-a","gpu_class":"a100-80gb","gpu_series":"ndm-a100-v4","gpu_allocatable":"8"},
    {"name":"h100-a","gpu_class":"h100-95gb","gpu_series":"nc-h100-v5","gpu_allocatable":"2"},
    {"name":"h200-a","gpu_class":"h200-141gb","gpu_series":"nd-h200-v5","gpu_allocatable":"8"}
  ]
}
EOF
cat >"${fixture}/cli/cli-smoke-results.jsonl" <<'EOF'
{"command":"cluster-validate","status":"passed","duration_seconds":4}
{"command":"job-submit","status":"passed","duration_seconds":2}
{"command":"ray-logs","status":"failed","duration_seconds":3,"reason":"marker missing"}
EOF
cat >"${fixture}/cli/cleanup-result.json" <<'EOF'
{"status":"passed","reason":"","workloads":["tau-flex-job-42-1","tau-flex-ray-42-1"]}
EOF
cat >"${fixture}/gpu-routing/gpu-routing-results.jsonl" <<'EOF'
{"target":"a100","status":"passed","reason":"","admitted_flavor":"tau-gpu-a100-80gb-v2","node":"a100-a"}
{"target":"h100","status":"passed","reason":"","admitted_flavor":"tau-gpu-h100-95gb-v2","node":"h100-a"}
{"target":"h200","status":"passed","reason":"","admitted_flavor":"tau-gpu-h200-141gb-v2","node":"h200-a"}
EOF
cat >"${fixture}/gpu-routing/gpu-routing-cleanup-result.json" <<'EOF'
{"status":"passed","reason":"","namespace":"taugrid-nightly-routing-42-1"}
EOF
cat >"${fixture}/gpu-profiles/gpu-profile-results.jsonl" <<'EOF'
{"profile":"azure.research.training.l","phase":"readiness","shape":"1/1/fixed/unconstrained/singleCluster","status":"passed","reason":""}
{"profile":"azure.research.training.l","phase":"dry-run","shape":"1/1/fixed/unconstrained/singleCluster","status":"passed","reason":""}
{"profile":"azure.research.training.l","phase":"live","shape":"1/1/fixed/unconstrained/singleCluster","status":"passed","reason":""}
{"profile":"azure.experimental.training.l","phase":"dry-run","shape":"1/1/fixed/unconstrained/singleCluster","status":"passed","reason":""}
EOF
cat >"${fixture}/gpu-profiles/gpu-profile-representatives.json" <<'EOF'
[{"name":"azure.research.training.l","shape":"1/1/fixed/unconstrained/singleCluster"}]
EOF
cat >"${fixture}/gpu-profiles/gpu-profile-cleanup-result.json" <<'EOF'
{"status":"passed","reason":""}
EOF
cat >"${fixture}/storage/storage-results.jsonl" <<'EOF'
{"target":"a100-write","status":"passed","reason":""}
{"target":"h200-read-write","status":"passed","reason":""}
{"target":"a100-readback","status":"passed","reason":""}
EOF
cat >"${fixture}/storage/storage-cleanup-result.json" <<'EOF'
{"status":"passed","reason":""}
EOF
cat >"${fixture}/kusto/kusto-check-result.json" <<'EOF'
{
  "status": "passed",
  "query_database": "Metrics",
  "results_database": "CITests",
  "portal_workspace": "default",
  "test_outcomes": {"rows": 12, "failures": 1}
}
EOF

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
cat >"${fixture}/rdma/diagnostics/rdma-postflight-status.json" <<'EOF'
{
  "cleanup": {"status": "failed", "reason": "RDMA namespace still exists after cleanup"},
  "capability": {"status": "failed", "reason": "capability differed from the pre-RDMA contract"}
}
EOF
cat >"${fixture}/rdma/fineweb-metrics.json" <<'EOF'
{
  "performance": {
    "duration_seconds": 12.5,
    "global_tokens": 32768,
    "tokens_per_second": 2621.44,
    "steps": 2,
    "world_size": 16,
    "batch_size_per_rank": 1,
    "block_size": 1024
  },
  "infiniband": {
    "hosts": 2,
    "transmit_bytes": 1000000,
    "receive_bytes": 900000,
    "transmit_packets": 10000,
    "receive_packets": 9000,
    "error_events": 0
  },
  "ranks": []
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
TAUCLUSTER_JOB_RESULT=Succeeded \
PREFLIGHT_JOB_RESULT=Succeeded \
BUILD_IMAGES_JOB_RESULT=Succeeded \
DEPLOY_JOB_RESULT=Succeeded \
CLI_SMOKE_JOB_RESULT=Failed \
GPU_ROUTING_JOB_RESULT=Succeeded \
GPU_PROFILE_JOB_RESULT=Succeeded \
STORAGE_JOB_RESULT=Succeeded \
MATRIX_JOB_RESULT=Failed \
RDMA_JOB_RESULT=Skipped \
KUSTO_JOB_RESULT=Succeeded \
"$REPORTER" "$fixture" "$output"

grep -Fq '| target-a | **available** | 2 | 15 free / 1 requested / 16 total | 8 |' "$output" ||
  fail "report must render preflight capacity"
grep -Fq '| `h100-95gb` | `nc-h100-v5` | `h100-a` | 2 |' "$output" ||
  fail "report must render H100 discovery"
grep -Fq '| h100 | **passed** | `tau-gpu-h100-95gb-v2` | `h100-a` |' "$output" ||
  fail "report must render exact H100 queue routing"
grep -Fq 'Helm revision: `7` -> `8`' "$output" ||
  fail "report must render the deployed Helm revision"
grep -Fq '| ray-logs | **failed** | 3s | marker missing |' "$output" ||
  fail "report must render failed Tau CLI commands"
grep -Fq 'Workload cleanup: **passed**' "$output" ||
  fail "report must render Tau CLI cleanup"
grep -Fq '| azure.research.training.l | live | 1/1/fixed/unconstrained/singleCluster | **passed** |' "$output" ||
  fail "report must render live GPU profile results"
grep -Fq 'Unique live-tested shapes: **1**' "$output" ||
  fail "report must render the representative GPU shape count"
grep -Fq 'GPU profile workload cleanup: **passed**' "$output" ||
  fail "report must render GPU profile cleanup"
grep -Fq '| h200-read-write | **passed** |' "$output" ||
  fail "report must render cross-region Blob PVC checks"
grep -Fq 'Blob test data and Job cleanup: **passed**' "$output" ||
  fail "report must render storage cleanup"
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
grep -Fq 'Namespace cleanup: **failed** - RDMA namespace still exists after cleanup' "$output" ||
  fail "report must render RDMA cleanup failures"
grep -Fq 'Capability restoration: **failed** - capability differed from the pre-RDMA contract' "$output" ||
  fail "report must render RDMA capability restoration failures"
grep -Fq '| Training throughput | 2621 tokens/s |' "$output" ||
  fail "report must render FineWeb training throughput"
grep -Fq '| InfiniBand transmit | 1000000 bytes / 10000 packets |' "$output" ||
  fail "report must render InfiniBand traffic counters"
grep -Fq '| InfiniBand error events | 0 |' "$output" ||
  fail "report must render InfiniBand error counters"
grep -Fq 'Ingested TestOutcomes rows: **12** (1 recorded test failures)' "$output" ||
  fail "report must render Kusto ingestion counts"
grep -Fq '| fail | 1 |' "$output" ||
  fail "report must summarize structured E2E outcomes"

empty_fixture="${fixture}/empty"
mkdir -p "$empty_fixture"
empty_output="${fixture}/empty-report.md"
PREFLIGHT_JOB_RESULT=Failed \
BUILD_IMAGES_JOB_RESULT=Skipped \
DEPLOY_JOB_RESULT=Skipped \
CLI_SMOKE_JOB_RESULT=Skipped \
GPU_PROFILE_JOB_RESULT=Skipped \
STORAGE_JOB_RESULT=Skipped \
MATRIX_JOB_RESULT=Skipped \
KUSTO_JOB_RESULT=Skipped \
FLEX_NIGHTLY_RDMA_ENABLED=false \
"$REPORTER" "$empty_fixture" "$empty_output"
grep -Fq 'No hardware contract was produced. Preflight job outcome: **FAIL**.' "$empty_output" ||
  fail "report must explain missing preflight output after failure"
grep -Fq 'No matrix cases were recorded. Hardware matrix job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain missing matrix output after skip"
grep -Fq 'No deployment result was produced. Deployment job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain a skipped deployment"
grep -Fq 'No Tau CLI command results were recorded. CLI smoke job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain skipped Tau CLI smoke"
grep -Fq 'No GPU profile results were recorded. GPU profile job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain skipped GPU profile checks"
grep -Fq 'No storage cases were recorded. Storage job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain skipped storage smoke"
grep -Fq 'No Portal/Kusto result was produced. Kusto job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain skipped Kusto checks"

echo "TauGrid Flex nightly report tests passed"
