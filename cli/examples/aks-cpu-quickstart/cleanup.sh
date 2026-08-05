#!/usr/bin/env bash
#
# TauGrid CPU quickstart — scripted teardown.
#
# Sequencer only, matching README.md > Cleanup. Invokes exactly three tools:
# az, tau, kubectl. Tolerates already-deleted resources so it is safe to re-run.
#
# Usage:  ./cli/examples/aks-cpu-quickstart/cleanup.sh
#         ./cli/examples/aks-cpu-quickstart/cleanup.sh --yes   # skip the prompt

set -uo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-cpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-cpu-quickstart}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-research}"
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-research}"
PLATFORM_NS="${TAU_QUICKSTART_PLATFORM_NS:-tau-platform}"
# Same default as run.sh: uninstall re-renders the release to drain the queue
# policy, so it needs the chart the release was installed from.
CHART="${TAU_QUICKSTART_CHART:-./charts/taugrid}"

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

# `tau run cancel` can report `signal: killed` from an internal Kueue wait step
# after it has already deleted the RayJob, so verify the real state directly.
step "Verify no RayJobs remain"
run kubectl get rayjob -n "$NAMESPACE"

step "2. Delete the TauWorkspace (lives in $PLATFORM_NS, not $NAMESPACE)"
run kubectl delete workspace.tau.azure.com "${WORKSPACE}" -n "$PLATFORM_NS" --ignore-not-found

step "3. Uninstall TauGrid"
# --yes is required once TauWorkspace objects have existed on the cluster.
# Uninstall drains the queue policy while Kueue still runs, so its finalizers
# are released before the controller goes away. See README > Cleanup.
run tau cluster uninstall --context "$CLUSTER" --chart "$CHART" --yes

step "4. Delete the resource group (this is what stops the billing)"
# Guarded: only groups this quickstart created carry the ownership tag. If the
# tag is absent, the group pre-existed (run.sh reuses an existing group), so
# deleting it could destroy unrelated resources. In that case tear down only
# the cluster this example created.
OWNER_TAG="$(az group show --name "$RG" \
  --query "tags.\"tau-quickstart-owned\"" -o tsv 2>/dev/null)"
if [[ "$OWNER_TAG" == "aks-cpu-quickstart" ]]; then
  run az group delete --name "$RG" --yes --no-wait
  echo "Deletion of $RG is asynchronous. Confirm it finished with:"
  echo "  az group show --name $RG      # expect: ResourceGroupNotFound"
else
  echo "  resource group '$RG' is not tagged as created by this quickstart."
  echo "  Refusing to delete it."
  # The group pre-existed, but the cluster inside it may still be ours.
  # run.sh reuses a same-name cluster too, so ownership has to be recorded and
  # checked independently of the group. Fail closed when the tag is absent.
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
