package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortalRunHistoryIsExplicitlyEnabled(t *testing.T) {
	cmd := newPortalCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	flag := serve.Flags().Lookup("run-history-enabled")
	if flag == nil {
		t.Fatal("portal serve is missing --run-history-enabled")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--run-history-enabled default = %q, want false", flag.DefValue)
	}
}

func TestPortalRunHistoryRequiresKustoSource(t *testing.T) {
	cmd := newPortalCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"serve", "--run-history-enabled"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--run-history-enabled requires --kusto-endpoint or --kusto-query-command") {
		t.Fatalf("portal serve error = %v, stderr = %q", err, stderr.String())
	}
}

func TestPortalJobsScopeDefaultsFailClosed(t *testing.T) {
	cmd := newPortalCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	if got := serve.Flags().Lookup("jobs-scope-mode").DefValue; got != "disabled" {
		t.Fatalf("--jobs-scope-mode default = %q, want disabled", got)
	}
	if got := serve.Flags().Lookup("namespace").DefValue; got != "" {
		t.Fatalf("--namespace default = %q, want empty for cluster-wide legacy Ray/Runs reads", got)
	}
}

func TestPortalServeKeepsRunningWithoutKubernetesCredentials(t *testing.T) {
	directoryPath := filepath.Join(t.TempDir(), "workspaces.json")
	directory := `{
		"localCluster": "cluster-a",
		"workspaces": [{
			"id": "alpha",
			"cluster": "cluster-a",
			"team": "alpha",
			"namespace": "team-alpha",
			"localQueue": "jobqueue",
			"source": "kusto",
			"authorization": {"mode": "workspace-rbac", "groups": ["group-alpha"]}
		}]
	}`
	if err := os.WriteFile(directoryPath, []byte(directory), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newPortalCmd()
	var stderr bytes.Buffer
	cmd.SetContext(ctx)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve",
		"--addr", "127.0.0.1:0",
		"--source", "kusto",
		"--jobs-scope-mode", "workspace",
		"--workspace-directory", directoryPath,
		"--kubeconfig", filepath.Join(t.TempDir(), "missing-kubeconfig"),
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("portal serve failed without Kubernetes credentials: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "boards disabled (no Kubernetes access)") {
		t.Fatalf("stderr = %q, want Kubernetes access warning", stderr.String())
	}
}

func TestLegacyKubernetesBoardsShareNamespace(t *testing.T) {
	rayOpts, runsOpts := legacyKubernetesBoardOptions("tau-default")
	if rayOpts.Namespace != "tau-default" || runsOpts.Namespace != "tau-default" {
		t.Fatalf("legacy board namespaces = ray %q, runs %q; want tau-default", rayOpts.Namespace, runsOpts.Namespace)
	}
}

func TestParseJobsOperatorScopes(t *testing.T) {
	scopes, err := parseJobsOperatorScopes([]string{
		"team-a=namespace-a/jobqueue",
		"team-b=namespace-b/jobqueue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0].Team != "team-a" || scopes[0].Namespace != "namespace-a" || scopes[0].Queue != "jobqueue" {
		t.Fatalf("scopes = %#v", scopes)
	}
}

func TestParseJobsOperatorScopesRejectsAmbiguity(t *testing.T) {
	for _, values := range [][]string{
		{"team-a=namespace-a"},
		{"team-a=shared/jobqueue", "team-b=shared/jobqueue"},
	} {
		if _, err := parseJobsOperatorScopes(values); err == nil {
			t.Fatalf("accepted invalid scopes %#v", values)
		}
	}
}
