#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly HELM_BIN="${HELM_BIN:-helm}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly TAUCLUSTER_NAME="${FLEX_NIGHTLY_TAUCLUSTER_NAME:-cluster}"
readonly CONTROLLER_CHART="${FLEX_NIGHTLY_CONTROLLER_CHART:-charts/tau-core-controller}"
readonly TAU_PROFILE="${FLEX_NIGHTLY_TAU_PROFILE:-nightly.cpu.1x}"
readonly TAU_NAMESPACE="${FLEX_NIGHTLY_TAU_NAMESPACE:-ray}"
readonly TAU_QUEUE="${FLEX_NIGHTLY_TAU_QUEUE:-backfill}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_TAUCLUSTER_ARTIFACT_DIR:-taucluster-reconcile}"
readonly RECONCILE_TIMEOUT_SECONDS="${FLEX_NIGHTLY_TAUCLUSTER_TIMEOUT_SECONDS:-300}"
readonly REQUIRED_GPU_CLASSES="${FLEX_NIGHTLY_REQUIRED_GPU_CLASSES:-a100-80gb,h100-95gb,h200-141gb}"
readonly RECONCILE_WORKLOAD_PROFILES="${FLEX_NIGHTLY_RECONCILE_WORKLOAD_PROFILES:-true}"

