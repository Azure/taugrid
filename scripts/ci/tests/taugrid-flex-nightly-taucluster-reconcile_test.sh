#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly RECONCILER="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-taucluster-reconcile.sh"

fail() {
  echo "TauGrid Flex TauCluster reconcile test failed: $*" >&2
  exit 1
}

[ -x "$RECONCILER" ] || fail "TauCluster reconcile helper must be executable"

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/chart/templates" "${fixture}/artifacts"

cat >"${fixture}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat <<'YAML'
apiVersion: tau.azure.com/v1alpha1
kind: TauCluster
metadata:
  name: cluster
spec:
  nodes:
    labelRules:
      - match:
          vmSizes: [Standard_ND96amsr_A100_v4]
        labels:
          kueue.azure.com/gpu-series: ndm-a100-v4
          tau.azure.com/gpu-class: a100-80gb
      - match:
          vmSizes: [Standard_NC40ads_H100_v5, Standard_NC80adis_H100_v5]
        labels:
          kueue.azure.com/gpu-series: nc-h100-v5
          tau.azure.com/gpu-class: h100-95gb
      - match:
          vmSizes: [Standard_ND96isr_H200_v5]
        labels:
          kueue.azure.com/gpu-series: nd-h200-v5
          tau.azure.com/gpu-class: h200-141gb
YAML
EOF

cat >"${fixture}/cluster.json" <<'JSON'
{
  "apiVersion": "tau.azure.com/v1alpha1",
  "kind": "TauCluster",
  "metadata": {"name": "cluster", "generation": 4, "resourceVersion": "10"},
  "spec": {
    "managementMode": "Reconcile",
    "nodes": {
      "labelRules": [
        {
          "match": {"vmSizes": ["Standard_ND96amsr_A100_v4"]},
          "labels": {
            "kueue.azure.com/gpu-series": "ndm-a100-v4",
            "tau.azure.com/gpu-class": "a100-80gb"
          }
        },
        {
          "match": {"vmSizes": ["Custom_GPU_v1"]},
          "labels": {
            "kueue.azure.com/gpu-series": "custom-gpu-v1",
            "tau.azure.com/gpu-class": "custom-1gb"
          }
        }
      ]
    },
    "queues": {"ownership": "External"},
    "workloadProfiles": [
      {"name": "custom.profile", "gpusPerWorker": 1, "workerCount": 1}
    ]
  },
  "status": {
    "observedGeneration": 4,
    "conditions": [
      {"type": "NodesReady", "status": "True"},
      {"type": "Ready", "status": "True"}
    ]
  }
}
JSON

cat >"${fixture}/nodes.json" <<'JSON'
{
  "items": [
    {
      "metadata": {
        "name": "a100",
        "labels": {
          "node.kubernetes.io/instance-type": "Standard_ND96amsr_A100_v4",
          "kueue.azure.com/gpu-series": "ndm-a100-v4",
          "tau.azure.com/gpu-class": "a100-80gb"
        }
      },
      "spec": {},
      "status": {"conditions": [{"type":"Ready","status":"True"}], "allocatable": {"nvidia.com/gpu": "8"}}
    },
    {
      "metadata": {
        "name": "h100",
        "labels": {
          "node.kubernetes.io/instance-type": "Standard_NC80adis_H100_v5",
          "kueue.azure.com/gpu-series": "nc-h100-v5",
          "tau.azure.com/gpu-class": "h100-95gb"
        }
      },
      "spec": {},
      "status": {"conditions": [{"type":"Ready","status":"True"}], "allocatable": {"nvidia.com/gpu": "2"}}
    },
    {
      "metadata": {
        "name": "h200",
        "labels": {
          "node.kubernetes.io/instance-type": "Standard_ND96isr_H200_v5",
          "kueue.azure.com/gpu-series": "nd-h200-v5",
          "tau.azure.com/gpu-class": "h200-141gb"
        }
      },
      "spec": {},
      "status": {"conditions": [{"type":"Ready","status":"True"}], "allocatable": {"nvidia.com/gpu": "8"}}
    }
  ]
}
JSON

