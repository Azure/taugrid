#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
LARGE_GPU_RAYJOB_NAME="${LARGE_GPU_RAYJOB_NAME:-e2e-nanogpt-large-gpu}"

fail() {
  echo "::error::$*" >&2
  exit 1
}

warn() {
  echo "::warning::$*" >&2
}

is_truthy() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

write_output() {
  local name="$1"
  local value="$2"
  [ -n "${GITHUB_OUTPUT:-}" ] || fail "GITHUB_OUTPUT is required to write '$name'"
  case "$value" in
    *$'\n'*|*$'\r'*)
      fail "Refusing to write multi-line output '$name'"
      ;;
  esac
  printf '%s=%s\n' "$name" "$value" >>"$GITHUB_OUTPUT"
}

write_optional_output() {
  local name="$1"
  local value="$2"
  [ -n "${GITHUB_OUTPUT:-}" ] || return 0
  write_output "$name" "$value"
}

scheduled_maintenance_state() {
  command -v python3 >/dev/null 2>&1 || fail "python3 is required to validate the scheduled maintenance window"
  python3 - <<'PY'
from datetime import datetime, timedelta, timezone
import os
import re
import sys

timestamp = os.environ.get("TAU_QUEUE_MAINTENANCE_UNTIL", "")
if not timestamp:
    print("run")
    raise SystemExit(0)

timestamp_pattern = r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"
if re.fullmatch(timestamp_pattern, timestamp) is None:
    print(
        "::error::TAU_QUEUE_MAINTENANCE_UNTIL must use canonical UTC RFC3339 YYYY-MM-DDTHH:MM:SSZ",
        file=sys.stderr,
    )
    raise SystemExit(1)

try:
    until = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
except ValueError:
    print(
        "::error::TAU_QUEUE_MAINTENANCE_UNTIL is not a valid UTC timestamp",
        file=sys.stderr,
    )
    raise SystemExit(1)

now = datetime.now(timezone.utc)
if until - now > timedelta(hours=4):
    print(
        "::error::TAU_QUEUE_MAINTENANCE_UNTIL must be no more than four hours in the future",
        file=sys.stderr,
    )
    raise SystemExit(1)

if until <= now:
    print("run")
    raise SystemExit(0)

change = os.environ.get("TAU_QUEUE_MAINTENANCE_CHANGE", "")
change_pattern = r"[A-Za-z0-9#](?:[A-Za-z0-9 ._:/#@+-]{0,198}[A-Za-z0-9#])?"
if (
    not change
    or len(change) > 200
    or change != change.strip()
    or re.fullmatch(change_pattern, change) is None
    or "::" in change
):
    print(
        "::error::TAU_QUEUE_MAINTENANCE_CHANGE must be a non-empty single-line printable change or reason reference (up to 200 characters)",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(f"::notice::Holding scheduled large-GPU conformance until {timestamp} for {change}", file=sys.stderr)
print("hold")
PY
}

require_env() {
  local name="$1"
  local value
  value="$(printenv "$name" || true)"
  [ -n "$value" ] || fail "$name is required"
  printf '%s' "$value"
}

parse_selector() {
  local expr="$1"
  local output_prefix="$2"
  local description="$3"
  local target="$4"
  local key="${expr%%=*}"
  local value="${expr#*=}"

  if [ -z "$expr" ] || [ -z "$key" ] || [ -z "$value" ] || [ "$key" = "$expr" ]; then
    fail "No valid ${description} selector configured for ${target} (selector='$expr')"
  fi

  write_output "${output_prefix}_key" "$key"
  write_output "${output_prefix}_value" "$value"
  write_output "${output_prefix}_expr" "$key=$value"
}

require_argocd_workload_queue_contract() {
  local name
  if ! is_truthy "${E2E_STACK_USE_ARGOCD_QUEUE:-}"; then
    fail "E2E_STACK_USE_ARGOCD_QUEUE=1 is required for large-GPU workload commands; refusing stack-kueue-resources or namespace management"
  fi
  for name in E2E_STACK_NAMESPACE E2E_STACK_QUEUE E2E_STACK_LARGE_GPU_QUEUE; do
    require_env "$name" >/dev/null
  done
}

resolved_workload_namespace() {
  require_argocd_workload_queue_contract || return 1
  require_env E2E_STACK_NAMESPACE
}

diagnostic_argocd_workload_queue_available() {
  local name
  if ! is_truthy "${E2E_STACK_USE_ARGOCD_QUEUE:-}"; then
    warn "workload diagnostics unavailable: E2E_STACK_USE_ARGOCD_QUEUE=1 is required"
    return 1
  fi
  for name in E2E_STACK_NAMESPACE E2E_STACK_QUEUE E2E_STACK_LARGE_GPU_QUEUE; do
    if [ -z "$(printenv "$name" || true)" ]; then
      warn "workload diagnostics unavailable: ${name} is required"
      return 1
    fi
  done
  return 0
}

require_workload_access_mode() {
  local mode
  mode="$(require_env LARGE_GPU_WORKLOAD_ACCESS_MODE)"
  case "$mode" in
    platform-direct|manager)
      ;;
    *)
      fail "LARGE_GPU_WORKLOAD_ACCESS_MODE must be 'platform-direct' or 'manager' (got '${mode}')"
      ;;
  esac
  printf '%s' "$mode"
}

