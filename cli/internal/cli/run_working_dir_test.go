// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProjectFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestBuildProjectArchiveDisabledByDefault(t *testing.T) {
	root := t.TempDir()
	script := writeProjectFile(t, root, "train.py", "print(1)\n")

	archive, name, err := buildProjectArchive(runDispatchOptions{script: script})
	if err != nil {
		t.Fatalf("buildProjectArchive: %v", err)
	}
	if archive != nil {
		t.Fatal("working_dir must stay opt-in so existing single-file configs render unchanged")
	}
	if name != "train.py" {
		t.Fatalf("script name = %q, want train.py", name)
	}
}

func TestBuildProjectArchivePackagesProjectAndRelativisesEntrypoint(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "helpers.py", "X = 1\n")
	script := writeProjectFile(t, root, "pipeline/train.py", "import helpers\n")

	archive, name, err := buildProjectArchive(runDispatchOptions{
		script:     script,
		workingDir: root,
	})
	if err != nil {
		t.Fatalf("buildProjectArchive: %v", err)
	}
	if len(archive) == 0 {
		t.Fatal("expected a project archive")
	}
	// The renderer turns this into `python3 -m pipeline.train`, so it must be
	// the path relative to the project root, in slash form.
	if name != "pipeline/train.py" {
		t.Fatalf("script name = %q, want pipeline/train.py", name)
	}
}

func TestBuildProjectArchiveRejectsEntrypointOutsideWorkingDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	writeProjectFile(t, root, "keep.py", "")
	script := writeProjectFile(t, base, "outside.py", "print(1)\n")

	_, _, err := buildProjectArchive(runDispatchOptions{script: script, workingDir: root})
	if err == nil {
		t.Fatal("want error: an entrypoint outside working_dir would never be shipped")
	}
	if !strings.Contains(err.Error(), "must live inside run.working_dir") {
		t.Fatalf("error should explain the packaging contract, got: %v", err)
	}
}

func TestBuildProjectArchiveHonoursExcludes(t *testing.T) {
	root := t.TempDir()
	script := writeProjectFile(t, root, "train.py", "print(1)\n")
	writeProjectFile(t, root, "data/blob.bin", strings.Repeat("a", 2048))

	withData, _, err := buildProjectArchive(runDispatchOptions{script: script, workingDir: root})
	if err != nil {
		t.Fatalf("buildProjectArchive: %v", err)
	}
	withoutData, _, err := buildProjectArchive(runDispatchOptions{
		script:             script,
		workingDir:         root,
		workingDirExcludes: []string{"data"},
	})
	if err != nil {
		t.Fatalf("buildProjectArchive: %v", err)
	}
	if len(withoutData) >= len(withData) {
		t.Fatalf("excludes should shrink the archive: %d vs %d", len(withoutData), len(withData))
	}
}

func TestEntrypointImportGateSuppressedWhenWorkingDirSet(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "helpers.py", "X = 1\n")
	script := writeProjectFile(t, root, "train.py", "import helpers\nprint(helpers.X)\n")

	// Without working_dir the sibling is not shipped, so the gate must fire
	// at submit time rather than letting Ray fail with ModuleNotFoundError.
	if err := checkEntrypointImports(runDispatchOptions{script: script}); err == nil {
		t.Fatal("want submit-time failure for an unshipped sibling import")
	} else if !strings.Contains(err.Error(), "working_dir") {
		t.Fatalf("error should point at the working_dir remedy, got: %v", err)
	}

	// With working_dir the sibling genuinely ships and Ray puts it on
	// PYTHONPATH, so the same import must be accepted.
	if err := checkEntrypointImports(runDispatchOptions{script: script, workingDir: root}); err != nil {
		t.Fatalf("working_dir ships siblings; gate should pass: %v", err)
	}
}

func TestWorkingDirRejectedForJobEngine(t *testing.T) {
	root := t.TempDir()
	script := writeProjectFile(t, root, "train.py", "print(1)\n")

	// engine: job renders a plain Kubernetes Job, which has no runtime_env.
	// Accepting working_dir there would also suppress the submit-time import
	// check, handing back the exact late ModuleNotFoundError it prevents.
	_, err := newRunJobRequest(runDispatchOptions{script: script, workingDir: root}, "wd-job")
	if err == nil {
		t.Fatal("want rejection: engine job cannot deliver working_dir")
	}
	for _, want := range []string{"not supported with engine: job", "engine: ray"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name the engine and the remedy, got: %v", err)
		}
	}
}

func TestJobEngineUnaffectedWhenWorkingDirUnset(t *testing.T) {
	root := t.TempDir()
	script := writeProjectFile(t, root, "train.py", "print(1)\n")

	if _, err := newRunJobRequest(runDispatchOptions{script: script}, "plain-job"); err != nil {
		t.Fatalf("existing single-file job configs must keep working: %v", err)
	}
}

func TestEntrypointImportGateSearchesProjectRootNotJustEntrypointDir(t *testing.T) {
	// Entrypoint in a subdirectory, shared module at the project root. This
	// resolves when the researcher runs it locally from the project root, so
	// it is exactly the layout most likely to slip through to a Ray worker.
	root := t.TempDir()
	writeProjectFile(t, root, "helpers.py", "def build(): return 1\n")
	script := writeProjectFile(t, root, "pipeline/train.py", "from helpers import build\n")

	err := checkEntrypointImports(runDispatchOptions{script: script, configDir: root})
	if err == nil {
		t.Fatal("want submit-time failure: helpers.py is at the project root and is not shipped")
	}
	if !strings.Contains(err.Error(), "helpers.py") {
		t.Fatalf("error should name the unshipped file, got: %v", err)
	}
}
