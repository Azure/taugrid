// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	ncclRDMATestFixture = "stack/fixtures/nccl-rdma-mpijob-2x8xh200.yaml"
	ncclRDMATestImage   = "mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type mpiJobFixtureDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		SlotsPerWorker int `yaml:"slotsPerWorker"`
		RunPolicy      struct {
			Suspend                 *bool  `yaml:"suspend"`
			CleanPodPolicy          string `yaml:"cleanPodPolicy"`
			BackoffLimit            int    `yaml:"backoffLimit"`
			ActiveDeadlineSeconds   int    `yaml:"activeDeadlineSeconds"`
			TTLSecondsAfterFinished int    `yaml:"ttlSecondsAfterFinished"`
		} `yaml:"runPolicy"`
		MPIReplicaSpecs map[string]mpiReplicaFixtureDoc `yaml:"mpiReplicaSpecs"`
	} `yaml:"spec"`
}

type mpiReplicaFixtureDoc struct {
	Replicas      int    `yaml:"replicas"`
	RestartPolicy string `yaml:"restartPolicy"`
	Template      struct {
		Metadata struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Spec mpiPodFixtureDoc `yaml:"spec"`
	} `yaml:"template"`
}

type mpiPodFixtureDoc struct {
	AutomountServiceAccountToken *bool             `yaml:"automountServiceAccountToken"`
	RestartPolicy                string            `yaml:"restartPolicy"`
	ServiceAccountName           string            `yaml:"serviceAccountName"`
	NodeSelector                 map[string]string `yaml:"nodeSelector"`
	Affinity                     struct {
		PodAntiAffinity struct {
			Required []struct {
				TopologyKey   string `yaml:"topologyKey"`
				LabelSelector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"labelSelector"`
			} `yaml:"requiredDuringSchedulingIgnoredDuringExecution"`
		} `yaml:"podAntiAffinity"`
	} `yaml:"affinity"`
	Tolerations []struct {
		Key      string `yaml:"key"`
		Operator string `yaml:"operator"`
		Effect   string `yaml:"effect"`
	} `yaml:"tolerations"`
	Containers []mpiContainerFixtureDoc `yaml:"containers"`
	Volumes    []struct {
		Name     string                 `yaml:"name"`
		EmptyDir map[string]interface{} `yaml:"emptyDir"`
		HostPath map[string]interface{} `yaml:"hostPath"`
		PVC      map[string]interface{} `yaml:"persistentVolumeClaim"`
	} `yaml:"volumes"`
}

type mpiContainerFixtureDoc struct {
	Name            string   `yaml:"name"`
	Image           string   `yaml:"image"`
	Command         []string `yaml:"command"`
	SecurityContext struct {
		RunAsUser                int64 `yaml:"runAsUser"`
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		Privileged               bool  `yaml:"privileged"`
		SeccompProfile           struct {
			Type string `yaml:"type"`
		} `yaml:"seccompProfile"`
		Capabilities struct {
			Drop []string `yaml:"drop"`
			Add  []string `yaml:"add"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

func renderNCCLRDMAFixture(t *testing.T) ([]byte, mpiJobFixtureDoc) {
	t.Helper()
	t.Setenv("NCCL_RDMA_E2E_IMAGE", ncclRDMATestImage)
	t.Setenv("E2E_STACK_NAMESPACE", "approved-nccl-rdma")
	t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", "h200-rdma")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")
	t.Setenv("NCCL_RDMA_INVOCATION", "nccl-rdma-0123456789abcdef0123456789abcdef")

	data, err := ReadFixtureWithSubstitutions(ncclRDMATestFixture)
	require.NoError(t, err)
	var doc mpiJobFixtureDoc
	require.NoError(t, yaml.Unmarshal(data, &doc))
	return data, doc
}

func TestNCCLRDMAMPIJobFixtureContract(t *testing.T) {
	data, doc := renderNCCLRDMAFixture(t)

	require.Equal(t, "kubeflow.org/v2beta1", doc.APIVersion)
	require.Equal(t, "MPIJob", doc.Kind)
	require.Equal(t, "e2e-nccl-rdma-2x8xh200", doc.Metadata.Name)
	require.Equal(t, "approved-nccl-rdma", doc.Metadata.Namespace)
	require.Equal(t, "h200-rdma", doc.Metadata.Labels["kueue.x-k8s.io/queue-name"])
	require.Equal(t, "nccl-rdma-2x8xh200", doc.Metadata.Labels["e2e.taugrid.azure.com/diagnostic"])
	require.Equal(t, "nccl-rdma-0123456789abcdef0123456789abcdef", doc.Metadata.Labels["e2e.taugrid.azure.com/invocation"])

	require.Equal(t, 8, doc.Spec.SlotsPerWorker)
	require.NotNil(t, doc.Spec.RunPolicy.Suspend)
	require.True(t, *doc.Spec.RunPolicy.Suspend)
	require.Equal(t, "All", doc.Spec.RunPolicy.CleanPodPolicy)
	require.Zero(t, doc.Spec.RunPolicy.BackoffLimit)
	require.Equal(t, 900, doc.Spec.RunPolicy.ActiveDeadlineSeconds)
	require.Equal(t, 600, doc.Spec.RunPolicy.TTLSecondsAfterFinished)
	require.Len(t, doc.Spec.MPIReplicaSpecs, 2)

	launcher := doc.Spec.MPIReplicaSpecs["Launcher"]
	worker := doc.Spec.MPIReplicaSpecs["Worker"]
	require.Equal(t, 1, launcher.Replicas)
	require.Equal(t, 2, worker.Replicas)
	require.Equal(t, "Never", launcher.RestartPolicy)
	require.Equal(t, "Never", worker.RestartPolicy)
	require.Len(t, launcher.Template.Spec.Containers, 1)
	require.Len(t, worker.Template.Spec.Containers, 1)
	require.NotNil(t, launcher.Template.Spec.AutomountServiceAccountToken)
	require.NotNil(t, worker.Template.Spec.AutomountServiceAccountToken)
	require.False(t, *launcher.Template.Spec.AutomountServiceAccountToken)
	require.False(t, *worker.Template.Spec.AutomountServiceAccountToken)
	require.Empty(t, launcher.Template.Spec.ServiceAccountName)
	require.Empty(t, worker.Template.Spec.ServiceAccountName)

	workerContainer := worker.Template.Spec.Containers[0]
	wantResources := map[string]string{
		"nvidia.com/gpu":            "8",
		"rdma/rdma_shared_device_a": "1",
	}
	require.Equal(t, wantResources, workerContainer.Resources.Requests)
	require.Equal(t, wantResources, workerContainer.Resources.Limits)
	require.Equal(t, ncclRDMATestImage, workerContainer.Image)
	require.Equal(t, map[string]string{"accelerator": "nvidia-h200"}, worker.Template.Spec.NodeSelector)

	require.Len(t, worker.Template.Spec.Affinity.PodAntiAffinity.Required, 1)
	antiAffinity := worker.Template.Spec.Affinity.PodAntiAffinity.Required[0]
	require.Equal(t, "kubernetes.io/hostname", antiAffinity.TopologyKey)
	require.Equal(t, "worker", antiAffinity.LabelSelector.MatchLabels["e2e.taugrid.azure.com/role"])
	require.Len(t, worker.Template.Spec.Tolerations, 2)
	require.ElementsMatch(t, []string{"nvidia.com/gpu", "sku"}, []string{
		worker.Template.Spec.Tolerations[0].Key,
		worker.Template.Spec.Tolerations[1].Key,
	})

	require.Len(t, worker.Template.Spec.Volumes, 1)
	require.Equal(t, "dshm", worker.Template.Spec.Volumes[0].Name)
	require.Equal(t, "Memory", worker.Template.Spec.Volumes[0].EmptyDir["medium"])
	require.Equal(t, "16Gi", worker.Template.Spec.Volumes[0].EmptyDir["sizeLimit"])
	require.Empty(t, worker.Template.Spec.Volumes[0].HostPath)
	require.Empty(t, worker.Template.Spec.Volumes[0].PVC)
	require.Contains(t, workerContainer.VolumeMounts, struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	}{Name: "dshm", MountPath: "/dev/shm"})

	launcherContainer := launcher.Template.Spec.Containers[0]
	for _, container := range []mpiContainerFixtureDoc{launcherContainer, workerContainer} {
		require.Equal(t, int64(0), container.SecurityContext.RunAsUser)
		require.False(t, container.SecurityContext.Privileged)
		require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
		require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
		require.Equal(t, "RuntimeDefault", container.SecurityContext.SeccompProfile.Type)
		require.Equal(t, []string{"ALL"}, container.SecurityContext.Capabilities.Drop)
	}
	require.Empty(t, launcherContainer.SecurityContext.Capabilities.Add)
	require.Equal(t, []string{"IPC_LOCK", "SETGID", "SETUID", "SYS_CHROOT", "SYS_RESOURCE"}, workerContainer.SecurityContext.Capabilities.Add)

	text := string(data)
	for _, forbidden := range []string{
		"hostPath:", "persistentVolumeClaim:", "serviceAccountName:", "privileged: true",
		"apt-get ", "dnf ", "yum ", "pip install", "curl ", "wget ", "git clone",
		"azure.workload.identity", "aadpodidbinding",
	} {
		require.NotContains(t, text, forbidden)
	}
}

func TestNCCLRDMALauncherCommandFailsClosed(t *testing.T) {
	_, doc := renderNCCLRDMAFixture(t)
	command := strings.Join(doc.Spec.MPIReplicaSpecs["Launcher"].Template.Spec.Containers[0].Command, "\n")

	for _, required := range []string{
		"all_reduce_perf_mpi", "-np 16", "PIPESTATUS[0]",
		"NET/IB[[:space:]]*:[[:space:]]*Using",
		"NET/Socket[[:space:]]*:[[:space:]]*Using",
		"Out of bounds values", "NF == 13", "$7", "$8", "$11", "$12", NCCLRDMAPassSentinel,
	} {
		require.Contains(t, command, required)
	}
	require.Contains(t, command, "exit \"${mpi_status}\"")
	require.NotContains(t, command, "|| true")
}

func TestNCCLRDMAImageMustBeDigestPinnedInMCR(t *testing.T) {
	for _, image := range []string{
		"",
		"mcr.microsoft.com/aks/ai-runtime/nccl-tests:latest",
		"mcr.microsoft.com/aks/ai-runtime/other@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"mcr.microsoft.com/other/nccl-tests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"example.azurecr.io/nccl-tests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:short",
		"mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		t.Run(strings.ReplaceAll(image, "/", "_"), func(t *testing.T) {
			t.Setenv("NCCL_RDMA_E2E_IMAGE", image)
			t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
			t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")
			t.Setenv("NCCL_RDMA_INVOCATION", "nccl-rdma-0123456789abcdef0123456789abcdef")
			_, err := ReadFixtureWithSubstitutions(ncclRDMATestFixture)
			require.Error(t, err)
			require.Contains(t, err.Error(), "mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256")
		})
	}
}

