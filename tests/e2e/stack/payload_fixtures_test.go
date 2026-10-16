// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/internal/scriptpayload"
	"github.com/Azure/taugrid/tests/e2e/internal/taukeys"
)

const maxArgStrlen = 131072

const lastAppliedConfigSoftLimit = 200 * 1024

type payloadFixtureCase struct {
	fixture    string
	scriptFile string
	targetDir  string
	env        map[string]string
}

func payloadFixtureCases() []payloadFixtureCase {
	return []payloadFixtureCase{
		{
			fixture:    "inference-rayjob.yaml",
			scriptFile: "inference_job.py",
			targetDir:  "/script",
			env: map[string]string{
				"RAY_E2E_IMAGE": "example.azurecr.io/aks/ai-runtime/ray:test",
			},
		},
		{
			fixture:    "inference-rayjob-gpu.yaml",
			scriptFile: "inference_job.py",
			targetDir:  "/script",
			env: map[string]string{
				"RAY_E2E_IMAGE":           "example.azurecr.io/aks/ai-runtime/ray:test",
				"TORCH_SPEC":              "torch==2.4.1",
				"TORCH_INDEX_URL":         "https://download.pytorch.org/whl/cu121",
				"GPU_NODE_SELECTOR_KEY":   "agentpool",
				"GPU_NODE_SELECTOR_VALUE": "a10",
			},
		},
		{
			fixture:    "fineweb-rayjob-16xh200-ib.yaml",
			scriptFile: "fineweb_ray_train.py",
			targetDir:  "/script",
			env: map[string]string{
				"RAY_E2E_IMAGE":                "example.azurecr.io/aks/ai-runtime/ray:test",
				"GPU_NODE_SELECTOR_KEY":        "kubernetes.io/hostname",
				"GPU_NODE_SELECTOR_VALUE":      "flex-h200-node-0",
				"FINEWEB_DATASET_URIS":         "az://airundatasets/datasets/fineweb-edu-sample-10bt/v1",
				"FINEWEB_DATASET_SHA256S":      "0000000000000000000000000000000000000000000000000000000000000000",
				"FINEWEB_DATASET_TOKEN_COUNTS": "10000000",
			},
		},
		{
			fixture:    "training-rayjob.yaml",
			scriptFile: "training_job_cpu.py",
			targetDir:  "/home/ray/scripts",
			env: map[string]string{
				"RAY_E2E_IMAGE": "example.azurecr.io/aks/ai-runtime/ray:test",
			},
		},
		{
			fixture:    "training-rayjob-gpu.yaml",
			scriptFile: "training_job.py",
			targetDir:  "/home/ray/scripts",
			env: map[string]string{
				"RAY_E2E_IMAGE":           "example.azurecr.io/aks/ai-runtime/ray:test",
				"TORCH_SPEC":              "torch==2.4.1",
				"TORCH_INDEX_URL":         "https://download.pytorch.org/whl/cu121",
				"GPU_NODE_SELECTOR_KEY":   "agentpool",
				"GPU_NODE_SELECTOR_VALUE": "a10",
			},
		},
		{
			fixture:    "nanogpt-rayjob-large-gpu.yaml",
			scriptFile: "nanogpt_ray_train.py",
			targetDir:  "/home/ray/scripts",
			env: map[string]string{
				"RAY_E2E_IMAGE":                "example.azurecr.io/aks/ai-runtime/ray:test",
				"GPU_NODE_SELECTOR_KEY":        "kubernetes.io/hostname",
				"GPU_NODE_SELECTOR_VALUE":      "flex-h200-node-0",
				"NANOGPT_DATASET_URIS":         "az://airundatasets/datasets/fineweb-edu-sample-10bt/v1",
				"NANOGPT_DATASET_SHA256S":      "0000000000000000000000000000000000000000000000000000000000000000",
				"NANOGPT_DATASET_TOKEN_COUNTS": "10000000",
			},
		},
	}
}

