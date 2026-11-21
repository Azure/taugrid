#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PIPELINE="${REPO_ROOT}/.pipelines/taugrid-unbounded-stable-nightly.yml"
readonly IMAGE_BUILDER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-build-image.sh"
readonly IMAGE_BUILDER_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-unbounded-stable-build-image_test.sh"
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-nightly-report.sh"
readonly RECOVER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-nightly-recover.sh"
readonly MANAGED_PREFLIGHT="${REPO_ROOT}/scripts/ci/taugrid-managed-control-plane-preflight.sh"
readonly MANAGED_PREFLIGHT_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-managed-control-plane-preflight_test.sh"
readonly SMOKE="${REPO_ROOT}/tests/e2e/cmd/taugrid-nightly-smoke/runner.go"
readonly SMOKE_TEST="${REPO_ROOT}/tests/e2e/cmd/taugrid-nightly-smoke/runner_test.go"
readonly RECOVER_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-unbounded-stable-nightly-recover_test.sh"

fail() {
  echo "TauGrid unbounded-stable nightly contract test failed: $*" >&2
  exit 1
}

job_block() {
  local job="$1"
  awk -v marker="  - job: ${job}" '
    $0 == marker {
      in_job = 1
    }
    in_job && $0 ~ /^  - (job|deployment): / && $0 != marker {
      exit
    }
    in_job {
      print
    }
  ' "$PIPELINE"
}

[ -f "$PIPELINE" ] || fail "pipeline is missing"
[ -x "$IMAGE_BUILDER" ] || fail "nightly image builder must be executable"
[ -x "$IMAGE_BUILDER_TEST" ] || fail "nightly image builder test must be executable"
[ -x "$REPORTER" ] || fail "nightly report generator must be executable"
[ -x "$RECOVER" ] || fail "nightly recovery helper must be executable"
[ -x "$MANAGED_PREFLIGHT" ] || fail "managed control-plane preflight must be executable"
[ -x "$MANAGED_PREFLIGHT_TEST" ] || fail "managed control-plane preflight test must be executable"
[ -f "$SMOKE" ] || fail "Tau-native nightly Go runner is missing"
[ -f "$SMOKE_TEST" ] || fail "Tau-native nightly Go runner tests are missing"
[ -x "$RECOVER_TEST" ] || fail "nightly recovery fixture must be executable"

grep -Fq 'cron: "45 7 * * *"' "$PIPELINE" ||
  fail "pipeline must run nightly"
grep -Fq "name: \$(TAUGRID_UNBOUNDED_STABLE_POOL)" "$PIPELINE" ||
  fail "pipeline must use a protected environment-specific pool"
grep -Fq "azureSubscription: \$(TAUGRID_UNBOUNDED_STABLE_IMAGE_SERVICE_CONNECTION)" "$PIPELINE" ||
  fail "image publication must use a protected environment-specific service connection"
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
  TAUGRID_UNBOUNDED_STABLE_SERVICE_CONNECTION \
  TAUGRID_UNBOUNDED_STABLE_IMAGE_SERVICE_CONNECTION \
  TAUGRID_UNBOUNDED_STABLE_IMAGE_REPOSITORY_PREFIX \
  TAUGRID_UNBOUNDED_STABLE_POOL; do
  grep -Fq "\$(${variable})" "$PIPELINE" ||
    fail "${variable} must be supplied by Azure DevOps"
done

grep -Fq 'image_tag="nightly-${BUILD_SOURCEVERSION:0:12}-${BUILD_BUILDID}"' "$PIPELINE" ||
  fail "image verification must use an immutable source-and-build tag"
grep -Fq 'image_tag="nightly-${BUILD_SOURCEVERSION:0:12}-${BUILD_BUILDID}"' "$IMAGE_BUILDER" ||
  fail "nightly image builds must use an immutable source-and-build tag"
