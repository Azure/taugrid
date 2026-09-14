// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func eightRankServeSnapshot(t *testing.T) []byte {
	t.Helper()
	p := serveTestProfile("h100-nvl-8node", profile.ExecutionTargetSingleCluster, "jobqueue", 1, 8, 23)
	p.Description = "Synthetic offline rendering fixture, not a live cluster export."
	p.Applicability = profile.ProfileApplicability{
		Namespaces: []string{"taugrid-default"}, Teams: []string{"research"}, Lanes: []string{"serving"},
	}
	p.LocalQueues = []profile.ResolvedLocalQueue{{
		Namespace: "taugrid-default", Name: "jobqueue", ClusterQueue: "example-gpu-cq",
	}}
	p.ClusterQueues = []string{"example-gpu-cq"}
	snapshot, err := profile.NewProfileSetSnapshot(23, []profile.ResolvedWorkloadProfile{p})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeServeSnapshot(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func preventServeClusterAccess(t *testing.T) {
	t.Helper()
	oldClient, oldRunner, oldConnection := newClusterProfileClient, newServeRunner, newServeConnectionEnsurer
	newClusterProfileClient = func(string) (dynamic.Interface, error) {
		t.Fatal("offline validation contacted Kubernetes")
		return nil, nil
	}
	newServeRunner = func(string) kubeRawRunner {
		t.Fatal("offline validation constructed a kubectl runner")
		return nil
	}
	newServeConnectionEnsurer = func(*cobra.Command) runConnectionEnsurer {
		t.Fatal("offline validation tried to establish a workspace connection")
		return nil
	}
	t.Cleanup(func() {
		newClusterProfileClient, newServeRunner, newServeConnectionEnsurer = oldClient, oldRunner, oldConnection
	})
}

func executeServeCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"serve"}, args...))
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

func TestServeRayOfflineSnapshot(t *testing.T) {
	preventServeClusterAccess(t)
	snapshot := writeServeSnapshot(t, eightRankServeSnapshot(t))
	out, stderr, err := executeServeCommand(t,
		"deploy", "distributed-model", "--kind=rayservice",
		"--profile=h100-nvl-8node", "--namespace=taugrid-default",
		"--workload-profile-snapshot="+snapshot, "--dry-run=client",
		"--image=example.invalid/ray:fixture", "--gpus=1", "--nodes=8",
		"--ray-version=2.58.0", "--import-path=model_app:app", "--replicas=1",
		"--port=8000", "--shm-size=32Gi",
		`--env=MODEL_CONFIG={"context_length":1048576}`,
		"--volume=models=pvc:model-weights", "--mount=models:/models:ro",
	)
	if err != nil {
		t.Fatalf("offline render: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "offline client dry-run") {
		t.Fatalf("offline output must not imply live validation: %s", stderr)
	}
	for _, want := range []string{
		"kind: RayService", "workerGroupSpecs:", "replicas: 8", "num_replicas: 1",
		"nvidia.com/gpu: 1", "medium: Memory", "sizeLimit: 32Gi", "1048576",
		"import_path: model_app:app", "num-gpus: \"0\"",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"LeaderWorkerSet", "LWS_", "sglang.launch_server"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("non-Ray launch path remains: %q", unwanted)
		}
	}
	var object struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(out), &object); err != nil {
		t.Fatal(err)
	}
	if object.Metadata.Annotations[serveProfileSourceAnnotation] != "snapshot" ||
		object.Metadata.Labels[workloadmeta.LabelWorkspace] != "" {
		t.Fatal("snapshot output must identify its source, not claim a verified workspace")
	}
}

func TestServeSnapshotCannotAuthorizeLiveOperations(t *testing.T) {
	preventServeClusterAccess(t)
	for _, dryRun := range []string{"", "server"} {
		t.Run("dry-run="+dryRun, func(t *testing.T) {
			args := []string{
				"deploy", "model", "--kind=rayservice",
				"--profile=h100-nvl-8node", "--namespace=taugrid-default",
				"--workload-profile-snapshot=/must-not-read-this-file",
			}
			if dryRun != "" {
				args = append(args, "--dry-run="+dryRun)
			}
			out, _, err := executeServeCommand(t, args...)
			if err == nil || !strings.Contains(err.Error(), "requires --dry-run=client") || out != "" {
				t.Fatalf("snapshot entered a live path: out=%q err=%v", out, err)
			}
		})
	}
}

