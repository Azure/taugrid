#!/usr/bin/env bash
set -euo pipefail

# FineWeb 16xH200 InfiniBand conformance harness. Sibling to
# large_gpu_conformance.sh: it resolves the flex-h200-ib target, consumes an
# explicit platform kubeconfig/context contract plus an explicit workload
# kubeconfig/context contract, resolves the dataset contract, runs a
# longhaul-friendly skip-if-busy guard (graceful no-op when the H200 GPUs are
# already in use), optionally performs platform-only InfiniBand preparation,
# asserts the RDMA resource is advertised, runs the Go conformance test,
# collects diagnostics, and cleans up the workload namespace.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
IB_SCRIPT="${IB_SCRIPT:-${REPO_ROOT}/scripts/infiniband/enable-infiniband.sh}"
RDMA_RESOURCE="rdma/rdma_shared_device_a"
FINEWEB_RAYJOB_NAME="e2e-fineweb-16xh200-ib"
REQUIRED_GPUS="${FINEWEB_REQUIRED_GPUS:-16}"

fail() {
  echo "::error::$*" >&2
  exit 1
}

warn() {
  echo "::warning::$*" >&2
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

write_github_env() {
  local name="$1"
  local value="$2"
  [ -n "${GITHUB_ENV:-}" ] || return 0
  case "$value" in
    *$'\n'*|*$'\r'*)
      fail "Refusing to write multi-line env '$name'"
      ;;
  esac
  printf '%s=%s\n' "$name" "$value" >>"$GITHUB_ENV"
}

require_env() {
  local name="$1"
  local value
  value="$(printenv "$name" || true)"
  [ -n "$value" ] || fail "$name is required"
  printf '%s' "$value"
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

csv_count() {
  local name="$1"
  python3 - "$name" <<'PY'
import os
import sys

print(len([v for v in os.environ.get(sys.argv[1], "").split(",") if v.strip()]))
PY
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
    fail "E2E_STACK_USE_ARGOCD_QUEUE=1 is required for FineWeb workload commands; refusing legacy fixture or namespace management"
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

# FINEWEB_WORKLOAD_ACCESS_MODE is explicit so the current direct-worker route is
# labeled honestly (`platform-direct`) rather than silently reusing a current
# context. `manager` is reserved for a future scoped workload identity.
require_workload_access_mode() {
  local mode
  mode="$(require_env FINEWEB_WORKLOAD_ACCESS_MODE)"
  case "$mode" in
    platform-direct|manager)
      ;;
    *)
      fail "FINEWEB_WORKLOAD_ACCESS_MODE must be 'platform-direct' or 'manager' (got '${mode}')"
      ;;
  esac
  printf '%s' "$mode"
}

# FINEWEB_PLATFORM_PREP_MODE keeps worker-infrastructure mutation explicit. The
# workflow can choose today's direct platform prep or future assert-only mode for
# pre-provisioned RDMA.
require_platform_prep_mode() {
  local mode
  mode="$(require_env FINEWEB_PLATFORM_PREP_MODE)"
  case "$mode" in
    direct-platform|assert-only)
      ;;
    *)
      fail "FINEWEB_PLATFORM_PREP_MODE must be 'direct-platform' or 'assert-only' (got '${mode}')"
      ;;
  esac
  printf '%s' "$mode"
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

kube_access_fingerprint() {
  local kubeconfig_var="$1"
  local context_var="$2"
  local fingerprint
  fingerprint="$(load_kube_access_view "fingerprint" "$kubeconfig_var" "$context_var" | jq -c '{cluster: (.clusters[0].cluster // {}), user: (.users[0].user // {})}')"
  [ -n "$fingerprint" ] || fail "Could not derive a credential fingerprint from ${kubeconfig_var}=$(require_env "$kubeconfig_var")"
  printf '%s' "$fingerprint"
}

kube_cluster_identity() {
  local kubeconfig_var="$1"
  local context_var="$2"
  local identity
  identity="$(load_kube_access_view "cluster-identity" "$kubeconfig_var" "$context_var" | jq -c '(.clusters[0].cluster // {})')"
  [ -n "$identity" ] || fail "Could not derive a cluster identity from ${kubeconfig_var}=$(require_env "$kubeconfig_var")"
  printf '%s' "$identity"
}

