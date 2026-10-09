#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PIPELINE="${REPO_ROOT}/.pipelines/taugrid-flex-nightly.yml"
readonly PREFLIGHT="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-preflight.sh"
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-report.sh"
readonly CLI_SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-cli-smoke.sh"
readonly CLI_SMOKE_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-cli-smoke_test.sh"
readonly TAUCLUSTER_RECONCILE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-taucluster-reconcile.sh"
readonly TAUCLUSTER_RECONCILE_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-taucluster-reconcile_test.sh"
readonly GPU_ROUTING_SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-gpu-routing-smoke.sh"
readonly GPU_ROUTING_SMOKE_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-gpu-routing-smoke_test.sh"
readonly GPU_PROFILE_SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-gpu-profile-smoke.sh"
readonly GPU_PROFILE_SMOKE_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-gpu-profile-smoke_test.sh"
readonly STORAGE_SMOKE="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-storage-smoke.sh"
readonly STORAGE_SMOKE_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-storage-smoke_test.sh"
readonly KUSTO_CHECK="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-kusto-check.sh"
readonly KUSTO_CHECK_TEST="${REPO_ROOT}/scripts/ci/tests/taugrid-flex-nightly-kusto-check_test.sh"
readonly QUEUE_OVERLAY="${REPO_ROOT}/cluster-overlays/queues/shared-gpu-queue.yaml"

fail() {
  echo "TauGrid Flex nightly contract test failed: $*" >&2
  exit 1
}

[ -f "$PIPELINE" ] || fail "pipeline is missing"
[ ! -e "${REPO_ROOT}/.github/workflows/taugrid-flex-nightly.yml" ] ||
  fail "nightly execution must not retain a GitHub Actions workflow"
[ -x "$PREFLIGHT" ] || fail "preflight must be executable"
[ -x "$REPORTER" ] || fail "nightly report generator must be executable"
[ -x "$CLI_SMOKE" ] || fail "Tau CLI smoke helper must be executable"
[ -x "$CLI_SMOKE_TEST" ] || fail "Tau CLI smoke fixture must be executable"
[ -x "$TAUCLUSTER_RECONCILE" ] || fail "TauCluster reconcile helper must be executable"
[ -x "$TAUCLUSTER_RECONCILE_TEST" ] || fail "TauCluster reconcile fixture must be executable"
[ -x "$GPU_ROUTING_SMOKE" ] || fail "GPU routing helper must be executable"
[ -x "$GPU_ROUTING_SMOKE_TEST" ] || fail "GPU routing fixture must be executable"
[ -x "$GPU_PROFILE_SMOKE" ] || fail "GPU profile smoke helper must be executable"
[ -x "$GPU_PROFILE_SMOKE_TEST" ] || fail "GPU profile smoke fixture must be executable"
[ -x "$STORAGE_SMOKE" ] || fail "storage smoke helper must be executable"
[ -x "$STORAGE_SMOKE_TEST" ] || fail "storage smoke fixture must be executable"
[ -x "$KUSTO_CHECK" ] || fail "Kusto check helper must be executable"
[ -x "$KUSTO_CHECK_TEST" ] || fail "Kusto check fixture must be executable"
[ -f "$QUEUE_OVERLAY" ] || fail "shared GPU queue overlay is missing"

grep -Fq 'readonly A100_SITE="${FLEX_NIGHTLY_A100_SITE:-}"' "$PREFLIGHT" ||
  fail "A100 site must be supplied through ADO variables"
grep -Fq 'readonly H100_SITE="${FLEX_NIGHTLY_H100_SITE:-}"' "$PREFLIGHT" ||
  fail "H100 site must be supplied through ADO variables"
grep -Fq 'readonly H200_SITE="${FLEX_NIGHTLY_H200_SITE:-}"' "$PREFLIGHT" ||
  fail "H200 site must be supplied through ADO variables"
grep -Fq 'readonly DGX_SITE="${FLEX_NIGHTLY_DGX_SITE:-}"' "$PREFLIGHT" ||
  fail "DGX site must be supplied through ADO variables"
