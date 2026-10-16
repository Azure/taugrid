#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-gpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-gpu-quickstart}"
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-taugrid-default}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-taugrid-default}"
CHART="${TAU_QUICKSTART_CHART:-}"
CHART_ARGS=()
if [[ -n "$CHART" ]]; then
  CHART_ARGS=(--chart "$CHART")
fi
RUN_NAME="tau-aks-gpu-quickstart"
KEEP_CLUSTER="${TAU_QUICKSTART_KEEP_CLUSTER:-0}"

export KUBECONFIG="${KUBECONFIG:-/tmp/${CLUSTER}.kubeconfig}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@"; }
try()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@" || echo "  (already gone / not applicable)"; }

step "1. Cancel the workload"
try tau run cancel "$RUN_NAME" -n "$NAMESPACE" --context "$CLUSTER"

step "2. Delete the workspace"
try kubectl delete workspace.tau.azure.com "$WORKSPACE" -n tau-system --context "$CLUSTER"

step "3. Uninstall TauGrid"
try tau cluster uninstall --context "$CLUSTER" "${CHART_ARGS[@]}" --yes

if [ "$KEEP_CLUSTER" = "1" ]; then
  step "4. Keeping the cluster (TAU_QUICKSTART_KEEP_CLUSTER=1)"
  echo "WARNING: the A100 node pool continues to bill by the hour."
  echo "Delete it with: az group delete --name $RG --yes"
  exit 0
fi

step "4. Delete the resource group (stops all billing)"
OWNER_TAG="$(az group show --name "$RG" \
  --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null || true)"
if [ "$OWNER_TAG" = "aks-gpu-quickstart" ]; then
  run az group delete --name "$RG" --yes --no-wait
  echo "deletion started; confirm with: az group show --name $RG"
else
  echo "resource group '$RG' is not tagged as created by this quickstart."
  echo "Refusing to delete it."
  CLUSTER_TAG="$(az aks show --resource-group "$RG" --name "$CLUSTER" \
    --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null || true)"
  if [ "$CLUSTER_TAG" = "aks-gpu-quickstart" ]; then
    echo "Cluster '$CLUSTER' carries this quickstart's ownership tag; deleting it."
    try az aks delete --resource-group "$RG" --name "$CLUSTER" --yes --no-wait
  else
    echo "Cluster '$CLUSTER' is not tagged as created by this quickstart either."
    echo "Refusing to delete it. TauGrid has been uninstalled; nothing else was removed."
  fi
  echo
  echo "WARNING: any GPU node pool left running continues to bill by the hour."
  echo "If these resources are disposable, delete them yourself:"
  echo "  az aks delete --resource-group $RG --name $CLUSTER --yes"
  echo "  az group delete --name $RG --yes"
fi

rm -f "$KUBECONFIG"
echo "removed local kubeconfig $KUBECONFIG"
