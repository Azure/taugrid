#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

for variable in $(compgen -A variable FLEX_NIGHTLY_); do
  unset "$variable"
done

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
import contextlib
import io
import os
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import time
from unittest.mock import patch

pipeline = Path(sys.argv[1]).read_text()
jobs = re.split(r"^  - job: ", pipeline, flags=re.MULTILINE)[1:]
targets = {"preflight": "preflight", "hardware_matrix": "hardware-matrix", "h200_rdma": ""}
rdma = next(job for job in jobs if job.splitlines()[0] == "h200_rdma")
assert "dependsOn:\n      - preflight\n      - hardware_matrix\n" in rdma
assert "condition: and(not(canceled()), eq(dependencies.preflight.result, 'Succeeded'))" in rdma
assert "parameters.rdmaConformance" not in pipeline, "legacy false must not bypass IB or misreport it"
assert "parameters.profile" not in rdma, "smoke and full must both require IB"
assert 'FLEX_NIGHTLY_RDMA_ENABLED: "true"' in pipeline
legacy = re.search(r"  - name: rdmaConformance\n(.*?)(?=  - name:)", pipeline, re.DOTALL)
assert legacy and "default: true" in legacy.group(1)
for name in ("hardware_matrix", "h200_rdma"):
    job = next(job for job in jobs if job.splitlines()[0] == name)
    for setting in (
        "export E2E_WORKLOAD_PRIORITY_CLASS=aks-low-priority",
        "export E2E_POD_PRIORITY_CLASS=tau-nightly-low",
    ):
        assert setting in job, (name, setting)
        assert job.index(setting) < job.index("go test -v"), (name, setting)
    assert "export E2E_TAU_RUN=1" in job, name
    assert 'export TAU_BINARY="$(Agent.TempDirectory)/tau"' in job, name
    assert 'cd "$(Build.SourcesDirectory)/cli"\n          make build' in job, name
    assert 'install -m 755 bin/tau "$(Agent.TempDirectory)/tau"' in job, name
    assert "export E2E_TAU_CONFIG_DIR=" in job, name
    assert 'export E2E_TAU_WORKSPACE="${FLEX_NIGHTLY_WORKSPACE}"' in job, name
    assert "export E2E_TAU_SYSTEM_NAMESPACE=tau-platform" in job, name
assert "kubectl create namespace" not in rdma, "IB must use an existing checkpoint namespace"
assert "kubectl delete namespace" not in rdma, "IB must never delete a shared checkpoint namespace"
assert "cleanup-ib" in rdma
assert "FINEWEB_CHECKPOINT_PVC: ${{ parameters.checkpointPVC }}" in rdma
preflight = next(job for job in jobs if job.splitlines()[0] == "preflight")
for resource in ("workloadpriorityclass aks-low-priority", "priorityclass tau-nightly-low"):
    assert f"kubectl get {resource} -o json" in preflight, resource
assert preflight.count("jq -e '.value == 100'") == 1
assert '.value == -100 and .preemptionPolicy == "Never" and .globalDefault != true' in preflight

