#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PUBLISHER="${REPO_ROOT}/.pipelines/publish-helm-charts.yml"
readonly READINESS_PIPELINE="${REPO_ROOT}/.github/workflows/release-ready.yaml"
readonly LEGACY_RELEASE="${REPO_ROOT}/.github/workflows/release-tau.yaml"

fail() {
  echo "release publisher contract test failed: $*" >&2
  exit 1
}

for parameter in releaseTag releaseSourceCommit releaseVersion; do
  grep -Fq -- "- name: ${parameter}" "$PUBLISHER" ||
    fail "protected publisher must define ${parameter}"
done
grep -Fq 'RELEASE_TAG: ${{ parameters.releaseTag }}' "$PUBLISHER" ||
  fail "releaseTag must enter tasks through the environment"
grep -Fq 'RELEASE_SOURCE_COMMIT: ${{ parameters.releaseSourceCommit }}' "$PUBLISHER" ||
  fail "releaseSourceCommit must enter tasks through the environment"
grep -Fq 'RELEASE_VERSION: ${{ parameters.releaseVersion }}' "$PUBLISHER" ||
  fail "releaseVersion must enter tasks through the environment"
if grep -Eq -- '- name: (sourceRef|sourceTag|forcePublish)' "$PUBLISHER"; then
  fail "protected publisher must expose only the approved release interface"
fi

grep -Fq 'repository: releaseSource' "$PUBLISHER" ||
  fail "protected publisher must define a separate release source"
grep -Fq 'job: package_charts' "$PUBLISHER" ||
  fail "protected publisher must package without credentials"
grep -Fq 'job: publish_charts' "$PUBLISHER" ||
  fail "protected publisher must publish in a fresh job"
grep -Fq 'PublishPipelineArtifact@1' "$PUBLISHER" ||
  fail "sealed packages must cross the job boundary as a pipeline artifact"
grep -Fq 'DownloadPipelineArtifact@2' "$PUBLISHER" ||
  fail "the credentialed job must download sealed packages"
grep -Fq 'artifact: taugrid-chart-publication' "$PUBLISHER" ||
  fail "publisher must emit the machine-verifiable completion artifact"
grep -Fq 'AzureCLI@2' "$PUBLISHER" ||
  fail "only the protected publisher may receive registry credentials"
grep -Fq 'validate_release_source.py' "$PUBLISHER" ||
  fail "the package job must validate the annotated source tag and SHA"
if grep -Fq 'publish-release-helm-charts.sh' "$PUBLISHER"; then
  fail "publisher must invoke the canonical protected helper"
fi
grep -Fq 'scripts/ci/publish-helm-charts.sh' "$PUBLISHER" ||
  fail "publisher must invoke the canonical protected helper"
grep -Fq 'verify_protected_chart_packages.py' \
  "${REPO_ROOT}/scripts/ci/publish-helm-charts.sh" ||
  fail "the publisher must verify sealed packages without a source checkout"
grep -Fq 'test "$release_version" = "${release_tag#v}"' "$PUBLISHER" ||
  fail "release tag and version must agree"
grep -Fq 'rev-parse origin/main^{commit}' "$PUBLISHER" ||
  fail "publisher control must execute from current protected main"
grep -Fq 'persistCredentials: false' "$PUBLISHER" ||
  fail "publisher checkouts must not persist repository credentials"

package_block=$(awk '
  /- job: package_charts/ { capture = 1 }
  /- job: publish_charts/ { capture = 0 }
  capture { print }
' "$PUBLISHER")
publish_block=$(awk '
  /- job: publish_charts/ { capture = 1 }
  capture { print }
' "$PUBLISHER")
grep -Fq 'checkout: releaseSource' <<<"$package_block" ||
  fail "the unprivileged package job must receive the release source"
if grep -Fq 'AzureCLI@2' <<<"$package_block"; then
  fail "the package job must not receive publication credentials"
fi
if grep -Fq 'checkout: releaseSource' <<<"$publish_block"; then
  fail "the credentialed publish job must not receive the release source"
fi
if grep -Fq 's/release' <<<"$publish_block"; then
  fail "the credentialed publish job must not access the release workspace"
fi

if awk '
  /^[[:space:]]*(- )?bash: \|[[:space:]]*$/ ||
  /^[[:space:]]*inlineScript: \|[[:space:]]*$/ {
    in_block = 1
    content_indent = -1
    next
  }
  in_block {
    if ($0 ~ /^[[:space:]]*$/) {
      print
      next
    }
    line_indent = match($0, /[^ ]/) - 1
    if (content_indent < 0) {
      content_indent = line_indent
    }
    if (line_indent < content_indent) {
      in_block = 0
      next
    }
    print
  }
' "$PUBLISHER" |
  grep -Eq '\$\{\{ parameters\.(releaseTag|releaseSourceCommit|releaseVersion) \}\}'; then
  fail "release parameters must not appear in generated shell source"
fi

if grep -Fq 'tests/scripts_test.sh' \
  "${REPO_ROOT}/scripts/ci/publish-helm-charts.sh"; then
  fail "protected packaging must not execute release-selected chart scripts"
fi
for makefile in \
  images/tau/Makefile \
  images/taugrid-portal/Makefile \
  images/tau-core-controller/Makefile \
  images/taugrid-metrics-collector/Makefile; do
  grep -Fq 'DOCKER_BUILD_ARGS ?=' "${REPO_ROOT}/${makefile}" ||
    fail "${makefile} must accept protected provenance build arguments"
  grep -Fq '$(DOCKER_BUILD_ARGS)' "${REPO_ROOT}/${makefile}" ||
    fail "${makefile} must pass protected provenance build arguments"
done
grep -Fq 'verify-release-readiness.py' "$READINESS_PIPELINE" ||
  fail "public readiness must remain a separate read-only verifier"
if grep -Eq 'AzureCLI@2|azureServiceConnection|az acr login|helm push' \
  "$READINESS_PIPELINE"; then
  fail "public readiness must not receive publication credentials"
fi
[[ -e "$LEGACY_RELEASE" ]] ||
  fail "the supported GitHub release workflow must remain available"

echo "Release publisher contract tests passed"
