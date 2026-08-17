#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# TauGrid A100 GPU quickstart — scripted run.
#
# This script is ONLY a sequencer for the commands documented in README.md.
# It contains no logic beyond ordering, existence checks, polling, and echoing
# what it is about to run. It invokes exactly three tools: az, tau, kubectl.
#
# It deliberately does NOT wrap, template, or generate any Kubernetes manifest,
# and it does not call helm/terraform/make/docker. `tau cluster install` shells
# out to helm internally; that is Tau's own prerequisite, not this script's.
#
# Creates EXPENSIVE billable Azure resources (an A100 node). Run ./cleanup.sh
# as soon as you have your evidence.
#
# Usage:  ./examples/aks-gpu-quickstart/run.sh
# Run from the repository root.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RG="${TAU_QUICKSTART_RG:-taugrid-gpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-gpu-quickstart}"
# A100 quota is region-specific and is usually the binding constraint here. See
# README.md > Choosing a region before changing this.
LOCATION="${TAU_QUICKSTART_LOCATION:-swedencentral}"
# CPU system pool. Standard_D4s_v5 is blocked by policy in some subscriptions;
# the non-"s" Standard_D4_v5 has the same 4 vCPU / 16 GiB shape.
NODE_SIZE="${TAU_QUICKSTART_NODE_SIZE:-Standard_D4_v5}"
NODE_COUNT="${TAU_QUICKSTART_NODE_COUNT:-2}"
# One A100 80GB per node, 24 vCPU. See README.md > Choosing a GPU SKU.
GPU_SIZE="${TAU_QUICKSTART_GPU_SIZE:-Standard_NC24ads_A100_v4}"
GPU_COUNT="${TAU_QUICKSTART_GPU_COUNT:-1}"
GPU_POOL="${TAU_QUICKSTART_GPU_POOL:-a100}"
# TauGrid v0 activates exactly one workspace per cluster and defaults its name,
# so this example does not choose one. Every stock install looks the same.
WORKSPACE="${TAU_QUICKSTART_WORKSPACE:-taugrid-default}"
NAMESPACE="${TAU_QUICKSTART_NAMESPACE:-taugrid-default}"
PRINCIPAL="${TAU_QUICKSTART_PRINCIPAL:-quickstart-researcher}"
RUN_NAME="tau-aks-gpu-quickstart"

export KUBECONFIG="${KUBECONFIG:-/tmp/${CLUSTER}.kubeconfig}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="${HERE}/tau.yaml"
CHART="${TAU_QUICKSTART_CHART:-}"
CHART_ARGS=()
if [[ -n "$CHART" ]]; then
  CHART_ARGS=(--chart "$CHART")
fi
# Host-diagnostic image for the MIG probe/repair below. Deliberately a fixed,
# digest-pinned MCR image rather than anything user-overridable: these pods run
# privileged as root in the node's host namespaces, so an override would be
# arbitrary root code execution on the node. AGENTS.md rule 18 requires this for
# privileged host-level workloads. base/core is minimal; tdnf adds util-linux
# for nsenter. Same pattern as
# tests/e2e/managedgpu/harness/fixtures/validate-gpu-job.yaml.
#
# The image the *workload* runs on is pinned in tau.yaml, not here.
DIAG_IMAGE="mcr.microsoft.com/azurelinux/base/core@sha256:4ecd6b297db85c54ec2df07145a28536c3655a3e98e54eb2364189bc4e6eac23"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
run()  { printf '\033[0;90m$ %s\033[0m\n' "$*"; "$@"; }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

for tool in az tau kubectl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done
command -v helm >/dev/null 2>&1 || {
  echo "missing helm: 'tau cluster install' shells out to helm internally." >&2
  echo "See README.md > Prerequisites. You do not invoke helm yourself." >&2
  exit 1
}

step "0. Static validation (no cluster required)"
run tau run --config "$CONFIG" --namespace "$NAMESPACE" --dry-run=client >/dev/null
echo "config renders a valid RayJob spec with an nvidia.com/gpu request"

step "0b. Confirm A100 quota in $LOCATION"
# Failing here costs seconds; failing after the cluster exists costs ~15 minutes
# and leaves a billable resource group behind.
#
# The quota family and per-node vCPU count are derived from $GPU_SIZE, not
# hardcoded: a Standard_NC48ads_A100_v4 override needs 48 vCPU per node from the
# same family, and a different family needs a different usage row entirely.
# This example only supports the NC-ads-A100-v4 family (the MIG and device-plugin
# handling below is A100-specific), so anything else is rejected here — before
# any billable resource is created — rather than silently passing the check.
case "$GPU_SIZE" in
  Standard_NC[0-9]*ads_A100_v4) ;;
  *)
    echo "  TAU_QUICKSTART_GPU_SIZE='$GPU_SIZE' is not in the NC-ads-A100-v4 family."
    echo "  This quickstart's MIG and device-plugin steps are A100-specific."
    echo "  See README.md > Choosing a GPU SKU."
    exit 1
    ;;
