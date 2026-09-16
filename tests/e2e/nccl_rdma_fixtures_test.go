// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/tests/e2e/internal/ncclimage"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const ncclRDMATestFixture = "stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml"

func renderNCCLRDMAFixture(t *testing.T) ([]byte, batchv1.Job) {
	t.Helper()
	t.Setenv("E2E_STACK_NAMESPACE", "approved-nccl-rdma")
	t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", "h200-rdma")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")
	t.Setenv("NCCL_RDMA_INVOCATION", "nccl-rdma-0123456789abcdef0123456789abcdef")

	data, err := ReadFixtureWithSubstitutions(ncclRDMATestFixture)
	require.NoError(t, err)
	var job batchv1.Job
	require.NoError(t, yaml.Unmarshal(data, &job))
	return data, job
}

func TestNCCLRDMAIndexedJobFixtureContract(t *testing.T) {
	data, job := renderNCCLRDMAFixture(t)
	require.Equal(t, "batch/v1", job.APIVersion)
	require.Equal(t, "Job", job.Kind)
	require.Equal(t, NCCLRDMAJobName, job.Name)
	require.Equal(t, "approved-nccl-rdma", job.Namespace)
	require.Equal(t, "h200-rdma", job.Labels["kueue.x-k8s.io/queue-name"])
	require.Equal(t, "nccl-rdma-0123456789abcdef0123456789abcdef", job.Labels[NCCLRDMAInvocationKey])
	require.NotNil(t, job.Spec.Suspend)
	require.True(t, *job.Spec.Suspend)
	require.NotNil(t, job.Spec.Completions)
	require.Equal(t, int32(2), *job.Spec.Completions)
	require.NotNil(t, job.Spec.Parallelism)
	require.Equal(t, int32(2), *job.Spec.Parallelism)
	require.NotNil(t, job.Spec.CompletionMode)
	require.Equal(t, batchv1.IndexedCompletion, *job.Spec.CompletionMode)
	require.NotNil(t, job.Spec.ManualSelector)
	require.True(t, *job.Spec.ManualSelector)
	require.Equal(t, map[string]string{
		"batch.kubernetes.io/job-name":     NCCLRDMAJobName,
		"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:              "nccl-rdma-0123456789abcdef0123456789abcdef",
	}, job.Spec.Selector.MatchLabels)
	require.Empty(t, job.Spec.Selector.MatchExpressions)
	for key, value := range job.Spec.Selector.MatchLabels {
		require.Equal(t, value, job.Spec.Template.Labels[key])
	}
	require.NotNil(t, job.Spec.BackoffLimit)
	require.Zero(t, *job.Spec.BackoffLimit)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	require.Equal(t, int64(900), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
	require.Equal(t, int32(600), *job.Spec.TTLSecondsAfterFinished)

	pod := job.Spec.Template.Spec
	require.Equal(t, NCCLRDMAServiceAccount, pod.ServiceAccountName)
	require.NotNil(t, pod.AutomountServiceAccountToken)
	require.False(t, *pod.AutomountServiceAccountToken)
	require.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.Equal(t, corev1.DefaultSchedulerName, pod.SchedulerName)
	require.Nil(t, pod.PreemptionPolicy)
	require.NotNil(t, pod.TerminationGracePeriodSeconds)
	require.Equal(t, int64(30), *pod.TerminationGracePeriodSeconds)
	require.Equal(t, corev1.DNSClusterFirst, pod.DNSPolicy)
	require.NotNil(t, pod.EnableServiceLinks)
	require.False(t, *pod.EnableServiceLinks)
	require.Equal(t, map[string]string{"accelerator": "nvidia-h200"}, pod.NodeSelector)
	require.Empty(t, pod.NodeName)
	require.Len(t, pod.Containers, 1)
	require.Empty(t, pod.InitContainers)
	require.Empty(t, pod.EphemeralContainers)
	require.NotNil(t, pod.SecurityContext.RunAsNonRoot)
	require.True(t, *pod.SecurityContext.RunAsNonRoot)
	require.Equal(t, int64(1000), *pod.SecurityContext.RunAsUser)
	require.Equal(t, int64(1000), *pod.SecurityContext.RunAsGroup)
	require.Equal(t, int64(1000), *pod.SecurityContext.FSGroup)
	require.Equal(t, corev1.FSGroupChangeOnRootMismatch, *pod.SecurityContext.FSGroupChangePolicy)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)

	container := pod.Containers[0]
	require.Equal(t, "probe", container.Name)
	require.Equal(t, ncclimage.Image, container.Image)
	require.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy)
	require.Equal(t, []string{"/usr/local/bin/torchrun"}, container.Command)
	require.Equal(t, []string{
		"--nnodes=2",
		"--nproc-per-node=1",
		"--node-rank=$(JOB_COMPLETION_INDEX)",
		"--master-addr=" + NCCLRDMAService,
		"--master-port=29500",
		"/opt/taugrid/torchrun-rdma-probe.py",
	}, container.Args)
	require.Equal(t, map[string]string{
		"TAUGRID_BACKEND":       "nccl",
		"TAUGRID_LIVE_RDMA":     "1",
		"TAUGRID_RUN_ID":        "nccl-rdma-0123456789abcdef0123456789abcdef",
		"TAUGRID_AUTH_KEY_FILE": "/var/run/taugrid-auth/key",
		"TAUGRID_ELEMENTS":      "16777216",
		"TAUGRID_WARMUP":        "5",
		"TAUGRID_ITERATIONS":    "20",
		"NCCL_DEBUG":            "INFO",
		"NCCL_DEBUG_SUBSYS":     "INIT,NET",
		"NCCL_IB_DISABLE":       "0",
		"HOME":                  "/tmp",
		"TMPDIR":                "/tmp",
		"PYTHONUNBUFFERED":      "1",
	}, literalEnv(container.Env))
	nodeEnv := findEnv(container.Env, "TAUGRID_NODE_NAME")
	require.NotNil(t, nodeEnv)
	require.NotNil(t, nodeEnv.ValueFrom)
	require.NotNil(t, nodeEnv.ValueFrom.FieldRef)
	require.Equal(t, "spec.nodeName", nodeEnv.ValueFrom.FieldRef.FieldPath)
	indexEnv := findEnv(container.Env, "JOB_COMPLETION_INDEX")
	require.NotNil(t, indexEnv)
	require.NotNil(t, indexEnv.ValueFrom)
	require.NotNil(t, indexEnv.ValueFrom.FieldRef)
	require.Equal(t, "metadata.annotations['batch.kubernetes.io/job-completion-index']",
		indexEnv.ValueFrom.FieldRef.FieldPath)

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.RunAsNonRoot)
	require.True(t, *container.SecurityContext.RunAsNonRoot)
	require.Equal(t, int64(1000), *container.SecurityContext.RunAsUser)
	require.Equal(t, int64(1000), *container.SecurityContext.RunAsGroup)
	require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
	require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem)
	require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, container.SecurityContext.SeccompProfile.Type)
	require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
	require.Empty(t, container.SecurityContext.Capabilities.Add)
	require.Equal(t, "/dev/termination-log", container.TerminationMessagePath)
	require.Equal(t, corev1.TerminationMessageReadFile, container.TerminationMessagePolicy)

	require.Equal(t, "4", container.Resources.Requests.Cpu().String())
	require.Equal(t, "16Gi", container.Resources.Requests.Memory().String())
	gpuRequest := container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]
	rdmaRequest := container.Resources.Requests[corev1.ResourceName("rdma/rdma_shared_device_a")]
	require.Equal(t, "1", gpuRequest.String())
	require.Equal(t, "1", rdmaRequest.String())
	require.Equal(t, "8", container.Resources.Limits.Cpu().String())
	require.Equal(t, "32Gi", container.Resources.Limits.Memory().String())
	require.Len(t, pod.Volumes, 4)
	require.ElementsMatch(t, []string{"probe", "auth", "tmp", "dshm"}, volumeNames(pod.Volumes))
	require.Len(t, container.VolumeMounts, 4)
	require.True(t, volumeMount(container.VolumeMounts, "probe").ReadOnly)
	require.True(t, volumeMount(container.VolumeMounts, "auth").ReadOnly)
	require.Equal(t, "/tmp", volumeMount(container.VolumeMounts, "tmp").MountPath)
	require.Equal(t, "/dev/shm", volumeMount(container.VolumeMounts, "dshm").MountPath)

	require.Len(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
	term := pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	require.Equal(t, "kubernetes.io/hostname", term.TopologyKey)
	require.Equal(t, map[string]string{
		"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:              "nccl-rdma-0123456789abcdef0123456789abcdef",
	}, term.LabelSelector.MatchLabels)

	text := string(data)
	for _, forbidden := range []string{
		"/bin/bash", "/bin/sh", "apt-get ", "pip install", "curl ", "wget ",
		"hostPath:", "persistentVolumeClaim:", "serviceAccountToken:",
		"privileged: true", "capabilities:\n            add:", "sshd", "mpirun",
	} {
		require.NotContains(t, text, forbidden)
	}
}

