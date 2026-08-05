package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveSymlinkedStartUsesActualLexicalWorktreeRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	deep := filepath.Join(root, "project", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(deep, "config.yaml")
	if err := os.WriteFile(config, []byte("name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fileLink := filepath.Join(root, "config.yaml")
	if err := os.Symlink(config, fileLink); err != nil {
		t.Fatal(err)
	}
	fileBoundary, err := Resolve(fileLink)
	if err != nil {
		t.Fatal(err)
	}
	if fileBoundary.LexicalRoot != root || fileBoundary.LexicalStartDir != root {
		t.Fatalf("file-link boundary = %#v", fileBoundary)
	}

	dirLink := filepath.Join(root, "shortcut")
	if err := os.Symlink(deep, dirLink); err != nil {
		t.Fatal(err)
	}
	dirBoundary, err := Resolve(filepath.Join(dirLink, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if dirBoundary.LexicalRoot != root || dirBoundary.LexicalStartDir != dirLink {
		t.Fatalf("directory-link boundary = %#v", dirBoundary)
	}
}

func TestGitCommandForcesStableLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")
	command := gitCommand(t.TempDir(), "rev-parse", "--show-toplevel")
	values := map[string]string{}
	for _, entry := range command.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LC_ALL"] != "C" || values["LANG"] != "C" {
		t.Fatalf("git locale = LC_ALL=%q LANG=%q", values["LC_ALL"], values["LANG"])
	}
}

func TestResolveAcceptsCaseInsensitiveWorktreeAlias(t *testing.T) {
	parent := t.TempDir()
	canonical := filepath.Join(parent, "CaseRepo")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", canonical, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	alias := filepath.Join(parent, "caserepo")
	canonicalInfo, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(canonicalInfo, aliasInfo) {
		t.Skip("test filesystem is case-sensitive")
	}
	boundary, err := Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !boundary.Git {
		t.Fatalf("alias boundary is not Git: %#v", boundary)
	}
	same, err := SamePath(boundary.Root, canonical)
	if err != nil || !same {
		t.Fatalf("boundary root %q is not canonical root %q: same=%v err=%v", boundary.Root, canonical, same, err)
	}
}
