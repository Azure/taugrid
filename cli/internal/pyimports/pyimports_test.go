// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pyimports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCheckFlagsUnshippedSiblingModule(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "import ray\nfrom helpers import load\n")
	writeFile(t, filepath.Join(dir, "helpers.py"), "def load():\n    return 1\n")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(findings), findings)
	}
	got := findings[0]
	if got.Module != "helpers" || got.Kind != KindModule || got.Line != 2 {
		t.Fatalf("unexpected finding: %+v", got)
	}

	err = Error(entry, findings)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"helpers", "helpers.py", "ModuleNotFoundError", "custom Ray image"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestCheckIgnoresThirdPartyAndStdlibImports(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "import os, sys\nimport ray.train\nfrom torch.utils.data import DataLoader\nimport numpy as np\n")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings, got %+v", findings)
	}
	if err := Error(entry, findings); err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
}

func TestCheckAcceptsShippedSibling(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "from helpers import load\n")
	writeFile(t, filepath.Join(dir, "helpers.py"), "")

	findings, err := Check(entry, []string{"train.py", "helpers.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings for a shipped sibling, got %+v", findings)
	}
}

func TestCheckFlagsLocalPackageDirectory(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "from daft_common_crawl.foo import bar\n")
	writeFile(t, filepath.Join(dir, "daft_common_crawl", "__init__.py"), "")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != KindPackage || findings[0].Module != "daft_common_crawl" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if msg := Error(entry, findings).Error(); !strings.Contains(msg, "package directory") {
		t.Fatalf("error should explain directories cannot be shipped: %s", msg)
	}
}

func TestCheckFlagsRelativeImport(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "from .util import helper\n")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != KindRelative {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if msg := Error(entry, findings).Error(); !strings.Contains(msg, "relative import") {
		t.Fatalf("error should call out the relative import: %s", msg)
	}
}

func TestCheckIgnoresCommentsAndDocstrings(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, `"""
Usage notes:
    import helpers
"""
# import helpers
    # import helpers
print("ok")
`)
	writeFile(t, filepath.Join(dir, "helpers.py"), "")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("prose and comments must not be treated as imports, got %+v", findings)
	}
}

func TestCheckReportsEachModuleOnce(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "import helpers\nfrom helpers import load\nimport helpers as h\n")
	writeFile(t, filepath.Join(dir, "helpers.py"), "")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Line != 1 {
		t.Fatalf("want one finding on the first occurrence, got %+v", findings)
	}
}

func TestCheckHandlesCommaSeparatedImports(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "train.py")
	writeFile(t, entry, "import os, helpers, sys  # local mixed with stdlib\n")
	writeFile(t, filepath.Join(dir, "helpers.py"), "")

	findings, err := Check(entry, []string{"train.py"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Module != "helpers" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
}
