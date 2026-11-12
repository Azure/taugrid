#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PIPELINE="${REPO_ROOT}/.pipelines/publish-helm-charts.yml"
readonly VALIDATION_WORKFLOW="${REPO_ROOT}/.github/workflows/taugrid-validation.yml"

fail() {
  echo "release publisher contract test failed: $*" >&2
  exit 1
}

[[ ! -e "${REPO_ROOT}/.github/workflows/release-tau.yaml" ]] ||
  fail "GitHub must not remain the release orchestrator"
[[ ! -e "${REPO_ROOT}/scripts/ci/release_backend.py" ]] ||
  fail "repository-to-publisher queue identity code must be removed"
grep -Fq -- '- name: sourceRef' "$PIPELINE" ||
  fail "chart publisher must require an exact source SHA for releases"
grep -Fq -- '- name: sourceTag' "$PIPELINE" ||
  fail "chart publisher must require the annotated source tag"
grep -Fq -- '- name: releaseTag' "$PIPELINE" ||
  fail "chart publisher must require the release version"
grep -Fq -- '- name: forcePublish' "$PIPELINE" ||
  fail "chart publisher must reject release overwrite requests"
grep -Fq 'repository: releaseSource' "$PIPELINE" ||
  fail "chart publisher must separate protected control from release source"
grep -Fq "test \"\$force_publish\" = \"False\"" "$PIPELINE" ||
  fail "release publication must reject forcePublish"
grep -Fq "test \"\$release_tag\" = \"\${source_tag#v}\"" "$PIPELINE" ||
  fail "source and release tags must agree"
grep -Fq "test \"\$BUILD_SOURCEBRANCH\" = refs/heads/main" "$PIPELINE" ||
  fail "release publisher control must execute from main"
grep -Fq 'rev-parse origin/main^{commit}' "$PIPELINE" ||
  fail "release publisher control must use the current protected main revision"
grep -Fq 'validate_release_source.py' "$PIPELINE" ||
  fail "release source must be independently validated"
grep -Fq 'TAUGRID_SOURCE_ROOT:' "$PIPELINE" ||
  fail "chart packaging must use the separately pinned release source"
grep -Fq 'persistCredentials: false' "$PIPELINE" ||
  fail "release checkouts must not persist repository credentials"
if grep -Eq 'RELEASE_(TENANT|CLIENT|OIDC|PUBLISHERS_CONFIG)|id-token: write' "$PIPELINE"; then
  fail "chart publication must not retain the repository-to-publisher identity"
fi
grep -Fq 'version="0.0.0-preflight"' "${VALIDATION_WORKFLOW}" ||
  fail "release validation must use a release-neutral synthetic version"
if grep -Fq 'charts/taugrid/Chart.yaml has invalid release version' "${VALIDATION_WORKFLOW}" ||
   grep -Eq 'cli/releases/v.*required for release preflight' "${VALIDATION_WORKFLOW}"; then
  fail "release validation must not derive a version or require notes from release-neutral source"
fi

echo "Release publisher contract tests passed"