load_kube_access_view() {
  local label="$1"
  local kubeconfig_var="$2"
  local context_var="$3"
  local kubeconfig context view
  kubeconfig="$(require_env "$kubeconfig_var")"
  context="$(require_env "$context_var")"
  [ -f "$kubeconfig" ] || fail "${label} kubeconfig does not exist: ${kubeconfig_var}=${kubeconfig}"
  view="$(kubectl --kubeconfig "$kubeconfig" --context "$context" config view --raw --minify -o json 2>/dev/null || true)"
  [ -n "$view" ] || fail "Could not resolve ${label} context ${context} from ${kubeconfig_var}=${kubeconfig}"
  printf '%s' "$view"
}

validate_kube_access_contract() {
  local label="$1"
  local kubeconfig_var="$2"
  local context_var="$3"
  local view
  view="$(load_kube_access_view "$label" "$kubeconfig_var" "$context_var")"
  echo "$view" | jq -e '((.clusters // []) | length) > 0 and ((.users // []) | length) > 0' >/dev/null \
    || fail "Context $(require_env "$context_var") from $(require_env "$kubeconfig_var") does not resolve a usable cluster/user pair"
}

diagnostic_kube_access_available() {
  local label="$1"
  local kubeconfig_var="$2"
  local context_var="$3"
  local kubeconfig context view
  kubeconfig="$(printenv "$kubeconfig_var" || true)"
  context="$(printenv "$context_var" || true)"
  if [ -z "$kubeconfig" ] || [ -z "$context" ]; then
    warn "${label} diagnostics unavailable: ${kubeconfig_var} and ${context_var} are required"
    return 1
  fi
  if [ ! -f "$kubeconfig" ]; then
    warn "${label} diagnostics unavailable: ${kubeconfig_var}=${kubeconfig} does not exist"
    return 1
  fi
  view="$(kubectl --kubeconfig "$kubeconfig" --context "$context" config view --raw --minify -o json 2>/dev/null || true)"
  if [ -z "$view" ]; then
    warn "${label} diagnostics unavailable: could not resolve context ${context} from ${kubeconfig_var}=${kubeconfig}"
    return 1
  fi
  if ! echo "$view" | jq -e '((.clusters // []) | length) > 0 and ((.users // []) | length) > 0' >/dev/null 2>&1; then
    warn "${label} diagnostics unavailable: context ${context} from ${kubeconfig_var}=${kubeconfig} does not resolve a usable cluster/user pair"
    return 1
  fi
  return 0
}

kube_access_fingerprint() {
  local kubeconfig_var="$1"
  local context_var="$2"
  local fingerprint
  fingerprint="$(load_kube_access_view "fingerprint" "$kubeconfig_var" "$context_var" | jq -c '{cluster: (.clusters[0].cluster // {}), user: (.users[0].user // {})}')"
  [ -n "$fingerprint" ] || fail "Could not derive an access fingerprint from ${kubeconfig_var}=$(require_env "$kubeconfig_var")"
  printf '%s' "$fingerprint"
}

kube_credential_fingerprint() {
  local kubeconfig_var="$1"
  local context_var="$2"
  local fingerprint
  fingerprint="$(load_kube_access_view "credential-fingerprint" "$kubeconfig_var" "$context_var" | jq -c '(.users[0].user // {})')"
  [ -n "$fingerprint" ] || fail "Could not derive a credential fingerprint from ${kubeconfig_var}=$(require_env "$kubeconfig_var")"
  printf '%s' "$fingerprint"
}

kube_cluster_identity() {
  local kubeconfig_var="$1"
  local context_var="$2"
  local identity
  identity="$(load_kube_access_view "cluster-identity" "$kubeconfig_var" "$context_var" |
    jq -er '.clusters[0].cluster.server | select(type == "string" and length > 0) | ascii_downcase | sub("/+$"; "") | select(length > 0)')" \
    || fail "Could not derive a normalized cluster API server from ${kubeconfig_var}=$(require_env "$kubeconfig_var")"
  printf '%s' "$identity"
}

platform_kubeconfig() {
  require_env LARGE_GPU_PLATFORM_KUBECONFIG
}

platform_context() {
  require_env LARGE_GPU_PLATFORM_KUBE_CONTEXT
}

workload_kubeconfig() {
  require_env LARGE_GPU_WORKLOAD_KUBECONFIG
}

workload_context() {
  require_env LARGE_GPU_WORKLOAD_KUBE_CONTEXT
}

assert_platform_access_contract() {
  validate_kube_access_contract "platform" LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT
}