platform_kubeconfig() {
  require_env FINEWEB_PLATFORM_KUBECONFIG
}

platform_context() {
  require_env FINEWEB_PLATFORM_KUBE_CONTEXT
}

workload_kubeconfig() {
  require_env FINEWEB_WORKLOAD_KUBECONFIG
}

workload_context() {
  require_env FINEWEB_WORKLOAD_KUBE_CONTEXT
}

assert_platform_access_contract() {
  validate_kube_access_contract "platform" FINEWEB_PLATFORM_KUBECONFIG FINEWEB_PLATFORM_KUBE_CONTEXT
}

assert_workload_access_contract() {
  local mode platform_cluster workload_cluster platform_fingerprint workload_fingerprint
  mode="$(require_workload_access_mode)" || return 1
  validate_kube_access_contract "platform" FINEWEB_PLATFORM_KUBECONFIG FINEWEB_PLATFORM_KUBE_CONTEXT || return 1
  validate_kube_access_contract "workload" FINEWEB_WORKLOAD_KUBECONFIG FINEWEB_WORKLOAD_KUBE_CONTEXT || return 1
  platform_cluster="$(kube_cluster_identity FINEWEB_PLATFORM_KUBECONFIG FINEWEB_PLATFORM_KUBE_CONTEXT)" || return 1
  workload_cluster="$(kube_cluster_identity FINEWEB_WORKLOAD_KUBECONFIG FINEWEB_WORKLOAD_KUBE_CONTEXT)" || return 1
  [ "$workload_cluster" = "$platform_cluster" ] || fail "FineWeb platform and workload access contracts must resolve to the same cluster identity"
  platform_fingerprint="$(kube_access_fingerprint FINEWEB_PLATFORM_KUBECONFIG FINEWEB_PLATFORM_KUBE_CONTEXT)" || return 1
  workload_fingerprint="$(kube_access_fingerprint FINEWEB_WORKLOAD_KUBECONFIG FINEWEB_WORKLOAD_KUBE_CONTEXT)" || return 1
  case "$mode" in
    platform-direct)
      [ "$workload_fingerprint" = "$platform_fingerprint" ] || fail "FINEWEB_WORKLOAD_ACCESS_MODE=platform-direct requires workload access to reuse the direct platform credential fingerprint"
      ;;
    manager)
      [ "$workload_fingerprint" != "$platform_fingerprint" ] || fail "FINEWEB_WORKLOAD_ACCESS_MODE=manager requires a credential fingerprint distinct from the direct platform access contract; refusing silent fallback to direct platform access"
      ;;
  esac
}

assert_platform_mutation_allowed() {
  local prep_mode workload_mode
  prep_mode="$(require_platform_prep_mode)"
  workload_mode="$(require_workload_access_mode)"
  [ "$prep_mode" = "direct-platform" ] || fail "FINEWEB_PLATFORM_PREP_MODE=${prep_mode} keeps worker infrastructure pre-provisioned; use assert-rdma-advertised instead of enable-ib"
  [ "$workload_mode" != "manager" ] || fail "FINEWEB_WORKLOAD_ACCESS_MODE=manager requires pre-provisioned IB/RDMA; refusing worker infrastructure mutation from the harness"
}

platform_kubectl() {
  kubectl --kubeconfig "$(platform_kubeconfig)" --context "$(platform_context)" "$@"
}

workload_kubectl() {
  kubectl --kubeconfig "$(workload_kubeconfig)" --context "$(workload_context)" "$@"
}

