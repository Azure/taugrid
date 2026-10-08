#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PIPELINE="${REPO_ROOT}/.pipelines/taugrid-unbounded-stable-nightly.yml"
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-nightly-report.sh"

fail() {
  echo "TauGrid unbounded-stable nightly contract test failed: $*" >&2
  exit 1
}

[ -f "$PIPELINE" ] || fail "pipeline is missing"
[ -x "$REPORTER" ] || fail "nightly report generator must be executable"

grep -Fq 'cron: "45 7 * * *"' "$PIPELINE" ||
  fail "pipeline must run nightly"
grep -Fq "name: 1es-aks-ai-runtime-ado-eastus2" "$PIPELINE" ||
  fail "pipeline must use the approved 1ES pool"
grep -Fq "azureSubscription: \$(imageServiceConnection)" "$PIPELINE" ||
  fail "image publication must use the approved registry service connection"
grep -Fq "value: aks ai runtime - corp" "$PIPELINE" ||
  fail "image publication must use the service connection that owns aksairuntime"
grep -Fq "azureSubscription: \$(TAUGRID_UNBOUNDED_STABLE_SERVICE_CONNECTION)" "$PIPELINE" ||
  fail "cluster deployment must use an environment-specific protected service connection"
grep -Fq "deployment: deploy_unbounded_stable" "$PIPELINE" ||
  fail "deployment must use an Azure DevOps deployment job"
grep -Fq "environment: unbounded-stable" "$PIPELINE" ||
  fail "deployment must be recorded against the first-class environment"
if ! grep -Fq "branches:" "$PIPELINE" ||
  ! grep -Fq -- "- main" "$PIPELINE"; then
  fail "nightly schedule must deploy main"
fi

for variable in \
  TAUGRID_UNBOUNDED_STABLE_ACR_NAME \
  TAUGRID_UNBOUNDED_STABLE_RESOURCE_GROUP \
  TAUGRID_UNBOUNDED_STABLE_CLUSTER_NAME \
  TAUGRID_UNBOUNDED_STABLE_SERVICE_CONNECTION; do
  grep -Fq "\$(${variable})" "$PIPELINE" ||
    fail "${variable} must be supplied by Azure DevOps"
done

grep -Fq 'image_tag="nightly-${BUILD_SOURCEVERSION:0:12}-${BUILD_BUILDID}"' "$PIPELINE" ||
  fail "nightly images must use an immutable source-and-build tag"
grep -Fq "tau-core-controller docker-push" "$PIPELINE" ||
  fail "pipeline must publish the controller from current main"
grep -Fq "taugrid-portal docker-push" "$PIPELINE" ||
  fail "pipeline must publish the portal from current main"
grep -Fq "gpu-metrics-collector docker-push" "$PIPELINE" ||
  fail "pipeline must publish the GPU collector from current main"
grep -Fq "az acr repository show" "$PIPELINE" ||
  fail "pipeline must verify published image digests"
if grep -Fq "az aks check-acr" "$PIPELINE"; then
  fail "the TME deployment identity must not query a cross-tenant corporate ACR"
fi

grep -Fq 'helm get values "${TAUGRID_RELEASE}"' "$PIPELINE" ||
  fail "deployment must preserve live operator-supplied values"
grep -Fq 'previous_revision="0"' "$PIPELINE" ||
  fail "deployment must support the first managed installation"
grep -Fq "printf '{}" "$PIPELINE" ||
  fail "first installation must start from reviewed chart defaults"
grep -Fq "scripts/ci/vendor-taugrid-dependencies.sh charts/taugrid" "$PIPELINE" ||
  fail "deployment must vendor the reviewed umbrella dependencies"
grep -Fq "cli/bin/tau \"\${install_args[@]}\" --dry-run" "$PIPELINE" ||
  fail "deployment must render the exact upgrade before mutation"
grep -Fq "cli/bin/tau \"\${install_args[@]}\"" "$PIPELINE" ||
  fail "deployment must use the supported TauGrid install path"
grep -Fq -- "--atomic" "$PIPELINE" ||
  fail "deployment must request rollback after Helm failure"
grep -Fq "tau cluster validate installation" "$PIPELINE" ||
  fail "deployment must run the TauGrid readiness gate"
grep -Fq 'helm rollback "${TAUGRID_RELEASE}" "${previous_revision}"' "$PIPELINE" ||
  fail "readiness failures must restore the previous Helm revision"
grep -Fq 'helm uninstall "${TAUGRID_RELEASE}"' "$PIPELINE" ||
  fail "a failed first installation must be removed"
grep -Fq 'recovery-validation.txt' "$PIPELINE" ||
  fail "a restored release must pass the TauGrid readiness gate"
grep -Fq "lifecycleRecorder.enabled == true" "$PIPELINE" ||
  fail "deployment must fail closed for an unpublished lifecycle recorder image"

grep -Fq "helm history" "$PIPELINE" ||
  fail "failure diagnostics must retain Helm history"
grep -Fq "helm-status.json" "$PIPELINE" ||
  fail "diagnostics must include allowlisted Helm status metadata"
if grep -Fq 'helm-status.yaml' "$PIPELINE" ||
  grep -Eq 'helm status .*--output yaml' "$PIPELINE"; then
  fail "published diagnostics must not include Helm release values, manifests, or hooks"
fi
grep -Fq "render_file=\"\$(Agent.TempDirectory)/unbounded-stable-render.txt\"" "$PIPELINE" ||
  fail "rendered manifests must remain in temporary storage"
grep -Fq "get events --sort-by=.lastTimestamp" "$PIPELINE" ||
  fail "failure diagnostics must retain Kubernetes events"
grep -Fq "condition: always()" "$PIPELINE" ||
  fail "diagnostics and reports must publish after failures"
grep -Fq '##vso[task.uploadsummary]' "$PIPELINE" ||
  fail "pipeline must upload a human-readable run summary"
grep -Fq "taugrid-unbounded-stable-nightly-report.md" "$PIPELINE" ||
  fail "pipeline must publish the nightly report"

if grep -Eqi \
  '(subscriptions/[0-9a-f-]{36}|https://[^[:space:]]*unbounded|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})' \
  "$PIPELINE"; then
  fail "pipeline must not contain subscriptions, private endpoints, or GUIDs"
fi

echo "TauGrid unbounded-stable nightly contract tests passed"