func TestNCCLRDMAInvocationMarkerMustBeUniqueShape(t *testing.T) {
	for _, invocation := range []string{"", "nccl-rdma-static", "nccl-rdma-0123456789ABCDEF0123456789ABCDEF"} {
		t.Run(invocation, func(t *testing.T) {
			t.Setenv("NCCL_RDMA_E2E_IMAGE", ncclRDMATestImage)
			t.Setenv("GPU_NODE_SELECTOR_KEY", "accelerator")
			t.Setenv("GPU_NODE_SELECTOR_VALUE", "nvidia-h200")
			t.Setenv("NCCL_RDMA_INVOCATION", invocation)
			_, err := ReadFixtureWithSubstitutions(ncclRDMATestFixture)
			require.Error(t, err)
			require.Contains(t, err.Error(), "32 lowercase hex")
		})
	}
}

func TestNCCLRDMAAdmissionProbeOmitsPersistedSuspend(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/harness/nccl_rdma_conformance.sh")
	require.NoError(t, err)
	command := exec.Command("bash", "-c", `source "$1"; render_admission_probe`, "bash", path)
	command.Env = append(os.Environ(),
		"E2E_STACK_NAMESPACE=approved-nccl-rdma",
		"E2E_STACK_LARGE_GPU_QUEUE=h200-rdma",
		"NCCL_RDMA_E2E_IMAGE="+ncclRDMATestImage,
		"NCCL_RDMA_INVOCATION=nccl-rdma-0123456789abcdef0123456789abcdef",
		"GPU_NODE_SELECTOR_KEY=accelerator",
		"GPU_NODE_SELECTOR_VALUE=nvidia-h200",
	)
	output, err := command.Output()
	require.NoError(t, err)

	var probe mpiJobFixtureDoc
	require.NoError(t, yaml.Unmarshal(output, &probe))
	require.Nil(t, probe.Spec.RunPolicy.Suspend)
}

