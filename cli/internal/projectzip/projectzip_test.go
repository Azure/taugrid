// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projectzip

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func entries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		rc.Close()
		out[f.Name] = buf.String()
	}
	return out
}

func TestBuildIncludesSiblingsAndPackagesAtArchiveRoot(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "import helpers\n")
	write(t, root, "helpers.py", "X = 1\n")
	write(t, root, "pipeline/__init__.py", "")
	write(t, root, "pipeline/stage.py", "Y = 2\n")
	write(t, root, "config.yaml", "lr: 0.1\n")

	archive, _, err := Build(Options{Dir: root})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := entries(t, archive)
	for _, want := range []string{"train.py", "helpers.py", "pipeline/__init__.py", "pipeline/stage.py", "config.yaml"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("archive missing %s; has %v", want, got)
		}
	}
	// Ray strips at most one top-level directory, so entries must sit at the
	// archive root for `import helpers` to resolve after unpacking.
	if got["helpers.py"] != "X = 1\n" {
		t.Fatalf("unexpected content: %q", got["helpers.py"])
	}
}

func TestBuildSkipsDefaultExcludes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "")
	write(t, root, ".git/config", "secret")
	write(t, root, "__pycache__/train.cpython-312.pyc", "junk")
	write(t, root, ".venv/lib/python3.12/site-packages/torch/__init__.py", "huge")
	write(t, root, "stale.pyc", "junk")

	archive, _, err := Build(Options{Dir: root})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := entries(t, archive)
	if len(got) != 1 || got["train.py"] != "" {
		t.Fatalf("only the source file should ship, got %v", got)
	}
}

func TestBuildHonoursExtraExcludes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "")
	write(t, root, "data/corpus.jsonl", "big")
	write(t, root, "notes.md", "x")

	archive, _, err := Build(Options{Dir: root, Excludes: []string{"data", "*.md"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := entries(t, archive)
	if _, ok := got["data/corpus.jsonl"]; ok {
		t.Fatalf("excluded directory leaked: %v", got)
	}
	if _, ok := got["notes.md"]; ok {
		t.Fatalf("excluded glob leaked: %v", got)
	}
	if _, ok := got["train.py"]; !ok {
		t.Fatalf("entrypoint dropped: %v", got)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "import helpers\n")
	write(t, root, "helpers.py", "X = 1\n")

	first, _, err := Build(Options{Dir: root})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Touch mtimes; a reproducible archive must not notice.
	if err := os.Chtimes(filepath.Join(root, "train.py"), fixedModTime, fixedModTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	second, _, err := Build(Options{Dir: root})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("archives must be byte-identical so rendered specs and payload digests stay stable")
	}
}

func TestBuildRejectsOversizeProjectAndNamesLargestFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "x")
	write(t, root, "corpus.bin", strings.Repeat("a", 4096))

	_, files, err := Build(Options{Dir: root, MaxBytes: 1024})
	if err == nil {
		t.Fatal("want size failure")
	}
	if !strings.Contains(err.Error(), "exceeds the limit of") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) == 0 || files[0].Name != "corpus.bin" {
		t.Fatalf("largest file must be reported first, got %+v", files)
	}
	if desc := DescribeLargest(files, 5); !strings.Contains(desc, "corpus.bin") {
		t.Fatalf("description should name the offender: %s", desc)
	}
}

func TestBuildRejectsEmptyProject(t *testing.T) {
	if _, _, err := Build(Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("want error for a project with nothing to ship")
	}
}

func TestBuildRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, "train.py", "")
	if _, _, err := Build(Options{Dir: filepath.Join(root, "train.py")}); err == nil {
		t.Fatal("want error when pointed at a file")
	}
}
