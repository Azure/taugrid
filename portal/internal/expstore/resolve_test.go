package expstore

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRootPrecedence(t *testing.T) {
	tmp := t.TempDir()
	explicit := filepath.Join(tmp, "explicit")
	exactEnv := filepath.Join(tmp, "env")
	root := filepath.Join(tmp, "root")
	env := map[string]string{
		ExpStoreEnv:     exactEnv,
		ExpStoreRootEnv: root,
		ExpContextEnv:   "kind-taugrid",
		ExpTeamEnv:      "research",
		ExpProjectEnv:   "project-alpha",
	}
	got, err := ResolveRoot(ResolveOptions{
		Explicit: explicit,
		Getenv:   envGetenv(env),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != cleanPath(t, explicit) {
		t.Fatalf("ResolveRoot explicit = %q, want %q", got, cleanPath(t, explicit))
	}

	got, err = ResolveRoot(ResolveOptions{Getenv: envGetenv(env)})
	if err != nil {
		t.Fatal(err)
	}
	if got != cleanPath(t, exactEnv) {
		t.Fatalf("ResolveRoot env = %q, want %q", got, cleanPath(t, exactEnv))
	}
}

func TestResolveRootBuildsTeamProjectDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tau-exp")
	env := map[string]string{
		ExpStoreRootEnv: root,
		ExpContextEnv:   "kind-taugrid",
		ExpTeamEnv:      "research",
		ExpProjectEnv:   "project-alpha",
	}
	got, err := ResolveRoot(ResolveOptions{Getenv: envGetenv(env)})
	if err != nil {
		t.Fatal(err)
	}
	want := cleanPath(t, filepath.Join(root, "kind-taugrid", "research", "project-alpha"))
	if got != want {
		t.Fatalf("ResolveRoot default = %q, want %q", got, want)
	}
}

func TestResolveRootOptionComponentsOverrideEnv(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tau-exp")
	env := map[string]string{
		ExpStoreRootEnv: root,
		ExpContextEnv:   "env-context",
		ExpTeamEnv:      "env-team",
		ExpProjectEnv:   "env-project",
	}
	got, err := ResolveRoot(ResolveOptions{
		Context: "flag-context",
		Team:    "flag-team",
		Project: "flag-project",
		Getenv:  envGetenv(env),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := cleanPath(t, filepath.Join(root, "flag-context", "flag-team", "flag-project"))
	if got != want {
		t.Fatalf("ResolveRoot option default = %q, want %q", got, want)
	}
}

func TestResolveRootMissingConfigIsActionable(t *testing.T) {
	_, err := ResolveRoot(ResolveOptions{Getenv: envGetenv(nil)})
	if err == nil {
		t.Fatal("expected missing store error")
	}
	for _, want := range []string{"--store", ExpStoreEnv, ExpStoreRootEnv, ExpContextEnv, ExpTeamEnv, ExpProjectEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing error %q in %q", want, err.Error())
		}
	}
}

func TestResolveRootRejectsUnsafeDefaultComponents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tau-exp")
	env := map[string]string{
		ExpStoreRootEnv: root,
		ExpContextEnv:   "invalid/context",
		ExpTeamEnv:      "research",
		ExpProjectEnv:   "project-alpha",
	}
	_, err := ResolveRoot(ResolveOptions{Getenv: envGetenv(env)})
	if err == nil {
		t.Fatal("expected invalid component error")
	}
	if !strings.Contains(err.Error(), ExpContextEnv) || !strings.Contains(err.Error(), "single path component") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func envGetenv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func cleanPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}