func TestNCCLRDMAQualifiedImageCoordinatesMatchFixture(t *testing.T) {
	_, job := renderNCCLRDMAFixture(t)
	require.NoError(t, ncclimage.Validate(
		job.Spec.Template.Spec.Containers[0].Image,
		ncclimage.Repository,
		ncclimage.IndexDigest,
		ncclimage.LinuxAMD64Digest,
		ncclimage.LinuxAMD64Config,
	))
	require.NotEqual(t, ncclimage.IndexDigest, ncclimage.LinuxAMD64Digest)
	require.Equal(t, "25.11-py3", ncclimage.QualifiedTag)
	require.Equal(t, "13.0.2", ncclimage.QualifiedCUDA)
	require.Equal(t, "2.28.8", ncclimage.QualifiedNCCL)
	require.Equal(t, "2.10.0a0+b558c98", ncclimage.QualifiedPyTorch)
}

func TestNCCLRDMAQualifiedSupplyChainEvidenceIsDocumented(t *testing.T) {
	evidence := ncclimage.QualifiedSupplyChainEvidence()
	require.NoError(t, ncclimage.ValidateSupplyChainEvidence(evidence))
	data, err := ReadRepoFile("tests/e2e/README.md")
	require.NoError(t, err)
	text := string(data)
	for _, value := range []string{
		ncclimage.IndexDigest,
		ncclimage.LinuxAMD64Digest,
		ncclimage.LinuxAMD64Config,
		evidence.SBOMManifestDigest,
		evidence.SBOMLayerDigest,
		evidence.VEXManifestDigest,
		evidence.VEXLayerDigest,
		evidence.SignatureManifestDigest,
		evidence.SignatureLayerDigest,
		evidence.LicensePath,
		"NVIDIA Software License Agreement",
		"NVIDIA AI Product Agreement terms",
		"379",
		"262 `exploitable`",
		"110 `in-triage`",
		"7 `not-affected`",
		"no severity ratings",
		"not be represented as a verified signature",
		"No OCI license label was observed",
		"platform-approval input",
		"suppress or conceal findings",
		"mlx5, verbs, RDMA CM, and UCX",
		"effective capabilities",
	} {
		t.Run(value, func(t *testing.T) {
			require.Contains(t, text, value)
		})
	}
}

func TestNCCLRDMAProbeScriptIsStaticAuthenticatedAndShellFree(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	for _, required := range []string{
		"world_size != 2", "torch.cuda.device_count() != 1",
		"resource.getrlimit(resource.RLIMIT_MEMLOCK)",
		"hmac.new", "hmac.compare_digest", "peer-auth-hmac-mismatch",
		"len(nodes) != world_size", "dist.all_reduce", "ReduceOp.MAX",
		"TAUGRID_RDMA_RANK_PASS", "TAUGRID_RDMA_PASS", "TAUGRID_CONTROL_PLANE_PASS",
	} {
		require.Contains(t, text, required)
	}
	for _, forbidden := range []string{
		"subprocess", "os.system", "shell=True", "pip install", "apt-get", "ssh",
		"print(key", "print(auth_key", `"mac": received_mac`,
	} {
		require.NotContains(t, text, forbidden)
	}

	tempScript := filepath.Join(t.TempDir(), "probe.py")
	require.NoError(t, os.WriteFile(tempScript, data, 0o600))
	command := exec.Command("python3", "-m", "py_compile", tempScript)
	require.NoError(t, command.Run())

	authSmoke := exec.Command("python3", "-c", `
import runpy
import sys
import types

torch = types.ModuleType("torch")
torch.__path__ = []
distributed = types.ModuleType("torch.distributed")
torch.distributed = distributed
sys.modules["torch"] = torch
sys.modules["torch.distributed"] = distributed

probe = runpy.run_path(sys.argv[1], run_name="taugrid_probe_module")
key = b"k" * 32
run_id = "nccl-rdma-0123456789abcdef0123456789abcdef"
receipts = [
    probe["signed_identity"](key, {
        "run_id": run_id, "rank": rank, "node": f"h200-{rank}",
        "host": f"pod-{rank}", "nonce": f"nonce-{rank}",
    })
    for rank in range(2)
]
verified = probe["verify_identities"](key, receipts, run_id, 2)
assert [item["rank"] for item in verified] == [0, 1]
receipts[1]["mac"] = "0" * 64
try:
    probe["verify_identities"](key, receipts, run_id, 2)
except SystemExit:
    pass
else:
    raise AssertionError("tampered HMAC receipt was accepted")
`, tempScript)
	require.NoError(t, authSmoke.Run())
}

func TestNCCLRDMASupportResourcesAreRunScopedAndLeastPrivilege(t *testing.T) {
	const invocation = "nccl-rdma-0123456789abcdef0123456789abcdef"
	resources, err := BuildNCCLRDMASupportResources("approved-nccl-rdma", invocation, "print('probe')\n", make([]byte, 32))
	require.NoError(t, err)

	require.NotNil(t, resources.ServiceAccount.AutomountServiceAccountToken)
	require.False(t, *resources.ServiceAccount.AutomountServiceAccountToken)
	require.True(t, *resources.ConfigMap.Immutable)
	require.Equal(t, "print('probe')\n", resources.ConfigMap.Data["torchrun-rdma-probe.py"])
	require.True(t, *resources.Secret.Immutable)
	require.Equal(t, corev1.SecretTypeOpaque, resources.Secret.Type)
	require.Len(t, resources.Secret.Data["key"], 32)
	for _, metadata := range []map[string]string{
		resources.ServiceAccount.Labels,
		resources.ConfigMap.Labels,
		resources.Secret.Labels,
		resources.Service.Labels,
		resources.NetworkPolicy.Labels,
	} {
		require.Equal(t, invocation, metadata[NCCLRDMAInvocationKey])
	}

	require.Equal(t, corev1.ClusterIPNone, resources.Service.Spec.ClusterIP)
	require.Equal(t, map[string]string{
		"batch.kubernetes.io/job-name":             NCCLRDMAJobName,
		"batch.kubernetes.io/job-completion-index": "0",
		"e2e.taugrid.azure.com/diagnostic":         "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:                      invocation,
	}, resources.Service.Spec.Selector)
	require.Equal(t, int32(29500), resources.Service.Spec.Ports[0].Port)

	policy := resources.NetworkPolicy.Spec
	require.ElementsMatch(t, []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	}, policy.PolicyTypes)
	require.Equal(t, invocation, policy.PodSelector.MatchLabels[NCCLRDMAInvocationKey])
	require.Equal(t, NCCLRDMAJobName, policy.PodSelector.MatchLabels["batch.kubernetes.io/job-name"])
	require.Len(t, policy.Ingress, 1)
	require.Len(t, policy.Ingress[0].From, 1)
	require.Equal(t, policy.PodSelector, *policy.Ingress[0].From[0].PodSelector)
	require.Len(t, policy.Egress, 2)
	require.Equal(t, policy.PodSelector, *policy.Egress[0].To[0].PodSelector)
	require.Equal(t, "kube-system", policy.Egress[1].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	require.Equal(t, "kube-dns", policy.Egress[1].To[0].PodSelector.MatchLabels["k8s-app"])
	require.ElementsMatch(t, []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP}, []corev1.Protocol{
		*policy.Egress[1].Ports[0].Protocol,
		*policy.Egress[1].Ports[1].Protocol,
	})
}