assert_workload_access_contract() {
  local mode platform_access workload_access platform_credential workload_credential
  local platform_cluster workload_cluster platform_kubeconfig_value workload_kubeconfig_value
  mode="$(require_workload_access_mode)" || return 1
  validate_kube_access_contract "platform" LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT || return 1
  validate_kube_access_contract "workload" LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT || return 1
  platform_access="$(kube_access_fingerprint LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT)" || return 1
  workload_access="$(kube_access_fingerprint LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT)" || return 1
  platform_credential="$(kube_credential_fingerprint LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT)" || return 1
  workload_credential="$(kube_credential_fingerprint LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT)" || return 1
  platform_cluster="$(kube_cluster_identity LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT)" || return 1
  workload_cluster="$(kube_cluster_identity LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT)" || return 1
  case "$mode" in
    platform-direct)
      [ "$workload_access" = "$platform_access" ] || fail "LARGE_GPU_WORKLOAD_ACCESS_MODE=platform-direct requires workload access to reuse the direct platform access fingerprint"
      ;;
    manager)
      platform_kubeconfig_value="$(platform_kubeconfig)" || return 1
      workload_kubeconfig_value="$(workload_kubeconfig)" || return 1
      [ "$workload_kubeconfig_value" != "$platform_kubeconfig_value" ] || fail "LARGE_GPU_WORKLOAD_ACCESS_MODE=manager requires a separately materialized workload kubeconfig; refusing the direct platform kubeconfig"
      [ "$workload_credential" != "$platform_credential" ] || fail "LARGE_GPU_WORKLOAD_ACCESS_MODE=manager requires a credential fingerprint distinct from the direct platform access contract; refusing silent fallback to direct worker credentials"
      [ "$workload_cluster" != "$platform_cluster" ] || fail "LARGE_GPU_WORKLOAD_ACCESS_MODE=manager requires workload access to resolve to a manager cluster distinct from the direct platform worker cluster; both contexts resolve to ${platform_cluster}"
      ;;
  esac
}

assert_workload_execution_contract() {
  local mode expected_platform_cluster workload_cluster
  mode="$(require_workload_access_mode)" || return 1
  validate_kube_access_contract "workload" LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT || return 1
  expected_platform_cluster="$(require_env LARGE_GPU_PLATFORM_CLUSTER_IDENTITY)" || return 1
  workload_cluster="$(kube_cluster_identity LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT)" || return 1
  case "$mode" in
    platform-direct)
      [ "$workload_cluster" = "$expected_platform_cluster" ] || fail "platform-direct workload access no longer resolves to the validated direct platform cluster"
      ;;
    manager)
      [ "$workload_cluster" != "$expected_platform_cluster" ] || fail "manager workload execution resolves to the direct platform worker cluster; refusing submission"
      ;;
  esac
  require_argocd_workload_queue_contract
}

platform_kubectl() {
  kubectl --kubeconfig "$(platform_kubeconfig)" --context "$(platform_context)" "$@"
}

workload_kubectl() {
  kubectl --kubeconfig "$(workload_kubeconfig)" --context "$(workload_context)" "$@"
}

resolve_access_contract() {
  local workload_access_mode workload_kubeconfig_value workload_context_value
  local workload_stack_namespace workload_stack_queue workload_stack_large_gpu_queue
  local platform_cluster_identity

  LARGE_GPU_PLATFORM_KUBECONFIG="$(require_env DIRECT_PLATFORM_KUBECONFIG)" || return 1
  LARGE_GPU_PLATFORM_KUBE_CONTEXT="$(require_env DIRECT_PLATFORM_CONTEXT)" || return 1
  export LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT

  workload_access_mode="${INPUT_WORKLOAD_ACCESS_MODE:-${DEFAULT_WORKLOAD_ACCESS_MODE:-platform-direct}}"
  case "$workload_access_mode" in
    platform-direct)
      echo "::notice::Large-GPU workload access is temporarily using direct platform access; no scoped researcher/manager credential exists yet."
      workload_kubeconfig_value="$LARGE_GPU_PLATFORM_KUBECONFIG"
      workload_context_value="$LARGE_GPU_PLATFORM_KUBE_CONTEXT"
      ;;
    manager)
      workload_kubeconfig_value="${MANAGER_WORKLOAD_KUBECONFIG:-}"
      workload_context_value="${MANAGER_WORKLOAD_CONTEXT:-}"
      if [ -z "$workload_kubeconfig_value" ] || [ -z "$workload_context_value" ]; then
        fail "workload_access_mode=manager requires a distinct manager workload kubeconfig/context. Materialize them into MANAGER_WORKLOAD_KUBECONFIG and MANAGER_WORKLOAD_CONTEXT before enabling manager mode; direct platform credentials will not be reused."
      fi
      ;;
    *)
      fail "workload_access_mode must be platform-direct or manager (got '${workload_access_mode}')"
      ;;
  esac

  workload_stack_namespace="${INPUT_WORKLOAD_STACK_NAMESPACE:-${DEFAULT_WORKLOAD_STACK_NAMESPACE:-taugrid-e2e}}"
  workload_stack_queue="${INPUT_WORKLOAD_STACK_QUEUE:-${DEFAULT_WORKLOAD_STACK_QUEUE:-jobqueue}}"
  workload_stack_large_gpu_queue="${INPUT_WORKLOAD_STACK_LARGE_GPU_QUEUE:-${DEFAULT_WORKLOAD_STACK_LARGE_GPU_QUEUE:-jobqueue}}"

  export LARGE_GPU_WORKLOAD_ACCESS_MODE="$workload_access_mode"
  export LARGE_GPU_WORKLOAD_KUBECONFIG="$workload_kubeconfig_value"
  export LARGE_GPU_WORKLOAD_KUBE_CONTEXT="$workload_context_value"
  export E2E_STACK_USE_ARGOCD_QUEUE=1
  export E2E_STACK_NAMESPACE="$workload_stack_namespace"
  export E2E_STACK_QUEUE="$workload_stack_queue"
  export E2E_STACK_LARGE_GPU_QUEUE="$workload_stack_large_gpu_queue"

  assert_workload_access_contract
  require_argocd_workload_queue_contract
  platform_cluster_identity="$(kube_cluster_identity LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT)" || return 1

  write_output "workload_access_mode" "$workload_access_mode"
  write_output "platform_kubeconfig" "$LARGE_GPU_PLATFORM_KUBECONFIG"
  write_output "platform_context" "$LARGE_GPU_PLATFORM_KUBE_CONTEXT"
  write_output "platform_cluster_identity" "$platform_cluster_identity"
  write_output "workload_kubeconfig" "$LARGE_GPU_WORKLOAD_KUBECONFIG"
  write_output "workload_context" "$LARGE_GPU_WORKLOAD_KUBE_CONTEXT"
  write_output "workload_stack_use_argocd_queue" "$E2E_STACK_USE_ARGOCD_QUEUE"
  write_output "workload_stack_namespace" "$E2E_STACK_NAMESPACE"
  write_output "workload_stack_queue" "$E2E_STACK_QUEUE"
  write_output "workload_stack_large_gpu_queue" "$E2E_STACK_LARGE_GPU_QUEUE"
}