# count_available_gpus prints how many nvidia.com/gpu are free on the Ready,
# schedulable nodes matching $1 (allocatable minus non-terminal pod requests).
count_available_gpus() {
  local selector="$1"
  local nodes_json node_names_json allocatable_gpu requested_gpu
  nodes_json="$(platform_kubectl get nodes -l "$selector" -o json)"

  node_names_json="$(echo "$nodes_json" | jq -c '[.items[]? | select((.spec.unschedulable // false) == false) | select(any(.status.conditions[]?; .type=="Ready" and .status=="True")) | select(((.status.allocatable["nvidia.com/gpu"] // "0") | tonumber) > 0) | .metadata.name]')"
  allocatable_gpu="$(echo "$nodes_json" | jq '[.items[]? | select((.spec.unschedulable // false) == false) | select(any(.status.conditions[]?; .type=="Ready" and .status=="True")) | select(((.status.allocatable["nvidia.com/gpu"] // "0") | tonumber) > 0) | (.status.allocatable["nvidia.com/gpu"] | tonumber)] | add // 0')"
  requested_gpu="$(platform_kubectl get pods -A -o json | jq --argjson nodes "$node_names_json" '[.items[]? | select((.status.phase != "Succeeded") and (.status.phase != "Failed")) | select(.spec.nodeName as $node | ($nodes | index($node))) | (([.spec.initContainers[]?.resources.requests["nvidia.com/gpu"]? // "0" | tonumber] | max // 0) as $init | ([.spec.containers[]?.resources.requests["nvidia.com/gpu"]? // "0" | tonumber] | add // 0) as $containers | [$init, $containers] | max)] | add // 0')"

  echo $((allocatable_gpu - requested_gpu))
}

resolve_target_config() {
  local event_name should_run gpu_selector submitter_selector rg cluster
  event_name="${GITHUB_EVENT_NAME:-${EVENT_NAME:-}}"

  should_run="true"
  # workflow_dispatch can request a one-off run regardless of schedule gating.
  if [ "$event_name" = "schedule" ] && [ "${FINEWEB_RUN_ON_SCHEDULE:-true}" != "true" ]; then
    should_run="false"
  fi

  if [ "$should_run" != "true" ]; then
    write_output "should_run" "false"
    echo "Skipping flex-h200-ib for event=${event_name}"
    return 0
  fi

  rg="${INPUT_FLEX_RESOURCE_GROUP:-${AKS_AI_RUNTIME_FLEX_RESOURCE_GROUP:-}}"
  cluster="${INPUT_FLEX_CLUSTER_NAME:-${AKS_AI_RUNTIME_FLEX_CLUSTER_NAME:-}}"
  [ -n "$rg" ] || fail "Resource group is required (set AKS_AI_RUNTIME_FLEX_RESOURCE_GROUP)"
  [ -n "$cluster" ] || fail "Cluster name is required (set AKS_AI_RUNTIME_FLEX_CLUSTER_NAME)"

  gpu_selector="${AKS_AI_RUNTIME_FLEX_H200_SELECTOR:-kueue.azure.com/gpu-series=nd-h200-v5}"
  submitter_selector="${AKS_AI_RUNTIME_FLEX_SUBMITTER_SELECTOR:-kubernetes.azure.com/mode=system}"

  local train_steps checkpoint_interval min_total_tokens report_every
  train_steps="${FINEWEB_TRAIN_STEPS:-60}"
  checkpoint_interval="${FINEWEB_CHECKPOINT_INTERVAL:-50}"
  min_total_tokens="${FINEWEB_MIN_TOTAL_TOKENS:-10000000}"
  report_every="${FINEWEB_REPORT_EVERY:-10}"

  write_output "should_run" "true"
  write_output "resource_group" "$rg"
  write_output "cluster" "$cluster"
  write_output "train_steps" "$train_steps"
  write_output "checkpoint_interval" "$checkpoint_interval"
  write_output "min_total_tokens" "$min_total_tokens"
  write_output "report_every" "$report_every"
  parse_selector "$gpu_selector" "gpu_selector" "GPU node" "flex-h200-ib"
  parse_selector "$submitter_selector" "submitter_selector" "Ray submitter/head" "flex-h200-ib"

  echo "Target flex-h200-ib: event=${event_name} steps=${train_steps} checkpoint_interval=${checkpoint_interval} rg=${rg} cluster=${cluster} gpu_selector=${gpu_selector} submitter_selector=${submitter_selector}"
}