func TestNCCLRDMAInvocationMarkerMustBeUniqueShape(t *testing.T) {
	for _, invocation := range []string{"", "nccl-rdma-static", "nccl-rdma-0123456789ABCDEF0123456789ABCDEF"} {
		t.Run(invocation, func(t *testing.T) {
			t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
			t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")
			t.Setenv("NCCL_RDMA_INVOCATION", invocation)
			_, err := ReadFixtureWithSubstitutions(ncclRDMATestFixture)
			require.Error(t, err)
			require.Contains(t, err.Error(), "32 lowercase hex")
		})
	}
}

func TestParseNCCLRDMAOutputAcceptsAuthenticatedLiveNCCLRun(t *testing.T) {
	result, err := ParseNCCLRDMAOutput(validNCCLRDMAOutput)
	require.NoError(t, err)
	require.Equal(t, [2]string{"h200-a", "h200-b"}, result.Nodes)
	require.Equal(t, 12.5, result.MaxAlgBW)
	require.Equal(t, 12.5, result.MaxBusBW)
	require.Len(t, result.Memlock, 2)
	require.Len(t, result.Runtime, 2)
	require.Len(t, result.Measurements, 2)
	require.Len(t, result.IBEvidence, 2)
	require.Contains(t, result.IBEvidence[0], "rank=0")
	require.Contains(t, result.IBEvidence[1], "rank=1")
	require.Equal(t, "GPU-aaaaaaaa", result.Runtime[0].GPUUUID)
	require.Equal(t, "eth2", result.Runtime[1].RDMAInterface)
	require.Equal(t, "2.28.8", result.NCCLVersion)
	require.Equal(t, int64(4206821376), result.Memlock[0].Soft)
}

func TestParseNCCLRDMAOutputFailsClosed(t *testing.T) {
	tests := map[string]string{
		"gloo sentinel": strings.Replace(
			validNCCLRDMAOutput,
			"TAUGRID_RDMA_PASS",
			"TAUGRID_CONTROL_PLANE_PASS",
			1,
		),
		"socket fallback": validNCCLRDMAOutput + "\nNCCL INFO NET/Socket : Using eth0\n",
		"missing ib": strings.Replace(
			validNCCLRDMAOutput,
			"NCCL INFO NET/IB : Using [0]mlx5_0:1/IB [RO]; OOB eth1:10.0.0.10<0>",
			"",
			1,
		),
		"rank one missing ib": strings.Replace(
			validNCCLRDMAOutput,
			"NCCL INFO NET/IB : Using [0]mlx5_1:1/IB [RO]; OOB eth2:10.0.0.11<0>",
			"",
			1,
		),
		"runtime device not used by NCCL": strings.Replace(
			validNCCLRDMAOutput,
			`"rdma_device":"mlx5_1"`,
			`"rdma_device":"mlx5_9"`,
			1,
		),
		"unsafe ib evidence": strings.Replace(
			validNCCLRDMAOutput,
			"NCCL INFO NET/IB : Using [0]mlx5_0:1/IB [RO]; OOB eth1:10.0.0.10<0>",
			`NCCL INFO NET/IB : Using [0]mlx5_0:1/IB [RO]; OOB eth1:10.0.0.10<0> {"token":"unsafe"}`,
			1,
		),
		"verbs failure":     validNCCLRDMAOutput + "\nibv_reg_mr failed\n",
		"probe failure":     validNCCLRDMAOutput + "\nTAUGRID_RDMA_FAIL reason=test\n",
		"missing rank one":  strings.Replace(validNCCLRDMAOutput, "TAUGRID_RDMA_RANK_PASS rank=1\n", "", 1),
		"missing peer auth": strings.Replace(validNCCLRDMAOutput, "TAUGRID_PEER_AUTH rank=1 peers=2\n", "", 1),
		"extra peer auth":   validNCCLRDMAOutput + "TAUGRID_PEER_AUTH rank=2 peers=2\n",
		"extra rank pass":   validNCCLRDMAOutput + "TAUGRID_RDMA_RANK_PASS rank=2\n",
		"missing memlock":   strings.Replace(validNCCLRDMAOutput, memlockLine(1), "", 1),
		"duplicate memlock": validNCCLRDMAOutput + memlockLine(1),
		"missing runtime": strings.Replace(
			validNCCLRDMAOutput,
			runtimeLine(1),
			"",
			1,
		),
		"missing gpu UUID":       strings.Replace(validNCCLRDMAOutput, `"gpu_uuid":"GPU-aaaaaaaa"`, `"gpu_uuid":""`, 1),
		"missing RDMA interface": strings.Replace(validNCCLRDMAOutput, `"rdma_interface":"eth2"`, `"rdma_interface":""`, 1),
		"inactive RDMA link":     strings.Replace(validNCCLRDMAOutput, `"rdma_link_state":"4: ACTIVE"`, `"rdma_link_state":"1: DOWN"`, 1),
		"extra runtime field":    strings.Replace(validNCCLRDMAOutput, `"rank":0`, `"unexpected":true,"rank":0`, 1),
		"missing measurement": strings.Replace(
			validNCCLRDMAOutput,
			measurementLine(1),
			"",
			1,
		),
		"zero measurement": strings.Replace(validNCCLRDMAOutput, `"algbw_gbps":13.4217728`, `"algbw_gbps":0`, 1),
		"unapproved environment": strings.Replace(
			validNCCLRDMAOutput,
			`"TAUGRID_WARMUP":"5"`,
			`"UNAPPROVED":"value"`,
			1,
		),
		"missing memlock rank": strings.Replace(
			validNCCLRDMAOutput,
			`"infinity":-1,"rank":0,`,
			`"infinity":-1,`,
			1,
		),
		"zero memlock":        strings.Replace(validNCCLRDMAOutput, `"soft":4206821376`, `"soft":0`, 1),
		"non-distinct nodes":  strings.Replace(validNCCLRDMAOutput, `"nodes":["h200-a","h200-b"]`, `"nodes":["h200-a","h200-a"]`, 1),
		"missing hosts":       strings.Replace(validNCCLRDMAOutput, `,"hosts":["pod-0","pod-1"]`, "", 1),
		"one host":            strings.Replace(validNCCLRDMAOutput, `"hosts":["pod-0","pod-1"]`, `"hosts":["pod-0"]`, 1),
		"wrong backend":       strings.Replace(validNCCLRDMAOutput, `"backend":"nccl"`, `"backend":"gloo"`, 1),
		"auth not verified":   strings.Replace(validNCCLRDMAOutput, `"peer_auth_verified":true`, `"peer_auth_verified":false`, 1),
		"correctness failure": strings.Replace(validNCCLRDMAOutput, `"max_error":0`, `"max_error":1`, 1),
		"missing max error":   strings.Replace(validNCCLRDMAOutput, `,"max_error":0`, "", 1),
		"zero bandwidth":      strings.Replace(validNCCLRDMAOutput, `"algbw_gbps":12.5`, `"algbw_gbps":0`, 1),
		"aggregate timing mismatch": strings.Replace(
			validNCCLRDMAOutput,
			`"max_elapsed_seconds":0.1073741824`,
			`"max_elapsed_seconds":0.2`,
			1,
		),
		"runtime pass version mismatch": strings.Replace(
			validNCCLRDMAOutput,
			`"nccl_version":"2.28.8","nodes"`,
			`"nccl_version":"2.27.0","nodes"`,
			1,
		),
	}

	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNCCLRDMAOutput(output)
			require.Error(t, err)
		})
	}
	t.Run("unlimited memlock is accepted", func(t *testing.T) {
		output := strings.ReplaceAll(validNCCLRDMAOutput, "4206821376", "-1")
		result, err := ParseNCCLRDMAOutput(output)
		require.NoError(t, err)
		require.Equal(t, int64(-1), result.Memlock[0].Soft)
	})
}

