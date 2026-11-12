#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PIPELINE="${REPO_ROOT}/.pipelines/taugrid-flex-nightly.yml"
readonly PREFLIGHT="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-preflight.sh"
readonly REPORTER="${REPO_ROOT}/scripts/ci/taugrid-flex-nightly-report.sh"
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
[ -f "$QUEUE_OVERLAY" ] || fail "shared GPU queue overlay is missing"

grep -Fq 'readonly A100_SITE="${FLEX_NIGHTLY_A100_SITE:-}"' "$PREFLIGHT" ||
  fail "A100 site must be supplied through ADO variables"
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
grep -Fq "azureSubscription: \$(azureServiceConnection)" "$PIPELINE" ||
  fail "pipeline must use the approved Azure DevOps service connection"
python3 - "$PIPELINE" <<'PY'
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap

pipeline = Path(sys.argv[1]).read_text()
jobs = re.split(r"^  - job: ", pipeline, flags=re.MULTILINE)[1:]
targets = {"preflight": "preflight", "hardware_matrix": "hardware-matrix", "h200_rdma": ""}

with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    binary = root / "bin"
    binary.mkdir()
    for command in ("az", "kubelogin"):
        stub = binary / command
        stub.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            f'printf "%s\\n" "{command} $*" >>"$AUTH_CALLS"\n'
            'test -d "$EXPECTED_ARTIFACT_DIR"\n'
            'if [[ "$*" == "$FAIL_COMMAND" ]]; then exit 41; fi\n'
        )
        stub.chmod(0o755)

    for job in jobs:
        name = job.splitlines()[0]
        if name not in targets:
            continue
        assert "azureSubscription: $(azureServiceConnection)" in job, name
        assert "FLEX_NIGHTLY_SUBSCRIPTION_ID: $(AKS_AI_RUNTIME_FLEX_SUBSCRIPTION_ID)" in job, name
        initialization = re.search(
            r"      - bash: \|\n(.*?)        displayName: Initialize nightly diagnostics\n",
            job,
            re.DOTALL,
        )
        assert initialization, f"{name}: diagnostics must initialize outside AzureCLI login"
        assert initialization.end() < job.index("      - task: AzureCLI@2"), name
        inline = re.search(r"          inlineScript: \|\n(.*?)        env:\n", job, re.DOTALL)
        assert inline, name
        authentication = textwrap.dedent(inline.group(1)).splitlines(keepends=True)
        end = next(i for i, line in enumerate(authentication) if "kubelogin convert-kubeconfig" in line)
        authentication = "".join(authentication[:end + 1])

        for scenario, subscription, failure, expected, status in (
            ("success", "test-subscription", "", 3, 0),
            ("missing", None, "", 0, 1),
            ("empty", "", "", 0, 1),
            ("unexpanded", "$(AKS_AI_RUNTIME_FLEX_SUBSCRIPTION_ID)", "", 0, 1),
            ("selection-failure", "test-subscription", "account set --subscription test-subscription", 1, 41),
            ("credential-failure", "test-subscription", None, 2, 41),
            ("conversion-failure", "test-subscription", None, 3, 41),
        ):
            directory = root / name / scenario
            directory.mkdir(parents=True)
            artifact = directory / "artifacts"
            calls = directory / "calls"
            calls.touch()
            kubeconfig = directory / "agent" / "flex-kubeconfig"
            credential_command = (
                "aks get-credentials --resource-group test-resource-group --name test-cluster "
                f"--file {kubeconfig} --overwrite-existing"
            )
            conversion_command = f"convert-kubeconfig --kubeconfig {kubeconfig} --login azurecli"
            if scenario == "credential-failure":
                failure = credential_command
            elif scenario == "conversion-failure":
                failure = conversion_command
            environment = dict(os.environ)
            environment.pop("FLEX_NIGHTLY_SUBSCRIPTION_ID", None)
            environment.update(
                PATH=f"{binary}:{os.environ['PATH']}",
                AUTH_CALLS=str(calls),
                EXPECTED_ARTIFACT_DIR=str(artifact / targets[name]),
                FAIL_COMMAND=failure,
                FLEX_NIGHTLY_RESOURCE_GROUP="test-resource-group",
                FLEX_NIGHTLY_CLUSTER_NAME="test-cluster",
            )
            if subscription is not None:
                environment["FLEX_NIGHTLY_SUBSCRIPTION_ID"] = subscription

            def expand(script):
                return script.replace("$(Build.ArtifactStagingDirectory)", str(artifact)).replace(
                    "$(Agent.TempDirectory)", str(directory / "agent")
                )

            subprocess.run(
                ["bash", "-c", expand(textwrap.dedent(initialization.group(1)))],
                env=environment,
                check=True,
                capture_output=True,
                text=True,
            )
            assert (artifact / targets[name]).is_dir(), f"{name}: missing diagnostics before login"
            result = subprocess.run(
                ["bash", "-c", expand(authentication)],
                env=environment,
                capture_output=True,
                text=True,
            )
            assert result.returncode == status, (name, scenario, result.returncode, result.stderr)
            actual = calls.read_text().splitlines()
            ordered = [
                "az account set --subscription test-subscription",
                f"az {credential_command}",
                f"kubelogin {conversion_command}",
            ]
            assert actual == ordered[:expected], (name, scenario, actual)
            if expected == 0:
                assert "FLEX_NIGHTLY_SUBSCRIPTION_ID" in result.stderr, (name, scenario)

    kubectl = binary / "kubectl"
    kubectl.write_text(
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "%s\\n" "$*" >>"$ROUTING_CALLS"\n'
        'if [[ "$1" == "apply" ]]; then cat >>"$ROUTING_MANIFESTS"; fi\n'
    )
    kubectl.chmod(0o755)
    for job in jobs:
        name = job.splitlines()[0]
        if name not in targets:
            continue
        overrides = {}
        if name != "h200_rdma":
            for target in ("A100", "H200", "DGX"):
                overrides[f"FLEX_NIGHTLY_{target}_FLAVOR"] = f"{target.lower()}Flavor"
                overrides[f"FLEX_NIGHTLY_{target}_CLUSTER_QUEUE"] = "clusterQueue"
        else:
            overrides["FLEX_NIGHTLY_CLUSTER_QUEUE"] = "clusterQueue"
        if name != "preflight":
            overrides["FLEX_NIGHTLY_WORKSPACE"] = "workspace"
        for variable, parameter in overrides.items():
            assert f"{variable}: ${{{{ parameters.{parameter} }}}}" in job, (name, variable)
        routing_source = 'source "$(Build.SourcesDirectory)/scripts/ci/taugrid-flex-nightly-routing.sh"'
        assert routing_source in job, name
        first_use = (
            "scripts/ci/taugrid-flex-nightly-preflight.sh cluster"
            if name == "preflight" else "kubectl create namespace"
        )
        assert job.index(routing_source) < job.index(first_use), name
        defaults = {
            "workspace": ("FLEX_NIGHTLY_WORKSPACE", "nightly"),
            "clusterQueue": ("FLEX_NIGHTLY_CLUSTER_QUEUE", "tau-gpu-cq"),
            "a100Flavor": ("FLEX_NIGHTLY_A100_FLAVOR", "tau-gpu-a100-80gb-v2"),
            "h200Flavor": ("FLEX_NIGHTLY_H200_FLAVOR", "tau-gpu-h200-141gb-v2"),
            "dgxFlavor": ("FLEX_NIGHTLY_DGX_FLAVOR", "tau-gpu-dgx-spark-v2"),
        }
        configured = {
            parameter: f"shared-{parameter.lower()}"
            for parameter in defaults
        }
        explicit = {
            parameter: default
            for parameter, (_, default) in defaults.items()
        }
        for parameter, (variable, _) in defaults.items():
            definition = re.search(
                rf"^  - name: {parameter}\n(.*?)(?=^  - name: |^pool:)",
                pipeline, re.MULTILINE | re.DOTALL,
            )
            assert definition and f"default: $({variable})" in definition.group(1), parameter
        for scenario, settings, parameters in (
            ("legacy-defaults", {}, {}),
            ("scheduled-definition-settings", configured, {}),
            ("explicit-defaults-win", configured, explicit),
            ("partial-manual-override", configured, {"h200Flavor": "manual-h200"}),
            ("empty-definition-settings", {parameter: "" for parameter in defaults}, {}),
        ):
            environment = {
                key: value for key, value in os.environ.items()
                if not key.startswith("FLEX_NIGHTLY_")
            }
            expected = []
            for variable, parameter in overrides.items():
                setting, fallback = defaults[parameter]
                template_default = f"$({setting})"
                value = parameters.get(parameter, template_default)
                if value == template_default and parameter in settings:
                    value = settings[parameter]
                environment[variable] = value
                expected.append(
                    fallback if not value or value == template_default else value
                )
            source = routing_source.replace(
                "$(Build.SourcesDirectory)", str(Path(sys.argv[1]).resolve().parent.parent)
            )
            output = 'printf "%s\\n" ' + " ".join(
                f'"${variable}"' for variable in overrides
            )
            result = subprocess.run(
                ["bash", "-c", f"set -euo pipefail\n{source}\n{output}"],
                env=environment, capture_output=True, text=True,
            )
            assert result.returncode == 0, (name, scenario, result.stderr)
            assert result.stdout.splitlines() == expected, (name, scenario, result.stdout, expected)
        if name == "preflight":
            continue
        inline = re.search(r"          inlineScript: \|\n(.*?)        env:\n", job, re.DOTALL)
        script = textwrap.dedent(inline.group(1))
        routing = script[script.index('kubectl create namespace'):script.index('cd "$(Build.SourcesDirectory)/tests/e2e"')]
        routing = routing.replace("$(Build.BuildId)", "42").replace("$(System.JobAttempt)", "1")
        for workspace, queue in (("nightly", "tau-gpu-cq"), ("shared-workspace", "shared-queue")):
            directory = root / name / workspace
            directory.mkdir()
            calls = directory / "routing-calls"
            manifests = directory / "routing-manifests"
            contract = directory / "contract.json"
            contract.write_text(
                '{"targets":['
                f'{{"name":"a100","status":"available","cluster_queue":"{queue}"}},'
                f'{{"name":"h200","status":"available","cluster_queue":"{queue}"}}'
                ']}'
            )
            environment = dict(os.environ)
            environment.update(
                PATH=f"{binary}:{os.environ['PATH']}",
                ROUTING_CALLS=str(calls),
                ROUTING_MANIFESTS=str(manifests),
                FLEX_NIGHTLY_WORKSPACE=workspace,
                FLEX_NIGHTLY_CLUSTER_QUEUE=queue,
                TEST_CONTRACT=str(contract),
            )
            result = subprocess.run(
                ["bash", "-c", 'set -euo pipefail\nnamespace=taugrid-nightly-test\ncontract="$TEST_CONTRACT"\n' + routing],
                env=environment,
                capture_output=True,
                text=True,
            )
            assert result.returncode == 0, (name, workspace, result.stderr)
            actual = calls.read_text().splitlines()
            assert actual[0] == "create namespace taugrid-nightly-test", (name, actual)
            assert actual[1].startswith("label namespace taugrid-nightly-test "), (name, actual)
            assert f"tau.azure.com/workspace={workspace}" in actual[1].split(), (name, actual)
            assert "tau.azure.com/gpu-queue=enabled" in actual[1].split(), (name, actual)
            expected_queues = 2 if name == "hardware_matrix" else 1
            assert actual[2:] == ["apply -f -"] * expected_queues, (name, actual)
            assert manifests.read_text().count(f"clusterQueue: {queue}\n") == expected_queues, name

    preflight = next(job for job in jobs if job.startswith("preflight\n"))
    inline = re.search(r"          inlineScript: \|\n(.*?)        env:\n", preflight, re.DOTALL)
    script = textwrap.dedent(inline.group(1))
    start = script.index('artifact_dir=')
    version_check = script.index('if [[ "${RAY_E2E_IMAGE}" =~ ray')
    end = script.index('export FLEX_NIGHTLY_CONTRACT_FILE=')
    inspection = script[start:version_check]
    validation = script[start:end]
    docker = binary / "docker"
    docker.write_text(
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        '[[ "$*" == "manifest inspect ${EXPECTED_IMAGE}" ]] || exit 42\n'
        'if [[ "${MANIFEST_FAILURE:-false}" == "true" ]]; then exit 43; fi\n'
        """printf '%s\\n' '{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}}]}'\n"""
    )
    docker.chmod(0o755)
    digest = "sha256:" + "a" * 64
    tag = "py3.12-ray2.58.0-cuda13.0"
    for repository in ("registry.example.com/team/ray", "registry.example.com:5000/team/ray", "ray"):
        for reference, expected in (
            (f"{repository}:{tag}@{digest}", f"{repository}@{digest}"),
            (f"{repository}@{digest}", f"{repository}@{digest}"),
            (f"{repository}:{tag}", f"{repository}:{tag}"),
        ):
            environment = dict(os.environ)
            environment.update(
                PATH=f"{binary}:{os.environ['PATH']}",
                RAY_E2E_IMAGE=reference,
                EXPECTED_IMAGE=expected,
                INCLUDE_DGX="true",
                MANIFEST_FAILURE="false",
            )
            expanded = inspection.replace("$(Build.ArtifactStagingDirectory)", str(root / "image-artifacts"))
            result = subprocess.run(
                ["bash", "-c", 'set -euo pipefail\n' + expanded + 'printf "%s\\n" "$RAY_E2E_IMAGE" "$image_platforms"'],
                env=environment,
                capture_output=True,
                text=True,
            )
            assert result.returncode == 0, (reference, result.returncode, result.stderr)
            assert result.stdout.splitlines() == [reference, "linux/amd64", "linux/arm64"], reference
            if reference == f"{repository}:{tag}@{digest}":
                expanded = validation.replace("$(Build.ArtifactStagingDirectory)", str(root / "image-artifacts"))
                for failure, status in (("false", 0), ("true", 43)):
                    environment["MANIFEST_FAILURE"] = failure
                    result = subprocess.run(
                        ["bash", "-c", 'set -euo pipefail\n' + expanded],
                        env=environment,
                        capture_output=True,
                        text=True,
                    )
                    assert result.returncode == status, (reference, failure, result.stderr)
                assert (root / "image-artifacts/preflight/ray-version.txt").read_text() == "2.58.0\n"