ensure_managed_flavor_labels() {
  local instance_type="${GPU_INSTANCE_TYPE:-Standard_ND96isr_H200_v5}"
  local series="${GPU_SERIES:-nd-h200-v5}"
  local nodes node matching_nodes node_count matching_count

  nodes="$(platform_kubectl get nodes \
    -l "node.kubernetes.io/instance-type=${instance_type}" \
    -o name)"
  [ -n "$nodes" ] ||
    fail "No nodes found for managed GPU series ${series} with instance type ${instance_type}"

  if [ "$(require_platform_prep_mode)" = "direct-platform" ]; then
    while IFS= read -r node; do
      [ -n "$node" ] || continue
      platform_kubectl label "$node" \
        "kueue.azure.com/gpu-series=${series}" \
        --overwrite >/dev/null
    done <<<"$nodes"
  fi

  matching_nodes="$(platform_kubectl get nodes \
    -l "node.kubernetes.io/instance-type=${instance_type},kueue.azure.com/gpu-series=${series}" \
    -o name)"
  node_count="$(printf '%s\n' "$nodes" | awk 'NF { count++ } END { print count+0 }')"
  matching_count="$(printf '%s\n' "$matching_nodes" | awk 'NF { count++ } END { print count+0 }')"
  [ "$matching_count" -eq "$node_count" ] ||
    fail "Managed ResourceFlavor label ${series} covers ${matching_count}/${node_count} ${instance_type} nodes; every node in the immutable instance-type pool must match"
  printf '%s\n' "$matching_nodes"
}

# resolve_fineweb_from_registry: when explicitly configured, resolve the managed
# `tau dataset` registry as the dataset source of truth. The flex conformance
# workflow intentionally does not default to this path yet: the live flex cluster
# has no blob-training PVC in the ray namespace, and `-o env --staged-root` emits
# file:// paths that require a real staging/mount path before the Ray workers can
# consume them. Until that staging path exists, the durable HTTP shard env contract
# remains the default working path.
resolve_fineweb_from_registry() {
  local dataset_ref registry namespace stage_root env_block wctx wkcfg
  registry="${FINEWEB_DATASET_REGISTRY:-}"
  [ -n "$registry" ] || return 1
  dataset_ref="${FINEWEB_DATASET_REF:-fineweb-sample-10bt@v1}"
  namespace="${FINEWEB_DATASET_REGISTRY_NAMESPACE:-ray}"
  stage_root="${FINEWEB_STAGE_ROOT:-/mnt/datasets/fineweb-sample-10bt/v1}"

  if [ "$registry" = "pvc" ]; then
    assert_workload_access_contract || fail "FINEWEB_DATASET_REGISTRY=pvc requires a valid explicit workload access contract"
  fi
  command -v tau >/dev/null 2>&1 || return 1

  local args=(data dataset ref "$dataset_ref" --staged-root "$stage_root" -o env --registry "$registry")
  if [ "$registry" = "pvc" ]; then
    args+=(--namespace "$namespace")
    wkcfg="$(workload_kubeconfig)"
    wctx="$(workload_context)"
    args+=(--context "$wctx")
  fi

  if ! env_block="$(KUBECONFIG="${wkcfg:-${KUBECONFIG:-}}" tau "${args[@]}" 2>&1)"; then
    echo "::warning::tau dataset registry unavailable for ${dataset_ref}: ${env_block//$'\n'/; }" >&2
    return 1
  fi
  while IFS='=' read -r key value; do
    case "$key" in
      FINEWEB_DATASET_URIS|FINEWEB_DATASET_SHA256S|FINEWEB_DATASET_TOKEN_COUNTS)
        export "$key=$value"
        ;;
      "")
        ;;
      *)
        echo "::warning::Ignoring unexpected tau dataset env key ${key}"
        ;;
    esac
  done <<<"$env_block"
  export FINEWEB_DATASET_URIS FINEWEB_DATASET_SHA256S FINEWEB_DATASET_TOKEN_COUNTS
}

persist_dataset_contract() {
  local name
  for name in FINEWEB_DATASET_URIS FINEWEB_DATASET_SHA256S FINEWEB_DATASET_TOKEN_COUNTS; do
    write_github_env "$name" "$(require_env "$name")"
  done
}