func TestNCCLRDMAOutputFailureReasonSeparatesProvenAndUnknownFailures(t *testing.T) {
	tests := map[string]struct {
		output string
		reason rdmavalidation.ReasonCode
	}{
		"socket": {
			output: validNCCLRDMAOutput + "\nNCCL INFO NET/Socket : Using eth0\n",
			reason: rdmavalidation.ReasonSocketFallbackObserved,
		},
		"verbs": {
			output: validNCCLRDMAOutput + "\nibv_reg_mr failed\n",
			reason: rdmavalidation.ReasonTransportFailure,
		},
		"correctness": {
			output: strings.Replace(validNCCLRDMAOutput, `"max_error":0`, `"max_error":1`, 1),
			reason: rdmavalidation.ReasonCorrectnessError,
		},
		"backend": {
			output: strings.Replace(validNCCLRDMAOutput, `"backend":"nccl"`, `"backend":"gloo"`, 1),
			reason: rdmavalidation.ReasonTransportFailure,
		},
		"placement": {
			output: strings.Replace(validNCCLRDMAOutput, `"nodes":["h200-a","h200-b"]`, `"nodes":["h200-a","h200-a"]`, 1),
			reason: rdmavalidation.ReasonPlacementMismatch,
		},
		"inactive link": {
			output: strings.Replace(validNCCLRDMAOutput, `"rdma_link_state":"4: ACTIVE"`, `"rdma_link_state":"1: DOWN"`, 1),
			reason: rdmavalidation.ReasonTransportFailure,
		},
		"rank IB missing": {
			output: strings.Replace(
				validNCCLRDMAOutput,
				"NCCL INFO NET/IB : Using [0]mlx5_1:1/IB [RO]; OOB eth2:10.0.0.11<0>",
				"",
				1,
			),
			reason: rdmavalidation.ReasonIBTransportNotProven,
		},
		"incomplete": {output: "incomplete output", reason: rdmavalidation.ReasonParserRejected},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNCCLRDMAOutput(test.output)
			require.Error(t, err)
			require.Equal(t, test.reason, NCCLRDMAParseFailureReason(err))
		})
	}
	t.Run("one site may span regions when region is unconstrained", func(t *testing.T) {
		nodes := baselineNCCLRDMANodes()
		nodes.Items[1].Labels["topology.kubernetes.io/region"] = "westus3"
		result := runNCCLRDMAHarnessCapacityWithTopology(t, nodes, corev1.PodList{}, "eastus2", "")
		require.NoError(t, result.err, result.output)
		require.Contains(t, result.output, "Unbounded site topology resolved")
	})
}

const validNCCLRDMAOutput = `TAUGRID_RANK_LOG_BEGIN rank=0
NCCL INFO NET/IB : Using [0]mlx5_0:1/IB [RO]; OOB eth1:10.0.0.10<0>
TAUGRID_MEMLOCK {"hard":4206821376,"infinity":-1,"rank":0,"soft":4206821376}
TAUGRID_RDMA_RUNTIME {"environment":{"NCCL_DEBUG":"INFO","NCCL_DEBUG_SUBSYS":"INIT,NET","NCCL_IB_DISABLE":"0","TAUGRID_BACKEND":"nccl","TAUGRID_ELEMENTS":"16777216","TAUGRID_ITERATIONS":"20","TAUGRID_LIVE_RDMA":"1","TAUGRID_WARMUP":"5"},"gpu_model":"NVIDIA H200","gpu_uuid":"GPU-aaaaaaaa","host":"pod-0","nccl_version":"2.28.8","node":"h200-a","rank":0,"rdma_device":"mlx5_0","rdma_interface":"eth1","rdma_link_state":"4: ACTIVE"}
TAUGRID_PEER_AUTH rank=0 peers=2
TAUGRID_RDMA_MEASUREMENT {"algbw_gbps":13.4217728,"busbw_gbps":13.4217728,"elapsed_seconds":0.1,"rank":0}
TAUGRID_RDMA_RANK_PASS rank=0
TAUGRID_RDMA_PASS {"algbw_gbps":12.5,"backend":"nccl","busbw_gbps":12.5,"hosts":["pod-0","pod-1"],"iterations":20,"max_elapsed_seconds":0.1073741824,"max_error":0,"nccl_version":"2.28.8","nodes":["h200-a","h200-b"],"payload_bytes":67108864,"peer_auth_verified":true,"seconds_per_iteration":0.00536870912,"world_size":2}
TAUGRID_RANK_LOG_END rank=0
TAUGRID_RANK_LOG_BEGIN rank=1
NCCL INFO NET/IB : Using [0]mlx5_1:1/IB [RO]; OOB eth2:10.0.0.11<0>
TAUGRID_MEMLOCK {"hard":4206821376,"infinity":-1,"rank":1,"soft":4206821376}
TAUGRID_RDMA_RUNTIME {"environment":{"NCCL_DEBUG":"INFO","NCCL_DEBUG_SUBSYS":"INIT,NET","NCCL_IB_DISABLE":"0","TAUGRID_BACKEND":"nccl","TAUGRID_ELEMENTS":"16777216","TAUGRID_ITERATIONS":"20","TAUGRID_LIVE_RDMA":"1","TAUGRID_WARMUP":"5"},"gpu_model":"NVIDIA H200","gpu_uuid":"GPU-bbbbbbbb","host":"pod-1","nccl_version":"2.28.8","node":"h200-b","rank":1,"rdma_device":"mlx5_1","rdma_interface":"eth2","rdma_link_state":"4: ACTIVE"}
TAUGRID_PEER_AUTH rank=1 peers=2
TAUGRID_RDMA_MEASUREMENT {"algbw_gbps":12.5,"busbw_gbps":12.5,"elapsed_seconds":0.1073741824,"rank":1}
TAUGRID_RDMA_RANK_PASS rank=1
TAUGRID_RANK_LOG_END rank=1
`

func memlockLine(rank int) string {
	return strings.Replace(
		"TAUGRID_MEMLOCK {\"hard\":4206821376,\"infinity\":-1,\"rank\":RANK,\"soft\":4206821376}\n",
		"RANK",
		string(rune('0'+rank)),
		1,
	)
}

func runtimeLine(rank int) string {
	for _, line := range strings.Split(validNCCLRDMAOutput, "\n") {
		if strings.HasPrefix(line, "TAUGRID_RDMA_RUNTIME ") && strings.Contains(line, fmt.Sprintf(`"rank":%d`, rank)) {
			return line + "\n"
		}
	}
	return ""
}

func measurementLine(rank int) string {
	for _, line := range strings.Split(validNCCLRDMAOutput, "\n") {
		if strings.HasPrefix(line, "TAUGRID_RDMA_MEASUREMENT ") && strings.Contains(line, fmt.Sprintf(`"rank":%d`, rank)) {
			return line + "\n"
		}
	}
	return ""
}

func literalEnv(env []corev1.EnvVar) map[string]string {
	result := map[string]string{}
	for _, item := range env {
		if item.ValueFrom == nil {
			result[item.Name] = item.Value
		}
	}
	return result
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func volumeNames(volumes []corev1.Volume) []string {
	result := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		result = append(result, volume.Name)
	}
	return result
}

func volumeMount(mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	for _, mount := range mounts {
		if mount.Name == name {
			return mount
		}
	}
	return corev1.VolumeMount{}
}

func TestNCCLRDMAFixtureHasNoUnexpectedMutableMaps(t *testing.T) {
	_, job := renderNCCLRDMAFixture(t)
	require.True(t, reflect.DeepEqual(job.Spec.Template.Labels, map[string]string{
		"app.kubernetes.io/name":           "nccl-rdma-diagnostic",
		"batch.kubernetes.io/job-name":     NCCLRDMAJobName,
		"e2e.taugrid.azure.com/diagnostic": "nccl-rdma-2x1xh200",
		NCCLRDMAInvocationKey:              "nccl-rdma-0123456789abcdef0123456789abcdef",
	}))
}