for job in \
  build_controller_image \
  build_portal_image \
  build_collector_image; do
  block="$(job_block "$job")"
  [[ -n "$block" ]] ||
    fail "${job} must be an independent image build job"
  grep -Fq "    dependsOn: preflight" <<<"$block" ||
    fail "${job} must start after preflight without another image dependency"
  grep -Fq "    timeoutInMinutes: 180" <<<"$block" ||
    fail "${job} must retain the existing image build timeout"
  case "$job" in
    build_controller_image) component=controller ;;
    build_portal_image) component=portal ;;
    build_collector_image) component=collector ;;
  esac
  grep -Fq "scriptPath: scripts/ci/taugrid-unbounded-stable-build-image.sh" <<<"$block" ||
    fail "${job} must use the reviewed nightly image builder"
  grep -Fq "arguments: ${component}" <<<"$block" ||
    fail "${job} must publish its assigned image"
done

verification_block="$(job_block build_images)"
[[ -n "$verification_block" ]] ||
  fail "image verification job is missing"
for dependency in \
  build_controller_image \
  build_portal_image \
  build_collector_image; do
  grep -Fq "      - ${dependency}" <<<"$verification_block" ||
    fail "image verification must depend on ${dependency}"
done
grep -Fq "    condition: always()" <<<"$verification_block" ||
  fail "image verification must retain failures from independent builds"
if grep -Fq "make -C images/" <<<"$verification_block"; then
  fail "image verification must not serialize image builds"
fi
grep -Fq 'CONTROLLER_IMAGE_JOB_RESULT: $[ dependencies.build_controller_image.result ]' <<<"$verification_block" ||
  fail "image verification must retain controller build failures"
grep -Fq 'PORTAL_IMAGE_JOB_RESULT: $[ dependencies.build_portal_image.result ]' <<<"$verification_block" ||
  fail "image verification must retain portal build failures"
grep -Fq 'COLLECTOR_IMAGE_JOB_RESULT: $[ dependencies.build_collector_image.result ]' <<<"$verification_block" ||
  fail "image verification must retain collector build failures"
grep -Fq 'if [[ "${result}" != "Succeeded" ]]' <<<"$verification_block" ||
  fail "image verification must fail when an independent build does not succeed"
grep -Fq "tau-core-controller docker-push" "$IMAGE_BUILDER" ||
  fail "pipeline must publish the controller from current main"
grep -Fq "taugrid-portal docker-push" "$IMAGE_BUILDER" ||
  fail "pipeline must publish the portal from current main"
grep -Fq "gpu-metrics-collector docker-push" "$IMAGE_BUILDER" ||
  fail "pipeline must publish the GPU collector from current main"
if [[ "$(grep -Fc "PLATFORMS=linux/amd64,linux/arm64" "$IMAGE_BUILDER")" -ne 3 ]]; then
  fail "every nightly image must publish amd64 and arm64"
fi
grep -Fq "az acr repository show" "$PIPELINE" ||
  fail "pipeline must verify published image digests"
if grep -Fq "az aks check-acr" "$PIPELINE"; then
  fail "the deployment identity must not query the image registry"
fi

grep -Fq 'helm get values "${TAUGRID_RELEASE}"' "$PIPELINE" ||
  fail "deployment must preserve live operator-supplied values"
grep -Fq "previous_revision=\"\$(jq -r '.revision' <<<\"\${release_state}\")\"" "$PIPELINE" ||
  fail "deployment must support the first managed installation"
grep -Fq "printf '{}" "$PIPELINE" ||
  fail "first installation must start from reviewed chart defaults"
grep -Fq "scripts/ci/vendor-taugrid-dependencies.sh charts/taugrid" "$PIPELINE" ||
  fail "deployment must vendor the reviewed umbrella dependencies"
grep -Fq "cli/bin/tau \"\${install_args[@]}\" --dry-run" "$PIPELINE" ||
  fail "deployment must render the exact upgrade before mutation"
grep -Fq "cli/bin/tau \"\${install_args[@]}\"" "$PIPELINE" ||
  fail "deployment must use the supported TauGrid install path"
grep -Fq -- '--set "components.kueue.enabled=false"' "$PIPELINE" ||
  fail "nightly must not install or own managed Kueue"
