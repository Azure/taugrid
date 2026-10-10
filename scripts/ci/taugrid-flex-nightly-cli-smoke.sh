#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TAU_BIN="${TAU_BIN:-cli/bin/tau}"
readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly NAMESPACE="${FLEX_NIGHTLY_TAU_NAMESPACE:-ray}"
readonly QUEUE="${FLEX_NIGHTLY_TAU_QUEUE:-backfill}"
JOB_PROFILE="${FLEX_NIGHTLY_TAU_JOB_PROFILE:-}"
RAY_PROFILE="${FLEX_NIGHTLY_TAU_RAY_PROFILE:-}"
readonly RAY_IMAGE="${RAY_E2E_IMAGE:-}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_TAU_ARTIFACT_DIR:-}"
readonly BUILD_ID="${BUILD_BUILDID:-local}"
readonly JOB_ATTEMPT="${SYSTEM_JOBATTEMPT:-1}"
readonly RUN_SUFFIX="${BUILD_ID}-${JOB_ATTEMPT}"
readonly JOB_NAME="tau-flex-job-${RUN_SUFFIX}"
readonly RAY_NAME="tau-flex-ray-${RUN_SUFFIX}"

fail() {
  echo "TauGrid Flex CLI smoke failed: $*" >&2
  exit 1
}

require_value() {
  local name="$1"
  local value="${!name:-}"
  [ -n "$value" ] || fail "${name} is required"
  [[ "$value" != '$('* ]] || fail "${name} is unresolved (${value})"
}

for name in RAY_IMAGE ARTIFACT_DIR; do
  require_value "$name"
done
command -v "$TAU_BIN" >/dev/null 2>&1 || fail "${TAU_BIN} is required"
command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

readonly CONFIG_DIR="${ARTIFACT_DIR}/configs"
readonly LOG_DIR="${ARTIFACT_DIR}/logs"
readonly RESULT_FILE="${ARTIFACT_DIR}/cli-smoke-results.jsonl"
readonly CLEANUP_FILE="${ARTIFACT_DIR}/cleanup-result.json"
mkdir -p "$CONFIG_DIR" "$LOG_DIR"
: >"$RESULT_FILE"