if grep -Fq "tau.azure.com/node-pool:" "$QUEUE_OVERLAY"; then
  fail "environment-specific DGX selector must not be published in the shared queue overlay"
fi

grep -Fq 'cron: "15 8 * * *"' "$PIPELINE" ||
  fail "pipeline must run nightly"
grep -Fq "name: 1es-aks-ai-runtime-ado-eastus2" "$PIPELINE" ||
  fail "pipeline must run on the approved 1ES AKS AI Runtime pool"
grep -Fq "value: aks ai runtime - corp" "$PIPELINE" ||
  fail "cluster operations must use the corp Azure DevOps service connection"
grep -Fq "value: aks ai runtime - prod" "$PIPELINE" ||
  fail "registry operations must use the prod Azure DevOps service connection"
[[ "$(grep -Fc 'azureSubscription: $(azureClusterServiceConnection)' "$PIPELINE")" -eq 10 ]] ||
  fail "every cluster-facing AzureCLI task must use the corp service connection"
[[ "$(grep -Fc 'azureSubscription: $(azureRegistryServiceConnection)' "$PIPELINE")" -eq 1 ]] ||
  fail "only the image publishing task may use the prod service connection"
grep -Fq 'value: $(AKS_AI_RUNTIME_FLEX_SUBSCRIPTION_ID)' "$PIPELINE" ||
  fail "pipeline must receive the Flex cluster subscription through an ADO variable"
credential_commands="$(grep -Fc "az aks get-credentials" "$PIPELINE")"
subscription_routes="$(grep -Fc -- '--subscription "${FLEX_NIGHTLY_CLUSTER_SUBSCRIPTION_ID}"' "$PIPELINE")"
[[ "$credential_commands" -gt 0 && "$credential_commands" -eq "$subscription_routes" ]] ||
  fail "every AKS credential request must explicitly target the Flex cluster subscription"
missing_context_routes="$(
  awk '
    /az aks get-credentials/ {
      active = 1
      has_context = 0
    }
    active && /--context flex-nightly/ {
      has_context = 1
    }
    active && /--overwrite-existing/ {
      if (!has_context) {
        missing++
      }
      active = 0
    }
    END {
      print missing + 0
    }
  ' "$PIPELINE"
)"
[[ "$missing_context_routes" -eq 0 ]] ||
  fail "every AKS credential request must create the shared flex-nightly context"
grep -Fq "deployment: deploy_flex" "$PIPELINE" ||
  fail "current main must be deployed through an Azure DevOps deployment job"
grep -Fq "job: taucluster_reconcile" "$PIPELINE" ||
  fail "nightly must reconcile the externally owned TauCluster before preflight"
grep -Fq "taugrid-flex-nightly-taucluster-reconcile.sh" "$PIPELINE" ||
  fail "nightly must invoke the checked-in TauCluster reconcile helper"
grep -Fq "scripts/ci/tests/taugrid-flex-nightly-taucluster-reconcile_test.sh" "$PIPELINE" ||
  fail "nightly must validate TauCluster reconciliation before cluster mutation"
grep -Fq "environment: flex" "$PIPELINE" ||
  fail "deployment must be recorded against the Flex environment"
grep -Fq 'image_tag="nightly-${BUILD_SOURCEVERSION:0:12}-${BUILD_BUILDID}"' "$PIPELINE" ||
  fail "nightly images must use an immutable source-and-build tag"
grep -Fq "tau-core-controller docker-push" "$PIPELINE" ||
  fail "pipeline must publish the controller from current main"
grep -Fq "taugrid-portal docker-push" "$PIPELINE" ||
  fail "pipeline must publish the portal from current main"
grep -Fq "gpu-metrics-collector docker-push" "$PIPELINE" ||
  fail "pipeline must publish the GPU collector from current main"
grep -Fq 'helm get values "${TAUGRID_RELEASE}"' "$PIPELINE" ||
  fail "deployment must preserve live operator-supplied values"
grep -Fq -- '--set "components.kueue.enabled=false"' "$PIPELINE" ||
  fail "deployment must not replace the shared Kueue controller"
