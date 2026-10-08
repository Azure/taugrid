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

job_block() {
  local job="$1"
  awk -v job="${job}" '
    $0 == "  " job ":" {
      in_job = 1
    }
    in_job && $0 ~ /^  [[:alnum:]_-]+:$/ && $0 != "  " job ":" {
      exit
    }
    in_job {
      print
    }
  ' "${WORKFLOW}"
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
validate_block="$(job_block validate)"
sbom_block="$(job_block sbom)"
publish_block="$(job_block publish)"
if grep -Fq 'releases/generate-notes' <<<"${validate_block}"; then
  fail "read-only validation must not call the write-authorized generate-notes endpoint"
fi
grep -Fq 'contents: write' <<<"${publish_block}" ||
  fail "release publication must retain narrowly scoped contents write permission"
grep -Fq "\"/repos/\$GITHUB_REPOSITORY/releases/generate-notes\"" <<<"${publish_block}" ||
  fail "release notes must include GitHub-generated change and contributor attribution"
grep -Fq "cat \"\$generated\"" <<<"${publish_block}" ||
  fail "generated change and contributor notes must be appended to curated notes"
grep -Fq 'SYFT_VERSION: 1.54.1' <<<"${sbom_block}" ||
  fail "release SBOM generation must pin Syft"
grep -Eq 'SYFT_SHA256: [0-9a-f]{64}' <<<"${sbom_block}" ||
  fail "release SBOM generation must pin and verify the Syft archive digest"
grep -Fq 'release_sbom.py resolve-images' <<<"${sbom_block}" ||
  fail "release SBOM generation must resolve public image tags to digests"
grep -Fq 'release_sbom.py generate' <<<"${sbom_block}" ||
  fail "release SBOM generation must cover CLI binaries and coordinated images"
grep -Fq "tau-release-sbom-\${{ env.RELEASE_TAG }}" <<<"${sbom_block}" ||
  fail "SBOM-enriched assets must be transferred separately from reproducible binaries"
grep -Fq "name: tau-release-sbom-\${{ env.RELEASE_TAG }}" <<<"${publish_block}" ||
  fail "publication must consume the SBOM-enriched release artifact"
grep -Fq 'release_sbom.py validate' <<<"${publish_block}" ||
  fail "publication must validate the complete SBOM index and asset set"
if grep -Eq 'docker (build|push)|cosign|attest|referrer' <<<"${sbom_block}"; then
  fail "GitHub release SBOM generation must not build, push, sign, or attest images"
fi

echo "Release workflow contract tests passed"