func TestNCCLRDMACleanupKubectlUsesBoundedRequestTimeout(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/harness/nccl_rdma_conformance.sh")
	require.NoError(t, err)
	command := exec.Command("bash", "-c", `
source "$1"
kubectl() { printf '%s\n' "$@"; }
export NCCL_RDMA_KUBECONFIG=/explicit/kubeconfig
export NCCL_RDMA_KUBE_CONTEXT=explicit-context
cleanup_kube "$((SECONDS + 3))" get pods
`, "bash", path)
	output, err := command.Output()
	require.NoError(t, err)
	require.Regexp(t, `--request-timeout=[123]s`, string(output))
	require.Contains(t, string(output), "--kubeconfig")
	require.Contains(t, string(output), "--context")
}

func TestNCCLRDMAWorkerEntrypointRaisesMemlockBeforeSSHD(t *testing.T) {
	path, err := findRepoFile("images/nccl-tests/worker-entrypoint.sh")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	memlock := strings.Index(text, "ulimit -l unlimited")
	hostKeys := strings.Index(text, "ssh-keygen -A")
	sshd := strings.Index(text, "/usr/sbin/sshd")
	require.GreaterOrEqual(t, memlock, 0)
	require.Greater(t, hostKeys, memlock)
	require.Greater(t, sshd, hostKeys)
}

