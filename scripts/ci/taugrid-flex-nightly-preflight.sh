#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly GPU_RESOURCE="${FLEX_NIGHTLY_GPU_RESOURCE:-nvidia.com/gpu}"
readonly RDMA_RESOURCE="${FLEX_NIGHTLY_RDMA_RESOURCE:-rdma/rdma_shared_device_a}"
readonly MIN_RDMA_NODES="${FLEX_NIGHTLY_MIN_RDMA_NODES:-2}"
readonly MAX_LEASE_AGE_SECONDS="${FLEX_NIGHTLY_MAX_LEASE_AGE_SECONDS:-120}"
readonly CONTRACT_FILE="${FLEX_NIGHTLY_CONTRACT_FILE:-taugrid-flex-nightly-contract.json}"
readonly GPU_TOPOLOGY="${FLEX_NIGHTLY_GPU_TOPOLOGY:-taugrid-gpu-topology}"
readonly INCLUDE_DGX="${FLEX_NIGHTLY_INCLUDE_DGX:-false}"

readonly DGX_SELECTOR="${FLEX_NIGHTLY_DGX_SELECTOR:-}"
readonly DGX_SITE="${FLEX_NIGHTLY_DGX_SITE:-}"
readonly DGX_FLAVOR="${FLEX_NIGHTLY_DGX_FLAVOR:-tau-gpu-dgx-spark-v2}"
readonly DGX_CLUSTER_QUEUE="${FLEX_NIGHTLY_DGX_CLUSTER_QUEUE:-tau-gpu-cq}"
readonly DGX_MIN_NODES="${FLEX_NIGHTLY_DGX_MIN_NODES:-2}"
readonly DGX_GPUS_PER_NODE="${FLEX_NIGHTLY_DGX_GPUS_PER_NODE:-1}"
readonly DGX_ARCHITECTURE="${FLEX_NIGHTLY_DGX_ARCHITECTURE:-arm64}"

readonly A100_SELECTOR="${FLEX_NIGHTLY_A100_SELECTOR:-kueue.azure.com/gpu-series=ndm-a100-v4}"
readonly A100_SITE="${FLEX_NIGHTLY_A100_SITE:-}"
readonly A100_FLAVOR="${FLEX_NIGHTLY_A100_FLAVOR:-tau-gpu-a100-80gb-v2}"
readonly A100_CLUSTER_QUEUE="${FLEX_NIGHTLY_A100_CLUSTER_QUEUE:-tau-gpu-cq}"
readonly A100_MIN_NODES="${FLEX_NIGHTLY_A100_MIN_NODES:-2}"
readonly A100_GPUS_PER_NODE="${FLEX_NIGHTLY_A100_GPUS_PER_NODE:-8}"
readonly A100_ARCHITECTURE="${FLEX_NIGHTLY_A100_ARCHITECTURE:-amd64}"

readonly H200_SELECTOR="${FLEX_NIGHTLY_H200_SELECTOR:-kueue.azure.com/gpu-series=nd-h200-v5}"
readonly H200_SITE="${FLEX_NIGHTLY_H200_SITE:-}"
readonly H200_FLAVOR="${FLEX_NIGHTLY_H200_FLAVOR:-tau-gpu-h200-141gb-v2}"
readonly H200_CLUSTER_QUEUE="${FLEX_NIGHTLY_H200_CLUSTER_QUEUE:-tau-gpu-cq}"
readonly H200_MIN_NODES="${FLEX_NIGHTLY_H200_MIN_NODES:-2}"
readonly H200_GPUS_PER_NODE="${FLEX_NIGHTLY_H200_GPUS_PER_NODE:-8}"
readonly H200_ARCHITECTURE="${FLEX_NIGHTLY_H200_ARCHITECTURE:-amd64}"

fail() {
  echo "::error::$*" >&2
  exit 1
}

require_env() {
  local name="$1"
  local value="${!name:-}"
  [ -n "$value" ] || fail "${name} is required"
  [[ "$value" != '$('* ]] || fail "${name} is unresolved (${value})"
  printf '%s' "$value"
}

require_non_negative_int() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[0-9]+$ ]] || fail "${name} must be a non-negative integer (got '${value}')"
}

