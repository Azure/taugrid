#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUEUE_NAMESPACE="${TAUGRID_MANAGED_KUEUE_NAMESPACE:-kueue-system}"
readonly KUEUE_DEPLOYMENT="${TAUGRID_MANAGED_KUEUE_DEPLOYMENT:-kueue-controller-manager}"
readonly KUBERAY_NAMESPACE="${TAUGRID_MANAGED_KUBERAY_NAMESPACE:-kuberay-system}"
readonly KUBERAY_DEPLOYMENT="${TAUGRID_MANAGED_KUBERAY_DEPLOYMENT:-kuberay-operator}"

fail() {
  echo "Managed control-plane preflight failed: $*" >&2
  exit 1
}

api_ready() {
  local context="$1"
  local api_path="$2"
  shift 2
  local discovery
  local resource

  discovery="$("$KUBECTL_BIN" --context "$context" get --raw "$api_path")" ||
    fail "cannot discover ${api_path}"
  for resource in "$@"; do
    jq -e --arg resource "$resource" \
      'any(.resources[]?; .name == $resource)' <<<"$discovery" >/dev/null ||
      fail "${api_path} does not serve required resource ${resource}"
  done
}

deployment_ready() {
  local context="$1"
  local namespace="$2"
  local name="$3"
  local deployment

  deployment="$("$KUBECTL_BIN" \
    --context "$context" \
    --namespace "$namespace" \
    get deployment "$name" \
    --output json)" ||
    fail "cannot read Deployment ${namespace}/${name}"
  jq -e '
    (.spec.replicas // 0) > 0 and
    (.status.observedGeneration // 0) >= .metadata.generation and
    (.status.availableReplicas // 0) >= .spec.replicas and
    any(.status.conditions[]?; .type == "Available" and .status == "True")
  ' <<<"$deployment" >/dev/null ||
    fail "Deployment ${namespace}/${name} is not available"
}

main() {
  local context="${1:-}"

  [ -n "$context" ] || fail "kube context is required"
  command -v jq >/dev/null 2>&1 || fail "jq is required"
  command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"

  api_ready "$context" /apis/kueue.x-k8s.io/v1beta2 \
    workloads clusterqueues localqueues resourceflavors topologies
  api_ready "$context" /apis/ray.io/v1 \
    rayjobs rayclusters rayservices
  deployment_ready "$context" "$KUEUE_NAMESPACE" "$KUEUE_DEPLOYMENT"
  deployment_ready "$context" "$KUBERAY_NAMESPACE" "$KUBERAY_DEPLOYMENT"

  echo "Managed Kueue and KubeRay control planes are ready"
}

main "$@"