validate_dataset_values() {
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
    if parsed.scheme in {"http", "https"}:
        if not parsed.netloc or not parsed.path:
            raise ValueError(
                f"FINEWEB_DATASET_URIS item {index} must have a non-empty shard host and path"
            )
    elif parsed.scheme == "file":
        if not parsed.path:
            raise ValueError(f"FINEWEB_DATASET_URIS item {index} must have a non-empty file path")
    else:
        raise ValueError(
            f"FINEWEB_DATASET_URIS item {index} must be an http(s) or file URL ({safe_uri(uri)})"
        )

    basename = parsed.path.rsplit("/", 1)[-1]
    if ".bin" in basename and not basename.endswith(".bin"):
        raise ValueError(
            f"FINEWEB_DATASET_URIS item {index} has unexpected characters after .bin; "
            f"only put URI data in FINEWEB_DATASET_URIS, not token counts or SHA256s ({safe_uri(uri)})"
        )
    if not basename.endswith(".bin"):
        raise ValueError(
            f"FINEWEB_DATASET_URIS item {index} must point to a .bin shard ({safe_uri(uri)})"
        )


def validate_http_reachable(index, uri):
    parsed = urllib.parse.urlsplit(uri)
    if parsed.scheme not in {"http", "https"}:
        return
    request = urllib.request.Request(uri, method="HEAD")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status >= 400:
                raise ValueError(
                    f"FINEWEB_DATASET_URIS item {index} is not reachable "
                    f"({safe_uri(uri)}): HTTP {response.status}"
                )
    except urllib.error.HTTPError as exc:
        raise ValueError(
            f"FINEWEB_DATASET_URIS item {index} is not reachable "
            f"({safe_uri(uri)}): HTTP {exc.code} {exc.reason}"
        ) from exc
    except urllib.error.URLError as exc:
        raise ValueError(
            f"FINEWEB_DATASET_URIS item {index} is not reachable "
            f"({safe_uri(uri)}): {exc.reason}"
        ) from exc


try:
    uris = csv_values("FINEWEB_DATASET_URIS")
    sha256s = csv_values("FINEWEB_DATASET_SHA256S")
    token_counts = csv_values("FINEWEB_DATASET_TOKEN_COUNTS")

    if len(uris) != len(sha256s) or len(uris) != len(token_counts):
        raise ValueError(
            "FineWeb shard URI/SHA/token-count lists must have equal lengths "
            f"(uris={len(uris)} sha={len(sha256s)} tokens={len(token_counts)})"
        )

    for index, uri in enumerate(uris, start=1):
        validate_shard_uri(index, uri)

    for index, digest in enumerate(sha256s, start=1):
        if not re.fullmatch(r"[0-9a-fA-F]{64}", digest):
            raise ValueError(
                f"FINEWEB_DATASET_SHA256S item {index} must be a 64-character hexadecimal SHA256 digest"
            )

    for index, token_count in enumerate(token_counts, start=1):
        if not re.fullmatch(r"[1-9][0-9]*", token_count):
            raise ValueError(
                f"FINEWEB_DATASET_TOKEN_COUNTS item {index} must be a positive integer token count"
            )

    for index, uri in enumerate(uris, start=1):
        validate_http_reachable(index, uri)
except ValueError as exc:
    print(f"::error::{exc}", file=sys.stderr)
    sys.exit(1)
PY
}

validate_dataset() {
  if resolve_fineweb_from_registry; then
    echo "Resolved FineWeb shards from tau dataset registry (${FINEWEB_DATASET_REF:-fineweb-sample-10bt@v1})"
  elif [ -n "${FINEWEB_DATASET_REGISTRY:-}" ]; then
    echo "tau dataset registry unavailable; using FINEWEB_DATASET_* env fallback"
  else
    echo "tau dataset registry not configured; using FINEWEB_DATASET_* env fallback"
  fi

  local missing=() provided=0
  for name in FINEWEB_DATASET_URIS FINEWEB_DATASET_SHA256S FINEWEB_DATASET_TOKEN_COUNTS; do
    if [ -z "$(printenv "$name" || true)" ]; then
      missing+=("$name")
    else
      provided=$((provided + 1))
    fi
  done

  if [ "${#missing[@]}" -gt 0 ]; then
    if [ "$provided" -eq 0 ]; then
      fail "No fixed pre-tokenized FineWeb dataset contract is configured. Configure the tau dataset registry or all of FINEWEB_DATASET_URIS, FINEWEB_DATASET_SHA256S, and FINEWEB_DATASET_TOKEN_COUNTS before running FineWeb IB conformance."
    fi
    fail "Incomplete FineWeb dataset contract: missing ${missing[*]}. Configure all of FINEWEB_DATASET_URIS, FINEWEB_DATASET_SHA256S, and FINEWEB_DATASET_TOKEN_COUNTS."
  fi

  local uri_count sha_count token_count
  uri_count="$(csv_count FINEWEB_DATASET_URIS)"
  sha_count="$(csv_count FINEWEB_DATASET_SHA256S)"
  token_count="$(csv_count FINEWEB_DATASET_TOKEN_COUNTS)"

  if [ "$uri_count" -ne "$sha_count" ] || [ "$uri_count" -ne "$token_count" ]; then
    fail "FineWeb shard URI/SHA/token-count lists must have equal lengths (uris=${uri_count} sha=${sha_count} tokens=${token_count})"
  fi
  validate_dataset_values
  write_optional_output "should_run" "true"
  persist_dataset_contract
  echo "Configured ${uri_count} FineWeb shard URI(s) for IB conformance"
}

