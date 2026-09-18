// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestCachedRunLogsRouteUsesItsVerifiedSystemNamespace(t *testing.T) {
	// An invalid endpoint proves the real execute path reached the selected
	// ConfigMap, without contacting ADX or acquiring an Azure credential.
	logPath := fakeKubectl(t, `
if [ "$1 $2 $3 $4" != "--kubeconfig worker.conf --context worker" ]; then
  echo "wrong cluster route: $*" >&2; exit 3
fi
shift 4
case "$*" in
  "-n custom-system get workspace.tau.azure.com research -o json")
    printf '%s' '{"metadata":{"name":"research","uid":"research-uid","generation":1},"spec":{"target":{"namespace":"research-ns"}},"status":{"phase":"Ready","observedGeneration":1}}' ;;
  "-n research-ns get job train -o json")
    echo 'jobs.batch "train" not found' >&2; exit 1 ;;
  "-n research-ns get rayjob train -o json")
    printf '%s' '{"metadata":{"name":"train"},"status":{"jobStatus":"SUCCEEDED","rayClusterName":"train-cluster","jobId":"driver"}}' ;;
  *"get workloads.kueue.x-k8s.io"*) printf '%s' '{"items":[]}' ;;
  *status.jobId*) printf '%s' 'driver' ;;
  *status.rayClusterName*) printf '%s' 'train-cluster' ;;
  *"get pods"*) printf '' ;;
  "-n custom-system get configmap tau-log-connection -o json")
    printf '%s' '`+loggingConfigMapJSON(t, `{"schema":"tau.logs.connection.v1","endpoint":"invalid-endpoint","database":"Logs","cluster":"worker"}`)+`' ;;
  *) echo "unexpected request: $*" >&2; exit 3 ;;
esac
`)
	hooks := defaultRunLogsDiscoveryHooks(&cobra.Command{}, &runLifecycleConnectionFlags{})
	err := hooks.execute(context.Background(), &bytes.Buffer{}, runLogsRoute{
		Workspace: "research", WorkspaceUID: "research-uid",
		KubeContext: "worker", Kubeconfig: "worker.conf",
		SystemNamespace: "custom-system", Namespace: "research-ns",
	}, "train", runLogsOptions{SystemNamespace: "manager-system", Namespace: "manager-ns", Tail: 10})
	if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
		t.Fatalf("expected discovered endpoint validation, got %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(raw)
	if strings.Contains(calls, "manager-") || strings.Count(calls, "get configmap tau-log-connection") != 1 {
		t.Fatalf("expected one discovery read on verified route: %s", calls)
	}
}

func TestRunLifecycleExplicitSystemNamespaceOverridesConnection(t *testing.T) {
	root := t.TempDir()
	runRunRoutingGit(t, root, "init", "--quiet")
	t.Chdir(root)
	t.Setenv("TAU_CONTEXT", "")
	t.Setenv("KUBECONFIG", "")
	fakeKubectl(t, `
if [ "$*" != "--context worker -n explicit-system get workspaces.tau.azure.com research -o json" ]; then
  echo "unexpected workspace route: $*" >&2; exit 3
fi
printf '%s' '{"metadata":{"name":"research","generation":1},"spec":{"target":{"namespace":"explicit-research"}},"status":{"phase":"Ready","observedGeneration":1}}'
`)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var flags runLifecycleConnectionFlags
	flags.add(cmd)
	cmd.Flags().String("system-namespace", "", "system namespace")
	if err := cmd.ParseFlags([]string{"--system-namespace=explicit-system", "--workspace=research"}); err != nil {
		t.Fatal(err)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "worker", Namespace: "research", SystemNamespace: "connection-system",
	}}
	kubeContext, namespace, restore, err := flags.resolveWithEnsurer(cmd, ensurer)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if kubeContext != "worker" || namespace != "explicit-research" || flags.systemNamespace != "explicit-system" || ensurer.calls != 1 {
		t.Fatalf("route = %s/%s, system namespace = %s, calls = %d", kubeContext, namespace, flags.systemNamespace, ensurer.calls)
	}
}

