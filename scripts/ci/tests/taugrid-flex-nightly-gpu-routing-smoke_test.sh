#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-gpu-routing-smoke.sh"

fail() {
  echo "TauGrid Flex GPU routing test failed: $*" >&2
  exit 1
}

[ -x "$SMOKE" ] || fail "GPU routing helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/artifacts"

cat >"${fixture}/contract.json" <<'JSON'
{
  "targets": [
    {"name":"a100","status":"available","gpu_class":"a100-80gb","selector":"kueue.azure.com/gpu-series=ndm-a100-v4","flavor":"tau-gpu-a100-80gb-v2","cluster_queue":"tau-gpu-cq"},
    {"name":"h100","status":"available","gpu_class":"h100-95gb","selector":"kueue.azure.com/gpu-series=nc-h100-v5","flavor":"tau-gpu-h100-95gb-v2","cluster_queue":"tau-gpu-cq"},
    {"name":"h200","status":"available","gpu_class":"h200-141gb","selector":"kueue.azure.com/gpu-series=nd-h200-v5","flavor":"tau-gpu-h200-141gb-v2","cluster_queue":"tau-gpu-cq"}
  ]
}
JSON

cat >"${fixture}/bin/tau" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_TAU_LOG}"
case "$*" in
  *"run logs tau-route-"*) echo "tau gpu routing complete" ;;
  *) echo "ok" ;;
esac
EOF

cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_KUBECTL_LOG}"
args="$*"
case "$args" in
  *"apply -f -") cat >/dev/null; echo "localqueue.kueue.x-k8s.io/routing created" ;;
  *"get job tau-route-"*"-o jsonpath="*)
    sed -E 's/.*get job tau-route-([^-]+)-.*/\1/' <<<"$args" >"${FAKE_TARGET_FILE}"
    printf 'job-uid'
    ;;
  *"get workloads.kueue.x-k8s.io -o json")
    target="$(cat "${FAKE_TARGET_FILE}")"
    case "$target" in
      a100) flavor=tau-gpu-a100-80gb-v2 ;;
      h100) flavor=tau-gpu-h100-95gb-v2 ;;
      h200) flavor=tau-gpu-h200-141gb-v2 ;;
    esac
    printf '{"items":[{"metadata":{"ownerReferences":[{"uid":"job-uid"}]},"status":{"admission":{"podSetAssignments":[{"flavors":{"nvidia.com/gpu":"%s"}}]}}}]}\n' "$flavor"
    ;;
  *"get pods -l job-name=tau-route-"*"-o json")
    target="$(sed -E 's/.*job-name=tau-route-([^-]+)-.*/\1/' <<<"$args")"
    printf '{"items":[{"spec":{"nodeName":"%s-node"}}]}\n' "$target"
    ;;
  *"get node "*" -o json")
    node="$(sed -E 's/.*get node ([^ ]+) -o json.*/\1/' <<<"$args")"
    target="${node%-node}"
    case "$target" in
      a100) class=a100-80gb; series=ndm-a100-v4 ;;
      h100) class=h100-95gb; series=nc-h100-v5 ;;
      h200) class=h200-141gb; series=nd-h200-v5 ;;
    esac
    printf '{"metadata":{"labels":{"tau.azure.com/gpu-class":"%s","kueue.azure.com/gpu-series":"%s"}}}\n' "$class" "$series"
    ;;
  *"get job tau-route-"*) exit 1 ;;
  *"get namespace taugrid-nightly-routing-"*) exit 1 ;;
  *) echo "ok" ;;
esac
EOF
chmod +x "${fixture}/bin/tau" "${fixture}/bin/kubectl"

export TAU_BIN="${fixture}/bin/tau"
export KUBECTL_BIN="${fixture}/bin/kubectl"
export FAKE_TAU_LOG="${fixture}/tau.log"
export FAKE_KUBECTL_LOG="${fixture}/kubectl.log"
export FAKE_TARGET_FILE="${fixture}/target"
export FLEX_NIGHTLY_HARDWARE_CONTRACT="${fixture}/contract.json"
export FLEX_NIGHTLY_GPU_ROUTING_ARTIFACT_DIR="${fixture}/artifacts"
export RAY_E2E_IMAGE=example.test/ray:latest
export BUILD_BUILDID=42
export SYSTEM_JOBATTEMPT=1

"$SMOKE"

[ "$(jq -s 'map(select(.status == "passed")) | length' "${fixture}/artifacts/gpu-routing-results.jsonl")" -eq 3 ] ||
  fail "A100, H100, and H200 routing results must pass"
grep -Fq "gpu_class: h100-95gb" "${fixture}/artifacts/configs/h100.yaml" ||
  fail "H100 config must request the exact researcher GPU class"
grep -Fq "tau-gpu-h100-95gb-v2" "${fixture}/artifacts/h100-workload.json" ||
  fail "H100 admission must use the expected ResourceFlavor"
jq -e '.status == "passed"' "${fixture}/artifacts/gpu-routing-cleanup-result.json" >/dev/null ||
  fail "GPU routing cleanup must pass"

echo "TauGrid Flex GPU routing tests passed"
