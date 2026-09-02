// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

type fakeCredentialProvider struct {
	calls int
	raw   []byte
	err   error
}

func (f *fakeCredentialProvider) UserKubeconfig(context.Context, Descriptor) ([]byte, error) {
	f.calls++
	return append([]byte(nil), f.raw...), f.err
}

type fakeVerifier struct {
	calls  int
	result Verification
	err    error
	paths  []string
	modes  []os.FileMode
}

func (f *fakeVerifier) Verify(_ context.Context, _ Descriptor, kubeconfigPath string) (Verification, error) {
	f.calls++
	f.paths = append(f.paths, kubeconfigPath)
	if info, err := os.Stat(kubeconfigPath); err == nil {
		f.modes = append(f.modes, info.Mode().Perm())
	}
	if f.err != nil {
		return Verification{}, f.err
	}
	result := f.result
	if result.WorkspaceUID == "" {
		result.WorkspaceUID = "workspace-uid"
	}
	return result, nil
}

func writeDescriptorFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeKubeconfigDescriptorFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  contextName: taugrid-flex
  systemNamespace: tau-system
access:
  method: kubeconfig
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestListCachedConnectionsReturnsConfiguredRoutesInStableOrder(t *testing.T) {
	configDir := t.TempDir()
	connectionsDir := filepath.Join(configDir, "connections")
	if err := os.MkdirAll(connectionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	states := map[string]connectionState{
		"z.json": {
			Schema: connectionStateSchema, Workspace: "vision", ContextName: "west",
			KubeconfigPath: "/tmp/vision-kubeconfig", Namespace: "vision-ns",
			WorkspaceUID: "vision-uid", ConfiguredAt: now,
		},
		"a.json": {
			Schema: connectionStateSchemaV2, Workspace: "language", ContextName: "east",
			KubeconfigPath: "/tmp/language-kubeconfig", Namespace: "language-ns",
			WorkspaceUID: "language-uid", ConfiguredAt: now,
		},
		"partial.json": {
			Schema: connectionStateSchema, Workspace: "incomplete",
		},
	}
	for name, state := range states {
		if err := fileutil.WriteJSONFileAtomic(filepath.Join(connectionsDir, name), state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListCachedConnections(configDir)
	if err != nil {
		t.Fatalf("ListCachedConnections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("connections = %#v", got)
	}
	if got[0].Workspace != "language" || got[1].Workspace != "vision" {
		t.Fatalf("connections are not stably sorted: %#v", got)
	}
	if got[0].Namespace != "language-ns" || got[0].KubeconfigPath != "/tmp/language-kubeconfig" {
		t.Fatalf("language route = %#v", got[0])
	}
	if got[0].WorkspaceUID != "language-uid" || got[1].WorkspaceUID != "vision-uid" {
		t.Fatalf("workspace identities were not surfaced for revalidation: %#v", got)
	}
}

func TestListCachedConnectionsDeduplicatesRepositoryScopedRoutes(t *testing.T) {
	configDir := t.TempDir()
	connectionsDir := filepath.Join(configDir, "connections")
	if err := os.MkdirAll(connectionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configuredAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	states := map[string]connectionState{
		"older.json": {
			Schema: connectionStateSchema, Workspace: "language", ContextName: "east",
			KubeconfigPath: "/tmp/older-kubeconfig", Namespace: "language-ns",
			WorkspaceUID: "language-uid", ConfiguredAt: configuredAt,
			VerifiedAt: configuredAt,
		},
		"newer.json": {
			Schema: connectionStateSchema, Workspace: "language", ContextName: "east",
			KubeconfigPath: "/tmp/newer-kubeconfig", Namespace: "language-ns",
			WorkspaceUID: "language-uid", ConfiguredAt: configuredAt,
			VerifiedAt: configuredAt.Add(time.Minute),
		},
	}
	for name, state := range states {
		if err := fileutil.WriteJSONFileAtomic(filepath.Join(connectionsDir, name), state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListCachedConnections(configDir)
	if err != nil {
		t.Fatalf("ListCachedConnections: %v", err)
	}
	if len(got) != 1 || got[0].KubeconfigPath != "/tmp/newer-kubeconfig" {
		t.Fatalf("deduplicated connections = %#v", got)
	}
}

func testKubeconfig(server, token string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: %s
contexts:
- name: taugrid-flex
  context:
    cluster: cluster
    user: researcher
current-context: taugrid-flex
users:
- name: researcher
  user:
    token: %s
`, server, token))
}

func withFirstUseApproval(manager Manager) Manager {
	manager.Interactive = true
	manager.Input = strings.NewReader("yes\n")
	manager.Output = &bytes.Buffer{}
	return manager
}

func TestManagerConfiguresFirstConnectionAfterTrust(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{result: Verification{
		ContextName:    "taugrid-flex",
		Namespace:      "sample",
		Queue:          "jobqueue",
		WorkspacePhase: "Ready",
	}}
	var output bytes.Buffer
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("yes\n"),
		Output:      &output,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return now },
	}

	connection, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	for _, detail := range []string{
		"First-time workspace connection",
		"has not been connected with Tau on this machine",
		"Review where Tau will connect",
		"Workspace:       sample",
		"Access method:   aks",
		"Context:         taugrid-flex",
		"Authorization:   cluster-wide",
		"Private network: required",
		"AKS resource:",
		"Entra tenant:",
		"Nothing has been accessed or saved yet",
		"Approve and connect",
	} {
		if !strings.Contains(output.String(), detail) {
			t.Fatalf("first-use review omitted %q:\n%s", detail, output.String())
		}
	}
	if connection.Workspace != "sample" ||
		connection.Namespace != "sample" ||
		connection.Queue != "jobqueue" ||
		connection.SystemNamespace != "tau-system" ||
		connection.AuthorizationMode != AuthorizationModeClusterWide {
		t.Fatalf("connection = %#v", connection)
	}
	info, err := os.Stat(connection.KubeconfigPath)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")); err != nil {
		t.Fatalf("connection state missing: %v", err)
	}
	state, err := loadConnectionState(filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Schema != connectionStateSchema ||
		state.ConfiguredAt != now ||
		state.VerifiedAt != now ||
		state.AccessMethod != AccessMethodAKS ||
		state.AccessIdentity != "aks:/subscriptions/00000000-0000-0000-0000-000000000000/resourcegroups/rg-ai/providers/microsoft.containerservice/managedclusters/taugrid-flex:11111111-1111-1111-1111-111111111111" ||
		state.SystemNamespace != "tau-system" {
		t.Fatalf("persisted configuration/readiness state = %#v", state)
	}
}

func TestManagerRejectsNoninteractiveFirstUseWithoutSideEffects(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Input:       strings.NewReader("yes\n"),
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := manager.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "run `tau workspace connection` from an interactive terminal") ||
		!strings.Contains(err.Error(), "review and approve the destination") {
		t.Fatalf("Ensure() error = %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("noninteractive first use acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("noninteractive first use wrote local state: %v", entries)
	}
}

func TestManagerRejectsKubeconfigFirstUseWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		input       string
		wantErr     error
	}{
		{name: "noninteractive", wantErr: ErrInteractiveRequired},
		{name: "declined", interactive: true, input: "\n", wantErr: ErrConnectionDeclined},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeKubeconfigDescriptorFixture(t)
			configDir := t.TempDir()
			credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
			verifier := &fakeVerifier{}
			manager := Manager{
				ConfigDir:   configDir,
				Interactive: test.interactive,
				Input:       strings.NewReader(test.input),
				Output:      &bytes.Buffer{},
				Credentials: credentials,
				Verifier:    verifier,
			}

			_, err := manager.Ensure(context.Background(), root)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Ensure() error = %v, want %v", err, test.wantErr)
			}
			if credentials.calls != 0 || verifier.calls != 0 {
				t.Fatalf("first use acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
			}
			entries, err := os.ReadDir(configDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("first use wrote local state: %v", entries)
			}
		})
	}
}

func TestManagerDoesNotShareTrustAcrossRepositories(t *testing.T) {
	trustedRoot := writeDescriptorFixture(t)
	copiedRoot := writeDescriptorFixture(t)
	configDir := t.TempDir()
	trusted := Manager{
		ConfigDir:   configDir,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(trusted).Ensure(context.Background(), trustedRoot); err != nil {
		t.Fatalf("trust first repository: %v", err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	copied := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	_, err := copied.Ensure(context.Background(), copiedRoot)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("copied repository inherited trust: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("copied repository acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
}

func TestManagerDoesNotShareTrustForCommonDescriptorTarget(t *testing.T) {
	firstRoot := writeDescriptorFixture(t)
	secondRoot := t.TempDir()
	firstDiscovery, err := Discover(firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	secondDiscovery := firstDiscovery
	secondDiscovery.RepositoryRoot = secondRoot
	secondDiscovery.RealRepositoryRoot = secondRoot
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).EnsureDiscovery(context.Background(), firstDiscovery); err != nil {
		t.Fatalf("trust first repository: %v", err)
	}
	if ConnectionKeyForDiscovery(firstDiscovery) == ConnectionKeyForDiscovery(secondDiscovery) {
		t.Fatal("distinct canonical repository roots produced the same connection key")
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	if _, err := second.EnsureDiscovery(context.Background(), secondDiscovery); !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("second repository inherited trust: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("second repository acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
}

func TestManagerReusesTrustAcrossSymlinkedRepositoryPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := writeDescriptorFixture(t)
	alias := filepath.Join(t.TempDir(), "repository")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	firstCredentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	first := Manager{
		ConfigDir:   configDir,
		Credentials: firstCredentials,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), alias); err != nil {
		t.Fatalf("trust symlinked repository: %v", err)
	}

	secondCredentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	secondVerifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: secondCredentials,
		Verifier:    secondVerifier,
	}
	if _, err := second.Ensure(context.Background(), root); err != nil {
		t.Fatalf("reuse trust through real path: %v", err)
	}
	if secondCredentials.calls != 0 || secondVerifier.calls != 0 {
		t.Fatalf("trusted repository acquired credentials=%d or verified=%d", secondCredentials.calls, secondVerifier.calls)
	}

	aliasDiscovery, err := Discover(alias)
	if err != nil {
		t.Fatal(err)
	}
	realDiscovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if ConnectionKeyForDiscovery(aliasDiscovery) != ConnectionKeyForDiscovery(realDiscovery) {
		t.Fatalf("equivalent repository paths produced different connection keys")
	}
}

func TestManagerRequiresApprovalToMigrateLegacyConnectionState(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatalf("configure connection: %v", err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	connectionsDir := filepath.Join(configDir, "connections")
	scopedPath := filepath.Join(connectionsDir, ConnectionKeyForDiscovery(discovery)+".json")
	legacyPath := filepath.Join(connectionsDir, ConnectionKey(discovery.Descriptor)+".json")
	state, err := loadConnectionState(scopedPath)
	if err != nil {
		t.Fatal(err)
	}
	state.RepositoryRoot = ""
	state.DescriptorPath = discovery.Path
	if err := fileutil.WriteJSONFileAtomic(legacyPath, state); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}
	if err := os.Remove(scopedPath); err != nil {
		t.Fatalf("remove repository-scoped state: %v", err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	if _, err := second.Ensure(context.Background(), root); !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("legacy state reused without approval: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("legacy state acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}

	second.Credentials = &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	second.Verifier = &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	if _, err := withFirstUseApproval(second).Ensure(context.Background(), root); err != nil {
		t.Fatalf("approve legacy state migration: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy state was not removed after migration: %v", err)
	}
	migrated, err := loadConnectionState(scopedPath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.RepositoryRoot != discoveryTrustRoot(discovery) ||
		migrated.DescriptorPath != discoveryTrustPath(discovery) {
		t.Fatalf("migrated trust identity = %#v", migrated)
	}
}

func TestManagerRejectsRetargetedDescriptorSymlinkWithoutSideEffects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tau"), 0o755); err != nil {
		t.Fatal(err)
	}
	firstTarget := filepath.Join(root, "first.yaml")
	secondTarget := filepath.Join(root, "second.yaml")
	if err := os.WriteFile(firstTarget, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondTarget, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	descriptorPath := filepath.Join(root, DescriptorRelativePath)
	if err := os.Symlink(firstTarget, descriptorPath); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatalf("trust first descriptor target: %v", err)
	}
	if err := os.Remove(descriptorPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondTarget, descriptorPath); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	if _, err := second.Ensure(context.Background(), root); !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("retargeted descriptor reused trust: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("retargeted descriptor acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
}

func TestManagerDeclinesFirstUseWithoutSideEffects(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := manager.Ensure(context.Background(), root)
	if !errors.Is(err, ErrConnectionDeclined) {
		t.Fatalf("Ensure() error = %v, want ErrConnectionDeclined", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("declined first use acquired credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("declined first use wrote local state: %v", entries)
	}
}

func TestManagerPersistsConfiguredSystemNamespace(t *testing.T) {
	root := writeDescriptorFixture(t)
	descriptorPath := filepath.Join(root, DescriptorRelativePath)
	raw, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}

	raw = []byte(strings.Replace(string(raw), "  systemNamespace: tau-system\n", "  systemNamespace: custom-system\n", 1))
	if err := os.WriteFile(descriptorPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	connection, err := withFirstUseApproval(manager).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if connection.SystemNamespace != "custom-system" {
		t.Fatalf("SystemNamespace = %q, want custom-system", connection.SystemNamespace)
	}
}

func TestManagerRefreshesKubeconfigCredentialsAndRejectsTargetDrift(t *testing.T) {
	root := writeKubeconfigDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	credentials := &fakeCredentialProvider{raw: testKubeconfig("https://first.example", "token-one")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return now },
	}
	first, err := withFirstUseApproval(manager).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	credentials.raw = testKubeconfig("https://first.example", "token-two")
	verifier.err = errors.New("authorization denied")
	if _, err := manager.Ensure(context.Background(), root); err == nil ||
		!strings.Contains(err.Error(), "authorization denied") {
		t.Fatalf("unverified same-target credential refresh error = %v", err)
	}
	if credentials.calls != 2 || verifier.calls != 2 {
		t.Fatalf("rejected same-target refresh credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	unchanged, err := os.ReadFile(first.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unchanged, []byte("token-one")) ||
		bytes.Contains(unchanged, []byte("token-two")) {
		t.Fatalf("unverified credentials replaced isolated kubeconfig:\n%s", unchanged)
	}

	verifier.err = nil
	if _, err := manager.Ensure(context.Background(), root); err != nil {
		t.Fatalf("refresh same-target credentials: %v", err)
	}
	if credentials.calls != 3 || verifier.calls != 3 {
		t.Fatalf("same-target refresh credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	refreshed, err := os.ReadFile(first.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(refreshed, []byte("token-two")) {
		t.Fatalf("isolated kubeconfig did not refresh credentials:\n%s", refreshed)
	}

	credentials.raw = testKubeconfig("https://second.example", "token-three")
	_, err = manager.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "Kubernetes context target changed") ||
		strings.Contains(err.Error(), "stored configuration is incomplete") {
		t.Fatalf("target drift error = %v", err)
	}
	if verifier.calls != 3 {
		t.Fatalf("target drift verified before review: %d calls", verifier.calls)
	}
	unchanged, err = os.ReadFile(first.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unchanged, []byte("https://first.example")) {
		t.Fatalf("target drift replaced pinned kubeconfig before review:\n%s", unchanged)
	}

	descriptorPath := filepath.Join(root, DescriptorRelativePath)
	rawDescriptor, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	rawDescriptor = []byte(strings.Replace(string(rawDescriptor), "minTauVersion: 0.3.0", "minTauVersion: 0.4.0", 1))
	if err := os.WriteFile(descriptorPath, rawDescriptor, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "descriptor digest") ||
		!strings.Contains(err.Error(), "Kubernetes context target changed") {
		t.Fatalf("combined descriptor and target drift error = %v", err)
	}
}

func TestConnectionStateDefaultsSystemNamespace(t *testing.T) {
	connection := (connectionState{}).active()
	if connection.SystemNamespace != "tau-system" {
		t.Fatalf("SystemNamespace = %q, want tau-system", connection.SystemNamespace)
	}
}

func TestManagerFirstConnectionCredentialFailureLeavesNoStateOrKubeconfig(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{err: errors.New("noninteractive Azure identity unavailable")}
	verifier := &fakeVerifier{}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := withFirstUseApproval(manager).Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "noninteractive Azure identity unavailable") {
		t.Fatalf("expected credential failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 0 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	for _, path := range []string{
		filepath.Join(configDir, "connections"),
		filepath.Join(configDir, "kubeconfigs"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("credential failure left residue at %s: %v", path, statErr)
		}
	}
}

func TestManagerFirstConnectionAuthorizationFailureLeavesNoTrustedState(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{err: errors.New("cluster-user credential is not authorized")}
	manager := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}

	_, err := withFirstUseApproval(manager).Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected authorization failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("authorization failure left trusted state at %s: %v", statePath, statErr)
	}
	kubeconfigEntries, readErr := os.ReadDir(filepath.Join(configDir, "kubeconfigs"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(kubeconfigEntries) != 0 {
		t.Fatalf("authorization failure left kubeconfig residue: %v", kubeconfigEntries)
	}
}

func TestManagerReusesFreshConnectionNonInteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	firstCredentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	firstVerifier := &fakeVerifier{result: Verification{
		ContextName:    "taugrid-flex",
		Namespace:      "sample",
		Queue:          "jobqueue",
		WorkspacePhase: "Ready",
	}}
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: firstCredentials,
		Verifier:    firstVerifier,
		Now:         func() time.Time { return now },
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	secondCredentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	secondVerifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: secondCredentials,
		Verifier:    secondVerifier,
		Now:         func() time.Time { return now.Add(time.Minute) },
	}
	if _, err := second.Ensure(context.Background(), root); err != nil {
		t.Fatalf("fresh cached Ensure: %v", err)
	}
	if secondCredentials.calls != 0 || secondVerifier.calls != 0 {
		t.Fatalf("fresh cache made credentials=%d verifier=%d calls", secondCredentials.calls, secondVerifier.calls)
	}
}

func TestManagerRevalidatesStaleConnectionWithoutRefetchingCredentials(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}

	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	second := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Credentials:  credentials,
		Verifier:     verifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("stale cached Ensure: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 1 {
		t.Fatalf("stale cache made credentials=%d verifier=%d calls", credentials.calls, verifier.calls)
	}
}

func TestManagerReacquiresMissingKubeconfigNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			ServiceAccount: "workspace-sa", WorkspacePhase: "Ready",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := withFirstUseApproval(first).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}

	refreshedAt := configuredAt.Add(10 * time.Minute)
	replacementRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: replacement\n")
	credentials := &fakeCredentialProvider{raw: replacementRaw}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		ServiceAccount: "workspace-sa", WorkspacePhase: "Ready",
	}}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return refreshedAt },
	}
	got, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("reacquire missing kubeconfig: %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if len(verifier.paths) != 1 ||
		verifier.paths[0] == got.KubeconfigPath ||
		len(verifier.modes) != 1 ||
		verifier.modes[0] != 0o600 {
		t.Fatalf("candidate paths=%v modes=%v final=%s", verifier.paths, verifier.modes, got.KubeconfigPath)
	}
	if _, statErr := os.Stat(verifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("verified candidate was not removed: %v", statErr)
	}
	if got.Namespace != "sample" || got.Queue != "jobqueue" {
		t.Fatalf("reacquired connection = %#v", got)
	}
	info, err := os.Stat(got.KubeconfigPath)
	if err != nil {
		t.Fatalf("reacquired kubeconfig: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
	finalRaw, err := os.ReadFile(got.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finalRaw, replacementRaw) {
		t.Fatalf("final kubeconfig = %q, want verified replacement %q", finalRaw, replacementRaw)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ConfiguredAt != configuredAt ||
		state.VerifiedAt != refreshedAt ||
		state.KubeconfigPath != got.KubeconfigPath {
		t.Fatalf("persisted reacquired state = %#v", state)
	}
}

func TestManagerMissingKubeconfigRejectsLiveDriftWithoutCredentialResidue(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			ServiceAccount: "workspace-sa", WorkspacePhase: "Ready",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := withFirstUseApproval(first).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
		ServiceAccount: "replacement-sa", WorkspaceUID: "replacement-uid",
		WorkspacePhase: "Ready",
	}}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return configuredAt.Add(10 * time.Minute) },
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "workspace UID") ||
		!strings.Contains(err.Error(), "namespace") ||
		!strings.Contains(err.Error(), "LocalQueue") ||
		!strings.Contains(err.Error(), "service account") {
		t.Fatalf("expected reacquired connection drift failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if _, statErr := os.Stat(connection.KubeconfigPath); !os.IsNotExist(statErr) {
		t.Fatalf("live drift left reacquired kubeconfig: %v", statErr)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("live drift changed pinned state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}

	var out bytes.Buffer
	interactive := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
			ServiceAccount: "replacement-sa", WorkspaceUID: "replacement-uid",
			WorkspacePhase: "Ready",
		}},
		Now: func() time.Time { return configuredAt.Add(11 * time.Minute) },
	}
	accepted, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("accept reacquired live drift: %v", err)
	}
	if accepted.Namespace != "sample-v2" || accepted.Queue != "priority" {
		t.Fatalf("accepted connection = %#v", accepted)
	}
	if !strings.Contains(out.String(), "Pin the updated namespace") {
		t.Fatalf("interactive drift output:\n%s", out.String())
	}
}

func TestManagerMissingKubeconfigVerificationFailureLeavesStateUntouched(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	configuredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now: func() time.Time { return configuredAt },
	}
	connection, err := withFirstUseApproval(first).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(connection.KubeconfigPath); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	verifier := &fakeVerifier{err: errors.New("workspace authorization verification failed")}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
		Now:         func() time.Time { return configuredAt.Add(10 * time.Minute) },
	}
	_, err = second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "workspace authorization verification failed") {
		t.Fatalf("expected verifier failure, got %v", err)
	}
	if credentials.calls != 1 || verifier.calls != 1 {
		t.Fatalf("credentials=%d verifier=%d", credentials.calls, verifier.calls)
	}
	if _, statErr := os.Stat(connection.KubeconfigPath); !os.IsNotExist(statErr) {
		t.Fatalf("verification failure left reacquired kubeconfig: %v", statErr)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("verification failure changed pinned state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}
}

func TestContractChangesTreatsEmptyServiceAccountAsDefault(t *testing.T) {
	state := connectionState{Namespace: "sample", Queue: "jobqueue"}
	verification := Verification{Namespace: "sample", Queue: "jobqueue", ServiceAccount: "default"}
	if changes := state.contractChanges(verification); len(changes) != 0 {
		t.Fatalf("empty legacy ServiceAccount should match default: %v", changes)
	}

	verification.ServiceAccount = "workspace-sa"
	changes := state.contractChanges(verification)
	if len(changes) != 1 || !strings.Contains(changes[0], `service account "default" -> "workspace-sa"`) {
		t.Fatalf("workspace ServiceAccount drift = %v", changes)
	}
}

func TestManagerRejectsUnsupportedConnectionState(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	unsupportedState := map[string]any{"schema": "tau.workspace.connection-state.unsupported"}
	if err := fileutil.WriteJSONFileAtomic(statePath, unsupportedState); err != nil {
		t.Fatal(err)
	}

	second := Manager{ConfigDir: configDir, Interactive: false}
	if _, err := second.Ensure(context.Background(), root); err == nil ||
		!strings.Contains(err.Error(), "configured workspace connection changed") {
		t.Fatalf("expected unsupported state rejection, got %v", err)
	}
}

func TestManagerRejectsUIDLessStateNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now: func() time.Time { return now },
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.WorkspaceUID = ""
	if err := fileutil.WriteJSONFileAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}

	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier:    verifier,
		Now:         func() time.Time { return now.Add(30 * time.Second) },
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected UID-less v2 state to require interactive review, got %v", err)
	}
	if verifier.calls != 0 {
		t.Fatalf("UID-less v2 state was verified before review: calls=%d", verifier.calls)
	}
}

func TestManagerInteractiveReconfigureVerifierFailurePreservesExistingKubeconfig(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	originalRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: original\n")
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: originalRaw},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	connection, err := withFirstUseApproval(first).Ensure(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", ConnectionKeyForDiscovery(discovery)+".json")
	state, err := loadConnectionState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.WorkspaceUID = ""
	if err := fileutil.WriteJSONFileAtomic(statePath, state); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfigBefore, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}

	verifier := &fakeVerifier{err: errors.New("candidate verification failed")}
	var out bytes.Buffer
	second := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: &fakeCredentialProvider{raw: []byte("replacement credentials")},
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "candidate verification failed") {
		t.Fatalf("expected candidate verification failure, got %v", err)
	}
	if len(verifier.paths) != 1 ||
		verifier.paths[0] == connection.KubeconfigPath ||
		len(verifier.modes) != 1 ||
		verifier.modes[0] != 0o600 {
		t.Fatalf("candidate paths=%v modes=%v final=%s", verifier.paths, verifier.modes, connection.KubeconfigPath)
	}
	if _, statErr := os.Stat(verifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("failed candidate was not removed: %v", statErr)
	}
	kubeconfigAfter, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatalf("original kubeconfig was removed: %v", err)
	}
	if !bytes.Equal(kubeconfigBefore, kubeconfigAfter) || !bytes.Equal(kubeconfigAfter, originalRaw) {
		t.Fatalf("original kubeconfig changed:\nbefore=%q\nafter=%q", kubeconfigBefore, kubeconfigAfter)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("failed reconfigure changed state:\nbefore:\n%s\nafter:\n%s", stateBefore, stateAfter)
	}
	if !strings.Contains(out.String(), "workspace UID pin is missing") {
		t.Fatalf("interactive reconfigure output:\n%s", out.String())
	}

	replacementRaw := []byte("apiVersion: v1\nkind: Config\ncurrent-context: replacement\n")
	successVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	successful := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: replacementRaw},
		Verifier:    successVerifier,
	}
	reconfigured, err := successful.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("successful staged reconfigure: %v", err)
	}
	if reconfigured.KubeconfigPath != connection.KubeconfigPath {
		t.Fatalf("reconfigured connection = %#v", reconfigured)
	}
	if len(successVerifier.paths) != 1 ||
		successVerifier.paths[0] == connection.KubeconfigPath ||
		len(successVerifier.modes) != 1 ||
		successVerifier.modes[0] != 0o600 {
		t.Fatalf("successful candidate paths=%v modes=%v final=%s", successVerifier.paths, successVerifier.modes, connection.KubeconfigPath)
	}
	if _, statErr := os.Stat(successVerifier.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("successful candidate was not removed: %v", statErr)
	}
	replacedRaw, err := os.ReadFile(connection.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replacedRaw, replacementRaw) {
		t.Fatalf("final kubeconfig = %q, want %q", replacedRaw, replacementRaw)
	}
}

func TestManagerRejectsDescriptorTrustChangeNonInteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DescriptorRelativePath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(
		string(raw),
		"tenantID: 11111111-1111-1111-1111-111111111111",
		"tenantID: 33333333-3333-3333-3333-333333333333",
		1,
	))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected changed descriptor to require review, got %v", err)
	}
	for _, secret := range []string{
		"11111111-1111-1111-1111-111111111111",
		"33333333-3333-3333-3333-333333333333",
		"/subscriptions/",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("noninteractive drift error exposed %q: %v", secret, err)
		}
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("changed trust fetched credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}

	var out bytes.Buffer
	interactiveCredentials := &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")}
	interactiveVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	interactive := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &out,
		Credentials: interactiveCredentials,
		Verifier:    interactiveVerifier,
	}
	got, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("confirm descriptor drift: %v", err)
	}
	if interactiveCredentials.calls != 1 || interactiveVerifier.calls != 1 {
		t.Fatalf("interactive credentials=%d verifier=%d", interactiveCredentials.calls, interactiveVerifier.calls)
	}
	if got.Workspace != "sample" {
		t.Fatalf("updated connection = %#v", got)
	}
	if !strings.Contains(out.String(), "change to the configured workspace connection") ||
		!strings.Contains(out.String(), "access identity") ||
		!strings.Contains(out.String(), "Pin workspace") {
		t.Fatalf("interactive drift review output:\n%s", out.String())
	}
	for _, secret := range []string{
		"11111111-1111-1111-1111-111111111111",
		"33333333-3333-3333-3333-333333333333",
		"/subscriptions/",
	} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("interactive drift output exposed %q:\n%s", secret, out.String())
		}
	}
}

func TestManagerRejectsResourceChangeAcrossConnectionKeyNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, DescriptorRelativePath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(
		string(raw),
		"/subscriptions/00000000-0000-0000-0000-000000000000/",
		"/subscriptions/22222222-2222-2222-2222-222222222222/",
		1,
	))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: credentials,
		Verifier:    verifier,
	}
	_, err = second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "access identity") {
		t.Fatalf("expected changed resource to require review, got %v", err)
	}
	if strings.Contains(err.Error(), "/subscriptions/") ||
		strings.Contains(err.Error(), "22222222-2222-2222-2222-222222222222") {
		t.Fatalf("changed resource error exposed ARM identity: %v", err)
	}
	if credentials.calls != 0 || verifier.calls != 0 {
		t.Fatalf("changed resource fetched credentials=%d or verified=%d", credentials.calls, verifier.calls)
	}
}

func TestManagerRejectsNotReadyDuringNoninteractiveRefresh(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Degraded",
		}},
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), `workspace "sample" is not Ready (phase=Degraded)`) {
		t.Fatalf("expected NotReady failure, got %v", err)
	}
}

func TestManagerRejectsLiveContractChangeNoninteractively(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	second := Manager{
		ConfigDir:   configDir,
		Interactive: false,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
			WorkspaceUID: "replacement-uid", WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if !errors.Is(err, ErrInteractiveRequired) ||
		!strings.Contains(err.Error(), "workspace UID") ||
		!strings.Contains(err.Error(), "namespace") ||
		!strings.Contains(err.Error(), "LocalQueue") {
		t.Fatalf("expected changed live contract to require review, got %v", err)
	}

	var out bytes.Buffer
	interactiveVerifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample-v2", Queue: "priority",
		WorkspaceUID: "replacement-uid", WorkspacePhase: "Ready",
	}}
	interactive := Manager{
		ConfigDir:    configDir,
		Interactive:  true,
		Input:        strings.NewReader("y\n"),
		Output:       &out,
		Verifier:     interactiveVerifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	got, err := interactive.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("confirm live contract drift: %v", err)
	}
	if got.Namespace != "sample-v2" || got.Queue != "priority" {
		t.Fatalf("updated live contract = %#v", got)
	}
	if !strings.Contains(out.String(), "Pin the updated namespace") {
		t.Fatalf("interactive live drift review output:\n%s", out.String())
	}
}

// blockingVerifier models kubelogin waiting on a sign-in prompt that will never
// be answered because no terminal is attached.
type blockingVerifier struct{ calls int }

func (b *blockingVerifier) Verify(ctx context.Context, _ Descriptor, _ string) (Verification, error) {
	b.calls++
	<-ctx.Done()
	return Verification{}, ctx.Err()
}

// Revalidation only reads the TauWorkspace and runs `auth can-i`; it asks the
// user nothing, so an automated caller with no TTY must still get through.
func TestManagerRevalidatesStaleConnectionWithoutTTY(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	credentials := &fakeCredentialProvider{raw: []byte("must-not-be-used")}
	verifier := &fakeVerifier{result: Verification{
		ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
		WorkspacePhase: "Ready",
	}}
	second := Manager{
		ConfigDir:    configDir,
		Interactive:  false,
		Credentials:  credentials,
		Verifier:     verifier,
		Now:          func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL: time.Minute,
	}
	_, err := second.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("non-interactive stale revalidation: %v", err)
	}
	if credentials.calls != 0 {
		t.Fatalf("revalidation refetched credentials %d times", credentials.calls)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier called %d times, want 1", verifier.calls)
	}
}

// The kubeconfig is kubelogin exec auth, so revalidation can block on a sign-in
// prompt. With no terminal that would hang forever; it must fail fast instead.
func TestManagerBoundsNonInteractiveRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	verifier := &blockingVerifier{}
	second := Manager{
		ConfigDir:           configDir,
		Interactive:         false,
		Credentials:         &fakeCredentialProvider{raw: []byte("must-not-be-used")},
		Verifier:            verifier,
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: 50 * time.Millisecond,
	}
	_, err := second.Ensure(context.Background(), root)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected ErrInteractiveRequired, got %v", err)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier called %d times, want 1", verifier.calls)
	}
}

// An interactive caller must stay unbounded so a real sign-in can complete.
func TestManagerDoesNotBoundInteractiveRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	first := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(first).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	second := Manager{
		ConfigDir:           configDir,
		Interactive:         true,
		Credentials:         &fakeCredentialProvider{raw: []byte("must-not-be-used")},
		Verifier:            &blockingVerifier{},
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: time.Millisecond,
	}
	_, err := second.Ensure(ctx, root)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	// It stopped because the caller cancelled, not because a timeout fired.
	if errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("interactive revalidation must not be bounded by the timeout: %v", err)
	}
}

// seedConnection establishes one cached connection interactively so skew tests
// start from a file this binary actually wrote.
func seedConnection(t *testing.T, root, configDir string, now time.Time) {
	t.Helper()
	seed := Manager{
		ConfigDir:   configDir,
		Interactive: true,
		Input:       strings.NewReader("\n"),
		Output:      &bytes.Buffer{},
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
		Now:          func() time.Time { return now },
		ReadinessTTL: time.Minute,
	}
	if _, err := withFirstUseApproval(seed).Ensure(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}

// Approval must not be mistaken for a terminal: kubelogin can still block on a
// stale credential, so revalidation stays bounded even when stdin has an answer.
func TestManagerPipedApprovalStillBoundsRevalidation(t *testing.T) {
	root := writeDescriptorFixture(t)
	configDir := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	seedConnection(t, root, configDir, now)

	manager := Manager{
		ConfigDir:           configDir,
		Interactive:         false,
		Input:               strings.NewReader("y\n"),
		Credentials:         &fakeCredentialProvider{raw: []byte("unused")},
		Verifier:            &blockingVerifier{},
		Now:                 func() time.Time { return now.Add(2 * time.Minute) },
		ReadinessTTL:        time.Minute,
		RevalidationTimeout: 50 * time.Millisecond,
	}
	if _, err := manager.Ensure(context.Background(), root); !errors.Is(err, ErrInteractiveRequired) {
		t.Fatalf("expected bounded revalidation failure, got %v", err)
	}
}

func TestManagerSeparatesClustersWithSameContextName(t *testing.T) {
	firstRoot := writeDescriptorFixture(t)
	secondRoot := t.TempDir()
	secondPath := filepath.Join(secondRoot, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o755); err != nil {
		t.Fatal(err)
	}
	secondRaw := strings.Replace(
		validDescriptorYAML,
		"/subscriptions/00000000-0000-0000-0000-000000000000/",
		"/subscriptions/22222222-2222-2222-2222-222222222222/",
		1,
	)
	if err := os.WriteFile(secondPath, []byte(secondRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	newManager := func() Manager {
		return Manager{
			ConfigDir:   configDir,
			Interactive: false,
			Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
			Verifier: &fakeVerifier{result: Verification{
				ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
				WorkspacePhase: "Ready",
			}},
		}
	}
	first, err := withFirstUseApproval(newManager()).Ensure(context.Background(), firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := withFirstUseApproval(newManager()).Ensure(context.Background(), secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.KubeconfigPath == second.KubeconfigPath {
		t.Fatalf("different AKS resource IDs shared kubeconfig path %s", first.KubeconfigPath)
	}
	if _, err := os.Stat(first.KubeconfigPath); err != nil {
		t.Fatalf("first kubeconfig was overwritten or removed: %v", err)
	}
	if _, err := os.Stat(second.KubeconfigPath); err != nil {
		t.Fatalf("second kubeconfig missing: %v", err)
	}
}

func TestManagerEnsureDiscoveryUsesExactDescriptor(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "tau", "workspace.connection.yaml")
	secondPath := filepath.Join(root, "connections", "selected.yaml")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstPath, []byte(strings.Replace(validDescriptorYAML, "workspace: sample", "workspace: wrong", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	discovery, err := LoadFile(secondPath, root)
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: false,
		Credentials: &fakeCredentialProvider{raw: []byte("apiVersion: v1\nkind: Config\n")},
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspacePhase: "Ready",
		}},
	}
	connection, err := withFirstUseApproval(manager).EnsureDiscovery(context.Background(), discovery)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Workspace != "sample" {
		t.Fatalf("connection = %#v", connection)
	}
}

// Device code is the supported non-TTY sign-in flow: it prints a code and waits
// for a human to finish in a browser, which routinely outlives any short
// deadline. Bounding credential acquisition would cancel exactly the path this
// command advertises, so it must stay on the caller context.
func TestManagerDoesNotBoundHumanPacedSignInAfterTrust(t *testing.T) {
	root := writeDescriptorFixture(t)
	credentials := &slowCredentialProvider{delay: 80 * time.Millisecond, raw: []byte("apiVersion: v1\nkind: Config\n")}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: credentials,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspaceUID: "11111111-1111-1111-1111-111111111111", WorkspacePhase: "Ready",
		}},
		RevalidationTimeout: 10 * time.Millisecond,
	}
	got, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("sign-in slower than the verify deadline must still succeed: %v", err)
	}
	if got.Namespace != "sample" {
		t.Fatalf("namespace = %q, want sample", got.Namespace)
	}
}

// An interactive caller must keep the unbounded context: a human can actually
// answer the sign-in prompt, and a 30s cap would abort them mid-browser.
func TestManagerDoesNotBoundInteractiveColdStart(t *testing.T) {
	root := writeDescriptorFixture(t)
	slow := &slowCredentialProvider{delay: 80 * time.Millisecond, raw: []byte("apiVersion: v1\nkind: Config\n")}
	manager := Manager{
		ConfigDir:   t.TempDir(),
		Interactive: true,
		Input:       strings.NewReader("y\n"),
		Output:      &bytes.Buffer{},
		Credentials: slow,
		Verifier: &fakeVerifier{result: Verification{
			ContextName: "taugrid-flex", Namespace: "sample", Queue: "jobqueue",
			WorkspaceUID: "11111111-1111-1111-1111-111111111111", WorkspacePhase: "Ready",
		}},
		RevalidationTimeout: 10 * time.Millisecond,
	}
	got, err := manager.Ensure(context.Background(), root)
	if err != nil {
		t.Fatalf("interactive cold start must not be bounded: %v", err)
	}
	if got.Namespace != "sample" {
		t.Fatalf("namespace = %q, want sample", got.Namespace)
	}
}

// slowCredentialProvider outlives the non-interactive timeout without blocking
// forever, so it can prove the interactive path is not bounded.
type slowCredentialProvider struct {
	delay time.Duration
	raw   []byte
}

func (s *slowCredentialProvider) UserKubeconfig(ctx context.Context, _ Descriptor) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return append([]byte(nil), s.raw...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// BlobFuse and other fixed-mode filesystems reject chmod on a file this process
// just created. os.CreateTemp already made it 0600, so that must not abort the
// connection, exactly as fileutil.WriteFileAtomic treats it.
func TestWriteKubeconfigCandidateToleratesUnsupportedChmod(t *testing.T) {
	original := candidateChmod
	t.Cleanup(func() { candidateChmod = original })
	candidateChmod = func(*os.File, os.FileMode) error {
		return &os.PathError{Op: "chmod", Err: syscall.EPERM}
	}

	path, err := writeKubeconfigCandidate(t.TempDir(), []byte("apiVersion: v1\n"))
	if err != nil {
		t.Fatalf("unsupported chmod must not fail the candidate write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "apiVersion: v1\n" {
		t.Fatalf("candidate contents = %q", raw)
	}
}

func TestWriteKubeconfigCandidateFailsOnRealChmodError(t *testing.T) {
	original := candidateChmod
	t.Cleanup(func() { candidateChmod = original })
	candidateChmod = func(*os.File, os.FileMode) error {
		return &os.PathError{Op: "chmod", Err: syscall.EIO}
	}

	dir := t.TempDir()
	if _, err := writeKubeconfigCandidate(dir, []byte("apiVersion: v1\n")); err == nil {
		t.Fatal("expected a genuine chmod failure to abort the candidate write")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "kubeconfigs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed candidate left %d file(s) behind", len(entries))
	}
}