grep -Fq -- '--set "components.kuberayOperator.enabled=false"' "$PIPELINE" ||
  fail "deployment must not replace the shared KubeRay operator"
grep -Fq -- '--set "baselineQueue.enabled=false"' "$PIPELINE" ||
  fail "deployment must not replace shared queue policy"
grep -Fq "refusing to transfer ownership during a nightly" "$PIPELINE" ||
  fail "deployment must fail closed instead of changing shared controller ownership"
grep -Fq "The managed Flex TauGrid release is absent" "$PIPELINE" ||
  fail "nightly deployment must refuse to bootstrap over an unmanaged installation"
grep -Fq "cli/bin/tau \"\${install_args[@]}\" --dry-run" "$PIPELINE" ||
  fail "deployment must render the exact upgrade before mutation"
grep -Fq "current_profiles=\"\$(jq -c" "$PIPELINE" ||
  fail "deployment must preserve the live TauCluster profile catalog"
grep -Fq 'map(select(.name != $profile.name)) + [$profile]' "$PIPELINE" ||
  fail "deployment must append or replace only the dedicated nightly CPU profile"
grep -Fq "value: nightly.cpu.1x" "$PIPELINE" ||
  fail "Tau CLI Job and Ray smoke must use the dedicated zero-GPU profile"
grep -Fq -- "--atomic" "$PIPELINE" ||
  fail "deployment must request rollback after Helm failure"
grep -Fq "taugrid-unbounded-stable-nightly-recover.sh recover" "$PIPELINE" ||
  fail "deployment failures must restore the previous release"
grep -Fq "job: tau_cli_smoke" "$PIPELINE" ||
  fail "pipeline must run the Tau CLI functional smoke"
grep -Fq "dependsOn: deploy_flex" "$PIPELINE" ||
  fail "Tau CLI smoke must run against the newly deployed revision"
grep -Fq "scripts/ci/taugrid-flex-nightly-cli-smoke.sh" "$PIPELINE" ||
  fail "pipeline must invoke the checked-in Tau CLI smoke helper"
grep -Fq "FLEX_NIGHTLY_TAU_JOB_PROFILE: \$(FLEX_NIGHTLY_TAU_JOB_PROFILE)" "$PIPELINE" ||
  fail "Tau CLI smoke must receive the explicit nightly Job profile"
grep -Fq "job: gpu_profile_smoke" "$PIPELINE" ||
  fail "pipeline must exercise deployed GPU workload profiles"
grep -Fq "job: gpu_queue_routing" "$PIPELINE" ||
  fail "pipeline must verify exact Tau GPU-class queue routing"
grep -Fq "scripts/ci/taugrid-flex-nightly-gpu-routing-smoke.sh" "$PIPELINE" ||
  fail "pipeline must invoke the checked-in GPU routing helper"
grep -Fq "scripts/ci/tests/taugrid-flex-nightly-gpu-routing-smoke_test.sh" "$PIPELINE" ||
  fail "pipeline must validate the GPU routing helper before live execution"
grep -Fq "dependsOn: gpu_queue_routing" "$PIPELINE" ||
  fail "GPU profile validation must wait for exact GPU-class routing"
grep -Fq "scripts/ci/taugrid-flex-nightly-gpu-profile-smoke.sh" "$PIPELINE" ||
  fail "pipeline must invoke the checked-in GPU profile helper"
grep -Fq "group_by(.shape)" "$GPU_PROFILE_SMOKE" ||
  fail "live GPU profile tests must deduplicate equivalent team variants"
grep -Fq -- "--dry-run=server" "$GPU_PROFILE_SMOKE" ||
  fail "every ready GPU profile must receive a server-side dry-run"
grep -Fq "TAU_GPU_PROFILE_EXPECTED_GPUS" "$GPU_PROFILE_SMOKE" ||
  fail "live GPU profile checks must verify the profile GPU cardinality"
grep -Fq "dependsOn: gpu_profile_smoke" "$PIPELINE" ||
  fail "storage validation must wait for GPU profile checks"
grep -Fq "job: storage_smoke" "$PIPELINE" ||
  fail "pipeline must validate the pre-provisioned Azure Blob PVC"