fail() {
  echo "::error::$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

render_reviewed_rules() {
  "$HELM_BIN" template taugrid-flex-nightly "$CONTROLLER_CHART" \
    --show-only templates/taucluster.yaml |
    "$KUBECTL_BIN" create --dry-run=client -f - -o json |
    jq -c '.spec.nodes.labelRules'
}

validate_rule_compatibility() {
  local live_rules="$1"
  local reviewed_rules="$2"
  local conflicts

  if jq -e 'any(.[]; ((.match.vmSizes // []) | length) == 0)' <<<"$live_rules" >/dev/null; then
    fail "the live TauCluster contains a catch-all node label rule; it cannot be safely merged with the reviewed GPU catalog"
  fi

  conflicts="$(jq -cn \
    --argjson live "$live_rules" \
    --argjson reviewed "$reviewed_rules" '
      [
        $live[] as $liveRule
        | ($liveRule.match.vmSizes // [])[] as $vmSize
        | ($reviewed | map(select((.match.vmSizes // []) | index($vmSize))) | first) as $reviewedRule
        | select($reviewedRule != null and $liveRule.labels != $reviewedRule.labels)
        | {
            vm_size: $vmSize,
            live_labels: $liveRule.labels,
            reviewed_labels: $reviewedRule.labels
          }
      ]
    ')"
  [ "$(jq 'length' <<<"$conflicts")" -eq 0 ] ||
    fail "live TauCluster rules conflict with the reviewed GPU catalog: $(jq -c '.' <<<"$conflicts")"
}

merge_rules() {
  local live_rules="$1"
  local reviewed_rules="$2"

  jq -cn \
    --argjson live "$live_rules" \
    --argjson reviewed "$reviewed_rules" '
      ($reviewed | map(.match.vmSizes[]?) | unique) as $reviewedVMSizes
      | $reviewed + [
          $live[]
          | . as $rule
          | (($rule.match.vmSizes // [])
              | map(. as $vmSize | select(($reviewedVMSizes | index($vmSize)) == null))) as $customVMSizes
          | select(($customVMSizes | length) > 0)
          | .match.vmSizes = $customVMSizes
        ]
    '
}

nightly_profile() {
  jq -cn \
    --arg name "$TAU_PROFILE" \
    --arg namespace "$TAU_NAMESPACE" \
    --arg queue "$TAU_QUEUE" '
      {
        name: $name,
        description: "TauGrid Flex nightly CPU command lifecycle.",
        applicability: {
          teams: [],
          lanes: ["training"],
          namespaces: [$namespace]
        },
        gpusPerWorker: 0,
        workerCount: 1,
        mode: "fixed",
        placement: "unconstrained",
        defaultLocalQueue: $queue,
        executionTarget: "singleCluster",
        priorities: {
          workloadPriorityClassName: "tau-train-default",
          podPriorityClassName: "tau-train-default"
        }
      }
    '
}

migrate_profiles() {
  jq -c '
    map(
      if .placement == "independent" then
        .placement = "unconstrained"
      elif .placement == "single-node-nvlink" then
        .placement = "same-host"
      elif .placement == "multi-node-nccl" then
        .placement = "same-network-domain"
      else
        .
      end
    )
  '
}

wait_for_reconciliation() {
  local deadline cluster_json
  deadline=$((SECONDS + RECONCILE_TIMEOUT_SECONDS))
  while ((SECONDS < deadline)); do
    cluster_json="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" \
      get clusters.tau.azure.com "$TAUCLUSTER_NAME" -o json)"
    if jq -e --argjson reconcileProfiles "$RECONCILE_WORKLOAD_PROFILES" '
      .status.observedGeneration == .metadata.generation
      and any(.status.conditions[]?; .type == "NodesReady" and .status == "True")
      and any(.status.conditions[]?; .type == "Ready" and .status == "True")
      and (
        ($reconcileProfiles | not)
        or any(.status.conditions[]?; .type == "WorkloadProfilesReady" and .status == "True")
      )
    ' <<<"$cluster_json" >/dev/null; then
      printf '%s\n' "$cluster_json" >"${ARTIFACT_DIR}/taucluster-after.json"
      return 0
    fi
    sleep 5
  done
  fail "TauCluster did not reconcile within ${RECONCILE_TIMEOUT_SECONDS}s"
}

verify_gpu_classes() {
  local nodes_json missing
  nodes_json="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" get nodes -o json)"
  missing="$(jq -c \
    --arg required "$REQUIRED_GPU_CLASSES" '
      . as $nodes
      | ($required | split(",") | map(select(length > 0))) as $classes
      | [
          $classes[]
          | . as $gpuClass
          | select(any(
              $nodes.items[];
              .metadata.labels["tau.azure.com/gpu-class"] == $gpuClass
              and ((.status.allocatable["nvidia.com/gpu"] // "0") | tonumber) > 0
              and .spec.unschedulable != true
              and any(.status.conditions[]?; .type == "Ready" and .status == "True")
            ) | not)
        ]
    ' <<<"$nodes_json")"
  [ "$(jq 'length' <<<"$missing")" -eq 0 ] ||
    fail "TauGrid did not discover Ready GPU capacity for required classes: $(jq -r 'join(", ")' <<<"$missing")"

  jq \
    --arg reconciledAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg requiredGPUClasses "$REQUIRED_GPU_CLASSES" '
      {
        reconciled_at: $reconciledAt,
        required_gpu_classes: ($requiredGPUClasses | split(",")),
        nodes: [
          .items[]
          | select(.metadata.labels["tau.azure.com/gpu-class"] != null)
          | {
              name: .metadata.name,
              vm_size: .metadata.labels["node.kubernetes.io/instance-type"],
              gpu_class: .metadata.labels["tau.azure.com/gpu-class"],
              gpu_series: .metadata.labels["kueue.azure.com/gpu-series"],
              gpu_allocatable: .status.allocatable["nvidia.com/gpu"]
            }
        ]
      }
    ' <<<"$nodes_json" >"${ARTIFACT_DIR}/gpu-class-inventory.json"
}

main() {
  local live_cluster live_rules reviewed_rules merged_rules profile updated_profiles patch operation

  require_command "$KUBECTL_BIN"
  require_command "$HELM_BIN"
  require_command jq
  [[ "$RECONCILE_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] ||
    fail "FLEX_NIGHTLY_TAUCLUSTER_TIMEOUT_SECONDS must be a positive integer"
  [[ "$RECONCILE_WORKLOAD_PROFILES" == "true" || "$RECONCILE_WORKLOAD_PROFILES" == "false" ]] ||
    fail "FLEX_NIGHTLY_RECONCILE_WORKLOAD_PROFILES must be true or false"
  [ -d "$CONTROLLER_CHART" ] || fail "controller chart is missing: ${CONTROLLER_CHART}"
  mkdir -p "$ARTIFACT_DIR"

  live_cluster="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" \
    get clusters.tau.azure.com "$TAUCLUSTER_NAME" -o json)"
  printf '%s\n' "$live_cluster" >"${ARTIFACT_DIR}/taucluster-before.json"
  jq -e '.spec.managementMode == "Reconcile"' <<<"$live_cluster" >/dev/null ||
    fail "TauCluster ${TAUCLUSTER_NAME} must use managementMode=Reconcile"
  jq -e '.spec.queues.ownership == "External"' <<<"$live_cluster" >/dev/null ||
    fail "TauCluster ${TAUCLUSTER_NAME} must retain external queue ownership"

  reviewed_rules="$(render_reviewed_rules)"
  live_rules="$(jq -c '.spec.nodes.labelRules // []' <<<"$live_cluster")"
  validate_rule_compatibility "$live_rules" "$reviewed_rules"
  merged_rules="$(merge_rules "$live_rules" "$reviewed_rules")"
  patch="$(jq -cn \
    --arg resourceVersion "$(jq -r '.metadata.resourceVersion' <<<"$live_cluster")" \
    --argjson rules "$merged_rules" '
      [
        {
          op: "test",
          path: "/metadata/resourceVersion",
          value: $resourceVersion
        },
        {
          op: "replace",
          path: "/spec/nodes/labelRules",
          value: $rules
        }
      ]
    ')"
  if [[ "$RECONCILE_WORKLOAD_PROFILES" == "true" ]]; then
    profile="$(nightly_profile)"
    updated_profiles="$(jq -c \
      --argjson profile "$profile" '
        (.spec.workloadProfiles // [])
        | map(select(.name != $profile.name)) + [$profile]
      ' <<<"$live_cluster" | migrate_profiles)"
    operation="replace"
    jq -e '.spec.workloadProfiles != null' <<<"$live_cluster" >/dev/null || operation="add"
    patch="$(jq -c \
      --arg profileOperation "$operation" \
      --argjson profiles "$updated_profiles" '
        . + [{
          op: $profileOperation,
          path: "/spec/workloadProfiles",
          value: $profiles
        }]
      ' <<<"$patch")"
  fi
  printf '%s\n' "$patch" >"${ARTIFACT_DIR}/taucluster-patch.json"
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" patch \
    clusters.tau.azure.com "$TAUCLUSTER_NAME" \
    --type=json \
    --patch "$patch" \
    -o json >"${ARTIFACT_DIR}/taucluster-patched.json"

  wait_for_reconciliation
  verify_gpu_classes
  if [[ "$RECONCILE_WORKLOAD_PROFILES" == "true" ]]; then
    echo "TauCluster reviewed GPU catalog and nightly CPU profile reconciled"
  else
    echo "TauCluster reviewed GPU catalog reconciled; workload profiles deferred until after controller upgrade"
  fi
}

main "$@"
