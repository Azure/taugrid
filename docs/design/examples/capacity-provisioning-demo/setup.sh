#!/usr/bin/env bash
# Creates a kind cluster with the real Kueue + ProvisioningRequest CRD, KWOK for simulated
# GPU nodes, and the demo queues. Then builds the tau-demo binary.
set -euo pipefail
cd "$(dirname "$0")"

CLUSTER=${CLUSTER:-tau278}
KUEUE_VERSION=${KUEUE_VERSION:-v0.19.2}        # what charts/taugrid ships
KWOK_VERSION=${KWOK_VERSION:-v0.8.0}
CA_TAG=${CA_TAG:-cluster-autoscaler-1.35.2}    # source of the ProvisioningRequest CRD
KUEUE_IMAGE=${KUEUE_IMAGE:-mcr.microsoft.com/oss/v2/kueue/kueue:$KUEUE_VERSION}   # same image charts/taugrid uses
KWOK_IMAGE=${KWOK_IMAGE:-registry.k8s.io/kwok/kwok:$KWOK_VERSION}

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

for bin in docker kind kubectl go; do
  command -v "$bin" >/dev/null || { echo "missing: $bin"; exit 1; }
done

step "kind cluster $CLUSTER"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  # KUBE_PROXY_MODE=nftables helps on kernels without the iptables "statistic" module
  # (kube-proxy logs "RULE_APPEND failed" and Services, including Kueue's webhook, time out).
  kind create cluster --name "$CLUSTER" --wait 120s --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  kubeProxyMode: ${KUBE_PROXY_MODE:-iptables}
EOF
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

step "Preload images into kind (works behind proxies and on networks that block registry.k8s.io from the node)"
for img in "$KUEUE_IMAGE" "$KWOK_IMAGE"; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img"
  if ! kind load docker-image "$img" --name "$CLUSTER" >/dev/null 2>&1; then
    # kind's --all-platforms import fails on multi-arch images with Docker's containerd store.
    docker save "$img" | docker exec -i "$CLUSTER-control-plane" \
      ctr --namespace=k8s.io images import --digests --snapshotter=overlayfs - >/dev/null
  fi
  echo "loaded $img"
done

step "ProvisioningRequest CRD (must exist before Kueue starts, or Kueue disables the feature)"
kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/kubernetes/autoscaler/$CA_TAG/cluster-autoscaler/apis/config/crd/autoscaling.x-k8s.io_provisioningrequests.yaml"
kubectl wait --for condition=established --timeout=60s crd/provisioningrequests.autoscaling.x-k8s.io

step "Kueue $KUEUE_VERSION"
kubectl apply --server-side --force-conflicts -f "https://github.com/kubernetes-sigs/kueue/releases/download/$KUEUE_VERSION/manifests.yaml" >/dev/null
use_preloaded() {   # namespace deployment image
  kubectl -n "$1" patch deploy "$2" --type=json -p "[
    {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/image\",\"value\":\"$3\"},
    {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/imagePullPolicy\",\"value\":\"IfNotPresent\"}]" >/dev/null
}
use_preloaded kueue-system kueue-controller-manager "$KUEUE_IMAGE"
kubectl -n kueue-system rollout status deploy/kueue-controller-manager --timeout=180s

step "KWOK $KWOK_VERSION (simulated nodes and pods)"
kubectl apply --server-side --force-conflicts -f "https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION/kwok.yaml" >/dev/null
use_preloaded kube-system kwok-controller "$KWOK_IMAGE"
base="https://raw.githubusercontent.com/kubernetes-sigs/kwok/$KWOK_VERSION/kustomize/stage"
for f in node/fast/node-initialize node/heartbeat/node-heartbeat \
         pod/general/pod-create pod/general/pod-init-container-running \
         pod/general/pod-init-container-completed pod/general/pod-ready \
         pod/general/pod-complete pod/general/pod-delete; do
  kubectl apply --server-side -f "$base/$f.yaml"
done
kubectl -n kube-system rollout status deploy/kwok-controller --timeout=120s

step "Demo queues, flavor, ProvisioningRequestConfig, AdmissionCheck, node pool config"
# Kueue's webhook needs a few seconds after the pod is Ready before it accepts requests.
for i in $(seq 1 30); do
  kubectl apply --server-side -f manifests/kueue-objects.yaml >/dev/null 2>&1 && break
  [ "$i" = 30 ] && kubectl apply --server-side -f manifests/kueue-objects.yaml
  sleep 3
done
kubectl wait --for condition=Active --timeout=60s admissioncheck/taugrid-cas-gpu

step "Build tau-demo"
(cd tau-demo && go build -o ../bin/tau-demo .)

cat <<'EOF'

Ready. In another terminal run:
  ./bin/tau-demo fake-ca              # simulated AKS Cluster Autoscaler
Then: ./scenario.sh
EOF
