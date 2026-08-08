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

// Every file that tells a human or a script to name the workspace resource to
// kubectl. Listing them explicitly rather than walking the tree keeps the
// guard from silently going quiet if a file is moved or renamed.
var workspaceKubectlHintFiles = []string{
	repoPath("cli", "internal", "cli", "cluster_install.go"),
	repoPath("cli", "internal", "cli", "workspace_create.go"),
	repoPath("cli", "internal", "queueresolve", "resolve.go"),
	repoPath("charts", "taugrid", "templates", "NOTES.txt"),
	repoPath("skills", "taugrid", "references", "platform.md"),
	repoPath("cli", "examples", "aks-cpu-quickstart", "README.md"),
	repoPath("cli", "examples", "aks-cpu-quickstart", "cleanup.sh"),
	repoPath("cli", "examples", "aks-gpu-quickstart", "cleanup.sh"),
}

// kubectl resolves a resource argument against a CRD's plural, singular, and
// shortNames — never its kind. `kubectl get tauworkspace` therefore fails even
// though the kind is TauWorkspace, and the error reads as "the CRDs failed to
// install" rather than "wrong name", which is how it cost an operator an
// afternoon on a working cluster (issue #1308, blocker 6).
//
// This derives the accepted names from the CRD instead of asserting a literal,
// so renaming the plural fails here rather than shipping hints that no longer
// resolve.
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

// kubectlWorkspaceArgument matches a kubectl invocation whose resource
// argument mentions "workspace", tolerating any flags before it (`--context`,
// `-n`, `-o`) so a hint cannot slip past by ordering them differently. The
// trailing `/NAME` form is stripped by the capture group's character class.
var kubectlWorkspaceArgument = regexp.MustCompile(
	`kubectl\s+(?:(?:-{1,2}[A-Za-z0-9-]+(?:[= ]\S+)?)\s+)*` +
		`(?:get|edit|describe|delete|wait|patch|apply)\s+` +
		`(?:(?:-{1,2}[A-Za-z0-9-]+(?:[= ]\S+)?)\s+)*` +
		`"?([A-Za-z0-9.-]*[Ww]orkspace[A-Za-z0-9.-]*)`)

// workspaceKubectlHints returns the resource argument of every kubectl
// invocation that targets a workspace. Matching on "workspace" keeps unrelated
// hints (namespaces, localqueues) out without enumerating them.
func workspaceKubectlHints(body string) []string {
	var found []string
	for _, match := range kubectlWorkspaceArgument.FindAllStringSubmatch(joinContinuedLines(body), -1) {
		resource := match[1]
		// Roles and bindings named after a workspace are a different kind.
		if strings.Contains(strings.ToLower(resource), "-workspace-") {
			continue
		}
		found = append(found, resource)
	}
	return found
}

// joinContinuedLines folds shell line continuations onto one line. Docs wrap
// long kubectl invocations that way, and the resource argument routinely lands
// on a later line than the verb.
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

	// A bare argument resolves against plural, singular, and shortNames only.
	accepted := map[string]bool{}
	for _, name := range append([]string{crd.Spec.Names.Plural, crd.Spec.Names.Singular}, crd.Spec.Names.ShortNames...) {
		if name == "" {
			continue
		}
		accepted[name] = true
		accepted[name+"."+crd.Spec.Group] = true
	}
	// Group-qualified, kubectl also matches the lowercased kind, which is why
	// `tauworkspace.tau.azure.com` resolves while bare `tauworkspace` does not.
	if kind := crd.Spec.Names.Kind; kind != "" {
		accepted[strings.ToLower(kind)+"."+crd.Spec.Group] = true
	}
	return accepted
}

// kubectl resolves a bare argument against plural/singular/shortNames, but a
// group-qualified one also matches the lowercased kind. Both halves verified
// against a live cluster (kubectl v1.35.3): `kubectl get tauworkspace` fails,
// `kubectl get tauworkspace.tau.azure.com` succeeds. Accepting only the first
// rule would make the guard reject a hint that works.
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
	// The bare kind is the original bug and must stay rejected.
	if accepted["tauworkspace"] {
		t.Error("bare `tauworkspace` does not resolve; accepting it would readmit issue #1308 blocker 6")
	}
}

// The regex is the guard's whole reach, so exercise the forms it has to catch.
// Every hint in the repo that carries --context reached this shape, which the
// first version of this pattern missed.
func TestWorkspaceKubectlHintsMatchEveryFlagOrdering(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{"plain", "kubectl get tauworkspace -n tau-platform", "tauworkspace"},
		{"leading -n", "kubectl -n tau-platform get tauworkspace", "tauworkspace"},
		{"leading --context", "kubectl --context ai get tauworkspace -n tau-platform", "tauworkspace"},
		{"context then -n", "kubectl --context ai -n tau-platform get tauworkspace", "tauworkspace"},
		{"flag after verb", "kubectl get -n tau-platform tauworkspace", "tauworkspace"},
		{"equals form", "kubectl --context=ai get tauworkspace", "tauworkspace"},
		{"edit", "kubectl edit tauworkspace demo -n tau-platform", "tauworkspace"},
		{"delete", "kubectl delete tauworkspace demo", "tauworkspace"},
		{"wait on a named object", `kubectl wait --for=condition=Ready workspace.tau.azure.com/demo`, "workspace.tau.azure.com"},
		{"quoted", `kubectl -n x get "tauworkspace/demo"`, "tauworkspace"},
		{"line continuation", "kubectl --context ai -n tau-platform wait \\\n  --for=jsonpath='{.status.phase}'=Ready \\\n  tauworkspace/demo \\\n  --timeout=5m", "tauworkspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceKubectlHints(tc.line)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("workspaceKubectlHints(%q) = %v, want [%s]", tc.line, got, tc.want)
			}
		})
	}

	// Resources that merely mention a workspace in their name are a different
	// kind and must not be reported as unresolvable.
	for _, line := range []string{
		"kubectl -n tau-platform get role tau-workspace-reader-demo",
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
