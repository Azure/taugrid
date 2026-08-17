#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# TauGrid A100 GPU quickstart — teardown.
#
# Tears down in the order README.md documents: workload, workspace, TauGrid,
# then the whole resource group. Uses only az, tau, and kubectl.
#
# The resource group delete is what actually stops the billing, and it is the
# only step that matters if you are in a hurry — everything above it lives
# inside that group. The earlier steps exist so the example demonstrates the
# supported per-layer teardown path, and so that re-running run.sh against a
# kept cluster starts from a clean slate.

set -euo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-gpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-gpu-quickstart}"
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-taugrid-default}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-taugrid-default}"
# Empty uses Tau's public MCR default. Keep this aligned with run.sh when a
# contributor explicitly overrides the chart.
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
# Teardown must be re-runnable after a partial failure, so a missing object is
# never fatal here.
try()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@" || echo "  (already gone / not applicable)"; }

step "1. Cancel the workload"
try tau run cancel "$RUN_NAME" -n "$NAMESPACE" --context "$CLUSTER"

step "2. Delete the workspace"
# There is no `tau workspace delete` verb; kubectl is the documented path.
try kubectl delete workspace.tau.azure.com "$WORKSPACE" -n tau-platform --context "$CLUSTER"

step "3. Uninstall TauGrid"
# Uninstall drains the queue policy while Kueue still runs, so its finalizers
# are released rather than stranded. Still `try`: the install-side half of
# upstream issue #1288 is unfixed, and the resource group delete supersedes it.
try tau cluster uninstall --context "$CLUSTER" "${CHART_ARGS[@]}" --yes

if [ "$KEEP_CLUSTER" = "1" ]; then
  step "4. Keeping the cluster (TAU_QUICKSTART_KEEP_CLUSTER=1)"
  echo "WARNING: the A100 node pool continues to bill by the hour."
  echo "Delete it with: az group delete --name $RG --yes"
  exit 0
fi

step "4. Delete the resource group (stops all billing)"
# Guarded: only groups this quickstart created carry the ownership tag. If the
# tag is absent, the group pre-existed (run.sh reuses an existing group), so
# deleting it could destroy unrelated resources. In that case tear down only
# the cluster this example created.
OWNER_TAG="$(az group show --name "$RG" \
  --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null || true)"
if [ "$OWNER_TAG" = "aks-gpu-quickstart" ]; then
  run az group delete --name "$RG" --yes --no-wait
  echo "deletion started; confirm with: az group show --name $RG"
else
  echo "resource group '$RG' is not tagged as created by this quickstart."
  echo "Refusing to delete it."
  # The group pre-existed, but the cluster inside it may still be ours.
  # run.sh reuses a same-name cluster too, so ownership has to be recorded and
  # checked independently of the group. Fail closed when the tag is absent.
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