esac
GPU_VCPU_PER_NODE="$(printf '%s' "$GPU_SIZE" | sed -E 's/^Standard_NC([0-9]+)ads_A100_v4$/\1/')"
QUOTA_FAMILY="NCADS_A100_v4"
az vm list-usage --location "$LOCATION" -o tsv \
  --query "[].{n:localName,c:currentValue,l:limit}" \
  | awk -F'\t' -v need=$((GPU_COUNT * GPU_VCPU_PER_NODE)) -v fam="$QUOTA_FAMILY" -v loc="$LOCATION" '
      index($1, fam) {
        found = 1
        printf "  %s: %s/%s used, need %s more\n", $1, $2, $3, need
        if ($3 - $2 < need) { print "  INSUFFICIENT QUOTA — see README.md > Choosing a region"; exit 1 }
      }
      # Fail closed: no matching usage row means the family is unavailable in
      # this region, which is indistinguishable from zero quota. Without this
      # awk exits 0 and the script goes on to create the billable CPU cluster.
      END { if (!found) {
        printf "  no %s quota row in %s — the family is unavailable in this region\n", fam, loc
        print "  see README.md > Choosing a region"
        exit 1
      } }'

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
    --tags tau-quickstart-owned=aks-gpu-quickstart
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
    --nodepool-name system \
    --tags tau-quickstart-owned=aks-gpu-quickstart \
    --generate-ssh-keys
fi

step "2. Add the A100 node pool"
# Deliberately UNTAINTED. TauGrid's stock ResourceFlavor (charts/taugrid
# values.yaml) selects only kubernetes.io/os=linux and carries no tolerations,
# so a tainted GPU pool would never be admitted by Kueue without editing the
# chart — which would break the "every TauGrid install looks the same"
# invariant. AKS installs the NVIDIA *driver* automatically for GPU SKUs
# (gpuProfile.driver=Install), but NOT the device plugin — see step 2a.
if az aks nodepool show --resource-group "$RG" --cluster-name "$CLUSTER" \
     --name "$GPU_POOL" >/dev/null 2>&1; then
  echo "node pool $GPU_POOL already exists — skipping create"
else
  run az aks nodepool add \
    --resource-group "$RG" \
    --cluster-name "$CLUSTER" \
    --name "$GPU_POOL" \
    --node-vm-size "$GPU_SIZE" \
    --node-count "$GPU_COUNT"
fi

run az aks get-credentials \
  --resource-group "$RG" \
  --name "$CLUSTER" \
  --file "$KUBECONFIG" \
  --overwrite-existing

step "2a. Install the NVIDIA device plugin"
# AKS installs the driver but not the device plugin, so without this DaemonSet
# the node never advertises nvidia.com/gpu and every GPU pod stays Pending.
# The plugin needs privileged + hostPath /dev: with only NVIDIA_VISIBLE_DEVICES
# it logs "No devices found. Waiting indefinitely." and silently never registers.
run kubectl apply -f "$SCRIPT_DIR/nvidia-device-plugin.yaml"

step "2b. Wait for the GPU to be advertised"
# A GPU node goes Ready before the device plugin has registered the device, so
# Ready alone is not enough: submitting in that window leaves the pod Pending
# on "Insufficient nvidia.com/gpu" with no obvious cause.
for _ in $(seq 1 40); do
  gpus="$(kubectl get nodes -l agentpool="$GPU_POOL" \
    -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)"
  echo "  allocatable nvidia.com/gpu: ${gpus:-<none yet>}"
  [ -n "${gpus// /}" ] && break
  sleep 15
done
run kubectl get nodes -L agentpool -L kubernetes.azure.com/accelerator

step "2c. Disable MIG on the A100"
# Standard_NC24ads_A100_v4 comes up with MIG *enabled* and no MIG instances
# configured, even though `az aks nodepool show` reports gpuInstanceProfile:null.
# Signature: nvidia-smi -L lists the A100 (NVML works) but cuInit() returns 100
# CUDA_ERROR_NO_DEVICE, and torch reports device_count 1 with is_available False.
# nvidia-smi -mig 0 only sets a *pending* state; the driver applies it on reload,
# hence the VMSS restart. Both steps are skipped when MIG is already Disabled.
# Runs a command in the GPU node's host namespaces and echoes its stdout.
# $1 = pod name, $2 = command line to execute on the host.
#
# Requests NO nvidia.com/gpu. That is the whole point: the state this code
# exists to repair (MIG enabled with no MIG instances configured) is exactly
# the state in which the device plugin advertises zero allocatable GPUs. A pod
# that requested one would sit Pending forever, `kubectl logs` would return
# empty, and the caller would misread that as "MIG is Disabled" — silently
# skipping the repair and failing later with CUDA_ERROR_NO_DEVICE.
host_exec() {
  _pod="$1"; _cmd="$2"
  kubectl delete pod "$_pod" --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${_pod}
spec:
  restartPolicy: Never
  hostPID: true
  nodeSelector:
    accelerator: nvidia
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  - key: sku
    operator: Exists
    effect: NoSchedule
  containers:
  - name: probe
    image: ${DIAG_IMAGE}
    securityContext:
      privileged: true
      runAsUser: 0
    command: ["/bin/bash", "-c"]
    args:
    - |
      tdnf install -y --quiet util-linux >/dev/null 2>&1 \
        || tdnf install -y util-linux >/dev/null 2>&1
      nsenter -t 1 -m -u -i -n -p -- ${_cmd}
YAML
  for _ in $(seq 1 40); do
    case "$(kubectl get pod "$_pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
      Succeeded|Failed) break ;;
    esac
    sleep 10
  done
  kubectl logs "$_pod" 2>/dev/null
  kubectl delete pod "$_pod" --wait=false >/dev/null 2>&1 || true
}

