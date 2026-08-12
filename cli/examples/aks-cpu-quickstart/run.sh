#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# TauGrid CPU quickstart — scripted run.
#
# This script is ONLY a sequencer for the commands documented in README.md.
# It contains no logic beyond ordering, existence checks, and echoing what it
# is about to run. It invokes exactly three tools: az, tau, kubectl.
#
# It deliberately does NOT wrap, template, or generate any Kubernetes manifest,
# and it does not call helm/terraform/make/docker. `tau cluster install` shells
# out to helm internally; that is Tau's own prerequisite, not this script's.
#
# Creates billable Azure resources. Run ./cleanup.sh when you are done.
#
# Usage:  ./cli/examples/aks-cpu-quickstart/run.sh
# Run from the repository root.

set -euo pipefail

RG="${TAU_QUICKSTART_RG:-taugrid-cpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-cpu-quickstart}"
LOCATION="${TAU_QUICKSTART_LOCATION:-eastus2}"
NODE_COUNT="${TAU_QUICKSTART_NODE_COUNT:-3}"
# Standard_D4s_v5 is blocked by policy in some subscriptions/regions; the
# non-"s" Standard_D4_v5 has the same 4 vCPU / 16 GiB shape and is broadly
# allowed. If `az aks create` rejects this, the error lists the permitted sizes.
NODE_SIZE="${TAU_QUICKSTART_NODE_SIZE:-Standard_D4_v5}"
# TauGrid v0 activates exactly one workspace per cluster and now defaults its
# name, so this example does not choose one. Every stock install looks the same.
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-taugrid-default}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-taugrid-default}"
# Subject the workspace RBAC is bound to. Any principal name works for a
# single-operator quickstart; use the real Entra identity on a shared cluster.
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
# Older `tau` builds called `helm list --all`, a flag Helm 4 removed, so they
# failed with "unknown flag: --all". Current `tau` passes the individual state
# flags and works on both Helm 3 and Helm 4, so no version pin is needed.
# HELM3_BIN remains an opt-in escape hatch for running this example against a
# pre-fix `tau` build; it is never required and never fatal.
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
# The ownership tag is what makes teardown safe. cleanup.sh deletes the whole
# resource group only when it finds this tag, so pointing TAU_QUICKSTART_RG at a
# pre-existing shared group can never cause cleanup.sh to delete resources this
# quickstart did not create.
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
# --wait=false is required on Helm 4. Helm 4 extended its readiness wait to
# custom resources, and TauGrid's TauCluster CR never reports Ready on a stock
# install: it sits at InProgress with "queue and storage observers are not
# enabled", which is the normal steady state, not a failure. Helm 3 only waited
# on built-in kinds, so this did not surface there. With --atomic left on, the
# 15m timeout also rolls the whole release back.
#
# Readiness is not skipped, only moved: `tau cluster validate installation`
# below is Tau's own readiness check and is the authoritative gate.
#
# With no override, tau uses the public MCR chart and the version pinned in the
# installed CLI. TAU_QUICKSTART_CHART remains available for contributor tests.
run tau cluster install "${CHART_ARGS[@]}" --context "$CLUSTER" \
  --wait=false --atomic=false
run tau cluster validate installation --context "$CLUSTER"

step "3. Create the researcher workspace"
# NAME is optional, but pass WORKSPACE explicitly so an override creates the
# same workspace that the status checks below target.
run tau workspace create "$WORKSPACE" \
  --principal-name "$PRINCIPAL" \
  --context "$CLUSTER" \
  --apply

# Poll until the tau-core-controller reports phase=Ready. `tau workspace status`
# is a reporting verb with no wait/--check flag, so the loop lives here.
for _ in $(seq 1 30); do
  phase="$(tau workspace status "$WORKSPACE" --context "$CLUSTER" 2>/dev/null \
    | awk '/^ *[Pp]hase/ {print $NF; exit}')"
  echo "  workspace phase: ${phase:-<pending>}"
  [ "$phase" = "Ready" ] && break
  sleep 10
done
run tau workspace status "$WORKSPACE" --context "$CLUSTER"

step "4. Connectivity smoke test"
# No --workspace: tau resolves the cluster's single workspace itself.
run tau run smoke --context "$CLUSTER"

step "5. Submit the real PyTorch workload"
# Also no --workspace. A researcher never names it.
run tau run --config "$CONFIG" --context "$CLUSTER"

step "6. Observe it"
run tau run status "$RUN_NAME" -n "$NAMESPACE" --context "$CLUSTER"
echo
echo "Follow the training logs with:"
echo "  tau run logs $RUN_NAME -n $NAMESPACE --context $CLUSTER -f"
echo
echo "Expect loss to fall from ~3.7 to ~0.3-0.7 across 4 DDP workers."
echo "When you are done:  ${HERE}/cleanup.sh"
