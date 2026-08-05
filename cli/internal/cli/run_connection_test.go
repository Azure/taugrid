package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type fakeRunConnectionEnsurer struct {
	calls       int
	connection  workspaceconnection.ActiveConnection
	err         error
	discoveries []workspaceconnection.Discovery
}

func (f *fakeRunConnectionEnsurer) Ensure(context.Context, string) (workspaceconnection.ActiveConnection, error) {
	f.calls++
	return f.connection, f.err
}

func (f *fakeRunConnectionEnsurer) EnsureDiscovery(_ context.Context, discovery workspaceconnection.Discovery) (workspaceconnection.ActiveConnection, error) {
	f.calls++
	f.discoveries = append(f.discoveries, discovery)
	return f.connection, f.err
}

func TestApplyAutomaticRunConnectionFillsWorkspaceAndContext(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "sample", ContextName: "aks-flex", KubeconfigPath: "/tmp/tau-kubeconfig",
	}}
	got, connection, err := applyAutomaticRunConnection(
		context.Background(),
		defaultRunDispatchOptions(),
		runConnectionSource{StartDir: "/repo"},
		true,
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.workspace != "sample" || got.kubeContext != "aks-flex" || connection.KubeconfigPath == "" {
		t.Fatalf("options=%#v connection=%#v", got, connection)
	}
}

func TestApplyAutomaticRunConnectionPreservesExplicitPolicy(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.workspace = "explicit"
	options.workspaceExplicit = true
	ensurer := &fakeRunConnectionEnsurer{}
	got, _, err := applyAutomaticRunConnection(context.Background(), options, runConnectionSource{StartDir: "/repo"}, true, ensurer)
	if err != nil {
		t.Fatal(err)
	}
	if got.workspace != "explicit" || ensurer.calls != 0 {
		t.Fatalf("options=%#v calls=%d", got, ensurer.calls)
	}
}

func TestApplyAutomaticRunConnectionRequiresDescriptorForSmoke(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{err: workspaceconnection.ErrDescriptorNotFound}
	_, _, err := applyAutomaticRunConnection(context.Background(), defaultRunDispatchOptions(), runConnectionSource{StartDir: "/repo"}, true, ensurer)
	if !errors.Is(err, workspaceconnection.ErrDescriptorNotFound) {
		t.Fatalf("expected descriptor error, got %v", err)
	}
}

func TestCatalogConnectionIsNotBypassedByConfigWorkspace(t *testing.T) {
	descriptor, err := workspaceconnection.Parse([]byte(runRoutingDescriptor))
	if err != nil {
		t.Fatal(err)
	}
	discovery := workspaceconnection.Discovery{Descriptor: descriptor}
	source := runConnectionSource{
		Catalog:   true,
		Project:   "alpha",
		Discovery: &discovery,
	}
	options := defaultRunDispatchOptions()
	options.workspace = "sample"
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "sample", ContextName: "catalog-context",
	}}
	got, _, err := applyAutomaticRunConnection(context.Background(), options, source, false, ensurer)
	if err != nil {
		t.Fatal(err)
	}
	if ensurer.calls != 1 || got.kubeContext != "catalog-context" {
		t.Fatalf("calls=%d options=%#v", ensurer.calls, got)
	}
}

func TestCatalogConnectionRejectsConflictingConfigWorkspaceBeforeActivation(t *testing.T) {
	descriptor, err := workspaceconnection.Parse([]byte(runRoutingDescriptor))
	if err != nil {
		t.Fatal(err)
	}
	discovery := workspaceconnection.Discovery{Descriptor: descriptor}
	options := defaultRunDispatchOptions()
	options.workspace = "other"
	ensurer := &fakeRunConnectionEnsurer{}
	_, _, err = applyAutomaticRunConnection(
		context.Background(),
		options,
		runConnectionSource{Catalog: true, Project: "alpha", Discovery: &discovery},
		false,
		ensurer,
	)
	if err == nil ||
		!strings.Contains(err.Error(), `policy.workspace "other"`) ||
		!strings.Contains(err.Error(), `project "alpha"`) {
		t.Fatalf("expected catalog workspace conflict, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("workspace conflict activated connection %d times", ensurer.calls)
	}
}

func TestExplicitWorkspaceStillBypassesCatalogConnection(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.workspace = "operator-override"
	options.workspaceExplicit = true
	ensurer := &fakeRunConnectionEnsurer{}
	got, _, err := applyAutomaticRunConnection(
		context.Background(),
		options,
		runConnectionSource{Catalog: true},
		false,
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.workspace != "operator-override" || ensurer.calls != 0 {
		t.Fatalf("options=%#v calls=%d", got, ensurer.calls)
	}
}

func TestUseKubeconfigRestoresEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/original")
	restore, err := useKubeconfig("/tmp/tau")
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KUBECONFIG"); got != "/tmp/tau" {
		t.Fatalf("KUBECONFIG = %q", got)
	}
	restore()
	if got := os.Getenv("KUBECONFIG"); got != "/tmp/original" {
		t.Fatalf("restored KUBECONFIG = %q", got)
	}
}