validate_selector() {
  local name="$1"
  local selector
  selector="$(require_env "$name")"
  [[ "$selector" != *$'\n'* && "$selector" != *$'\r'* ]] ||
    fail "${name} must be a single-line Kubernetes selector"
}

validate_simple_selector() {
  local name="$1"
  local selector key value
  selector="$(require_env "$name")"
  validate_selector "$name"
  key="${selector%%=*}"
  value="${selector#*=}"
  [ "$key" != "$selector" ] && [ -n "$key" ] && [ -n "$value" ] &&
    [[ "$key" != *","* && "$value" != *","* ]] ||
    fail "${name} must contain one key=value expression"
}

validate_config() {
  local deploy_mode

  command -v jq >/dev/null 2>&1 || fail "jq is required"
  command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"

  validate_simple_selector_value "A100 selector" "$A100_SELECTOR"
  validate_simple_selector_value "H200 selector" "$H200_SELECTOR"
  [ -n "$A100_SITE" ] && [[ "$A100_SITE" != '$('* ]] ||
    fail "FLEX_NIGHTLY_A100_SITE is required"
  [ -n "$H200_SITE" ] && [[ "$H200_SITE" != '$('* ]] ||
    fail "FLEX_NIGHTLY_H200_SITE is required"
  require_non_negative_int FLEX_NIGHTLY_MIN_RDMA_NODES "$MIN_RDMA_NODES"
  require_non_negative_int FLEX_NIGHTLY_MAX_LEASE_AGE_SECONDS "$MAX_LEASE_AGE_SECONDS"
  require_non_negative_int FLEX_NIGHTLY_DGX_MIN_NODES "$DGX_MIN_NODES"
  require_non_negative_int FLEX_NIGHTLY_DGX_GPUS_PER_NODE "$DGX_GPUS_PER_NODE"
  require_non_negative_int FLEX_NIGHTLY_A100_MIN_NODES "$A100_MIN_NODES"
  require_non_negative_int FLEX_NIGHTLY_A100_GPUS_PER_NODE "$A100_GPUS_PER_NODE"
  require_non_negative_int FLEX_NIGHTLY_H200_MIN_NODES "$H200_MIN_NODES"
  require_non_negative_int FLEX_NIGHTLY_H200_GPUS_PER_NODE "$H200_GPUS_PER_NODE"
  [[ "$INCLUDE_DGX" == "true" || "$INCLUDE_DGX" == "false" ||
    "$INCLUDE_DGX" == "True" || "$INCLUDE_DGX" == "False" ]] ||
    fail "FLEX_NIGHTLY_INCLUDE_DGX must be true or false"
  if [[ "$INCLUDE_DGX" == "true" || "$INCLUDE_DGX" == "True" ]]; then
    validate_simple_selector_value "DGX selector" "$DGX_SELECTOR"
    [ -n "$DGX_SITE" ] && [[ "$DGX_SITE" != '$('* ]] ||
      fail "FLEX_NIGHTLY_DGX_SITE is required when DGX Spark is enabled"
  fi

  deploy_mode="${FLEX_NIGHTLY_DEPLOY_MODE:-shared-cluster-controllers}"
  [ "$deploy_mode" = "shared-cluster-controllers" ] ||
    fail "FLEX_NIGHTLY_DEPLOY_MODE must be shared-cluster-controllers; isolated controller installation is unsafe until cluster-scoped names and ownership are configurable"
}

validate_simple_selector_value() {
  local name="$1"
  local selector="$2"
  local key="${selector%%=*}"
  local value="${selector#*=}"

  [[ "$selector" != '$('* ]] || fail "${name} is unresolved (${selector})"
  [ "$key" != "$selector" ] && [ -n "$key" ] && [ -n "$value" ] &&
    [[ "$key" != *","* && "$value" != *","* ]] ||
    fail "${name} must contain one key=value expression"
}

deployment_ready() {
  local namespace="$1"
  local name="$2"
  local available

  available="$("$KUBECTL_BIN" -n "$namespace" get deployment "$name" -o json |
    jq -r '.status.availableReplicas // 0')"
  [ "$available" -ge 1 ] ||
    fail "Deployment ${namespace}/${name} has no available replicas"
}