grep -Fq "scripts/ci/taugrid-flex-nightly-storage-smoke.sh" "$PIPELINE" ||
  fail "pipeline must invoke the checked-in storage smoke helper"
grep -Fq "dependsOn: tau_cli_smoke" "$PIPELINE" ||
  fail "GPU profile validation must wait for bread-and-butter Tau commands"
grep -Fq "dependsOn: storage_smoke" "$PIPELINE" ||
  fail "hardware qualification must wait for cross-region storage validation"
grep -Fq "AKS_AI_RUNTIME_FLEX_STORAGE_NAMESPACE" "$PIPELINE" ||
  fail "storage smoke must use the configured platform-owned PVC namespace"
grep -Fq "AKS_AI_RUNTIME_FLEX_STORAGE_ACCOUNT_REGION" "$PIPELINE" ||
  fail "storage smoke must verify the configured Azure Blob account region"
grep -Fq "tau.azure.com/site: \${site}" "$STORAGE_SMOKE" ||
  fail "storage Jobs must pin A100 and H200 validation to their configured sites"
grep -Fq "post-deploy-hardware-contract.json" "$PIPELINE" ||
  fail "hardware capability must be revalidated after the TauGrid deployment"
grep -Fq "value: shared-cluster-controllers" "$PIPELINE" ||
  fail "pipeline must explicitly use the safe shared-controller mode"
grep -Fq "taugrid-flex-nightly-preflight.sh cluster" "$PIPELINE" ||
  fail "pipeline must run cluster preflight before workloads"
grep -Fq "compare-capability" "$PIPELINE" ||
  fail "pipeline must compare Flex node capability after cleanup"
grep -Fq "capability_violations" "$PREFLIGHT" ||
  fail "preflight must expose Flex node capability violations"
grep -Fq "lease_age_seconds" "$PREFLIGHT" ||
  fail "preflight must validate Flex node lease freshness"
grep -Fq "value: taugrid-gpu-topology" "$PIPELINE" ||
  fail "pipeline must require the authoritative TauGrid GPU topology"
grep -Fq "name: rdmaConformance" "$PIPELINE" ||
  fail "H200 InfiniBand conformance must remain explicitly selectable"
grep -Fq "name: includeDGXSpark" "$PIPELINE" ||
  fail "DGX Spark execution must remain an explicit opt-in while the nodes are reserved"
awk '
  /name: rdmaConformance/ { in_rdma=1 }
  in_rdma && /default: true/ { found=1; exit }
  in_rdma && /name: includeDGXSpark/ { exit }
  END { exit(found ? 0 : 1) }
' "$PIPELINE" ||
  fail "H200 InfiniBand conformance must run in the default nightly"
grep -Fq "TestRayServeGPU" "$PIPELINE" ||
  fail "hardware matrix must run Ray Serve online inference"
grep -Fq "Ray Serve protobuf compatibility fix" "$PIPELINE" ||
  fail "preflight must reject Ray versions older than 2.56.1"
grep -Fq 'if [[ "${INCLUDE_DGX}" == "true"' "$PIPELINE" ||
  fail "arm64 image validation must be conditional on DGX Spark opt-in"
grep -Fq "grep -Fxq linux/arm64" "$PIPELINE" ||
  fail "DGX Spark opt-in must require an arm64 Ray image"
grep -Fq "grep -Fxq linux/amd64" "$PIPELINE" ||
  fail "preflight must require an amd64 Ray image for A100/H200"
grep -Fq "TestRayTrainGPU" "$PIPELINE" ||
  fail "hardware matrix must run distributed Ray Train"
grep -Fq "run_case \"\${target}\" 2 2 1 tau.azure.com/site" "$PIPELINE" ||
  fail "DGX Spark matrix must include two GPUs across two hosts"
grep -Fq "run_case \"\${target}\" 2 1 2 kubernetes.io/hostname" "$PIPELINE" ||
  fail "H100 matrix must exercise the two-GPU H100 NVL pool"
grep -Fq "run_case \"\${target}\" 8 1 8 kubernetes.io/hostname" "$PIPELINE" ||
  fail "A100/H200 matrix must include eight GPUs on one host"
