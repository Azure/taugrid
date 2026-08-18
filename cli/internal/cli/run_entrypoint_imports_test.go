// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one-file packaging contract used to be enforced only at runtime: an
// ordinary sibling import passed validation, passed Kueue admission, brought up
// the RayCluster, and then died with ModuleNotFoundError inside a worker. These
// tests pin the failure to submit time instead.

func TestValidateRunDispatchOptionsRejectsUnshippedSiblingImport(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("import ray\nfrom pipeline import run\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.py"), []byte("def run():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	err := validateRunDispatchOptions(runDispatchOptions{runPayloadInput: runPayloadInput{script: script}})
	if err == nil {
		t.Fatal("want submit-time failure for an unshipped sibling import")
	}
	for _, want := range []string{"pipeline", "pipeline.py", "ModuleNotFoundError"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestValidateRunDispatchOptionsAcceptsSelfContainedScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("import ray\nimport torch\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := validateRunDispatchOptions(runDispatchOptions{runPayloadInput: runPayloadInput{script: script}}); err != nil {
		t.Fatalf("self-contained script must validate: %v", err)
	}
}

func TestValidateRunDispatchOptionsAcceptsDeclaredExtraScripts(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("from pipeline import run\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.py"), []byte(""), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	o := runDispatchOptions{runPayloadInput: runPayloadInput{script: script, extraScripts: []string{filepath.Join(dir, "pipeline.py")}}}
	if err := validateRunDispatchOptions(o); err != nil {
		t.Fatalf("a sibling listed in extra_scripts is shipped and must validate: %v", err)
	}
}

func TestValidateRunDispatchOptionsIgnoresMissingEntrypoint(t *testing.T) {
	// The dispatch path reports a missing entrypoint with better context; the
	// import gate must not pre-empt it with a worse message.
	o := runDispatchOptions{runPayloadInput: runPayloadInput{script: filepath.Join(t.TempDir(), "absent.py")}}
	if err := validateRunDispatchOptions(o); err != nil {
		t.Fatalf("want no error from the import gate, got %v", err)
	}
}

// A managed workflow runs main_script, not run.entrypoint, and a config that
// sets only main_script leaves script empty. The gate used to return early on
// that, so exactly the configs that go through managed execution kept the
// late ModuleNotFoundError this check exists to prevent.
func TestValidateRunDispatchOptionsChecksManagedWorkflowMainScript(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "train.py")
	if err := os.WriteFile(main, []byte("from pipeline import run\n"), 0o644); err != nil {
		t.Fatalf("write main script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.py"), []byte("def run():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	err := validateRunDispatchOptions(runDispatchOptions{
		runDispatchInput: runDispatchInput{file: filepath.Join(dir, "workflow.yaml")},
		runPayloadInput:  runPayloadInput{mainScript: main},
	})
	if err == nil {
		t.Fatal("want submit-time failure for an unshipped import in a managed workflow main script")
	}
	if !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("error must name the missing module: %v", err)
	}
}

// The direct engines execute run.entrypoint even when a config also carries a
// main_script, so the gate must check the entrypoint there rather than
// validating a file that never runs.
func TestValidateRunDispatchOptionsChecksEntrypointWhenNotAManagedWorkflow(t *testing.T) {
	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "train.py")
	if err := os.WriteFile(entrypoint, []byte("from pipeline import run\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.py"), []byte("def run():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	// Self-contained, and never executed on this path.
	unused := filepath.Join(dir, "unused_main.py")
	if err := os.WriteFile(unused, []byte("import torch\n"), 0o644); err != nil {
		t.Fatalf("write main script: %v", err)
	}

	err := validateRunDispatchOptions(runDispatchOptions{runPayloadInput: runPayloadInput{script: entrypoint, mainScript: unused}})
	if err == nil {
		t.Fatal("want the entrypoint checked when no managed workflow file is set")
	}
	if !strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("error must name the entrypoint's missing module: %v", err)
	}
}

// Extra scripts are SRC[:DEST] specs and stage under DEST. Passing the raw
// spec made the shipped name "implementation.py:helpers", so a valid
// "import helpers" was rejected.
func TestValidateRunDispatchOptionsAcceptsRenamedExtraScriptDestination(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("import helpers\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	src := filepath.Join(dir, "implementation.py")
	if err := os.WriteFile(src, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatalf("write extra script: %v", err)
	}
	// helpers.py must exist locally, otherwise the import resolves to nothing
	// and the checker stays silent for an unrelated reason.
	if err := os.WriteFile(filepath.Join(dir, "helpers.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatalf("write local module: %v", err)
	}

	err := validateRunDispatchOptions(runDispatchOptions{
		runPayloadInput: runPayloadInput{script: script, extraScripts: []string{src + ":helpers.py"}},
	})
	if err != nil {
		t.Fatalf("extra script renamed to helpers.py ships as helpers: %v", err)
	}
}

// The same-name form must keep working: the destination falls back to the
// source's base name.
func TestValidateRunDispatchOptionsAcceptsExtraScriptWithoutDestination(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("import helpers\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	src := filepath.Join(dir, "helpers.py")
	if err := os.WriteFile(src, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatalf("write extra script: %v", err)
	}

	if err := validateRunDispatchOptions(runDispatchOptions{
		runPayloadInput: runPayloadInput{script: script, extraScripts: []string{src}},
	}); err != nil {
		t.Fatalf("extra script without DEST ships under its base name: %v", err)
	}
}