cat >"${fixture}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_KUBECTL_LOG}"
case "$*" in
  "create --dry-run=client -f - -o json")
    cat >/dev/null
    cat <<'JSON'
{"spec":{"nodes":{"labelRules":[{"match":{"vmSizes":["Standard_ND96amsr_A100_v4"]},"labels":{"kueue.azure.com/gpu-series":"ndm-a100-v4","tau.azure.com/gpu-class":"a100-80gb"}},{"match":{"vmSizes":["Standard_NC40ads_H100_v5","Standard_NC80adis_H100_v5"]},"labels":{"kueue.azure.com/gpu-series":"nc-h100-v5","tau.azure.com/gpu-class":"h100-95gb"}},{"match":{"vmSizes":["Standard_ND96isr_H200_v5"]},"labels":{"kueue.azure.com/gpu-series":"nd-h200-v5","tau.azure.com/gpu-class":"h200-141gb"}}]}}}
JSON
    ;;
  *"get clusters.tau.azure.com cluster -o json")
    cat "${FAKE_CLUSTER_JSON}"
    ;;
  *"patch clusters.tau.azure.com cluster "*)
    printf '%s\n' "$*" >"${FAKE_PATCH_LOG}"
    cat "${FAKE_CLUSTER_JSON}"
    ;;
  *"get nodes -o json")
    cat "${FAKE_NODES_JSON}"
    ;;
  *)
    echo "unexpected kubectl invocation: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fixture}/bin/helm" "${fixture}/bin/kubectl"

export HELM_BIN="${fixture}/bin/helm"
export KUBECTL_BIN="${fixture}/bin/kubectl"
export FAKE_CLUSTER_JSON="${fixture}/cluster.json"
export FAKE_NODES_JSON="${fixture}/nodes.json"
export FAKE_KUBECTL_LOG="${fixture}/kubectl.log"
export FAKE_PATCH_LOG="${fixture}/patch.log"
export FLEX_NIGHTLY_CONTROLLER_CHART="${fixture}/chart"
export FLEX_NIGHTLY_TAUCLUSTER_ARTIFACT_DIR="${fixture}/artifacts"
export FLEX_NIGHTLY_TAUCLUSTER_TIMEOUT_SECONDS=5

"$RECONCILER"

jq -e '
  map(select(.path == "/spec/nodes/labelRules"))[0].value
  | any(.[]; .match.vmSizes == ["Standard_NC40ads_H100_v5", "Standard_NC80adis_H100_v5"])
' "${fixture}/artifacts/taucluster-patch.json" >/dev/null ||
  fail "reviewed H100 rules must be added to the live TauCluster"
jq -e '
  map(select(.path == "/spec/nodes/labelRules"))[0].value
  | any(.[]; .match.vmSizes == ["Custom_GPU_v1"])
' "${fixture}/artifacts/taucluster-patch.json" >/dev/null ||
  fail "cluster-specific node label rules must be preserved"
jq -e '
  map(select(.path == "/spec/workloadProfiles"))[0].value
  | any(.[]; .name == "custom.profile")
  and any(.[]; .name == "nightly.cpu.1x" and .gpusPerWorker == 0)
' "${fixture}/artifacts/taucluster-patch.json" >/dev/null ||
  fail "custom profiles and the nightly CPU profile must both be retained"
jq -e '
  .required_gpu_classes == ["a100-80gb", "h100-95gb", "h200-141gb"]
  and (.nodes | length) == 3
' "${fixture}/artifacts/gpu-class-inventory.json" >/dev/null ||
  fail "GPU class inventory must prove A100, H100, and H200 discovery"

jq '
  .spec.nodes.labelRules += [{
    "match":{"vmSizes":["Standard_NC40ads_H100_v5"]},
    "labels":{
      "kueue.azure.com/gpu-series":"wrong-h100-series",
      "tau.azure.com/gpu-class":"h100-95gb"
    }
  }]
' "${fixture}/cluster.json" >"${fixture}/conflicting-cluster.json"
export FAKE_CLUSTER_JSON="${fixture}/conflicting-cluster.json"
if "$RECONCILER" >/dev/null 2>&1; then
  fail "conflicting live rules must fail closed instead of overwriting platform policy"
fi

echo "TauGrid Flex TauCluster reconcile tests passed"