assert not re.search(r"^\s*- name: azureServiceConnection\s*$", pipeline, re.MULTILINE), (
    "service connection must be configured outside public source"
)
PY
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
  fail "extended H200 RDMA conformance must remain explicitly selectable"
grep -Fq "name: includeDGXSpark" "$PIPELINE" ||
  fail "DGX Spark execution must remain an explicit opt-in while the nodes are reserved"
grep -Fq "default: false" "$PIPELINE" ||
  fail "extended H200 RDMA conformance must be disabled by default"
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
  fail "optional H200 RDMA/NCCL conformance must remain available"
grep -Fq 'name: FINEWEB_CHECKPOINT_INTERVAL' "$PIPELINE" ||
  fail "short RDMA qualification must explicitly configure its checkpoint interval"
grep -Fq 'value: "1"' "$PIPELINE" ||
  fail "two-step RDMA qualification must checkpoint within the run"
grep -Fq 'FINEWEB_CHECKPOINT_INTERVAL: $(FINEWEB_CHECKPOINT_INTERVAL)' "$PIPELINE" ||
  fail "RDMA qualification must forward its checkpoint interval to the fixture"
grep -Fq "export FINEWEB_TAS_MODE=site" "$PIPELINE" ||
  fail "H200 RDMA extension must use site-level topology for its two-node shape"
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
grep -Fq 'MATRIX_JOB_RESULT: $[ dependencies.hardware_matrix.result ]' "$PIPELINE" ||
  fail "nightly report must retain matrix outcome after failures"
