#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

# Tau core controller Kind E2E
# ============================
#
# Proves the Kubernetes-native TauWorkspace onboarding path end to end on a
# local Kind cluster, using direct Kubernetes desired state as a GitOps stand-in:
#
#   1. Build the current controller image and install it (Deployment and RBAC)
#      plus the v0.5 tau.azure.com CRDs
#      from the chart's Kustomize manifests via `kubectl apply -k`.
#   2. Prove topology labels reconcile on an existing native-style Node, a newly
#      joined Flex-style Node, and subsequent label drift.
#   3. Apply a native TauWorkspace as platform desired state.
#   4. Prove the Workspace reaches Ready without a storage readiness condition,
#      while a platform-owned PVC binds for later workloads.
#   5. Prove the researcher subject can get the one workspace in
#      tau-platform and create/read ordinary namespaced workload resources,
#      but cannot read another namespace's secrets or create cluster-scoped
#      resources.
#   6. Delete only the TauWorkspace desired state and prove the controller's
#      finalizer removes controller-owned resources while platform-owned
#      Namespace/PVC state remains.
#   7. Assert only workspaces.tau.azure.com is ever used (the legacy
#      rune.example.com prototype API is never touched by this flow).
#
# Ownership / scope
# ------------------
# This file (and any new files under this scripts/ directory) is the only
# thing this test module is allowed to change. It never edits controller
# reconcile logic, the Tau CLI, tau-queues, docs, or a live cluster.
#
# GPU: Kind has no GPU device plugin and this script installs no real Kueue
# controller. The mock ClusterQueue below carries a fake `nvidia.com/gpu`
# resource purely so the render contract exercises the real schema shape;
# nothing here schedules, claims, or executes on a real GPU.

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CONTROLLER_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd -- "${CONTROLLER_DIR}/../.." && pwd)"
TAU_DIR="${REPO_ROOT}/cli"
APP_BASE_DIR="${REPO_ROOT}/charts/tau-core-controller"
IMAGE_DOCKERFILE="${REPO_ROOT}/images/tau-core-controller/Dockerfile"

CLUSTER_NAME="${TAU_CORE_KIND_CLUSTER_NAME:-tau-core-e2e}"
KUBE_CONTEXT="${TAU_CORE_KIND_CONTEXT:-kind-${CLUSTER_NAME}}"
PLATFORM_NAMESPACE="tau-platform" # fixed: cli/internal/workspace.PlatformNamespace
WORKSPACE_NAME="${TAU_CORE_KIND_WORKSPACE:-aurora}"
WORKSPACE_GROUP="${TAU_CORE_KIND_GROUP:-aurora-researchers}"
TARGET_NAMESPACE="${TAU_CORE_KIND_TARGET_NAMESPACE:-aurora}"
STORAGE_CLASS_NAME="tau-workspace-e2e-manual"
WAIT_SECONDS="${TAU_CORE_KIND_WAIT_SECONDS:-120}"
ROLLOUT_WAIT_SECONDS="${TAU_CORE_KIND_ROLLOUT_WAIT_SECONDS:-180}"
DELETE_CLUSTER="${TAU_CORE_KIND_DELETE_CLUSTER:-}"
RECREATE_CLUSTER="${TAU_CORE_KIND_RECREATE:-0}"
LOCAL_IMAGE="${TAU_CORE_KIND_LOCAL_IMAGE:-tau-core-controller:kind-e2e}"
STATIC_ONLY="${TAU_CORE_KIND_STATIC_ONLY:-0}"
if [[ -n "${TAU_CORE_KIND_CONTAINER_ENGINE:-}" ]]; then
  CONTAINER_ENGINE="${TAU_CORE_KIND_CONTAINER_ENGINE}"
elif [[ "${KIND_EXPERIMENTAL_PROVIDER:-}" == "podman" ]]; then
  CONTAINER_ENGINE="podman"
else
  CONTAINER_ENGINE="docker"
