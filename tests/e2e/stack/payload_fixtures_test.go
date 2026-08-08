// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Static regression coverage for issues #869/#871 PR3/PR3b: every
// manager-routed RayJob fixture (inference, inference-gpu, fineweb, training,
// training-gpu, nanogpt) embeds its driver script directly (head-only
// initContainer + emptyDir, mounted either at /script or -- for the PR3b
// fixtures -- at /home/ray/scripts, the path their ConfigMaps previously
// used) instead of mounting it from a ConfigMap, so MultiKueue's
// worker-cluster dispatch -- which does not replicate ConfigMaps -- has
// everything the head pod needs inside the RayJob spec it copies over.
// These tests exercise the fully-substituted
// fixture bytes (via e2e.ReadFixtureWithSubstitutions, the same path
// ApplyFixtureWithClient uses at apply time) without touching a live
// cluster: they decode the embedded payload with the test-only
// tests/e2e/internal/scriptpayload package (a documented, behaviorally
// verified mirror of PR1's cli/internal/payload wire format,
// see scriptpayload's package doc for the duplication boundary/rationale)
// and assert it round-trips to the real fixtures/*.py source files, that the
// payload only appears on the head pod template, and that the rendered
// fixture stays safely under Kubernetes' last-applied-configuration and
// MAX_ARG_STRLEN limits.
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

// maxArgStrlen is Linux's MAX_ARG_STRLEN: the maximum length, in bytes, of a
// single argv/envp string an exec* syscall will accept (checked per-string,
// not against the aggregate argv+envp size). The TAU_PAYLOAD_B64 env value
// set on the tau-payload initContainer must stay under this or the
// container never starts. See scriptpayload_test.go's
// TestEncodedEnvValueStaysUnderMaxArgStrlen for the boundary-cap version of
// this same guarantee.
const maxArgStrlen = 131072

// lastAppliedConfigSoftLimit mirrors
// rayjobrender.TestRenderNearCapPayloadStaysUnderLastAppliedConfigLimit's 200
// KiB threshold: kubectl/controller-runtime apply stores the full applied
// object in the kubectl.kubernetes.io/last-applied-configuration annotation,
// and etcd enforces a ~1.5 MiB per-object hard limit split across all
// annotations. 200 KiB is a conservative regression guard, not the actual
// wire-contract cap (that's scriptpayload.MaxDecodedBytes, enforced -- and
// boundary-tested -- independently in scriptpayload_test.go).
const lastAppliedConfigSoftLimit = 200 * 1024

// payloadFixtureCase describes one of the six manager-routed RayJob fixtures
// under test: its file name, the real driver script it embeds, the target
// directory its tau-payload initContainer writes to (and its script volume
// is mounted at), and the env vars readFixture/ReadFixtureWithSubstitutions
// requires to fully render it (RAY_IMAGE and friends have no default and
// error out if unset).
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

// renderPayloadFixture sets the required env vars (t.Setenv auto-restores
// them after the test) and returns the fully-substituted fixture bytes.
func renderPayloadFixture(t *testing.T, tc payloadFixtureCase) []byte {
	t.Helper()
	for k, v := range tc.env {
		t.Setenv(k, v)
	}
	data, err := e2e.ReadFixtureWithSubstitutions(tc.fixture)
	require.NoErrorf(t, err, "rendering fixture %s", tc.fixture)
	return data
}

// rayJobDoc is a minimal typed view over the fields these tests need from a
// rendered RayJob manifest; it deliberately does not model the full
// ray.io/v1 RayJob schema.
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

