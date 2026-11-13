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
grep -Fq 'source_tag="${SOURCE_TAG:-}"' "$PIPELINE" ||
  fail "sourceTag must enter the validation script through the task environment"
grep -Fq 'release_tag="${RELEASE_TAG:-}"' "$PIPELINE" ||
  fail "releaseTag must enter the validation script through the task environment"
grep -Fq 'source_ref="${SOURCE_REF:-}"' "$PIPELINE" ||
  fail "sourceRef must enter the validation script through the task environment"
grep -Fq 'force_publish="${FORCE_PUBLISH:-}"' "$PIPELINE" ||
  fail "forcePublish must enter the validation script through the task environment"
if grep -Eq '^[[:space:]]+(source_tag|release_tag|source_ref|force_publish)="\$\{\{ parameters\.' \
  "$PIPELINE"; then
  fail "release parameters must not be embedded in Bash source"
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
' "$PIPELINE" |
  grep -Eq '\$\{\{ parameters\.(sourceRef|sourceTag|releaseTag|forcePublish) \}\}'; then
  fail "release parameters must not appear in generated shell source"
fi
grep -Fq 'SOURCE_REF: ${{ parameters.sourceRef }}' "$PIPELINE" ||
  fail "sourceRef task environment binding is required"
grep -Fq 'SOURCE_TAG: ${{ parameters.sourceTag }}' "$PIPELINE" ||
  fail "sourceTag task environment binding is required"
grep -Fq 'RELEASE_TAG: ${{ parameters.releaseTag }}' "$PIPELINE" ||
  fail "releaseTag task environment binding is required"
grep -Fq 'FORCE_PUBLISH: ${{ parameters.forcePublish }}' "$PIPELINE" ||
  fail "forcePublish task environment binding is required"
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
if grep -Fq '$(Build.SourcesDirectory)/control' "$PIPELINE" ||
   grep -Fq '$(Build.SourcesDirectory)/release' "$PIPELINE"; then
  fail "multi-checkout paths must not depend on Build.SourcesDirectory"
fi
grep -Fq '$(Pipeline.Workspace)/s/control' "$PIPELINE" ||
  fail "protected control paths must use the stable pipeline workspace root"
grep -Fq '$(Pipeline.Workspace)/s/$(TAUGRID_SOURCE_DIR)' "$PIPELINE" ||
  fail "control and release working directories must use the stable pipeline workspace root"
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
