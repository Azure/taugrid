#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly PIPELINE="${REPO_ROOT}/.pipelines/taugrid-flex-nightly.yml"
readonly PREFLIGHT="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-preflight.sh"
readonly QUEUE_OVERLAY="${REPO_ROOT}/cluster-overlays/queues/shared-gpu-queue.yaml"

fail() {
  echo "TauGrid Flex nightly contract test failed: $*" >&2
  exit 1
}

[ -f "$PIPELINE" ] || fail "pipeline is missing"
[ ! -e "${REPO_ROOT}/.github/workflows/taugrid-flex-nightly.yml" ] ||
  fail "nightly execution must not retain a GitHub Actions workflow"
[ -x "$PREFLIGHT" ] || fail "preflight must be executable"
[ -f "$QUEUE_OVERLAY" ] || fail "shared GPU queue overlay is missing"

grep -Fq 'readonly A100_SITE="${FLEX_NIGHTLY_A100_SITE:-}"' "$PREFLIGHT" ||
  fail "A100 site must be supplied through ADO variables"
grep -Fq 'readonly H200_SITE="${FLEX_NIGHTLY_H200_SITE:-}"' "$PREFLIGHT" ||
  fail "H200 site must be supplied through ADO variables"
grep -Fq 'readonly DGX_SITE="${FLEX_NIGHTLY_DGX_SITE:-}"' "$PREFLIGHT" ||
  fail "DGX site must be supplied through ADO variables"
if grep -Fq "tau.azure.com/node-pool:" "$QUEUE_OVERLAY"; then
  fail "environment-specific DGX selector must not be published in the shared queue overlay"
fi

grep -Fq 'cron: "15 8 * * *"' "$PIPELINE" ||
  fail "pipeline must run nightly"
grep -Fq "name: 1es-aks-ai-runtime-ado-eastus2" "$PIPELINE" ||
  fail "pipeline must run on the approved 1ES AKS AI Runtime pool"
grep -Fq "azureSubscription: \$(azureServiceConnection)" "$PIPELINE" ||
  fail "pipeline must use the approved Azure DevOps service connection"
grep -Fq "value: shared-cluster-controllers" "$PIPELINE" ||
  fail "pipeline must explicitly use the safe shared-controller mode"
grep -Fq "taugrid-flex-nightly-preflight.sh cluster" "$PIPELINE" ||
  fail "pipeline must run cluster preflight before workloads"
grep -Fq "compare-capability" "$PIPELINE" ||
  fail "pipeline must compare Flex node capability after cleanup"
grep -Fq "capability_violations" "$PREFLIGHT" ||
  fail "preflight must expose Flex node capability violations"
grep -Fq "lease_age_seconds" "$PREFLIGHT" ||
  fail "preflight must validate Flex node lease freshness"
grep -Fq "value: taugrid-gpu-topology" "$PIPELINE" ||
  fail "pipeline must require the authoritative TauGrid GPU topology"
grep -Fq "name: rdmaConformance" "$PIPELINE" ||
  fail "extended H200 RDMA conformance must remain explicitly selectable"
grep -Fq "name: includeDGXSpark" "$PIPELINE" ||
  fail "DGX Spark execution must remain an explicit opt-in while the nodes are reserved"
grep -Fq "default: false" "$PIPELINE" ||
  fail "extended H200 RDMA conformance must be disabled by default"
grep -Fq "TestRayServeGPU" "$PIPELINE" ||
  fail "hardware matrix must run Ray Serve online inference"
grep -Fq "Ray Serve protobuf compatibility fix" "$PIPELINE" ||
  fail "preflight must reject Ray versions older than 2.56.1"
grep -Fq 'if [[ "${INCLUDE_DGX}" == "true"' "$PIPELINE" ||
  fail "arm64 image validation must be conditional on DGX Spark opt-in"
grep -Fq "grep -Fxq linux/arm64" "$PIPELINE" ||
  fail "DGX Spark opt-in must require an arm64 Ray image"
grep -Fq "grep -Fxq linux/amd64" "$PIPELINE" ||
  fail "preflight must require an amd64 Ray image for A100/H200"
