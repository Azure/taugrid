#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
TAU_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd -- "${TAU_DIR}/.." && pwd)"
EXAMPLE_DIR="${TAU_DIR}/examples/kind-smoke"

CLUSTER_NAME="${TAU_KIND_CLUSTER_NAME:-tau-kind}"
KUBE_CONTEXT="${TAU_KIND_CONTEXT:-kind-${CLUSTER_NAME}}"
NAMESPACE="${TAU_KIND_NAMESPACE:-ray}"
JOB_NAME="${TAU_KIND_RUN_NAME:-tau-kind-smoke}"
RAY_JOB_NAME="${TAU_KIND_RAY_RUN_NAME:-tau-kind-ray}"
WAIT_TIMEOUT="${TAU_KIND_WAIT_TIMEOUT:-180s}"
RAY_WAIT_TIMEOUT="${TAU_KIND_RAY_WAIT_TIMEOUT:-600s}"
TAUGRID_RELEASE="${TAU_KIND_TAUGRID_RELEASE:-taugrid-kind}"
TAUGRID_NAMESPACE="${TAU_KIND_TAUGRID_NAMESPACE:-tau-system}"
TAU_BIN="${TAU_BIN:-${TAU_DIR}/bin/tau}"
KIND_GET_TIMEOUT_SECONDS="${TAU_KIND_GET_TIMEOUT_SECONDS:-30}"
KIND_DELETE_TIMEOUT_SECONDS="${TAU_KIND_DELETE_TIMEOUT_SECONDS:-180}"
KIND_CREATE_TIMEOUT_SECONDS="${TAU_KIND_CREATE_TIMEOUT_SECONDS:-600}"
if [[ -n "${TAU_KIND_CONTAINER_ENGINE:-}" ]]; then
  CONTAINER_ENGINE="${TAU_KIND_CONTAINER_ENGINE}"
elif [[ "${KIND_EXPERIMENTAL_PROVIDER:-}" == "podman" ]]; then
  CONTAINER_ENGINE="podman"
else
  CONTAINER_ENGINE="docker"
fi
ENGINE_INFO_TIMEOUT_SECONDS="${TAU_KIND_ENGINE_INFO_TIMEOUT_SECONDS:-${TAU_KIND_DOCKER_INFO_TIMEOUT_SECONDS:-20}}"
ENGINE_MIN_MEMORY_MIB="${TAU_KIND_ENGINE_MIN_MEMORY_MIB:-${TAU_KIND_DOCKER_MIN_MEMORY_MIB:-7680}}"
ENGINE_RECOMMENDED_MEMORY_MIB="${TAU_KIND_ENGINE_RECOMMENDED_MEMORY_MIB:-${TAU_KIND_DOCKER_RECOMMENDED_MEMORY_MIB:-7800}}"
KIND_NODE_TASKS_MAX="${TAU_KIND_NODE_TASKS_MAX:-1024}"
RAY_COMPLETION_MARKER="${TAU_KIND_RAY_COMPLETION_MARKER:-tau kind ray smoke complete}"
DIAGNOSTICS_READY=0

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 127
  fi
}

for tool in kind kubectl helm "$CONTAINER_ENGINE"; do
  need "$tool"
done
if [[ "${TAU_KIND_SKIP_BUILD:-0}" != "1" ]]; then
  need go
fi

run_with_timeout() {
  local timeout_seconds="$1"
  shift
  local pid start status
  "$@" &
  pid=$!
  start=$SECONDS
  while kill -0 "$pid" 2>/dev/null; do
    if (( SECONDS - start >= timeout_seconds )); then
      echo "command timed out after ${timeout_seconds}s: $*" >&2
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 1
  done
  wait "$pid"
  status=$?
  return "$status"
}