func renderPayloadFixture(t *testing.T, tc payloadFixtureCase) []byte {
	t.Helper()
	for k, v := range tc.env {
		t.Setenv(k, v)
	}
	data, err := e2e.ReadFixtureWithSubstitutions(tc.fixture)
	require.NoErrorf(t, err, "rendering fixture %s", tc.fixture)
	return data
}

func TestFineWebSyntheticFixtureDoesNotRequireDatasetContract(t *testing.T) {
	t.Setenv("RAY_E2E_IMAGE", "example.azurecr.io/aks/ai-runtime/ray:test")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "kueue.azure.com/gpu-series")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nd-h200-v5")
	t.Setenv("FINEWEB_DATA_MODE", "synthetic")
	t.Setenv("FINEWEB_TAS_MODE", "disabled")
	t.Setenv("FINEWEB_DATASET_URIS", "")
	t.Setenv("FINEWEB_DATASET_SHA256S", "")
	t.Setenv("FINEWEB_DATASET_TOKEN_COUNTS", "")

	data, err := e2e.ReadFixtureWithSubstitutions("fineweb-rayjob-16xh200-ib.yaml")
	require.NoError(t, err)
	require.Contains(t, string(data), `FINEWEB_DATA_MODE: "synthetic"`)
	require.NotContains(t, string(data), "podset-required-topology")
	require.Contains(t, string(data), "hostname spread enforces the 8+8 worker placement")
}

func TestFineWebSiteTopologyFixture(t *testing.T) {
	t.Setenv("RAY_E2E_IMAGE", "example.azurecr.io/aks/ai-runtime/ray:test")
	t.Setenv("GPU_NODE_SELECTOR_KEY", "kueue.azure.com/gpu-series")
	t.Setenv("GPU_NODE_SELECTOR_VALUE", "nd-h200-v5")
	t.Setenv("FINEWEB_DATA_MODE", "synthetic")
	t.Setenv("FINEWEB_TAS_MODE", "site")

	data, err := e2e.ReadFixtureWithSubstitutions("fineweb-rayjob-16xh200-ib.yaml")
	require.NoError(t, err)
	require.Contains(t, string(data),
		"kueue.x-k8s.io/podset-required-topology: tau.azure.com/site")
}

func TestFineWebSchedulerRecoveryEligibility(t *testing.T) {
	valid := e2e.SchedulerRecoveryObservation{
		TotalWorkers:          16,
		RunningWorkers:        15,
		PendingWorkers:        1,
		RunningOnTarget:       7,
		RunningOnOtherNodes:   8,
		RequestedGPUsOnTarget: 7,
		PendingGPURequest:     1,
		TargetAllocatableGPUs: 8,
		HasInsufficientEvent:  true,
	}
	require.True(t, eligibleForFineWebSchedulerRecovery(16, valid))

	cases := map[string]func(*e2e.SchedulerRecoveryObservation){
		"all workers already running": func(state *e2e.SchedulerRecoveryObservation) { state.RunningWorkers = 16 },
		"extra target request":        func(state *e2e.SchedulerRecoveryObservation) { state.RequestedGPUsOnTarget = 8 },
		"wrong placement":             func(state *e2e.SchedulerRecoveryObservation) { state.RunningOnTarget = 6 },
		"missing scheduler evidence":  func(state *e2e.SchedulerRecoveryObservation) { state.HasInsufficientEvent = false },
		"multi-gpu pending pod":       func(state *e2e.SchedulerRecoveryObservation) { state.PendingGPURequest = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			state := valid
			mutate(&state)
			require.False(t, eligibleForFineWebSchedulerRecovery(16, state))
		})
	}
}

