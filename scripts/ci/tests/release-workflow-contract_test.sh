#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly WORKFLOW="${REPO_ROOT}/.github/workflows/release-tau.yaml"

fail() {
  echo "release workflow contract test failed: $*" >&2
  exit 1
}

on_block="$(
  awk '
    /^on:$/ {
      in_on = 1
      next
    }
    in_on && /^[^[:space:]#]/ {
      exit
    }
    in_on {
      print
    }
  ' "${WORKFLOW}"
)"

grep -Eq '^[[:space:]]+workflow_dispatch:$' <<<"${on_block}" ||
  fail "workflow_dispatch must be the release workflow trigger"
if grep -Eq '^[[:space:]]+(push|pull_request|schedule):$' <<<"${on_block}"; then
  fail "release publication must not have an automatic trigger"
fi

grep -Fq "run-name: Release TauGrid \${{ inputs.tag }}" "${WORKFLOW}" ||
  fail "run name must use the required dispatch tag"
grep -Fq "RELEASE_TAG: \${{ inputs.tag }}" "${WORKFLOW}" ||
  fail "release tag must use the required dispatch input"
grep -Fq "group: release-tau-\${{ inputs.tag }}" "${WORKFLOW}" ||
  fail "release concurrency must be scoped to the dispatch tag"
grep -Fq "if [[ \"\$GITHUB_REF_TYPE\" != \"tag\" ]]; then" "${WORKFLOW}" ||
  fail "normal releases must dispatch from an existing tag ref"
if grep -Fq 'GITHUB_EVENT_NAME' "${WORKFLOW}"; then
  fail "release behavior must not retain a tag-push event path"
fi
if grep -Fq 'allow_main_release_notes' "${WORKFLOW}"; then
  fail "published legacy releases must not retain a recovery path"
fi

echo "Release workflow contract tests passed"
