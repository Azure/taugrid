#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

run_waiter() {
  local function_statuses="$1"

  (
    az() { :; }
    helm() { :; }
    kubectl() {
      if [[ "$1" == "get" ]]; then
        printf '%s\n' "$function_statuses"
      fi
    }
    sleep() { :; }

    export ADX_FUNCTION_WAIT_SECONDS=1
    export MAXIMUM_ADX_FUNCTION_ATTEMPTS=1
    set -- subscription resource-group cluster kubeconfig chart base-values environment-values
    source "$script_directory/wait-for-adx-functions-ready.sh"
  )
}

run_waiter $'metrics\tSuccess\nlogs\tSuccess'

if run_waiter $'metrics\tPending'; then
  echo "The waiter accepted a Function that was not successful." >&2
  exit 1
fi
