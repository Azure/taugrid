#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

verify_ib_queue_contract() {
  local queue="$1" flavor="$2" directory="$3" cohort
  kubectl --request-timeout=30s get clusterqueue "${queue}" -o json >"${directory}/ib-clusterqueue.json"
  cohort="$(jq -er '.spec.cohortName // .spec.cohort // ""' "${directory}/ib-clusterqueue.json")"
  printf '{}\n' >"${directory}/ib-cohort.json"
  if [[ -n "${cohort}" ]]; then
    kubectl --request-timeout=30s get cohort "${cohort}" -o json >"${directory}/ib-cohort.json"
  fi
  if ! jq -en --arg resource rdma/rdma_shared_device_a --arg flavor "${flavor}" \
    --slurpfile queues "${directory}/ib-clusterqueue.json" \
    --slurpfile cohorts "${directory}/ib-cohort.json" '
      def units:
        tostring | if test("^[0-9]+$") then tonumber else error("invalid RDMA quota quantity") end;
      def quota($object):
        [$object.spec.resourceGroups[]?
          | select(any(.coveredResources[]?; . == $resource))
          | .flavors[]? | select(.name == $flavor)
          | .resources[]? | select(.name == $resource)];
      $queues[0] as $queue | $cohorts[0] as $cohort
      | quota($queue) as $quotas | quota($cohort) as $cohortQuotas
      | (if ($quotas | length) != 1 then
          error("selected ClusterQueue must cover RDMA on the selected H200 flavor")
        else $quotas[0] end) as $quota
      | ($quota.nominalQuota | units) as $nominal
      | if ($cohort | length) == 0 then
          if $nominal < 16 then error("ClusterQueue nominal RDMA quota is below 16") else true end
        elif ($cohort.spec.parentName // "") != "" then
          error("nested RDMA cohort capacity requires separate qualification")
        elif ($cohortQuotas | length) != 1 then
          error("selected Cohort must cover RDMA on the selected H200 flavor")
        else
          ($cohortQuotas[0].nominalQuota | units) as $pool
          | ($quota.borrowingLimit // $pool | units) as $borrowing
          | if $nominal + ([$borrowing, $pool] | min) < 16 then
              error("declared ClusterQueue/Cohort RDMA quota cannot admit 16 workers")
            else true end
        end
    ' >"${directory}/ib-queue-contract.json"; then
    echo "IB admission contract failed for selected ClusterQueue/Cohort; declare RDMA coverage and quota through their authoritative configuration" >&2
    return 1
  fi
}

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
contract="${1:?hardware contract is required}"
artifact_dir="${2:?artifact directory is required}"
run_id="${3:?run identity is required}"
[[ "${run_id}" =~ ^[0-9]+-[0-9]+$ ]] || { echo "Invalid nightly IB run identity" >&2; exit 1; }
[[ "${contract}" == /* && "${artifact_dir}" == /* ]] || {
  echo "Nightly IB contract and artifact paths must be absolute" >&2
  exit 1
}
if [[ -n "${AI_RUNTIME_E2E_KUBE_CONTEXT:-}" &&
      "$(kubectl config current-context)" != "${AI_RUNTIME_E2E_KUBE_CONTEXT}" ]]; then
  echo "IB shell lane requires its pipeline kubeconfig context; use the focused Go test for a context override" >&2
  exit 1
fi
namespace="taugrid-nightly-matrix-${run_id}"
bundle_dir="${artifact_dir}/diagnostics"
mkdir -p "${bundle_dir}"
test -s "${contract}"

cleanup() {
  rc=$?
  trap - EXIT
  set +e
  kubectl --request-timeout=30s get rayjobs,rayclusters,workloads,localqueues,pods,jobs \
    -n "${namespace}" -o yaml >"${bundle_dir}/resources.yaml" 2>&1
  kubectl --request-timeout=30s get events -n "${namespace}" \
    --sort-by=.lastTimestamp >"${bundle_dir}/events.txt" 2>&1
  cleanup_status=passed
  capability_status=failed
  if ! python3 "${repo_root}/scripts/ci/taugrid-flex-nightly-cleanup.py" \
    cleanup "${namespace}" "${run_id}" "${bundle_dir}/namespace-owner.json" \
    >"${bundle_dir}/owned-cleanup-status.json"; then
    cleanup_status=failed
    rc=1
  elif FLEX_NIGHTLY_CONTRACT_FILE="${bundle_dir}/postflight-hardware-contract.json" \
    "${repo_root}/scripts/ci/taugrid-flex-nightly-preflight.sh" cluster &&
    "${repo_root}/scripts/ci/taugrid-flex-nightly-preflight.sh" compare-capability \
      "${contract}" "${bundle_dir}/postflight-hardware-contract.json"; then
    capability_status=passed
  else
    echo "Nightly IB postflight capability verification failed" >&2
    rc=1
  fi
  jq -n --arg cleanup "${cleanup_status}" --arg capability "${capability_status}" \
    '{cleanup:{status:$cleanup},capability:{status:$capability}}' \
    >"${bundle_dir}/postflight-status.json" || rc=1
  exit "${rc}"
}

FLEX_NIGHTLY_CONTRACT_FILE="${bundle_dir}/preflight-hardware-contract.json" \
  "${repo_root}/scripts/ci/taugrid-flex-nightly-preflight.sh" cluster
"${repo_root}/scripts/ci/taugrid-flex-nightly-preflight.sh" compare-capability \
  "${contract}" "${bundle_dir}/preflight-hardware-contract.json"
if ! h200="$(jq -ce '.targets[] | select(
  .name == "h200" and .status == "available" and .gpu_available >= 16
  and (.nodes | length) == 2
  and all(.nodes[]; .gpu_available >= 8 and .rdma_allocatable >= 8)
)' "${bundle_dir}/preflight-hardware-contract.json")"; then
  echo "IB requires exactly two available eight-GPU hosts with at least eight RDMA units each" >&2
  exit 1
fi
cluster_queue="$(jq -er '.cluster_queue' <<<"${h200}")"
flavor="$(jq -er '.flavor' <<<"${h200}")"
verify_ib_queue_contract "${cluster_queue}" "${flavor}" "${bundle_dir}"
trap cleanup EXIT
kubectl create namespace "${namespace}"
kubectl label namespace "${namespace}" \
  "tau.azure.com/workspace=${FLEX_NIGHTLY_WORKSPACE:?workspace is required}" \
  tau.azure.com/gpu-queue=enabled "tau.azure.com/e2e-run=${run_id}"
python3 "${repo_root}/scripts/ci/taugrid-flex-nightly-cleanup.py" \
  record "${namespace}" "${run_id}" "${bundle_dir}/namespace-owner.json"
cat <<EOF | kubectl apply -f -
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: matrix-ib
  namespace: ${namespace}
  labels:
    tau.azure.com/e2e-run: "${run_id}"
spec:
  clusterQueue: ${cluster_queue}
EOF

selector="$(jq -er '.selector' <<<"${h200}")"
export MATRIX_GPU_SELECTOR="${selector}"
export MATRIX_SITE="$(jq -er '.expected_site' <<<"${h200}")"
export MATRIX_NAMESPACE="${namespace}"
export MATRIX_QUEUE=matrix-ib
export MATRIX_TARGET=h200
export MATRIX_WORKERS=16
export MATRIX_EXPECTED_HOSTS=2
export MATRIX_EXPECTED_GPUS_PER_HOST=8
export MATRIX_REQUIRED_TOPOLOGY=tau.azure.com/site
export AI_RUNTIME_E2E=1 E2E_HARDWARE_MATRIX=1 E2E_HARDWARE_IB=1
export E2E_BUNDLE_DIR="${bundle_dir}"
export E2E_RESULT_FILE="${artifact_dir}/e2e-results.jsonl"
unset MATRIX_SCHEDULER_RECOVERY_NODE
cd "${repo_root}/tests/e2e"
go test -v -count=1 -timeout 50m -run '^TestRayIBGPU$' ./hardwarematrix/