func TestNCCLRDMASSHSmokeUsesManifestCapabilitiesAndAuthentication(t *testing.T) {
	path, err := findRepoFile("images/nccl-tests/smoke-ssh.sh")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	for _, capability := range []string{"IPC_LOCK", "SETGID", "SETUID", "SYS_CHROOT", "SYS_RESOURCE"} {
		require.Contains(t, text, "--cap-add "+capability)
	}
	for _, required := range []string{
		"--cap-drop ALL", "ssh-keygen", "authorized_keys", "BatchMode=yes",
		"IdentitiesOnly=yes", "NCCL_RDMA_SSH_COMMAND_OK",
	} {
		require.Contains(t, text, required)
	}
}

func TestNCCLRDMAImagePinsMatchParserContract(t *testing.T) {
	path, err := findRepoFile("images/nccl-tests/versions.json")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var versions struct {
		Architecture string `json:"architecture"`
		ImageTag     string `json:"imageTag"`
		CUDA         string `json:"cuda"`
		OpenMPI      string `json:"openmpi"`
		NCCL         string `json:"nccl"`
		RDMACore     string `json:"rdmaCore"`
		NCCLTests    struct {
			Version       string `json:"version"`
			Commit        string `json:"commit"`
			ArchiveSHA256 string `json:"archiveSha256"`
			OutputFormat  string `json:"outputFormat"`
		} `json:"ncclTests"`
	}
	require.NoError(t, json.Unmarshal(data, &versions))
	require.Equal(t, "linux/amd64", versions.Architecture)
	require.Equal(t, "cuda12.4-nccl2.21.5-tests2.16.0", versions.ImageTag)
	require.Equal(t, "12.4.1", versions.CUDA)
	require.Equal(t, "5.0.6", versions.OpenMPI)
	require.Equal(t, "2.21.5-1+cuda12.4", versions.NCCL)
	require.Equal(t, "39.0-1", versions.RDMACore)
	require.Equal(t, "2.16.0", versions.NCCLTests.Version)
	require.Len(t, versions.NCCLTests.Commit, 40)
	require.Len(t, versions.NCCLTests.ArchiveSHA256, 64)
	require.Equal(t, NCCLRDMAOutputFormat, versions.NCCLTests.OutputFormat)

	output, err := exec.Command("make", "--silent", "-C", filepath.Dir(path), "print-build-args").Output()
	require.NoError(t, err)
	buildArgs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, found := strings.Cut(line, "=")
		require.True(t, found, "build arg line must be KEY=value: %q", line)
		buildArgs[key] = value
	}
	require.NotContains(t, buildArgs, "BASE_IMAGE")
	require.Equal(t, versions.CUDA, buildArgs["CUDA_VERSION"])
	require.Equal(t, versions.OpenMPI, buildArgs["OPENMPI_VERSION"])
	require.Equal(t, versions.NCCL, buildArgs["NCCL_VERSION"])
	require.Equal(t, versions.RDMACore, buildArgs["RDMA_CORE_VERSION"])
	require.Equal(t, versions.NCCLTests.Version, buildArgs["NCCL_TESTS_VERSION"])
	require.Equal(t, versions.NCCLTests.Commit, buildArgs["NCCL_TESTS_COMMIT"])
	require.Equal(t, versions.NCCLTests.ArchiveSHA256, buildArgs["NCCL_TESTS_SHA256"])

	dockerfilePath, err := findRepoFile("images/nccl-tests/Dockerfile")
	require.NoError(t, err)
	dockerfile, err := os.ReadFile(dockerfilePath)
	require.NoError(t, err)
	for _, arg := range []string{
		"CUDA_VERSION", "OPENMPI_VERSION", "NCCL_VERSION", "RDMA_CORE_VERSION",
		"NCCL_TESTS_VERSION", "NCCL_TESTS_COMMIT", "NCCL_TESTS_SHA256",
	} {
		require.Contains(t, string(dockerfile), "ARG "+arg+"\n")
		require.NotContains(t, string(dockerfile), "ARG "+arg+"=")
	}
	require.Regexp(t,
		`(?m)^FROM mcr\.microsoft\.com/azureml/openmpi5\.0-cuda12\.4-ubuntu22\.04:[a-zA-Z0-9._-]+@sha256:[a-f0-9]{64}$`,
		string(dockerfile))
	require.Equal(t, 1, strings.Count(string(dockerfile), "\nFROM "))
	require.NotContains(t, string(dockerfile), "ARG BASE_IMAGE")
	require.NotContains(t, string(data), `"baseImage"`)

	dependabotPath, err := findRepoFile(".github/dependabot.yml")
	require.NoError(t, err)
	dependabot, err := os.ReadFile(dependabotPath)
	require.NoError(t, err)
	require.Contains(t, string(dependabot), `- "/images/nccl-tests"`)

	workflowPath, err := findRepoFile(".github/workflows/taugrid-image-validation.yml")
	require.NoError(t, err)
	workflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	require.Contains(t, string(workflow), "print-build-args")
	require.Contains(t, string(workflow), "build-args: ${{ steps.metadata.outputs.build_args }}")
	require.Contains(t, string(workflow), "smoke_make: test")
}