container_engine_preflight() {
  echo "Checking ${CONTAINER_ENGINE} health"
  if ! run_with_timeout "$ENGINE_INFO_TIMEOUT_SECONDS" "$CONTAINER_ENGINE" info >/dev/null; then
    echo "${CONTAINER_ENGINE} did not respond within ${ENGINE_INFO_TIMEOUT_SECONDS}s; restart it before running the Kind E2E" >&2
    return 1
  fi

  local memory_format mem_bytes mem_mib
  case "$CONTAINER_ENGINE" in
    podman) memory_format='{{.Host.MemTotal}}' ;;
    *) memory_format='{{.MemTotal}}' ;;
  esac
  mem_bytes="$(run_with_timeout "$ENGINE_INFO_TIMEOUT_SECONDS" "$CONTAINER_ENGINE" info --format "$memory_format" 2>/dev/null || true)"
  if [[ "$mem_bytes" =~ ^[0-9]+$ ]]; then
    mem_mib=$((mem_bytes / 1024 / 1024))
    echo "${CONTAINER_ENGINE} memory: ${mem_mib}MiB"
    if (( mem_mib < ENGINE_MIN_MEMORY_MIB )); then
      echo "${CONTAINER_ENGINE} memory ${mem_mib}MiB is below the required ${ENGINE_MIN_MEMORY_MIB}MiB for Tau Kind smoke" >&2
      echo "configure at least 8192MiB for the Docker or Podman VM before running this two-node Ray smoke" >&2
      return 1
    fi
    if (( mem_mib < ENGINE_RECOMMENDED_MEMORY_MIB )); then
      echo "warning: ${CONTAINER_ENGINE} memory ${mem_mib}MiB is below recommended ${ENGINE_RECOMMENDED_MEMORY_MIB}MiB; Ray driver completion may be slow or flaky" >&2
      if [[ "${TAU_KIND_RAY_WAIT_FOR_COMPLETION:-0}" == "1" ]]; then
        echo "TAU_KIND_RAY_WAIT_FOR_COMPLETION=1 requires at least ${ENGINE_RECOMMENDED_MEMORY_MIB}MiB ${CONTAINER_ENGINE} memory; raise it or unset completion mode" >&2
        return 1
      fi
    fi
  else
    echo "warning: could not determine ${CONTAINER_ENGINE} memory from ${CONTAINER_ENGINE} info; continuing" >&2
  fi
}

configure_kind_node_task_budget() {
  if [[ "$CONTAINER_ENGINE" != "podman" ]]; then
    return 0
  fi
  if ! [[ "$KIND_NODE_TASKS_MAX" =~ ^[0-9]+$ ]] || (( KIND_NODE_TASKS_MAX < 512 )); then
    echo "TAU_KIND_NODE_TASKS_MAX must be an integer of at least 512" >&2
    return 1
  fi

  local node_container="${CLUSTER_NAME}-control-plane"
  local current
  current="$("$CONTAINER_ENGINE" exec "$node_container" systemctl show -p DefaultTasksMax --value)"
  if [[ "$current" =~ ^[0-9]+$ ]] && (( current >= KIND_NODE_TASKS_MAX )); then
    echo "Kind node task budget: ${current}"
    return 0
  fi

  echo "Raising Podman Kind node task budget from ${current:-unknown} to ${KIND_NODE_TASKS_MAX}"
  "$CONTAINER_ENGINE" exec "$node_container" sh -c '
    mkdir -p /etc/systemd/system.conf.d
    printf "%s\n" "[Manager]" "DefaultTasksMax=$1" > /etc/systemd/system.conf.d/90-tau-kind-tasks.conf
    systemctl daemon-reexec
  ' sh "$KIND_NODE_TASKS_MAX"
  current="$("$CONTAINER_ENGINE" exec "$node_container" systemctl show -p DefaultTasksMax --value)"
  if [[ "$current" != "$KIND_NODE_TASKS_MAX" ]]; then
    echo "failed to configure Podman Kind node task budget: expected ${KIND_NODE_TASKS_MAX}, got ${current:-unknown}" >&2
    return 1
  fi
}