grep -Fq "TestRayTrainGPU" "$PIPELINE" ||
  fail "hardware matrix must run distributed Ray Train"
grep -Fq "run_case \"\${target}\" 2 2 1 tau.azure.com/site" "$PIPELINE" ||
  fail "DGX Spark matrix must include two GPUs across two hosts"
grep -Fq "run_case \"\${target}\" 8 1 8 kubernetes.io/hostname" "$PIPELINE" ||
  fail "A100/H200 matrix must include eight GPUs on one host"
grep -Fq "run_case \"\${target}\" 16 2 8 tau.azure.com/site" "$PIPELINE" ||
  fail "A100/H200 matrix must include sixteen GPUs split across two hosts"
grep -Fq "MATRIX_SCHEDULER_RECOVERY_NODE" "$PIPELINE" ||
  fail "pipeline must retain the guarded H200 scheduler recovery"
grep -Fq "TestConcurrentGPUWorkloadsAcrossHardwareSites" \
  "${REPO_ROOT}/tests/e2e/multisite/multisite_test.go" ||
  fail "full profile must include concurrent hardware-site validation"
grep -Fq "TestFineWebRayTrain16xH200IB" "$PIPELINE" ||
  fail "optional H200 RDMA/NCCL conformance must remain available"
grep -Fq "export FINEWEB_TAS_MODE=site" "$PIPELINE" ||
  fail "H200 RDMA extension must use site-level topology for its two-node shape"
grep -Fq "trap cleanup EXIT" "$PIPELINE" ||
  fail "pipeline must retain fail-safe cleanup traps"
grep -Fq "condition: always()" "$PIPELINE" ||
  fail "pipeline must publish diagnostics after failures"
if grep -Eq 'helm (upgrade|install).*(kueue|kuberay|tau-core-controller)' "$PIPELINE"; then
  fail "pipeline must not install colliding cluster-scoped controllers"
fi
for flavor in tau-gpu-a100-80gb-v2 tau-gpu-h200-141gb-v2; do
  grep -Fq "name: ${flavor}" "$QUEUE_OVERLAY" ||
    fail "queue overlay must define ${flavor}"
done
[ "$(grep -Fc 'topologyName: taugrid-gpu-topology' "$QUEUE_OVERLAY")" -ge 3 ] ||
  fail "all shared GPU flavors must use taugrid-gpu-topology"
grep -Fq "tau.azure.com/gpu-queue: enabled" "$QUEUE_OVERLAY" ||
  fail "shared queue namespace selection must use the GPU queue opt-in label"

