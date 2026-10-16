#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RG="${TAU_QUICKSTART_RG:-taugrid-gpu-quickstart-rg}"
CLUSTER="${TAU_QUICKSTART_CLUSTER:-tau-gpu-quickstart}"
LOCATION="${TAU_QUICKSTART_LOCATION:-swedencentral}"
NODE_SIZE="${TAU_QUICKSTART_NODE_SIZE:-Standard_D4_v5}"
NODE_COUNT="${TAU_QUICKSTART_NODE_COUNT:-2}"
GPU_SIZE="${TAU_QUICKSTART_GPU_SIZE:-Standard_NC24ads_A100_v4}"
GPU_COUNT="${TAU_QUICKSTART_GPU_COUNT:-1}"
GPU_POOL="${TAU_QUICKSTART_GPU_POOL:-a100}"
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
      END { if (!found) {
        printf "  no %s quota row in %s — the family is unavailable in this region\n", fam, loc
        print "  see README.md > Choosing a region"
        exit 1
      } }'

step "1. Resource group + CPU-only AKS cluster"
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
run kubectl apply -f "$SCRIPT_DIR/nvidia-device-plugin.yaml"

step "2b. Wait for the GPU to be advertised"
for _ in $(seq 1 40); do
  gpus="$(kubectl get nodes -l agentpool="$GPU_POOL" \
    -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)"
  echo "  allocatable nvidia.com/gpu: ${gpus:-<none yet>}"
  [ -n "${gpus// /}" ] && break
  sleep 15
done
run kubectl get nodes -L agentpool -L kubernetes.azure.com/accelerator

step "2c. Disable MIG on the A100"
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
run tau cluster install "${CHART_ARGS[@]}" --context "$CLUSTER" \
  --wait=false --atomic=false
run tau cluster validate installation --context "$CLUSTER"

step "4. Create the researcher workspace"
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

step "5. Submit the A100 PyTorch workload"
run tau run --config "$CONFIG" --context "$CLUSTER"

step "6. Observe it"
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