delete_fineweb_rayjob() {
  local namespace
  assert_workload_access_contract
  namespace="$(resolved_workload_namespace)" || return 1
  workload_kubectl delete rayjob "$FINEWEB_RAYJOB_NAME" -n "$namespace" \
    --ignore-not-found --wait --timeout=5m --cascade=foreground
  echo "Deleted stale FineWeb RayJob ${FINEWEB_RAYJOB_NAME} from shared namespace ${namespace}"
}

# skip-if-busy: longhaul guard. It first removes the fixed-name FineWeb RayJob
# from an interrupted prior run, then sets should_run=false (graceful no-op) when
# fewer than REQUIRED_GPUS are available.
skip_if_busy() {
  local selector available
  assert_platform_access_contract
  delete_fineweb_rayjob
  selector="$(require_env GPU_SELECTOR_EXPR)"

  available="$(count_available_gpus "$selector")"
  echo "Available GPUs on ${selector}: ${available} (need ${REQUIRED_GPUS})"

  if [ "$available" -lt "$REQUIRED_GPUS" ]; then
    write_output "should_run" "false"
    echo "::notice::Skipping FineWeb IB conformance: only ${available}/${REQUIRED_GPUS} GPUs available on the H200 nodes (cluster busy). This is a graceful no-op, not a failure."
    return 0
  fi

  write_output "should_run" "true"
  echo "Cluster idle enough: ${available} GPUs available; proceeding."
}

# export_ib_node_selector ensures GPU_NODE_SELECTOR_KEY/VALUE are exported so the IB
# enablement script scopes its rdma-resource wait to the H200 nodes only. On the mixed
# flex cluster (A100 + H200) the script's default selector (nvidia.com/gpu.present=true)
# would otherwise wait for rdma on EVERY GPU node, and the A100 nodes never advertise it,
# timing out install. When the caller did not set KEY/VALUE explicitly, derive them from
# GPU_SELECTOR_EXPR (key=value) so manual harness runs scope correctly too.
export_ib_node_selector() {
  if [ -n "${GPU_NODE_SELECTOR_KEY:-}" ] && [ -n "${GPU_NODE_SELECTOR_VALUE:-}" ]; then
    export GPU_NODE_SELECTOR_KEY GPU_NODE_SELECTOR_VALUE
    return 0
  fi
  local expr="${GPU_SELECTOR_EXPR:-}"
  local key="${expr%%=*}"
  local value="${expr#*=}"
  if [ -n "$expr" ] && [ -n "$key" ] && [ -n "$value" ] && [ "$key" != "$expr" ]; then
    export GPU_NODE_SELECTOR_KEY="$key"
    export GPU_NODE_SELECTOR_VALUE="$value"
  else
    echo "::warning::No GPU node selector resolved for InfiniBand scoping; the IB script will use its default selector"
  fi
}

enable_ib() {
  [ -x "$IB_SCRIPT" ] || [ -f "$IB_SCRIPT" ] || fail "InfiniBand enablement script not found at ${IB_SCRIPT}"
  local ctx
  assert_platform_access_contract
  assert_platform_mutation_allowed
  ctx="$(platform_context)"
  export_ib_node_selector
  echo "Enabling InfiniBand on context=${ctx} (node selector ${GPU_NODE_SELECTOR_KEY:-<default>}=${GPU_NODE_SELECTOR_VALUE:-<default>})"
  KUBECONFIG="$(platform_kubeconfig)" bash "$IB_SCRIPT" --context "$ctx" --yes preflight
  KUBECONFIG="$(platform_kubeconfig)" bash "$IB_SCRIPT" --context "$ctx" --yes install
}