type rayJobDoc struct {
	Metadata struct {
		Annotations map[string]string `yaml:"annotations"`
		Labels      map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		SubmitterPodTemplate podTemplate `yaml:"submitterPodTemplate"`
		RayClusterSpec       struct {
			HeadGroupSpec struct {
				Template podTemplate `yaml:"template"`
			} `yaml:"headGroupSpec"`
			WorkerGroupSpecs []struct {
				Template podTemplate `yaml:"template"`
			} `yaml:"workerGroupSpecs"`
		} `yaml:"rayClusterSpec"`
	} `yaml:"spec"`
}

type podTemplate struct {
	Metadata struct {
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec podSpecDoc `yaml:"spec"`
}

type podSpecDoc struct {
	InitContainers []containerDoc `yaml:"initContainers"`
	Containers     []containerDoc `yaml:"containers"`
	Volumes        []volumeDoc    `yaml:"volumes"`
}

type containerDoc struct {
	Name         string           `yaml:"name"`
	Command      []string         `yaml:"command"`
	Env          []envVarDoc      `yaml:"env"`
	VolumeMounts []volumeMountDoc `yaml:"volumeMounts"`
}

type envVarDoc struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type volumeMountDoc struct {
	Name string `yaml:"name"`
}

type volumeDoc struct {
	Name      string                 `yaml:"name"`
	EmptyDir  map[string]interface{} `yaml:"emptyDir"`
	ConfigMap map[string]interface{} `yaml:"configMap"`
	HostPath  map[string]interface{} `yaml:"hostPath"`
}

func parseRayJobDoc(t *testing.T, data []byte) rayJobDoc {
	t.Helper()
	var doc rayJobDoc
	require.NoError(t, yaml.Unmarshal(data, &doc), "unmarshalling rendered RayJob YAML")
	return doc
}

func envValue(env []envVarDoc, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func containerByName(containers []containerDoc, name string) (containerDoc, bool) {
	for _, c := range containers {
		if c.Name == name {
			return c, true
		}
	}
	return containerDoc{}, false
}

func hasVolumeMount(mounts []volumeMountDoc, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

func TestNanoGPTRayJobFixtureOptsIntoTASOnlyForArgoCDQueue(t *testing.T) {
	const (
		fixture    = "nanogpt-rayjob-large-gpu.yaml"
		annotation = "kueue.x-k8s.io/podset-unconstrained-topology"
	)
	var fixtureCase payloadFixtureCase
	for _, tc := range payloadFixtureCases() {
		if tc.fixture == fixture {
			fixtureCase = tc
			break
		}
	}
	require.NotEmpty(t, fixtureCase.fixture, "missing payload fixture case for %s", fixture)

	for _, tc := range []struct {
		name      string
		useArgoCD string
		wantQueue string
		wantTAS   bool
	}{
		{name: "local queue", useArgoCD: "false", wantQueue: "e2e-stack-large-gpu-queue"},
		{name: "ArgoCD queue", useArgoCD: "true", wantQueue: "jobqueue", wantTAS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("E2E_STACK_USE_ARGOCD_QUEUE", tc.useArgoCD)
			t.Setenv("E2E_STACK_NAMESPACE", "")
			t.Setenv("E2E_STACK_QUEUE", "")
			t.Setenv("E2E_STACK_LARGE_GPU_QUEUE", "")

			doc := parseRayJobDoc(t, renderPayloadFixture(t, fixtureCase))
			require.Equal(t, tc.wantQueue, doc.Metadata.Labels["kueue.x-k8s.io/queue-name"])
			require.Len(t, doc.Spec.RayClusterSpec.WorkerGroupSpecs, 1)

			podSets := []struct {
				name     string
				template podTemplate
			}{
				{name: "submitter", template: doc.Spec.SubmitterPodTemplate},
				{name: "head", template: doc.Spec.RayClusterSpec.HeadGroupSpec.Template},
				{name: "workers", template: doc.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template},
			}
			for _, podSet := range podSets {
				got, present := podSet.template.Metadata.Annotations[annotation]
				if tc.wantTAS {
					require.Truef(t, present, "%s PodTemplate must opt into TAS", podSet.name)
					require.Equal(t, "true", got)
				} else {
					require.Falsef(t, present, "%s PodTemplate must omit TAS for the local queue", podSet.name)
				}
			}
		})
	}
}

func TestPayloadFixturesEmbedCorrectDigestAndContent(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available on PATH; skipping fixture payload execution test")
	}

	for _, tc := range payloadFixtureCases() {
		tc := tc
		t.Run(tc.fixture, func(t *testing.T) {
			data := renderPayloadFixture(t, tc)
			doc := parseRayJobDoc(t, data)

			annotationDigest := doc.Metadata.Annotations[scriptpayload.AnnotationDigest]
			require.NotEmpty(t, annotationDigest, "metadata.annotations[%s] must be set", scriptpayload.AnnotationDigest)

			headSpec := doc.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec
			payloadContainer, ok := containerByName(headSpec.InitContainers, scriptpayload.InitContainerName)
			require.True(t, ok, "head template must have an initContainer named %s", scriptpayload.InitContainerName)

			b64, ok := envValue(payloadContainer.Env, scriptpayload.EnvB64)
			require.True(t, ok, "%s env %s must be set", scriptpayload.InitContainerName, scriptpayload.EnvB64)
			require.NotEmpty(t, b64)

			digest, ok := envValue(payloadContainer.Env, scriptpayload.EnvDigest)
			require.True(t, ok, "%s env %s must be set", scriptpayload.InitContainerName, scriptpayload.EnvDigest)
			require.Equal(t, annotationDigest, digest, "initContainer digest env must match the metadata.annotations digest")

			targetDirEnv, ok := envValue(payloadContainer.Env, scriptpayload.EnvTargetDir)
			require.True(t, ok, "%s env %s must be set", scriptpayload.InitContainerName, scriptpayload.EnvTargetDir)
			require.Equal(t, tc.targetDir, targetDirEnv)

			require.Lenf(t, payloadContainer.Command, 3, "%s command must be exactly [python3, -c, <script>], got %v", scriptpayload.InitContainerName, payloadContainer.Command)
			require.Equal(t, "python3", payloadContainer.Command[0], "%s command[0] must invoke python3", scriptpayload.InitContainerName)
			require.Equal(t, "-c", payloadContainer.Command[1], "%s command[1] must be -c", scriptpayload.InitContainerName)
			script := payloadContainer.Command[2]
			require.NotEmpty(t, script, "%s's embedded script text must not be empty", scriptpayload.InitContainerName)

			targetDir := t.TempDir()
			cmd := exec.Command(python3, "-c", script)
			cmd.Env = append(os.Environ(),
				scriptpayload.EnvB64+"="+b64,
				scriptpayload.EnvDigest+"="+digest,
				scriptpayload.EnvTargetDir+"="+targetDir,
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			require.NoErrorf(t, cmd.Run(), "executing %s's embedded tau-payload command against a real python3 interpreter\nstderr:\n%s", tc.fixture, stderr.String())

			decodedPath := filepath.Join(targetDir, tc.scriptFile)
			decoded, err := os.ReadFile(decodedPath)
			require.NoErrorf(t, err, "expected %s's executed tau-payload command to write %s", tc.fixture, decodedPath)

			scriptPath := filepath.Join("fixtures", tc.scriptFile)
			want, err := os.ReadFile(scriptPath)
			require.NoError(t, err, "reading real script %s", scriptPath)
			require.Equal(t, string(want), string(decoded), "%s's embedded tau-payload command, executed for real, must write out %s byte-for-byte", tc.fixture, tc.scriptFile)

			require.LessOrEqual(t, len(decoded), scriptpayload.MaxDecodedBytes, "decoded script must stay at or under the 64 KiB payload cap")

			_, recomputed, err := scriptpayload.Encode(map[string][]byte{tc.scriptFile: decoded})
			require.NoError(t, err)
			require.Equal(t, digest, recomputed, "digest recomputed from the decoded bytes must match the embedded digest")
		})
	}
}

func TestPayloadFixturesAreHeadOnly(t *testing.T) {
	for _, tc := range payloadFixtureCases() {
		tc := tc
		t.Run(tc.fixture, func(t *testing.T) {
			data := renderPayloadFixture(t, tc)
			doc := parseRayJobDoc(t, data)

			headSpec := doc.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec
			_, hasPayloadInit := containerByName(headSpec.InitContainers, scriptpayload.InitContainerName)
			require.True(t, hasPayloadInit, "head template must have the %s initContainer", scriptpayload.InitContainerName)

			headHasScriptVolume := false
			for _, v := range headSpec.Volumes {
				if v.Name == "script" {
					headHasScriptVolume = true
					require.NotNil(t, v.EmptyDir, "head script volume must be an emptyDir")
					require.Nil(t, v.ConfigMap, "head script volume must not be a configMap")
				}
			}
			require.True(t, headHasScriptVolume, "head template must have a script emptyDir volume")

			require.NotEmpty(t, headSpec.Containers, "head template must have containers")
			headMain := headSpec.Containers[0]
			require.True(t, hasVolumeMount(headMain.VolumeMounts, "script"), "head main container must mount the script volume")

			require.NotEmpty(t, doc.Spec.RayClusterSpec.WorkerGroupSpecs, "fixture must have at least one worker group")
			for i, wg := range doc.Spec.RayClusterSpec.WorkerGroupSpecs {
				workerSpec := wg.Template.Spec

				_, workerHasPayloadInit := containerByName(workerSpec.InitContainers, scriptpayload.InitContainerName)
				require.Falsef(t, workerHasPayloadInit, "worker group %d must not have the %s initContainer", i, scriptpayload.InitContainerName)

				for _, v := range workerSpec.Volumes {
					require.NotEqualf(t, "script", v.Name, "worker group %d must not have a script volume", i)
				}

				for _, c := range workerSpec.Containers {
					require.Falsef(t, hasVolumeMount(c.VolumeMounts, "script"), "worker group %d container %s must not mount the script volume", i, c.Name)
					for _, envName := range []string{scriptpayload.EnvB64, scriptpayload.EnvDigest, scriptpayload.EnvTargetDir} {
						_, has := envValue(c.Env, envName)
						require.Falsef(t, has, "worker group %d container %s must not set %s", i, c.Name, envName)
					}
				}
				for _, ic := range workerSpec.InitContainers {
					for _, envName := range []string{scriptpayload.EnvB64, scriptpayload.EnvDigest, scriptpayload.EnvTargetDir} {
						_, has := envValue(ic.Env, envName)
						require.Falsef(t, has, "worker group %d initContainer %s must not set %s", i, ic.Name, envName)
					}
				}
			}
		})
	}
}

func renderedFixtureJSONSize(t *testing.T, data []byte) int {
	t.Helper()
	var generic map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &generic), "unmarshalling rendered fixture into a generic document")
	jsonBytes, err := json.Marshal(generic)
	require.NoError(t, err, "marshalling parsed fixture document to JSON")
	return len(jsonBytes)
}