with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    binary = root / "bin"
    binary.mkdir()
    priority_check = "kubectl get workloadpriorityclass" + preflight.split(
        "kubectl get workloadpriorityclass", 1
    )[1].split('artifact_dir=', 1)[0]
    kubectl = binary / "kubectl"
    kubectl.write_text(
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'if [[ "$2" == "$MISSING_CLASS" ]]; then exit 1; fi\n'
        'if [[ "$2" == "workloadpriorityclass" ]]; then value="$WORKLOAD_VALUE"; '
        'else value="$POD_VALUE"; fi\n'
        'printf \'{"value":%s,"preemptionPolicy":"%s","globalDefault":%s}\\n\' "$value" "$POD_PREEMPTION" "$GLOBAL_DEFAULT"\n'
    )
    kubectl.chmod(0o755)
    for missing, workload_value, pod_value, pod_preemption, global_default, expected in (
        ("", "100", "-100", "Never", "false", 0),
        ("workloadpriorityclass", "100", "-100", "Never", "false", 1),
        ("priorityclass", "100", "-100", "Never", "false", 1),
        ("", "500", "-100", "Never", "false", 1),
        ("", "100", "100", "Never", "false", 1),
        ("", "100", "-100", "PreemptLowerPriority", "false", 1),
        ("", "100", "-100", "Never", "true", 1),
    ):
        result = subprocess.run(
            ["bash", "-c", "set -euo pipefail\n" + priority_check],
            env=dict(
                os.environ, PATH=f"{binary}:{os.environ['PATH']}",
                MISSING_CLASS=missing, WORKLOAD_VALUE=workload_value, POD_VALUE=pod_value,
                POD_PREEMPTION=pod_preemption, GLOBAL_DEFAULT=global_default,
            ),
            capture_output=True, text=True,
        )
        assert result.returncode == expected, (missing, workload_value, pod_value, result.stderr)
        if expected:
            assert "Nightly requires" in result.stderr, result.stderr
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
        'if [[ "$1" == "get" ]]; then printf \'{"metadata":{"name":"%s",'
        '"uid":"11111111-1111-4111-8111-111111111111",'
        '"labels":{"tau.azure.com/workspace":"%s"}},'
        '"spec":{"clusterQueue":"%s"}}\\n\' "$3" "$FLEX_NIGHTLY_WORKSPACE" "$FLEX_NIGHTLY_CLUSTER_QUEUE"; fi\n'
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
            if name == "preflight" else (
                'namespace="${FLEX_NIGHTLY_RDMA_NAMESPACE}"'
            )
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
        if name in ("preflight", "h200_rdma"):
            continue
        inline = re.search(r"          inlineScript: \|\n(.*?)        env:\n", job, re.DOTALL)
        script = textwrap.dedent(inline.group(1))
        routing = script[script.index('kubectl get namespace "${namespace}"'):script.index('cd "$(Build.SourcesDirectory)/tests/e2e"')]
        routing = routing.replace("$(Build.BuildId)", "42").replace("$(System.JobAttempt)", "1")
        routing = routing.replace("$(Build.SourcesDirectory)", str(Path(sys.argv[1]).resolve().parent.parent))
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
                ["bash", "-c",
                 'set -euo pipefail\n'
                 'namespace=taugrid-nightly-rdma\n'
                 f'bundle_dir="{directory}"\n'
                 'contract="$TEST_CONTRACT"\n' + routing],
                env=environment,
                capture_output=True,
                text=True,
            )
            assert result.returncode == 0, (name, workspace, result.stderr)
            actual = calls.read_text().splitlines()
            assert actual == [
                "get namespace taugrid-nightly-rdma -o json",
                "get localqueue matrix-a100 -n taugrid-nightly-rdma -o json",
                "get localqueue matrix-h200 -n taugrid-nightly-rdma -o json",
            ], (name, actual)
            assert not manifests.exists(), "nightly must not mutate platform-owned queues"

    matrix = next(job for job in jobs if job.startswith("hardware_matrix\n"))
    assert "cancelTimeoutInMinutes: 45" in matrix
    cleanup_step = re.search(
        r"      - task: AzureCLI@2\n"
        r"        displayName: Clean up owned hardware matrix workloads\n"
        r"(.*?)"
        r"      - task: PublishPipelineArtifact@1",
        matrix, re.DOTALL,
    )
    assert cleanup_step, "owned cleanup must run before artifact publication"
    assert "condition: always()" in cleanup_step.group(1)
    assert "timeoutInMinutes: 20" in cleanup_step.group(1)
    assert "azureSubscription: $(azureServiceConnection)" in cleanup_step.group(1)
    assert "taugrid-flex-nightly-cleanup.py" in cleanup_step.group(1)
    assert "owned-cleanup-status.json" in cleanup_step.group(1)
    assert "kubectl delete namespace" not in matrix
    assert "kubectl create namespace" not in matrix
    assert "kubectl label namespace" not in matrix
    assert "cleanup-runs" in cleanup_step.group(1)
    cleanup_path = Path(sys.argv[1]).resolve().parent.parent / "scripts/ci/taugrid-flex-nightly-cleanup.py"
    spec = importlib.util.spec_from_file_location("nightly_cleanup", cleanup_path)
    cleanup = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cleanup)
    original_run = cleanup.subprocess.run
    cancellations = []
    def fake_cancel(command, **kwargs):
        cancellations.append(command)
        assert 0 < kwargs["timeout"] <= 960
        assert kwargs["check"] is True
        return subprocess.CompletedProcess(command, 0)
    cleanup.subprocess.run = fake_cancel
    routing_environment = patch.dict(os.environ, {
        "AI_RUNTIME_E2E_KUBE_CONTEXT": "nightly-context",
        "E2E_TAU_WORKSPACE": "nightly",
        "E2E_TAU_SYSTEM_NAMESPACE": "tau-platform",
    })
    routing_environment.start()
    try:
        assert cleanup.cleanup_runs("taugrid-nightly-rdma", "42-1", Path("/test/tau")) == "released"
        assert sorted(command[3] for command in cancellations) == sorted([
            "e2e-matrix-serve-42-1", "e2e-matrix-train-42-1",
            "e2e-multisite-a100-42-1", "e2e-multisite-h200-42-1",
            "e2e-multisite-dgx-spark-42-1",
        ])
        assert all(command[:3] == ["/test/tau", "run", "cancel"] for command in cancellations)
        assert all(command[4:] == [
            "--namespace", "taugrid-nightly-rdma", "--teardown-timeout", "15m",
            "--context", "nightly-context", "--workspace", "nightly",
            "--system-namespace", "tau-platform",
        ] for command in cancellations)
        for invalid in ("other", "42-1;delete"):
            try:
                cleanup.cleanup_runs("taugrid-nightly-rdma", invalid, Path("/test/tau"))
            except RuntimeError:
                pass
            else:
                raise AssertionError("invalid run identity accepted")
        assert len(cancellations) == 5
        cancellations.clear()
        def failing_cancel(command, **kwargs):
            cancellations.append(command)
            if command[3] == "e2e-matrix-serve-42-1":
                raise subprocess.CalledProcessError(1, command, stderr="compute still present")
            return subprocess.CompletedProcess(command, 0)
        cleanup.subprocess.run = failing_cancel
        try:
            cleanup.cleanup_runs("taugrid-nightly-rdma", "42-1", Path("/test/tau"))
        except RuntimeError as error:
            assert "compute still present" in str(error)
        else:
            raise AssertionError("failed compute cleanup passed")
        assert len(cancellations) == 5, "one failure must not skip other owned runs"
    finally:
        cleanup.subprocess.run = original_run
        routing_environment.stop()
    fake_tau = binary / "tau"
    fake_tau.write_text(
        "#!/usr/bin/env python3\n"
        "import json, os, sys, time\n"
        "with open(os.environ['FAKE_TAU_CALLS'], 'a') as stream:\n"
        "    stream.write(json.dumps(sys.argv[1:]) + '\\n')\n"
        "time.sleep(float(os.environ.get('FAKE_TAU_SLEEP', '0')))\n"
        "if sys.argv[3] == os.environ.get('FAKE_TAU_FAILURE'):\n"
        "    sys.stderr.write('compute still present\\n')\n"
        "    sys.exit(1)\n"
    )
    fake_tau.chmod(0o755)
    for scenario, delay, failed_run in (
        ("released", "0", ""),
        ("command-failure", "0", "e2e-matrix-serve-42-1"),
        ("budget-expired", "3", ""),
    ):
        calls = root / f"real-cleanup-{scenario}.jsonl"
        environment = {
            "FAKE_TAU_CALLS": str(calls),
            "FAKE_TAU_SLEEP": delay,
            "FAKE_TAU_FAILURE": failed_run,
            "E2E_TAU_WORKSPACE": "shared-workspace",
            "E2E_TAU_SYSTEM_NAMESPACE": "tau-platform",
            "AI_RUNTIME_E2E_KUBE_CONTEXT": "",
        }
        started = time.monotonic()
        with patch.dict(os.environ, environment), patch.object(cleanup, "CLEANUP_TIMEOUT_SECONDS", 1):
            try:
                result = cleanup.cleanup_runs("shared-namespace", "42-1", fake_tau)
            except RuntimeError as error:
                assert scenario != "released", error
                expected_error = "budget expired" if scenario == "budget-expired" else "compute still present"
                assert expected_error in str(error), error
            else:
                assert scenario == "released" and result == "released"
        assert time.monotonic() - started < 3, "cleanup must respect its shared budget"
        actual = [json.loads(line) for line in calls.read_text().splitlines()]
        assert {command[2] for command in actual} == {
            "e2e-matrix-serve-42-1", "e2e-matrix-train-42-1",
            "e2e-multisite-a100-42-1", "e2e-multisite-h200-42-1",
            "e2e-multisite-dgx-spark-42-1",
        }, "a stalled or failed cancellation must not starve later owned runs"
        assert all(command[:2] == ["run", "cancel"] for command in actual)
        assert all("--namespace" in command and "shared-namespace" in command for command in actual)

    ib_cleanup_step = re.search(
        r"      - task: AzureCLI@2\n"
        r"        displayName: Clean up owned IB workload\n"
        r"(.*?)"
        r"      - task: PublishPipelineArtifact@1",
        rdma, re.DOTALL,
    )
    assert ib_cleanup_step, "IB cancellation must not depend on the execution trap"
    assert "cancelTimeoutInMinutes: 30" in rdma
    assert "condition: always()" in ib_cleanup_step.group(1)
    assert "timeoutInMinutes: 20" in ib_cleanup_step.group(1)
    assert "cleanup-ib" in ib_cleanup_step.group(1)
    assert "owned-ib-cleanup-status.json" in ib_cleanup_step.group(1)
    assert "--teardown-timeout" not in rdma, "the diagnostics trap must not consume cancellation grace"
    inline = re.search(
        r"          inlineScript: \|\n(.*?)        env:\n", ib_cleanup_step.group(1), re.DOTALL,
    )
    script = textwrap.dedent(inline.group(1))
    for scenario, failed_run in (
        ("canceled-execution", ""),
        ("failed-execution", ""),
        ("cancellation-failure", "e2e-fineweb-16xh200-ib-42-1"),
    ):
        directory = root / scenario
        agent = directory / "agent"
        artifact = directory / "artifacts"
        agent.mkdir(parents=True)
        (agent / "tau").symlink_to(fake_tau)
        calls = directory / "ib-calls.jsonl"
        environment = dict(
            os.environ,
            PATH=f"{binary}:{os.environ['PATH']}",
            AUTH_CALLS=str(directory / "auth-calls"),
            EXPECTED_ARTIFACT_DIR=str(artifact),
            FAIL_COMMAND="",
            FLEX_NIGHTLY_SUBSCRIPTION_ID="test-subscription",
            FLEX_NIGHTLY_RESOURCE_GROUP="test-resource-group",
            FLEX_NIGHTLY_CLUSTER_NAME="test-cluster",
            FLEX_NIGHTLY_RDMA_NAMESPACE="shared-namespace",
            E2E_TAU_WORKSPACE="shared-workspace",
            E2E_TAU_SYSTEM_NAMESPACE="tau-platform",
            AI_RUNTIME_E2E_KUBE_CONTEXT="",
            FAKE_TAU_CALLS=str(calls),
            FAKE_TAU_SLEEP="0",
            FAKE_TAU_FAILURE=failed_run,
        )
        expanded = script.replace("$(Build.SourcesDirectory)", str(cleanup_path.parents[2]))
        expanded = expanded.replace("$(Build.ArtifactStagingDirectory)", str(artifact))
        expanded = expanded.replace("$(Agent.TempDirectory)", str(agent))
        expanded = expanded.replace("$(Build.BuildId)", "42").replace("$(System.JobAttempt)", "1")
        result = subprocess.run(
            ["bash", "-c", expanded], env=environment, capture_output=True, text=True, timeout=5,
        )
        assert (result.returncode == 0) == (scenario != "cancellation-failure"), (scenario, result.stderr)
        actual = [json.loads(line) for line in calls.read_text().splitlines()]
        assert len(actual) == 1
        assert actual[0] == [
            "run", "cancel", "e2e-fineweb-16xh200-ib-42-1",
            "--namespace", "shared-namespace", "--teardown-timeout", "15m",
            "--workspace", "shared-workspace", "--system-namespace", "tau-platform",
        ]
        if result.returncode == 0:
            status = json.loads((artifact / "owned-ib-cleanup-status.json").read_text())
            assert status == {"action": "cleanup-ib", "status": "released"}
        else:
            assert "compute still present" in result.stderr
    namespace = "taugrid-nightly-matrix-42-1"
    run_id = "42-1"
    uid = "11111111-1111-4111-8111-111111111111"
    owner_path = root / "cleanup-owner.json"
    live = {"metadata": {
        "name": namespace, "uid": uid,
        "labels": {"tau.azure.com/e2e-run": run_id},
    }}
    for scenario in (
        "record", "delete", "absent", "missing-owner", "wrong-uid", "wrong-label",
        "pvc", "api-failure", "delete-conflict", "replacement", "timeout",
    ):
        owner_path.write_text(json.dumps({"namespace": namespace, "run_id": run_id, "uid": uid}))
        state = json.loads(json.dumps(live))
        if scenario == "missing-owner":
            owner_path.unlink()
        if scenario == "wrong-uid":
            state["metadata"]["uid"] = "22222222-2222-4222-8222-222222222222"
        if scenario == "wrong-label":
            state["metadata"]["labels"]["tau.azure.com/e2e-run"] = "43-1"
        calls = []

        def fake_kubectl(*arguments, input_text=None):
            calls.append((arguments, input_text))
            if arguments[:2] == ("get", "namespace"):
                if scenario == "api-failure":
                    raise subprocess.CalledProcessError(1, ["kubectl"], stderr="unavailable")
                if scenario == "absent":
                    return ""
                deleted = any(call[0][0] == "delete" for call in calls)
                if deleted and scenario == "replacement":
                    replacement = json.loads(json.dumps(state))
                    replacement["metadata"]["uid"] = "22222222-2222-4222-8222-222222222222"
                    return json.dumps(replacement)
                return "" if deleted and scenario != "timeout" else json.dumps(state)
            if arguments[:2] == ("get", "pvc"):
                return json.dumps({"items": [{}] if scenario == "pvc" else []})
            assert arguments == ("delete", "--raw", f"/api/v1/namespaces/{namespace}", "-f", "-"), arguments
            assert json.loads(input_text)["preconditions"] == {"uid": uid}
            if scenario == "delete-conflict":
                raise subprocess.CalledProcessError(1, ["kubectl"], stderr="UID conflict")
            return "{}"

        expected = {"record": "recorded", "delete": "deleted", "absent": "absent"}
        with patch.object(cleanup, "kubectl", side_effect=fake_kubectl), \
             patch.object(cleanup.time, "sleep"), \
             patch.object(cleanup.time, "monotonic", side_effect=[0, 1, 901]):
            try:
                status = cleanup.run(
                    "record" if scenario == "record" else "cleanup",
                    namespace, run_id, owner_path,
                )
            except (OSError, RuntimeError, subprocess.CalledProcessError):
                assert scenario not in expected, scenario
            else:
                assert status == expected[scenario], (scenario, status)
        if scenario in ("missing-owner", "wrong-uid", "wrong-label", "pvc", "api-failure", "record", "absent"):
            assert not any(call[0][0] == "delete" for call in calls), scenario
    with patch.object(cleanup, "kubectl") as mocked:
        try:
            cleanup.run("cleanup", "unrelated", run_id, owner_path)
        except RuntimeError:
            pass
        else:
            raise AssertionError("unrelated namespace accepted")
        mocked.assert_not_called()

    timeout_stderr = io.StringIO()
    with patch.object(
        cleanup.subprocess, "run",
        side_effect=subprocess.TimeoutExpired(["kubectl"], 30),
    ) as process, patch.object(
        sys, "argv", [str(cleanup_path), "cleanup", namespace, run_id, str(owner_path)],
    ), contextlib.redirect_stderr(timeout_stderr):
        try:
            cleanup.main()
        except SystemExit as error:
            assert error.code == 1, error.code
        else:
            raise AssertionError("timed-out namespace lookup succeeded")
        process.assert_called_once()
        assert process.call_args.args[0][:3] == ["kubectl", "get", "namespace"]
        assert process.call_args.kwargs["timeout"] == 30
    assert "kubectl timed out after 30 seconds" in timeout_stderr.getvalue()

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
  fail "legacy RDMA parameter must remain accepted for compatibility"
