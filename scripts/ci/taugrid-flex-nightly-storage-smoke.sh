#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly NAMESPACE="${FLEX_NIGHTLY_STORAGE_NAMESPACE:-}"
readonly PVC="${FLEX_NIGHTLY_STORAGE_PVC:-}"
readonly STORAGE_ACCOUNT="${FLEX_NIGHTLY_STORAGE_ACCOUNT:-}"
readonly STORAGE_REGION="${FLEX_NIGHTLY_STORAGE_ACCOUNT_REGION:-}"
readonly A100_SITE="${FLEX_NIGHTLY_A100_SITE:-}"
readonly H200_SITE="${FLEX_NIGHTLY_H200_SITE:-}"
readonly A100_REGION="${FLEX_NIGHTLY_A100_REGION:-}"
readonly H200_REGION="${FLEX_NIGHTLY_H200_REGION:-}"
readonly IMAGE="${RAY_E2E_IMAGE:-}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_STORAGE_ARTIFACT_DIR:-}"
readonly BUILD_ID="${BUILD_BUILDID:-local}"
readonly ATTEMPT="${SYSTEM_JOBATTEMPT:-1}"
readonly RUN_ID="${BUILD_ID}-${ATTEMPT}"
readonly A100_JOB="tau-storage-a100-${RUN_ID}"
readonly H200_JOB="tau-storage-h200-${RUN_ID}"
readonly VERIFY_JOB="tau-storage-verify-${RUN_ID}"
readonly CLEANUP_JOB="tau-storage-cleanup-${RUN_ID}"
readonly DATA_DIR="/data/taugrid-nightly/${RUN_ID}"
readonly RESULT_FILE="${ARTIFACT_DIR}/storage-results.jsonl"
readonly CLEANUP_FILE="${ARTIFACT_DIR}/storage-cleanup-result.json"

fail() {
  echo "TauGrid Flex storage smoke failed: $*" >&2
  exit 1
}

require_value() {
  local name="$1"
  local value="${!name:-}"
  [ -n "$value" ] || fail "${name} is required"
  [[ "$value" != '$('* ]] || fail "${name} is unresolved (${value})"
}

for name in NAMESPACE PVC STORAGE_ACCOUNT STORAGE_REGION A100_SITE H200_SITE \
  A100_REGION H200_REGION IMAGE ARTIFACT_DIR; do
  require_value "$name"
done
command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v az >/dev/null 2>&1 || fail "az is required"

mkdir -p "${ARTIFACT_DIR}/diagnostics"
: >"$RESULT_FILE"
data_created=false

record_result() {
  local target="$1"
  local status="$2"
  local reason="${3:-}"
  jq -cn \
    --arg target "$target" \
    --arg status "$status" \
    --arg reason "$reason" \
    '{target:$target,status:$status,reason:$reason}' >>"$RESULT_FILE"
}

apply_job() {
  local name="$1"
  local selector="$2"
  local site="$3"
  local region="$4"
  local script="$5"
  local selector_key="${selector%%=*}"
  local selector_value="${selector#*=}"
  local script_b64
  local manifest="${ARTIFACT_DIR}/diagnostics/${name}.yaml"
  script_b64="$(printf '%s' "$script" | base64 | tr -d '\n')"

  cat >"$manifest" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    tau.azure.com/e2e-run: "${RUN_ID}"
    tau.azure.com/e2e-storage: "true"
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 600
  template:
    metadata:
      labels:
        tau.azure.com/e2e-run: "${RUN_ID}"
        tau.azure.com/e2e-storage: "true"
    spec:
      restartPolicy: Never
      nodeSelector:
        ${selector_key}: ${selector_value}
        tau.azure.com/site: ${site}
        topology.kubernetes.io/region: ${region}
      tolerations:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      containers:
        - name: storage
          image: ${IMAGE}
          command: ["/bin/bash", "-lc"]
          args:
            - echo '${script_b64}' | base64 -d | /bin/bash
          resources:
            requests:
              nvidia.com/gpu: "1"
            limits:
              nvidia.com/gpu: "1"
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: ${PVC}
EOF
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" apply -f "$manifest"
}