grep -Fq "run_case \"\${target}\" 16 2 8 tau.azure.com/site" "$PIPELINE" ||
  fail "A100/H200 matrix must include sixteen GPUs split across two hosts"
grep -Fq "MATRIX_SCHEDULER_RECOVERY_NODE" "$PIPELINE" ||
  fail "pipeline must retain the guarded H200 scheduler recovery"
grep -Fq "TestConcurrentGPUWorkloadsAcrossHardwareSites" \
  "${REPO_ROOT}/tests/e2e/multisite/multisite_test.go" ||
  fail "full profile must include concurrent hardware-site validation"
grep -Fq "TestFineWebRayTrain16xH200IB" "$PIPELINE" ||
  fail "default H200 RDMA/NCCL conformance must remain available"
grep -Fq "fineweb-metrics.json" \
  "${REPO_ROOT}/tests/e2e/stack/integration_test.go" ||
  fail "H200 InfiniBand conformance must publish structured metrics"
grep -Fq "FINEWEB_PERF_METRICS_JSON" \
  "${REPO_ROOT}/tests/e2e/stack/fixtures/fineweb_ray_train.py" ||
  fail "H200 conformance must emit distributed performance metrics"
grep -Fq "FINEWEB_IB_METRICS_JSON" \
  "${REPO_ROOT}/tests/e2e/stack/fixtures/fineweb_ray_train.py" ||
  fail "H200 conformance must emit InfiniBand counter deltas"
grep -Fq "port_xmit_data" \
  "${REPO_ROOT}/tests/e2e/stack/fixtures/fineweb_ray_train.py" ||
  fail "H200 conformance must collect InfiniBand traffic counters"
grep -Fq 'name: FINEWEB_CHECKPOINT_INTERVAL' "$PIPELINE" ||
  fail "short RDMA qualification must explicitly configure its checkpoint interval"
grep -Fq 'value: "1"' "$PIPELINE" ||
  fail "two-step RDMA qualification must checkpoint within the run"
grep -Fq 'FINEWEB_CHECKPOINT_INTERVAL: $(FINEWEB_CHECKPOINT_INTERVAL)' "$PIPELINE" ||
  fail "RDMA qualification must forward its checkpoint interval to the fixture"
grep -Fq "export FINEWEB_TAS_MODE=site" "$PIPELINE" ||
  fail "H200 RDMA extension must use site-level topology for its two-node shape"
grep -Fq 'export E2E_BUNDLE_DIR="${bundle_dir}/e2e"' "$PIPELINE" ||
  fail "live E2E diagnostics must be retained in published artifacts"
grep -Fq 'export E2E_RESULT_FILE="${artifact_dir}/e2e-results.jsonl"' "$PIPELINE" ||
  fail "live E2E outcomes must be retained in published artifacts"
grep -Fq 'rdma-postflight-status.json' "$PIPELINE" ||
  fail "RDMA cleanup must record postflight status"
grep -Fq 'RDMA namespace ${namespace} still exists after cleanup' "$PIPELINE" ||
  fail "RDMA cleanup must fail when its namespace remains"
grep -Fq 'compare-capability "${preflight_contract}" "${postflight_contract}"' "$PIPELINE" ||
  fail "RDMA cleanup must verify that Flex node capability recovered"
grep -Fq 'targetPath: $(Build.ArtifactStagingDirectory)/h200-rdma' "$PIPELINE" ||
  fail "RDMA diagnostics must publish from their dedicated artifact directory"
for artifact_dir in \
  taucluster-reconcile \
  preflight \
  images \
  deployment \
  tau-cli-smoke \
  gpu-routing-smoke \
  gpu-profile-smoke \
  storage-smoke \
  hardware-matrix \
  h200-rdma \
  kusto-checks \
  nightly-report; do
  grep -Fq "mkdir -p \"\$(Build.ArtifactStagingDirectory)/${artifact_dir}\"" "$PIPELINE" ||
    fail "pipeline must initialize ${artifact_dir} before unconditional artifact publishing"
