// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	o := runDispatchOptions{runPayloadInput: runPayloadInput{script: filepath.Join(t.TempDir(), "absent.py")}}
	if err := validateRunDispatchOptions(o); err != nil {
		t.Fatalf("want no error from the import gate, got %v", err)
	}
}

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

func TestValidateRunDispatchOptionsChecksEntrypointWhenNotAManagedWorkflow(t *testing.T) {
	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "train.py")
	if err := os.WriteFile(entrypoint, []byte("from pipeline import run\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.py"), []byte("def run():\n    pass\n"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
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
