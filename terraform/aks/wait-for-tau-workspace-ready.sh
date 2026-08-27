#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

subscription_id="$1"
resource_group="$2"
cluster_name="$3"
kubeconfig="$4"
workspace_manifest="$5"
workspace_name="$6"
workspace_resource="workspace.tau.azure.com/${workspace_name}"

az aks get-credentials \
  --admin \
  --subscription "$subscription_id" \
  --resource-group "$resource_group" \
  --name "$cluster_name" \
  --file "$kubeconfig" \
  --overwrite-existing
kubectl apply --server-side --field-manager=taugrid-terraform -f "$workspace_manifest"

generation="$(kubectl get "$workspace_resource" --namespace tau-system --output=jsonpath='{.metadata.generation}')"
if [[ -z "$generation" ]]; then
  echo "Unable to read metadata.generation for $workspace_resource." >&2
  exit 1
fi

kubectl wait \
  "--for=jsonpath={.status.observedGeneration}=$generation" \
  "$workspace_resource" \
  --namespace tau-system \
  --timeout=10m
kubectl wait \
  --for=jsonpath='{.status.phase}'=Ready \
  "$workspace_resource" \
  --namespace tau-system \
  --timeout=10m