fi
if [[ "${CONTAINER_ENGINE}" == "podman" && "${LOCAL_IMAGE}" != */* ]]; then
  # Podman canonicalizes unqualified local names under the localhost registry.
  LOCAL_IMAGE="localhost/${LOCAL_IMAGE}"
fi
SCRATCH_DIR="${CONTROLLER_DIR}/.kind-e2e-scratch"
CREATED_CLUSTER=0

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 127
  fi
}

# Bounded wrapper: prefer GNU coreutils `timeout` (Linux CI), fall back to
# macOS's `gtimeout` (Homebrew coreutils) if present, else run unbounded (the
# caller is still expected to be a short, deterministic command).
bounded() {
  local seconds="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "${seconds}" "$@"
  elif command -v gtimeout >/dev/null 2>&1; then
    gtimeout "${seconds}" "$@"
  else
    "$@"
  fi
}

wait_for_gpu_series() {
  local node="$1"
  local want="$2"
  local deadline=$((SECONDS + WAIT_SECONDS))
  local got=""
  while (( SECONDS < deadline )); do
    got="$(kubectl get node "${node}" \
      -o go-template='{{ index .metadata.labels "kueue.azure.com/gpu-series" }}' \
      2>/dev/null || true)"
    if [[ "${got}" == "${want}" ]]; then
      return 0
    fi
    sleep 2
  done
  echo "node ${node} gpu-series=${got:-<missing>}, want ${want}" >&2
  return 1
}

load_local_image() {
  if [[ "${CONTAINER_ENGINE}" == "podman" ]]; then
    local archive="${SCRATCH_DIR}/controller-image.tar"
    # kind's Podman provider still shells out to `docker image inspect` for
    # docker-image loads. An OCI-independent archive avoids requiring Docker.
    podman save --format docker-archive -o "${archive}" "${LOCAL_IMAGE}"
    KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive "${archive}" --name "${CLUSTER_NAME}"
    return
  fi

  KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" \
    kind load docker-image "${LOCAL_IMAGE}" --name "${CLUSTER_NAME}"
}

cleanup() {
  local status=$?
  if [[ "${DELETE_CLUSTER}" == "1" || ( -z "${DELETE_CLUSTER}" && "${CREATED_CLUSTER}" == "1" ) ]]; then
    KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  rm -rf "${SCRATCH_DIR}"
  return "${status}"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Static guards: no cluster, no Docker, no network required. These run first
# so `TAU_CORE_KIND_STATIC_ONLY=1` can validate the harness itself (and
# the manifests it depends on) in an environment where Docker/Kind are
# unavailable.
# ---------------------------------------------------------------------------

echo "== static guard: legacy rune.example.com API must not appear in the manifests/code this harness installs =="
legacy_hits="$(grep -rn "rune\.example\.com" \
  "${CONTROLLER_DIR}/api" \
  "${CONTROLLER_DIR}/internal" \
  "${CONTROLLER_DIR}/config" \
  "${APP_BASE_DIR}" \
  "${TAU_DIR}/internal/workspace" \
  "${TAU_DIR}/internal/cli/workspace.go" \
  2>/dev/null || true)"
if [[ -n "${legacy_hits}" ]]; then
  echo "found forbidden legacy rune.example.com references:" >&2
  echo "${legacy_hits}" >&2
  exit 1
fi

echo "== static guard: CRDs and rendered TauWorkspace object must use tau.azure.com only =="
for crd in \
  "${CONTROLLER_DIR}/config/crd/bases/tau.azure.com_clusters.yaml" \
  "${CONTROLLER_DIR}/config/crd/bases/tau.azure.com_workspaces.yaml" \
  "${CONTROLLER_DIR}/config/crd/bases/tau.azure.com_quotarequests.yaml"; do
  grep -q "^  group: tau.azure.com$" "${crd}"
done
grep -q 'APIGroup\s*=\s*"tau.azure.com"' "${TAU_DIR}/internal/workspace/workspace.go"

echo "== static guard: local-image deployment rewrite is well-formed =="
if [[ ! -f "${IMAGE_DOCKERFILE}" ]]; then
  echo "controller image Dockerfile not found: ${IMAGE_DOCKERFILE}" >&2
  exit 1
fi
rendered_deployment="$(sed \
  -e "s#image: mcr\.microsoft\.com/aks/ai-runtime/tau-core-controller@sha256:[a-f0-9]*#image: ${LOCAL_IMAGE}#" \
  -e "s#imagePullPolicy: IfNotPresent#imagePullPolicy: Never#" \
  -e "s#- --leader-elect\$#- --leader-elect=false#" \
  "${APP_BASE_DIR}/kustomize/deployment.yaml")"
for want in \
  "image: ${LOCAL_IMAGE}" \
  "imagePullPolicy: Never" \
  "- --leader-elect=false"; do
  if ! grep -qF -- "${want}" <<<"${rendered_deployment}"; then
    echo "local-image deployment rewrite is missing expected line: ${want}" >&2
    exit 1
  fi
done
if grep -q "azurecr.io" <<<"${rendered_deployment}"; then
  echo "local-image deployment rewrite still references the private ACR digest" >&2
  exit 1
fi

echo "== static guard: go build/vet for the controller module =="
(cd "${CONTROLLER_DIR}" && go build ./... && go vet ./...)

echo "static checks passed"
if [[ "${STATIC_ONLY}" == "1" ]]; then
  echo "TAU_CORE_KIND_STATIC_ONLY=1: skipping Kind/container-engine cluster steps"
  exit 0
fi

# ---------------------------------------------------------------------------
# Live cluster steps below. Kind supports Docker by default and Podman when
# KIND_EXPERIMENTAL_PROVIDER=podman. Build and load through the same provider.
# ---------------------------------------------------------------------------

for tool in kind kubectl go python3 "${CONTAINER_ENGINE}" sed; do
  need "${tool}"
done

echo "== checking ${CONTAINER_ENGINE} engine reachability (bounded 15s) =="
if ! bounded 15 "${CONTAINER_ENGINE}" info >/dev/null 2>&1; then
  echo "${CONTAINER_ENGINE} engine is not reachable (${CONTAINER_ENGINE} info timed out or failed)." >&2
  echo "This harness requires a working container engine for Kind. Skipping the live cluster run." >&2
  exit 69 # EX_UNAVAILABLE
fi

rm -rf "${SCRATCH_DIR}"
mkdir -p "${SCRATCH_DIR}"

if ! KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" kind create cluster --name "${CLUSTER_NAME}" --wait 120s
  CREATED_CLUSTER=1
elif [[ "${RECREATE_CLUSTER}" == "1" ]]; then
  KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" kind delete cluster --name "${CLUSTER_NAME}"
  KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-}" kind create cluster --name "${CLUSTER_NAME}" --wait 120s
  CREATED_CLUSTER=1
fi

kubectl config use-context "${KUBE_CONTEXT}" >/dev/null

# --- Mock Kueue CRDs -------------------------------------------------------
#
# LocalQueue is served in BOTH v1beta1 and v1beta2: the controller's own
# queueStatus()/clusterQueueGPUQuota() (internal/controller/workspace_controller.go)
# get LocalQueue/ClusterQueue at v1beta1, while reconciliation applies
# LocalQueue at v1beta2 (matching the production Kueue API). Both
# schemas are intentionally x-kubernetes-preserve-unknown-fields so the
# default "None" conversion strategy is a lossless passthrough between them.
cat >"${SCRATCH_DIR}/mock-kueue-crds.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: localqueues.kueue.x-k8s.io
spec:
  group: kueue.x-k8s.io
  names:
    kind: LocalQueue
    listKind: LocalQueueList
    plural: localqueues
    singular: localqueue
  scope: Namespaced
  versions:
    - name: v1beta1
      served: true
      storage: false
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
    - name: v1beta2
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterqueues.kueue.x-k8s.io
spec:
  group: kueue.x-k8s.io
  names:
    kind: ClusterQueue
    listKind: ClusterQueueList
    plural: clusterqueues
    singular: clusterqueue
  scope: Cluster
  versions:
    - name: v1beta2
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: workloads.kueue.x-k8s.io
spec:
  group: kueue.x-k8s.io
  names:
    kind: Workload
    listKind: WorkloadList
    plural: workloads
    singular: workload
  scope: Namespaced
  versions:
    - name: v1beta1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
YAML
kubectl apply -f "${SCRATCH_DIR}/mock-kueue-crds.yaml"

# --- Establish the Tau APIs before applying the same full kustomization ArgoCD
# applies. kubectl's RESTMapper cannot discover a CRD and its TauCluster
# instance in one fresh-cluster apply pass. ---
kubectl apply -f "${APP_BASE_DIR}/kustomize/namespace.yaml"
kubectl apply -f "${APP_BASE_DIR}/crds"
kubectl wait --for=condition=Established \
  crd/clusters.tau.azure.com \
  crd/quotarequests.tau.azure.com \
  crd/workspaces.tau.azure.com \
  --timeout="${WAIT_SECONDS}s"
kubectl apply -k "${APP_BASE_DIR}"

# --- Build and load the controller image locally; no external mutable
# image is introduced, and no network access is required after this point.
"${CONTAINER_ENGINE}" build \
  --file "${IMAGE_DOCKERFILE}" \
  --tag "${LOCAL_IMAGE}" \
  "${CONTROLLER_DIR}"
load_local_image

# Overlay the Deployment with the locally-built image and
# imagePullPolicy: Never so nothing tries to reach the network, plus
# --leader-elect=false since a single-replica Kind run has no need for the
# extra Lease-acquisition startup latency.
sed \
  -e "s#image: mcr\.microsoft\.com/aks/ai-runtime/tau-core-controller@sha256:[a-f0-9]*#image: ${LOCAL_IMAGE}#" \
  -e "s#imagePullPolicy: IfNotPresent#imagePullPolicy: Never#" \
  -e "s#- --leader-elect\$#- --leader-elect=false#" \
  "${APP_BASE_DIR}/kustomize/deployment.yaml" >"${SCRATCH_DIR}/deployment.local.yaml"
kubectl apply -f "${SCRATCH_DIR}/deployment.local.yaml"
kubectl -n "${PLATFORM_NAMESPACE}" rollout status deployment/tau-core-controller --timeout="${ROLLOUT_WAIT_SECONDS}s"

# --- Continuous native/Flex Node topology reconciliation -------------------
#
# Kind cannot join a real AKS Flex machine, so this uses the exact portable
# contract shared by both providers: node.kubernetes.io/instance-type. The
# existing Kind Node stands in for an AKS-managed pool Node; a newly created
# Node object stands in for a later Flex join.
echo "== proving continuous native and Flex Node topology reconciliation =="
native_node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
kubectl label node "${native_node}" \
  node.kubernetes.io/instance-type=Standard_ND96amsr_A100_v4 --overwrite

cat >"${SCRATCH_DIR}/flex-node.yaml" <<'YAML'
apiVersion: v1
kind: Node
metadata:
  name: simulated-flex-h200
  labels:
    node.kubernetes.io/instance-type: Standard_ND96isr_H200_v5
    example.com/flex-join: "true"
YAML
kubectl apply -f "${SCRATCH_DIR}/flex-node.yaml"

wait_for_gpu_series "${native_node}" ndm-a100-v4
wait_for_gpu_series simulated-flex-h200 nd-h200-v5
kubectl get node simulated-flex-h200 \
  -o go-template='{{ index .metadata.labels "example.com/flex-join" }}' |
  grep -qx true

kubectl label node simulated-flex-h200 \
  kueue.azure.com/gpu-series=stale-value --overwrite
wait_for_gpu_series simulated-flex-h200 nd-h200-v5

deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  nodes_ready="$(kubectl get clusters.tau.azure.com cluster \
    -o jsonpath='{.status.conditions[?(@.type=="NodesReady")].status}' \
    2>/dev/null || true)"
  [[ "${nodes_ready}" == "True" ]] && break
  sleep 2
done
[[ "${nodes_ready:-}" == "True" ]]

# --- Manual, Immediate-binding StorageClass + unclaimed PV so the
# platform-owned PVC is usable by later workload exercises without a
# consuming pod. Path is inside the ephemeral Kind node container, not this
# host.
"${CONTAINER_ENGINE}" exec "${CLUSTER_NAME}-control-plane" \
  sh -c 'mkdir -p /var/local/tau-workspace-e2e-pv && chmod 0777 /var/local/tau-workspace-e2e-pv'
cat >"${SCRATCH_DIR}/manual-storage.yaml" <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ${STORAGE_CLASS_NAME}
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: Immediate
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${STORAGE_CLASS_NAME}-pv
spec:
  capacity:
    storage: 1Mi
  accessModes: ["ReadWriteOnce"]
  storageClassName: ${STORAGE_CLASS_NAME}
  persistentVolumeReclaimPolicy: Delete
  hostPath:
    path: /var/local/tau-workspace-e2e-pv
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: ${WORKSPACE_NAME}
spec:
  # namespaceSelector: {} admits the test namespace.
  namespaceSelector: {}
  resourceGroups:
    - coveredResources: ["nvidia.com/gpu"]
      flavors:
        - name: h200
          resources:
            # Schema-only placeholder: Kind has no GPU device plugin and no
            # real Kueue controller is installed here, so this quota is never
            # actually scheduled against. No real GPU execution is claimed.
            - name: nvidia.com/gpu
              nominalQuota: 16
YAML
kubectl apply -f "${SCRATCH_DIR}/manual-storage.yaml"

# ---------------------------------------------------------------------------
# Native desired state: platform applies TauWorkspace directly.
# ---------------------------------------------------------------------------
echo "== building a researcher impersonation kubeconfig =="
kubectl config view --raw --minify --flatten --context "${KUBE_CONTEXT}" -o json >"${SCRATCH_DIR}/kubeconfig-impersonation.json"
cat >"${SCRATCH_DIR}/add-impersonation-context.py" <<'PY'
import json
import sys

path = sys.argv[1]
researcher_group = sys.argv[2]
with open(path) as f:
    cfg = json.load(f)

existing_user = cfg["contexts"][0]["context"]["user"]
existing_cluster = cfg["contexts"][0]["context"]["cluster"]
base_user = next(u for u in cfg["users"] if u["name"] == existing_user)

researcher = json.loads(json.dumps(base_user))
researcher["name"] = "researcher-impersonator"
researcher["user"]["as"] = "researcher@example.com"
researcher["user"]["as-groups"] = [researcher_group]
cfg["users"].append(researcher)
cfg["contexts"].append({
    "name": "researcher",
    "context": {"cluster": existing_cluster, "user": "researcher-impersonator"},
})

with open(path, "w") as f:
    json.dump(cfg, f)
PY
python3 "${SCRATCH_DIR}/add-impersonation-context.py" "${SCRATCH_DIR}/kubeconfig-impersonation.json" "${WORKSPACE_GROUP}"

echo "== apply native TauWorkspace desired state =="
cat >"${SCRATCH_DIR}/workspace.yaml" <<YAML
apiVersion: tau.azure.com/v1alpha1
kind: TauWorkspace
metadata:
  name: ${WORKSPACE_NAME}
  namespace: ${PLATFORM_NAMESPACE}
spec:
  authorization:
    mode: workspace-rbac
  principalRef:
    provider: entra
    name: ${WORKSPACE_GROUP}
  kubernetesSubject:
    kind: Group
    name: ${WORKSPACE_GROUP}
  role: tau-researcher-v1
  target:
    namespace: ${TARGET_NAMESPACE}
    createNamespace: true
  queue: ${WORKSPACE_NAME}
  workloadIdentity:
    serviceAccountName: tau-workload
    clientId: kind-client-id
  defaults:
    outputRoot: /data/projects/${WORKSPACE_NAME}/runs
YAML
kubectl apply --server-side --field-manager=tau-kind-e2e -f "${SCRATCH_DIR}/workspace.yaml"

# ---------------------------------------------------------------------------
# Wait for the reconciled namespace/ServiceAccount and Ready phase, and
# separately require the PVC to be Bound before workload exercises use it.
# ---------------------------------------------------------------------------
deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  kubectl get namespace "${TARGET_NAMESPACE}" >/dev/null 2>&1 && break
  sleep 1
done
kubectl get namespace "${TARGET_NAMESPACE}" >/dev/null
kubectl get namespace "${TARGET_NAMESPACE}" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' | grep -qx baseline
kubectl get namespace "${TARGET_NAMESPACE}" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/audit}' | grep -qx restricted
kubectl get namespace "${TARGET_NAMESPACE}" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/warn}' | grep -qx restricted

cat >"${SCRATCH_DIR}/workspace-storage.yaml" <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: blob-training
  namespace: ${TARGET_NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Mi
  storageClassName: ${STORAGE_CLASS_NAME}
  volumeName: ${STORAGE_CLASS_NAME}-pv
YAML
kubectl apply -f "${SCRATCH_DIR}/workspace-storage.yaml"

deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  kubectl -n "${TARGET_NAMESPACE}" get serviceaccount tau-workload >/dev/null 2>&1 && break
  sleep 1
done
kubectl -n "${TARGET_NAMESPACE}" get serviceaccount tau-workload -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}' | grep -qx kind-client-id
kubectl -n "${TARGET_NAMESPACE}" get serviceaccount tau-workload -o jsonpath='{.metadata.labels.azure\.workload\.identity/use}' | grep -qx "true"

deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  phase="$(kubectl -n "${TARGET_NAMESPACE}" get pvc blob-training -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "${phase}" == "Bound" ]] && break
  sleep 1
done
kubectl -n "${TARGET_NAMESPACE}" get pvc blob-training -o jsonpath='{.status.phase}' | grep -qx Bound

deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  phase="$(kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "${phase}" == "Ready" ]] && break
  sleep 1
done
phase="$(kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" -o jsonpath='{.status.phase}')"
if [[ "${phase}" != "Ready" ]]; then
  echo "workspace ${WORKSPACE_NAME} did not reach Ready (phase=${phase})" >&2
  kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" -o yaml >&2 || true
  kubectl -n "${PLATFORM_NAMESPACE}" logs deployment/tau-core-controller --tail=200 >&2 || true
  exit 1
fi

echo "== verifying StorageReady is absent and the workspace-cleanup finalizer exists =="
workspace_conditions="$(kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}')"
if grep -qx StorageReady <<<"${workspace_conditions}"; then
  echo "workspace status unexpectedly contains StorageReady" >&2
  exit 1
fi
kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  -o jsonpath='{.metadata.finalizers}' | grep -q "tau.azure.com/workspace-cleanup"

# ---------------------------------------------------------------------------
# RBAC boundary: researcher can read their own workspace and work in their
# namespace, but cannot read another namespace's secrets or create
# cluster-scoped resources.
# ---------------------------------------------------------------------------
echo "== RBAC boundary checks for the researcher subject =="
kubectl auth can-i create jobs.batch -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
kubectl auth can-i get jobs.batch -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
kubectl auth can-i create configmaps -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
kubectl auth can-i get configmaps -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
kubectl auth can-i get "workspaces.tau.azure.com/${WORKSPACE_NAME}" -n "${PLATFORM_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
kubectl auth can-i create quotarequests.tau.azure.com -n "${PLATFORM_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
[[ "$(kubectl auth can-i list workspaces.tau.azure.com -n "${PLATFORM_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]
[[ "$(kubectl auth can-i update "workspaces.tau.azure.com/${WORKSPACE_NAME}" -n "${PLATFORM_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]
# Cannot read another namespace's secrets: tau-researcher-v1 is bound via a
# namespace-scoped RoleBinding in the target namespace only, so this same
# subject has zero permissions in tau-platform (or any other namespace).
[[ "$(kubectl auth can-i get secrets -n "${PLATFORM_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]
[[ "$(kubectl auth can-i list secrets -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" != "yes" ]] # secrets are namespace-scoped create/get, not enumerable elsewhere; see below for the positive case
kubectl auth can-i get secrets -n "${TARGET_NAMESPACE}" --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" | grep -qx yes
# Cannot create cluster-scoped resources.
[[ "$(kubectl auth can-i create namespaces --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]
[[ "$(kubectl auth can-i create clusterroles --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]
[[ "$(kubectl auth can-i create persistentvolumes --as=researcher@example.com --as-group="${WORKSPACE_GROUP}" || true)" == "no" ]]

echo "== quota decision admission checks for the researcher subject =="
cat >"${SCRATCH_DIR}/quota-request-pending.yaml" <<YAML
apiVersion: tau.azure.com/v1alpha1
kind: TauQuotaRequest
metadata:
  name: ${WORKSPACE_NAME}-pending
  namespace: ${PLATFORM_NAMESPACE}
spec:
  workspace: ${WORKSPACE_NAME}
  resource: h200
  requested: 1
  reason: kind-e2e-pending-request
  mutationMode: ReportOnly
YAML
KUBECONFIG="${SCRATCH_DIR}/kubeconfig-impersonation.json" \
  kubectl --context researcher create -f "${SCRATCH_DIR}/quota-request-pending.yaml"

cat >"${SCRATCH_DIR}/quota-request-self-approved.yaml" <<YAML
apiVersion: tau.azure.com/v1alpha1
kind: TauQuotaRequest
metadata:
  name: ${WORKSPACE_NAME}-self-approved
  namespace: ${PLATFORM_NAMESPACE}
  annotations:
    tau.azure.com/approved: "true"
    tau.azure.com/reviewed-by: platform
spec:
  workspace: ${WORKSPACE_NAME}
  resource: h200
  requested: 1
  reason: kind-e2e-self-approval-must-fail
  mutationMode: ReportOnly
YAML
if KUBECONFIG="${SCRATCH_DIR}/kubeconfig-impersonation.json" \
  kubectl --context researcher create -f "${SCRATCH_DIR}/quota-request-self-approved.yaml" \
  >"${SCRATCH_DIR}/quota-self-approval.txt" 2>&1; then
  echo "researcher unexpectedly created a self-approved quota request" >&2
  exit 1
fi
grep -q "quota decision annotations are platform-owned" "${SCRATCH_DIR}/quota-self-approval.txt"

# ---------------------------------------------------------------------------
# Tau CLI exercise: workspace status, connection descriptor, run
# train/smoke/status/logs against the reconciled workspace. This is the
# ordinary researcher path. Every Tau invocation below uses the researcher
# impersonation context, so the test cannot pass through Kind-admin access.
# ---------------------------------------------------------------------------
cd "${TAU_DIR}"
KUBECONFIG="${SCRATCH_DIR}/kubeconfig-impersonation.json" \
  go run ./cmd/tau workspace status "${WORKSPACE_NAME}" --context researcher | tee "${SCRATCH_DIR}/tau-workspace-status.txt"
grep -q 'phase:      Ready' "${SCRATCH_DIR}/tau-workspace-status.txt"
grep -Eq 'WorkloadIdentityReady[[:space:]]+True' "${SCRATCH_DIR}/tau-workspace-status.txt"
if grep -q 'StorageReady' "${SCRATCH_DIR}/tau-workspace-status.txt"; then
  echo "tau workspace status unexpectedly rendered StorageReady" >&2
  exit 1
fi

tau_home="${SCRATCH_DIR}/rune-home"
mkdir -p "${tau_home}/home" "${tau_home}/tau" "${tau_home}/tau-config/connections" "${tau_home}/tau-config/kubeconfigs"
go build -o "${tau_home}/tau-bin" ./cmd/tau
cp "${SCRATCH_DIR}/kubeconfig-impersonation.json" "${tau_home}/tau-config/kubeconfigs/researcher.yaml"
chmod 600 "${tau_home}/tau-config/kubeconfigs/researcher.yaml"

cat >"${tau_home}/tau/workspace.connection.yaml" <<YAML
schema: tau.workspace.connection.v1
workspace: ${WORKSPACE_NAME}
cluster:
  provider: azure
  resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/kind/providers/Microsoft.ContainerService/managedClusters/${CLUSTER_NAME}
  contextName: researcher
identity:
  tenantID: 11111111-1111-1111-1111-111111111111
authorization:
  mode: workspace-rbac
  requiredRole: tau-researcher-v1
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
YAML

cat >"${tau_home}/train.sh" <<'SH'
#!/bin/sh
set -eu
echo rune-project-train-ok
SH
chmod +x "${tau_home}/train.sh"
cat >"${tau_home}/tau/train.yaml" <<'YAML'
name: project-train
engine: job
entrypoint: ../train.sh
runtime:
  image: mcr.microsoft.com/azurelinux/base/core:3.0
compute:
  cpu_request: 50m
  memory_request: 64Mi
  cpu_limit: 250m
  memory_limit: 128Mi
policy:
  profile: rune-cpu-train
  disable_default_priorities: true
YAML

descriptor_json="$(cd "${tau_home}" && "${tau_home}/tau-bin" workspace connection inspect -o json)"
descriptor_digest="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])' <<<"${descriptor_json}")"
connection_key="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["connectionKey"])' <<<"${descriptor_json}")"
cat >"${tau_home}/tau-config/connections/${connection_key}.json" <<JSON
{
  "schema": "tau.workspace.connection-state.v1",
  "workspace": "${WORKSPACE_NAME}",
  "resource_id": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/kind/providers/Microsoft.ContainerService/managedClusters/${CLUSTER_NAME}",
  "authorization_mode": "workspace-rbac",
  "context_name": "researcher",
  "kubeconfig_path": "${tau_home}/tau-config/kubeconfigs/researcher.yaml",
  "namespace": "${TARGET_NAMESPACE}",
  "queue": "${WORKSPACE_NAME}",
  "service_account": "tau-workload",
  "required_role": "tau-researcher-v1",
  "descriptor_path": "${tau_home}/tau/workspace.connection.yaml",
  "descriptor_digest": "${descriptor_digest}",
  "workspace_revision": "1",
  "verified_at": "2000-01-01T00:00:00Z",
  "repository_root": "${tau_home}",
  "private_cluster": false
}
JSON

# Force a real Kubernetes verifier pass against the Kind API before trusting
# the local state. The pseudo-TTY permits stale-state revalidation without
# fetching Azure credentials; the following smoke then proves the fresh
# non-interactive path.
if ! (
  cd "${tau_home}"
  script -q /dev/null env \
    HOME="${tau_home}/home" \
    TAU_CONFIG_DIR="${tau_home}/tau-config" \
    "${tau_home}/tau-bin" run train --dry-run=client
) >"${tau_home}/verified-train.yaml"; then
  cat "${tau_home}/verified-train.yaml" >&2
  exit 1
fi
grep -q "name: project-train" "${tau_home}/verified-train.yaml"

(
  cd "${tau_home}"
  HOME="${tau_home}/home" TAU_CONFIG_DIR="${tau_home}/tau-config" \
    "${tau_home}/tau-bin" run smoke >"${tau_home}/smoke.out" 2>"${tau_home}/smoke.err"
) &
SMOKE_PID=$!

deadline=$((SECONDS + WAIT_SECONDS))
smoke_job=""
while (( SECONDS < deadline )); do
  smoke_job="$(kubectl -n "${TARGET_NAMESPACE}" get jobs.batch -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep '^smoke-' | head -n1 || true)"
  [[ -n "${smoke_job}" ]] && break
  sleep 1
done
if [[ -z "${smoke_job}" ]]; then
  cat "${tau_home}/smoke.err" >&2 || true
  exit 1
fi
kubectl -n "${TARGET_NAMESPACE}" get job "${smoke_job}" -o jsonpath='{.spec.suspend}' | grep -qx true
kubectl -n "${TARGET_NAMESPACE}" get job "${smoke_job}" -o jsonpath='{.metadata.labels.kueue\.x-k8s\.io/queue-name}' | grep -qx "${WORKSPACE_NAME}"

# The Kind fixture installs Kueue CRDs only (no real Kueue controller, no
# GPU device plugin). Unsuspend by hand to model admission after proving
# Tau submitted through the designated LocalQueue -- this is a CPU-only Job,
# not a claim of real GPU scheduling or execution.
kubectl -n "${TARGET_NAMESPACE}" patch job "${smoke_job}" --type=merge -p '{"spec":{"suspend":false}}' >/dev/null
wait "${SMOKE_PID}"
grep -q "Run: ${smoke_job}" "${tau_home}/smoke.out"
grep -q "Workspace: ${WORKSPACE_NAME}" "${tau_home}/smoke.out"
grep -q "Phase: Succeeded" "${tau_home}/smoke.out"
grep -q "Tau onboarding complete." "${tau_home}/smoke.out"

(
  cd "${tau_home}"
  HOME="${tau_home}/home" TAU_CONFIG_DIR="${tau_home}/tau-config" \
    "${tau_home}/tau-bin" run train --dry-run=client
) >"${SCRATCH_DIR}/tau-run-train.yaml"
grep -q "namespace: ${TARGET_NAMESPACE}" "${SCRATCH_DIR}/tau-run-train.yaml"
grep -q "kueue.x-k8s.io/queue-name: ${WORKSPACE_NAME}" "${SCRATCH_DIR}/tau-run-train.yaml"
grep -q "name: project-train" "${SCRATCH_DIR}/tau-run-train.yaml"

(
  cd "${tau_home}"
  HOME="${tau_home}/home" TAU_CONFIG_DIR="${tau_home}/tau-config" \
    "${tau_home}/tau-bin" run status "${smoke_job}"
) >"${tau_home}/status.out"
grep -q "Job: ${TARGET_NAMESPACE}/${smoke_job}" "${tau_home}/status.out"
(
  cd "${tau_home}"
  HOME="${tau_home}/home" TAU_CONFIG_DIR="${tau_home}/tau-config" \
    "${tau_home}/tau-bin" run logs "${smoke_job}"
) >"${tau_home}/logs.out"
grep -q tau-onboarding-smoke-ok "${tau_home}/logs.out"

kubectl -n "${TARGET_NAMESPACE}" delete job "${smoke_job}" --wait=false >/dev/null

# ---------------------------------------------------------------------------
# Authorization migration: the same platform-owned desired state can explicitly
# switch to cluster-wide mode without granting system:authenticated or leaving
# stale subject-specific RBAC. Existing workload identity remains intact.
# ---------------------------------------------------------------------------
echo "== switching TauWorkspace to cluster-wide existing authorization =="
kubectl -n "${PLATFORM_NAMESPACE}" patch "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  --type=json -p='[
    {"op":"replace","path":"/spec/authorization/mode","value":"cluster-wide"},
    {"op":"remove","path":"/spec/principalRef"},
    {"op":"remove","path":"/spec/kubernetesSubject"},
    {"op":"remove","path":"/spec/role"}
  ]'

deadline=$((SECONDS + WAIT_SECONDS))
while (( SECONDS < deadline )); do
  reason="$(kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
    -o jsonpath='{.status.conditions[?(@.type=="RBACReady")].reason}' 2>/dev/null || true)"
  [[ "${reason}" == "ExistingClusterAuthorization" ]] && break
  sleep 1
done
kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  -o jsonpath='{.spec.authorization.mode}' | grep -qx cluster-wide
kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  -o jsonpath='{.status.conditions[?(@.type=="RBACReady")].reason}' | grep -qx ExistingClusterAuthorization
if kubectl -n "${TARGET_NAMESPACE}" get rolebinding tau-researcher-v1 >/dev/null 2>&1; then
  echo "cluster-wide authorization retained the researcher RoleBinding" >&2
  exit 1
fi
if kubectl -n "${PLATFORM_NAMESPACE}" get role "tau-workspace-reader-${WORKSPACE_NAME}" >/dev/null 2>&1 ||
   kubectl -n "${PLATFORM_NAMESPACE}" get rolebinding "tau-workspace-reader-${WORKSPACE_NAME}" >/dev/null 2>&1; then
  echo "cluster-wide authorization retained platform-reader RBAC" >&2
  exit 1
fi
kubectl -n "${TARGET_NAMESPACE}" get serviceaccount tau-workload >/dev/null

# ---------------------------------------------------------------------------
# Finalizer / cleanup semantics: remove only TauWorkspace desired state.
# Platform-owned Namespace/PVC remain; controller-owned LocalQueue,
# RBAC/ServiceAccount, and platform-reader access are removed by the finalizer.
# ---------------------------------------------------------------------------
echo "== delete TauWorkspace desired state and wait for finalizer =="
if ! kubectl -n "${PLATFORM_NAMESPACE}" delete \
  "workspaces.tau.azure.com/${WORKSPACE_NAME}" \
  --wait=true --timeout="${WAIT_SECONDS}s"; then
  kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" -o yaml >&2 || true
  kubectl -n "${PLATFORM_NAMESPACE}" logs deployment/tau-core-controller --tail=200 >&2 || true
  exit 1
fi

if kubectl -n "${PLATFORM_NAMESPACE}" get "workspaces.tau.azure.com/${WORKSPACE_NAME}" >/dev/null 2>&1; then
  echo "TauWorkspace ${WORKSPACE_NAME} still exists after desired-state deletion (finalizer stuck?)" >&2
  exit 1
fi

echo "== verifying platform-owned Namespace and PVC are retained =="
kubectl get namespace "${TARGET_NAMESPACE}" >/dev/null
kubectl -n "${TARGET_NAMESPACE}" get pvc blob-training >/dev/null

echo "== verifying controller-owned queue, RBAC, and ServiceAccount were cleaned up =="
if kubectl -n "${TARGET_NAMESPACE}" get localqueue "${WORKSPACE_NAME}" >/dev/null 2>&1; then
  echo "workspace LocalQueue ${WORKSPACE_NAME} was not cleaned up" >&2
  exit 1
fi
if kubectl -n "${TARGET_NAMESPACE}" get serviceaccount tau-workload >/dev/null 2>&1; then
  echo "target-namespace ServiceAccount tau-workload was not cleaned up" >&2
  exit 1
fi
if [[ -n "$(kubectl -n "${TARGET_NAMESPACE}" get rolebindings -l "tau.azure.com/workspace=${WORKSPACE_NAME}" --no-headers 2>/dev/null || true)" ]]; then
  echo "target-namespace RoleBindings labeled tau.azure.com/workspace=${WORKSPACE_NAME} were not cleaned up" >&2
  exit 1
fi
if kubectl -n "${PLATFORM_NAMESPACE}" get role "tau-workspace-reader-${WORKSPACE_NAME}" >/dev/null 2>&1; then
  echo "platform-namespace reader Role tau-workspace-reader-${WORKSPACE_NAME} was not cleaned up" >&2
  exit 1
fi
if kubectl -n "${PLATFORM_NAMESPACE}" get rolebinding "tau-workspace-reader-${WORKSPACE_NAME}" >/dev/null 2>&1; then
  echo "platform-namespace reader RoleBinding tau-workspace-reader-${WORKSPACE_NAME} was not cleaned up" >&2
  exit 1
fi

echo "== final guard: legacy rune.example.com API must never have been used in the live cluster =="
if kubectl api-resources 2>/dev/null | grep -q "rune\.example\.com"; then
  echo "the legacy rune.example.com API group is registered in the live cluster" >&2
  exit 1
fi

echo "TauWorkspace Kind e2e (native desired state + RBAC boundaries) passed"