func TestServeSnapshotFailsClosed(t *testing.T) {
	preventServeClusterAccess(t)
	data := eightRankServeSnapshot(t)
	path := writeServeSnapshot(t, data)
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"namespace required", nil, "--namespace is required"},
		{"context conflict", []string{"--namespace=taugrid-default", "--context=ai"}, "--context cannot"},
		{"scope mismatch", []string{"--namespace=other-team"}, "namespace"},
		{"GPU conflict", []string{"--namespace=taugrid-default", "--gpus=8"}, "--gpus=8 conflicts"},
		{"node conflict", []string{"--namespace=taugrid-default", "--nodes=4"}, "--nodes=4 conflicts"},
		{"deployment cardinality", []string{"--namespace=taugrid-default", "--kind=deployment"}, "exactly one profile worker"},
		{"unavailable profile", []string{"--namespace=taugrid-default", "--profile=missing"}, "unavailable"},
		{"startup args", []string{"--namespace=taugrid-default", "--args=--use-ray"}, "legacy --args"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"deploy", "model", "--kind=rayservice",
				"--profile=h100-nvl-8node", "--workload-profile-snapshot=" + path,
				"--image=example.invalid/model:fixture", "--dry-run=client",
			}
			out, _, err := executeServeCommand(t, append(args, test.flags...)...)
			if err == nil || !strings.Contains(err.Error(), test.want) || out != "" {
				t.Fatalf("out=%q err=%v, want %q", out, err, test.want)
			}
		})
	}
	corrupt := bytes.Replace(data, []byte("workerCount: 8"), []byte("workerCount: 9"), 1)
	out, _, err := executeServeCommand(t,
		"deploy", "model", "--profile=h100-nvl-8node", "--namespace=taugrid-default",
		"--workload-profile-snapshot="+writeServeSnapshot(t, corrupt), "--dry-run=client",
	)
	if err == nil || !strings.Contains(err.Error(), "profileSetHash mismatch") || out != "" {
		t.Fatalf("tampered snapshot was accepted: out=%q err=%v", out, err)
	}
}

func TestServeRayConnectedMultiWorkerProfile(t *testing.T) {
	p := serveTestProfile("h100-eight", profile.ExecutionTargetSingleCluster, "jobqueue", 1, 8, 23)
	out, err := executeAuthoritativeServe(t, p,
		"distributed", "--kind=rayservice", "--profile=h100-eight",
		"--nodes=8", "--replicas=1", "--gpus=1",
		"--image=example.invalid/model:fixture", "--dry-run=client", "-n", "alpha",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workerGroupSpecs:", "replicas: 8", "num_replicas: 1", "tau.azure.com/workspace: sample"} {
		if !strings.Contains(out, want) {
			t.Fatalf("connected output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, serveProfileSourceAnnotation) {
		t.Fatal("a connected render was mislabeled as a snapshot")
	}
}

func TestServeRayRejectsNonRayFlagsBeforeConnecting(t *testing.T) {
	preventServeClusterAccess(t)
	for _, flags := range [][]string{
		{"--kind=leaderworkerset"},
		{"--kind=rayservice", "--command=python3"},
		{"--kind=rayservice", "--arg=--use-ray"},
		{"--kind=deployment", "--nodes=8"},
		{"--kind=deployment", "--shm-size=32Gi"},
	} {
		args := []string{"deploy", "model", "--profile=profile", "--dry-run=client"}
		if out, _, err := executeServeCommand(t, append(args, flags...)...); err == nil || out != "" {
			t.Fatalf("unsupported flags must fail before connecting: %v, err=%v", flags, err)
		}
	}
}

func TestServeEightRankSnapshotFixture(t *testing.T) {
	expected := eightRankServeSnapshot(t)
	fixture, err := os.ReadFile(filepath.Join("testdata", "serve-h100-eight.snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lfFixture := bytes.ReplaceAll(fixture, []byte("\r\n"), []byte("\n"))
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"checkout", fixture},
		{"LF", lfFixture},
		{"CRLF", bytes.ReplaceAll(lfFixture, []byte("\n"), []byte("\r\n"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalized := bytes.ReplaceAll(test.data, []byte("\r\n"), []byte("\n"))
			if !bytes.Equal(normalized, expected) {
				t.Fatalf("fixture differs from canonical snapshot; expected:\n%s", expected)
			}
			if _, err := profile.DecodeSnapshotProvider(test.data); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeSnapshotProvenanceOnDeploymentChildren(t *testing.T) {
	preventServeClusterAccess(t)
	snapshot, err := profile.DecodeProfileSetSnapshot(eightRankServeSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	p := snapshot.Profiles[0]
	p.WorkerCount, p.Placement = 1, profile.PlacementIndependent
	snapshot, err = profile.NewProfileSetSnapshot(snapshot.TauClusterGeneration, []profile.ResolvedWorkloadProfile{p})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeServeCommand(t,
		"deploy", "model", "--kind=deployment", "--profile=h100-nvl-8node",
		"--namespace=taugrid-default", "--workload-profile-snapshot="+writeServeSnapshot(t, data),
		"--dry-run=client", "--image=example.invalid/model:fixture",
		"--service-port=30000", "--max-replicas=2",
	)
	if err != nil {
		t.Fatal(err)
	}
	documents := strings.Split(out, "\n---\n")
	if len(documents) != 3 {
		t.Fatalf("expected Deployment, Service and HPA:\n%s", out)
	}
	for _, raw := range documents {
		var object struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		if object.Metadata.Annotations[serveProfileSourceAnnotation] != "snapshot" {
			t.Fatalf("child object lost snapshot provenance:\n%s", raw)
		}
	}
}
