#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly RESOURCE_GROUP="${TAU_EXTENSIONS_RG:?set TAU_EXTENSIONS_RG to the AKS resource group}"
readonly CLUSTER_NAME="${TAU_EXTENSIONS_CLUSTER:?set TAU_EXTENSIONS_CLUSTER to the AKS cluster name}"
readonly KUEUE_EXTENSION_NAME="${TAU_KUEUE_EXTENSION_NAME:-aks-kueue}"
readonly KUBERAY_EXTENSION_NAME="${TAU_KUBERAY_EXTENSION_NAME:-kuberay}"
readonly CHART="${TAU_EXTENSIONS_CHART:-}"

export KUBECONFIG="${KUBECONFIG:-/tmp/${CLUSTER_NAME}-extensions.kubeconfig}"

for tool in az kubectl helm tau; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

cluster_version="$(
  az aks show \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --query kubernetesVersion \
    --output tsv
)"
IFS=. read -r cluster_major cluster_minor _ <<<"$cluster_version"
if (( cluster_major < 1 || (cluster_major == 1 && cluster_minor < 34) )); then
  echo "managed Kueue and KubeRay require AKS 1.34 or later; found ${cluster_version}" >&2
  exit 1
fi

az extension add --name k8s-extension --upgrade
az provider register --namespace Microsoft.ContainerService --wait
az provider register --namespace Microsoft.KubernetesConfiguration --wait
az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --file "$KUBECONFIG" \
  --overwrite-existing

KUBE_CONTEXT="$(kubectl config current-context)"
readonly KUBE_CONTEXT

extension_exists() {
  az k8s-extension show \
    --resource-group "$RESOURCE_GROUP" \
    --cluster-name "$CLUSTER_NAME" \
    --cluster-type managedClusters \
    --name "$1" \
    >/dev/null 2>&1
}

deployment_exists() {
  local name="$1"
  local deployments
  local deployment

  deployments="$(
    kubectl get deployments --all-namespaces \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
  )"
  while IFS= read -r deployment; do
    [[ "$deployment" == "$name" ]] && return 0
  done <<<"$deployments"
  return 1
}

if ! extension_exists "$KUEUE_EXTENSION_NAME" && deployment_exists kueue-controller-manager; then
  echo "self-managed Kueue is already installed; remove it through its owning installer before enabling the managed extension" >&2
  exit 1
fi
if ! extension_exists "$KUBERAY_EXTENSION_NAME" && deployment_exists kuberay-operator; then
  echo "self-managed KubeRay is already installed; remove it through its owning installer before enabling the managed extension" >&2
  exit 1
fi

if ! extension_exists "$KUEUE_EXTENSION_NAME"; then
  az k8s-extension create \
    --resource-group "$RESOURCE_GROUP" \
    --cluster-name "$CLUSTER_NAME" \
    --cluster-type managedClusters \
    --name "$KUEUE_EXTENSION_NAME" \
    --extension-type microsoft.kueue \
    --scope cluster \
    --release-train preview \
    --auto-upgrade-minor-version true
fi

if ! extension_exists "$KUBERAY_EXTENSION_NAME"; then
  az k8s-extension create \
    --resource-group "$RESOURCE_GROUP" \
    --cluster-name "$CLUSTER_NAME" \
    --cluster-type managedClusters \
    --name "$KUBERAY_EXTENSION_NAME" \
    --extension-type microsoft.kuberay \
    --scope cluster \
    --release-namespace kuberay-system \
    --release-train preview \
    --auto-upgrade-minor-version true
fi

wait_for_extension() {
  local name="$1"
  local state
  local extension_state
  for _ in $(seq 1 60); do
    state="$(
      az k8s-extension show \
        --resource-group "$RESOURCE_GROUP" \
        --cluster-name "$CLUSTER_NAME" \
        --cluster-type managedClusters \
        --name "$name" \
        --query provisioningState \
        --output tsv
    )"
    extension_state="$(
      az k8s-extension show \
        --resource-group "$RESOURCE_GROUP" \
        --cluster-name "$CLUSTER_NAME" \
        --cluster-type managedClusters \
        --name "$name" \
        --query extensionState \
        --output tsv
    )"
    printf '%s: provisioning=%s extension=%s\n' "$name" "$state" "$extension_state"
    if [[ "$state" == "Succeeded" && ( "$extension_state" == "Available" || "$extension_state" == "Ready" ) ]]; then
      return
    fi
    if [[ "$state" == "Failed" ]]; then
      echo "extension $name failed to provision" >&2
      exit 1
    fi
    sleep 10
  done
  echo "extension $name did not become ready within 10 minutes" >&2
  exit 1
}

wait_for_labelled_deployment() {
  local selector="$1"
  local component="$2"
  local reference
  local namespace
  local deployment

  reference="$(
    kubectl get deployment --all-namespaces \
      --selector "$selector" \
      -o jsonpath='{.items[0].metadata.namespace}/{.items[0].metadata.name}'
  )"
  if [[ -z "$reference" || "$reference" == "/" ]]; then
    echo "could not find the managed ${component} deployment with selector ${selector}" >&2
    exit 1
  fi
  namespace="${reference%%/*}"
  deployment="${reference#*/}"
  kubectl wait \
    --for=condition=Available \
    "deployment/${deployment}" \
    --namespace "$namespace" \
    --timeout=5m
}

wait_for_extension "$KUEUE_EXTENSION_NAME"
wait_for_extension "$KUBERAY_EXTENSION_NAME"
wait_for_labelled_deployment "app.kubernetes.io/name=kueue" Kueue
wait_for_labelled_deployment "app.kubernetes.io/name=kuberay-operator" KubeRay

kubectl get crd \
  clusterqueues.kueue.x-k8s.io \
  localqueues.kueue.x-k8s.io \
  resourceflavors.kueue.x-k8s.io \
  workloads.kueue.x-k8s.io \
  rayclusters.ray.io \
  raycronjobs.ray.io \
  rayjobs.ray.io \
  rayservices.ray.io \
  >/dev/null

install_args=(
  cluster install
  --context "$KUBE_CONTEXT"
  --kueue-mode external
  --set components.kuberayOperator.enabled=false
  --wait=false
  --atomic=false
)
if [[ -n "$CHART" ]]; then
  install_args+=(--chart "$CHART")
fi
tau "${install_args[@]}"
tau cluster validate installation --context "$KUBE_CONTEXT"
