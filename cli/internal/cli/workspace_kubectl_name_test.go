// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var workspaceKubectlHintFiles = []string{
	repoPath("cli", "internal", "cli", "cluster_install.go"),
	repoPath("cli", "internal", "cli", "workspace_create.go"),
	repoPath("charts", "taugrid", "templates", "NOTES.txt"),
	repoPath("skills", "taugrid", "references", "platform.md"),
	repoPath("examples", "aks-cpu-quickstart", "README.md"),
	repoPath("examples", "aks-cpu-quickstart", "cleanup.sh"),
	repoPath("examples", "aks-gpu-quickstart", "cleanup.sh"),
}

func TestWorkspaceKubectlHintsUseAResolvableResourceName(t *testing.T) {
	accepted := acceptedKubectlNames(t, repoPath("controllers", "tau-core", "config", "crd", "bases", "tau.azure.com_workspaces.yaml"))

	for _, file := range workspaceKubectlHintFiles {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		hints := workspaceKubectlHints(string(body))
		if len(hints) == 0 {
			t.Errorf("%s: no kubectl workspace hint found; if the hint moved, "+
				"point this guard at its new home rather than deleting the case", file)
			continue
		}
		for _, resource := range hints {
			if !accepted[resource] {
				t.Errorf("%s: `kubectl ... %s` does not resolve. kubectl matches a CRD's "+
					"plural, singular, or shortNames, not its kind. Accepted: %s",
					file, resource, strings.Join(slices.Sorted(maps.Keys(accepted)), ", "))
			}
		}
	}
}

var kubectlWorkspaceArgument = regexp.MustCompile(
	`kubectl\s+(?:(?:-{1,2}[A-Za-z0-9-]+(?:[= ]\S+)?)\s+)*` +
		`(?:get|edit|describe|delete|wait|patch|apply)\s+` +
		`(?:(?:-{1,2}[A-Za-z0-9-]+(?:[= ]\S+)?)\s+)*` +
		`"?([A-Za-z0-9.-]*[Ww]orkspace[A-Za-z0-9.-]*)`)

func workspaceKubectlHints(body string) []string {
	var found []string
	for _, match := range kubectlWorkspaceArgument.FindAllStringSubmatch(joinContinuedLines(body), -1) {
		resource := match[1]
		if strings.Contains(strings.ToLower(resource), "-workspace-") {
			continue
		}
		found = append(found, resource)
	}
	return found
}

var shellContinuation = regexp.MustCompile(`\\\s*\n\s*`)

func joinContinuedLines(body string) string {
	return shellContinuation.ReplaceAllString(body, " ")
}

func acceptedKubectlNames(t *testing.T, crdPath string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read %s: %v", crdPath, err)
	}
	var crd struct {
		Spec struct {
			Group string `yaml:"group"`
			Names struct {
				Kind       string   `yaml:"kind"`
				Plural     string   `yaml:"plural"`
				Singular   string   `yaml:"singular"`
				ShortNames []string `yaml:"shortNames"`
			} `yaml:"names"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(body, &crd); err != nil {
		t.Fatalf("parse %s: %v", crdPath, err)
	}
	if crd.Spec.Group == "" || crd.Spec.Names.Plural == "" {
		t.Fatalf("%s: CRD is missing spec.group or spec.names.plural", crdPath)
	}

	accepted := map[string]bool{}
	for _, name := range append([]string{crd.Spec.Names.Plural, crd.Spec.Names.Singular}, crd.Spec.Names.ShortNames...) {
		if name == "" {
			continue
		}
		accepted[name] = true
		accepted[name+"."+crd.Spec.Group] = true
	}
	if kind := crd.Spec.Names.Kind; kind != "" {
		accepted[strings.ToLower(kind)+"."+crd.Spec.Group] = true
	}
	return accepted
}

func TestAcceptedKubectlNamesFollowBothResolutionRules(t *testing.T) {
	accepted := acceptedKubectlNames(t, repoPath("controllers", "tau-core", "config", "crd", "bases", "tau.azure.com_workspaces.yaml"))

	for _, name := range []string{
		"workspaces", "workspace", "tw",
		"workspaces.tau.azure.com", "workspace.tau.azure.com", "tw.tau.azure.com",
		"tauworkspace.tau.azure.com",
	} {
		if !accepted[name] {
			t.Errorf("kubectl resolves %q, but the guard would reject it", name)
		}
	}
	if accepted["tauworkspace"] {
		t.Error("bare `tauworkspace` does not resolve; accepting it would readmit issue #1308 blocker 6")
	}
}

func TestWorkspaceKubectlHintsMatchEveryFlagOrdering(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{"plain", "kubectl get tauworkspace -n tau-system", "tauworkspace"},
		{"leading -n", "kubectl -n tau-system get tauworkspace", "tauworkspace"},
		{"leading --context", "kubectl --context ai get tauworkspace -n tau-system", "tauworkspace"},
		{"context then -n", "kubectl --context ai -n tau-system get tauworkspace", "tauworkspace"},
		{"flag after verb", "kubectl get -n tau-system tauworkspace", "tauworkspace"},
		{"equals form", "kubectl --context=ai get tauworkspace", "tauworkspace"},
		{"edit", "kubectl edit tauworkspace demo -n tau-system", "tauworkspace"},
		{"delete", "kubectl delete tauworkspace demo", "tauworkspace"},
		{"wait on a named object", `kubectl wait --for=condition=Ready workspace.tau.azure.com/demo`, "workspace.tau.azure.com"},
		{"quoted", `kubectl -n x get "tauworkspace/demo"`, "tauworkspace"},
		{"line continuation", "kubectl --context ai -n tau-system wait \\\n  --for=jsonpath='{.status.phase}'=Ready \\\n  tauworkspace/demo \\\n  --timeout=5m", "tauworkspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceKubectlHints(tc.line)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("workspaceKubectlHints(%q) = %v, want [%s]", tc.line, got, tc.want)
			}
		})
	}

	for _, line := range []string{
		"kubectl -n tau-system get role tau-workspace-reader-demo",
		"kubectl get rolebinding tau-workspace-reader-demo",
	} {
		if got := workspaceKubectlHints(line); len(got) != 0 {
			t.Errorf("workspaceKubectlHints(%q) = %v, want none", line, got)
		}
	}
}

func repoPath(elements ...string) string {
	return filepath.Join(append([]string{"..", "..", ".."}, elements...)...)
}