resolve_target_config() {
  local event_name maintenance_state
  event_name="${GITHUB_EVENT_NAME:-${EVENT_NAME:-}}"
  if [ "$event_name" = "schedule" ]; then
    maintenance_state="$(scheduled_maintenance_state)" || return 1
    if [ "$maintenance_state" = "hold" ]; then
      write_output "should_run" "false"
      return 0
    fi
  fi

  local matrix_target matrix_sku matrix_run_on_schedule matrix_default_cluster matrix_default_gpu_selector matrix_gpu_series
  matrix_target="$(require_env MATRIX_TARGET)"
  matrix_sku="$(require_env MATRIX_SKU)"
  matrix_run_on_schedule="${MATRIX_RUN_ON_SCHEDULE:-false}"
  matrix_default_cluster="$(require_env MATRIX_DEFAULT_CLUSTER)"
  matrix_default_gpu_selector="$(require_env MATRIX_DEFAULT_GPU_SELECTOR)"
  matrix_gpu_series="$(require_env MATRIX_GPU_SERIES)"

  local requested_target workload_profile should_run
  requested_target="${INPUT_TARGET:-all}"
  workload_profile="${INPUT_WORKLOAD_PROFILE:-conformance}"

  if [ "$event_name" = "schedule" ]; then
    requested_target="scheduled"
    workload_profile="conformance"
  fi

  should_run="true"
  case "$requested_target" in
    scheduled)
      if [ "$matrix_run_on_schedule" != "true" ]; then
        should_run="false"
      fi
      ;;
    all)
      ;;
    h200)
      if [ "$matrix_sku" != "h200" ]; then
        should_run="false"
      fi
      ;;
    eastus2-h200|flex-managed-a100|flex-h200)
      if [ "$requested_target" != "$matrix_target" ]; then
        should_run="false"
      fi
      ;;
    *)
      fail "Unknown target input '$requested_target'"
      ;;
  esac

  if [ "$should_run" != "true" ]; then
    write_output "should_run" "false"
    echo "Skipping ${matrix_target} for event=${event_name} target=${requested_target}"
    return 0
  fi

  local train_steps min_total_tokens report_every
  case "$workload_profile" in
    smoke)
      train_steps="${NANOGPT_SMOKE_TRAIN_STEPS:-}"
      [ -n "$train_steps" ] || train_steps="1"
      min_total_tokens="${NANOGPT_SMOKE_MIN_TOTAL_TOKENS:-}"
      [ -n "$min_total_tokens" ] || min_total_tokens="1"
      report_every="${NANOGPT_SMOKE_REPORT_EVERY:-}"
      [ -n "$report_every" ] || report_every="1"
      ;;
    conformance)
      train_steps="${NANOGPT_TRAIN_STEPS:-}"
      [ -n "$train_steps" ] || train_steps="2000"
      min_total_tokens="${NANOGPT_MIN_TOTAL_TOKENS:-}"
      [ -n "$min_total_tokens" ] || min_total_tokens="100000000"
      report_every="${NANOGPT_REPORT_EVERY:-}"
      [ -n "$report_every" ] || report_every="25"
      ;;
    *)
      fail "Unknown workload_profile input '$workload_profile'"
      ;;
  esac

  local rg cluster gpu_selector submitter_selector nccl_ib_disable
  case "$matrix_target" in
    eastus2-h200)
      rg="${INPUT_EASTUS2_RESOURCE_GROUP:-}"
      [ -n "$rg" ] || rg="${AKS_AI_RUNTIME_EASTUS2_RESOURCE_GROUP:-}"
      cluster="${INPUT_EASTUS2_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="${AKS_AI_RUNTIME_EASTUS2_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="$matrix_default_cluster"
      gpu_selector="${AKS_AI_RUNTIME_EASTUS2_H200_SELECTOR:-}"
      submitter_selector="${AKS_AI_RUNTIME_EASTUS2_SUBMITTER_SELECTOR:-}"
      nccl_ib_disable="${AKS_AI_RUNTIME_EASTUS2_H200_NCCL_IB_DISABLE:-}"
      ;;
    flex-managed-a100)
      rg="${INPUT_FLEX_RESOURCE_GROUP:-}"
      [ -n "$rg" ] || rg="${AKS_AI_RUNTIME_FLEX_RESOURCE_GROUP:-}"
      cluster="${INPUT_FLEX_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="${AKS_AI_RUNTIME_FLEX_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="$matrix_default_cluster"
      gpu_selector="${AKS_AI_RUNTIME_FLEX_MANAGED_A100_SELECTOR:-}"
      [ -n "$gpu_selector" ] || gpu_selector="${AKS_AI_RUNTIME_FLEX_A100_SELECTOR:-}"
      submitter_selector="${AKS_AI_RUNTIME_FLEX_SUBMITTER_SELECTOR:-}"
      nccl_ib_disable="${AKS_AI_RUNTIME_FLEX_MANAGED_A100_NCCL_IB_DISABLE:-}"
      [ -n "$nccl_ib_disable" ] || nccl_ib_disable="${AKS_AI_RUNTIME_FLEX_A100_NCCL_IB_DISABLE:-}"
      ;;
    flex-h200)
      rg="${INPUT_FLEX_RESOURCE_GROUP:-}"
      [ -n "$rg" ] || rg="${AKS_AI_RUNTIME_FLEX_RESOURCE_GROUP:-}"
      cluster="${INPUT_FLEX_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="${AKS_AI_RUNTIME_FLEX_CLUSTER_NAME:-}"
      [ -n "$cluster" ] || cluster="$matrix_default_cluster"
      gpu_selector="${AKS_AI_RUNTIME_FLEX_H200_SELECTOR:-}"
      submitter_selector="${AKS_AI_RUNTIME_FLEX_SUBMITTER_SELECTOR:-}"
      nccl_ib_disable="${AKS_AI_RUNTIME_FLEX_H200_NCCL_IB_DISABLE:-}"
      ;;
    *)
      fail "Unknown target ${matrix_target}"
      ;;
  esac

  [ -n "$rg" ] || fail "Resource group is required for ${matrix_target}"
  gpu_selector="kueue.azure.com/gpu-series=${matrix_gpu_series}"
  [ "$gpu_selector" = "$matrix_default_gpu_selector" ] ||
    fail "Matrix GPU selector must match the managed ResourceFlavor series label for ${matrix_target}"
  [ -n "$submitter_selector" ] || submitter_selector="kubernetes.azure.com/mode=system"
  [ -n "$nccl_ib_disable" ] || nccl_ib_disable="1"

  write_output "resource_group" "$rg"
  write_output "cluster" "$cluster"
  write_output "nccl_ib_disable" "$nccl_ib_disable"
  write_output "should_run" "true"
  write_output "workload_profile" "$workload_profile"
  write_output "train_steps" "$train_steps"
  write_output "min_total_tokens" "$min_total_tokens"
  write_output "report_every" "$report_every"
  parse_selector "$gpu_selector" "gpu_selector" "GPU node" "$matrix_target"
  parse_selector "$submitter_selector" "submitter_selector" "Ray submitter/head" "$matrix_target"

  echo "Target ${matrix_target}: event=${event_name} profile=${workload_profile} steps=${train_steps} rg=${rg} cluster=${cluster} gpu_selector=${gpu_selector} submitter_selector=${submitter_selector} nccl_ib_disable=${nccl_ib_disable}"
}