dump_diagnostics() {
  status=$?
  if [[ $status -ne 0 ]]; then
    if [[ "$DIAGNOSTICS_READY" != "1" ]]; then
      echo
      echo "Tau Kind smoke failed before a Kubernetes context was confirmed; skipping cluster diagnostics" >&2
      return "$status"
    fi
    echo
    echo "Tau Kind smoke failed; dumping diagnostics from ${KUBE_CONTEXT}/${NAMESPACE}" >&2
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" get namespace "$NAMESPACE" -o wide >&2 || true
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get job,pod,rayjob.ray.io,raycluster.ray.io,workloads.kueue.x-k8s.io,localqueues.kueue.x-k8s.io -o wide >&2 || true
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" describe job "$JOB_NAME" >&2 || true
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" describe rayjob.ray.io "$RAY_JOB_NAME" >&2 || true
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get events --sort-by=.lastTimestamp >&2 || true
    kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$TAUGRID_NAMESPACE" get pod,deploy -o wide >&2 || true
  fi
  return "$status"
}
trap dump_diagnostics EXIT

wait_for_workload_admitted() {
  local kind_name="$1"
  local name="$2"
  local timeout="$3"
  local uid deadline admitted workload
  uid="$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get "$kind_name" "$name" -o jsonpath='{.metadata.uid}')"
  deadline=$((SECONDS + ${timeout%s}))
  while (( SECONDS < deadline )); do
    workload="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=${uid}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    admitted="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=${uid}" -o jsonpath='{.items[0].status.conditions[?(@.type=="Admitted")].status}' 2>/dev/null || true)"
    if [[ "$admitted" == "True" ]]; then
      echo "Kueue admitted ${kind_name}/${name} via Workload ${workload}"
      return 0
    fi
    sleep 5
  done
  echo "timed out waiting for Kueue Workload admission for ${kind_name}/${name}" >&2
  return 1
}

workload_name_for() {
  local kind_name="$1"
  local name="$2"
  local uid
  uid="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get "$kind_name" "$name" -o jsonpath='{.metadata.uid}')"
  kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=${uid}" -o jsonpath='{.items[0].metadata.name}'
}

wait_for_workload_finished() {
  local kind_name="$1"
  local name="$2"
  local timeout="$3"
  local workload deadline finished
  workload="$(workload_name_for "$kind_name" "$name")"
  if [[ -z "$workload" ]]; then
    echo "no Kueue Workload found for ${kind_name}/${name}" >&2
    return 1
  fi
  deadline=$((SECONDS + ${timeout%s}))
  while (( SECONDS < deadline )); do
    finished="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io "$workload" -o jsonpath='{.status.conditions[?(@.type=="Finished")].status}' 2>/dev/null || true)"
    if [[ "$finished" == "True" ]]; then
      echo "Kueue Workload ${workload} is Finished"
      return 0
    fi
    sleep 5
  done
  echo "timed out waiting for Kueue Workload ${workload} to become Finished" >&2
  return 1
}

