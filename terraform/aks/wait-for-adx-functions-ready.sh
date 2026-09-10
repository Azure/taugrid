#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if (($# < 7 || $# > 9)); then
  echo "Usage: $0 subscription resource-group cluster kubeconfig chart base-values environment-values [release [release-namespace]]" >&2
  exit 1
fi
subscription_id="$1"
resource_group="$2"
cluster_name="$3"
kubeconfig="$4"
chart_path="$5"
base_values_file="$6"
environment_values_file="$7"
release="${8:-adx-mon}"
release_namespace="${9:-adx-mon}"
maximum_attempts="${MAXIMUM_ADX_FUNCTION_ATTEMPTS:-6}"
function_wait_seconds="${ADX_FUNCTION_WAIT_SECONDS:-300}"
allow_no_functions="${ADX_ALLOW_NO_FUNCTIONS:-false}"
policy="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/adx-function-state.jq"

if [[ ! "$maximum_attempts" =~ ^([1-9]|10)$ || ! "$function_wait_seconds" =~ ^[0-9]{2,3}$ ]] ||
    ((10#$function_wait_seconds < 60 || 10#$function_wait_seconds > 900)) ||
    [[ "$allow_no_functions" != true && "$allow_no_functions" != false ]]; then
  echo "Invalid waiter configuration: attempts must be 1-10, wait seconds 60-900, and ADX_ALLOW_NO_FUNCTIONS true or false." >&2
  exit 1
fi
function_wait_seconds=$((10#$function_wait_seconds))
for tool in az helm kubectl jq yq; do
  command -v "$tool" >/dev/null || { echo "Required tool not found: $tool" >&2; exit 1; }
done
if [[ "$(yq --version)" != *"github.com/mikefarah/yq/"*"version v4."* ]]; then
  echo "The Function waiter requires Mike Farah yq v4." >&2
  exit 1
fi

chart_arguments=("$release" "$chart_path" --namespace "$release_namespace" --values "$base_values_file" --values "$environment_values_file")
expected='[]'
validate_source() {
  jq -cse --arg mode "$1" --arg release "$release" --arg releaseNamespace "$release_namespace" \
    --arg namespace "${2:-}" --argjson expected "$expected" --from-file "$policy"
}

# Render the same ordered values as every upgrade; never infer intent from live inventory.
expected="$(helm template "${chart_arguments[@]}" | yq eval-all -o=json -I=0 '[.]' - | validate_source expected)" ||
  { echo "Unable to establish the required Function set from the installation chart/values." >&2; exit 1; }
if [[ "$expected" == '[]' && "$allow_no_functions" != true ]]; then
  echo "No required Functions rendered. Review the chart/values; set ADX_ALLOW_NO_FUNCTIONS=true only for an intentionally Function-free installation." >&2
  exit 1
fi
namespaces="$(jq -r '[.[].namespace] | unique[]' <<< "$expected")"

get_states() {
  local mode="$1" namespace raw states combined='[]'
  while IFS= read -r namespace; do
    [[ -n "$namespace" ]] || continue
    raw="$(kubectl get functions.adx-mon.azure.com --namespace "$namespace" --output=json --kubeconfig "$kubeconfig" --request-timeout=30s)" ||
      { echo "Unable to read adx-mon Functions in $namespace." >&2; return 1; }
    states="$(validate_source "$mode" "$namespace" <<< "$raw")" || return 1
    combined="$(printf '%s\n%s\n' "$combined" "$states" | jq -cs 'add')" || return 1
  done <<< "$namespaces"
  printf '%s\n' "$combined"
}

az aks get-credentials --admin --subscription "$subscription_id" --resource-group "$resource_group" \
  --name "$cluster_name" --file "$kubeconfig" --overwrite-existing

for ((attempt = 1; attempt <= maximum_attempts; attempt++)); do
  # Check ownership before Helm can update an existing expected name, including after a delete conflict.
  get_states preflight >/dev/null
  helm upgrade --install "${chart_arguments[@]}" --reset-values --kubeconfig "$kubeconfig" \
    --create-namespace --wait --timeout 30m
  if [[ "$expected" == '[]' ]]; then
    echo "No Functions rendered; Function readiness was explicitly disabled."
    exit 0
  fi

  deadline=$((SECONDS + function_wait_seconds))
  retryable_failures='[]'
  states='[]'
  while ((SECONDS < deadline)); do
    states="$(get_states state)"
    terminal_failure="$(jq -r '.[] | select(.phase == "Terminal") | .diagnostic' <<< "$states")"
    if [[ -n "$terminal_failure" ]]; then
      echo "adx-mon Function reconciliation reached a terminal failure: $terminal_failure" >&2
      exit 1
    fi
    retryable_failures="$(jq -c '[.[] | select(.phase == "Throttle")]' <<< "$states")"
    if [[ "$retryable_failures" != '[]' ]]; then
      break
    fi
    if jq -e 'length > 0 and all(.phase == "Success")' <<< "$states" >/dev/null; then
      echo "All required adx-mon Functions reached Success."
      exit 0
    fi
    sleep 15
  done

  if [[ "$retryable_failures" == '[]' ]]; then
    echo "adx-mon Functions did not reach Success within $function_wait_seconds seconds on attempt $attempt: $(jq -r '.[] | select(.phase != "Success") | .diagnostic' <<< "$states")" >&2
    exit 1
  fi
  echo "adx-mon Functions reached retryable ADX throttling on attempt $attempt." >&2
  if ((attempt == maximum_attempts)); then
    echo "adx-mon Functions did not recover from ADX throttling after $maximum_attempts attempts." >&2
    exit 1
  fi

  while IFS= read -r failure; do
    path="$(jq -r '"/apis/adx-mon.azure.com/v1/namespaces/\(.namespace)/functions/\(.name)"' <<< "$failure")"
    options="$(jq -c '{apiVersion:"v1", kind:"DeleteOptions", preconditions:{uid:.uid,resourceVersion:.resourceVersion}}' <<< "$failure")"
    # Both preconditions are enforced by Kubernetes: a new incarnation or any intervening update wins.
    if delete_output="$(printf '%s\n' "$options" | kubectl delete --raw "$path" --filename - --kubeconfig "$kubeconfig" --request-timeout=30s 2>&1)"; then
      printf '%s\n' "$delete_output"
    elif [[ "$delete_output" == *"Error from server (Conflict):"* || "$delete_output" == *"Error from server (NotFound):"* ]]; then
      echo "Function changed before conditional deletion; re-observing within the retry budget: $delete_output" >&2
      break
    else
      echo "Conditional Function deletion failed: $delete_output" >&2
      exit 1
    fi
  done < <(jq -c '.[]' <<< "$retryable_failures")
  sleep $((60 * attempt))
done