// TestPayloadFixturesEmbedCorrectDigestAndContent extracts each fixture's
// ACTUAL tau-payload initContainer command -- the literal
// `python3 -c <script>` text that ships in the YAML, not a canonical Go
// constant -- and executes it against a real python3 interpreter with the
// fixture's own TAU_PAYLOAD_B64/TAU_PAYLOAD_DIGEST env values and a temp
// target dir. This proves the exact bytes shipped in each fixture are
// behaviorally correct: a copy-paste error, indentation mistake, or
// manual-transcription typo in the embedded YAML Python text would fail
// here (either the process errors out, or the written file doesn't match
// the real source), whereas calling scriptpayload.Decode directly only
// proves the Go package round-trips with itself and would not catch a bug
// in the YAML-embedded copy. It skips if python3 is not on PATH.
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

			// Pull the literal command the fixture actually ships, not a
			// constant from the scriptpayload package.
			require.Lenf(t, payloadContainer.Command, 3, "%s command must be exactly [python3, -c, <script>], got %v", scriptpayload.InitContainerName, payloadContainer.Command)
			require.Equal(t, "python3", payloadContainer.Command[0], "%s command[0] must invoke python3", scriptpayload.InitContainerName)
			require.Equal(t, "-c", payloadContainer.Command[1], "%s command[1] must be -c", scriptpayload.InitContainerName)
			script := payloadContainer.Command[2]
			require.NotEmpty(t, script, "%s's embedded script text must not be empty", scriptpayload.InitContainerName)

			// Execute the fixture's real script text for real, against a
			// real python3 interpreter, writing to a real temp directory --
			// this is the actual runtime behavior of the fixture, not a
			// stand-in for it.
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

			// The wire contract accepts a decoded payload up to and
			// including MaxDecodedBytes (scriptpayload.Encode only rejects
			// size > MaxDecodedBytes); exactly 64 KiB is valid, so this must
			// be <=, not <.
			require.LessOrEqual(t, len(decoded), scriptpayload.MaxDecodedBytes, "decoded script must stay at or under the 64 KiB payload cap")

			// Recompute the digest independently from the decoded bytes
			// (via the Go-side Encode, not the executed script's own
			// internal check) so the digest match isn't solely attested by
			// the same script that just ran: a fresh Encode of the same
			// single file must reproduce the exact same digest the fixture
			// embeds.
			_, recomputed, err := scriptpayload.Encode(map[string][]byte{tc.scriptFile: decoded})
			require.NoError(t, err)
			require.Equal(t, digest, recomputed, "digest recomputed from the decoded bytes must match the embedded digest")
		})
	}
}

// TestPayloadFixturesAreHeadOnly asserts the tau-payload initContainer, its
// env vars, and the script volume/mount appear ONLY on the head pod
// template -- never on any worker group's pod template. MultiKueue's
// worker-cluster dispatch does not replicate ConfigMaps, but it does copy
// the full RayJob spec (including the head template), so the payload must
// live there; a worker carrying it too would be redundant and would also
// mean the head-only invariant this fixture conversion depends on has
// silently broken.
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

// renderedFixtureJSONSize parses the fully-substituted fixture YAML into a
// generic document and re-marshals it as JSON, returning the JSON byte
// length. This mirrors what kubectl/controller-runtime actually transmit to
// the API server (and what lands in the
// kubectl.kubernetes.io/last-applied-configuration annotation): YAML
// comments are dropped at parse time and never reach the wire. Measuring raw
// substituted YAML bytes instead would incorrectly count documentation text
// as if it were part of the applied object -- notably, a header comment
// that happens to mention a placeholder token gets that token replaced too
// (readFixture's substitution is a blind whole-file text replace), which can
// duplicate a large base64 payload into the raw bytes without it ever
// reaching the actual Kubernetes object.
func renderedFixtureJSONSize(t *testing.T, data []byte) int {
	t.Helper()
	var generic map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &generic), "unmarshalling rendered fixture into a generic document")
	jsonBytes, err := json.Marshal(generic)
	require.NoError(t, err, "marshalling parsed fixture document to JSON")
	return len(jsonBytes)
}

// TestPayloadFixturesStayUnderSizeLimits guards the two hard runtime limits
// the payload conversion is designed around: the TAU_PAYLOAD_B64 env value
// must stay under Linux's MAX_ARG_STRLEN (or the initContainer can't even
// exec), and the fully rendered fixture -- measured as the parsed-and-JSON-
// marshaled object, i.e. what actually gets applied, not raw YAML-with-
// comments bytes -- must stay safely under the last-applied-configuration
// soft limit used for kubectl/controller-runtime apply. These are
// regression guards against the *real* scripts these fixtures embed today
// (a few KB each); the wire-format's actual 64 KiB cap-enforcement is
// boundary-tested independently in scriptpayload_test.go, and
// TestRenderedFixtureAtPayloadCapPassesJSONSizeGuard below proves the
// JSON-based measurement itself correctly accepts an exactly-at-cap
// payload.
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