func TestNCCLRDMAHarnessIsExplicitBoundedAndMutationScoped(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/harness/nccl_rdma_conformance.sh")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)

	for _, required := range []string{
		"NCCL_RDMA_KUBECONFIG", "NCCL_RDMA_KUBE_CONTEXT",
		"E2E_STACK_NAMESPACE", "E2E_STACK_LARGE_GPU_QUEUE",
		"NCCL_RDMA_H200_SELECTOR", "NCCL_RDMA_E2E_IMAGE",
		"mcr.microsoft.com/aks/ai-runtime/nccl-tests@sha256",
		`pod-security\.kubernetes\.io/enforce`, "nccl-rdma-diagnostic-approved",
		"kubeflow.org/v2beta1", "kubeflow.org/mpijob",
		"kueue-controller-manager", `--dry-run=server`,
		"render_admission_probe", "spec.runPolicy.suspend=true",
		"exactly two Ready schedulable H200 nodes",
		"NCCL_RDMA_CONFIRM", "apply-fixed-nccl-rdma-mpijob",
		"NCCL_RDMA_INVOCATION", "nccl-rdma-owned-delete",
		"CLEANUP_OVERALL_SECONDS=180", "CLEANUP_REQUEST_TIMEOUT_SECONDS=10",
		`--request-timeout=`, `--timeout", f"{operation_timeout}s"`,
		"trap cleanup_on_exit EXIT",
		"go test -count=1 -v -timeout 15m", "TestNCCLRDMA2x8H200",
		"cleanup_owned_invocation", "no retry was attempted",
	} {
		require.Contains(t, text, required)
	}
	for _, forbidden := range []string{
		"kubectl apply", "kubectl delete", "kube apply", "--force", "delete --raw",
		"az aks", "helm install", "create namespace", "delete namespace",
		"stack-kueue-resources.yaml",
	} {
		require.NotContains(t, text, forbidden)
	}
}