assert_rdma_advertised() {
  local selector ready_count advertised
  assert_platform_access_contract
  selector="$(require_env GPU_SELECTOR_EXPR)"

  echo "Waiting for both H200 nodes to advertise ${RDMA_RESOURCE}"
  local attempt
  for attempt in $(seq 1 30); do
    advertised="$(platform_kubectl get nodes -l "$selector" -o json | jq '[.items[]? | select(((.status.allocatable["'"$RDMA_RESOURCE"'"] // "0") | tonumber) > 0)] | length')"
    ready_count="$(platform_kubectl get nodes -l "$selector" -o json | jq '[.items[]?] | length')"
    echo "  attempt ${attempt}: ${advertised}/${ready_count} nodes advertise ${RDMA_RESOURCE}"
    if [ "${advertised:-0}" -ge 2 ]; then
      echo "Both H200 nodes advertise ${RDMA_RESOURCE}"
      return 0
    fi
    sleep 10
  done
  platform_kubectl describe nodes -l "$selector" | grep -A3 "Allocatable" || true
  fail "Timed out waiting for ${RDMA_RESOURCE} to be advertised on the H200 nodes"
}

resolve_ray_image() {
  if [ -n "${RAY_FINEWEB_IMAGE:-}" ]; then
    write_output "image" "$RAY_FINEWEB_IMAGE"
    local ray_version
    ray_version="$(sed -nE 's/.*:.*ray([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' <<<"$RAY_FINEWEB_IMAGE" | head -n1)"
    if [ -n "$ray_version" ]; then
      write_output "version" "$ray_version"
    fi
    echo "Using RAY_FINEWEB_IMAGE override"
    return 0
  fi

  local version python_version ray_version cuda_version registry repository image
  version="$(jq -er 'max_by(.ray | split(".") | map(tonumber))' "${REPO_ROOT}/images/ray/versions.json")"
  python_version="$(jq -r '.python' <<<"$version")"
  ray_version="$(jq -r '.ray' <<<"$version")"
  cuda_version="$(jq -r '.cuda' <<<"$version")"
  registry="${AKS_AI_RUNTIME_RAY_IMAGE_REGISTRY:-mcr.microsoft.com}"
  repository="${AKS_AI_RUNTIME_RAY_IMAGE_REPOSITORY:-aks/ai-runtime/ray}"
  image="${registry}/${repository}:py${python_version}-ray${ray_version}-cuda${cuda_version}"

  write_output "image" "$image"
  write_output "version" "$ray_version"
  echo "Resolved RAY_E2E_IMAGE=${image} from images/ray/versions.json"
}

reset_namespace() {
  delete_fineweb_rayjob
}

run_conformance() {
  local namespace
  assert_workload_access_contract
  namespace="$(resolved_workload_namespace)" || return 1
  cd "${REPO_ROOT}/tests/e2e"
  echo "target=flex-h200-ib namespace=${namespace} gpu_selector=${GPU_NODE_SELECTOR_KEY:-}=${GPU_NODE_SELECTOR_VALUE:-} submitter_selector=${RAY_SUBMITTER_NODE_SELECTOR_KEY:-}=${RAY_SUBMITTER_NODE_SELECTOR_VALUE:-} workers=${FINEWEB_TRAIN_WORKERS:-16} steps=${FINEWEB_TRAIN_STEPS:-60}"
  KUBECONFIG="$(workload_kubeconfig)" AI_RUNTIME_E2E_KUBE_CONTEXT="$(workload_context)" TEST_NAMESPACE="$namespace" go test -v -timeout 150m -count=1 \
    -run '^TestFineWebRayTrain16xH200IB$' ./stack/
}