fake_bin="$(mktemp -d)"
trap 'rm -rf "$fake_bin"' EXIT
cat >"${fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  *"get nodes -l example.com/node-pool=spark -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"spark-a","labels":{"tau.azure.com/site":"site-dgx","tau.azure.com/network-domain":"spark-a","tau.azure.com/accelerator-domain":"spark-a"}},"spec":{},"status":{"nodeInfo":{"architecture":"arm64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"1"}}},{"metadata":{"name":"spark-b","labels":{"tau.azure.com/site":"site-dgx","tau.azure.com/network-domain":"spark-b","tau.azure.com/accelerator-domain":"spark-b"}},"spec":{},"status":{"nodeInfo":{"architecture":"arm64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"1"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=ndm-a100-v4 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"a100-a","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/network-domain":"a100-a","tau.azure.com/accelerator-domain":"a100-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}},{"metadata":{"name":"a100-b","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/network-domain":"a100-b","tau.azure.com/accelerator-domain":"a100-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=nd-h200-v5 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"h200-a","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/network-domain":"h200-a","tau.azure.com/accelerator-domain":"h200-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}},{"metadata":{"name":"h200-b","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/network-domain":"h200-b","tau.azure.com/accelerator-domain":"h200-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}}]}
JSON
    ;;
  *"-n kube-node-lease get leases.coordination.k8s.io -o json"*)
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [[ "${FAKE_STALE_LEASE:-0}" == "1" ]]; then
      now="2000-01-01T00:00:00Z"
    fi
    printf '{"items":[{"metadata":{"name":"spark-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"spark-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-b"},"spec":{"renewTime":"%s"}}]}\n' \
      "$now" "$now" "$now" "$now" "$now" "$now"
    ;;
  *"get pods -A -o json"*)
    printf '%s\n' '{"items":[{"spec":{"nodeName":"a100-a","containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},"status":{"phase":"Running"}}]}'
    ;;
  *"get deployment "*" -o json"*)
    printf '%s\n' '{"status":{"availableReplicas":1}}'
    ;;
  *"get topology taugrid-gpu-topology -o json"*)
    printf '%s\n' '{"spec":{"levels":[{"nodeLabel":"tau.azure.com/site"},{"nodeLabel":"tau.azure.com/network-domain"},{"nodeLabel":"tau.azure.com/accelerator-domain"},{"nodeLabel":"kubernetes.io/hostname"}]}}'
    ;;
  *"get resourceflavor tau-gpu-dgx-spark-v2 -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    ;;
  *"get resourceflavor tau-gpu-a100-80gb-v2 -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    ;;
  *"get resourceflavor tau-gpu-h200-141gb-v2 -o json"*)
    if [[ "${FAKE_STALE_H200:-0}" == "1" ]]; then
      printf '%s\n' '{"spec":{"topologyName":"default-node-topology"}}'
    else
      printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    fi
    ;;
  *"get clusterqueue tau-gpu-cq -o json"*)
    printf '%s\n' '{"spec":{"resourceGroups":[{"flavors":[{"name":"tau-gpu-dgx-spark-v2"},{"name":"tau-gpu-h200-141gb-v2"},{"name":"tau-gpu-a100-80gb-v2"}]}]},"status":{"conditions":[{"type":"Active","status":"True"}]}}'
    ;;
  *"get crd "*)
    printf '%s\n' '{}'
    ;;
  *)
    echo "unexpected kubectl arguments: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/kubectl"

export KUBECTL_BIN="${fake_bin}/kubectl"
export FLEX_NIGHTLY_DEPLOY_MODE='shared-cluster-controllers'
export FLEX_NIGHTLY_INCLUDE_DGX='true'
export FLEX_NIGHTLY_DGX_SELECTOR='example.com/node-pool=spark'
export FLEX_NIGHTLY_DGX_SITE='site-dgx'
export FLEX_NIGHTLY_A100_SITE='site-a100'
export FLEX_NIGHTLY_H200_SITE='site-h200'
"$PREFLIGHT" validate-config

if FLEX_NIGHTLY_DEPLOY_MODE=isolated-controllers "$PREFLIGHT" validate-config >/dev/null 2>&1; then
  fail "preflight must reject unsafe isolated controller installation"
fi

export FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/contract.json"
"$PREFLIGHT" cluster
jq -e '
  .deploy_mode == "shared-cluster-controllers"
  and .topology == "taugrid-gpu-topology"
  and (.targets | length) == 3
  and (.targets | all(.capability_ready))
  and (.targets[] | select(.name == "dgx-spark") | .capability_contract.architecture) == "arm64"
  and (.targets[] | select(.name == "a100") | .capability_contract.gpus_per_node) == 8
  and (.targets[] | select(.name == "dgx-spark") | .gpu_available) == 2
  and (.targets[] | select(.name == "a100") | .gpu_available) == 15
  and (.targets[] | select(.name == "h200") | .gpu_available) == 16
  and .rdma_target.rdma_nodes == 2
' "$FLEX_NIGHTLY_CONTRACT_FILE" >/dev/null ||
  fail "preflight contract does not preserve the hardware matrix inventory"

if FAKE_STALE_H200=1 "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject GPU flavors that are not wired to taugrid-gpu-topology"
fi
if FAKE_STALE_LEASE=1 "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject stale Flex node leases"
fi

cp "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"
"$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"

jq '(.targets[] | select(.name == "a100") | .nodes[0].gpu_allocatable) = 7' \
  "$FLEX_NIGHTLY_CONTRACT_FILE" >"${fake_bin}/drifted.json"
if "$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/drifted.json" >/dev/null 2>&1; then
  fail "postflight comparison must reject changed Flex node capability"
fi

echo "TauGrid Flex nightly contract tests passed"