# Echoes exactly one of: Enabled, Disabled, N/A, Unknown.
# Unknown is a probe failure, never "assume it is off".
mig_mode() {
  _out="$(host_exec tau-mig-probe \
    'nvidia-smi --query-gpu=mig.mode.current --format=csv,noheader' \
    | tr -d '[:space:]')"
  case "$_out" in
    Enabled)  printf 'Enabled'  ;;
    Disabled) printf 'Disabled' ;;
    N/A)      printf 'N/A'      ;;
    *)        printf 'Unknown'  ;;
  esac
}

MIG_STATE="$(mig_mode)"
echo "  MIG mode reported by the node: $MIG_STATE"
case "$MIG_STATE" in
  Unknown)
    die "MIG probe returned no readable state. Refusing to continue: if MIG is
in fact Enabled, training will fail later with CUDA_ERROR_NO_DEVICE and the
cause will be far from here. Investigate with:
  kubectl describe pod tau-mig-probe
  kubectl get nodes -l agentpool=$GPU_POOL -o wide"
    ;;
  Disabled|N/A)
    echo "  MIG already off ($MIG_STATE) — nothing to do"
    ;;
  Enabled)
    echo "  MIG is Enabled — disabling (CUDA cannot create a context while it is on)"
    host_exec tau-mig-off 'nvidia-smi -mig 0'

    echo "  restarting the node pool so the driver applies the pending MIG change"
    MC="$(az aks show --resource-group "$RG" --name "$CLUSTER" \
          --query nodeResourceGroup -o tsv)"
    VMSS="$(az vmss list --resource-group "$MC" \
            --query "[?starts_with(name,'aks-$GPU_POOL-')].name" -o tsv)"
    run az vmss restart --resource-group "$MC" --name "$VMSS"

    gpus=""
    for _ in $(seq 1 40); do
      gpus="$(kubectl get nodes -l agentpool="$GPU_POOL" \
        -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)"
      echo "  allocatable nvidia.com/gpu after restart: ${gpus:-<none yet>}"
      [ -n "${gpus// /}" ] && break
      sleep 20
    done
    [ -n "${gpus// /}" ] \
      || die "node pool restarted but no allocatable nvidia.com/gpu appeared."

    MIG_STATE="$(mig_mode)"
    echo "  MIG mode is now: $MIG_STATE"
    case "$MIG_STATE" in
      Disabled|N/A) : ;;
      *) die "MIG is still '$MIG_STATE' after the restart; training would fail." ;;
    esac
    ;;
esac

step "3. Install TauGrid"
# --wait=false is required on Helm 4: it extended its readiness wait to custom
# resources, and TauGrid's TauCluster CR sits at InProgress on a stock install,
# which is the normal steady state rather than a failure. With --atomic left on
# the timeout would also roll the release back. Readiness is not skipped, only
# moved to `tau cluster validate installation`, which is Tau's own gate.
#
# With no override, tau uses the public MCR chart and the version pinned in the
# installed CLI. TAU_QUICKSTART_CHART remains available for contributor tests.
run tau cluster install "${CHART_ARGS[@]}" --context "$CLUSTER" \
  --wait=false --atomic=false
run tau cluster validate installation --context "$CLUSTER"

step "4. Create the researcher workspace"
# NAME is optional, but pass WORKSPACE explicitly so an override creates the
# same workspace that the status checks below target.
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

step "5. Connectivity smoke test"
run tau run smoke --context "$CLUSTER"

step "6. Submit the A100 PyTorch workload"
run tau run --config "$CONFIG" --context "$CLUSTER"

step "7. Observe it"
run tau run status "$RUN_NAME" -n "$NAMESPACE" --context "$CLUSTER"
cat <<TXT

Follow the proof as it runs:
  tau run logs $RUN_NAME -n $NAMESPACE --context $CLUSTER -f

Expect, in order:
  [gate 1] device: NVIDIA A100 80GB PCIe, capability 8.0, ~79 GiB VRAM
  [gate 2] throughput: 150-300 TFLOP/s TF32 (floor is 20)
  [train]  loss falling over 400 steps
  === TAU-GPU-EVIDENCE-BEGIN === ... the full JSON verdict

Extract just the evidence block:
  tau run logs $RUN_NAME -n $NAMESPACE --context $CLUSTER \\
    | sed -n '/TAU-GPU-EVIDENCE-BEGIN/,/TAU-GPU-EVIDENCE-END/p'

An A100 node bills by the hour. When you are done:  ${HERE}/cleanup.sh
TXT