wait_for_rayjob_running() {
  local name="$1"
  local timeout="$2"
  local deadline status deployment cluster_name cluster_state scheduling scheduling_issue pod_states
  local failed_pod_issue reported_failed_pods
  reported_failed_pods=""
  deadline=$((SECONDS + ${timeout%s}))
  while (( SECONDS < deadline )); do
    status="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.jobStatus}' 2>/dev/null || true)"
    deployment="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.jobDeploymentStatus}' 2>/dev/null || true)"
    cluster_name="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.rayClusterName}' 2>/dev/null || true)"
    cluster_state="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.rayClusterStatus.state}' 2>/dev/null || true)"
    if [[ -z "$cluster_state" && -n "$cluster_name" ]]; then
      cluster_state="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get raycluster.ray.io "$cluster_name" -o jsonpath='{.status.state}' 2>/dev/null || true)"
    fi
    if [[ "$status" == "SUCCEEDED" || ( "$deployment" == "Running" && "$cluster_state" == "ready" ) ]]; then
      echo "RayJob ${name} reached jobStatus=${status:-<empty>} jobDeploymentStatus=${deployment:-<empty>} rayCluster=${cluster_name:-<empty>} rayClusterStatus=${cluster_state:-<empty>}"
      return 0
    fi
    if [[ "$status" == "FAILED" || "$deployment" == "Failed" ]]; then
      echo "RayJob ${name} failed: jobStatus=${status:-<empty>} jobDeploymentStatus=${deployment:-<empty>} rayClusterStatus=${cluster_state:-<empty>}" >&2
      return 1
    fi
    scheduling_issue=""
    failed_pod_issue=""
    if [[ -n "$cluster_name" ]]; then
      scheduling="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pods \
        -l "ray.io/cluster=${cluster_name}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[?(@.type=="PodScheduled")]}{.status}{"\t"}{.reason}{"\t"}{.message}{end}{"\n"}{end}' \
        2>/dev/null || true)"
      while IFS=$'\t' read -r pod_name scheduled reason message; do
        if [[ "$scheduled" == "False" && "$reason" == "Unschedulable" ]]; then
          scheduling_issue="; pod ${pod_name} is currently Unschedulable: ${message}"
        fi
      done <<<"$scheduling"

      pod_states="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pods \
        -l "ray.io/cluster=${cluster_name}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.containerStatuses[*]}{.name}{"="}{.state.terminated.exitCode}{":"}{.state.terminated.reason}{","}{end}{"\n"}{end}' \
        2>/dev/null || true)"
      while IFS=$'\t' read -r pod_name phase container_states; do
        if [[ "$phase" == "Failed" || "$container_states" =~ =([1-9][0-9]*): ]]; then
          failed_pod_issue="; pod ${pod_name} exited with ${container_states:-<empty>}, waiting for KubeRay recovery"
          if ! grep -Fxq "$pod_name" <<<"$reported_failed_pods"; then
            echo "RayJob ${name} observed failed pod ${pod_name} with container state ${container_states:-<empty>}; waiting for KubeRay recovery" >&2
            kubectl --request-timeout=20s --context "$KUBE_CONTEXT" -n "$NAMESPACE" logs "$pod_name" \
              --all-containers=true --tail=100 >&2 || true
            reported_failed_pods+="${pod_name}"$'\n'
          fi
        fi
      done <<<"$pod_states"
    fi
    echo "waiting for RayJob ${name}: jobStatus=${status:-<empty>} jobDeploymentStatus=${deployment:-<empty>} rayCluster=${cluster_name:-<empty>} rayClusterStatus=${cluster_state:-<empty>}${scheduling_issue}${failed_pod_issue}"
    sleep 2
  done
  echo "timed out waiting for RayJob ${name} to reach Running with a ready RayCluster" >&2
  return 1
}

wait_for_rayjob_succeeded() {
  local name="$1"
  local timeout="$2"
  local deadline status deployment
  deadline=$((SECONDS + ${timeout%s}))
  while (( SECONDS < deadline )); do
    status="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.jobStatus}' 2>/dev/null || true)"
    deployment="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.jobDeploymentStatus}' 2>/dev/null || true)"
    if [[ "$status" == "SUCCEEDED" ]]; then
      echo "RayJob ${name} reached jobStatus=SUCCEEDED"
      return 0
    fi
    if [[ "$status" == "FAILED" || "$deployment" == "Failed" ]]; then
      echo "RayJob ${name} failed: jobStatus=${status:-<empty>} jobDeploymentStatus=${deployment:-<empty>}" >&2
      return 1
    fi
    echo "waiting for RayJob ${name}: jobStatus=${status:-<empty>} jobDeploymentStatus=${deployment:-<empty>}"
    sleep 2
  done
  echo "timed out waiting for RayJob ${name} to succeed" >&2
  return 1
}

