// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestRunContextFromEnvSuppressesConnectionTakeover(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")

	cmd := &cobra.Command{Use: "run"}
	var kubeContext string
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if cmd.Flags().Changed("context") {
		t.Fatal("precondition: an unflagged run must leave cobra's Changed false, or this guard proves nothing")
	}
	if kubeContext != "named-by-env" {
		t.Fatalf("precondition: the flag default must carry $%s, got %q", tauContextEnv, kubeContext)
	}
	if !runContextExplicit(cmd) {
		t.Fatalf("$%s names the target cluster, so it must count as explicit", tauContextEnv)
	}
}

func TestRunContextExplicitFalseWithoutAnyContext(t *testing.T) {
	t.Setenv(tauContextEnv, "")

	cmd := &cobra.Command{Use: "run"}
	var kubeContext string
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if runContextExplicit(cmd) {
		t.Fatal("no flag and no environment means no context was named")
	}
}

func TestRunContextExplicitTrueWhenFlagPassed(t *testing.T) {
	t.Setenv(tauContextEnv, "")

	cmd := &cobra.Command{Use: "run"}
	var kubeContext string
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	if err := cmd.ParseFlags([]string{"--context", "named-by-flag"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if !runContextExplicit(cmd) {
		t.Fatal("an explicitly passed --context must count as explicit")
	}
}

func TestRunContextExplicitFalseWhenCommandHasNoContextFlag(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")
	if runContextExplicit(&cobra.Command{Use: "no-context-flag"}) {
		t.Fatal("a command without a --context flag cannot have one named")
	}
}

func TestLifecycleConnectionHonoursEnvContext(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")
	withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
		ws := tauworkspace.Workspace{}
		ws.Metadata.Name = "ws"
		ws.Status.Target.ResolvedNamespace = "ws-namespace"
		return ws, nil
	})

	cmd := &cobra.Command{Use: "status"}
	var kubeContext, namespace string
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "cached", ContextName: "cached-cluster", KubeconfigPath: "/tmp/cached-kubeconfig",
	}}
	resolvedContext, _, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		cmd,
		kubeContext,
		namespace,
		runContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
		false,
		"",
		ensurer,
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer restore()

	if ensurer.calls != 0 {
		t.Fatalf("$%s named the cluster, so the connection cache must not be consulted; got %d calls",
			tauContextEnv, ensurer.calls)
	}
	if resolvedContext != "named-by-env" {
		t.Fatalf("resolved context = %q, want the value from $%s", resolvedContext, tauContextEnv)
	}
}

func TestLifecycleVerbsReportDescriptorContextConflict(t *testing.T) {
	root := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(root, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
	withRunRoutingCWD(t, root)

	descriptor, err := workspaceconnection.Parse([]byte(runRoutingDescriptor))
	if err != nil {
		t.Fatal(err)
	}
	descriptorContext := descriptor.Cluster.ContextName
	if descriptorContext == "" {
		t.Fatal("precondition: the fixture descriptor must name a context")
	}
	t.Setenv(tauContextEnv, descriptorContext+"-somewhere-else")

	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())
	flags := &runLifecycleConnectionFlags{}
	flags.add(cmd)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	_, _, _, err = resolveRunLifecycleConnectionWithEnsurer(
		cmd,
		flags.kubeContext,
		flags.namespace,
		lifecycleFlagsContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
		false,
		"",
		&fakeRunConnectionEnsurer{},
	)
	if err == nil {
		t.Fatal("an ambient context disagreeing with the repository descriptor must be reported, not silently used")
	}
	if !strings.Contains(err.Error(), descriptorContext) {
		t.Fatalf("error must name the descriptor's context, got: %v", err)
	}
}

func TestLifecycleVerbsAcceptTypedContextOverDescriptor(t *testing.T) {
	root := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(root, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
	withRunRoutingCWD(t, root)
	withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
		ws := tauworkspace.Workspace{}
		ws.Metadata.Name = "ws"
		ws.Status.Target.ResolvedNamespace = "ws-namespace"
		return ws, nil
	})

	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())
	flags := &runLifecycleConnectionFlags{}
	flags.add(cmd)
	if err := cmd.ParseFlags([]string{"--context", "chosen-by-flag"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	resolvedContext, _, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		cmd,
		flags.kubeContext,
		flags.namespace,
		lifecycleFlagsContextExplicit(cmd),
		cmd.Flags().Changed("namespace"),
		false,
		"",
		&fakeRunConnectionEnsurer{},
	)
	if err != nil {
		t.Fatalf("a typed --context must settle the conflict: %v", err)
	}
	defer restore()
	if resolvedContext != "chosen-by-flag" {
		t.Fatalf("resolved context = %q, want the typed flag value", resolvedContext)
	}
}