grep -Fq "name: includeDGXSpark" "$PIPELINE" ||
  fail "DGX Spark execution must remain an explicit opt-in while the nodes are reserved"
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
  fail "required H200 RDMA/NCCL conformance must remain available"
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
  fail "nightly report must retain required RDMA outcome"
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
    cat <<'JSON' | jq --arg ready "${FAKE_H200_READY:-True}" \
      '(.items[1].status.conditions[] | select(.type == "Ready") | .status) = $ready'
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

mkdir -p "${fake_bin}/unready"
if FAKE_H200_READY=Unknown \
  FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/unready/hardware-matrix.json" \
  "$PREFLIGHT" cluster >"${fake_bin}/unready-preflight.log" 2>&1; then
  fail "preflight must reject an H200 node whose Ready condition is Unknown"
fi
[ -s "${fake_bin}/unready/hardware-matrix.json" ] ||
  fail "failed preflight must preserve the collected hardware inventory"
jq -e '
  .targets[] | select(.name == "h200")
  | .status == "misconfigured"
    and .ready_nodes == 1
    and (.capability_ready | not)
    and .capability_violations == ["h200-b: Ready is not True"]
    and .topology_ready and .queue_active and .queue_has_flavor
' "${fake_bin}/unready/hardware-matrix.json" >/dev/null ||
  fail "failed preflight must preserve the node readiness violation"