// TestRenderedFixtureAtPayloadCapPassesJSONSizeGuard is a boundary test for
// the size-measurement method itself (not a real fixture file): it builds a
// minimal RayJob-shaped YAML document embedding an exactly-64-KiB
// (scriptpayload.MaxDecodedBytes) contract-valid payload -- the same size
// scriptpayload_test.go's TestEncodeAcceptsPayloadExactlyAtCap proves the
// encoder accepts -- and deliberately reproduces the historical duplication
// bug (a header comment literally mentioning the placeholder token, which
// readFixture's blind text-replace would substitute into the comment too,
// just as the real fixtures' header comments do for their *_DIGEST
// placeholders). It proves that:
//  1. the raw substituted YAML bytes are inflated well past the 200 KiB
//     soft limit purely by the comment duplication artifact (a sanity check
//     that this test is actually exercising the bug), yet
//  2. the parsed-and-JSON-marshaled size -- what
//     TestPayloadFixturesStayUnderSizeLimits actually asserts against --
//     correctly counts the payload once and stays within the soft limit.
//
// This demonstrates a contract-valid, exactly-at-cap payload is accepted by
// the fixture size guard despite the comment-duplication artifact, which a
// naive raw-YAML-byte-length check would have wrongly rejected.
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

	// Mirror readFixture's actual substitution mechanism: a blind
	// whole-file string replace, which hits the comment mention as well as
	// the real env value.
	rendered := []byte(strings.ReplaceAll(template, placeholder, encoded))

	require.Greaterf(t, len(rendered), lastAppliedConfigSoftLimit,
		"test setup sanity check: raw substituted bytes (%d) should be inflated past the %d byte soft limit by the simulated comment duplication -- otherwise this test isn't exercising the bug it guards against",
		len(rendered), lastAppliedConfigSoftLimit)

	jsonSize := renderedFixtureJSONSize(t, rendered)
	require.LessOrEqualf(t, jsonSize, lastAppliedConfigSoftLimit,
		"JSON-marshaled size of an exactly-at-cap (%d byte) payload must stay within the %d byte soft limit despite YAML comment duplication; got %d bytes (raw YAML was %d bytes)",
		scriptpayload.MaxDecodedBytes, lastAppliedConfigSoftLimit, jsonSize, len(rendered))
}

// TestDecodedPayloadCapIsInclusiveBoundary makes the wire contract's cap
// semantics explicit at the point where TestPayloadFixturesEmbedCorrectDigestAndContent
// asserts against them: the decoded payload cap is inclusive, i.e. exactly
// scriptpayload.MaxDecodedBytes (65536) is a *valid* payload, and only a
// payload strictly greater than the cap is rejected. This guards against the
// off-by-one this test file previously had (require.Less instead of
// require.LessOrEqual on the decoded-size assertion), which would have
// wrongly failed a legitimate, contract-valid, exactly-at-cap fixture.
func TestDecodedPayloadCapIsInclusiveBoundary(t *testing.T) {
	atCap := bytes.Repeat([]byte("A"), scriptpayload.MaxDecodedBytes)
	_, _, err := scriptpayload.Encode(map[string][]byte{"at_cap.py": atCap})
	require.NoErrorf(t, err, "a payload of exactly %d bytes (the cap) must be accepted", scriptpayload.MaxDecodedBytes)
	require.LessOrEqual(t, len(atCap), scriptpayload.MaxDecodedBytes, "exactly-at-cap payload must satisfy the <= assertion used against real fixtures' decoded sizes")

	overCap := bytes.Repeat([]byte("A"), scriptpayload.MaxDecodedBytes+1)
	_, _, err = scriptpayload.Encode(map[string][]byte{"over_cap.py": overCap})
	require.Errorf(t, err, "a payload of %d bytes (cap+1) must be rejected", scriptpayload.MaxDecodedBytes+1)
}
