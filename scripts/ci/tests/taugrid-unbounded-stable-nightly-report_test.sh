#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-nightly-report.sh"

fail() {
  echo "TauGrid unbounded-stable nightly report test failed: $*" >&2
  exit 1
}

[ -x "$REPORTER" ] || fail "report generator must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/images" "${fixture}/deployment/tau-native-smoke"

cat >"${fixture}/images/images.json" <<'EOF'
{
  "registry": "example.azurecr.io",
  "repository_prefix": "nightly/taugrid",
  "tag": "nightly-0123456789ab-42",
  "source_version": "0123456789abcdef"
}
EOF
cat >"${fixture}/deployment/tau-native-smoke/tau-native-smoke-result.json" <<'EOF'
{
  "status": "passed",
  "reason": "",
  "run_name": "taugrid-nightly-42-1",
  "workspace": "taugrid-default",
  "profile": "unbounded.cpu.topology-smoke",
  "lifecycle_state": "succeeded",
  "config_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "topology": {
    "level": "kubernetes.io/hostname",
    "flavor": "taugrid-default-cpu",
    "cluster_queue": "jobqueue",
    "workload": "rayjob-taugrid-nightly-42-1-abcd",
    "pod_set": "taugrid-nightly-42-1-w",
    "assigned_workers": 2,
    "domains": [
      {
        "values": ["node-1"],
        "count": 2
      }
    ],
    "pod_nodes": ["node-1", "node-1"]
  }
}
EOF
cat >"${fixture}/deployment/deployment-result.json" <<'EOF'
{
  "environment": "unbounded-stable",
  "status": "passed",
  "reason": "",
  "source_version": "0123456789abcdef",
  "image_tag": "nightly-0123456789ab-42",
  "previous_revision": "7",
  "current_revision": "8"
}
EOF

output="${fixture}/nightly-report.md"
BUILD_BUILDNUMBER=20261008.1 \
BUILD_BUILDID=42 \
BUILD_SOURCEBRANCH=refs/heads/main \
BUILD_SOURCEVERSION=0123456789abcdef \
PREFLIGHT_JOB_RESULT=Succeeded \
BUILD_IMAGES_JOB_RESULT=Succeeded \
DEPLOY_JOB_RESULT=Succeeded \
"$REPORTER" "$fixture" "$output"

grep -Fq '| Environment | **unbounded-stable** |' "$output" ||
  fail "report must name the deployment environment"
grep -Fq '| Deployment job | **PASS** |' "$output" ||
  fail "report must render the deployment job result"
grep -Fq -- '- Outcome: **passed**' "$output" ||
  fail "report must render the deployment result"
grep -Fq -- '- Helm revision: `7` -> `8`' "$output" ||
  fail "report must render the Helm revision change"
grep -Fq -- '- Run: `taugrid-nightly-42-1`' "$output" ||
  fail "report must render the Tau-native smoke run"
grep -Fq -- '- Workspace/profile: `taugrid-default` / `unbounded.cpu.topology-smoke`' "$output" ||
  fail "report must render the Tau-native workspace and profile"
grep -Fq -- '- Tau lifecycle state: `succeeded`' "$output" ||
  fail "report must render the Tau lifecycle state"
grep -Fq -- '- Topology admission: workload `rayjob-taugrid-nightly-42-1-abcd` PodSet `taugrid-nightly-42-1-w` admitted by `jobqueue`; `kubernetes.io/hostname` using `taugrid-default-cpu` for 2 workers' "$output" ||
  fail "report must render the topology admission contract"
grep -Fq -- '- Topology domains: `node-1=2`' "$output" ||
  fail "report must render the assigned topology domains"
grep -Fq -- '- Ray worker nodes: `node-1, node-1`' "$output" ||
  fail "report must render the actual Ray worker nodes"
grep -Fq -- '- Immutable tag: `nightly-0123456789ab-42`' "$output" ||
  fail "report must render the immutable image tag"
if grep -Fq 'example.azurecr.io' "$output"; then
  fail "report must not expose the environment-specific registry"
fi

empty_fixture="${fixture}/empty"
mkdir -p "$empty_fixture"
empty_output="${fixture}/empty-report.md"
PREFLIGHT_JOB_RESULT=Succeeded \
BUILD_IMAGES_JOB_RESULT=Failed \
DEPLOY_JOB_RESULT=Skipped \
"$REPORTER" "$empty_fixture" "$empty_output"
grep -Fq 'No deployment result was produced. Deployment job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain missing deployment output"
grep -Fq 'No image contract was produced. Image publication job outcome: **FAIL**.' "$empty_output" ||
  fail "report must explain missing image output"
grep -Fq 'No Tau-native smoke result was produced. Deployment job outcome: **SKIP**.' "$empty_output" ||
  fail "report must explain missing Tau-native smoke output"

echo "TauGrid unbounded-stable nightly report tests passed"