ensure_managed_flavor_labels() {
  local instance_type series nodes
  instance_type="$(require_env GPU_INSTANCE_TYPE)"
  series="$(require_env GPU_SERIES)"
  nodes="$(platform_kubectl get nodes \
    -l "node.kubernetes.io/instance-type=${instance_type}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
  [ -n "$nodes" ] || fail "No nodes found for GPU instance type ${instance_type}"

  while IFS= read -r node; do
    [ -n "$node" ] || continue
    platform_kubectl label node "$node" \
      "kueue.azure.com/gpu-series=${series}" \
      --overwrite >/dev/null
  done <<<"$nodes"

  platform_kubectl get nodes \
    -l "node.kubernetes.io/instance-type=${instance_type},kueue.azure.com/gpu-series=${series}" \
    -o name
}

validate_dataset() {
  for name in NANOGPT_DATASET_URIS NANOGPT_DATASET_SHA256S NANOGPT_DATASET_TOKEN_COUNTS; do
    if [ -z "$(printenv "$name" || true)" ]; then
      fail "$name is required. Configure fixed pre-tokenized OpenWebText shard URIs, SHA256s, and token counts before running large GPU conformance."
    fi
  done

  python3 <<'PY'
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


def csv_values(name):
    values = [item.strip() for item in os.environ[name].split(",")]
    if any(value == "" for value in values):
        raise ValueError(f"{name} contains an empty item; remove blank CSV entries")
    return values


def safe_uri(uri):
    parsed = urllib.parse.urlsplit(uri)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))


def validate_shard_uri(index, uri):
    parsed = urllib.parse.urlsplit(uri)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc or not parsed.path:
        raise ValueError(
            f"NANOGPT_DATASET_URIS item {index} must be an http(s) URL with a non-empty shard path"
        )

    basename = parsed.path.rsplit("/", 1)[-1]
    if ".bin" in basename and not basename.endswith(".bin"):
        raise ValueError(
            f"NANOGPT_DATASET_URIS item {index} has unexpected characters after .bin; "
            f"only put URI data in NANOGPT_DATASET_URIS, not token counts or SHA256s ({safe_uri(uri)})"
        )
    if not basename.endswith(".bin"):
        raise ValueError(
            f"NANOGPT_DATASET_URIS item {index} must point to a .bin shard ({safe_uri(uri)})"
        )