func TestPayloadFixturesStayUnderSizeLimits(t *testing.T) {
	for _, tc := range payloadFixtureCases() {
		tc := tc
		t.Run(tc.fixture, func(t *testing.T) {
			data := renderPayloadFixture(t, tc)

			jsonSize := renderedFixtureJSONSize(t, data)
			require.LessOrEqualf(t, jsonSize, lastAppliedConfigSoftLimit, "rendered fixture %s (parsed+JSON-marshaled, %d bytes) must stay under the %d byte last-applied-configuration soft limit", tc.fixture, jsonSize, lastAppliedConfigSoftLimit)

			doc := parseRayJobDoc(t, data)
			headSpec := doc.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec
			payloadContainer, ok := containerByName(headSpec.InitContainers, scriptpayload.InitContainerName)
			require.True(t, ok)

			b64, ok := envValue(payloadContainer.Env, scriptpayload.EnvB64)
			require.True(t, ok)
			require.Lessf(t, len(b64), maxArgStrlen, "TAU_PAYLOAD_B64 for %s must stay under MAX_ARG_STRLEN (%d bytes)", tc.fixture, maxArgStrlen)

			t.Logf("%s: raw substituted YAML = %d bytes, parsed+JSON-marshaled = %d bytes, TAU_PAYLOAD_B64 = %d bytes", tc.fixture, len(data), jsonSize, len(b64))
		})
	}
}