run_job() {
  local target="$1"
  local name="$2"
  local selector="$3"
  local site="$4"
  local region="$5"
  local script="$6"
  apply_job "$name" "$selector" "$site" "$region" "$script"
  if ! "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    wait --for=condition=complete "job/${name}" --timeout=12m; then
    record_result "$target" failed "Job did not complete"
    return 1
  fi
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    logs "job/${name}" >"${ARTIFACT_DIR}/diagnostics/${target}.log" 2>&1
  record_result "$target" passed
}

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  local status=passed
  local reason=""

  if [[ "$data_created" == "true" ]]; then
    apply_job "$CLEANUP_JOB" "kueue.azure.com/gpu-series=ndm-a100-v4" \
      "$A100_SITE" "$A100_REGION" \
      "rm -rf '${DATA_DIR}' && echo storage-cleanup-complete" \
      >"${ARTIFACT_DIR}/diagnostics/cleanup-apply.log" 2>&1
    if ! "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
      wait --for=condition=complete "job/${CLEANUP_JOB}" --timeout=12m \
      >"${ARTIFACT_DIR}/diagnostics/cleanup-wait.log" 2>&1; then
      status=failed
      reason="failed to remove nightly Blob test data"
      rc=1
    fi
  fi
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get jobs,pods -l "tau.azure.com/e2e-run=${RUN_ID}" -o wide \
    >"${ARTIFACT_DIR}/diagnostics/resources.txt" 2>&1
  "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get events --sort-by=.lastTimestamp \
    >"${ARTIFACT_DIR}/diagnostics/events.txt" 2>&1
  if ! "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" delete job \
    "$A100_JOB" "$H200_JOB" "$VERIFY_JOB" "$CLEANUP_JOB" \
    --ignore-not-found --wait=true --timeout=15m; then
    status=failed
    reason="failed to delete storage smoke Jobs"
    rc=1
  fi
  if "$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
    get jobs -l "tau.azure.com/e2e-run=${RUN_ID}" -o name |
    grep -q .; then
    status=failed
    reason="storage smoke Jobs remain after cleanup"
    rc=1
  fi
  jq -n \
    --arg status "$status" \
    --arg reason "$reason" \
    '{status:$status,reason:$reason}' >"$CLEANUP_FILE"
  exit "$rc"
}
trap cleanup EXIT

actual_region="$(az storage account show --name "$STORAGE_ACCOUNT" --query location -o tsv)"
[[ "${actual_region,,}" == "${STORAGE_REGION,,}" ]] ||
  fail "storage account ${STORAGE_ACCOUNT} is in ${actual_region}, expected ${STORAGE_REGION}"
"$KUBECTL_BIN" --context "$KUBE_CONTEXT" get csidriver blob.csi.azure.com >/dev/null
[[ "$("$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
  get pvc "$PVC" -o jsonpath='{.status.phase}')" == "Bound" ]] ||
  fail "PVC ${NAMESPACE}/${PVC} is not Bound"

data_created=true
run_job a100-write "$A100_JOB" "kueue.azure.com/gpu-series=ndm-a100-v4" \
  "$A100_SITE" "$A100_REGION" \
  "mkdir -p '${DATA_DIR}' && printf 'a100-${RUN_ID}' >'${DATA_DIR}/a100.txt' && sync && cat '${DATA_DIR}/a100.txt'"
run_job h200-read-write "$H200_JOB" "kueue.azure.com/gpu-series=nd-h200-v5" \
  "$H200_SITE" "$H200_REGION" \
  "test \"\$(cat '${DATA_DIR}/a100.txt')\" = 'a100-${RUN_ID}' && printf 'h200-${RUN_ID}' >'${DATA_DIR}/h200.txt' && sync && cat '${DATA_DIR}/h200.txt'"
run_job a100-readback "$VERIFY_JOB" "kueue.azure.com/gpu-series=ndm-a100-v4" \
  "$A100_SITE" "$A100_REGION" \
  "test \"\$(cat '${DATA_DIR}/h200.txt')\" = 'h200-${RUN_ID}' && echo storage-cross-region-complete"

echo "TauGrid Flex storage smoke passed"
