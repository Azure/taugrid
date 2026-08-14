// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"strings"
	"testing"
)

var publicRootCommands = []string{"cluster", "workspace", "run", "serve", "data", "python", "version"}

func TestPublicRootCommandTree(t *testing.T) {
	root := NewRoot()
	got := map[string]bool{}
	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		got[cmd.Name()] = true
	}
	for _, want := range publicRootCommands {
		if !got[want] {
			t.Fatalf("root command %q is not registered; got %#v", want, got)
		}
		delete(got, want)
	}
	delete(got, "completion")
	delete(got, "help")
	if len(got) != 0 {
		t.Fatalf("unexpected public root commands registered: %#v", got)
	}
}

func TestRootHelpDescribesReleasedContract(t *testing.T) {
	root := NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root help failed: %v\nstderr:\n%s", err, stderr.String())
	}
	help := stdout.String()
	if !strings.Contains(help, "repository-first") {
		t.Fatalf("root help omits the v0.5 product model:\n%s", help)
	}
}

func TestRunHelpIsConfigFirst(t *testing.T) {
	cmd := NewRoot()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run help failed: %v\nstderr:\n%s", err, stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{"--config", "--dry-run", "--manifest-out", "--context", "--namespace", "--project"} {
		if !strings.Contains(help, want) {
			t.Fatalf("run help missing %q:\n%s", want, help)
		}
	}
	for _, hidden := range []string{"--engine", "--script", "--file", "--queue", "--gpu-class", "--workload-kind", "--runtime-pip"} {
		if strings.Contains(help, hidden) {
			t.Fatalf("run help exposes internal flag %q:\n%s", hidden, help)
		}
	}
}

func TestRunSubmitManifestCommandIsPublicAndScoped(t *testing.T) {
	root := NewRoot()
	command, _, err := root.Find([]string{"run", "submit-manifest"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Use != "submit-manifest" {
		t.Fatalf("command use = %q", command.Use)
	}
	for _, flag := range []string{"manifest", "digest", "name", "namespace", "context"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("submit-manifest missing --%s", flag)
		}
	}
}

func TestRunProjectFlagIsInheritedByEveryLifecycleCommand(t *testing.T) {
	root := NewRoot()
	for _, name := range []string{"status", "logs", "get", "list", "cancel", "resume"} {
		command, _, err := root.Find([]string{"run", name})
		if err != nil {
			t.Fatalf("find run %s: %v", name, err)
		}
		if flag := command.InheritedFlags().Lookup("project"); flag == nil {
			t.Errorf("tau run %s does not inherit --project", name)
		}
		if flag := command.LocalNonPersistentFlags().Lookup("project"); flag != nil {
			t.Errorf("tau run %s defines a duplicate local --project", name)
		}
	}
}