func TestRunLifecycleExplicitWorkspaceResolvesItsTargetNamespace(t *testing.T) {
	command := &cobra.Command{}
	command.SetContext(context.Background())
	workspace := readyWorkspace()
	workspace.Metadata.Name = "research"
	workspace.Status.Target.ResolvedNamespace = "research-runs"

	var gotRoutingNamespace string
	var gotRoutingNamespaceExplicit bool
	restored := false
	resolve := func(
		_ *cobra.Command,
		_ string,
		namespace string,
		_ bool,
		namespaceExplicit bool,
	) (string, string, func(), error) {
		gotRoutingNamespace = namespace
		gotRoutingNamespaceExplicit = namespaceExplicit
		return "workspace-context", "connected-namespace", func() { restored = true }, nil
	}
	fetch := func(
		_ *cobra.Command,
		kubeContext, namespace, name string,
	) (tauworkspace.Workspace, error) {
		if kubeContext != "workspace-context" || namespace != tauworkspace.PlatformNamespace || name != "research" {
			t.Fatalf("workspace lookup = context %q namespace %q name %q", kubeContext, namespace, name)
		}
		return workspace, nil
	}

	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithWorkspaceUsing(
		command,
		"ambient-context",
		"",
		"research",
		false,
		false,
		true,
		resolve,
		fetch,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoutingNamespace != "research" || !gotRoutingNamespaceExplicit {
		t.Fatalf("routing namespace = %q explicit=%v", gotRoutingNamespace, gotRoutingNamespaceExplicit)
	}
	if gotContext != "workspace-context" || gotNamespace != "research-runs" {
		t.Fatalf("resolved context=%q namespace=%q", gotContext, gotNamespace)
	}
	if restored {
		t.Fatal("successful workspace resolution restored kubeconfig too early")
	}
	restore()
	if !restored {
		t.Fatal("workspace connection restore was not preserved")
	}
}

func TestRunLifecycleExplicitNamespaceMustMatchWorkspace(t *testing.T) {
	command := &cobra.Command{}
	command.SetContext(context.Background())
	workspace := readyWorkspace()
	workspace.Metadata.Name = "research"
	workspace.Status.Target.ResolvedNamespace = "research-runs"

	restored := false
	_, _, _, err := resolveRunLifecycleConnectionWithWorkspaceUsing(
		command,
		"ambient-context",
		"other",
		"research",
		false,
		true,
		true,
		func(
			_ *cobra.Command,
			_ string,
			namespace string,
			_ bool,
			namespaceExplicit bool,
		) (string, string, func(), error) {
			if namespace != "other" || !namespaceExplicit {
				t.Fatalf("routing namespace = %q explicit=%v", namespace, namespaceExplicit)
			}
			return "workspace-context", namespace, func() { restored = true }, nil
		},
		func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
			return workspace, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), `namespace "other" conflicts with TauWorkspace "research" target namespace "research-runs"`) {
		t.Fatalf("workspace namespace conflict = %v", err)
	}
	if !restored {
		t.Fatal("workspace namespace conflict did not restore the connection")
	}
}

