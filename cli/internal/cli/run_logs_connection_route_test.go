// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"os"
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
