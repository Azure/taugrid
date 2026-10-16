// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/artifactindex"
)

func TestServeResolvesArtifactIndexWrittenByManagedWorkflow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executes a POSIX shell script")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	root := t.TempDir()
	hot := filepath.Join(root, "hot")
	durable := filepath.Join(root, "durable")
	const run = "artifact-demo"
	const artifact = "last.safetensors"

	if err := os.MkdirAll(hot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hot, artifact), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := artifactindex.Script(artifactindex.Config{
		Artifact:     artifact,
		Run:          run,
		ResourceName: "tau-" + run,
		Namespace:    "research",
	})
	if script == "" {
		t.Fatal("artifactindex.Script returned empty; the producer half of this test is not running")
	}

	cmd := exec.Command("sh", "-eu")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(),
		"TAU_CHECKPOINTS_DIR="+hot,
		"TAU_DURABLE_CHECKPOINTS_DIR="+durable,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("artifact index step failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(durable, "finetunes", run, "artifacts.json"))
	if err != nil {
		t.Fatalf("read artifacts.json: %v", err)
	}

	got, err := selectManagedWorkflowArtifact(raw, "")
	if err != nil {
		t.Fatalf("serve could not resolve the checkpoint it just trained: %v", err)
	}
	if got.Name != "checkpoint" {
		t.Errorf("Name = %q, want %q", got.Name, "checkpoint")
	}
	if got.ManifestPath != artifact {
		t.Errorf("ManifestPath = %q, want %q", got.ManifestPath, artifact)
	}
	wantDurable := filepath.Join(durable, "finetunes", run, "artifacts", artifact)
	if got.DurablePath != wantDurable {
		t.Errorf("DurablePath = %q, want %q", got.DurablePath, wantDurable)
	}
	if got.SizeBytes != int64(len("weights")) {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, len("weights"))
	}

	if _, err := selectManagedWorkflowArtifact(raw, "no-such-artifact"); err == nil {
		t.Error("expected an error for an unknown artifact name; the resolver is not discriminating")
	}
}
