#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TAU_BIN="${TAU_BIN:-tau}"
readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly CONTRACT_FILE="${FLEX_NIGHTLY_HARDWARE_CONTRACT:?FLEX_NIGHTLY_HARDWARE_CONTRACT is required}"
readonly RAY_IMAGE="${RAY_E2E_IMAGE:?RAY_E2E_IMAGE is required}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_GPU_ROUTING_ARTIFACT_DIR:-gpu-routing-smoke}"
readonly BUILD_ID="${BUILD_BUILDID:-local}"
readonly ATTEMPT="${SYSTEM_JOBATTEMPT:-1}"
readonly NAMESPACE="${FLEX_NIGHTLY_GPU_ROUTING_NAMESPACE:-taugrid-nightly-routing-${BUILD_ID}-${ATTEMPT}}"
readonly QUEUE="${FLEX_NIGHTLY_GPU_ROUTING_QUEUE:-routing}"
readonly RESULT_FILE="${ARTIFACT_DIR}/gpu-routing-results.jsonl"
readonly LOG_DIR="${ARTIFACT_DIR}/logs"
readonly CONFIG_DIR="${ARTIFACT_DIR}/configs"

fail() {
  echo "::error::$*" >&2
  exit 1
}

record_result() {
  local target="$1"
  local status="$2"
  local reason="${3:-}"
  local flavor="${4:-}"
  local node="${5:-}"
  jq -cn \
    --arg target "$target" \
    --arg status "$status" \
    --arg reason "$reason" \
    --arg flavor "$flavor" \
    --arg node "$node" \
    '{
      target: $target,
      status: $status,
      reason: $reason,
      admitted_flavor: $flavor,
      node: $node
    }' >>"$RESULT_FILE"
}

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  local status=passed
  local reason=""
  if ! "$KUBECTL_BIN" --context "$KUBE_CONTEXT" delete namespace "$NAMESPACE" \
    --ignore-not-found --wait=true --timeout=15m \
    >"${LOG_DIR}/cleanup.log" 2>&1; then
    status=failed
    reason="namespace deletion failed"
    rc=1
  fi
  if "$KUBECTL_BIN" --context "$KUBE_CONTEXT" get namespace "$NAMESPACE" \
    >/dev/null 2>&1; then
    status=failed
    reason="namespace remains after cleanup"
    rc=1
  fi
  jq -n \
    --arg status "$status" \
    --arg reason "$reason" \
    --arg namespace "$NAMESPACE" \
    '{status:$status,reason:$reason,namespace:$namespace}' \
    >"${ARTIFACT_DIR}/gpu-routing-cleanup-result.json"
  exit "$rc"
}

wait_for_admission() {
  local job_uid="$1"
  local deadline workload workloads_json
  deadline=$((SECONDS + 900))
  while ((SECONDS < deadline)); do
    workloads_json="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
      get workloads.kueue.x-k8s.io -o json 2>/dev/null || printf '{"items":[]}')"
    workload="$(jq -c --arg uid "$job_uid" '
        first(
          .items[]
          | select(any(.metadata.ownerReferences[]?; .uid == $uid))
          | select(.status.admission != null)
        ) // empty
      ' <<<"$workloads_json")"
    if [[ -n "$workload" ]]; then
      printf '%s\n' "$workload"
      return 0
    fi
    sleep 5
  done
  return 1
}

