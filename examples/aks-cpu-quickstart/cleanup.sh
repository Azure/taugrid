#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -uo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-cpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-cpu-quickstart}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-research}"
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-research}"
SYSTEM_NS="${TAU_QUICKSTART_SYSTEM_NS:-tau-system}"
CHART="${TAU_QUICKSTART_CHART:-}"
CHART_ARGS=()
if [[ -n "$CHART" ]]; then
  CHART_ARGS=(--chart "$CHART")
fi

export KUBECONFIG="${KUBECONFIG:-/tmp/${CLUSTER}.kubeconfig}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@" || echo "  (non-fatal: continuing teardown)"; }

if [[ "${1:-}" != "--yes" ]]; then
  echo "This deletes the AKS cluster '$CLUSTER'."
  echo "The resource group '$RG' is deleted too, but only if this quickstart created it."
  read -r -p "Type 'delete' to continue: " confirm
  [[ "$confirm" == "delete" ]] || { echo "aborted"; exit 1; }
fi

step "1. Cancel workloads"
for job in tau-aks-cpu-quickstart aks-cpu-quickstart-stellar-demo; do
  run tau run cancel "$job" -n "$NAMESPACE" --context "$CLUSTER" --teardown-timeout 3m
done

step "Verify no RayJobs remain"
run kubectl get rayjob -n "$NAMESPACE"

step "2. Delete the TauWorkspace (lives in $SYSTEM_NS, not $NAMESPACE)"
run kubectl delete workspace.tau.azure.com "${WORKSPACE}" -n "$SYSTEM_NS" --ignore-not-found

step "3. Uninstall TauGrid"
run tau cluster uninstall --context "$CLUSTER" "${CHART_ARGS[@]}" --yes

step "4. Delete the resource group (this is what stops the billing)"
OWNER_TAG="$(az group show --name "$RG" \
  --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null)"
if [[ "$OWNER_TAG" == "aks-cpu-quickstart" ]]; then
  run az group delete --name "$RG" --yes --no-wait
  echo "Deletion of $RG is asynchronous. Confirm it finished with:"
  echo "  az group show --name $RG      # expect: ResourceGroupNotFound"
else
  echo "  resource group '$RG' is not tagged as created by this quickstart."
  echo "  Refusing to delete it."
  CLUSTER_TAG="$(az aks show --resource-group "$RG" --name "$CLUSTER" \
    --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null || true)"
  if [[ "$CLUSTER_TAG" == "aks-cpu-quickstart" ]]; then
    echo "  Cluster '$CLUSTER' carries this quickstart's ownership tag; deleting it."
    run az aks delete --resource-group "$RG" --name "$CLUSTER" --yes --no-wait
  else
    echo "  Cluster '$CLUSTER' is not tagged as created by this quickstart either."
    echo "  Refusing to delete it. TauGrid has been uninstalled; nothing else was removed."
  fi
  echo
  echo "If you are certain these resources are disposable, delete them yourself:"
  echo "  az aks delete --resource-group $RG --name $CLUSTER --yes"
  echo "  az group delete --name $RG --yes"
fi

step "5. Remove the local kubeconfig"
run rm -f "$KUBECONFIG"