func TestRenderedFixtureAtPayloadCapPassesJSONSizeGuard(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), scriptpayload.MaxDecodedBytes)
	encoded, digest, err := scriptpayload.Encode(map[string][]byte{"at_cap.py": payload})
	require.NoError(t, err)

	const placeholder = "{{AT_CAP_SCRIPT_PAYLOAD_B64}}"
	template := "" +
		"# Some doc comment mentioning " + placeholder + " for context.\n" +
		"apiVersion: ray.io/v1\n" +
		"kind: RayJob\n" +
		"metadata:\n" +
		"  annotations:\n" +
		"    " + taukeys.AnnotationPayloadDigest + ": \"" + digest + "\"\n" +
		"spec:\n" +
		"  rayClusterSpec:\n" +
		"    headGroupSpec:\n" +
		"      template:\n" +
		"        spec:\n" +
		"          initContainers:\n" +
		"          - name: " + scriptpayload.InitContainerName + "\n" +
		"            env:\n" +
		"            - name: " + scriptpayload.EnvB64 + "\n" +
		"              value: \"" + placeholder + "\"\n" +
		"            - name: " + scriptpayload.EnvDigest + "\n" +
		"              value: \"" + digest + "\"\n"

	rendered := []byte(strings.ReplaceAll(template, placeholder, encoded))

	require.Greaterf(t, len(rendered), lastAppliedConfigSoftLimit,
		"test setup sanity check: raw substituted bytes (%d) should be inflated past the %d byte soft limit by the simulated comment duplication -- otherwise this test isn't exercising the bug it guards against",
		len(rendered), lastAppliedConfigSoftLimit)

	jsonSize := renderedFixtureJSONSize(t, rendered)
	require.LessOrEqualf(t, jsonSize, lastAppliedConfigSoftLimit,
		"JSON-marshaled size of an exactly-at-cap (%d byte) payload must stay within the %d byte soft limit despite YAML comment duplication; got %d bytes (raw YAML was %d bytes)",
		scriptpayload.MaxDecodedBytes, lastAppliedConfigSoftLimit, jsonSize, len(rendered))
}

func TestDecodedPayloadCapIsInclusiveBoundary(t *testing.T) {
	atCap := bytes.Repeat([]byte("A"), scriptpayload.MaxDecodedBytes)
	_, _, err := scriptpayload.Encode(map[string][]byte{"at_cap.py": atCap})
	require.NoErrorf(t, err, "a payload of exactly %d bytes (the cap) must be accepted", scriptpayload.MaxDecodedBytes)
	require.LessOrEqual(t, len(atCap), scriptpayload.MaxDecodedBytes, "exactly-at-cap payload must satisfy the <= assertion used against real fixtures' decoded sizes")

	overCap := bytes.Repeat([]byte("A"), scriptpayload.MaxDecodedBytes+1)
	_, _, err = scriptpayload.Encode(map[string][]byte{"over_cap.py": overCap})
	require.Errorf(t, err, "a payload of %d bytes (cap+1) must be rejected", scriptpayload.MaxDecodedBytes+1)
}