grep -Fq 'RDMA_JOB_RESULT: $[ dependencies.h200_rdma.result ]' "$PIPELINE" ||
  fail "nightly report must retain optional RDMA outcome"
grep -Fq 'postflight-status.json' "$PIPELINE" ||
  fail "hardware matrix cleanup must record postflight status"
[ "$(grep -Fc "done < <(jq -c '.targets[]' \"\${contract}\")" "$PIPELINE")" -eq 1 ] ||
  fail "disabled hardware targets must produce explicit skipped matrix cases"
if grep -Eq 'helm (upgrade|install).*(kueue|kuberay|tau-core-controller)' "$PIPELINE"; then
  fail "pipeline must not install colliding cluster-scoped controllers"
fi
for flavor in tau-gpu-a100-80gb-v2 tau-gpu-h200-141gb-v2; do
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
{"items":[{"metadata":{"name":"a100-a","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/network-domain":"a100-a","tau.azure.com/accelerator-domain":"a100-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}},{"metadata":{"name":"a100-b","labels":{"tau.azure.com/site":"site-a100","tau.azure.com/network-domain":"a100-b","tau.azure.com/accelerator-domain":"a100-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"true","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://1.7.31"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8"}}}]}
JSON
    ;;
  *"get nodes -l kueue.azure.com/gpu-series=nd-h200-v5 -o json"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"h200-a","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/network-domain":"h200-a","tau.azure.com/accelerator-domain":"h200-a"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}},{"metadata":{"name":"h200-b","labels":{"tau.azure.com/site":"site-h200","tau.azure.com/network-domain":"h200-b","tau.azure.com/accelerator-domain":"h200-b"}},"spec":{"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}]},"status":{"nodeInfo":{"architecture":"amd64","operatingSystem":"linux","containerRuntimeVersion":"containerd://2.0.4"},"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}],"allocatable":{"nvidia.com/gpu":"8","rdma/rdma_shared_device_a":"1"}}}]}
JSON
    ;;
  *"-n kube-node-lease get leases.coordination.k8s.io -o json"*)
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [[ "${FAKE_STALE_LEASE:-0}" == "1" ]]; then
      now="2000-01-01T00:00:00Z"
    fi
    printf '{"items":[{"metadata":{"name":"spark-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"spark-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"a100-b"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-a"},"spec":{"renewTime":"%s"}},{"metadata":{"name":"h200-b"},"spec":{"renewTime":"%s"}}]}\n' \
      "$now" "$now" "$now" "$now" "$now" "$now"
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
  *"get resourceflavor tau-gpu-a100-80gb-v2 -o json"*|*"get resourceflavor test-a100-flavor -o json"*)
    printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    ;;
  *"get resourceflavor tau-gpu-h200-141gb-v2 -o json"*|*"get resourceflavor test-h200-flavor -o json"*)
    if [[ "${FAKE_STALE_H200:-0}" == "1" ]]; then
      printf '%s\n' '{"spec":{"topologyName":"default-node-topology"}}'
    else
      printf '%s\n' '{"spec":{"topologyName":"taugrid-gpu-topology"}}'
    fi
    ;;
  *"get clusterqueue tau-gpu-cq -o json"*|*"get clusterqueue test-shared-queue -o json"*)
    printf '%s\n' '{"spec":{"resourceGroups":[{"flavors":[{"name":"tau-gpu-dgx-spark-v2"},{"name":"tau-gpu-h200-141gb-v2"},{"name":"tau-gpu-a100-80gb-v2"},{"name":"test-a100-flavor"},{"name":"test-h200-flavor"}]}]},"status":{"conditions":[{"type":"Active","status":"True"}]}}'
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
  and (.targets | length) == 3
  and (.targets | all(.capability_ready))
  and (.targets[] | select(.name == "dgx-spark") | .capability_contract.architecture) == "arm64"
  and (.targets[] | select(.name == "a100") | .capability_contract.gpus_per_node) == 8
  and (.targets[] | select(.name == "dgx-spark") | .gpu_available) == 2
  and (.targets[] | select(.name == "a100") | .gpu_available) == 15
  and (.targets[] | select(.name == "h200") | .gpu_available) == 16
  and .rdma_target.rdma_nodes == 2
' "$FLEX_NIGHTLY_CONTRACT_FILE" >/dev/null ||
  fail "preflight contract does not preserve the hardware matrix inventory"

FLEX_NIGHTLY_INCLUDE_DGX=false \
FLEX_NIGHTLY_A100_FLAVOR=test-a100-flavor \
FLEX_NIGHTLY_H200_FLAVOR=test-h200-flavor \
FLEX_NIGHTLY_A100_CLUSTER_QUEUE=test-shared-queue \
FLEX_NIGHTLY_H200_CLUSTER_QUEUE=test-shared-queue \
FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/shared-contract.json" \
  "$PREFLIGHT" cluster
jq -e '
  (.targets[] | select(.name == "a100") | .flavor == "test-a100-flavor" and .cluster_queue == "test-shared-queue")
  and (.targets[] | select(.name == "h200") | .flavor == "test-h200-flavor" and .cluster_queue == "test-shared-queue")
' "${fake_bin}/shared-contract.json" >/dev/null ||
  fail "preflight must retain configured shared flavors and queue"

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