def validate_shard_reachable(index, uri):
    request = urllib.request.Request(uri, method="HEAD")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status >= 400:
                raise ValueError(
                    f"NANOGPT_DATASET_URIS item {index} is not reachable "
                    f"({safe_uri(uri)}): HTTP {response.status}"
                )
    except urllib.error.HTTPError as exc:
        raise ValueError(
            f"NANOGPT_DATASET_URIS item {index} is not reachable "
            f"({safe_uri(uri)}): HTTP {exc.code} {exc.reason}"
        ) from exc
    except urllib.error.URLError as exc:
        raise ValueError(
            f"NANOGPT_DATASET_URIS item {index} is not reachable "
            f"({safe_uri(uri)}): {exc.reason}"
        ) from exc


try:
    uris = csv_values("NANOGPT_DATASET_URIS")
    sha256s = csv_values("NANOGPT_DATASET_SHA256S")
    token_counts = csv_values("NANOGPT_DATASET_TOKEN_COUNTS")

    if len(uris) != len(sha256s) or len(uris) != len(token_counts):
        raise ValueError(
            "OpenWebText shard URI/SHA/token-count lists must have equal lengths "
            f"(uris={len(uris)} sha={len(sha256s)} tokens={len(token_counts)})"
        )

    for index, uri in enumerate(uris, start=1):
        validate_shard_uri(index, uri)

    for index, digest in enumerate(sha256s, start=1):
        if not re.fullmatch(r"[0-9a-fA-F]{64}", digest):
            raise ValueError(
                f"NANOGPT_DATASET_SHA256S item {index} must be a 64-character hexadecimal SHA256 digest"
            )

    for index, token_count in enumerate(token_counts, start=1):
        if not re.fullmatch(r"[1-9][0-9]*", token_count):
            raise ValueError(
                f"NANOGPT_DATASET_TOKEN_COUNTS item {index} must be a positive integer token count"
            )

    for index, uri in enumerate(uris, start=1):
        validate_shard_reachable(index, uri)
except ValueError as exc:
    print(f"::error::{exc}", file=sys.stderr)
    sys.exit(1)

print(f"Configured {len(uris)} OpenWebText shard URI(s) for large GPU conformance")
PY
}

assert_capacity() {
  local selector
  assert_platform_access_contract
  selector="$(require_env GPU_SELECTOR_EXPR)"
  local required_gpus="${REQUIRED_GPUS:-16}"
  case "$required_gpus" in
    ''|*[!0-9]*)
      fail "REQUIRED_GPUS must be a positive integer"
      ;;
  esac
  if [ "$required_gpus" -le 0 ]; then
    fail "REQUIRED_GPUS must be a positive integer"
  fi

  echo "Asserting at least ${required_gpus} available GPUs on Ready schedulable nodes matching ${selector}"
  local nodes_json ready_nodes ready_count node_names_json allocatable_gpu requested_gpu available_gpu
  nodes_json="$(platform_kubectl get nodes -l "$selector" -o json)"
  echo "$nodes_json" | jq -r '.items[]? | [.metadata.name, (.spec.unschedulable // false), ([.status.conditions[]? | select(.type=="Ready") | .status] | first // "Unknown"), (.status.allocatable["nvidia.com/gpu"] // "0")] | @tsv'

  ready_nodes="$(echo "$nodes_json" | jq -r '.items[]? | select((.spec.unschedulable // false) == false) | select(any(.status.conditions[]?; .type=="Ready" and .status=="True")) | select(((.status.allocatable["nvidia.com/gpu"] // "0") | tonumber) > 0) | .metadata.name')"
  ready_count="$(printf '%s\n' "$ready_nodes" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [ "$ready_count" -eq 0 ]; then
    echo "::error::No Ready schedulable GPU nodes with allocatable nvidia.com/gpu found for ${selector}" >&2
    platform_kubectl describe nodes -l "$selector" || true
    exit 1
  fi

  node_names_json="$(printf '%s\n' "$ready_nodes" | sed '/^$/d' | jq -R . | jq -s .)"
  allocatable_gpu="$(echo "$nodes_json" | jq '[.items[]? | select((.spec.unschedulable // false) == false) | select(any(.status.conditions[]?; .type=="Ready" and .status=="True")) | select(((.status.allocatable["nvidia.com/gpu"] // "0") | tonumber) > 0) | (.status.allocatable["nvidia.com/gpu"] | tonumber)] | add // 0')"
  requested_gpu="$(platform_kubectl get pods -A -o json | jq --argjson nodes "$node_names_json" '[.items[]? | select((.status.phase != "Succeeded") and (.status.phase != "Failed")) | select(.spec.nodeName as $node | ($nodes | index($node))) | (([.spec.initContainers[]?.resources.requests["nvidia.com/gpu"]? // "0" | tonumber] | max // 0) as $init | ([.spec.containers[]?.resources.requests["nvidia.com/gpu"]? // "0" | tonumber] | add // 0) as $containers | [$init, $containers] | max)] | add // 0')"
  available_gpu=$((allocatable_gpu - requested_gpu))

  echo "Ready GPU nodes=${ready_count} allocatable_gpu=${allocatable_gpu} requested_gpu=${requested_gpu} available_gpu=${available_gpu}"
  if [ "$available_gpu" -lt "$required_gpus" ]; then
    # Capacity contention on shared GPU pools should skip this scheduled conformance run;
    # the workflow only reaches the test step when the full GPU set is available.
    if [ "$requested_gpu" -gt 0 ]; then
      write_optional_output "should_run" "false"
      echo "::notice::Skipping large GPU conformance for ${MATRIX_TARGET:-target}: only ${available_gpu}/${required_gpus} GPUs are available on selected nodes because existing GPU pods are consuming capacity."
      platform_kubectl get pods -A -o wide | grep -F -f <(printf '%s\n' "$ready_nodes") || true
      return 0
    fi

    echo "::error::Need ${required_gpus} available GPUs for ${MATRIX_TARGET:-target}, found ${available_gpu}. Selected nodes do not have enough Ready schedulable GPU capacity." >&2
    platform_kubectl get pods -A -o wide | grep -F -f <(printf '%s\n' "$ready_nodes") || true
    exit 1
  fi
  write_optional_output "should_run" "true"
}

