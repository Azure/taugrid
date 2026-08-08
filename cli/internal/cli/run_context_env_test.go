// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/onboarding"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

// TAU_CONTEXT is documented as the way to name the target cluster once instead
// of passing --context on every invocation, so the two must be interchangeable.
//
// They were not. Every --context flag is registered with defaultKubeContext()
// — that is, $TAU_CONTEXT — as its *default value*, and the connection layer
// asked cobra's Changed("context"), which reports whether the user typed the
// flag, not whether a context was resolved. A context supplied by the
// environment therefore read as "no context given", so the run path took over
// the connection and pointed KUBECONFIG at a previously-cached cluster.
//
// The failure is silent: the command succeeds against the wrong cluster, or
// fails claiming a workspace does not exist when it exists on the cluster the
// user actually named.

// TestRunContextFromEnvSuppressesConnectionTakeover is the guard. The seam is
// runContextExplicit, which every call site uses in place of the raw
// Changed("context") probe.
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

// TestRunContextExplicitFalseWithoutAnyContext keeps the fix from swallowing
// the case it must not: with neither flag nor environment, the connection layer
// still owns the target and the workspace-connection flow has to run.
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

// TestRunContextExplicitTrueWhenFlagPassed pins the path that already worked,
// so a regression there is caught too.
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

// TestRunContextExplicitFalseWhenCommandHasNoContextFlag covers the callers
// that probe a command which never registered the flag; they must not panic or
// report a context that cannot exist.
func TestRunContextExplicitFalseWhenCommandHasNoContextFlag(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")
	if runContextExplicit(&cobra.Command{Use: "no-context-flag"}) {
		t.Fatal("a command without a --context flag cannot have one named")
	}
}

// TestLifecycleConnectionHonoursEnvContext is the end-to-end guard, and the
// one that fails on the unfixed code: it drives the real resolver the query
// verbs call rather than the seam in isolation.
//
// With $TAU_CONTEXT set and no --context typed, the resolver must treat the
// cluster as named and leave the connection alone. Consulting the ensurer here
// is the bug — that is the step that swaps KUBECONFIG for a cached cluster.
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

// TestLifecycleVerbsReportDescriptorContextConflict closes the gap the review
// flagged: `run get/status/logs/cancel` took the ambient context and returned
// early without ever comparing it against the repository's descriptor, so a
// forgotten TAU_CONTEXT silently pointed them at a different cluster than the
// one the repository names — while `tau run` reported the same disagreement.
//
// Reading state against the wrong cluster is less damaging than submitting to
// it, but it is the same silent wrong answer, and the inconsistency is its own
// trap: the submit is refused, then the status call that follows quietly
// succeeds somewhere else.
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

// A typed --context settles the disagreement for the lifecycle verbs too, so
// the error's own advice works here as it does for `tau run`.
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

// TestLifecycleFlagsResolveHonoursEnvContext guards the shared entry point that
// `run get/status/logs/cancel` funnel through.
//
// Separate from TestLifecycleConnectionHonoursEnvContext because that one is
// handed its explicitness by the test. runLifecycleConnectionFlags.resolve
// computes that value itself, and is the one place a rebase can reinstate the
// bug for four verbs at once while still compiling and passing everything else.
//
// resolve() wires in the real connection ensurer with no seam, so rather than
// drive it end to end this asserts on the value it computes. That is the whole
// of what the fix changes there; the rest of the call is main's, unmodified.
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

// TestSmokeHonoursEnvContext covers `tau run smoke`, which reaches the
// connection layer through its own options struct rather than the dispatch
// path the other verbs share. It carried the bug after the rest was fixed,
// because it derived "was a context named" from the emptiness of a string that
// had already been blanked for exactly the case being tested.
func TestSmokeHonoursEnvContext(t *testing.T) {
	t.Setenv(tauContextEnv, "named-by-env")

	cmd := &cobra.Command{Use: "smoke"}
	cmd.Flags().String("context", defaultKubeContext(), kubeContextHelp())
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "cached", ContextName: "cached-cluster", KubeconfigPath: "/tmp/cached-kubeconfig",
	}}
	err := executeBuiltinSmoke(cmd, builtinSmokeCLIOptions{
		KubeContext:         defaultKubeContext(),
		KubeContextExplicit: runContextExplicit(cmd),
		KubeContextFromFlag: cmd.Flags().Changed("context"),
		DryRun:              "client",
		// Catalog defeats the second short-circuit in
		// applyAutomaticRunConnection (`!source.Catalog && kubeContext != ""`),
		// so kubeContextExplicit is the only thing left that can prevent the
		// takeover. Without this the test passes on the broken code, guarded by
		// a condition it is not trying to test.
		Connection:        runConnectionSource{StartDir: "/repo", Catalog: true},
		ConnectionFactory: func(*cobra.Command) runConnectionEnsurer { return ensurer },
		WorkspaceDiscoverer: func(_ *cobra.Command, kubeContext string) (tauworkspace.Workspace, error) {
			if kubeContext != "named-by-env" {
				t.Errorf("smoke resolved context %q, want the value from $%s", kubeContext, tauContextEnv)
			}
			ws := tauworkspace.Workspace{}
			ws.Metadata.Name = "ws"
			return ws, nil
		},
		WorkspaceFetcher: func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
			ws := tauworkspace.Workspace{}
			ws.Metadata.Name = "ws"
			ws.Metadata.Generation = 1
			ws.Status.ObservedGeneration = 1
			ws.Status.Phase = "Ready"
			ws.Status.Target.ResolvedNamespace = "ws-namespace"
			ws.Status.Queue.LocalQueue = "ws-queue"
			ws.Status.Conditions = []tauworkspace.Condition{{Type: "Ready", Status: "True"}}
			return ws, nil
		},
		SmokeRunner: stubSmokeRunner{},
	})
	if err != nil {
		t.Fatalf("smoke: %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("$%s named the cluster, so smoke must not consult the connection cache; got %d calls",
			tauContextEnv, ensurer.calls)
	}
}

type stubSmokeRunner struct{}

func (stubSmokeRunner) Run(context.Context, onboarding.SmokeOptions) (onboarding.SmokeResult, error) {
	return onboarding.SmokeResult{Phase: "DryRun", Manifest: []byte("kind: Job\n")}, nil
}

// TestPlainRepositoryDescriptorConflictIsReported drives the real wiring rather
// than handing checkDescriptorContextConflict a Discovery directly.
//
// The unit tests above passed while the check was effectively dead: they built
// a Discovery themselves, but production only populates source.Discovery for
// catalog entries. A plain repository's descriptor is read further down, inside
// Ensure, so the conflict was never detected on the path most people are on.
// This test starts from a descriptor on disk, which is what makes it catch that.
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

// A repository's checked-in workspace.connection.yaml is a shared, deliberate
// statement of which cluster the project targets; an ambient TAU_CONTEXT is
// per-shell and easy to forget. Neither can be assumed to outrank the other, so
// a disagreement is reported instead of silently resolved. Picking a winner
// either lets a stale shell variable redirect a teammate's checkout, or lets a
// checked-in file quietly ignore the cluster the user just named.
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

// Agreement is not a conflict: naming the same cluster the descriptor already
// names is the common case for someone who exports TAU_CONTEXT out of habit.
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

// The check must stay silent in every case where there is nothing to compare,
// otherwise it turns ordinary runs into errors.
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

// TestEnvContextReachesDispatchOptions is the end of the wire: the run command
// must mark the dispatch options explicit, since that field is what
// applyAutomaticRunConnection reads to decide whether to take over.
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