done
grep -Fq "trap cleanup EXIT" "$PIPELINE" ||
  fail "pipeline must retain fail-safe cleanup traps"
grep -Fq "condition: always()" "$PIPELINE" ||
  fail "pipeline must publish diagnostics after failures"
grep -Fq '##vso[task.uploadsummary]' "$PIPELINE" ||
  fail "pipeline must upload the Markdown report to the Azure DevOps run summary"
grep -Fq 'taugrid-flex-nightly-report.md' "$PIPELINE" ||
  fail "pipeline must publish a human-readable nightly report"
grep -Fq 'artifact: taugrid-flex-nightly-report-' "$PIPELINE" ||
  fail "pipeline must publish the Markdown report in a pipeline artifact"
grep -Fq 'PREFLIGHT_JOB_RESULT: $[ dependencies.preflight.result ]' "$PIPELINE" ||
  fail "nightly report must retain preflight outcome after failures"
grep -Fq 'TAUCLUSTER_JOB_RESULT: $[ dependencies.taucluster_reconcile.result ]' "$PIPELINE" ||
  fail "nightly report must retain TauCluster reconciliation outcome"
grep -Fq 'BUILD_IMAGES_JOB_RESULT: $[ dependencies.build_images.result ]' "$PIPELINE" ||
  fail "nightly report must retain image publication outcome"
grep -Fq 'DEPLOY_JOB_RESULT: $[ dependencies.deploy_flex.result ]' "$PIPELINE" ||
  fail "nightly report must retain deployment outcome"
grep -Fq 'CLI_SMOKE_JOB_RESULT: $[ dependencies.tau_cli_smoke.result ]' "$PIPELINE" ||
  fail "nightly report must retain Tau CLI smoke outcome"
grep -Fq 'GPU_ROUTING_JOB_RESULT: $[ dependencies.gpu_queue_routing.result ]' "$PIPELINE" ||
  fail "nightly report must retain GPU routing outcome"
grep -Fq 'MATRIX_JOB_RESULT: $[ dependencies.hardware_matrix.result ]' "$PIPELINE" ||
  fail "nightly report must retain matrix outcome after failures"
grep -Fq 'RDMA_JOB_RESULT: $[ dependencies.h200_rdma.result ]' "$PIPELINE" ||
  fail "nightly report must retain optional RDMA outcome"
grep -Fq "job: kusto_checks" "$PIPELINE" ||
  fail "pipeline must validate Portal discovery and Kusto ingestion"
grep -Fq "scripts/ci/taugrid-flex-nightly-kusto-check.sh" "$PIPELINE" ||
  fail "pipeline must invoke the checked-in Kusto helper"
grep -Fq "E2E_KUSTO_URI: \$(AKS_AI_RUNTIME_FLEX_KUSTO_ENDPOINT)" "$PIPELINE" ||
  fail "live Go E2E jobs must receive the Kusto sink endpoint"
grep -Fq "E2E_KUSTO_DB: \$(AKS_AI_RUNTIME_FLEX_KUSTO_RESULTS_DATABASE)" "$PIPELINE" ||
  fail "live Go E2E jobs must receive the Kusto results database"
grep -Fq "KUSTO_JOB_RESULT: \$[ dependencies.kusto_checks.result ]" "$PIPELINE" ||
  fail "nightly report must retain Portal and Kusto outcome"
grep -Fq "STORAGE_JOB_RESULT: \$[ dependencies.storage_smoke.result ]" "$PIPELINE" ||
  fail "nightly report must retain storage outcome"
grep -Fq "GPU_PROFILE_JOB_RESULT: \$[ dependencies.gpu_profile_smoke.result ]" "$PIPELINE" ||
  fail "nightly report must retain GPU profile outcome"
grep -Fq 'postflight-status.json' "$PIPELINE" ||
  fail "hardware matrix cleanup must record postflight status"