discover_profile() {
  local workers="$1"
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" \
    get cluster.tau.azure.com cluster -o json |
    jq -er \
      --arg namespace "$NAMESPACE" \
      --arg queue "$QUEUE" \
      --argjson workers "$workers" '
        [
          .status.workloadProfiles.profiles[]?
          | select(any(.conditions[]?; .type == "Ready" and .status == "True"))
          | select((.gpusPerWorker // 0) == 0)
          | select((.workerCount // 1) == $workers)
          | select(
              ((.applicability.lanes // []) | length == 0)
              or ((.applicability.lanes // []) | index("training") != null)
            )
          | select(
              ((.applicability.namespaces // []) | length == 0)
              or ((.applicability.namespaces // []) | index($namespace) != null)
            )
          | select(any(.localQueues[]?; .namespace == $namespace and .name == $queue))
          | .name
        ]
        | if length == 1 then .[0]
          elif length == 0 then error("no ready CPU profile matches the nightly namespace and queue")
          else error("multiple ready CPU profiles match; set an explicit nightly profile")
          end
      '
}

if [[ -z "$JOB_PROFILE" ]]; then
  JOB_PROFILE="$(discover_profile 1)"
fi
if [[ -z "$RAY_PROFILE" ]]; then
  RAY_PROFILE="$(discover_profile 1)"
fi

record_result() {
  local name="$1"
  local status="$2"
  local duration="$3"
  local reason="${4:-}"
  jq -cn \
    --arg command "$name" \
    --arg status "$status" \
    --arg reason "$reason" \
    --argjson duration_seconds "$duration" \
    '{
      command: $command,
      status: $status,
      duration_seconds: $duration_seconds,
      reason: $reason
    }' >>"$RESULT_FILE"
}

run_step() {
  local name="$1"
  shift
  local log_name="${name//[^a-zA-Z0-9._-]/-}"
  local started ended
  started="$(date +%s)"
  if "$@" >"${LOG_DIR}/${log_name}.log" 2>&1; then
    ended="$(date +%s)"
    record_result "$name" passed "$((ended - started))"
    return 0
  fi
  ended="$(date +%s)"
  record_result "$name" failed "$((ended - started))" "command exited non-zero"
  cat "${LOG_DIR}/${log_name}.log" >&2
  return 1
}

assert_contains() {
  local name="$1"
  local file="$2"
  local expected="$3"
  if grep -Fq "$expected" "$file"; then
    record_result "$name" passed 0
    return 0
  fi
  record_result "$name" failed 0 "expected marker was not present"
  fail "${name}: expected marker was not present"
}

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  local cleanup_status=passed
  local cleanup_reason=""

  if ! "$TAU_BIN" run cancel "$JOB_NAME" \
    -n "$NAMESPACE" --context "$KUBE_CONTEXT" --wait=false \
    >"${LOG_DIR}/cleanup-job.log" 2>&1; then
    cleanup_status=failed
    cleanup_reason="failed to cancel ${JOB_NAME}"
    rc=1
  fi
  if ! "$TAU_BIN" run cancel "$RAY_NAME" \
    -n "$NAMESPACE" --context "$KUBE_CONTEXT" \
    --timeout 5m --teardown-timeout 12m \
    >"${LOG_DIR}/cleanup-ray.log" 2>&1; then
    cleanup_status=failed
    cleanup_reason="failed to cancel ${RAY_NAME}"
    rc=1
  fi
  if "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get job "$JOB_NAME" >/dev/null 2>&1 ||
    "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
      get rayjob.ray.io "$RAY_NAME" >/dev/null 2>&1; then
    cleanup_status=failed
    cleanup_reason="nightly Tau workloads remain after cleanup"
    rc=1
  fi

  jq -n \
    --arg status "$cleanup_status" \
    --arg reason "$cleanup_reason" \
    --arg job "$JOB_NAME" \
    --arg ray "$RAY_NAME" \
    '{
      status: $status,
      reason: $reason,
      workloads: [$job, $ray]
    }' >"$CLEANUP_FILE"
  exit "$rc"
}
trap cleanup EXIT

cat >"${CONFIG_DIR}/train.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "tau flex job smoke complete"
EOF
chmod +x "${CONFIG_DIR}/train.sh"

cat >"${CONFIG_DIR}/ray_train.py" <<'EOF'
import ray

ray.init(address="auto")
print(f"tau flex ray resources={ray.cluster_resources()}")
print("tau flex ray smoke complete")
EOF

cat >"${CONFIG_DIR}/tau-job.yaml" <<EOF
name: ${JOB_NAME}
engine: job
entrypoint: train.sh
compute:
  gpus: 0
  cpu_request: 10m
  memory_request: 32Mi
runtime:
  image: ${RAY_IMAGE}
policy:
  namespace: ${NAMESPACE}
  profile: ${JOB_PROFILE}
  queue: ${QUEUE}
  lane: training
experiment:
  project: TauGrid nightly
  name: Flex CLI Job smoke
  group: flex-nightly
EOF

cat >"${CONFIG_DIR}/tau-ray.yaml" <<EOF
name: ${RAY_NAME}
engine: rayjob
entrypoint: ray_train.py
runtime:
  image: ${RAY_IMAGE}
compute:
  workers: 1
  gpus_per_worker: 0
  head_cpu_request: 10m
  head_memory_request: 512Mi
  worker_cpu_request: 10m
  worker_memory_request: 256Mi
policy:
  namespace: ${NAMESPACE}
  profile: ${RAY_PROFILE}
  queue: ${QUEUE}
  lane: training
experiment:
  project: TauGrid nightly
  name: Flex CLI Ray smoke
  group: flex-nightly
EOF

run_step "cluster-validate" "$TAU_BIN" cluster validate installation \
  --context "$KUBE_CONTEXT" \
  --release "${TAUGRID_RELEASE:-taugrid}" \
  --namespace "${TAUGRID_SYSTEM_NAMESPACE:-tau-system}" \
  --timeout 10m
run_step "run-schema" "$TAU_BIN" run schema --output json
run_step "job-config-validate" "$TAU_BIN" run validate \
  --config "${CONFIG_DIR}/tau-job.yaml"
run_step "ray-config-validate" "$TAU_BIN" run validate \
  --config "${CONFIG_DIR}/tau-ray.yaml"
run_step "job-client-dry-run" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-job.yaml" \
  --context "$KUBE_CONTEXT" --dry-run=client
run_step "job-server-dry-run" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-job.yaml" \
  --context "$KUBE_CONTEXT" --dry-run=server
run_step "ray-client-dry-run" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-ray.yaml" \
  --context "$KUBE_CONTEXT" --dry-run=client
run_step "ray-server-dry-run" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-ray.yaml" \
  --context "$KUBE_CONTEXT" --dry-run=server

run_step "job-submit" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-job.yaml" --context "$KUBE_CONTEXT"
run_step "job-wait" "$KUBECTL_BIN" --context "$KUBE_CONTEXT" \
  -n "$NAMESPACE" wait --for=condition=complete "job/${JOB_NAME}" --timeout=10m
run_step "job-status" "$TAU_BIN" run status "$JOB_NAME" \
  -n "$NAMESPACE" --context "$KUBE_CONTEXT" --output json
run_step "job-logs" "$TAU_BIN" run logs "$JOB_NAME" \
  -n "$NAMESPACE" --context "$KUBE_CONTEXT" --tail=-1
assert_contains "job-log-marker" "${LOG_DIR}/job-logs.log" \
  "tau flex job smoke complete"

run_step "ray-submit" "$TAU_BIN" run \
  --config "${CONFIG_DIR}/tau-ray.yaml" --context "$KUBE_CONTEXT"
ray_started="$(date +%s)"
ray_status=""
while [[ "$(date +%s)" -lt "$((ray_started + 1200))" ]]; do
  ray_status="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get rayjob.ray.io "$RAY_NAME" -o jsonpath='{.status.jobStatus}' 2>/dev/null || true)"
  case "$ray_status" in
    SUCCEEDED) break ;;
    FAILED | STOPPED)
      record_result "ray-wait" failed "$(( $(date +%s) - ray_started ))" \
        "RayJob reached terminal status ${ray_status}"
      fail "RayJob ${RAY_NAME} reached terminal status ${ray_status}"
      ;;
  esac
  sleep 10
done
if [[ "$ray_status" != "SUCCEEDED" ]]; then
  record_result "ray-wait" failed "$(( $(date +%s) - ray_started ))" \
    "RayJob did not succeed within 20 minutes"
  fail "RayJob ${RAY_NAME} did not succeed within 20 minutes"
fi
record_result "ray-wait" passed "$(( $(date +%s) - ray_started ))"
run_step "ray-status" "$TAU_BIN" run status "$RAY_NAME" \
  -n "$NAMESPACE" --context "$KUBE_CONTEXT" --output json
run_step "ray-logs" "$TAU_BIN" run logs "$RAY_NAME" \
  -n "$NAMESPACE" --context "$KUBE_CONTEXT" --tail=-1
assert_contains "ray-log-marker" "${LOG_DIR}/ray-logs.log" \
  "tau flex ray smoke complete"

run_step "run-list" "$TAU_BIN" run list \
  -n "$NAMESPACE" --context "$KUBE_CONTEXT" --output json
if jq -e --arg job "$JOB_NAME" --arg ray "$RAY_NAME" \
  '([.. | strings] | index($job)) != null and ([.. | strings] | index($ray)) != null' \
  "${LOG_DIR}/run-list.log" >/dev/null; then
  record_result "run-list-contents" passed 0
else
  record_result "run-list-contents" failed 0 \
    "both nightly workloads were not present"
  fail "tau run list did not include both nightly workloads"
fi

echo "TauGrid Flex CLI smoke passed"