func TestNCCLRDMAFixtureJSONRoundTrip(t *testing.T) {
	_, job := renderNCCLRDMAFixture(t)
	data, err := json.Marshal(job)
	require.NoError(t, err)
	var roundTrip batchv1.Job
	require.NoError(t, json.Unmarshal(data, &roundTrip))
	require.Equal(t, job.Spec.Template.Spec.Containers[0].Command, roundTrip.Spec.Template.Spec.Containers[0].Command)
}

func TestNCCLRDMAAdmissionProbeOmitsOnlyPersistedSuspend(t *testing.T) {
	harness := ncclRDMAHarnessPath(t)
	command := exec.Command("bash", "-c", fmt.Sprintf("source %q; render_admission_probe", harness))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=approved-nccl-rdma",
		"E2E_STACK_LARGE_GPU_QUEUE=h200-rdma",
		"GPU_NODE_SELECTOR_KEY=accelerator",
		"GPU_NODE_SELECTOR_VALUE=nvidia-h200",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
	)
	output, err := command.Output()
	require.NoError(t, err)
	text := string(output)
	require.NotContains(t, text, "  suspend: true\n")
	require.Contains(t, text, "completionMode: Indexed")
	require.Contains(t, text, ncclimage.Image)
	require.NotContains(t, text, "preemptionPolicy:")
}

func TestNCCLRDMAHarnessQualifiedImageCoordinatesMatchAuthority(t *testing.T) {
	command := exec.Command(
		"bash",
		"-c",
		fmt.Sprintf("source %q; require_qualified_image", ncclRDMAHarnessPath(t)),
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestNCCLRDMAHarnessIsCreateOnlyBoundedAndMutationScoped(t *testing.T) {
	harness := ncclRDMAHarnessPath(t)
	info, err := os.Stat(harness)
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111, "documented harness must remain directly executable")
	data, err := os.ReadFile(harness)
	require.NoError(t, err)
	text := string(data)
	for _, required := range []string{
		"create-fixed-nccl-rdma-indexed-job",
		`if spec.get("admissionChecks")`,
		`if spec.get("admissionChecksStrategy")`,
		"validate_active_security_boundary",
		"./cmd/nccl-rdma-admission-check",
		"NCCL_RDMA_OPERATOR_USERNAME",
		"NCCL_RDMA_KUEUE_CONTROLLER_USERNAME",
		"NCCL_RDMA_JOB_CONTROLLER_USERNAME",
		"NCCL_RDMA_GARBAGE_COLLECTOR_USERNAME",
		"NCCL_RDMA_UNTRUSTED_USERNAME",
		"--boundary \"$SECURITY_BOUNDARY_FIXTURE\"",
		"--request-timeout=",
		"./cmd/nccl-rdma-owned-delete",
		`"--uid", uid`,
		"CLEANUP_OVERALL_SECONDS=180",
		"NCCL_RDMA_OWNED_UID_FILE",
		"NCCL_RDMA_RESULT_PATH",
		"NCCL_RDMA_WORKSPACE_ID",
		"NCCL_RDMA_EXPECTED_SITE",
		"NCCL_RDMA_EXPECTED_REGION",
		"NCCL_RDMA_EXPECTED_POOL",
		"NCCL_RDMA_EXPECTED_GPU_MODEL",
		"NCCL_RDMA_SOURCE_REVISION",
		"rdma-validation.v1",
		"tau.rdma_validation",
		"successful-create UID ledger",
		"cleanup_owned_uids",
		"validate_delete_access_boundary",
		`mktemp "${TMPDIR:-/tmp}/taugrid-nccl-rdma-owned.XXXXXX"`,
		`os.setsid()`,
		`"go", "test", "-count=1", "-v", "-timeout", "15m"`,
		`"-run", "^TestNCCLRDMA2x1H200$", "./stack/"`,
	} {
		require.Contains(t, text, required)
	}
	for _, forbidden := range []string{
		"kubectl apply", "kubectl delete", "--force", "mpijob", "MPIJob",
		"images/nccl-tests", "NCCL_RDMA_E2E_IMAGE", "cleanup_owned_invocation",
		"object_marker",
	} {
		require.NotContains(t, text, forbidden)
	}
}

func TestNCCLRDMAHarnessCleanupUsesOnlySuccessfulCreateUIDLedger(t *testing.T) {
	temp := t.TempDir()
	ledger := filepath.Join(temp, "owned-uids")
	calls := filepath.Join(temp, "delete-calls")
	require.NoError(t, os.WriteFile(
		ledger,
		[]byte(`{"group":"batch","version":"v1","resource":"jobs","namespace":"taugrid-rdma-diagnostic","name":"e2e-nccl-rdma-2x1xh200","uid":"successful-create-uid"}`+"\n"),
		0o600,
	))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
run_owned_delete_bounded() {
  printf '%%s|%%s|%%s|%%s|%%s|%%s\n' "$2" "$3" "$4" "$5" "$6" "$7" >>"$DELETE_CALLS"
}
cleanup_kube() {
  shift
  if [[ "$1" == get && "$2" == pods ]]; then
    return 0
  fi
  printf '{"metadata":{"uid":"replacement-uid","labels":{"e2e.taugrid.azure.com/invocation":"%%s"}}}\n' \
    "$NCCL_RDMA_INVOCATION"
}
cleanup_owned_uids
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_OWNED_UID_FILE="+ledger,
		"DELETE_CALLS="+calls,
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Contains(t, string(data), "successful-create-uid")
	require.NotContains(t, string(data), "replacement-uid")
}

func TestNCCLRDMAHarnessCleanupRejectsMalformedUIDLedger(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "owned-uids")
	require.NoError(t, os.WriteFile(ledger, []byte("ambiguous-create-without-uid\n"), 0o600))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
run_owned_delete_bounded() {
  echo "unexpected delete" >&2
  return 99
}
cleanup_owned_uids
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_OWNED_UID_FILE="+ledger,
	)
	output, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "malformed successful-create UID ledger entry")
	require.NotContains(t, string(output), "unexpected delete")
}

func TestNCCLRDMAHarnessCleanupPreservesSupportAfterJobDeleteFailure(t *testing.T) {
	temp := t.TempDir()
	ledger := filepath.Join(temp, "owned-uids")
	calls := filepath.Join(temp, "delete-calls")
	require.NoError(t, os.WriteFile(
		ledger,
		[]byte(
			`{"group":"","version":"v1","resource":"configmaps","namespace":"taugrid-rdma-diagnostic","name":"nccl-rdma-probe","uid":"configmap-uid"}`+"\n"+
				`{"group":"batch","version":"v1","resource":"jobs","namespace":"taugrid-rdma-diagnostic","name":"e2e-nccl-rdma-2x1xh200","uid":"job-uid"}`+"\n",
		),
		0o600,
	))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
run_owned_delete_bounded() {
  printf '%%s\n' "$*" >>"$DELETE_CALLS"
  [[ "$5" != "jobs" ]]
}
cleanup_kube() {
  shift
  return 0
}
cleanup_owned_uids
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_OWNED_UID_FILE="+ledger,
		"DELETE_CALLS="+calls,
	)
	output, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "UID-precondition deletion failed")
	data, readErr := os.ReadFile(calls)
	require.NoError(t, readErr)
	require.Contains(t, string(data), "jobs")
	require.NotContains(t, string(data), "configmaps")
}