cluster_queue_ready() {
  local name="$1"

  "$KUBECTL_BIN" get clusterqueue "$name" -o json |
    jq -e 'any(.status.conditions[]?; .type == "Active" and .status == "True")' >/dev/null ||
    fail "ClusterQueue ${name} is not Active=True"
}

topology_ready() {
  local levels

  levels="$("$KUBECTL_BIN" get topology "$GPU_TOPOLOGY" -o json |
    jq -c '[.spec.levels[]?.nodeLabel]')"
  [ "$levels" = '["tau.azure.com/site","tau.azure.com/network-domain","tau.azure.com/accelerator-domain","kubernetes.io/hostname"]' ] ||
    fail "Topology ${GPU_TOPOLOGY} must define site > network-domain > accelerator-domain > hostname (got ${levels})"
}

target_summary() {
  local name="$1"
  local selector="$2"
  local expected_site="$3"
  local flavor="$4"
  local cluster_queue="$5"
  local expected_min_nodes="$6"
  local expected_gpus_per_node="$7"
  local expected_architecture="$8"
  local require_gpu_taint="$9"
  local temp_dir output
  temp_dir="$(mktemp -d)"

  "$KUBECTL_BIN" get nodes -l "$selector" -o json >"${temp_dir}/nodes.json" ||
    fail "cannot inventory ${name} nodes for selector ${selector}"
  "$KUBECTL_BIN" get pods -A -o json >"${temp_dir}/pods.json" ||
    fail "cannot inventory active GPU requests for ${name}"
  "$KUBECTL_BIN" -n kube-node-lease get leases.coordination.k8s.io -o json >"${temp_dir}/leases.json" ||
    fail "cannot inventory node leases for ${name}"
  "$KUBECTL_BIN" get resourceflavor "$flavor" -o json >"${temp_dir}/flavor.json" ||
    fail "ResourceFlavor ${flavor} is missing; apply the topology-aware shared GPU queue migration"
  "$KUBECTL_BIN" get clusterqueue "$cluster_queue" -o json >"${temp_dir}/queue.json" ||
    fail "ClusterQueue ${cluster_queue} is missing; apply the topology-aware shared GPU queue migration"

  output="$(jq -n -c \
    --arg name "$name" \
    --arg selector "$selector" \
    --arg expectedSite "$expected_site" \
    --arg flavor "$flavor" \
    --arg clusterQueue "$cluster_queue" \
    --arg topology "$GPU_TOPOLOGY" \
    --arg gpuResource "$GPU_RESOURCE" \
    --arg rdmaResource "$RDMA_RESOURCE" \
    --arg expectedArchitecture "$expected_architecture" \
    --argjson expectedMinNodes "$expected_min_nodes" \
    --argjson expectedGPUsPerNode "$expected_gpus_per_node" \
    --argjson requireGPUTaint "$require_gpu_taint" \
    --argjson maxLeaseAgeSeconds "$MAX_LEASE_AGE_SECONDS" \
    --slurpfile nodes "${temp_dir}/nodes.json" \
    --slurpfile pods "${temp_dir}/pods.json" \
    --slurpfile leases "${temp_dir}/leases.json" \
    --slurpfile flavorObjects "${temp_dir}/flavor.json" \
    --slurpfile queueObjects "${temp_dir}/queue.json" '
      def quantity:
        if . == null then 0
        elif type == "number" then .
        elif test("^[0-9]+$") then tonumber
        else 0
        end;
      def pod_gpu_request:
        ([.spec.containers[]?.resources.requests[$gpuResource] | quantity] | add // 0) as $regular
        | ([.spec.initContainers[]?.resources.requests[$gpuResource] | quantity] | max // 0) as $init
        | ((.spec.resources.requests[$gpuResource] // "0") | quantity) as $podLevel
        | [$regular, $init, $podLevel] | max;
      ($leases[0].items
        | map({key: .metadata.name, value: (.spec.renewTime // "")})
        | from_entries) as $leaseTimes
      | [
        $nodes[0].items[]
        | . as $node
        | ([ $pods[0].items[]
             | select(.spec.nodeName == $node.metadata.name)
             | select(.status.phase != "Succeeded" and .status.phase != "Failed")
             | pod_gpu_request
           ] | add // 0) as $requested
        | (($node.status.allocatable[$gpuResource] // "0") | quantity) as $allocatable
        | ($leaseTimes[$node.metadata.name] // "") as $leaseRenewTime
        | {
            name: $node.metadata.name,
            site: ($node.metadata.labels["tau.azure.com/site"] // ""),
            architecture: ($node.status.nodeInfo.architecture // ""),
            operating_system: ($node.status.nodeInfo.operatingSystem // ""),
            container_runtime: ($node.status.nodeInfo.containerRuntimeVersion // ""),
            schedulable: ($node.spec.unschedulable != true),
            ready: any($node.status.conditions[]?; .type == "Ready" and .status == "True"),
            pressure_free: (
              ([
                $node.status.conditions[]?
                | select(.type == "MemoryPressure" or .type == "DiskPressure" or .type == "PIDPressure")
                | .type
              ] | unique | length) == 3
              and all(
                $node.status.conditions[]?
                | select(.type == "MemoryPressure" or .type == "DiskPressure" or .type == "PIDPressure");
                .status == "False"
              )
            ),
            gpu_taint: any(
              $node.spec.taints[]?;
              .key == "nvidia.com/gpu" and .effect == "NoSchedule"
            ),
            lease_renew_time: $leaseRenewTime,
            lease_age_seconds: (
              if $leaseRenewTime == "" then null
              else (now - ($leaseRenewTime | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) | floor)
              end
            ),
            gpu_allocatable: $allocatable,
            gpu_requested: $requested,
            gpu_available: ([$allocatable - $requested, 0] | max),
            rdma_allocatable: (($node.status.allocatable[$rdmaResource] // "0") | quantity),
            network_domain: ($node.metadata.labels["tau.azure.com/network-domain"] // ""),
            accelerator_domain: ($node.metadata.labels["tau.azure.com/accelerator-domain"] // "")
          }
      ] as $readyNodes
      | ($flavorObjects[0].spec.topologyName // "") as $flavorTopology
      | ([ $queueObjects[0].spec.resourceGroups[]?.flavors[]?.name ] | index($flavor) != null) as $queueHasFlavor
      | (any($queueObjects[0].status.conditions[]?; .type == "Active" and .status == "True")) as $queueActive
      | ($readyNodes | map(.site) | unique | sort) as $sites
      | {
          minimum_nodes: $expectedMinNodes,
          gpus_per_node: $expectedGPUsPerNode,
          architecture: $expectedArchitecture,
          operating_system: "linux",
          container_runtime_prefix: "containerd://",
          require_gpu_taint: $requireGPUTaint,
          max_lease_age_seconds: $maxLeaseAgeSeconds
        } as $capabilityContract
      | ([
          $readyNodes[]
          | . as $node
          | if ($node.architecture != $expectedArchitecture) then "\($node.name): architecture=\($node.architecture)"
            elif ($node.operating_system != "linux") then "\($node.name): operating_system=\($node.operating_system)"
            elif ($node.container_runtime | startswith("containerd://") | not) then "\($node.name): container_runtime=\($node.container_runtime)"
            elif ($node.schedulable | not) then "\($node.name): unschedulable"
            elif ($node.ready | not) then "\($node.name): Ready is not True"
            elif ($node.pressure_free | not) then "\($node.name): node pressure condition is active"
            elif ($node.gpu_allocatable != $expectedGPUsPerNode) then "\($node.name): gpu_allocatable=\($node.gpu_allocatable)"
            elif ($node.site != $expectedSite) then "\($node.name): site=\($node.site)"
            elif ($node.network_domain == "") then "\($node.name): missing network-domain"
            elif ($node.accelerator_domain == "") then "\($node.name): missing accelerator-domain"
            elif ($requireGPUTaint and ($node.gpu_taint | not)) then "\($node.name): missing nvidia.com/gpu NoSchedule taint"
            elif ($node.lease_age_seconds == null) then "\($node.name): node lease is missing"
            elif ($node.lease_age_seconds < 0 or $node.lease_age_seconds > $maxLeaseAgeSeconds) then "\($node.name): lease_age_seconds=\($node.lease_age_seconds)"
            else empty
            end
        ]
        + if ($readyNodes | length) < $expectedMinNodes
          then ["ready node count \($readyNodes | length) is below \($expectedMinNodes)"]
          else []
          end) as $capabilityViolations
      | {
          name: $name,
          selector: $selector,
          expected_site: $expectedSite,
          flavor: $flavor,
          cluster_queue: $clusterQueue,
          topology: $topology,
          topology_ready: ($flavorTopology == $topology),
          queue_active: $queueActive,
          queue_has_flavor: $queueHasFlavor,
          capability_contract: $capabilityContract,
          capability_ready: ($capabilityViolations | length == 0),
          capability_violations: $capabilityViolations,
          ready_nodes: ([$readyNodes[] | select(.ready and .schedulable)] | length),
          sites: $sites,
          gpu_total: ($readyNodes | map(.gpu_allocatable) | add // 0),
          gpu_requested: ($readyNodes | map(.gpu_requested) | add // 0),
          gpu_available: ($readyNodes | map(.gpu_available) | add // 0),
          max_gpu_available_per_node: ($readyNodes | map(.gpu_available) | max // 0),
          nodes: $readyNodes
        }
      | .status = (
          if (.capability_ready | not) or (.topology_ready | not) or (.queue_active | not) or (.queue_has_flavor | not)
             or (.sites | length) > 1 or ((.sites | length) == 1 and .sites[0] != .expected_site)
          then "misconfigured"
          elif .ready_nodes == 0 or .gpu_total == 0
          then "unavailable"
          elif .gpu_available == 0
          then "busy"
          else "available"
          end
        )
    ')"
  rm -f \
    "${temp_dir}/nodes.json" \
    "${temp_dir}/pods.json" \
    "${temp_dir}/leases.json" \
    "${temp_dir}/flavor.json" \
    "${temp_dir}/queue.json"
  rmdir "$temp_dir"
  printf '%s\n' "$output"
}

run_cluster_preflight() {
  local dgx a100 h200 rdma rdma_nodes
  local kueue_namespace kueue_deployment kuberay_namespace kuberay_deployment

  validate_config

  topology_ready
  if [[ "$INCLUDE_DGX" == "true" || "$INCLUDE_DGX" == "True" ]]; then
    dgx="$(target_summary dgx-spark "$DGX_SELECTOR" "$DGX_SITE" "$DGX_FLAVOR" "$DGX_CLUSTER_QUEUE" "$DGX_MIN_NODES" "$DGX_GPUS_PER_NODE" "$DGX_ARCHITECTURE" false)"
  else
    dgx="$(jq -n -c \
      --arg name dgx-spark \
      --arg selector "$DGX_SELECTOR" \
      --arg site "$DGX_SITE" \
      --arg flavor "$DGX_FLAVOR" \
      --arg clusterQueue "$DGX_CLUSTER_QUEUE" \
      --arg topology "$GPU_TOPOLOGY" \
      '{
        name: $name,
        selector: $selector,
        expected_site: $site,
        flavor: $flavor,
        cluster_queue: $clusterQueue,
        topology: $topology,
        status: "disabled",
        reason: "DGX Spark execution is disabled for this run",
        ready_nodes: 0,
        gpu_total: 0,
        gpu_requested: 0,
        gpu_available: 0,
        max_gpu_available_per_node: 0,
        nodes: []
      }')"
  fi
  a100="$(target_summary a100 "$A100_SELECTOR" "$A100_SITE" "$A100_FLAVOR" "$A100_CLUSTER_QUEUE" "$A100_MIN_NODES" "$A100_GPUS_PER_NODE" "$A100_ARCHITECTURE" true)"
  h200="$(target_summary h200 "$H200_SELECTOR" "$H200_SITE" "$H200_FLAVOR" "$H200_CLUSTER_QUEUE" "$H200_MIN_NODES" "$H200_GPUS_PER_NODE" "$H200_ARCHITECTURE" true)"

  for target in "$dgx" "$a100" "$h200"; do
    [ "$(jq -r '.status' <<<"$target")" != "misconfigured" ] ||
      fail "hardware target $(jq -r '.name' <<<"$target") is misconfigured: $(jq -c '{sites,expected_site,capability_ready,capability_violations,topology_ready,queue_active,queue_has_flavor}' <<<"$target")"
  done

  rdma="$(jq -c '{rdma_nodes: ([.nodes[] | select(.rdma_allocatable > 0)] | length)}' <<<"$h200")"
  rdma_nodes="$(jq -r '.rdma_nodes' <<<"$rdma")"
  [ "$rdma_nodes" -ge "$MIN_RDMA_NODES" ] ||
    fail "RDMA selector has ${rdma_nodes}/${MIN_RDMA_NODES} Ready nodes advertising ${RDMA_RESOURCE}"

  kueue_namespace="${FLEX_NIGHTLY_KUEUE_NAMESPACE:-kueue-system}"
  kueue_deployment="${FLEX_NIGHTLY_KUEUE_DEPLOYMENT:-kueue-controller-manager}"
  kuberay_namespace="${FLEX_NIGHTLY_KUBERAY_NAMESPACE:-kuberay-system}"
  kuberay_deployment="${FLEX_NIGHTLY_KUBERAY_DEPLOYMENT:-kuberay-operator}"

  "$KUBECTL_BIN" get crd rayjobs.ray.io >/dev/null
  "$KUBECTL_BIN" get crd workloads.kueue.x-k8s.io >/dev/null
  deployment_ready "$kueue_namespace" "$kueue_deployment"
  deployment_ready "$kuberay_namespace" "$kuberay_deployment"
  if [[ "$INCLUDE_DGX" == "true" || "$INCLUDE_DGX" == "True" ]]; then
    cluster_queue_ready "$DGX_CLUSTER_QUEUE"
  fi
  cluster_queue_ready "$A100_CLUSTER_QUEUE"
  cluster_queue_ready "$H200_CLUSTER_QUEUE"

  jq -n \
    --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg deployMode "shared-cluster-controllers" \
    --arg topology "$GPU_TOPOLOGY" \
    --arg gpuResource "$GPU_RESOURCE" \
    --arg rdmaResource "$RDMA_RESOURCE" \
    --argjson dgx "$dgx" \
    --argjson a100 "$a100" \
    --argjson h200 "$h200" \
    --argjson rdma "$rdma" \
    '{
      generated_at: $generatedAt,
      deploy_mode: $deployMode,
      topology: $topology,
      resources: {
        gpu: $gpuResource,
        rdma: $rdmaResource
      },
      targets: [$dgx, $a100, $h200],
      rdma_target: $rdma
    }' >"$CONTRACT_FILE"

  echo "Flex nightly preflight passed for DGX Spark, A100, and H200 targets"
  echo "Contract written to ${CONTRACT_FILE}"
}

compare_capability_contracts() {
  local before="${1:-}"
  local after="${2:-}"

  [ -s "$before" ] || fail "preflight contract is missing: ${before}"
  [ -s "$after" ] || fail "postflight contract is missing: ${after}"

  jq -e -n \
    --slurpfile before "$before" \
    --slurpfile after "$after" '
      def signature:
        [
          .targets[]
          | select(.status != "disabled")
          | {
              name,
              capability_contract,
              capability_ready,
              node_capabilities: [
                .nodes[]
                | {
                    name,
                    site,
                    architecture,
                    operating_system,
                    container_runtime,
                    schedulable,
                    ready,
                    pressure_free,
                    gpu_taint,
                    gpu_allocatable,
                    rdma_allocatable,
                    network_domain,
                    accelerator_domain
                  }
              ] | sort_by(.name)
            }
        ] | sort_by(.name);
      ($before[0] | signature) as $beforeSignature
      | ($after[0] | signature) as $afterSignature
      | ($afterSignature | all(.capability_ready))
      and ($beforeSignature == $afterSignature)
    ' >/dev/null ||
    fail "Flex node capability changed or did not recover after workload cleanup"

  echo "Flex node capability matches the preflight snapshot after cleanup"
}

case "${1:-cluster}" in
  validate-config)
    validate_config
    ;;
  cluster)
    run_cluster_preflight
    ;;
  compare-capability)
    compare_capability_contracts "${2:-}" "${3:-}"
    ;;
  *)
    fail "usage: $0 [validate-config|cluster|compare-capability BEFORE AFTER]"
    ;;
esac