func TestRootLogsSystemNamespaceRouting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		cached   bool
		systemNS string
	}{
		{"explicit context", []string{"logs", "train", "--context=worker", "-n", "research-ns", "--system-namespace=explicit-system"}, false, "explicit-system"},
		{"run inherited flag", []string{"run", "--system-namespace=explicit-system", "logs", "train", "--context=worker", "-n", "research-ns"}, false, "explicit-system"},
		{"cached default", []string{"logs", "train"}, true, "custom-system"},
		{"cached explicit override", []string{"logs", "train", "--system-namespace=explicit-system"}, true, "explicit-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			t.Chdir(rootDir)
			t.Setenv("TAU_CONTEXT", "")
			t.Setenv("KUBECONFIG", "")
			t.Setenv(systemNamespaceEnvironment, "wrong-default-system")
			configDir := filepath.Join(rootDir, "config")
			t.Setenv("TAU_CONFIG_DIR", configDir)
			prefix := "--context worker"
			if tc.cached {
				prefix = "--kubeconfig worker.conf --context worker"
				connectionsDir := filepath.Join(configDir, "connections")
				if err := os.MkdirAll(connectionsDir, 0o755); err != nil {
					t.Fatal(err)
				}
				state := `{"schema":"tau.workspace.connection-state.v1","workspace":"research","workspace_uid":"research-uid","context_name":"worker","system_namespace":"custom-system","kubeconfig_path":"worker.conf","namespace":"research-ns","configured_at":"2026-09-08T00:00:00Z","verified_at":"2026-09-08T00:00:00Z"}`
				if err := os.WriteFile(filepath.Join(connectionsDir, "research.json"), []byte(state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			configMap := strings.ReplaceAll(loggingConfigMapJSON(t, `{"schema":"tau.logs.connection.v1","endpoint":"invalid-endpoint","database":"Logs","cluster":"worker"}`), "custom-system", tc.systemNS)
			logPath := fakeKubectl(t, `
	case "$*" in
	  "`+prefix+` -n custom-system get workspace.tau.azure.com research -o json")
	    printf '%s' '{"metadata":{"name":"research","uid":"research-uid","generation":1},"spec":{"target":{"namespace":"research-ns"}},"status":{"phase":"Ready","observedGeneration":1}}' ;;
	  "`+prefix+` -n research-ns get job train -o json")
	    echo 'jobs.batch "train" not found' >&2; exit 1 ;;
	  "`+prefix+` -n research-ns get rayjob train -o json")
	    printf '%s' '{"metadata":{"name":"train"},"status":{"jobStatus":"SUCCEEDED","rayClusterName":"train-cluster","jobId":"driver"}}' ;;
	  "`+prefix+` -n research-ns get workloads.kueue.x-k8s.io "*) printf '%s' '{"items":[]}' ;;
	  "`+prefix+` -n research-ns "*status.jobId*) printf '%s' 'driver' ;;
	  "`+prefix+` -n research-ns "*status.rayClusterName*) printf '%s' 'train-cluster' ;;
	  "`+prefix+` -n research-ns get pods "*) printf '' ;;
	  "`+prefix+` -n `+tc.systemNS+` get configmap tau-log-connection -o json")
	    printf '%s' '`+configMap+`' ;;
	  *) echo "unexpected request: $*" >&2; exit 3 ;;
	esac
	`)
			root := NewRoot()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			err := root.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
				t.Fatalf("expected selected ConfigMap endpoint validation through real command tree, got %v", err)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			want := prefix + " -n " + tc.systemNS + " get configmap tau-log-connection -o json"
			if strings.Count(string(raw), want) != 1 || strings.Contains(string(raw), "wrong-default-system") {
				t.Fatalf("expected one metadata read on selected route %q:\n%s", want, raw)
			}
		})
	}
}
