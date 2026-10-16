// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactindex

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func requireUnixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test executes a POSIX shell script")
	}
}

func readerStructTags(t *testing.T, structName string) []string {
	t.Helper()
	const src = "../cli/pvc_helpers.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var got []string
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != structName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
			name := strings.Split(tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				got = append(got, name)
			}
		}
		return false
	})
	if len(got) == 0 {
		t.Fatalf("no json tags found for %s in %s — the guard is not reading the struct it thinks it is", structName, src)
	}
	sort.Strings(got)
	return got
}

func TestScriptEmptyWhenNothingDeclared(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Artifact: "last.safetensors"},
		{Run: "demo"},
		{Artifact: "   ", Run: "demo"},
	} {
		if got := Script(cfg); got != "" {
			t.Errorf("Script(%+v) = %q, want empty so existing renders are byte-identical", cfg, got)
		}
		if got := IndentedScript(cfg, 4); got != "" {
			t.Errorf("IndentedScript(%+v) = %q, want empty", cfg, got)
		}
	}
}

func TestIndentedScriptIndentsEveryLine(t *testing.T) {
	cfg := Config{Artifact: "last.safetensors", Run: "demo"}
	out := IndentedScript(cfg, 4)
	if out == "" {
		t.Fatal("IndentedScript returned empty for a fully-specified config")
	}
	for i, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("line %d not indented: %q", i, line)
		}
	}
}

func TestShellQuoteHandlesEmbeddedQuote(t *testing.T) {
	if got, want := shellQuote(`a'b`), `'a'"'"'b'`; got != want {
		t.Fatalf("shellQuote = %s, want %s", got, want)
	}
}

func TestScriptWritesIndexMatchingReaderSchema(t *testing.T) {
	requireUnixShell(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	root := t.TempDir()
	hot := filepath.Join(root, "hot")
	durable := filepath.Join(root, "durable")
	const run = "demo-run"
	const artifact = "last.safetensors"

	if err := os.MkdirAll(hot, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("weights")
	if err := os.WriteFile(filepath.Join(hot, artifact), want, 0o644); err != nil {
		t.Fatal(err)
	}

	script := Script(Config{
		Artifact:     artifact,
		Run:          run,
		ResourceName: "demo-run-rayjob",
		Namespace:    "research",
	})
	if script == "" {
		t.Fatal("Script returned empty for a fully-specified config")
	}

	cmd := exec.Command("sh", "-eu")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(),
		"TAU_CHECKPOINTS_DIR="+hot,
		"TAU_DURABLE_CHECKPOINTS_DIR="+durable,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}

	indexPath := filepath.Join(durable, "finetunes", run, "artifacts.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("artifacts.json not written at %s: %v\nscript output:\n%s", indexPath, err, out)
	}

	var index map[string]any
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("emitted invalid JSON: %v\n%s", err, raw)
	}

	assertSubset(t, index, readerStructTags(t, "managedWorkflowArtifactIndex"), "index")

	artifacts, ok := index["artifacts"].([]any)
	if !ok || len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v, want exactly one record", index["artifacts"])
	}
	rec, ok := artifacts[0].(map[string]any)
	if !ok {
		t.Fatalf("artifact record is %T, want object", artifacts[0])
	}
	assertSubset(t, rec, readerStructTags(t, "managedWorkflowArtifact"), "artifact")

	if rec["name"] != "checkpoint" {
		t.Errorf(`name = %v, want "checkpoint" (serve.go defaults artifactName to "checkpoint")`, rec["name"])
	}
	if rec["status"] != "ready" {
		t.Errorf(`status = %v, want "ready" (serve.go rejects anything else)`, rec["status"])
	}
	durablePath, _ := rec["durable_path"].(string)
	if durablePath == "" {
		t.Fatal("durable_path empty; serve.go rejects the record")
	}
	got, err := os.ReadFile(durablePath)
	if err != nil {
		t.Fatalf("durable_path %s does not exist: %v", durablePath, err)
	}
	if string(got) != string(want) {
		t.Errorf("durable copy = %q, want %q", got, want)
	}
}