func TestNCCLRDMALiveTestUsesCreateOnlyUIDOwnership(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/integration_test.go")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	for _, required := range []string{
		`os.Getenv("AI_RUNTIME_E2E")`, `os.Getenv("NCCL_RDMA_CONFIRM")`,
		"requireNCCLRDMANamespaceApproval", ".Create(tc.Ctx(), job",
		"claimAmbiguousNCCLRDMACreate", "Preconditions:     &metav1.Preconditions{UID: &uid}",
		"ncclRDMAInvocationKey", "waitForMPIJobSuspendedState(tc, ownedUID, false",
		"context.WithTimeout(tc.Ctx(), 3*time.Minute)",
	} {
		require.Contains(t, text, required)
	}
	require.NotContains(t, text, `ApplyFixtureWithClient(tc.Ctx(), tc.DynamicClient(), "nccl-rdma-mpijob-2x8xh200.yaml")`)
}

const validNCCLRDMAOutput = `# taugrid nccl-tests format: nccl-tests-2.16.0-table-v1
node-0:123:456 [0] NCCL INFO NET/IB : Using [0]mlx5_0:1/IB
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
     8388608       2097152     float     sum      -1    0.125   67.11  125.83      0    0.123   68.20  127.87      0
# Out of bounds values : 0 OK
NCCL_RDMA_CONFORMANCE_PASS
`

func TestParseNCCLRDMAOutput(t *testing.T) {
	result, err := ParseNCCLRDMAOutput(validNCCLRDMAOutput)
	require.NoError(t, err)
	require.Equal(t, 1, result.DataRows)
	require.Equal(t, 68.20, result.MaxAlgBW)
	require.Equal(t, 127.87, result.MaxBusBW)
}

func TestParseNCCLRDMAOutputRejectsIncompleteOrUnsafeEvidence(t *testing.T) {
	tests := map[string]string{
		"missing format":     strings.Replace(validNCCLRDMAOutput, "# taugrid nccl-tests format: nccl-tests-2.16.0-table-v1\n", "", 1),
		"missing header":     strings.Replace(validNCCLRDMAOutput, "#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong\n", "", 1),
		"missing sentinel":   strings.Replace(validNCCLRDMAOutput, NCCLRDMAPassSentinel+"\n", "", 1),
		"duplicate sentinel": validNCCLRDMAOutput + NCCLRDMAPassSentinel + "\n",
		"missing ib":         strings.Replace(validNCCLRDMAOutput, "NET/IB : Using", "NET/IB : available", 1),
		"socket fallback":    validNCCLRDMAOutput + "NCCL INFO NET/Socket : Using [0]eth0\n",
		"verbs failure":      validNCCLRDMAOutput + "ibv_create_cq failed: Cannot allocate memory\n",
		"device failure":     validNCCLRDMAOutput + "NCCL WARN NET/IB : No device found\n",
		"nonzero bounds":     strings.Replace(validNCCLRDMAOutput, "Out of bounds values : 0 OK", "Out of bounds values : 1 FAILED", 1),
		"zero bandwidth": strings.NewReplacer(
			"67.11  125.83", "0.00   0.00",
			"68.20  127.87", "0.00   0.00",
		).Replace(validNCCLRDMAOutput),
		"malformed bandwidth":  strings.Replace(validNCCLRDMAOutput, "67.11", "not-a-number", 1),
		"non-finite bandwidth": strings.Replace(validNCCLRDMAOutput, "67.11", "Inf", 1),
		"extra result field":   strings.Replace(validNCCLRDMAOutput, "68.20  127.87      0\n", "68.20  127.87      0 unexpected\n", 1),
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNCCLRDMAOutput(output)
			require.Error(t, err)
		})
	}
}