func TestLifecycleFlagsResolveHonoursEnvContext(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")

	cmd := &cobra.Command{Use: "status"}
	flags := &runLifecycleConnectionFlags{}
	flags.add(cmd)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if cmd.Flags().Changed("context") {
		t.Fatal("precondition: the flag must not read as typed, or this guard proves nothing")
	}
	if flags.kubeContext != "named-by-env" {
		t.Fatalf("precondition: the flag default must carry $%s, got %q", tauContextEnv, flags.kubeContext)
	}
	if !lifecycleFlagsContextExplicit(cmd) {
		t.Fatalf("$%s names the cluster, so the lifecycle verbs must treat it as explicit and leave the connection alone",
			tauContextEnv)
	}
}

func TestPlainRepositoryDescriptorConflictIsReported(t *testing.T) {
	root := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(root, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
	withRunRoutingCWD(t, root)

	descriptor, err := workspaceconnection.Parse([]byte(runRoutingDescriptor))
	if err != nil {
		t.Fatal(err)
	}
	descriptorContext := descriptor.Cluster.ContextName
	if descriptorContext == "" {
		t.Fatal("precondition: the fixture descriptor must name a context")
	}

	options := defaultRunDispatchOptions()
	options.kubeContext = descriptorContext + "-somewhere-else"
	options.kubeContextExplicit = true
	options.kubeContextFromFlag = false

	ensurer := &fakeRunConnectionEnsurer{}
	_, _, err = applyAutomaticRunConnection(
		context.Background(),
		options,
		runConnectionSource{StartDir: root},
		false,
		ensurer,
	)
	if err == nil {
		t.Fatal("an ambient context disagreeing with the repository descriptor must be reported")
	}
	if !strings.Contains(err.Error(), descriptorContext) {
		t.Fatalf("error must name the descriptor's context, got: %v", err)
	}
}

func TestDescriptorContextConflictIsReported(t *testing.T) {
	err := checkDescriptorContextConflict("cluster-from-env", false, &workspaceconnection.Discovery{
		Path: "tau/workspace.connection.yaml",
		Descriptor: workspaceconnection.Descriptor{
			Cluster: workspaceconnection.ClusterDescriptor{ContextName: "cluster-from-descriptor"},
		},
	})
	if err == nil {
		t.Fatal("a descriptor naming a different cluster than the caller must not be silently resolved")
	}
	for _, want := range []string{tauContextEnv, "cluster-from-env", "cluster-from-descriptor", "--context"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name both clusters and the way out; missing %q in: %v", want, err)
		}
	}
}

func TestDescriptorContextAgreementIsAccepted(t *testing.T) {
	err := checkDescriptorContextConflict("same-cluster", false, &workspaceconnection.Discovery{
		Descriptor: workspaceconnection.Descriptor{
			Cluster: workspaceconnection.ClusterDescriptor{ContextName: "same-cluster"},
		},
	})
	if err != nil {
		t.Fatalf("matching values are not a conflict: %v", err)
	}
}

func TestDescriptorContextConflictSkipsWhenNothingToCompare(t *testing.T) {
	descriptor := &workspaceconnection.Discovery{
		Descriptor: workspaceconnection.Descriptor{
			Cluster: workspaceconnection.ClusterDescriptor{ContextName: "cluster-from-descriptor"},
		},
	}
	cases := []struct {
		name      string
		context   string
		fromFlag  bool
		discovery *workspaceconnection.Discovery
	}{
		{"no context named", "", false, descriptor},
		{"no descriptor", "cluster-from-env", false, nil},
		{"descriptor names no cluster", "cluster-from-env", false, &workspaceconnection.Discovery{}},
		{"typed --context settles it", "cluster-from-flag", true, descriptor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkDescriptorContextConflict(c.context, c.fromFlag, c.discovery); err != nil {
				t.Fatalf("nothing to compare must not error: %v", err)
			}
		})
	}
}

func TestEnvContextReachesDispatchOptions(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")

	options := defaultRunDispatchOptions()
	options.kubeContext = defaultKubeContext()
	options.kubeContextExplicit = true

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "cached", ContextName: "cached-cluster", KubeconfigPath: "/tmp/cached-kubeconfig",
	}}
	got, connection, err := applyAutomaticRunConnection(
		context.Background(),
		options,
		runConnectionSource{StartDir: "/repo"},
		false,
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("an explicit context must not consult the connection cache, got %d calls", ensurer.calls)
	}
	if got.kubeContext != "named-by-env" {
		t.Fatalf("kubeContext = %q, want the environment's value", got.kubeContext)
	}
	if connection.KubeconfigPath != "" {
		t.Fatalf("no kubeconfig may be swapped in, got %q", connection.KubeconfigPath)
	}
}