[ "$(grep -Fc "done < <(jq -c '.targets[]' \"\${contract}\")" "$PIPELINE")" -eq 1 ] ||
  fail "disabled hardware targets must produce explicit skipped matrix cases"
if grep -Eq 'helm (upgrade|install).*(kueue|kuberay|tau-core-controller)' "$PIPELINE"; then
  fail "pipeline must not install colliding cluster-scoped controllers"
fi
for flavor in tau-gpu-a100-80gb-v2 tau-gpu-h100-95gb-v2 tau-gpu-h200-141gb-v2; do
  grep -Fq "name: ${flavor}" "$QUEUE_OVERLAY" ||
    fail "queue overlay must define ${flavor}"
done
[ "$(grep -Fc 'topologyName: taugrid-gpu-topology' "$QUEUE_OVERLAY")" -ge 3 ] ||
  fail "all shared GPU flavors must use taugrid-gpu-topology"
grep -Fq "tau.azure.com/gpu-queue: enabled" "$QUEUE_OVERLAY" ||
  fail "shared queue namespace selection must use the GPU queue opt-in label"

fake_bin="$(mktemp -d)"
trap 'rm -rf "$fake_bin"' EXIT
cat >"${fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  *"get nodes -l example.com/node-pool=spark -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"spark-a","labels":{"tau.azure.com/site":"site-dgx","tau.azure.com/network-domain":"spark-a","tau.azure.com/accelerator-domain":"spark-a"}},"spec":{},"status":{"nodeInfo":{"architecture":"arm64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"1"}}},{"metadata":{"name":"spark-b","labels":{"tau.azure.com/site":"site-dgx","tau.azure.com/network-domain":"spark-b","tau.azure.com/accelerator-domain":"spark-b"}},"spec":{},"status":{"nodeInfo":{"architecture":"arm64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"1"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=ndm-a100-v4 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"a100-a","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/gpu-class":"a100-80gb","tau.azure.com/network-domain":"a100-a","tau.azure.com/accelerator-domain":"a100-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}},{"metadata":{"name":"a100-b","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/gpu-class":"a100-80gb","tau.azure.com/network-domain":"a100-b","tau.azure.com/accelerator-domain":"a100-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=nc-h100-v5 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"h100-a","labels":{"tau.azure.com/site":"site-h100","tau.azure.com/gpu-class":"h100-95gb","tau.azure.com/network-domain":"h100-a","tau.azure.com/accelerator-domain":"h100-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"2"}}},{"metadata":{"name":"h100-b","labels":{"tau.azure.com/site":"site-h100","tau.azure.com/gpu-class":"h100-95gb","tau.azure.com/network-domain":"h100-b","tau.azure.com/accelerator-domain":"h100-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"1"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=nd-h200-v5 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"h200-a","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/gpu-class":"h200-141gb","tau.azure.com/network-domain":"h200-a","tau.azure.com/accelerator-domain":"h200-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}},{"metadata":{"name":"h200-b","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/gpu-class":"h200-141gb","tau.azure.com/network-domain":"h200-b","tau.azure.com/accelerator-domain":"h200-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}}]}
JSON
    ;;
  *"-n kube-node-lease get leases.coordination.k8s.io -o json"*)
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [[ "${FAKE_STALE_LEASE:-0}" == "1" ]]; then
      now="2000-01-01T00:00:00Z"
    fi
    printf '{"items":[{"metadata":{"name":"spark-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"spark-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h100-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h100-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-b"},"spec":{"renewTime":"%s"}}]}\n' \
      "$now" "$now" "$now" "$now" "$now" "$now" "$now" "$now"
    ;;
  *"get pods -A -o json"*)
    printf '%s\n' '{"items":[{"spec":{"nodeName":"a100-a","containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},"status":{"phase":"Running"}}]}'
    ;;
  *"get deployment "*" -o json"*)
    printf '%s\n' '{"status":{"availableReplicas":1}}'
    ;;
  *"get topology taugrid-gpu-topology -o json"*)
    printf '%s\n' '{"spec":{"levels":[{"nodeLabel":"tau.azure.com/site"},{"nodeLabel":"tau.azure.com/network-domain"},{"nodeLabel":"tau.azure.com/accelerator-domain"},{"nodeLabel":"kubernetes.io/hostname"}]}}'
    ;;
  *"get resourceflavor tau-gpu-dgx-spark-v2 -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    ;;
  *"get resourceflavor tau-gpu-a100-80gb-v2 -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology","nodeLabels":{"tau.azure.com/gpu-class":"a100-80gb"}}}'
    ;;
  *"get resourceflavor tau-gpu-h100-95gb-v2 -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology","nodeLabels":{"tau.azure.com/gpu-class":"h100-95gb"}}}'
    ;;
  *"get resourceflavor tau-gpu-h200-141gb-v2 -o json"*)
    if [[ "${FAKE_STALE_H200:-0}" == "1" ]]; then
      printf '%s\n' '{"spec":{"topologyName":"default-node-topology","nodeLabels":{"tau.azure.com/gpu-class":"h200-141gb"}}}'
    else
      printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology","nodeLabels":{"tau.azure.com/gpu-class":"h200-141gb"}}}'
    fi
    ;;
  *"get clusterqueue tau-gpu-cq -o json"*)
    printf '%s\n' '{"spec":{"resourceGroups":[{"flavors":[{"name":"tau-gpu-dgx-spark-v2"},{"name":"tau-gpu-a100-80gb-v2"},{"name":"tau-gpu-h100-95gb-v2"},{"name":"tau-gpu-h200-141gb-v2"}]}]},"status":{"conditions":[{"type":"Active","status":"True"}]}}'
    ;;
  *"get crd "*)
    printf '%s\n' '{}'
    ;;
  *)
    echo "unexpected kubectl arguments: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/kubectl"