func TestNCCLRDMAHarnessCleanupDrainsPodsBeforeRemovingNetworkPolicy(t *testing.T) {
	temp := t.TempDir()
	ledger := filepath.Join(temp, "owned-uids")
	calls := filepath.Join(temp, "delete-calls")
	podCalls := filepath.Join(temp, "pod-calls")
	require.NoError(t, os.WriteFile(podCalls, []byte("0"), 0o600))
	require.NoError(t, os.WriteFile(
		ledger,
		[]byte(
			`{"group":"networking.k8s.io","version":"v1","resource":"networkpolicies","namespace":"taugrid-rdma-diagnostic","name":"nccl-rdma-isolation","uid":"network-policy-uid"}`+"\n"+
				`{"group":"batch","version":"v1","resource":"jobs","namespace":"taugrid-rdma-diagnostic","name":"e2e-nccl-rdma-2x1xh200","uid":"job-uid"}`+"\n",
		),
		0o600,
	))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
run_owned_delete_bounded() {
  printf '%%s\n' "$5" >>"$DELETE_CALLS"
}
cleanup_get_owned_json() {
  return 0
}
cleanup_kube() {
  shift
  if [[ "$1" == get && "$2" == pods ]]; then
    count="$(cat "$POD_CALLS")"
    printf '%%s' "$((count + 1))" >"$POD_CALLS"
    ((count > 0)) || printf 'pod/owned\n'
  fi
}
cleanup_owned_uids
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_OWNED_UID_FILE="+ledger,
		"DELETE_CALLS="+calls,
		"POD_CALLS="+podCalls,
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "jobs\nnetworkpolicies\n", string(data))
}

func TestNCCLRDMAHarnessCleanupPreservesEmptyAPIGroupAndClusterScope(t *testing.T) {
	temp := t.TempDir()
	ledger := filepath.Join(temp, "owned-uids")
	calls := filepath.Join(temp, "delete-calls")
	require.NoError(t, os.WriteFile(
		ledger,
		[]byte(`{"group":"","version":"v1","resource":"namespaces","namespace":"","name":"taugrid-rdma-diagnostic","uid":"namespace-uid"}`+"\n"),
		0o600,
	))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
run_owned_delete_bounded() {
  printf '%%s|%%s|%%s|%%s|%%s|%%s\n' "$2" "$3" "$4" "$5" "$6" "$7" >>"$DELETE_CALLS"
}
cleanup_get_owned_json() {
  return 0
}
cleanup_kube() {
  shift
  return 0
}
cleanup_owned_uids
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_OWNED_UID_FILE="+ledger,
		"DELETE_CALLS="+calls,
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "||v1|namespaces|taugrid-rdma-diagnostic|namespace-uid\n", string(data))
}

func TestNCCLRDMAHarnessStopsManagedTestBeforeExitCleanup(t *testing.T) {
	temp := t.TempDir()
	ledger := filepath.Join(temp, "owned-uids")
	cleanupMarker := filepath.Join(temp, "cleanup-complete")
	childMarker := filepath.Join(temp, "child-survived")
	require.NoError(t, os.WriteFile(ledger, nil, 0o600))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
cleanup_owned_uids() {
  : >"$CLEANUP_MARKER"
}
NCCL_RDMA_OWNED_UID_FILE="$LEDGER"
trap cleanup_on_exit EXIT
trap 'stop_managed_test TERM 143' TERM
python3 - "$CHILD_MARKER" <<'PY' &
import os
import sys
import time

os.setsid()
time.sleep(2)
open(sys.argv[1], "w", encoding="utf-8").close()
PY
NCCL_RDMA_TEST_PID=$!
(sleep 0.1; kill -TERM "$$") &
wait "$NCCL_RDMA_TEST_PID"
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"LEDGER="+ledger,
		"CLEANUP_MARKER="+cleanupMarker,
		"CHILD_MARKER="+childMarker,
	)
	output, err := command.CombinedOutput()
	require.Error(t, err, string(output))
	require.FileExists(t, cleanupMarker)
	time.Sleep(2200 * time.Millisecond)
	require.NoFileExists(t, childMarker, "managed live-test child continued after harness termination")
}

