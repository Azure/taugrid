#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TAU_BIN="${TAU_BIN:-cli/bin/tau}"
readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly RAY_IMAGE="${RAY_E2E_IMAGE:-}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_GPU_PROFILE_ARTIFACT_DIR:-}"
readonly BUILD_ID="${BUILD_BUILDID:-local}"
readonly JOB_ATTEMPT="${SYSTEM_JOBATTEMPT:-1}"
readonly CONFIG_DIR="${ARTIFACT_DIR}/configs"
readonly LOG_DIR="${ARTIFACT_DIR}/logs"
readonly INVENTORY_FILE="${ARTIFACT_DIR}/gpu-profiles.json"
readonly RESULT_FILE="${ARTIFACT_DIR}/gpu-profile-results.jsonl"
readonly CLEANUP_FILE="${ARTIFACT_DIR}/gpu-profile-cleanup-result.json"

fail() {
  echo "TauGrid Flex GPU profile smoke failed: $*" >&2
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

mkdir -p "$CONFIG_DIR" "$LOG_DIR"
: >"$RESULT_FILE"
current_name=""
current_namespace=""
cleanup_status=passed
cleanup_reason=""

record_result() {
  local profile="$1"
  local phase="$2"
  local status="$3"
  local reason="${4:-}"
  local shape="${5:-}"
  jq -cn \
    --arg profile "$profile" \
    --arg phase "$phase" \
    --arg status "$status" \
    --arg reason "$reason" \
    --arg shape "$shape" \
    '{profile:$profile,phase:$phase,status:$status,reason:$reason,shape:$shape}' \
    >>"$RESULT_FILE"
}

cancel_current() {
  [[ -n "$current_name" && -n "$current_namespace" ]] || return 0
  if "$TAU_BIN" run cancel "$current_name" \
    -n "$current_namespace" \
    --context "$KUBE_CONTEXT" \
    --timeout 5m \
    --teardown-timeout 12m \
    >"${LOG_DIR}/${current_name}-cleanup.log" 2>&1; then
    current_name=""
    current_namespace=""
    return 0
  fi
  cleanup_status=failed
  cleanup_reason="failed to cancel ${current_namespace}/${current_name}"
  return 1
}

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  if ! cancel_current; then
    rc=1
  fi
  jq -n \
    --arg status "$cleanup_status" \
    --arg reason "$cleanup_reason" \
    '{status:$status,reason:$reason}' >"$CLEANUP_FILE"
  exit "$rc"
}
trap cleanup EXIT

