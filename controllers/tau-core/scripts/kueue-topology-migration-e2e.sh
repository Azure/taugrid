#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/kind.sh"

CLUSTER_NAME="${TAU_TOPOLOGY_MIGRATION_KIND_CLUSTER_NAME:-tau-topology-migration-e2e}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
KUEUE_VERSION="${TAU_TOPOLOGY_MIGRATION_KUEUE_VERSION:-0.19.2}"
WAIT_SECONDS="${TAU_TOPOLOGY_MIGRATION_WAIT_SECONDS:-120}"
DELETE_CLUSTER="${TAU_TOPOLOGY_MIGRATION_DELETE_CLUSTER:-1}"
CONTAINER_ENGINE="$(tau_kind_select_engine "${TAU_TOPOLOGY_MIGRATION_CONTAINER_ENGINE:-}" docker)"
tau_kind_configure_provider "${CONTAINER_ENGINE}"

cleanup() {
  local status=$?
  if [[ "${DELETE_CLUSTER}" == "1" ]]; then
    tau_kind_delete_cluster "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  return "${status}"
}
trap cleanup EXIT

for tool in kind kubectl helm "${CONTAINER_ENGINE}"; do
  tau_kind_need "${tool}"
done
if ! "${CONTAINER_ENGINE}" info >/dev/null 2>&1; then
  echo "${CONTAINER_ENGINE} is not reachable" >&2
  exit 69
fi

if tau_kind_cluster_exists "${CONTAINER_ENGINE}" "${CLUSTER_NAME}"; then
  tau_kind_delete_cluster "${CLUSTER_NAME}"
fi
tau_kind_create_cluster "${CLUSTER_NAME}" --wait "${WAIT_SECONDS}s"
kubectl config use-context "${KUBE_CONTEXT}" >/dev/null

helm upgrade --install kueue \
  "oci://mcr.microsoft.com/aks/ai-runtime/helm/kueue" \
  --version "${KUEUE_VERSION}" \
  --namespace kueue-system \
  --create-namespace \
  --wait \
  --timeout "${WAIT_SECONDS}s"

kubectl apply -f - <<'YAML'
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: taugrid-gpu-topology
spec:
  levels:
    - nodeLabel: tau.azure.com/site
    - nodeLabel: tau.azure.com/network-domain
    - nodeLabel: kubernetes.io/hostname
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: taugrid-gpu-topology-v2
spec:
  levels:
    - nodeLabel: tau.azure.com/site
    - nodeLabel: tau.azure.com/network-domain
    - nodeLabel: tau.azure.com/accelerator-domain
    - nodeLabel: kubernetes.io/hostname
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: taugrid-default-gpu-topology
spec:
  topologyName: taugrid-gpu-topology
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: taugrid-default-gpu-topology-v2
spec:
  topologyName: taugrid-gpu-topology-v2
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: topology-migration
spec:
  namespaceSelector: {}
  resourceGroups:
    - coveredResources: [cpu]
      flavors:
        - name: taugrid-default-gpu-topology
          resources:
            - name: cpu
              nominalQuota: "1"
YAML

for topology in taugrid-gpu-topology taugrid-gpu-topology-v2; do
  deadline=$((SECONDS + WAIT_SECONDS))
  finalizers=""
  while (( SECONDS < deadline )); do
    finalizers="$(kubectl get topology.kueue.x-k8s.io "${topology}" \
      -o jsonpath='{.metadata.finalizers[*]}' 2>/dev/null || true)"
    if [[ "${finalizers}" == *"kueue.x-k8s.io/resource-in-use"* ]]; then
      break
    fi
    sleep 2
  done
  if [[ "${finalizers}" != *"kueue.x-k8s.io/resource-in-use"* ]]; then
    echo "Kueue did not add its in-use finalizer to ${topology}: ${finalizers:-<missing>}" >&2
    exit 1
  fi
done

kubectl delete topology.kueue.x-k8s.io taugrid-gpu-topology --wait=false
sleep 5
deletion_timestamp="$(kubectl get topology.kueue.x-k8s.io taugrid-gpu-topology \
  -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
if [[ -z "${deletion_timestamp}" ]]; then
  echo "old Topology deletion was not blocked by its referencing ResourceFlavor" >&2
  exit 1
fi

kubectl patch clusterqueue.kueue.x-k8s.io topology-migration --type=merge -p \
  '{"spec":{"resourceGroups":[{"coveredResources":["cpu"],"flavors":[{"name":"taugrid-default-gpu-topology-v2","resources":[{"name":"cpu","nominalQuota":"1"}]}]}]}}'
queue_flavor="$(kubectl get clusterqueue.kueue.x-k8s.io topology-migration \
  -o jsonpath='{.spec.resourceGroups[0].flavors[0].name}')"
if [[ "${queue_flavor}" != "taugrid-default-gpu-topology-v2" ]]; then
  echo "ClusterQueue still references ${queue_flavor}, want v2 flavor" >&2
  exit 1
fi

kubectl delete resourceflavor.kueue.x-k8s.io taugrid-default-gpu-topology
kubectl wait --for=delete topology.kueue.x-k8s.io/taugrid-gpu-topology \
  --timeout="${WAIT_SECONDS}s"

kubectl get topology.kueue.x-k8s.io taugrid-gpu-topology-v2 >/dev/null
kubectl get resourceflavor.kueue.x-k8s.io taugrid-default-gpu-topology-v2 >/dev/null
echo "Kueue finalizer-safe topology migration passed"