func TestNCCLRDMAHarnessRunsExitCleanupAfterRedirectedSubshellFailure(t *testing.T) {
	cleanupMarker := filepath.Join(t.TempDir(), "cleanup-complete")
	command := exec.Command("bash", "-c", fmt.Sprintf(`
source %q
cleanup_owned_uids() {
  : >"$CLEANUP_MARKER"
}
NCCL_RDMA_OWNED_UID_FILE="$CLEANUP_MARKER.ledger"
: >"$NCCL_RDMA_OWNED_UID_FILE"
trap cleanup_on_exit EXIT
( false ) >"$CLEANUP_MARKER.output" 2>&1
echo "unexpected continuation" >&2
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(), "CLEANUP_MARKER="+cleanupMarker)
	output, err := command.CombinedOutput()
	require.Error(t, err, string(output))
	require.FileExists(t, cleanupMarker)
	require.NotContains(t, string(output), "unexpected continuation")
}

func TestNCCLRDMAHarnessResolvesRelativeResultPathAgainstLaunchDirectory(t *testing.T) {
	temp := t.TempDir()
	launchDir := filepath.Join(temp, "launch")
	require.NoError(t, os.MkdirAll(launchDir, 0o755))
	fakeGit := filepath.Join(temp, "git")
	require.NoError(t, os.WriteFile(fakeGit, []byte(`#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" status --porcelain "*) exit 0 ;;
  *" rev-parse HEAD "*) printf '%040d\n' 1 ;;
  *) echo "unexpected fake git arguments: $*" >&2; exit 2 ;;
esac
`), 0o700))
	command := exec.Command("bash", "-c", fmt.Sprintf(`
cd "$LAUNCH_DIR"
source %q
prepare_result_contract
printf '%%s' "$NCCL_RDMA_RESULT_PATH"
`, ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"PATH="+temp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LAUNCH_DIR="+launchDir,
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"NCCL_RDMA_RESULT_PATH=relative/result.json",
		"NCCL_RDMA_WORKSPACE_ID=taugrid-rdma",
		"NCCL_RDMA_CLUSTER=h200-validation",
		"NCCL_RDMA_EXPECTED_POOL=h200pool",
		"NCCL_RDMA_EXPECTED_GPU_MODEL=NVIDIA H200",
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Equal(t, filepath.Join(launchDir, "relative/result.json"), string(output))
}

func TestNCCLRDMAHarnessCapacityPreflightUsesRequestsAndAllowsFifteenOfSixteenGPUs(t *testing.T) {
	nodes := baselineNCCLRDMANodes()
	pods := corev1.PodList{Items: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-gpu", Namespace: "research"},
		Spec: corev1.PodSpec{
			NodeName: "h200-a",
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:                    resource.MustParse("1"),
					corev1.ResourceMemory:                 resource.MustParse("8Gi"),
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
				}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}}}
	result := runNCCLRDMAHarnessCapacity(t, nodes, pods)
	require.NoError(t, result.err, result.output)
	require.Contains(t, result.output, "Request-based capacity preflight passed")

	tests := map[string]func(*corev1.NodeList, *corev1.PodList){
		"rdma contention": func(_ *corev1.NodeList, pods *corev1.PodList) {
			pods.Items[0].Spec.Containers[0].Resources.Requests[corev1.ResourceName("rdma/rdma_shared_device_a")] =
				resource.MustParse("1")
		},
		"pending gpu race": func(_ *corev1.NodeList, pods *corev1.PodList) {
			pods.Items = append(pods.Items, corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending-gpu", Namespace: "research"},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					}},
				}}},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			})
		},
		"insufficient gpu": func(_ *corev1.NodeList, pods *corev1.PodList) {
			pods.Items[0].Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")] =
				resource.MustParse("8")
		},
		"insufficient cpu": func(_ *corev1.NodeList, pods *corev1.PodList) {
			pods.Items[0].Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] =
				resource.MustParse("30")
		},
		"insufficient memory": func(_ *corev1.NodeList, pods *corev1.PodList) {
			pods.Items[0].Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] =
				resource.MustParse("250Gi")
		},
		"wrong site": func(nodes *corev1.NodeList, _ *corev1.PodList) {
			nodes.Items[1].Labels[rdmavalidation.LegacyUnboundedSiteLabelKey] = "westus3"
		},
		"wrong region": func(nodes *corev1.NodeList, _ *corev1.PodList) {
			nodes.Items[1].Labels["topology.kubernetes.io/region"] = "westus3"
		},
		"wrong pool": func(nodes *corev1.NodeList, _ *corev1.PodList) {
			nodes.Items[1].Labels["kubernetes.azure.com/agentpool"] = "otherpool"
		},
		"wrong GPU model": func(nodes *corev1.NodeList, _ *corev1.PodList) {
			nodes.Items[1].Labels["nvidia.com/gpu.product"] = "NVIDIA-A100"
		},
		"unexpected taint": func(nodes *corev1.NodeList, _ *corev1.PodList) {
			nodes.Items[1].Spec.Taints = []corev1.Taint{{
				Key: "maintenance", Value: "planned", Effect: corev1.TaintEffectNoSchedule,
			}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidateNodes := nodes.DeepCopy()
			candidatePods := pods.DeepCopy()
			mutate(candidateNodes, candidatePods)
			result := runNCCLRDMAHarnessCapacity(t, *candidateNodes, *candidatePods)
			require.Error(t, result.err)
		})
	}
}

func TestNCCLRDMAHarnessCapacityPreflightResolvesExactUnboundedSiteLabels(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*corev1.NodeList)
		expectedSite string
		wantError    string
		wantOutput   string
	}{
		"canonical": {
			mutate: func(nodes *corev1.NodeList) {
				for index := range nodes.Items {
					delete(nodes.Items[index].Labels, rdmavalidation.LegacyUnboundedSiteLabelKey)
					nodes.Items[index].Labels[rdmavalidation.UnboundedSiteLabelKey] = "eastus2"
				}
			},
			expectedSite: "eastus2", wantOutput: rdmavalidation.UnboundedSiteLabelKey,
		},
		"legacy fallback": {
			mutate:       func(*corev1.NodeList) {},
			expectedSite: "eastus2", wantOutput: rdmavalidation.LegacyUnboundedSiteLabelKey,
		},
		"both same prefers canonical": {
			mutate: func(nodes *corev1.NodeList) {
				for index := range nodes.Items {
					nodes.Items[index].Labels[rdmavalidation.UnboundedSiteLabelKey] = "eastus2"
				}
			},
			expectedSite: "eastus2", wantOutput: rdmavalidation.UnboundedSiteLabelKey,
		},
		"both conflict fails closed": {
			mutate: func(nodes *corev1.NodeList) {
				for index := range nodes.Items {
					nodes.Items[index].Labels[rdmavalidation.UnboundedSiteLabelKey] = "eastus2"
					nodes.Items[index].Labels[rdmavalidation.LegacyUnboundedSiteLabelKey] = "westus3"
				}
			},
			expectedSite: "eastus2", wantError: "conflicting Unbounded site labels",
		},
		"absent is not applicable": {
			mutate: func(nodes *corev1.NodeList) {
				for index := range nodes.Items {
					delete(nodes.Items[index].Labels, rdmavalidation.LegacyUnboundedSiteLabelKey)
				}
			},
			wantOutput: "Unbounded site topology is not applicable",
		},
		"partial fails closed": {
			mutate: func(nodes *corev1.NodeList) {
				delete(nodes.Items[1].Labels, rdmavalidation.LegacyUnboundedSiteLabelKey)
			},
			expectedSite: "eastus2", wantError: "site label evidence is partial",
		},
		"disagreement fails closed": {
			mutate: func(nodes *corev1.NodeList) {
				nodes.Items[1].Labels[rdmavalidation.LegacyUnboundedSiteLabelKey] = "westus3"
			},
			expectedSite: "eastus2", wantError: "resolve to different Unbounded sites",
		},
		"lookalike is ignored": {
			mutate: func(nodes *corev1.NodeList) {
				for index := range nodes.Items {
					delete(nodes.Items[index].Labels, rdmavalidation.LegacyUnboundedSiteLabelKey)
					nodes.Items[index].Labels["example.com/unbounded-cloud.io/site"] = "eastus2"
				}
			},
			wantOutput: "Unbounded site topology is not applicable",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			nodes := baselineNCCLRDMANodes()
			test.mutate(&nodes)
			result := runNCCLRDMAHarnessCapacityWithSite(t, nodes, corev1.PodList{}, test.expectedSite)
			if test.wantError != "" {
				require.Error(t, result.err)
				require.Contains(t, result.output, test.wantError)
				return
			}
			require.NoError(t, result.err, result.output)
			require.Contains(t, result.output, test.wantOutput)
		})
	}
}

func TestNCCLRDMAHarnessRequiresIdleDedicatedKueueResources(t *testing.T) {
	localQueue := map[string]interface{}{
		"spec": map[string]interface{}{"clusterQueue": "h200-rdma-cq"},
		"status": map[string]interface{}{
			"pendingWorkloads": float64(0), "reservingWorkloads": float64(0),
			"admittedWorkloads": float64(0),
			"conditions":        []interface{}{map[string]interface{}{"type": "Active", "status": "True"}},
		},
	}
	clusterQueue := map[string]interface{}{
		"spec": map[string]interface{}{
			"preemption": map[string]interface{}{
				"withinClusterQueue": "Never", "reclaimWithinCohort": "Never",
				"borrowWithinCohort": map[string]interface{}{"policy": "Never"},
			},
		},
		"status": map[string]interface{}{
			"pendingWorkloads": float64(0), "reservingWorkloads": float64(0),
			"admittedWorkloads": float64(0),
		},
	}
	result := runNCCLRDMAHarnessQueue(t, localQueue, clusterQueue)
	require.NoError(t, result.err, result.output)

	for _, tc := range []struct {
		name   string
		target string
		field  string
	}{
		{name: "local pending", target: "local", field: "pendingWorkloads"},
		{name: "local reserving", target: "local", field: "reservingWorkloads"},
		{name: "cluster admitted", target: "cluster", field: "admittedWorkloads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidateLocal := deepCopyJSONMap(t, localQueue)
			candidateCluster := deepCopyJSONMap(t, clusterQueue)
			target := candidateLocal
			if tc.target == "cluster" {
				target = candidateCluster
			}
			target["status"].(map[string]interface{})[tc.field] = float64(1)
			result := runNCCLRDMAHarnessQueue(t, candidateLocal, candidateCluster)
			require.Error(t, result.err)
			require.Contains(t, result.output, "is not idle")
		})
	}
}

func TestNCCLRDMAHarnessRejectsStaleWorkloadAndConsumedQuota(t *testing.T) {
	result := runNCCLRDMAHarnessFixedObjectCheck(t, "", "0")
	require.NoError(t, result.err, result.output)

	result = runNCCLRDMAHarnessFixedObjectCheck(t, "workload.kueue.x-k8s.io/stale", "0")
	require.Error(t, result.err)
	require.Contains(t, result.output, "stale Kueue Workloads")

	result = runNCCLRDMAHarnessFixedObjectCheck(t, "", "1")
	require.Error(t, result.err)
	require.Contains(t, result.output, "Workload quota is not fully free")
}

func TestNCCLRDMAHarnessRequiresUntrustedDeleteDenial(t *testing.T) {
	result := runNCCLRDMAHarnessDeleteBoundary(t, "no")
	require.NoError(t, result.err, result.output)

	result = runNCCLRDMAHarnessDeleteBoundary(t, "yes")
	require.Error(t, result.err)
	require.Contains(t, result.output, "can delete protected")

	result = runNCCLRDMAHarnessDeleteBoundary(t, "error")
	require.Error(t, result.err)
	require.Contains(t, result.output, "cannot evaluate untrusted DELETE authority")
}

type ncclRDMACommandResult struct {
	output string
	err    error
}

func runNCCLRDMAHarnessCapacity(
	t *testing.T,
	nodes corev1.NodeList,
	pods corev1.PodList,
) ncclRDMACommandResult {
	return runNCCLRDMAHarnessCapacityWithTopology(t, nodes, pods, "eastus2", "eastus2euap")
}

func runNCCLRDMAHarnessCapacityWithSite(
	t *testing.T,
	nodes corev1.NodeList,
	pods corev1.PodList,
	expectedSite string,
) ncclRDMACommandResult {
	return runNCCLRDMAHarnessCapacityWithTopology(t, nodes, pods, expectedSite, "eastus2euap")
}

func runNCCLRDMAHarnessCapacityWithTopology(
	t *testing.T,
	nodes corev1.NodeList,
	pods corev1.PodList,
	expectedSite string,
	expectedRegion string,
) ncclRDMACommandResult {
	t.Helper()
	temp := t.TempDir()
	nodesPath := filepath.Join(temp, "nodes.json")
	podsPath := filepath.Join(temp, "pods.json")
	fakeKubectl := filepath.Join(temp, "kubectl")
	nodesJSON, err := json.Marshal(nodes)
	require.NoError(t, err)
	podsJSON, err := json.Marshal(pods)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(nodesPath, nodesJSON, 0o600))
	require.NoError(t, os.WriteFile(podsPath, podsJSON, 0o600))
	require.NoError(t, os.WriteFile(fakeKubectl, []byte(`#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" get nodes "*) cat "$FAKE_NODES" ;;
  *" get pods --all-namespaces "*) cat "$FAKE_PODS" ;;
  *) echo "unexpected fake kubectl arguments: $*" >&2; exit 2 ;;