func TestAmbientContextDoesNotBypassEnabledRepository(t *testing.T) {
	t.Setenv("TAU_CONTEXT", "ambient-admin-context")
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace: "sample", ContextName: "descriptor-context", Namespace: "sample",
	}}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		defaultKubeContext(),
		"",
		false,
		false,
		"",
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if ensurer.calls != 1 || gotContext != "descriptor-context" || gotNamespace != "sample" {
		t.Fatalf("calls=%d context=%q namespace=%q", ensurer.calls, gotContext, gotNamespace)
	}
}

func TestExplicitContextBypassesDescriptor(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"explicit-context",
		"custom",
		true,
		true,
		"",
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if ensurer.calls != 0 || gotContext != "explicit-context" || gotNamespace != "custom" {
		t.Fatalf("calls=%d context=%q namespace=%q", ensurer.calls, gotContext, gotNamespace)
	}
}

// An ambient TAU_CONTEXT names the target cluster for the smoke path exactly as
// a typed --context does, so it must reach the dispatch options as explicit.
//
// This replaces TestAmbientContextIsNotAnExplicitSmokeOverride, which asserted
// the opposite. That test guarded a real concern — a checked-in descriptor must
// not be silently overridden by a forgotten shell variable — but the mechanism
// was wrong: treating an ambient context as *unset* meant the connection layer
// took over and pointed KUBECONFIG at a cached cluster, which is the same
// silent redirection in the other direction. The concern is now handled where
// it belongs, by checkDescriptorContextConflict reporting the disagreement
// instead of either side winning quietly.
func TestAmbientContextIsAnExplicitSmokeTarget(t *testing.T) {
	t.Setenv("TAU_CONTEXT", "ambient-admin-context")
	command := &cobra.Command{}
	command.Flags().String("context", defaultKubeContext(), "")
	if err := command.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	if !runContextExplicit(command) {
		t.Fatal("an ambient TAU_CONTEXT names a cluster, so smoke must treat it as explicit")
	}
	if err := command.Flags().Set("context", "explicit-context"); err != nil {
		t.Fatal(err)
	}
	if !runContextExplicit(command) {
		t.Fatal("a typed --context must stay explicit")
	}
}

func TestLifecycleCatalogRoutingRequiresProjectAtRoot(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	withRunRoutingCWD(t, root)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "catalog-context",
		Namespace:   "catalog-namespace",
	}}

	_, _, _, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"ambient-context",
		"",
		false,
		false,
		"",
		ensurer,
	)
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("expected root lifecycle ambiguity, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("ambiguous lifecycle contacted connection manager %d times", ensurer.calls)
	}

	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"ambient-context",
		"",
		false,
		false,
		"alpha",
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if gotContext != "catalog-context" || gotNamespace != "catalog-namespace" {
		t.Fatalf("context=%q namespace=%q", gotContext, gotNamespace)
	}
	wantDiscovery, err := filepath.EvalSymlinks(filepath.Join(root, "connections", "shared.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ensurer.discoveries) != 1 ||
		ensurer.discoveries[0].Path != wantDiscovery {
		t.Fatalf("exact catalog discoveries = %#v", ensurer.discoveries)
	}
}

func TestLifecycleExplicitContextBypassesCatalogAmbiguity(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	withRunRoutingCWD(t, root)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{}
	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"explicit-context",
		"explicit-namespace",
		true,
		true,
		"",
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if gotContext != "explicit-context" || gotNamespace != "explicit-namespace" || ensurer.calls != 0 {
		t.Fatalf("context=%q namespace=%q calls=%d", gotContext, gotNamespace, ensurer.calls)
	}
}

func TestLifecycleOutsideRepositoryRequiresOperatorRouting(t *testing.T) {
	root := t.TempDir()
	withRunRoutingCWD(t, root)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{}
	if _, _, _, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"ambient-context",
		"",
		false,
		false,
		"",
		ensurer,
	); err == nil || !strings.Contains(err.Error(), "outside a Git repository") {
		t.Fatalf("expected outside-repository error, got %v", err)
	}
	gotContext, gotNamespace, restore, err := resolveRunLifecycleConnectionWithEnsurer(
		command,
		"ambient-context",
		"explicit-namespace",
		false,
		true,
		"",
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if gotContext != "ambient-context" || gotNamespace != "explicit-namespace" || ensurer.calls != 0 {
		t.Fatalf("context=%q namespace=%q calls=%d", gotContext, gotNamespace, ensurer.calls)
	}
}