grep -Fq -- '--set "components.kuberayOperator.enabled=false"' "$PIPELINE" ||
  fail "nightly must not install or own managed KubeRay"
[ "$(grep -Fc 'taugrid-managed-control-plane-preflight.sh unbounded-stable' "$PIPELINE")" -eq 2 ] ||
  fail "managed Kueue and KubeRay must pass preflight and postflight readiness checks"
if grep -Eq -- '--take-ownership|kubectl .*(label|annotate|delete).*crd' "$PIPELINE" "$MANAGED_PREFLIGHT"; then
  fail "nightly must never adopt, relabel, or delete externally managed CRDs"
fi
grep -Fq -- "--atomic" "$PIPELINE" ||
  fail "deployment must request rollback after Helm failure"
grep -Fq "tau cluster validate installation" "$PIPELINE" ||
  fail "deployment must run the TauGrid readiness gate"
grep -Fq 'go -C tests/e2e test ./cmd/taugrid-nightly-smoke' "$PIPELINE" ||
  fail "preflight must test the Tau-native nightly Go runner"
grep -Fq 'go -C tests/e2e build' "$PIPELINE" ||
  fail "deployment must build the Tau-native nightly Go runner"
grep -Fq '"${tools_dir}/taugrid-nightly-smoke"' "$PIPELINE" ||
  fail "deployment must run the Tau-native nightly Go runner"
grep -Fq -- '--tau "$(Build.SourcesDirectory)/cli/bin/tau"' "$PIPELINE" ||
  fail "Tau-native smoke must use the CLI built from the deployed source"
grep -Fq "Tau-native workload smoke failed" "$PIPELINE" ||
  fail "Tau-native smoke failures must enter deployment recovery"
grep -Fq "taugrid-unbounded-stable-nightly-recover.sh inspect" "$PIPELINE" ||
  fail "release absence must be established through structured inspection"
grep -Fq "Existing TauGrid release is not deployed" "$PIPELINE" ||
  fail "deployment must fail closed for a non-deployed existing release"
grep -Fq "installation or embedded readiness validation failed" "$PIPELINE" ||
  fail "embedded install readiness failures must enter recovery"
grep -Fq "taugrid-unbounded-stable-nightly-recover.sh recover" "$PIPELINE" ||
  fail "readiness failures must restore the previous release state"
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

grep -Fq '"workspace", "create"' "$SMOKE" ||
  fail "Tau-native smoke must provision its workspace through tau"
grep -Fq '"run", "validate"' "$SMOKE" ||
  fail "Tau-native smoke must validate the exact Tau config"
grep -Fq -- '--dry-run=client' "$SMOKE" ||
  fail "Tau-native smoke must client-dry-run the exact Tau config"
grep -Fq -- '--dry-run=server' "$SMOKE" ||
  fail "Tau-native smoke must server-dry-run the exact Tau config"
grep -Fq '"run", "--config"' "$SMOKE" ||
  fail "Tau-native smoke must submit through tau run"
grep -Fq '"run", "status"' "$SMOKE" ||
  fail "Tau-native smoke must observe lifecycle through tau"
grep -Fq '"logs", r.options.runName' "$SMOKE" ||
  fail "Tau-native smoke must verify workload progress through tau logs"
grep -Fq "TAUGRID_NIGHTLY_SMOKE_COMPLETE" "$SMOKE" ||
  fail "Tau-native smoke must require the workload completion marker"
grep -Fq '"placement":         "same-host"' "$SMOKE" ||
  fail "Tau-native smoke profile must require hostname topology"
grep -Fq "waitTopologyVerified" "$SMOKE" ||
  fail "Tau-native smoke must verify Kueue topology admission and worker placement"

if grep -Eqi \
  '(subscriptions/[0-9a-f-]{36}|https://[^[:space:]]*unbounded|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})' \
  "$PIPELINE"; then
  fail "pipeline must not contain internal infrastructure coordinates"
fi

"$IMAGE_BUILDER_TEST"
"$MANAGED_PREFLIGHT_TEST"

echo "TauGrid unbounded-stable nightly contract tests passed"
