package artifactpublish

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWrapShellScriptDefinesClosedArtifactContract(t *testing.T) {
	script, err := WrapShellScript("printf model > \"$TAU_OUTPUT_STAGING_DIR/model.safetensors\"\n", Runtime{
		Mode:          ModeStaged,
		OutputDir:     "/data/research-workspace/run-1",
		StagingDir:    "/mnt/tau-output/run-1",
		PublicationID: "publication-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TAU_OUTPUT_STAGING_DIR",
		"refusing to overwrite non-matching durable artifact",
		`cp -a "$tau_publish_source" "$tau_publish_tmp"`,
		`mv -n "$tau_publish_tmp" "$tau_publish_destination"`,
		"sha256sum",
		CompletionMarker,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("publication wrapper missing %q:\n%s", want, script)
		}
	}
	if err := exec.Command("bash", "-n", "-c", script).Run(); err != nil {
		t.Fatalf("publication wrapper is not valid bash: %v\n%s", err, script)
	}
}

func TestStagedPublicationIsVerifiedAndRetrySafe(t *testing.T) {
	runtime := Runtime{
		Mode:          ModeStaged,
		OutputDir:     "/data/test-output",
		StagingDir:    "/mnt/test-staging",
		PublicationID: "publication-1",
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	staging := filepath.Join(root, "staging")
	render := func(content string) string {
		t.Helper()
		script, err := WrapShellScript(`printf '`+content+`' > "$TAU_OUTPUT_STAGING_DIR/model.safetensors"`+"\n", runtime)
		if err != nil {
			t.Fatal(err)
		}
		script = strings.ReplaceAll(script, runtime.OutputDir, output)
		script = strings.ReplaceAll(script, runtime.StagingDir, staging)
		return script
	}

	script := render("model-v1")
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("first publication failed: %v\n%s", err, out)
	}
	published := filepath.Join(output, GenerationsDir, "publication-1")
	if raw, err := os.ReadFile(filepath.Join(published, "model.safetensors")); err != nil || string(raw) != "model-v1" {
		t.Fatalf("published model = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(published, CompletionMarker)); err != nil || string(raw) != "complete publication-1\n" {
		t.Fatalf("completion marker = %q, %v", raw, err)
	}
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("matching retry failed: %v\n%s", err, out)
	}

	mismatch := render("model-v2")
	out, err := exec.Command("bash", "-c", mismatch).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 || !strings.Contains(string(out), "non-matching durable artifact") {
		t.Fatalf("mismatched retry = %v\n%s", err, out)
	}
}

func TestRuntimeRejectsUnsafePublicationPaths(t *testing.T) {
	tests := []Runtime{
		{Mode: ModeStaged, OutputDir: "/tmp/output", StagingDir: "/mnt/stage", PublicationID: "publication-1"},
		{Mode: ModeStaged, OutputDir: "/data/output", StagingDir: "/data/stage", PublicationID: "publication-1"},
		{Mode: "STAGED", OutputDir: "/data/output", StagingDir: "/mnt/stage", PublicationID: "publication-1"},
		{Mode: "direct", OutputDir: "/data/output", StagingDir: "/mnt/stage", PublicationID: "publication-1"},
		{Mode: ModeStaged, OutputDir: "/data/output", StagingDir: "/mnt/stage", PublicationID: "../../escape"},
		{Mode: ModeStaged, OutputDir: "/data/output", StagingDir: "/mnt/stage", PublicationID: `..\escape`},
	}

	for _, runtime := range tests {
		if err := runtime.Validate(); err == nil {
			t.Fatalf("Validate(%+v) unexpectedly succeeded", runtime)
		}
	}
}

func TestStagedPublicationRejectsSymlinks(t *testing.T) {
	runtime := Runtime{
		Mode:          ModeStaged,
		OutputDir:     "/data/test-output",
		StagingDir:    "/mnt/test-staging",
		PublicationID: "publication-1",
	}

	root := t.TempDir()
	output := filepath.Join(root, "output")
	staging := filepath.Join(root, "staging")
	script, err := WrapShellScript(`tau_link_target=/tmp/tau-artifact-target.$$
trap 'rm -f "$tau_link_target"' EXIT
printf target > "$tau_link_target"
ln -s "$tau_link_target" "$TAU_OUTPUT_STAGING_DIR/model.safetensors"
`, runtime)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, runtime.OutputDir, output)
	script = strings.ReplaceAll(script, runtime.StagingDir, staging)
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 || !strings.Contains(string(out), "not a regular file") {
		t.Fatalf("symlink publication = %v\n%s", err, out)
	}
}

func TestStagedPublicationRejectsReservedMarker(t *testing.T) {
	runtime := Runtime{
		Mode:          ModeStaged,
		OutputDir:     "/data/test-output",
		StagingDir:    "/mnt/test-staging",
		PublicationID: "publication-1",
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	staging := filepath.Join(root, "staging")
	script, err := WrapShellScript(`printf forged > "$TAU_OUTPUT_STAGING_DIR/.tau-artifacts-complete"`+"\n", runtime)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, runtime.OutputDir, output)
	script = strings.ReplaceAll(script, runtime.StagingDir, staging)
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 || !strings.Contains(string(out), "reserved by Tau") {
		t.Fatalf("reserved marker publication = %v\n%s", err, out)
	}
}

func TestStagedPublicationWaitsForTerminatedChild(t *testing.T) {
	runtime := Runtime{
		Mode:          ModeStaged,
		OutputDir:     "/data/test-output",
		StagingDir:    "/mnt/test-staging",
		PublicationID: "publication-1",
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	staging := filepath.Join(root, "staging")
	script, err := WrapShellScript(`trap 'sleep 0.1; printf flushed > "$TAU_OUTPUT_STAGING_DIR/flushed"; exit 0' TERM
printf started > "$TAU_OUTPUT_STAGING_DIR/started"
while :; do sleep 1; done
`, runtime)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, runtime.OutputDir, output)
	script = strings.ReplaceAll(script, runtime.StagingDir, staging)
	published := filepath.Join(output, GenerationsDir, "publication-1")
	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(published, CompletionMarker), []byte("complete stale-publication\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(staging, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(published, CompletionMarker)); !os.IsNotExist(err) {
		_ = cmd.Process.Kill()
		t.Fatalf("stale completion marker remained while publication was active: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper did not wait for graceful child shutdown: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(published, "flushed")); err != nil || string(raw) != "flushed" {
		t.Fatalf("graceful flush artifact = %q, %v", raw, err)
	}
}