run_target() {
  local target_json="$1"
  local target gpu_class expected_flavor expected_series job_name config job_uid
  local workload admitted_flavor pod_json node_name node_json
  target="$(jq -r '.name' <<<"$target_json")"
  gpu_class="$(jq -r '.gpu_class' <<<"$target_json")"
  expected_flavor="$(jq -r '.flavor' <<<"$target_json")"
  expected_series="$(jq -r '.selector | split("=")[1]' <<<"$target_json")"
  job_name="tau-route-${target}-${BUILD_ID}-${ATTEMPT}"
  config="${CONFIG_DIR}/${target}.yaml"

  cat >"$config" <<EOF
name: ${job_name}
engine: job
entrypoint: gpu-check.sh
compute:
  gpus: 1
  cpu_request: 100m
  memory_request: 256Mi
runtime:
  image: ${RAY_IMAGE}
policy:
  namespace: ${NAMESPACE}
  queue: ${QUEUE}
  gpu_class: ${gpu_class}
  topology: same-host
  disable_default_priorities: true
experiment:
  project: TauGrid nightly
  name: Flex ${target} queue routing
  group: flex-nightly
EOF

  "$TAU_BIN" run validate --config "$config" \
    >"${LOG_DIR}/${target}-validate.log" 2>&1
  "$TAU_BIN" run --config "$config" --context "$KUBE_CONTEXT" --dry-run=server \
    >"${LOG_DIR}/${target}-dry-run.log" 2>&1
  "$TAU_BIN" run --config "$config" --context "$KUBE_CONTEXT" \
    >"${LOG_DIR}/${target}-submit.log" 2>&1

  job_uid="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get job "$job_name" -o jsonpath='{.metadata.uid}')"
  workload="$(wait_for_admission "$job_uid")" ||
    fail "${target}: Kueue did not admit ${job_name}"
  printf '%s\n' "$workload" >"${ARTIFACT_DIR}/${target}-workload.json"
  admitted_flavor="$(jq -r '
    [
      .status.admission.podSetAssignments[]?.flavors["nvidia.com/gpu"] // empty
    ] | unique | if length == 1 then .[0] else "" end
  ' <<<"$workload")"
  [[ "$admitted_flavor" == "$expected_flavor" ]] ||
    fail "${target}: expected ResourceFlavor ${expected_flavor}, got ${admitted_flavor:-none}"

  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    wait --for=condition=complete "job/${job_name}" --timeout=15m \
    >"${LOG_DIR}/${target}-wait.log" 2>&1
  "$TAU_BIN" run logs "$job_name" -n "$NAMESPACE" --context "$KUBE_CONTEXT" --tail=-1 \
    >"${LOG_DIR}/${target}-logs.log" 2>&1
  grep -Fq "tau gpu routing complete" "${LOG_DIR}/${target}-logs.log" ||
    fail "${target}: GPU completion marker is missing"

  pod_json="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get pods -l "job-name=${job_name}" -o json)"
  node_name="$(jq -r 'first(.items[] | .spec.nodeName) // ""' <<<"$pod_json")"
  [[ -n "$node_name" ]] || fail "${target}: completed Job has no Pod node"
  node_json="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" get node "$node_name" -o json)"
  [[ "$(jq -r '.metadata.labels["tau.azure.com/gpu-class"] // ""' <<<"$node_json")" == "$gpu_class" ]] ||
    fail "${target}: Pod node ${node_name} does not carry gpu_class=${gpu_class}"
  [[ "$(jq -r '.metadata.labels["kueue.azure.com/gpu-series"] // ""' <<<"$node_json")" == "$expected_series" ]] ||
    fail "${target}: Pod node ${node_name} does not carry gpu-series=${expected_series}"

  record_result "$target" passed "" "$admitted_flavor" "$node_name"
  "$TAU_BIN" run cancel "$job_name" -n "$NAMESPACE" --context "$KUBE_CONTEXT" --wait=false \
    >"${LOG_DIR}/${target}-cancel.log" 2>&1
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    wait --for=delete "job/${job_name}" --timeout=5m \
    >"${LOG_DIR}/${target}-delete-wait.log" 2>&1 ||
    fail "${target}: Job remains after cancellation"
}

main() {
  local cluster_queues cluster_queue
  command -v "$TAU_BIN" >/dev/null 2>&1 || fail "${TAU_BIN} is required"
  command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"
  command -v jq >/dev/null 2>&1 || fail "jq is required"
  [ -s "$CONTRACT_FILE" ] || fail "hardware contract is missing: ${CONTRACT_FILE}"
  mkdir -p "$ARTIFACT_DIR" "$LOG_DIR" "$CONFIG_DIR"
  : >"$RESULT_FILE"

  jq -e '
    [.targets[] | select(.name == "a100" or .name == "h100" or .name == "h200")]
    | length == 3
    and all(.[]; .status != "disabled" and (.gpu_class // "") != "")
  ' "$CONTRACT_FILE" >/dev/null ||
    fail "hardware contract must expose enabled A100, H100, and H200 gpu_class targets"

  cluster_queues="$(jq -c '
    [.targets[] | select(.name == "a100" or .name == "h100" or .name == "h200") | .cluster_queue]
    | unique
  ' "$CONTRACT_FILE")"
  [[ "$(jq 'length' <<<"$cluster_queues")" -eq 1 ]] ||
    fail "A100, H100, and H200 routing targets must share one ClusterQueue"
  cluster_queue="$(jq -r '.[0]' <<<"$cluster_queues")"

  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" create namespace "$NAMESPACE"
  trap cleanup EXIT
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" label namespace "$NAMESPACE" \
    tau.azure.com/workspace=nightly \
    tau.azure.com/gpu-queue=enabled \
    "tau.azure.com/e2e-run=${BUILD_ID}-${ATTEMPT}"
  cat <<EOF | "$KUBECTL_BIN" --context "$KUBE_CONTEXT" apply -f -
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: ${QUEUE}
  namespace: ${NAMESPACE}
spec:
  clusterQueue: ${cluster_queue}
EOF
  cat >"${CONFIG_DIR}/gpu-check.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
nvidia-smi --query-gpu=name,uuid --format=csv,noheader
echo "CUDA_VISIBLE_DEVICES=${CUDA_VISIBLE_DEVICES:-}"
echo "tau gpu routing complete"
EOF
  chmod +x "${CONFIG_DIR}/gpu-check.sh"

  while IFS= read -r target; do
    run_target "$target"
  done < <(jq -c '
    .targets[]
    | select(.name == "a100" or .name == "h100" or .name == "h200")
  ' "$CONTRACT_FILE")
}

main "$@"