func TestScriptSucceedsWhenArtifactMissing(t *testing.T) {
	requireUnixShell(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	root := t.TempDir()
	cmd := exec.Command("sh", "-eu")
	cmd.Stdin = strings.NewReader(Script(Config{Artifact: "never-written.bin", Run: "demo"}))
	cmd.Env = append(os.Environ(),
		"TAU_CHECKPOINTS_DIR="+filepath.Join(root, "hot"),
		"TAU_DURABLE_CHECKPOINTS_DIR="+filepath.Join(root, "durable"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script must exit 0 when the artifact is absent, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not found after training") {
		t.Errorf("expected a diagnostic naming the paths tried, got:\n%s", out)
	}
}

func noPythonEnv(hot, durable string) []string {
	return []string{
		"PATH=/nonexistent",
		"TAU_CHECKPOINTS_DIR=" + hot,
		"TAU_DURABLE_CHECKPOINTS_DIR=" + durable,
	}
}

func TestScriptFailsWhenInterpreterMissing(t *testing.T) {
	requireUnixShell(t)
	root := t.TempDir()
	cmd := exec.Command("sh", "-eu")
	cmd.Stdin = strings.NewReader(Script(Config{Artifact: "last.safetensors", Run: "demo"}))
	cmd.Env = noPythonEnv(filepath.Join(root, "hot"), filepath.Join(root, "durable"))

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("script exited %v with no python3; a run that cannot write its\n"+
			"artifact index must not report success. output:\n%s", err, out)
	}
	if got := exitErr.ExitCode(); got != 126 {
		t.Errorf("exit code = %d, want 126\n%s", got, out)
	}
	for _, want := range []string{
		"this image has no python3",
		"Use an image that ships python3",
		"last.safetensors",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("diagnostic does not mention %q; got:\n%s", want, out)
		}
	}
}

func TestScriptFailsWhenDestinationRefused(t *testing.T) {
	requireUnixShell(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	root := t.TempDir()
	cmd := exec.Command("sh", "-eu")
	cmd.Stdin = strings.NewReader(Script(Config{Artifact: "../escape.bin", Run: "demo"}))
	cmd.Env = append(os.Environ(),
		"TAU_CHECKPOINTS_DIR="+filepath.Join(root, "hot"),
		"TAU_DURABLE_CHECKPOINTS_DIR="+filepath.Join(root, "durable"),
	)

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("refusing an unsafe path exited %v; a refusal must not report success\n%s", err, out)
	}
	if got := exitErr.ExitCode(); got != 126 {
		t.Errorf("exit code = %d, want 126\n%s", got, out)
	}
	if !strings.Contains(string(out), "refusing unsafe checkpoint artifact path") {
		t.Errorf("expected the refusal diagnostic, got:\n%s", out)
	}
}

func assertSubset(t *testing.T, got map[string]any, allowed []string, label string) {
	t.Helper()
	set := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		set[k] = true
	}
	for k := range got {
		if !set[k] {
			t.Errorf("%s emits key %q which %v does not accept — schema drift", label, k, allowed)
		}
	}
}

func TestScriptDoesNotUseMetadataPreservingCopy(t *testing.T) {
	s := Script(Config{Artifact: "last.pt", Run: "r", ResourceName: "r", Namespace: "ns"})
	if s == "" {
		t.Fatal("empty script; guard would be vacuous")
	}
	for _, banned := range []string{"copy2", "copystat", "copymode"} {
		if strings.Contains(s, banned) {
			t.Errorf("script uses shutil.%s; Azure Files rejects metadata copy with EPERM", banned)
		}
	}
	if !strings.Contains(s, "copyfile") {
		t.Error("expected shutil.copyfile as the data-only copy primitive")
	}
}
