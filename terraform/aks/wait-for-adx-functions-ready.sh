#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

subscription_id="$1"
resource_group="$2"
cluster_name="$3"
kubeconfig="$4"
chart_path="$5"
base_values_file="$6"
environment_values_file="$7"
maximum_attempts="${MAXIMUM_ADX_FUNCTION_ATTEMPTS:-6}"
function_wait_seconds="${ADX_FUNCTION_WAIT_SECONDS:-300}"

az aks get-credentials \
  --admin \
  --subscription "$subscription_id" \
  --resource-group "$resource_group" \
  --name "$cluster_name" \
  --file "$kubeconfig" \
  --overwrite-existing

for ((attempt = 1; attempt <= maximum_attempts; attempt++)); do
  helm upgrade --install adx-mon "$chart_path" \
    --namespace adx-mon \
    --create-namespace \
    --values "$base_values_file" \
    --values "$environment_values_file" \
    --wait \
    --timeout 30m

  deadline=$((SECONDS + function_wait_seconds))
  retryable_failure_names=()
  while ((SECONDS < deadline)); do
    function_statuses="$(kubectl get functions --namespace adx-mon --output=jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.observedGeneration}{"\t"}{.status.status}{"\t"}{.status.error}{"\n"}{end}')"
    if [[ -n "$function_statuses" ]]; then
      terminal_failure="$(printf '%s\n' "$function_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "PermanentFailure" && $5 !~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ {
          print $1 " (generation=" $2 ", observedGeneration=" $3 ", status=" $4 ", error=" $5 ")"
        }')"
      if [[ -n "$terminal_failure" ]]; then
        echo "adx-mon Function reconciliation reached a terminal failure: $terminal_failure" >&2
        exit 1
      fi
      mapfile -t retryable_failure_names < <(printf '%s\n' "$function_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "PermanentFailure" && $5 ~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ { print $1 }')
      if ((${#retryable_failure_names[@]} > 0)); then
        break
      fi
      if printf '%s\n' "$function_statuses" | awk -F '\t' '$2 != $3 || $4 != "Success" { exit 1 }'; then
        echo "All adx-mon Functions reached Success."
        exit 0
      fi
    fi
    sleep 15
  done

  if ((${#retryable_failure_names[@]} == 0)); then
    echo "adx-mon Functions did not reach Success within $function_wait_seconds seconds on attempt $attempt." >&2
    exit 1
  fi
  echo "adx-mon Functions reached retryable ADX throttling on attempt $attempt: ${retryable_failure_names[*]}" >&2
  if ((attempt == maximum_attempts)); then
    echo "adx-mon Functions did not recover from ADX throttling after $maximum_attempts attempts." >&2
    exit 1
  fi

  kubectl delete functions --namespace adx-mon "${retryable_failure_names[@]}" --ignore-not-found
  sleep $((60 * attempt))
done
