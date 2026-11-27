#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly RESOURCE_GROUP="${TAU_EXTENSIONS_RG:?set TAU_EXTENSIONS_RG to the AKS resource group}"
readonly CLUSTER_NAME="${TAU_EXTENSIONS_CLUSTER:?set TAU_EXTENSIONS_CLUSTER to the AKS cluster name}"
readonly KUEUE_EXTENSION_NAME="aks-kueue"
readonly KUBERAY_EXTENSION_NAME="kuberay"
readonly KUBECONFIG_PATH="${KUBECONFIG:-/tmp/${CLUSTER_NAME}-taugrid.kubeconfig}"

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
if ((cluster_major < 1 || (cluster_major == 1 && cluster_minor < 34))); then
  echo "managed Kueue and KubeRay require AKS 1.34 or later; found ${cluster_version}" >&2
  exit 1
fi

az extension add --name k8s-extension --upgrade
az provider register --namespace Microsoft.KubernetesConfiguration --wait
az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --file "$KUBECONFIG_PATH" \
  --overwrite-existing

export KUBECONFIG="$KUBECONFIG_PATH"
TAU_CONTEXT="$(kubectl config current-context)"
readonly TAU_CONTEXT

extension_exists() {
  az k8s-extension show \
    --resource-group "$RESOURCE_GROUP" \
    --cluster-name "$CLUSTER_NAME" \
    --cluster-type managedClusters \
    --name "$1" \
    >/dev/null 2>&1
}

controller_exists() {
  kubectl get deployments \
    --all-namespaces \
    --selector "app.kubernetes.io/name=$1" \
    --output name |
    grep -q .
}

if ! extension_exists "$KUEUE_EXTENSION_NAME" && controller_exists kueue; then
  echo "a Kueue controller already exists; remove it through its current owner before provisioning the managed extension" >&2
  exit 1
fi
if ! extension_exists "$KUBERAY_EXTENSION_NAME" && controller_exists kuberay-operator; then
  echo "a KubeRay controller already exists; remove it through its current owner before provisioning the managed extension" >&2
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
  local provisioning_state
  local extension_state
  for _ in $(seq 1 60); do
    provisioning_state="$(
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
    if [[ "$provisioning_state" == "Succeeded" && ("$extension_state" == "Available" || "$extension_state" == "Ready") ]]; then
      return
    fi
    if [[ "$provisioning_state" == "Failed" ]]; then
      echo "extension $name failed to provision" >&2
      exit 1
    fi
    sleep 10
  done
  echo "extension $name did not become ready within 10 minutes" >&2
  exit 1
}

wait_for_extension "$KUEUE_EXTENSION_NAME"
wait_for_extension "$KUBERAY_EXTENSION_NAME"

wait_for_controller() {
  local app_name="$1"
  local reference
  local namespace
  local deployment
  reference="$(
    kubectl get deployments \
      --all-namespaces \
      --selector "app.kubernetes.io/name=${app_name}" \
      --output jsonpath='{.items[0].metadata.namespace}/{.items[0].metadata.name}'
  )"
  if [[ -z "$reference" || "$reference" == "/" ]]; then
    echo "managed ${app_name} controller Deployment was not found" >&2
    exit 1
  fi
  namespace="${reference%%/*}"
  deployment="${reference#*/}"
  kubectl wait \
    --namespace "$namespace" \
    --for=condition=Available \
    "deployment/${deployment}" \
    --timeout=5m
}

wait_for_controller kueue
wait_for_controller kuberay-operator

tau cluster install --mode managed --context "$TAU_CONTEXT"
tau cluster validate installation --context "$TAU_CONTEXT"