assert_ray_completion_marker() {
  local name="$1"
  local cluster head_pod
  cluster="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$name" -o jsonpath='{.status.rayClusterName}')"
  if [[ -z "$cluster" ]]; then
    echo "RayJob ${name} has no status.rayClusterName; cannot inspect Ray driver logs" >&2
    return 1
  fi
  head_pod="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get raycluster.ray.io "$cluster" -o jsonpath='{.status.head.podName}')"
  if [[ -z "$head_pod" ]]; then
    echo "RayCluster ${cluster} has no status.head.podName; cannot inspect Ray driver logs" >&2
    return 1
  fi
  if kubectl --request-timeout=20s --context "$KUBE_CONTEXT" -n "$NAMESPACE" exec "$head_pod" -- sh -c "grep -R '$RAY_COMPLETION_MARKER' /tmp/ray/session_latest/logs /tmp/ray/session_*/logs 2>/dev/null" >/dev/null; then
    echo "Ray driver logs contain completion marker: ${RAY_COMPLETION_MARKER}"
    return 0
  fi
  echo "Ray driver logs did not contain completion marker: ${RAY_COMPLETION_MARKER}" >&2
  return 1
}

wait_for_ray_cleanup() {
  local timeout="$1"
  local previous_ray_uid="${2:-}"
  local deadline remaining
  deadline=$((SECONDS + ${timeout%s}))
  while (( SECONDS < deadline )); do
    remaining="$(
      {
        kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$RAY_JOB_NAME" -o name 2>/dev/null || true
        kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get raycluster.ray.io -l "tau.azure.com/run-id=${RAY_JOB_NAME}" -o name 2>/dev/null || true
        kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get pod -l "tau.azure.com/run-id=${RAY_JOB_NAME}" -o name 2>/dev/null || true
        if [[ -n "$previous_ray_uid" ]]; then
          kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=${previous_ray_uid}" -o name 2>/dev/null || true
        fi
      } | sed '/^$/d'
    )"
    if [[ -z "$remaining" ]]; then
      echo "previous Ray resources are gone"
      return 0
    fi
    echo "waiting for previous Ray resources to disappear:"
    printf '%s\n' "$remaining"
    sleep 5
  done
  echo "timed out waiting for previous Ray resources to disappear" >&2
  printf '%s\n' "$remaining" >&2
  return 1
}

container_engine_preflight

if [[ "${TAU_KIND_RECREATE:-0}" == "1" ]]; then
  echo "Deleting existing Kind cluster ${CLUSTER_NAME} before recreate"
  run_with_timeout "$KIND_DELETE_TIMEOUT_SECONDS" kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
fi

echo "Checking Kind cluster ${CLUSTER_NAME}"
kind_clusters="$(run_with_timeout "$KIND_GET_TIMEOUT_SECONDS" kind get clusters)"
if ! grep -qx "$CLUSTER_NAME" <<<"$kind_clusters"; then
  echo "Creating Kind cluster ${CLUSTER_NAME}"
  run_with_timeout "$KIND_CREATE_TIMEOUT_SECONDS" kind create cluster --name "$CLUSTER_NAME" --config "$EXAMPLE_DIR/kind-cluster.yaml"
else
  echo "Reusing existing Kind cluster ${CLUSTER_NAME}"
fi
configure_kind_node_task_budget
DIAGNOSTICS_READY=1

if [[ "${TAU_KIND_SKIP_BUILD:-0}" != "1" ]]; then
  make -C "$TAU_DIR" build
fi

"$REPO_ROOT/scripts/ci/vendor-taugrid-dependencies.sh" \
  "$REPO_ROOT/charts/taugrid"

"$TAU_BIN" cluster install \
  --chart "$REPO_ROOT/charts/taugrid" \
  --version 0.2.3 \
  --release "$TAUGRID_RELEASE" \
  --namespace "$TAUGRID_NAMESPACE" \
  --context "$KUBE_CONTEXT" \
  --timeout 5m \
  --set components.tauCoreController.enabled=false \
  --set components.taugridCore.enabled=false

kubectl --context "$KUBE_CONTEXT" wait \
  --for=condition=Established \
  crd/rayjobs.ray.io \
  crd/rayclusters.ray.io \
  --timeout=120s