PREFLIGHT_JOB_RESULT=Failed MATRIX_JOB_RESULT=Skipped \
  "$REPORTER" "${fake_bin}/unready" "${fake_bin}/unready-report.md"
grep -Fq '| Preflight job | **FAIL** |' "${fake_bin}/unready-report.md" &&
  grep -Fq '| h200 | **misconfigured** | 1 |' "${fake_bin}/unready-report.md" &&
  grep -Fq 'h200-b: Ready is not True' "${fake_bin}/unready-report.md" ||
  fail "nightly report must show the failed readiness gate and collected inventory"

if FAKE_STALE_H200=1 FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/stale-topology.json" \
  "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject GPU flavors that are not wired to taugrid-gpu-topology"
fi
if FAKE_STALE_LEASE=1 FLEX_NIGHTLY_CONTRACT_FILE="${fake_bin}/stale-lease.json" \
  "$PREFLIGHT" cluster >/dev/null 2>&1; then
  fail "preflight must reject stale Flex node leases"
fi

cp "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"
"$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/postflight.json"

jq '(.targets[] | select(.name == "a100") | .nodes[0].gpu_allocatable) = 7' \
  "$FLEX_NIGHTLY_CONTRACT_FILE" >"${fake_bin}/drifted.json"
if "$PREFLIGHT" compare-capability "$FLEX_NIGHTLY_CONTRACT_FILE" "${fake_bin}/drifted.json" >/dev/null 2>&1; then
  fail "postflight comparison must reject changed Flex node capability"