"$KUBECTL_BIN" --context "$KUBE_CONTEXT" \
  get cluster.tau.azure.com cluster -o json |
  jq '
    [
      .status.workloadProfiles.profiles[]?
      | select((.gpusPerWorker // 0) > 0)
      | . + {
          ready: any(.conditions[]?; .type == "Ready" and .status == "True"),
          readyReason: (
            [.conditions[]? | select(.type == "Ready") | .message][0] // ""
          ),
          namespace: (.localQueues[0].namespace // ""),
          queue: (.localQueues[0].name // .defaultLocalQueue // ""),
          team: (.applicability.teams[0] // ""),
          lane: (.applicability.lanes[0] // ""),
          executionTarget: (.executionTarget // "singleCluster"),
          shape: ([
            (.gpusPerWorker // 0 | tostring),
            (.workerCount // 1 | tostring),
            (.mode // "fixed"),
            (.placement // "unconstrained"),
            (.executionTarget // "singleCluster")
          ] | join("/"))
        }
    ]
  ' >"$INVENTORY_FILE"

[[ "$(jq 'length' "$INVENTORY_FILE")" -gt 0 ]] ||
  fail "the deployed TauCluster reports no GPU workload profiles"

overall_rc=0
while IFS= read -r profile; do
  name="$(jq -r '.name' <<<"$profile")"
  shape="$(jq -r '.shape' <<<"$profile")"
  if [[ "$(jq -r '.ready' <<<"$profile")" != "true" ]]; then
    reason="$(jq -r '.readyReason // "profile is not Ready"' <<<"$profile")"
    record_result "$name" readiness failed "$reason" "$shape"
    overall_rc=1
    continue
  fi
  namespace="$(jq -r '.namespace' <<<"$profile")"
  queue="$(jq -r '.queue' <<<"$profile")"
  if [[ -z "$namespace" || -z "$queue" ]]; then
    record_result "$name" readiness failed \
      "ready profile has no resolved LocalQueue" "$shape"
    overall_rc=1
    continue
  fi
  record_result "$name" readiness passed "" "$shape"
done < <(jq -c '.[]' "$INVENTORY_FILE")

cat >"${CONFIG_DIR}/gpu_profile_smoke.py" <<'PY'
import os
import subprocess

import ray

ray.init(address="auto")
expected = int(os.environ["TAU_GPU_PROFILE_EXPECTED_GPUS"])
expected_workers = int(os.environ["TAU_GPU_PROFILE_EXPECTED_WORKERS"])
gpu_nodes = [
    node
    for node in ray.nodes()
    if node["Alive"] and float(node["Resources"].get("GPU", 0)) > 0
]
if len(gpu_nodes) != expected_workers:
    raise RuntimeError(
        f"expected {expected_workers} GPU worker nodes, got {len(gpu_nodes)}: {gpu_nodes}"
    )
observed_gpus = sum(float(node["Resources"].get("GPU", 0)) for node in gpu_nodes)
if observed_gpus != expected:
    raise RuntimeError(f"expected {expected} Ray GPUs, got {observed_gpus}")


@ray.remote(num_gpus=1)
class GPUProbe:
    def inspect(self):
        visible = os.environ.get("CUDA_VISIBLE_DEVICES", "")
        if not visible:
            raise RuntimeError("Ray reserved a GPU actor without CUDA_VISIBLE_DEVICES")
        subprocess.run(["nvidia-smi", "-L"], check=True, capture_output=True, text=True)
        return ray.get_runtime_context().get_node_id(), visible


actors = [GPUProbe.remote() for _ in range(expected)]
observations = ray.get([actor.inspect.remote() for actor in actors])
if len(set(observations)) != expected:
    raise RuntimeError(f"expected {expected} unique GPU placements, got {observations}")
print(f"tau gpu profile placements={observations}")
print("tau gpu profile smoke complete")
PY

write_config() {
  local profile_json="$1"
  local index="$2"
  local live="$3"
  local profile name namespace queue team lane workers gpus config
  profile="$(jq -r '.name' <<<"$profile_json")"
  namespace="$(jq -r '.namespace' <<<"$profile_json")"
  queue="$(jq -r '.queue' <<<"$profile_json")"
  team="$(jq -r '.team' <<<"$profile_json")"
  lane="$(jq -r '.lane' <<<"$profile_json")"
  workers="$(jq -r '.workerCount' <<<"$profile_json")"
  gpus="$(jq -r '.gpusPerWorker' <<<"$profile_json")"
  name="tau-gpu-profile-${live}-${index}-${BUILD_ID}-${JOB_ATTEMPT}"
  config="${CONFIG_DIR}/${name}.yaml"

  cat >"$config" <<EOF
name: ${name}
engine: rayjob
entrypoint: gpu_profile_smoke.py
runtime:
  image: ${RAY_IMAGE}
  env:
    TAU_GPU_PROFILE_EXPECTED_GPUS: "$((workers * gpus))"
    TAU_GPU_PROFILE_EXPECTED_WORKERS: "${workers}"
compute:
  workers: ${workers}
  gpus_per_worker: ${gpus}
  head_cpu_request: 100m
  head_memory_request: 512Mi
  worker_cpu_request: 100m
  worker_memory_request: 512Mi
policy:
  namespace: ${namespace}
  profile: ${profile}
  queue: ${queue}
EOF
  [[ -z "$team" ]] || printf '  team: %s\n' "$team" >>"$config"
  [[ -z "$lane" ]] || printf '  lane: %s\n' "$lane" >>"$config"
  cat >>"$config" <<EOF
experiment:
  project: TauGrid nightly
  name: GPU profile ${profile} ${live}
  group: flex-nightly-gpu-profiles
EOF
  printf '%s\t%s\t%s\n' "$config" "$name" "$namespace"
}

run_dry_run() {
  local profile_json="$1"
  local index="$2"
  local profile shape config_data config
  profile="$(jq -r '.name' <<<"$profile_json")"
  shape="$(jq -r '.shape' <<<"$profile_json")"
  config_data="$(write_config "$profile_json" "$index" dry-run)"
  config="${config_data%%$'\t'*}"

  if "$TAU_BIN" run validate --config "$config" \
    >"${LOG_DIR}/${profile}-validate.log" 2>&1 &&
    "$TAU_BIN" run --config "$config" \
      --context "$KUBE_CONTEXT" --dry-run=client \
      >"${LOG_DIR}/${profile}-client-dry-run.log" 2>&1 &&
    "$TAU_BIN" run --config "$config" \
      --context "$KUBE_CONTEXT" --dry-run=server \
      >"${LOG_DIR}/${profile}-server-dry-run.log" 2>&1; then
    record_result "$profile" dry-run passed "" "$shape"
    return 0
  fi
  record_result "$profile" dry-run failed "validation or dry-run failed" "$shape"
  return 1
}

run_live() {
  local profile_json="$1"
  local index="$2"
  local profile shape config_data config name namespace status started
  profile="$(jq -r '.name' <<<"$profile_json")"
  shape="$(jq -r '.shape' <<<"$profile_json")"
  config_data="$(write_config "$profile_json" "$index" live)"
  IFS=$'\t' read -r config name namespace <<<"$config_data"
  current_name="$name"
  current_namespace="$namespace"

  if ! "$TAU_BIN" run --config "$config" --context "$KUBE_CONTEXT" \
    >"${LOG_DIR}/${profile}-submit.log" 2>&1; then
    record_result "$profile" live failed "submission failed" "$shape"
    cancel_current || true
    return 1
  fi

  started="$(date +%s)"
  status=""
  while [[ "$(date +%s)" -lt "$((started + 1800))" ]]; do
    status="$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$namespace" \
      get rayjob.ray.io "$name" -o jsonpath='{.status.jobStatus}' \
      2>/dev/null || true)"
    case "$status" in
      SUCCEEDED) break ;;
      FAILED | STOPPED) break ;;
    esac
    sleep 10
  done

  live_rc=0
  if [[ "$status" != "SUCCEEDED" ]]; then
    record_result "$profile" live failed \
      "RayJob reached ${status:-no terminal status}" "$shape"
    live_rc=1
  elif ! "$TAU_BIN" run logs "$name" \
    -n "$namespace" --context "$KUBE_CONTEXT" --tail=-1 \
    >"${LOG_DIR}/${profile}-logs.log" 2>&1 ||
    ! grep -Fq "tau gpu profile smoke complete" \
      "${LOG_DIR}/${profile}-logs.log"; then
    record_result "$profile" live failed "GPU marker was not present" "$shape"
    live_rc=1
  else
    record_result "$profile" live passed "" "$shape"
  fi

  if ! cancel_current; then
    record_result "$profile" cleanup failed "$cleanup_reason" "$shape"
    live_rc=1
  else
    record_result "$profile" cleanup passed "" "$shape"
  fi
  return "$live_rc"
}

index=0
while IFS= read -r profile; do
  index=$((index + 1))
  run_dry_run "$profile" "$index" || overall_rc=1
done < <(jq -c '.[] | select(.ready)' "$INVENTORY_FILE")

representatives="$(jq '
  [.[] | select(.ready)]
  | sort_by(.shape, .name)
  | group_by(.shape)
  | map(.[0])
' "$INVENTORY_FILE")"
printf '%s\n' "$representatives" |
  jq . >"${ARTIFACT_DIR}/gpu-profile-representatives.json"

index=0
while IFS= read -r profile; do
  index=$((index + 1))
  run_live "$profile" "$index" || overall_rc=1
done < <(jq -c '.[]' <<<"$representatives")

[[ "$overall_rc" -eq 0 ]] || fail "one or more GPU profile checks failed"
echo "TauGrid Flex GPU profile smoke passed"