resolve_ray_image() {
  if [ -n "${RAY_NANOGPT_IMAGE:-}" ]; then
    write_output "image" "$RAY_NANOGPT_IMAGE"
    local ray_version
    ray_version="$(sed -nE 's/.*:.*ray([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' <<<"$RAY_NANOGPT_IMAGE" | head -n1)"
    if [ -n "$ray_version" ]; then
      write_output "version" "$ray_version"
    fi
    echo "Using RAY_NANOGPT_IMAGE override"
    return 0
  fi

  local version python_version ray_version cuda_version registry repository image
  version="$(jq -er 'max_by(.ray | split(".") | map(tonumber))' "${REPO_ROOT}/images/ray/versions.json")"
  python_version="$(jq -r '.python' <<<"$version")"
  ray_version="$(jq -r '.ray' <<<"$version")"
  cuda_version="$(jq -r '.cuda' <<<"$version")"
  registry="${AKS_AI_RUNTIME_RAY_IMAGE_REGISTRY:-}"
  [ -n "$registry" ] || registry="mcr.microsoft.com"
  repository="${AKS_AI_RUNTIME_RAY_IMAGE_REPOSITORY:-}"
  [ -n "$repository" ] || repository="aks/ai-runtime/ray"
  image="${registry}/${repository}:py${python_version}-ray${ray_version}-cuda${cuda_version}"

  write_output "image" "$image"
  write_output "version" "$ray_version"
  echo "Resolved RAY_E2E_IMAGE=${image} from images/ray/versions.json"
}

delete_nanogpt_rayjob() {
  local namespace
  assert_workload_access_contract
  namespace="$(resolved_workload_namespace)" || return 1
  workload_kubectl delete rayjob "$LARGE_GPU_RAYJOB_NAME" -n "$namespace" \
    --ignore-not-found --wait --timeout=5m --cascade=foreground
  echo "Deleted stale nanoGPT RayJob ${LARGE_GPU_RAYJOB_NAME} from shared namespace ${namespace}"
}

reset_namespace() {
  delete_nanogpt_rayjob
}

run_conformance() {
  local namespace manager_workload_only
  assert_workload_execution_contract
  namespace="$(resolved_workload_namespace)" || return 1
  manager_workload_only=0
  if [ "$LARGE_GPU_WORKLOAD_ACCESS_MODE" = "manager" ]; then
    manager_workload_only=1
  fi
  cd "${REPO_ROOT}/tests/e2e"
  echo "target=${MATRIX_TARGET:-unknown} sku=${MATRIX_SKU:-unknown} profile=${WORKLOAD_PROFILE:-unknown} cluster=${TARGET_CLUSTER:-unknown} namespace=${namespace} workload_access_mode=${LARGE_GPU_WORKLOAD_ACCESS_MODE} gpu_selector=${GPU_NODE_SELECTOR_KEY:-}=${GPU_NODE_SELECTOR_VALUE:-} submitter_selector=${RAY_SUBMITTER_NODE_SELECTOR_KEY:-}=${RAY_SUBMITTER_NODE_SELECTOR_VALUE:-} workers=${NANOGPT_TRAIN_WORKERS:-unknown} steps=${NANOGPT_TRAIN_STEPS:-unknown}"
  env -u LARGE_GPU_PLATFORM_KUBECONFIG \
    -u LARGE_GPU_PLATFORM_KUBE_CONTEXT \
    -u DIRECT_PLATFORM_KUBECONFIG \
    -u DIRECT_PLATFORM_CONTEXT \
    KUBECONFIG="$(workload_kubeconfig)" \
    AI_RUNTIME_E2E_KUBE_CONTEXT="$(workload_context)" \
    AI_RUNTIME_E2E_MANAGER_WORKLOAD_ONLY="$manager_workload_only" \
    TEST_NAMESPACE="$namespace" \
    go test -v -timeout 120m -count=1 \
    -run '^TestNanoGPTRayTrainLargeGPU$' ./stack/
}