esac
`), 0o700))

	command := exec.Command("bash", "-c", fmt.Sprintf("source %q; validate_capacity", ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"PATH="+temp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_NODES="+nodesPath,
		"FAKE_PODS="+podsPath,
		"NCCL_RDMA_KUBECONFIG=/tmp/not-used",
		"NCCL_RDMA_KUBE_CONTEXT=offline",
		"NCCL_RDMA_H200_SELECTOR=accelerator=nvidia-h200",
		"NCCL_RDMA_EXPECTED_SITE="+expectedSite,
		"NCCL_RDMA_EXPECTED_REGION="+expectedRegion,
		"NCCL_RDMA_EXPECTED_POOL=h200pool",
		"NCCL_RDMA_EXPECTED_GPU_MODEL=NVIDIA H200",
	)
	output, commandErr := command.CombinedOutput()
	return ncclRDMACommandResult{output: string(output), err: commandErr}
}

func runNCCLRDMAHarnessQueue(
	t *testing.T,
	localQueue, clusterQueue map[string]interface{},
) ncclRDMACommandResult {
	t.Helper()
	temp := t.TempDir()
	localQueuePath := filepath.Join(temp, "localqueue.json")
	clusterQueuePath := filepath.Join(temp, "clusterqueue.json")
	fakeKubectl := filepath.Join(temp, "kubectl")
	localQueueJSON, err := json.Marshal(localQueue)
	require.NoError(t, err)
	clusterQueueJSON, err := json.Marshal(clusterQueue)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(localQueuePath, localQueueJSON, 0o600))
	require.NoError(t, os.WriteFile(clusterQueuePath, clusterQueueJSON, 0o600))
	require.NoError(t, os.WriteFile(fakeKubectl, []byte(`#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" get --raw /apis/batch/v1 "*) printf '{}' ;;
  *" get localqueue.kueue.x-k8s.io "*) cat "$FAKE_LOCAL_QUEUE" ;;
  *" get clusterqueue.kueue.x-k8s.io "*) cat "$FAKE_CLUSTER_QUEUE" ;;
  *) echo "unexpected fake kubectl arguments: $*" >&2; exit 2 ;;
esac
`), 0o700))

	command := exec.Command("bash", "-c", fmt.Sprintf("source %q; validate_api_and_queue", ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"PATH="+temp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_LOCAL_QUEUE="+localQueuePath,
		"FAKE_CLUSTER_QUEUE="+clusterQueuePath,
		"NCCL_RDMA_KUBECONFIG=/tmp/not-used",
		"NCCL_RDMA_KUBE_CONTEXT=offline",
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"E2E_STACK_LARGE_GPU_QUEUE=h200-rdma",
	)
	output, commandErr := command.CombinedOutput()
	return ncclRDMACommandResult{output: string(output), err: commandErr}
}

func runNCCLRDMAHarnessFixedObjectCheck(
	t *testing.T,
	workloads, usedWorkloadQuota string,
) ncclRDMACommandResult {
	t.Helper()
	temp := t.TempDir()
	fakeKubectl := filepath.Join(temp, "kubectl")
	require.NoError(t, os.WriteFile(fakeKubectl, []byte(`#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" get workloads.kueue.x-k8s.io "*) printf '%s' "$FAKE_WORKLOADS" ;;
  *" get resourcequota nccl-rdma-shape "*) printf '{"status":{"used":{"count/workloads.kueue.x-k8s.io":"%s"}}}' "$FAKE_USED_WORKLOAD_QUOTA" ;;
  *) ;;
esac
`), 0o700))
	command := exec.Command("bash", "-c", fmt.Sprintf("source %q; validate_fixed_objects_absent", ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"PATH="+temp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NCCL_RDMA_KUBECONFIG=/tmp/not-used",
		"NCCL_RDMA_KUBE_CONTEXT=offline",
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"FAKE_WORKLOADS="+workloads,
		"FAKE_USED_WORKLOAD_QUOTA="+usedWorkloadQuota,
	)
	output, commandErr := command.CombinedOutput()
	return ncclRDMACommandResult{output: string(output), err: commandErr}
}

func runNCCLRDMAHarnessDeleteBoundary(t *testing.T, untrustedAnswer string) ncclRDMACommandResult {
	t.Helper()
	temp := t.TempDir()
	fakeKubectl := filepath.Join(temp, "kubectl")
	require.NoError(t, os.WriteFile(fakeKubectl, []byte(`#!/usr/bin/env bash
set -euo pipefail
if [[ " $* " == *" --as=untrusted@example.com "* ]]; then
  case "$UNTRUSTED_ANSWER" in
    no) printf 'no\n'; exit 1 ;;
    yes) printf 'yes\n' ;;
    error) printf 'authorization query failed\n' >&2; exit 2 ;;
  esac
else
  printf 'yes\n'
fi
`), 0o700))
	command := exec.Command("bash", "-c", fmt.Sprintf("source %q; validate_delete_access_boundary", ncclRDMAHarnessPath(t)))
	command.Env = append(os.Environ(),
		"PATH="+temp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NCCL_RDMA_KUBECONFIG=/tmp/not-used",
		"NCCL_RDMA_KUBE_CONTEXT=offline",
		"E2E_STACK_NAMESPACE=taugrid-rdma-diagnostic",
		"NCCL_RDMA_UNTRUSTED_USERNAME=untrusted@example.com",
		"UNTRUSTED_ANSWER="+untrustedAnswer,
	)
	output, commandErr := command.CombinedOutput()
	return ncclRDMACommandResult{output: string(output), err: commandErr}
}

func deepCopyJSONMap(t *testing.T, input map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &result))
	return result
}

func baselineNCCLRDMANodes() corev1.NodeList {
	makeNode := func(name string) corev1.Node {
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					rdmavalidation.LegacyUnboundedSiteLabelKey: "eastus2",
					"topology.kubernetes.io/region":            "eastus2euap",
					"kubernetes.azure.com/agentpool":           "h200pool",
					"nvidia.com/gpu.product":                   "NVIDIA-H200",
				},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{
					Type: corev1.NodeReady, Status: corev1.ConditionTrue,
				}},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:                               resource.MustParse("32"),
					corev1.ResourceMemory:                            resource.MustParse("256Gi"),
					corev1.ResourceName("nvidia.com/gpu"):            resource.MustParse("8"),
					corev1.ResourceName("rdma/rdma_shared_device_a"): resource.MustParse("8"),
				},
			},
		}
	}
	return corev1.NodeList{Items: []corev1.Node{makeNode("h200-a"), makeNode("h200-b")}}
}

func ncclRDMAHarnessPath(t *testing.T) string {
	t.Helper()
	path, err := findRepoFile("tests/e2e/stack/harness/nccl_rdma_conformance.sh")
	require.NoError(t, err)
	return path
}