collect_diagnostics() {
  local selector="${GPU_SELECTOR_EXPR:-}"
  local namespace=""
  local platform_ok=0 workload_ok=0
  if diagnostic_kube_access_available "platform" FINEWEB_PLATFORM_KUBECONFIG FINEWEB_PLATFORM_KUBE_CONTEXT; then
    platform_ok=1
  fi
  if diagnostic_kube_access_available "workload" FINEWEB_WORKLOAD_KUBECONFIG FINEWEB_WORKLOAD_KUBE_CONTEXT; then
    if diagnostic_argocd_workload_queue_available; then
      namespace="${E2E_STACK_NAMESPACE}"
      workload_ok=1
    fi
  fi

  if [ "$platform_ok" -eq 1 ]; then
    echo "=== GPU nodes ==="
    platform_kubectl get nodes -l "$selector" -o wide || true
    echo ""
    echo "=== ${RDMA_RESOURCE} advertised ==="
    platform_kubectl get nodes -l "$selector" -o json | jq -r '.items[]? | [.metadata.name, (.status.allocatable["'"$RDMA_RESOURCE"'"] // "0")] | @tsv' || true
    echo ""
    echo "=== nvidia-network-operator pods ==="
    platform_kubectl get pods -n nvidia-network-operator -o wide || true
    echo ""
    echo "=== Kueue controller logs ==="
    platform_kubectl logs deployment/kueue-controller-manager -n kueue-system --tail=150 || true
    echo ""
    echo "=== KubeRay operator logs ==="
    platform_kubectl logs deployment/kuberay-operator -n kuberay-system --tail=150 || true
    echo ""
  fi

  if [ "$workload_ok" -eq 1 ]; then
    echo "=== ${namespace} namespace ==="
    workload_kubectl get all -n "$namespace" -o wide || true
    echo ""
    echo "=== RayJob status ==="
    workload_kubectl get rayjob -n "$namespace" -o jsonpath='{range .items[*]}{.metadata.name}{" status="}{.status.jobStatus}{" deployment="}{.status.rayClusterName}{"\n"}{end}' || true
    echo ""
    echo "=== Workloads ==="
    workload_kubectl get workloads -n "$namespace" -o wide || true
    echo ""
    echo "=== Recent events (${namespace}) ==="
    workload_kubectl get events -n "$namespace" --sort-by=.lastTimestamp | tail -50 || true
  fi

  if [ "$platform_ok" -eq 0 ] && [ "$workload_ok" -eq 0 ]; then
    warn "FineWeb diagnostics could not access either platform or workload routes; returning success so the workflow still captures warning output."
  fi
}

cleanup_namespace() {
  delete_fineweb_rayjob
}

disable_ib() {
  [ -f "$IB_SCRIPT" ] || { echo "InfiniBand script not found at ${IB_SCRIPT}; nothing to uninstall"; return 0; }
  local ctx
  assert_platform_access_contract
  assert_platform_mutation_allowed
  ctx="$(platform_context)"
  echo "Uninstalling InfiniBand on context=${ctx}"
  export_ib_node_selector
  KUBECONFIG="$(platform_kubeconfig)" bash "$IB_SCRIPT" --context "$ctx" --yes uninstall || true
}

usage() {
  cat >&2 <<'EOF'
Usage: fineweb_ib_conformance.sh <command>

Commands:
  resolve-target-config
  ensure-managed-flavor-labels
  validate-dataset
  skip-if-busy
  enable-ib
  assert-rdma-advertised
  resolve-ray-image
  reset-namespace
  run-conformance
  collect-diagnostics
  cleanup-namespace
  disable-ib

Required access-contract env:
  FINEWEB_PLATFORM_KUBECONFIG / FINEWEB_PLATFORM_KUBE_CONTEXT
  FINEWEB_PLATFORM_PREP_MODE=direct-platform|assert-only
  FINEWEB_WORKLOAD_KUBECONFIG / FINEWEB_WORKLOAD_KUBE_CONTEXT
  FINEWEB_WORKLOAD_ACCESS_MODE=platform-direct|manager

Shared workload queue contract (live FineWeb path):
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
  ensure-managed-flavor-labels)
    ensure_managed_flavor_labels
    ;;
  validate-dataset)
    validate_dataset
    ;;
  skip-if-busy)
    skip_if_busy
    ;;
  enable-ib)
    enable_ib
    ;;
  assert-rdma-advertised)
    assert_rdma_advertised
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
  disable-ib)
    disable_ib
    ;;
  *)
    usage
    exit 2
    ;;
esac
