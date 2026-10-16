#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-cpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-cpu-quickstart}"
LOCATION="${TAU_QUICKSTART_LOCATION:-eastus2}"
NODE_COUNT="${TAU_QUICKSTART_NODE_COUNT:-3}"
NODE_SIZE="${TAU_QUICKSTART_NODE_SIZE:-Standard_D4_v5}"
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-taugrid-default}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-taugrid-default}"
PRINCIPAL="${TAU_QUICKSTART_PRINCIPAL:-quickstart-researcher}"
RUN_NAME="tau-aks-cpu-quickstart"

export KUBECONFIG="${KUBECONFIG:-/tmp/${CLUSTER}.kubeconfig}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="${HERE}/tau.yaml"
STELLAR_CONFIG="${HERE}/stellar-demo/tau.yaml"
CHART="${TAU_QUICKSTART_CHART:-}"
CHART_ARGS=()
if [[ -n "$CHART" ]]; then
  CHART_ARGS=(--chart "$CHART")
fi

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@"; }

for tool in az tau kubectl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
command -v helm >/dev/null 2>&1 || {
  echo "missing helm: 'tau cluster install' shells out to helm internally." >&2
  echo "See README.md > Prerequisites. You do not invoke helm yourself." >&2
  exit 1
}
if [ -x "${HELM3_BIN:-}" ]; then
  PATH="$(dirname "$HELM3_BIN"):$PATH"; export PATH
  echo "using pinned helm $(helm version --short) (HELM3_BIN set)"
else
  echo "using helm $(helm version --short 2>/dev/null || echo unknown)"
fi

step "0. Static validation (no cluster required)"
run tau run --config "$CONFIG"         --namespace "$NAMESPACE" --dry-run=client >/dev/null
run tau run --config "$STELLAR_CONFIG" --namespace "$NAMESPACE" --dry-run=client >/dev/null
echo "both configs render a valid RayJob spec"

step "1. Resource group + CPU-only AKS cluster"
if az group show --name "$RG" >/dev/null 2>&1; then
  echo "resource group $RG already exists — skipping create"
  echo "  NOTE: not tagging a pre-existing group. cleanup.sh will therefore"
  echo "  delete only the cluster, not the group."
else
  run az group create --name "$RG" --location "$LOCATION" \
    --tags tau-quickstart-owned=aks-cpu-quickstart
fi

if az aks show --resource-group "$RG" --name "$CLUSTER" >/dev/null 2>&1; then
  echo "cluster $CLUSTER already exists — skipping create"
else
  run az aks create \
    --resource-group "$RG" \
    --name "$CLUSTER" \
    --location "$LOCATION" \
    --node-count "$NODE_COUNT" \
    --node-vm-size "$NODE_SIZE" \
    --tags tau-quickstart-owned=aks-cpu-quickstart \
    --generate-ssh-keys
fi

run az aks get-credentials \
  --resource-group "$RG" \
  --name "$CLUSTER" \
  --file "$KUBECONFIG" \
  --overwrite-existing

step "2. Install TauGrid"
run tau cluster install "${CHART_ARGS[@]}" --context "$CLUSTER" \
  --wait=false --atomic=false
run tau cluster validate installation --context "$CLUSTER"

step "3. Create the researcher workspace"
run tau workspace create "$WORKSPACE" \
  --principal-name "$PRINCIPAL" \
  --context "$CLUSTER" \
  --apply

for _ in $(seq 1 30); do
  phase="$(tau workspace status "$WORKSPACE" --context "$CLUSTER" 2>/dev/null \
    | awk '/^ *[Pp]hase/ {print $NF; exit}')"
  echo "  workspace phase: ${phase:-<pending>}"
  [ "$phase" = "Ready" ] && break
  sleep 10
done
run tau workspace status "$WORKSPACE" --context "$CLUSTER"

step "4. Submit the PyTorch workload"
run tau run --config "$CONFIG" --context "$CLUSTER"

step "5. Observe it"
run tau run status "$RUN_NAME" -n "$NAMESPACE" --context "$CLUSTER"
echo
echo "Follow the training logs with:"
echo "  tau run logs $RUN_NAME -n $NAMESPACE --context $CLUSTER -f"
echo
echo "Expect loss to fall from ~3.7 to ~0.3-0.7 across 4 DDP workers."
echo "When you are done:  ${HERE}/cleanup.sh"