collect_diagnostics() {
  local selector="${GPU_SELECTOR_EXPR:-}"
  local namespace=""
  local platform_ok=0 workload_ok=0 manager_mode=0 diagnostic_mode
  diagnostic_mode="${LARGE_GPU_DIAGNOSTIC_WORKLOAD_ACCESS_MODE:-${LARGE_GPU_WORKLOAD_ACCESS_MODE:-}}"
  case "$diagnostic_mode" in
    platform-direct)
      ;;
    manager)
      manager_mode=1
      ;;
    *)
      manager_mode=1
      warn "Workload access mode is missing or invalid during diagnostics; suppressing direct platform diagnostics to fail closed."
      ;;
  esac
  if [ "$manager_mode" -eq 0 ] &&
    diagnostic_kube_access_available "platform" LARGE_GPU_PLATFORM_KUBECONFIG LARGE_GPU_PLATFORM_KUBE_CONTEXT; then
    platform_ok=1
  fi
  if diagnostic_kube_access_available "workload" LARGE_GPU_WORKLOAD_KUBECONFIG LARGE_GPU_WORKLOAD_KUBE_CONTEXT; then
    if diagnostic_argocd_workload_queue_available; then
      namespace="${E2E_STACK_NAMESPACE}"
      workload_ok=1
    fi
  fi

  if [ "$platform_ok" -eq 1 ]; then
    echo "=== GPU nodes ==="
    platform_kubectl get nodes -l "$selector" -o wide || true
    echo ""
    echo "=== Kueue controller logs ==="
    platform_kubectl logs deployment/kueue-controller-manager -n kueue-system --tail=150 || true
    echo ""
    echo "=== KubeRay operator logs ==="
    platform_kubectl logs deployment/kuberay-operator -n kuberay-system --tail=150 || true
    echo ""
  fi

  if [ "$workload_ok" -eq 1 ]; then
    echo "=== RayJob status ==="
    if [ "$manager_mode" -eq 1 ]; then
      workload_kubectl get rayjob "$LARGE_GPU_RAYJOB_NAME" -n "$namespace" \
        -o jsonpath='{.metadata.name}{" status="}{.status.jobStatus}{" deployment="}{.status.rayClusterName}{"\n"}' || true
    else
      workload_kubectl get rayjob -n "$namespace" -o jsonpath='{range .items[*]}{.metadata.name}{" status="}{.status.jobStatus}{" deployment="}{.status.rayClusterName}{"\n"}{end}' || true
    fi
    echo ""
    echo "=== Workloads ==="
    workload_kubectl get workloads -n "$namespace" -o wide || true
    if [ "$manager_mode" -eq 0 ]; then
      echo ""
      echo "=== ${namespace} namespace ==="
      workload_kubectl get all -n "$namespace" -o wide || true
      echo ""
      echo "=== Recent events (${namespace}) ==="
      workload_kubectl get events -n "$namespace" --sort-by=.lastTimestamp | tail -50 || true
    fi
  fi

  if [ "$platform_ok" -eq 0 ] && [ "$workload_ok" -eq 0 ]; then
    warn "Large-GPU diagnostics could not access either platform or workload routes; returning success so the workflow still captures warning output."
  fi
}

cleanup_namespace() {
  local namespace
  if ! (assert_workload_access_contract >/dev/null 2>&1); then
    warn "Final nanoGPT cleanup could not validate the workload access contract; leaving ${LARGE_GPU_RAYJOB_NAME} for the next fail-hard pre-run reset."
    return 0
  fi
  if ! namespace="$(resolved_workload_namespace 2>/dev/null)"; then
    warn "Final nanoGPT cleanup could not resolve the shared workload namespace; leaving ${LARGE_GPU_RAYJOB_NAME} for the next fail-hard pre-run reset."
    return 0
  fi
  if ! workload_kubectl delete rayjob "$LARGE_GPU_RAYJOB_NAME" -n "$namespace" \
    --ignore-not-found --wait=false; then
    warn "Final nanoGPT cleanup could not request deletion of RayJob ${namespace}/${LARGE_GPU_RAYJOB_NAME}; leaving it for the next fail-hard pre-run reset."
    return 0
  fi
  echo "Requested best-effort final cleanup of nanoGPT RayJob ${namespace}/${LARGE_GPU_RAYJOB_NAME}"
}

usage() {
  cat >&2 <<'EOF'
Usage: large_gpu_conformance.sh <command>

Commands:
  resolve-target-config
  resolve-access-contract
  ensure-managed-flavor-labels
  validate-dataset
  assert-capacity
  resolve-ray-image
  reset-namespace
  run-conformance
  collect-diagnostics
  cleanup-namespace

Required access-contract env:
  LARGE_GPU_PLATFORM_KUBECONFIG / LARGE_GPU_PLATFORM_KUBE_CONTEXT
  LARGE_GPU_WORKLOAD_KUBECONFIG / LARGE_GPU_WORKLOAD_KUBE_CONTEXT
  LARGE_GPU_WORKLOAD_ACCESS_MODE=platform-direct|manager
  LARGE_GPU_PLATFORM_CLUSTER_IDENTITY (run-conformance; no platform credential)

Shared workload queue contract:
  E2E_STACK_USE_ARGOCD_QUEUE=1
  E2E_STACK_NAMESPACE=taugrid-e2e
  E2E_STACK_QUEUE=jobqueue
  E2E_STACK_LARGE_GPU_QUEUE=jobqueue
EOF
}

command="${1:-}"
case "$command" in
  resolve-target-config)
    resolve_target_config
    ;;
  resolve-access-contract)
    resolve_access_contract
    ;;
  ensure-managed-flavor-labels)
    ensure_managed_flavor_labels
    ;;
  validate-dataset)
    validate_dataset
    ;;
  assert-capacity)
    assert_capacity
    ;;
  resolve-ray-image)
    resolve_ray_image
    ;;
  reset-namespace)
    reset_namespace
    ;;
  run-conformance)
    run_conformance
    ;;
  collect-diagnostics)
    collect_diagnostics
    ;;
  cleanup-namespace)
    cleanup_namespace
    ;;
  *)
    usage
    exit 2
    ;;
esac