export KUBECTL_BIN="${fake_bin}/kubectl"
export FLEX_NIGHTLY_DEPLOY_MODE='shared-cluster-controllers'
export FLEX_NIGHTLY_INCLUDE_DGX='true'
export FLEX_NIGHTLY_DGX_SELECTOR='example.com/node-pool=spark'
export FLEX_NIGHTLY_DGX_SITE='site-dgx'
export FLEX_NIGHTLY_A100_SITE='site-a100'
export FLEX_NIGHTLY_H100_SITE='site-h100'
export FLEX_NIGHTLY_H200_SITE='site-h200'
"$PREFLIGHT" validate-config

if FLEX_NIGHTLY_DEPLOY_MODE=isolated-controllers "$PREFLIGHT" validate-config >/dev/null 2>&1; then
  fail "preflight must reject unsafe isolated controller installation"
fi

export FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/contract.json"
"$PREFLIGHT" cluster
jq -e '
  .deploy_mode == "shared-cluster-controllers"
  and .topology == "taugrid-gpu-topology"
  and (.targets | length) == 4
  and (.targets | all(.capability_ready))
  and (.targets[] | select(.name == "dgx-spark") | .capability_contract.architecture) == "arm64"
  and (.targets[] | select(.name == "a100") | .capability_contract.gpus_per_node) == 8
  and (.targets[] | select(.name == "h100") | .capability_contract.allowed_gpus_per_node) == [1,2]
  and (.targets[] | select(.name == "h100") | .gpu_available) == 3
  and (.targets[] | select(.name == "dgx-spark") | .gpu_available) == 2
  and (.targets[] | select(.name == "a100") | .gpu_available) == 15
  and (.targets[] | select(.name == "h200") | .gpu_available) == 16
  and .rdma_target.rdma_nodes == 2
' "$FLEX_NIGHTLY_CONTRACT_FILE" >/dev/null ||
  fail "preflight contract does not preserve the hardware matrix inventory"

if FAKE_STALE_H200=1 "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject GPU flavors that are not wired to taugrid-gpu-topology"
fi
if FAKE_STALE_LEASE=1 "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject stale Flex node leases"
fi

cp "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"
"$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"

jq '(.targets[] | select(.name == "a100") | .nodes[0].gpu_allocatable) = 7' \
  "$FLEX_NIGHTLY_CONTRACT_FILE" >"${fake_bin}/drifted.json"
if "$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/drifted.json" >/dev/null 2>&1; then
  fail "postflight comparison must reject changed Flex node capability"
fi

echo "TauGrid Flex nightly contract tests passed"