kubectl --context "$KUBE_CONTEXT" wait \
  --for=condition=Established \
  crd/clusterqueues.kueue.x-k8s.io \
  crd/localqueues.kueue.x-k8s.io \
  crd/workloads.kueue.x-k8s.io \
  --timeout=120s
kubectl --context "$KUBE_CONTEXT" -n "$TAUGRID_NAMESPACE" rollout status deploy/kuberay-operator --timeout=180s
kubectl --context "$KUBE_CONTEXT" -n "$TAUGRID_NAMESPACE" rollout status \
  "deploy/${TAUGRID_RELEASE}-kueue-controller-manager" --timeout=180s

# This test-only lane is workload fixture data, not cluster installation state.
kubectl --context "$KUBE_CONTEXT" apply -f "$EXAMPLE_DIR/kind-kueue-lanes.yaml"
kubectl --context "$KUBE_CONTEXT" get \
  resourceflavors.kueue.x-k8s.io,clusterqueues.kueue.x-k8s.io,workloadpriorityclasses.kueue.x-k8s.io

previous_ray_uid="$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$RAY_JOB_NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete job "$JOB_NAME" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete rayjob.ray.io "$RAY_JOB_NAME" --ignore-not-found
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete raycluster.ray.io -l "tau.azure.com/run-id=${RAY_JOB_NAME}" --ignore-not-found || true
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete pod -l "tau.azure.com/run-id=${RAY_JOB_NAME}" \
  --grace-period=0 --force --wait=false --ignore-not-found || true
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete configmap "${RAY_JOB_NAME}-script" --ignore-not-found
if [[ -n "$previous_ray_uid" ]]; then
  kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete workloads.kueue.x-k8s.io \
    -l "kueue.x-k8s.io/job-uid=${previous_ray_uid}" --ignore-not-found || true
fi
wait_for_ray_cleanup "$WAIT_TIMEOUT" "$previous_ray_uid"

"$TAU_BIN" run --config "$EXAMPLE_DIR/tau.yaml" --context "$KUBE_CONTEXT"

kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" wait --for=condition=complete "job/${JOB_NAME}" --timeout="$WAIT_TIMEOUT"
"$TAU_BIN" run status "$JOB_NAME" -n "$NAMESPACE" --context "$KUBE_CONTEXT"

logs="$("$TAU_BIN" run logs "$JOB_NAME" -n "$NAMESPACE" --context "$KUBE_CONTEXT" --tail=-1)"
printf '%s\n' "$logs"
if [[ "$logs" != *"tau kind smoke complete"* ]]; then
  echo "expected smoke completion marker was not present in job logs" >&2
  exit 1
fi

"$TAU_BIN" run --config "$EXAMPLE_DIR/tau-ray.yaml" --context "$KUBE_CONTEXT"
wait_for_workload_admitted "rayjob.ray.io" "$RAY_JOB_NAME" "$WAIT_TIMEOUT"
wait_for_rayjob_running "$RAY_JOB_NAME" "$RAY_WAIT_TIMEOUT"
if [[ "${TAU_KIND_RAY_WAIT_FOR_COMPLETION:-0}" == "1" ]]; then
  wait_for_rayjob_succeeded "$RAY_JOB_NAME" "$RAY_WAIT_TIMEOUT"
  assert_ray_completion_marker "$RAY_JOB_NAME"
  wait_for_workload_finished "rayjob.ray.io" "$RAY_JOB_NAME" "$WAIT_TIMEOUT"
fi
kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$RAY_JOB_NAME" -o wide
kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=$(kubectl --request-timeout=10s --context "$KUBE_CONTEXT" -n "$NAMESPACE" get rayjob.ray.io "$RAY_JOB_NAME" -o jsonpath='{.metadata.uid}')" -o wide

echo "Tau Kind smoke passed on context ${KUBE_CONTEXT} (batch Job + Kueue + KubeRay RayJob)"

if [[ "${TAU_KIND_DELETE_CLUSTER:-0}" == "1" ]]; then
  kind delete cluster --name "$CLUSTER_NAME"
fi