fi

if [[ "${TAUGRID_NIGHTLY_CONTRACT_ENVIRONMENT_TEST:-}" != "1" ]]; then
  TAUGRID_NIGHTLY_CONTRACT_ENVIRONMENT_TEST=1 \
  FLEX_NIGHTLY_WORKSPACE=outer-workspace \
  FLEX_NIGHTLY_CLUSTER_QUEUE=outer-queue \
  FLEX_NIGHTLY_A100_FLAVOR=outer-a100 \
  FLEX_NIGHTLY_H200_FLAVOR=outer-h200 \
  FLEX_NIGHTLY_DGX_FLAVOR=outer-dgx \
  FLEX_NIGHTLY_A100_CLUSTER_QUEUE=outer-a100-queue \
  FLEX_NIGHTLY_H200_CLUSTER_QUEUE=outer-h200-queue \
  FLEX_NIGHTLY_DGX_CLUSTER_QUEUE=outer-dgx-queue \
  FLEX_NIGHTLY_INCLUDE_DGX=false \
  FLEX_NIGHTLY_GPU_TOPOLOGY=outer-topology \
  FLEX_NIGHTLY_A100_SELECTOR=example.com/outer=a100 \
  FLEX_NIGHTLY_H200_SELECTOR=example.com/outer=h200 \
  FLEX_NIGHTLY_MIN_RDMA_NODES=99 \
    bash "${BASH_SOURCE[0]}" >"${fake_bin}/outer-environment.log" 2>&1 || {
      cat "${fake_bin}/outer-environment.log" >&2
      fail "the complete offline fixture must ignore inherited deployment settings"
    }
fi

echo "TauGrid Flex nightly contract tests passed"
